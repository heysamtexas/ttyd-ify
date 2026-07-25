package main

import (
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
type Session struct {
	Name      string    `json:"name"`
	Attached  bool      `json:"attached"`
	CWD       string    `json:"cwd,omitempty"`
	PID       int       `json:"pid,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
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

		s := Session{
			Name:      name,
			Attached:  attachedTo(name, info, clients[path], stats),
			CreatedAt: info.ModTime(),
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

// attachedTo answers "is someone looking at this session".
//
// That sentence is the API contract (api/openapi.yaml, Session.attached); the derivation
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
func attachedTo(name string, info os.FileInfo, clientPIDs []int, stats map[string]hubStat) bool {
	st, held := stats[name]
	if st.clients > 0 {
		return true
	}

	for _, pid := range clientPIDs {
		pgid, err := syscall.Getpgid(pid)
		if err != nil {
			continue // exited between the scan and now; it is attached to nothing
		}
		if _, own := st.pgids[pgid]; held && own {
			continue // wtd's own held attachment, which is not a viewer
		}
		return true
	}

	if !held {
		// Verified against live sessions: srwx------ attached, srw------- idle.
		return info.Mode().Perm()&0o100 != 0
	}
	return false
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
