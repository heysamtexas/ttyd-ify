package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
// Missing pid/cwd is not an error: those come from /proc and are best-effort enrichment.
func listSessions(dir string) ([]Session, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No socket directory yet simply means no sessions. bin/wt mkdir -p's it on
			// first run, so this is the normal state on a fresh install, not a fault.
			return []Session{}, nil
		}
		return nil, fmt.Errorf("read session dir %s: %w", dir, err)
	}

	masters := dtachMasters(dir)

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
			Name: name,
			// dtach marks the socket executable while a client is attached and clears it
			// on detach. Verified against live sessions: srwx------ attached,
			// srw------- idle. Cheaper and more reliable than parsing /proc/net/unix,
			// and it is dtach's own signalling rather than a guess.
			Attached:  info.Mode().Perm()&0o100 != 0,
			CreatedAt: info.ModTime(),
		}
		if m, ok := masters[path]; ok {
			s.PID = m.pid
			s.CWD = m.cwd
		}
		sessions = append(sessions, s)
	}

	// Stable order so a client's list does not reshuffle between polls.
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Name < sessions[j].Name })
	return sessions, nil
}

type master struct {
	pid int
	cwd string
}

// dtachMasters maps socket path -> the dtach process supervising it, plus the working
// directory of the shell inside.
//
// Two dtach processes can reference one socket: the master that created it and a client
// that is attached to it. Only the master has a child (the session's shell), which is how
// they are told apart — and it is the child's cwd that is interesting, since the master's
// own cwd is wherever the session happened to be started from.
func dtachMasters(dir string) map[string]master {
	out := map[string]master{}

	procs, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}

	for _, p := range procs {
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

		child, ok := firstChild(pid)
		if !ok {
			continue // an attached client, not the master
		}
		cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(child), "cwd"))
		if err != nil {
			cwd = ""
		}
		out[socket] = master{pid: pid, cwd: cwd}
	}
	return out
}

// firstChild returns the first child pid of pid, via the thread's children file.
func firstChild(pid int) (int, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return 0, false
	}
	for _, f := range strings.Fields(string(data)) {
		if child, err := strconv.Atoi(f); err == nil {
			return child, true
		}
	}
	return 0, false
}
