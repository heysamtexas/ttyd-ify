package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
// Two decisions follow, and they are why this file only ever reads a file:
//
//  1. The summary is NOT made here. bin/wt-narrate makes it, from a Claude Code hook, at the
//     moment a turn ends. That keeps wtd at two dependencies with no outbound network call and no
//     API key anywhere near /etc/ttyd-ify/config -- and it makes the model's latency free, because
//     the turn is already over and nobody is waiting for the answer. The measurement that decides
//     this, and the exception to it, are in bin/wt-narrate's header.
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
// The files live under -state-dir, which is systemd's $RUNTIME_DIRECTORY: tmpfs, 0700, gone at
// reboot. Same reasoning ringstore.go states for the same directory -- a summary of a terminal
// session can quote a secret out of it, so it must not persist on disk. It is NOT gone at
// restart (RuntimeDirectoryPreserve=restart), which is why sweepNarration exists.

const (
	// narrationSubdir keeps the summaries out of the ring store's namespace, which owns *.ring in
	// the state directory root.
	narrationSubdir = "narration"
	narrationSuffix = ".json"
	// narrationTmpInfix is what bin/wt-narrate's mktemp template puts in a partial write, so a
	// crash mid-write leaves something sweepNarration can recognize as its own litter.
	narrationTmpInfix = narrationSuffix + ".tmp."
)

// maxNarrationBytes bounds the read. The writer is a local process, so this is not a defense
// against an attacker; it is a defense against serving something that cannot be a summary because
// it is far too large to ever be spoken.
const maxNarrationBytes = 64 << 10

// Narration is what a client gets.
//
// Field semantics are client-facing rules, so they live in api/openapi.yaml's Narration schema
// rather than here -- read that for what a client must do with `at` and `needsYou`. What matters
// on this side is only which fields carry behaviour, because those are the ones readNarration
// refuses to serve a file without.
type Narration struct {
	Session  string    `json:"session"`
	Event    string    `json:"event"`
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

// usableNarrationName rejects a session name that must not become a filename.
//
// One implementation for both callers on purpose: readNarration and dropNarration both join this
// into a path and one of them unlinks, so two copies that could disagree is the shape of the bug
// rather than a defense against it. Deliberately not validateAttachName, which also enforces a
// socket-path length bound that says nothing about a .json file in a different directory --
// unifying the two is #117.
func usableNarrationName(name string) bool {
	return name != "" && !strings.ContainsRune(name, '/') && !strings.Contains(name, "..")
}

// readNarration loads and checks one summary.
//
// The name must already have been matched against a real session by the caller. This check is not
// a redundant second copy of that one -- they are independent and neither implies the other. The
// caller's list match is what proves the session exists; this is what makes the function safe for
// the next caller, who may not do a list match at all.
func readNarration(dir, name string) (Narration, error) {
	var n Narration
	if dir == "" {
		return n, os.ErrNotExist
	}
	if !usableNarrationName(name) {
		return n, fmt.Errorf("unusable session name %q", name)
	}

	f, err := os.Open(filepath.Join(dir, name+narrationSuffix))
	if err != nil {
		return n, err
	}
	defer f.Close()

	// One byte past the limit, so a file at exactly the bound is served and one over is refused
	// rather than silently truncated into a decode error.
	raw, err := io.ReadAll(io.LimitReader(f, maxNarrationBytes+1))
	if err != nil {
		return n, fmt.Errorf("summary for %q is not readable: %w", name, err)
	}
	if len(raw) > maxNarrationBytes {
		return n, fmt.Errorf("summary for %q is larger than %d bytes", name, maxNarrationBytes)
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return n, fmt.Errorf("summary for %q is not readable: %w", name, err)
	}

	// Decoded and checked rather than relayed, so a half-written file is an error here instead of
	// a client's problem. Three fields carry behaviour and are `required` in the schema, so all
	// three are enforced: without At a client cannot tell a new summary from one it already
	// spoke, without Headline there is nothing to say, and Event is what a client switches on.
	switch {
	case n.At.IsZero():
		return Narration{}, fmt.Errorf("summary for %q has no usable timestamp", name)
	case strings.TrimSpace(n.Headline) == "":
		return Narration{}, fmt.Errorf("summary for %q has no headline", name)
	case n.Event == "":
		return Narration{}, fmt.Errorf("summary for %q names no event", name)
	}
	// The file names its own session; trust the filename over the contents, since that is what the
	// caller matched against the session list.
	n.Session = name
	return n, nil
}

// dropNarration removes a session's summary. Best effort by design: every failure mode here is
// either "there was nothing to remove" or a runtime directory problem that the delete it
// accompanies should not be reported as.
func dropNarration(dir, name string) {
	if dir == "" || !usableNarrationName(name) {
		return
	}
	_ = os.Remove(filepath.Join(dir, name+narrationSuffix))
}

// sweepNarration unlinks summaries whose session no longer provably exists, plus any partial write
// a crash left behind. Called once at startup.
//
// This is not housekeeping. The runtime directory is preserved across a restart
// (RuntimeDirectoryPreserve=restart in systemd/wt.service), so without this a summary outlives the
// session it describes and the browser client will speak it: turning voice on deliberately asks
// for the current summary, which is the right behaviour and the reason a stale one is dangerous.
// A listener has no way to tell a summary of a turn from before the restart from a current one.
//
// Same gate and the same reasoning as ringStore.sweep, which exists for the identical hazard on
// the identical directory -- and deliberately the same conservatism: a file this does not
// recognize is left alone, because deleting blind in a directory wtd shares with nothing is still
// how bugs eat data.
func sweepNarration(dir, sessionDir string) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	removed := 0
	for _, entry := range entries {
		fname := entry.Name()
		var remove bool
		switch {
		case strings.Contains(fname, narrationTmpInfix):
			// A hook that died mid-write; nothing is in flight at startup.
			remove = true
		case strings.HasSuffix(fname, narrationSuffix):
			name := strings.TrimSuffix(fname, narrationSuffix)
			remove = probeSocket(filepath.Join(sessionDir, name+socketSuffix)) != socketListening
		default:
			remove = false
		}
		if remove && os.Remove(filepath.Join(dir, fname)) == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Printf("wtd: removed %d narration file(s) for sessions that no longer exist", removed)
	}
}

// handleNarrationGet serves the summary for one session.
//
// The name is matched byte-exact against the real session list before any path is built from it --
// the same rule handleSessionGet and deleteSession follow.
//
// What 404 means, and what a client must do about it, is api/openapi.yaml's to state; it is a rule
// about client behaviour and this is the implementation of it. The short version, because it looks
// like an error and is not: a session with no summary is the ordinary case.
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
