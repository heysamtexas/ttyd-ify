package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Session state is read from the filesystem every time it is asked for. There is
// deliberately no cache: dtach sockets in WT_DIR are the source of truth, bin/wt's menu
// reads exactly the same place, and a cache would be one more thing that could disagree
// with the picker about what exists.
// The zero value of PID and CWD means "could not resolve", which the wire renders as null — see
// MarshalJSON. Internally they stay a plain int and string so that every reader is not a pointer
// dereference; the JSON shape is owned in exactly one place instead.
type Session struct {
	Name     string `json:"name"`
	Attached bool   `json:"attached"`
	// AttachedCount is how many viewers there are. Zero *while Attached is true* means the
	// count could not be established; a detached session is a known zero.
	//
	// That is a correlated two-field invariant, NOT the single out-of-range sentinel PID and
	// CWD use — do not read the two conventions as the same one. 0 is a legitimate count in a
	// way that pid 0 is not, so this field alone tells you nothing; always read it with
	// Attached. Kept as a pair rather than a *int because it is what attachedTo already
	// returns, the combination is unreachable for its counting signals, and it fails safe: a
	// future producer that sets Attached without a count renders null rather than a fabricated
	// number. MarshalJSON owns the rendering, TestAttachedCountMarshalsItsThreeStates pins it.
	AttachedCount int       `json:"attachedCount"`
	CWD           string    `json:"cwd"`
	PID           int       `json:"pid"`
	CreatedAt     time.Time `json:"createdAt"`
}

// MarshalJSON renders an unresolved pid or cwd as null rather than omitting the key.
//
// openapi.yaml lists both as *required and nullable*, and that is the stronger contract for a
// generated client: the key is always there, and null is the single unambiguous way to say "we
// could not find out". `omitempty` produced an absent key instead, which a decoder generated from
// this schema rejects — so a session whose /proc lookup failed was unrepresentable to exactly the
// audience an OpenAPI document exists for. Enrichment being best-effort is the reason these can be
// null at all; it is not a licence to make the shape vary.
//
// Note this is the marshal side only. Decoding still uses the struct tags, and encoding/json turns
// a null into the zero value, so a round trip through the wire preserves "unresolved".
func (s Session) MarshalJSON() ([]byte, error) {
	// A separate type, not a field-by-field map: the compiler then keeps this in step with the
	// struct above for everything except the two fields that deliberately differ.
	type wire struct {
		Name          string    `json:"name"`
		Attached      bool      `json:"attached"`
		AttachedCount *int      `json:"attachedCount"`
		CWD           *string   `json:"cwd"`
		PID           *int      `json:"pid"`
		CreatedAt     time.Time `json:"createdAt"`
	}
	w := wire{Name: s.Name, Attached: s.Attached, CreatedAt: s.CreatedAt}
	if !s.Attached || s.AttachedCount > 0 {
		w.AttachedCount = &s.AttachedCount
	}
	if s.CWD != "" {
		w.CWD = &s.CWD
	}
	if s.PID != 0 {
		w.PID = &s.PID
	}
	return json.Marshal(w)
}

const socketSuffix = ".sock"

// listSessions enumerates dtach sessions in dir.
//
// It reports every socket it finds, including names this API would refuse to *create*
// (bin/wt's menu accepts looser names than the create endpoint validates). If the two
// pickers disagreed about what exists, a session made from the terminal menu would be
// invisible to the app, which is worse than a name that looks odd in JSON.
//
// pid and cwd both describe the session's *shell*, never the dtach master supervising it —
// see sessionProc. Missing pid/cwd is not an error: those come from /proc and are best-effort
// enrichment.
//
// stats is wtd's own per-session client count, from the hub manager; pass nil when there is
// none. It is required for `attached` to mean anything — see attachedTo.
func listSessions(dir string, stats map[string]hubStat) ([]Session, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No socket directory yet simply means no sessions. bin/wt mkdir -p's it on
			// first run, so this is the normal state on a fresh install, not a fault.
			return []Session{}, nil
		}
		return nil, fmt.Errorf("read session dir %s: %w", dir, err)
	}

	procs, clients := scanDtach(dir)

	sessions := make([]Session, 0, len(entries))
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), socketSuffix)
		if name == entry.Name() { // no .sock suffix
			continue
		}

		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			continue // vanished between ReadDir and Stat; it is not a session any more
		}
		if info.Mode()&os.ModeSocket == 0 {
			continue
		}

		attached, viewers := attachedTo(name, info, clients[path], stats)
		s := Session{
			Name:          name,
			Attached:      attached,
			AttachedCount: viewers,
			CreatedAt:     info.ModTime(),
		}
		if p, ok := procs[path]; ok {
			s.PID = p.pid
			s.CWD = p.cwd
		}
		sessions = append(sessions, s)
	}

	// Stable order so a client's list does not reshuffle between polls.
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Name < sessions[j].Name })
	return sessions, nil
}

// attachedTo answers "is someone looking at this session", and "how many" where that is
// answerable at all.
//
// The first sentence is the API contract (api/openapi.yaml, Session.attached); the derivation
// below is not, and it has already had to change once. It used to be the socket's
// owner-execute bit, which dtach sets on attach and clears on last detach. That stopped
// working the moment wtd started holding an attachment of its own to buffer output for
// replay: the bit is now pinned on for every session wtd has ever served, and means nothing.
//
// So there are three signals, in order of authority:
//
//  1. wtd's own subscriber count for the session — exact, and the only thing that knows
//     about a client that is watching through this server.
//  2. dtach clients found in /proc that are *not* wtd's own — someone attached over SSH or
//     from the bash picker. Without this, every external attach to a session wtd holds warm
//     would read as detached, permanently.
//  3. The execute bit, but only for a session no hub holds. There it is still dtach's own
//     signalling and still ground truth, so there is no reason to throw it away.
//
// Only the first two can yield a *number*: they enumerate processes, so they add up. The
// execute bit is one bit and cannot be counted, so a session it reports as attached while the
// /proc walk found nothing to count is reported as attached with the count unresolved, rather
// than with a fabricated 1. That case is returned as (true, 0) — the combination the counting
// signals cannot produce, since they only ever reach `true` by counting something.
func attachedTo(name string, info os.FileInfo, clientPIDs []int, stats map[string]hubStat) (bool, int) {
	st, held := stats[name]

	// Summed, not short-circuited. Returning early on st.clients > 0 was right while this
	// produced a boolean, and would now silently drop an external viewer of a session that
	// also has a client through this server.
	n := st.clients
	for _, pid := range clientPIDs {
		pgid, err := syscall.Getpgid(pid)
		if err != nil {
			continue // exited between the scan and now; it is attached to nothing
		}
		if _, own := st.pgids[pgid]; held && own {
			continue // wtd's own held attachment, which is not a viewer
		}
		n++
	}
	if n > 0 {
		return true, n
	}

	if !held {
		// Verified against live sessions: srwx------ attached, srw------- idle.
		if info.Mode().Perm()&0o100 != 0 {
			return true, 0 // attached by someone this server could not enumerate
		}
	}
	return false, 0
}

// sessionProc is what the /proc walk learns about one live session: the pid of the shell
// running inside it, and where that shell is working.
//
// The pid is the *shell*, never the dtach master that supervises it. The two are one level
// apart in the process tree, so their pids are usually adjacent integers — which is how they
// stayed conflated here for months: `pid` carried the master while `cwd` was read from the
// shell, so one JSON object described two different processes. The master pid is plumbing that
// nothing outside scanDtach needs, so nothing outside scanDtach keeps it.
type sessionProc struct {
	pid int
	cwd string
}

// scanDtach walks /proc once and returns, per socket path, the shell running inside that
// session (with its working directory) and the pids of the dtach clients attached to it.
//
// Two kinds of dtach process can reference one socket: the master that created it and any
// number of clients attached to it. Identifying the master is the mechanism, not the result —
// what callers want is the shell it supervises, because that is the process the API reports
// (`Session.pid`, `Session.cwd`) and the one DELETE signals. The master's own cwd is merely
// wherever the session happened to be started from.
//
// "Has a child" is NOT sufficient to identify the master, which is the trap here. A client
// that created its session with `dtach -A` forks the master and the master stays its child
// for as long as the client lives, so that client has a child too. Measured on a live box:
//
//	2220052 (dtach client, from bin/wt) -> 2220053 (dtach master) -> 2220054 (bash)
//
// Only once the client exits does the master reparent to init. So the test is "has a child
// that is not itself dtach". Getting this wrong matters twice over: `pid`/`cwd` would resolve
// against the wrong process, and wtd's own held attachment — which always creates-or-attaches
// this way — would be misread as its session's master rather than as the client it is.
func scanDtach(dir string) (map[string]sessionProc, map[string][]int) {
	shells := map[string]sessionProc{}
	clients := map[string][]int{}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return shells, clients
	}

	for _, p := range entries {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue // not a process directory
		}

		cmdline, err := os.ReadFile(filepath.Join("/proc", p.Name(), "cmdline"))
		if err != nil {
			continue // process exited, or not ours to read
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if len(args) == 0 || filepath.Base(args[0]) != "dtach" {
			continue
		}

		socket := ""
		for _, a := range args {
			if strings.HasSuffix(a, socketSuffix) && filepath.Dir(a) == dir {
				socket = a
				break
			}
		}
		if socket == "" {
			continue
		}

		shell, ok := sessionShell(pid)
		if !ok {
			clients[socket] = append(clients[socket], pid)
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(shell), "cwd"))
		if err != nil {
			cwd = ""
		}
		shells[socket] = sessionProc{pid: shell, cwd: cwd}
	}
	return shells, clients
}

// sessionShell returns pid's first child that is not itself a dtach process — the session's
// shell — and reports false when there is none, which means pid is a client rather than a
// master. See scanDtach for why the dtach check is load-bearing.
func sessionShell(pid int) (int, bool) {
	for _, child := range childPIDs(pid) {
		if comm(child) == "dtach" {
			continue
		}
		return child, true
	}
	return 0, false
}

func childPIDs(pid int) []int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(data)) {
		if child, err := strconv.Atoi(f); err == nil {
			out = append(out, child)
		}
	}
	return out
}

// comm reads a process's executable name. /proc/<pid>/comm rather than cmdline: it is a
// single short token with no NUL parsing, and it is what "is this dtach" needs.
func comm(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
