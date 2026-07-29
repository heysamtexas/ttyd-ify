package main

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Where sessions and project shortcuts live.
//
// These defaults MUST match bin/wt's, because both read the same state and a mismatch
// would make the API and the terminal menu disagree about what exists — the failure this
// whole design is built to avoid. bin/wt:18 and bin/wt:31 are the source of truth:
//
//	DIR="${WT_DIR:-$HOME/.dtach}"
//	PROJ_FILE="${WT_PROJECTS:-$HOME/.config/wt/projects}"
//
// Precedence is -session-dir, then WT_DIR, then the default. The flag wins because it carries the
// operator's config file, which beats the environment everywhere else in this project too; the
// environment is kept because it is how a developer overrides one run, and how bin/wt is still
// pointed at a scratch directory over SSH.
func (s *server) sessionDir() string {
	if s.sessionDirFlag != "" {
		return filepath.Clean(s.sessionDirFlag)
	}
	if dir := os.Getenv("WT_DIR"); dir != "" {
		// Cleaned, because an uncleaned value silently breaks two things that compare paths
		// rather than open them. scanDtach matches a dtach argv against this with
		// filepath.Dir, which never yields a trailing slash, so `WT_DIR=/a/b/` blinds signal 2
		// entirely and nulls pid/cwd for every session. And sessionNameRoom measures a
		// filepath.Join-ed path while bin/wt measures "$DIR/$1.sock" literally, so the two
		// disagree by a byte about where the socket-path ceiling is. bin/wt strips its own
		// trailing slash for the same reason.
		return filepath.Clean(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dtach"
	}
	return filepath.Join(home, ".dtach")
}

// warnSessionDirDepth reports a WT_DIR deep enough to interfere with session sockets.
//
// Not fatal, and deliberately not: short names may still fit, and sessions get created either
// way. But a socket path over maxSocketPathLen cannot be named in a connect(2), so nothing can
// reach it — not this server's probes, and not `dtach -a` from bin/wt or over SSH. The symptom
// is an absence rather than an error, so it is worth one line at startup (#5).
//
// This warns; the create path and the deep link both refuse (POST via validateSocketPath,
// `?arg=` via bin/wt's sock_fits). The bash menu's new-session prompt still hands the name
// straight to dtach, so for that one path the warning is the only cover there is.
func warnSessionDirDepth(dir string) {
	switch room := sessionNameRoom(dir); {
	case room < 1:
		log.Printf("wtd: WARNING WT_DIR=%q leaves no room for any session socket to be reachable "+
			"by name (a socket path may be at most %d bytes): sessions can be created but not "+
			"attached", dir, maxSocketPathLen)
	case room < maxSessionNameLen:
		log.Printf("wtd: WARNING WT_DIR=%q leaves room for session names of only %d characters "+
			"instead of %d; longer names will be refused", dir, room, maxSessionNameLen)
	}
}

func (s *server) projectsFile() string {
	if s.projectsFileFlag != "" {
		return s.projectsFileFlag
	}
	if f := os.Getenv("WT_PROJECTS"); f != "" {
		return f
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "wt", "projects")
}

// loadProjects parses the shortcut file: one "name /absolute/path" per line, blank lines
// and #comments ignored. Same format bin/wt:32-37 reads. A missing or unreadable file
// means no shortcuts, not an error — that is the normal state on a fresh install.
func loadProjects(path string) map[string]string {
	out := map[string]string{}
	if path == "" {
		return out
	}

	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close() //nolint:errcheck // read-only

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// bin/wt uses `read -r _name _path`, which splits on whitespace and puts the
		// remainder in _path — so a path containing spaces keeps them.
		name, rest, ok := strings.Cut(line, " ")
		if !ok {
			if name, rest, ok = strings.Cut(line, "\t"); !ok {
				continue
			}
		}
		rest = strings.TrimSpace(rest)
		if name == "" || rest == "" {
			continue
		}
		out[name] = rest
	}
	return out
}

// logf exists so handlers can log without importing log everywhere, and so the call sites
// read as decisions rather than plumbing.
func logf(format string, args ...any) { log.Printf(format, args...) }
