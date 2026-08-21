package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Narration: the spoken-form summary of what a session just did.
//
// The problem this solves is not "read the terminal aloud". A speech engine reads a Claude Code
// turn-end message as "hash one eight zero" and "dot x l s b" and then recites a full URL, and a
// turn-end message runs to a couple of hundred words where two sentences were wanted. Something
// has to rewrite the text for the ear, and that something needs a model.
//
// Two decisions follow, and they are why this file is 80 lines instead of an HTTP client:
//
//  1. The summary is NOT made here. bin/wt-narrate makes it, from a Claude Code hook, at the
//     moment a turn ends. That keeps wtd at two dependencies with no outbound network call and no
//     API key anywhere near /etc/ttyd-ify/config -- and it makes the model's latency free, because
//     the turn is already over and nobody is waiting for the answer. By the time a client asks,
//     the file is on disk.
//
//  2. The source is the agent's own transcript, not hub.ring. The ring holds raw bytes including
//     cursor addressing, by design (ring.go), and Claude Code redraws its whole screen constantly
//     -- summarizing the ring means summarizing a repaint. It also only exists for sessions
//     deep-linked through this server since it started. See #112.
//
// So wtd's whole job is: read a small file, check it, serve it. The file is written by whatever
// agent runs in the session, which is deliberately not wtd's business -- Codex or opencode can
// write the same file, the way section 6a keeps WTState agent-agnostic.
//
// The files live under -state-dir, which is systemd's $RUNTIME_DIRECTORY: tmpfs, 0700, gone on
// reboot. Same reasoning ringstore.go states for the same directory -- a summary of a terminal
// session can quote a secret out of it, so it must not persist on disk.

// narrationSubdir keeps the summaries out of the ring store's namespace, which owns *.ring in the
// state directory root.
const narrationSubdir = "narration"

// maxNarrationBytes bounds the read. The writer is a local process, so this is not a defense
// against an attacker; it is a defense against reading a file that something truncated, filled, or
// left mid-write, and against a spoken summary that is not a summary.
const maxNarrationBytes = 64 << 10

// Narration is what a client gets. Two text fields rather than one so the client chooses how much
// to say: the headline alone while you are mid-conversation, both when you ask for more.
//
// NeedsYou is the field that makes this usable while driving. It marks the only case worth speaking
// without being asked -- the session is blocked on you -- so a client can stay silent for
// everything else instead of narrating a monologue at you in traffic.
type Narration struct {
	Session string `json:"session"`
	// Event uses the vocabulary of Session.AgentStatus (see status.go): waiting, attention. It is
	// not an enum here, and clients must ignore a value they do not know rather than treat it as
	// none -- the same forward-compatibility rule section 6a puts on WTState, for the same reason.
	Event string `json:"event"`
	// At is what a client keys on to decide whether it has already spoken this. Parsed rather than
	// passed through, because a client that cannot compare timestamps repeats itself out loud.
	At       time.Time `json:"at"`
	Headline string    `json:"headline"`
	Detail   string    `json:"detail,omitempty"`
	NeedsYou bool      `json:"needsYou"`
}

// narrationDir returns the directory holding the summaries, or "" when there is no state
// directory and the feature is therefore off.
func narrationDir(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, narrationSubdir)
}

// readNarration loads and checks one summary.
//
// The name must already have been matched against a real session by the caller: this builds a path
// from it. Checked again here anyway, because this is the only place in the server that turns a
// session name into a filename, and CLAUDE.md's rule about session names being untrusted network
// input is worth two lines of belt and braces.
func readNarration(dir, name string) (Narration, error) {
	var n Narration
	if dir == "" {
		return n, os.ErrNotExist
	}
	if name == "" || strings.ContainsRune(name, '/') || strings.Contains(name, "..") {
		return n, fmt.Errorf("unusable session name %q", name)
	}

	f, err := os.Open(filepath.Join(dir, name+".json"))
	if err != nil {
		return n, err
	}
	defer f.Close()

	dec := json.NewDecoder(&limitedReader{r: f, n: maxNarrationBytes})
	if err := dec.Decode(&n); err != nil {
		return n, fmt.Errorf("summary for %q is not readable: %w", name, err)
	}

	// Decoded rather than relayed, so a half-written or malformed file is an error here instead of
	// a client's problem. Two fields carry behaviour and must therefore be present: without At a
	// client cannot tell a new summary from one it already spoke, and without Headline there is
	// nothing to say.
	switch {
	case n.At.IsZero():
		return Narration{}, fmt.Errorf("summary for %q has no usable timestamp", name)
	case strings.TrimSpace(n.Headline) == "":
		return Narration{}, fmt.Errorf("summary for %q has no headline", name)
	}
	// The file names its own session; trust the filename over the contents, since that is what the
	// caller matched against the session list.
	n.Session = name
	return n, nil
}

// limitedReader is io.LimitReader plus an error, because a silently truncated JSON document
// decodes as a syntax error rather than as "too big", and the two want different messages.
type limitedReader struct {
	r *os.File
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("summary is larger than the readable limit")
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	k, err := l.r.Read(p)
	l.n -= int64(k)
	return k, err
}

// dropNarration removes a session's summary. Best effort by design: every failure mode here is
// either "there was nothing to remove" or a runtime directory problem that the delete it
// accompanies should not be reported as.
//
// The name is checked the same way readNarration checks it, because this also builds a path from
// it -- and this one unlinks.
func dropNarration(dir, name string) {
	if dir == "" || name == "" || strings.ContainsRune(name, '/') || strings.Contains(name, "..") {
		return
	}
	_ = os.Remove(filepath.Join(dir, name+".json"))
}

// handleNarrationGet serves the summary for one session.
//
// The name is matched byte-exact against the real session list before any path is built from it --
// the same rule handleSessionGet and deleteSession follow, and it matters more here, because this
// is the one route that turns a session name into a filename.
//
// A session with no summary is a 404, not an empty body. Three different things produce it and none
// of them is an error: narration is not configured, the agent in that session does not report, or
// it has not finished a turn yet. A client polls this and should treat 404 as "nothing to say".
func (s *server) handleNarrationGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	sessions, err := listSessions(s.sessionDir(), s.hubs.stats())
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot read sessions", err.Error())
		return
	}
	found := ""
	for i := range sessions {
		if sessions[i].Name == name {
			found = sessions[i].Name
			break
		}
	}
	if found == "" {
		writeError(w, http.StatusNotFound, codeNotFound, "no such session", name)
		return
	}

	n, err := readNarration(s.narrationDir, found)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, codeNotFound, "no narration for this session", found)
		return
	}
	if err != nil {
		// The writer is our own hook, so this is a bug in it rather than anything the client did.
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot read narration", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, n)
}
