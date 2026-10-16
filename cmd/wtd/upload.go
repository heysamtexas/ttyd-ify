package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// File upload: the terminal page's ↑ button. One file per request, its bytes as the body and its
// name in ?name=, written into one inbox directory (-upload-dir, default ~/uploads).
//
// This widens nothing about who can reach the shell: the route sits behind the same gate as /ws,
// and anyone past that gate already has a writable shell as this same user. What it adds is a
// CSRF surface — another page the user has open could otherwise write files here — and that is
// closed the way the sessions API closes it (guardMutatingAs): the Origin check, and a body type
// a cross-site <form> cannot send. That is why this is a raw application/octet-stream body and
// not multipart/form-data, which is exactly what a form sends.
//
// An upload never overwrites. A name already taken gets -1, -2, … before its extension, claimed
// with link(2), which fails rather than replaces when the name exists — so two uploads racing for
// the same name both land, and neither clobbers the other or a file the user made by hand.

// maxUploadBytes caps one file. A backstop against filling the disk by accident, not a security
// control: the shell this route sits beside can write as much as it likes. The spec publishes
// it as 1 GiB. A server field carries it (uploadLimit) only so a test need not send a gigabyte.
const maxUploadBytes = 1 << 30

// maxUploadAttempts bounds the -N search. Far past any real inbox; it exists so a directory
// that answers EEXIST for everything cannot spin a handler forever.
const maxUploadAttempts = 10000

const codeInvalidFilename = "invalid_filename"

// uploadDir is where uploads land. Precedence matches sessionDir's: the flag, then the default.
// No environment variable, for #28's reason — config reaches wtd as arguments.
func (s *server) uploadDir() string {
	if s.uploadDirFlag != "" {
		return filepath.Clean(s.uploadDirFlag)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "uploads")
}

// validateUploadName accepts what a browser reports as a file's name, which is the user's own
// choice of name and so may hold spaces and any script. It refuses what could escape the inbox
// or confuse a terminal that later prints it: path separators, "." and "..", control bytes and
// invalid UTF-8. The name is untrusted input arriving over the network, like a session name.
func validateUploadName(name string) error {
	switch {
	case name == "":
		return errors.New("name is empty")
	case len(name) > 255:
		return errors.New("name is longer than 255 bytes")
	case name == "." || name == "..":
		return errors.New("name must not be . or ..")
	case strings.ContainsAny(name, `/\`):
		return errors.New("name must not contain a path separator")
	case !utf8.ValidString(name):
		return errors.New("name is not valid UTF-8")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return errors.New("name must not contain control characters")
		}
	}
	return nil
}

// suffixedName is the n'th alternative to name: "shot.png" → "shot-1.png". A name whose only dot
// leads it (".env") is all stem, so it becomes ".env-1" rather than "-1.env".
func suffixedName(name string, n int) string {
	if n == 0 {
		return name
	}
	ext := filepath.Ext(name)
	if ext == name {
		ext = ""
	}
	return strings.TrimSuffix(name, ext) + "-" + strconv.Itoa(n) + ext
}

func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if err := validateUploadName(name); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidFilename, "invalid file name", err.Error())
		return
	}
	dir := s.uploadDir()
	if dir == "" {
		writeError(w, http.StatusInternalServerError, codeInternal, "no upload directory",
			"the service user has no home directory and -upload-dir is unset")
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot create upload directory", err.Error())
		return
	}

	// Written to a temporary name first, so a cut-off upload never appears under the real one.
	// CreateTemp opens 0600, which is the mode the finished file keeps.
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot write upload", err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, s.uploadLimit))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
				"file is too large", fmt.Sprintf("the limit is %d bytes", s.uploadLimit))
			return
		}
		writeError(w, http.StatusBadRequest, codeBadRequest, "upload did not complete", err.Error())
		return
	}

	for i := 0; i < maxUploadAttempts; i++ {
		final := suffixedName(name, i)
		dst := filepath.Join(dir, final)
		err := os.Link(tmp.Name(), dst)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "cannot write upload", err.Error())
			return
		}
		logf("wtd: upload: %s (%d bytes)", dst, n)
		writeJSON(w, http.StatusCreated, map[string]any{"name": final, "path": dst, "bytes": n})
		return
	}
	writeError(w, http.StatusInternalServerError, codeInternal, "no free name for upload",
		fmt.Sprintf("%q and %d suffixed alternatives all exist", name, maxUploadAttempts))
}
