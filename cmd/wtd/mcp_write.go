package main

// The MCP tools that act on sessions (#165): type into one, create one, end one — and the part of
// read_session that makes an unopened session readable.
//
// None of this widens who can reach the shell. /mcp sits behind the same gate as /ws, and anyone
// who can open /ws can already type into every session. What it adds is an *agent* that types, so
// each of these tools is annotated as not read-only — a client asks its human before calling one —
// and mcpInstructions tells the agent that what it reads on a screen is untrusted input.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// mcpKeys are the named keys send_input accepts, as the bytes a terminal sends for them. Normal
// cursor mode for the arrows: it is what a shell and a line editor expect, and the TUIs that switch
// to application mode accept these too.
var mcpKeys = map[string]string{
	"enter":     "\r",
	"tab":       "\t",
	"escape":    "\x1b",
	"backspace": "\x7f",
	"up":        "\x1b[A",
	"down":      "\x1b[B",
	"right":     "\x1b[C",
	"left":      "\x1b[D",
	"ctrl-c":    "\x03",
	"ctrl-d":    "\x04",
	"ctrl-z":    "\x1a",
	"ctrl-l":    "\x0c",
}

// maxInputBytes caps one send_input's text. Typing, not file transfer: uploads exist for that.
const maxInputBytes = 8 << 10

// keyGap separates the text from each key, and the keys from each other. A TUI reads a burst of
// bytes that arrives at once as a paste, and an Enter inside a paste is a newline in the input box
// rather than a submit — so "text then Enter" sent as one write types the text and never sends it.
const keyGap = 150 * time.Millisecond

// pushInput writes data to a session's terminal as if typed, with `dtach -p`.
//
// Push mode, not an attachment: dtach -p hands bytes to the master and exits, without becoming a
// client. Attaching would resize the session to this server's idea of its size and redraw it under
// whoever is watching; pushing changes nothing but what the program reads on stdin.
func pushInput(socket, data string) error {
	cmd := exec.Command("dtach", "-p", socket)
	cmd.Stdin = strings.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("dtach -p: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *server) toolSendInput(_ *http.Request, a toolArgs) (string, error) {
	if !s.sessionSocketExists(a.Name) {
		return "", fmt.Errorf("%w: %q", errNoSuchSession, a.Name)
	}
	if a.Text == "" && len(a.Keys) == 0 {
		return "", errors.New("nothing to send: give text, keys, or both")
	}
	if len(a.Text) > maxInputBytes {
		return "", fmt.Errorf("text is %d bytes; at most %d can be typed at once", len(a.Text), maxInputBytes)
	}
	// Every key is resolved before anything is typed, so a typo in the last key cannot leave the
	// text half-entered in someone's prompt.
	keys := make([]string, len(a.Keys))
	for i, k := range a.Keys {
		seq, ok := mcpKeys[strings.ToLower(k)]
		if !ok {
			return "", fmt.Errorf("unknown key %q; known keys: %s", k, strings.Join(sortedKeys(mcpKeys), ", "))
		}
		keys[i] = seq
	}

	// Open the session first if nothing holds it, so the output this input causes lands in a ring
	// for read_session to show. dtach keeps no buffer: output from a session with no hub is gone.
	if _, _, held := s.hubs.screen(a.Name); !held {
		s.openForReading(a.Name)
	}

	socket := filepath.Join(s.sessionDir(), a.Name+socketSuffix)
	if a.Text != "" {
		if err := pushInput(socket, a.Text); err != nil {
			return "", err
		}
	}
	for i, seq := range keys {
		if a.Text != "" || i > 0 {
			time.Sleep(keyGap)
		}
		if err := pushInput(socket, seq); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("Sent to %q. Call read_session to see the result.", a.Name), nil
}

func (s *server) toolCreateSession(_ *http.Request, a toolArgs) (string, error) {
	if err := validateSessionName(a.Name); err != nil {
		return "", fmt.Errorf("invalid session name: %w", err)
	}
	if err := validateSocketPath(s.sessionDir(), a.Name); err != nil {
		return "", fmt.Errorf("session name is too long for this server: %w", err)
	}
	if a.Path != "" && a.Project != "" {
		return "", errors.New("path and project are mutually exclusive")
	}
	workdir, code, msg := s.resolveWorkdir(a.Path, a.Project)
	if code != "" {
		return "", errors.New(msg)
	}
	if err := createSession(s.sessionDir(), a.Name, workdir); err != nil {
		return "", err
	}
	return fmt.Sprintf("Created session %q in %s.", a.Name, workdir), nil
}

func (s *server) toolKillSession(_ *http.Request, a toolArgs) (string, error) {
	switch err := deleteSession(s.sessionDir(), a.Name); {
	case err == nil:
		return fmt.Sprintf("Ended session %q.", a.Name), nil
	case errors.Is(err, errNotFound):
		return "", fmt.Errorf("%w: %q", errNoSuchSession, a.Name)
	default:
		return "", err
	}
}

// sessionSize reads the window size a session's terminal has now, from its shell's tty.
//
// This is what lets read_session open a hub for a session nobody has opened: a hub attaches at a
// size, dtach resizes the session to its newest client, and attaching at anything but the current
// size would reflow the screen of whoever is watching. Read from the tty itself because nothing else
// knows — dtach keeps no record, and the client that set it may be on another machine.
//
// ok is false when there is no shell to ask, or its stdin is not a terminal; a size of zero (a
// session created detached that no client has ever sized) is reported as-is.
func (s *server) sessionSize(name string) (cols, rows int, ok bool) {
	shells, _ := scanDtach(s.sessionDir())
	proc, found := shells[filepath.Join(s.sessionDir(), name+socketSuffix)]
	if !found || proc.pid <= 0 {
		return 0, 0, false
	}
	tty, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(proc.pid), "fd", "0"))
	if err != nil || !strings.HasPrefix(tty, "/dev/pts/") {
		return 0, 0, false
	}
	f, err := os.OpenFile(tty, os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = f.Close() }()
	ws, err := pty.GetsizeFull(f)
	if err != nil {
		return 0, 0, false
	}
	return int(ws.Cols), int(ws.Rows), true
}

const (
	// openSettle is how long output must stop before a freshly opened hub's screen is read, and
	// openWait the most read_session will wait for that. Attaching makes dtach send the program a
	// redraw, and a TUI repaints in a few writes; a shell may print nothing at all.
	openSettle = 300 * time.Millisecond
	openWait   = 2 * time.Second
)

// openForReading gives a session a hub at its current size, then waits for the redraw that
// attaching provokes to finish. Reports false when the size cannot be learned, in which case no hub
// is created: guessing a size is exactly the resize this exists to avoid.
func (s *server) openForReading(name string) bool {
	cols, rows, ok := s.sessionSize(name)
	if !ok {
		return false
	}
	if cols <= 0 || rows <= 0 {
		// Never sized by any client, so nobody is watching it at a size to disturb.
		cols, rows = 80, 24
	}
	if _, err := s.hubs.getOrCreate([]string{name}, cols, rows); err != nil {
		logf("wtd: mcp: open %q for reading: %v", name, err)
		return false
	}
	deadline := time.Now().Add(openWait)
	last, stable := -1, time.Now()
	for time.Now().Before(deadline) {
		raw, _, ok := s.hubs.screen(name)
		if !ok {
			return false
		}
		if len(raw) != last {
			last, stable = len(raw), time.Now()
		} else if last > 0 && time.Since(stable) >= openSettle {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
