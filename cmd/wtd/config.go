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
// Note that bin/wt reads these from the *environment*, not from the config file — and an
// un-exported setting in wt-web-serve silently never arrives. See CLAUDE.md.
func (s *server) sessionDir() string {
	if dir := os.Getenv("WT_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dtach"
	}
	return filepath.Join(home, ".dtach")
}

func (s *server) projectsFile() string {
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
