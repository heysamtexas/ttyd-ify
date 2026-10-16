package files

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

const (
	sessionCookie = "wt_files_session"
	sessionTTL    = 30 * 24 * time.Hour
	// bcrypt at this cost takes ~250ms, which is the bulk of the brute-force
	// defense; failDelay covers the rest without needing per-IP bookkeeping.
	failDelay = time.Second
	// hashCost is what Hash writes. The cost is read back from each hash, so raising this
	// only affects passwords set afterwards.
	hashCost = 12
)

// auth is the password gate on /files/ and nothing else. The rest of wtd stays
// unauthenticated, as it always has been; this gate exists because the tree it guards may
// hold material the network boundary alone was not trusted with, and it predates wtd's
// own login design (#27). When #27 lands, this is the thing it replaces.
type auth struct {
	hash []byte // bcrypt hash of the password
	key  []byte // HMAC key for session cookies
	tmpl *template.Template
	base string // mount point, e.g. "/files"
	// sleep is time.Sleep outside tests, which would otherwise pay failDelay per refusal.
	sleep func(time.Duration)
}

// newAuth reads the bcrypt hash from path, and fails closed: a missing, unreadable,
// world-readable or malformed file is an error, never a gate that lets everything through.
//
// The session key is derived from the hash itself, so there is no second secret to manage
// and changing the password invalidates every outstanding session.
func newAuth(path, base string, tmpl *template.Template) (*auth, error) {
	if path == "" {
		return nil, fmt.Errorf("no password file configured (set WT_FILES_PASSWD, or -files-passwd)")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("password file %s: %w (create one with `make files-password`)", path, err)
	}
	if info.Mode().Perm()&0o004 != 0 {
		return nil, fmt.Errorf("password file %s is world-readable (%04o); chmod 0640 it", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("password file %s: %w", path, err)
	}
	hash := []byte(strings.TrimSpace(string(raw)))
	if _, err := bcrypt.Cost(hash); err != nil {
		return nil, fmt.Errorf("password file %s does not contain a bcrypt hash: %w", path, err)
	}
	sum := sha256.Sum256(append([]byte("wtd-files-session-v1|"), hash...))
	return &auth{hash: hash, key: sum[:], tmpl: tmpl, base: base, sleep: time.Sleep}, nil
}

func (a *auth) check(password string) bool {
	return bcrypt.CompareHashAndPassword(a.hash, []byte(password)) == nil
}

func (a *auth) mintSession() string {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	return exp + "." + a.sign(exp)
}

func (a *auth) sign(msg string) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *auth) validSession(v string) bool {
	exp, sig, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	ts, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > ts {
		return false
	}
	return hmac.Equal([]byte(a.sign(exp)), []byte(sig))
}

// require gates everything behind a session cookie or HTTP Basic credentials.
// Browsers get redirected to the login form; anything else (curl, scripts) gets
// a 401 with a Basic challenge so `curl -u :PASSWORD` works.
//
// r.URL here is relative to the mount point (http.StripPrefix has run), so every URL sent
// back to the client is rebuilt with a.base in front.
func (a *auth) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && a.validSession(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if _, password, ok := r.BasicAuth(); ok {
			if a.check(password) {
				next.ServeHTTP(w, r)
				return
			}
			a.sleep(failDelay)
		}
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			back := a.base + r.URL.RequestURI()
			http.Redirect(w, r, a.base+"/login?next="+url.QueryEscape(back), http.StatusSeeOther)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="wtd files"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func (a *auth) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	next := a.safeNext(r.FormValue("next"))
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		a.loginPage(w, next, "", http.StatusOK)
	case http.MethodPost:
		if !a.check(r.FormValue("password")) {
			a.sleep(failDelay)
			a.loginPage(w, next, "Wrong password.", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:  sessionCookie,
			Value: a.mintSession(),
			// Scoped to the mount point. Nothing else on this server reads it, and the
			// terminal half of the origin has no business carrying it.
			Path:     a.base + "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(sessionTTL.Seconds()),
			// no Secure flag: this serves plain HTTP over an encrypted tailnet
		})
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *auth) loginPage(w http.ResponseWriter, next, errMsg string, code int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = a.tmpl.ExecuteTemplate(w, "login", map[string]string{
		"Next": next, "Error": errMsg, "Action": a.base + "/login",
	})
}

// safeNext keeps the post-login redirect inside the mount point: a rooted path under
// a.base only — never a scheme-relative "//evil.example" that browsers treat as absolute,
// and never the terminal half of the server, which a login has nothing to say about.
func (a *auth) safeNext(v string) string {
	if strings.HasPrefix(v, a.base+"/") && !strings.HasPrefix(v, "//") && !strings.Contains(v, `\`) {
		return v
	}
	return a.base + "/"
}

// Hash prints the bcrypt hash of a password read from in (asking twice, echo off, when in is
// a terminal). With in piped it reads one line instead, so the hash can be generated
// non-interactively: `printf %s "$PW" | wtd -files-hash`.
func Hash(in *os.File, out, prompts io.Writer) error {
	first, err := readPassword(in, prompts, "New /files/ password: ")
	if err != nil {
		return err
	}
	if term.IsTerminal(int(in.Fd())) {
		second, err := readPassword(in, prompts, "Repeat: ")
		if err != nil {
			return err
		}
		if string(first) != string(second) {
			return fmt.Errorf("passwords do not match")
		}
	}
	if len(first) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword(first, hashCost)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(hash))
	return err
}

func readPassword(in *os.File, prompts io.Writer, prompt string) ([]byte, error) {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err == io.EOF && line != "" {
			err = nil // input without a trailing newline is still a password
		}
		return []byte(strings.TrimRight(line, "\r\n")), err
	}
	fmt.Fprint(prompts, prompt)
	pw, err := term.ReadPassword(fd)
	fmt.Fprintln(prompts)
	return pw, err
}
