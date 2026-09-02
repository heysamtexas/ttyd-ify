package main

// Prompt history: the messages a human has sent to the agent in a session, so a client can list
// them without hunting through scrollback.
//
// **wtd only reads.** The file is written by bin/wt-prompt-hook, a Claude Code UserPromptSubmit
// hook running inside the session, and nothing in this server ever creates or modifies it. That
// split is the whole design and it is worth stating plainly, because the alternative was tempting:
// the prompt text also passes through this server as terminal output, so a scanner could have
// lifted it out of the stream the way status.go lifts the agent status. Three reasons it does not.
//
// The prompt in the stream is a *rendered TUI*, not a message. An agent redraws its input box
// constantly, with cursor addressing, history recall and re-editing interleaved, so reconstructing
// "the text that was submitted" from those bytes is guesswork -- and the same scanner would
// reconstruct a password typed at a sudo prompt just as eagerly.
//
// The bytes are evicted by output volume. They live in a byte-budgeted ring whose job is replaying
// a screen, so a busy session loses its prompt list exactly when the list is most wanted, and a
// multi-kilobyte payload per prompt would displace the output the ring exists for.
//
// And a hook already knows the answer exactly. It is handed the submitted text as a string, which
// is the thing being asked for, with no parsing and nothing to get wrong.
//
// api/ws-protocol.md section 6a's in-band convention was considered for this and rejected for the
// same reasons; the note there explains why it suits a four-value enum and not a human's prose.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// maxPromptFileBytes bounds what this server will read from one prompt file.
//
// The hook keeps 50 prompts of at most 2000 characters, so a well-behaved file is well under
// this. The bound is not about the hook: the file is on tmpfs in the service user's runtime
// directory, and a route that reads a file into memory on every poll must have a ceiling that
// does not depend on the good behaviour of whatever wrote it.
const maxPromptFileBytes = 256 << 10

const promptsSubdir = "prompts"

// prompt is one message, as the hook recorded it.
type prompt struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	// Truncated marks a prompt the hook shortened, so a client can say so rather than
	// presenting a cut-off sentence as the whole thing.
	Truncated bool `json:"truncated"`
}

// promptFile is the on-disk shape, which is also the wire shape. One type rather than two: the
// file is written by a script in this repo and read by a client, and an intermediate translation
// would be a third place for the format to drift.
type promptFile struct {
	Session string `json:"session"`
	// Prompts is oldest-first, which is the order they were said in. A nil slice marshals to
	// null, so this is always initialised -- an absent file means "nothing recorded", which is
	// an empty list and not unknown.
	Prompts []prompt `json:"prompts"`
}

// promptDir is where prompt files live, or "" when this server has no state directory at all.
func (s *server) promptDir() string {
	if s.stateDir == "" {
		return ""
	}
	return filepath.Join(s.stateDir, promptsSubdir)
}

// readPrompts loads one session's recorded prompts.
//
// Every kind of absence is an empty list, not an error: no state directory, no prompts directory,
// no file for this session, or a file this server cannot parse. They all mean the same thing to a
// client -- nothing has been recorded for this session -- and the most likely cause by far is that
// nobody has installed the hook, which is not a server fault to report as a 500.
func (s *server) readPrompts(name string) []prompt {
	dir := s.promptDir()
	if dir == "" {
		return nil
	}
	// The name arrived from the network and is about to become a filename. Re-applying the
	// attach-side rejection here is the same rule ringstore follows, and it is not made
	// redundant by the hook refusing bad names too: this side must hold on its own, because the
	// only thing standing between a request and a path is this check.
	if err := validateAttachName(s.sessionDir(), name); err != nil {
		return nil
	}
	f, err := os.Open(filepath.Join(dir, name+".json"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var file promptFile
	// LimitReader rather than ReadFile: a bounded read cannot be talked into allocating more
	// than the bound, whatever the file claims to be.
	if err := json.NewDecoder(io.LimitReader(f, maxPromptFileBytes)).Decode(&file); err != nil {
		logf("wtd: prompts for %q are unreadable, reporting none: %v", name, err)
		return nil
	}
	return file.Prompts
}

// handleSessionPrompts serves one session's prompt history.
//
// The existence check is a stat of the session's socket, deliberately not listSessions. That
// listing walks all of /proc to resolve a pid and cwd for every session on the box, and this route
// needs neither -- it reports no state about the session at all. #115 is the standing version of
// that rule, and this is the second route it applies to. It also must not reap as a side effect,
// for the same reason: this is built to be polled.
//
// A stat answers "is there a session by this name" the same way the listing would before reaping,
// so a stale socket reads as an existing session with no prompts. That is the same answer
// GET /api/v1/host gives, and it resolves itself the next time anything lists.
func (s *server) handleSessionPrompts(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// Validated before it is joined to anything. A rejected name is reported as not found
	// rather than as a bad request: no such session can exist, and the distinction between
	// "unusable" and "absent" is not one a client can act on differently.
	if err := validateAttachName(s.sessionDir(), name); err != nil {
		writeError(w, http.StatusNotFound, codeNotFound, "no such session", name)
		return
	}
	socket := filepath.Join(s.sessionDir(), name+socketSuffix)
	info, err := os.Stat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			logf("wtd: prompts for %q: stat %s: %v", name, socket, err)
		}
		writeError(w, http.StatusNotFound, codeNotFound, "no such session", name)
		return
	}

	prompts := s.readPrompts(name)
	if prompts == nil {
		// Always an array on the wire. Null would mean "unknown", and this route cannot
		// distinguish "no hook installed" from "nothing said yet" -- both are none.
		prompts = []prompt{}
	}
	writeJSON(w, http.StatusOK, promptFile{Session: name, Prompts: prompts})
}
