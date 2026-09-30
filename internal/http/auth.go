package http

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Password sign-in. One user, one password (MUSICLIB_PASSWORD), no user
// name. A successful sign-in creates a session in memory, so a restart
// signs the user out; the browser keeps its random id in a cookie. Every
// route needs a session except /login, /logout and /static/ (and the
// health endpoints, which cmd/musiclibd serves outside this package).
const (
	// MinPasswordChars and MaxPasswordBytes bound the password. The upper
	// bound keeps it within the sign-in form's body limit.
	MinPasswordChars = 12
	MaxPasswordBytes = 1024

	sessionCookie = "musiclib_session"
	// sessionIDLen is the length of a session id: 32 random bytes in
	// unpadded base64url.
	sessionIDLen = 43
	// sessionMaxAge is fixed from the sign-in: using a session does not
	// extend it.
	sessionMaxAge = 30 * 24 * time.Hour
	// signInDelay is how long a refused attempt holds the sign-in slot.
	signInDelay = time.Second
	// signInBodyBytes fits the longest password percent-encoded (3 bytes
	// per byte) after "password=".
	signInBodyBytes = 4 << 10
)

// CheckPassword refuses a password the sign-in form could never match: an
// empty one, one that is not UTF-8, one longer than MaxPasswordBytes, one
// with a control character (a line break left in .env, a tab), and one of
// fewer than MinPasswordChars characters. The error names the rule, never
// the value or its length.
func CheckPassword(p string) error {
	switch {
	case p == "":
		return errors.New("is missing or empty")
	case !utf8.ValidString(p):
		return errors.New("is not valid UTF-8")
	case len(p) > MaxPasswordBytes:
		return fmt.Errorf("is longer than %d bytes", MaxPasswordBytes)
	case strings.ContainsFunc(p, unicode.IsControl):
		return errors.New("contains a control character, such as a line break or a tab")
	case utf8.RuneCountInString(p) < MinPasswordChars:
		return fmt.Errorf("is shorter than %d characters", MinPasswordChars)
	}
	return nil
}

// loginView is the sign-in page; Failed says that the last attempt was
// refused.
type loginView struct{ Failed bool }

// sessions are the live sessions: the SHA-256 of each id and its expiry.
// Expired entries are deleted when met and at every sign-in; nothing runs
// in the background.
type sessions struct {
	mu sync.Mutex
	m  map[[sha256.Size]byte]time.Time
}

// create starts a session and returns its id.
func (s *sessions) create(now time.Time) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(raw[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, expiry := range s.m {
		if !now.Before(expiry) {
			delete(s.m, k)
		}
	}
	s.m[sha256.Sum256([]byte(id))] = now.Add(sessionMaxAge)
	return id, nil
}

// valid reports whether id is a live session.
func (s *sessions) valid(id string, now time.Time) bool {
	if len(id) != sessionIDLen {
		return false
	}
	k := sha256.Sum256([]byte(id))
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.m[k]
	if ok && !now.Before(expiry) {
		delete(s.m, k)
		return false
	}
	return ok
}

func (s *sessions) remove(id string) {
	k := sha256.Sum256([]byte(id))
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, k)
}

// signedIn reports whether r carries a live session.
func (a *API) signedIn(r *http.Request) bool {
	now := time.Now()
	for _, c := range r.CookiesNamed(sessionCookie) {
		if a.sessions.valid(c.Value, now) {
			return true
		}
	}
	return false
}

// cookie is the session cookie: never readable by scripts, never sent by
// another site, Secure when PUBLIC_ORIGIN is https, for the whole host.
func (a *API) cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true,
		Secure: a.secure, SameSite: http.SameSiteStrictMode}
}

// login is /login: the page (GET, HEAD) and the sign-in (POST). Signed in
// already, the page sends the browser on to the Library.
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if a.signedIn(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		a.execute(w, r, http.StatusOK, "login.html", pageData{Title: "Sign in", Login: &loginView{}})
	case http.MethodPost:
		a.signIn(w, r)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		a.writeError(w, newError(http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			"%s is not allowed here; allowed: GET, HEAD, POST", r.Method))
	}
}

// signIn is POST /login, a form with one field, password. The order is the
// guarantee: the body is read before the slot is taken, so a slow client
// never holds it; the comparison and the delay of a refusal happen inside
// the slot, so guesses go one at a time and at most one per signInDelay,
// whatever the concurrency; the delay is not cut short when the client
// leaves, so its timing tells nothing.
func (a *API) signIn(w http.ResponseWriter, r *http.Request) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		a.writeError(w, newError(http.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
			"the sign-in is a form: Content-Type must be application/x-www-form-urlencoded"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, signInBodyBytes)
	err := r.ParseForm()
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		a.writeError(w, newError(http.StatusRequestEntityTooLarge, CodeBodyTooLarge,
			"the sign-in form is larger than %d bytes", signInBodyBytes).with("limit", signInBodyBytes))
		return
	}
	// Any other malformed form is a refused attempt like a wrong password.
	// The body only: a password in the URL is ignored.
	password := ""
	if err == nil {
		password = r.PostForm.Get("password")
	}
	ok, checked := a.attempt(r.Context(), password)
	if !checked {
		return // The client left while waiting for its turn.
	}
	if !ok {
		a.log.Warn("sign-in refused", "remote_addr", r.RemoteAddr)
		a.execute(w, r, http.StatusUnauthorized, "login.html", pageData{Title: "Sign in", Login: &loginView{Failed: true}})
		return
	}
	id, err := a.sessions.create(time.Now())
	if err != nil {
		a.log.Error("creating a session", "error", err.Error())
		a.writeError(w, internalError())
		return
	}
	http.SetCookie(w, a.cookie(id, int(sessionMaxAge/time.Second)))
	a.log.Info("signed in", "remote_addr", r.RemoteAddr)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// attempt checks password in the sign-in slot: it waits for its turn
// (checked is false if ctx ends first), compares the SHA-256 digests in
// constant time, and on a mismatch holds the slot for signInDelay, which
// ctx does not cut short.
func (a *API) attempt(ctx context.Context, password string) (ok, checked bool) {
	select {
	case a.signIns <- struct{}{}:
	case <-ctx.Done():
		return false, false
	}
	defer func() { <-a.signIns }()
	got := sha256.Sum256([]byte(password))
	ok = subtle.ConstantTimeCompare(got[:], a.password[:]) == 1
	if !ok {
		time.Sleep(signInDelay)
	}
	return ok, true
}

// logout is POST /logout: it ends every session the request presents and
// clears the cookie, with or without a live session. Other methods sign
// nobody out.
func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		a.writeError(w, newError(http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			"%s is not allowed here; allowed: POST", r.Method))
		return
	}
	for _, c := range r.CookiesNamed(sessionCookie) {
		a.sessions.remove(c.Value)
	}
	http.SetCookie(w, a.cookie("", -1))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
