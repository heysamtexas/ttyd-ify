package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/heysamtexas/ttyd-ify/cmd/wtd/files"
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
//
// The inbox is also listed (GET /api/v1/uploads), served back (GET /uploads/{name}) and pruned
// (DELETE /api/v1/uploads/{name}) for the page's upload panel. All three see only regular files
// directly in the inbox — not directories, not symlinks — so the panel never offers to open or
// delete something the inbox merely points at.

// maxUploadBytes caps one file. A backstop against filling the disk by accident, not a security
// control: the shell this route sits beside can write as much as it likes. The spec publishes
// it as 1 GiB. A server field carries it (uploadLimit) only so a test need not send a gigabyte.
const maxUploadBytes = 1 << 30

// maxUploadAttempts bounds the -N search. Far past any real inbox; it exists so a directory
// that answers EEXIST for everything cannot spin a handler forever.
const maxUploadAttempts = 10000

const codeInvalidFilename = "invalid_filename"

// uploadsBase is where an uploaded file is served back as itself. Outside /api/v1 because the
// response is the file, not JSON — the /docs/ and /files/ precedent.
const uploadsBase = "/uploads"

// partialDir holds uploads still arriving, inside the inbox so link(2) never crosses a
// filesystem. A directory, so it never appears in the listing, which shows regular files only.
const partialDir = ".partial"

// uploadedFile is one entry in the inbox, as the list, the upload response and the panel see it.
type uploadedFile struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	URL        string    `json:"url"`
	Bytes      int64     `json:"bytes"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

func newUploadedFile(dir string, info os.FileInfo) uploadedFile {
	return uploadedFile{
		Name:       info.Name(),
		Path:       filepath.Join(dir, info.Name()),
		URL:        uploadsBase + "/" + url.PathEscape(info.Name()),
		Bytes:      info.Size(),
		ModifiedAt: info.ModTime().UTC(),
	}
}

// inboxFile resolves a requested name to a regular file directly in the inbox, or says why not.
// Lstat, so a symlink is "not found" rather than followed.
func (s *server) inboxFile(name string) (string, os.FileInfo, error) {
	if err := validateUploadName(name); err != nil {
		return "", nil, err
	}
	p := filepath.Join(s.uploadDir(), name)
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, os.ErrNotExist
	}
	return p, info, nil
}

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
	if err := os.MkdirAll(filepath.Join(dir, partialDir), 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot create upload directory", err.Error())
		return
	}

	// Written to a temporary name first, so a cut-off upload never appears under the real one.
	// CreateTemp opens 0600, which is the mode the finished file keeps.
	tmp, err := os.CreateTemp(filepath.Join(dir, partialDir), "upload-*")
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
		info, err := os.Lstat(dst)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "cannot read back upload", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, newUploadedFile(dir, info))
		return
	}
	writeError(w, http.StatusInternalServerError, codeInternal, "no free name for upload",
		fmt.Sprintf("%q and %d suffixed alternatives all exist", name, maxUploadAttempts))
}

// handleUploadsList lists the inbox, newest first. A missing inbox is an empty one: nothing has
// been uploaded yet, and the directory is made on the first upload.
func (s *server) handleUploadsList(w http.ResponseWriter, r *http.Request) {
	dir := s.uploadDir()
	list := []uploadedFile{}
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot read upload directory", err.Error())
		return
	}
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed between the read and the stat
		}
		list = append(list, newUploadedFile(dir, info))
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].ModifiedAt.Equal(list[j].ModifiedAt) {
			return list[i].ModifiedAt.After(list[j].ModifiedAt)
		}
		return list[i].Name < list[j].Name
	})
	writeJSON(w, http.StatusOK, list)
}

func (s *server) handleUploadDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateUploadName(name); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidFilename, "invalid file name", err.Error())
		return
	}
	p, _, err := s.inboxFile(name)
	if err == nil {
		err = os.Remove(p)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, codeNotFound, "no such upload", name)
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal, "cannot delete upload", err.Error())
	default:
		logf("wtd: upload deleted: %s", p)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleUploadServe sends an uploaded file back as itself, with the /files/ browser's guarantee
// that nothing served can run script in this origin. ?download=1 asks for a save rather than a
// view.
func (s *server) handleUploadServe(w http.ResponseWriter, r *http.Request) {
	p, info, err := s.inboxFile(r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(info.Name()))
	}
	files.ServeFile(w, r, p, info)
}
