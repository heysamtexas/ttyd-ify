package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// ringStore persists each session's replay ring across a wtd restart, so the first attach
// after `systemctl restart` replays context instead of a blank screen (#92). Without it the
// only recovery on attach is the SIGWINCH kick, which a full-screen program answers with a
// repaint and an inline-mode program cannot — its scrolled-off output exists nowhere it can
// be regenerated from.
//
// Where the files live is a security decision, not a convenience: raw terminal output can
// carry secrets, so the directory the unit provides is tmpfs (RuntimeDirectory=wt, mode 0700,
// RuntimeDirectoryPreserve=restart) — the bytes never touch persistent storage, and systemd
// removes them at stop and at reboot, which is when the sessions' buffers should die anyway.
// wtd itself writes every file 0600 and never creates the directory: an operator pointing
// -state-dir somewhere is asserting that place is as private as the terminal it buffers.
//
// The session name becomes a filename here, and the name is untrusted network input. save
// and load both re-apply the attach-side rejection (validateAttachName: no "/", no "..",
// socket path must fit) before joining it into a path — the same gate the socket path gets,
// which is the property that makes <name>.ring safe wherever <name>.sock was.
//
// A saved ring is only evidence about the past, so two rules keep it from lying:
//
//   - load consumes the file: unlinked on every attempt, returned or not. Replay describes
//     "what you missed", and a tail that reappeared on every future spawn would describe
//     nothing. The cost is paid on failure too — if the spawn that asked for the bytes then
//     dies, the retry gets an empty ring and the kick, which is exactly the pre-save
//     behavior — one restore attempt per shutdown, never a stale echo.
//   - load returns bytes only for a session that provably outlived the restart. Provably is
//     probeSocket == socketListening, NOT a stat: a kill -9'd master leaves its socket file
//     behind, S_ISSOCK and all (the reason reapStale exists — see sessionops.go), and
//     `dtach -A` recreates a session right over that file, so a stat gate would replay a
//     dead session's output into the fresh session that inherits its name. Reaping needs
//     provable death; restoring needs provable life; socketUnknown restores nothing for the
//     same reason it unlinks nothing. The check is racy in the harmless direction only — a
//     listener alive at load can die before dtach attaches, and that spawn fails exactly as
//     it does today.
//
// Output produced while wtd was down is not here and cannot be: dtach keeps no buffer, so
// those bytes were never observed. hub.gapNotice is what tells the client about that gap.
type ringStore struct {
	dir        string // where .ring files live; created by systemd, not by wtd
	sessionDir string // where dtach sockets live; the liveness gate for restores
}

const ringSuffix = ".ring"

// save writes one session's ring snapshot, atomically: a crash mid-write must not leave a
// torn file whose truncated tail ends inside an escape sequence, because load has no framing
// scanner — the ring re-scans on write, but only trims the head, never a corrupt tail.
func (rs *ringStore) save(name string, data []byte) error {
	if err := validateAttachName(rs.sessionDir, name); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil // nothing to replay is a state load already handles; no file says it better
	}
	path := filepath.Join(rs.dir, name+ringSuffix)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		// Removed now rather than left for the next startup's sweep: /run is size-capped
		// tmpfs, and a partial file is RAM held for no one.
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// load consumes the saved ring for name — see the two rules in the type comment. Callers
// get nil for every kind of absence — no file, dead or unprovable session, unusable name —
// because the answer to all of them is the same: spawn with an empty ring, exactly as
// before the store existed.
func (rs *ringStore) load(name string) []byte {
	if validateAttachName(rs.sessionDir, name) != nil {
		return nil
	}
	path := filepath.Join(rs.dir, name+ringSuffix)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	_ = os.Remove(path)
	if probeSocket(filepath.Join(rs.sessionDir, name+socketSuffix)) != socketListening {
		return nil
	}
	return data
}

// sweep unlinks saved rings whose session no longer provably exists, plus any .tmp a crash
// mid-save left behind. Run once at startup: load already refuses a dead session's file, so
// this is hygiene — state for sessions that died with the server should not sit in tmpfs
// waiting for a namesake — not a correctness gate.
func (rs *ringStore) sweep() {
	entries, err := os.ReadDir(rs.dir)
	if err != nil {
		return
	}
	removed := 0
	for _, entry := range entries {
		fname := entry.Name()
		var remove bool
		switch {
		case strings.HasSuffix(fname, ringSuffix+".tmp"):
			// A save that crashed mid-write; no save is in flight at startup.
			remove = true
		case strings.HasSuffix(fname, ringSuffix):
			// Same gate as load, for the same reason: a stale socket passes a stat.
			name := strings.TrimSuffix(fname, ringSuffix)
			remove = probeSocket(filepath.Join(rs.sessionDir, name+socketSuffix)) != socketListening
		default:
			// Not ours. The directory is wtd's, but deleting blind is how bugs eat data.
			remove = false
		}
		if remove && os.Remove(filepath.Join(rs.dir, fname)) == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Printf("wtd: removed %d saved replay file(s) for sessions that no longer exist", removed)
	}
}
