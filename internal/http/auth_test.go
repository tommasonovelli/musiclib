package http

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The sign-in is an API contract: no route but /login, /logout and
// /static/ answers without a session; the password is compared one attempt
// at a time with a delay after each refusal; the cookie follows
// PUBLIC_ORIGIN; sign-out ends the session on the server. These tests
// serve the API as cmd/musiclibd mounts it, without the signed-in wrapper
// of the other tests.

// authEnv is newEnv served without the signed-in wrapper: every request
// carries only the cookies it sends.
func authEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.srv.Close()
	e.srv = httptest.NewServer(mount(e.api))
	t.Cleanup(e.srv.Close)
	return e
}

// mount is the daemon's routing of the API and the pages (the health
// endpoints aside).
func mount(a *API) nethttp.Handler {
	mux := nethttp.NewServeMux()
	mux.Handle("/api", a)
	mux.Handle("/api/", a)
	mux.HandleFunc("/", a.Pages)
	return mux
}

// answer is a response read whole, with the time it took.
type answer struct {
	status  int
	header  nethttp.Header
	cookies []*nethttp.Cookie
	body    string
	elapsed time.Duration
}

// sendCtx sends one request to base with the Host of testOrigin and follows
// no redirect. headers are key, value pairs; "Host" sets the Host.
func sendCtx(ctx context.Context, base, method, path, body string, headers ...string) (answer, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := nethttp.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return answer{}, err
	}
	req.Host = testHost
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
			continue
		}
		req.Header.Add(headers[i], headers[i+1])
	}
	client := &nethttp.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*nethttp.Request, []*nethttp.Request) error { return nethttp.ErrUseLastResponse }}
	start := time.Now()
	res, err := client.Do(req)
	if err != nil {
		return answer{}, err
	}
	b, err := io.ReadAll(res.Body)
	if cerr := res.Body.Close(); err == nil {
		err = cerr
	}
	return answer{status: res.StatusCode, header: res.Header, cookies: res.Cookies(), body: string(b),
		elapsed: time.Since(start)}, err
}

func send(t *testing.T, e *env, method, path, body string, headers ...string) answer {
	t.Helper()
	a, err := sendCtx(context.Background(), e.srv.URL, method, path, body, headers...)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return a
}

const formType = "application/x-www-form-urlencoded"

// signIn posts password as the sign-in form does.
func signIn(t *testing.T, e *env, password string, headers ...string) answer {
	t.Helper()
	return send(t, e, "POST", "/login", "password="+url.QueryEscape(password), append([]string{"Content-Type", formType}, headers...)...)
}

// session signs in and returns the "Cookie" header value of the session.
func session(t *testing.T, e *env) string {
	t.Helper()
	a := signIn(t, e, testPassword)
	if a.status != nethttp.StatusSeeOther || len(a.cookies) != 1 {
		t.Fatalf("sign-in: %d %v", a.status, a.header)
	}
	return sessionCookie + "=" + a.cookies[0].Value
}

func (e *env) logText() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.logs.String()
}

// wantLoginRequired checks the API's 401: the usual error body, no-store,
// nothing of the catalog.
func wantLoginRequired(t *testing.T, a answer, what, secret string) {
	t.Helper()
	if a.status != nethttp.StatusUnauthorized || a.header.Get("Cache-Control") != "no-store" ||
		a.header.Get("X-Content-Type-Options") != "nosniff" || a.header.Get("WWW-Authenticate") != "" {
		t.Fatalf("%s: %d %v %s", what, a.status, a.header, a.body)
	}
	if !strings.HasPrefix(a.body, `{"code":"login_required","message":"`) || !strings.HasSuffix(a.body, `","details":{}}`+"\n") {
		t.Fatalf("%s: body %s", what, a.body)
	}
	if strings.Contains(a.body, secret) || len(a.cookies) != 0 {
		t.Fatalf("%s: %s %v", what, a.body, a.cookies)
	}
}

// Without a live session, every API route (known or not, any method) is
// 401 login_required and every page sends the browser to /login; nothing
// of the catalog is in either answer. With the session both work.
func TestAuthRequiredEverywhere(t *testing.T) {
	e := authEnv(t)
	const title = "Secret Title Of The Album"
	id := e.seed("Secret Artist", title)
	album, artist, other := "/api/albums/"+id.String(), "/api/artists/"+e.artistOf(id).String(), uuid.New().String()
	routes := [][2]string{
		{"GET", "/api/artists"}, {"POST", "/api/artists"}, {"GET", artist}, {"PUT", artist},
		{"GET", album}, {"PUT", album}, {"DELETE", album}, {"GET", album + "/status"},
		{"POST", album + "/restore"}, {"POST", album + "/render"},
		{"GET", album + "/cover"}, {"PUT", album + "/cover"}, {"DELETE", album + "/cover"},
		{"POST", album + "/attachments"}, {"DELETE", album + "/attachments/" + other},
		{"GET", album + "/attachments/" + other + "/content"},
		{"DELETE", album + "/tracks/" + other}, {"GET", album + "/tracks/" + other + "/original"},
		{"GET", album + "/tracks/" + other + "/lyrics"}, {"PUT", album + "/tracks/" + other + "/lyrics"},
		{"DELETE", album + "/tracks/" + other + "/lyrics"}, {"POST", album + "/move-tracks"},
		{"GET", "/api/albums"}, {"GET", "/api/albums?q=secret"}, {"GET", "/api/import-source"},
		{"POST", "/api/imports"}, {"GET", "/api/imports/" + other}, {"GET", "/api/jobs"},
		{"POST", "/api/jobs/" + other + "/retry"}, {"POST", "/api/jobs/" + other + "/dismiss"},
		{"POST", "/api/jobs/retry-failed"}, {"POST", "/api/render-all"}, {"POST", "/api/trash/empty"},
		{"GET", "/api"}, {"GET", "/api/"}, {"GET", "/api/nope"}, {"PATCH", "/api/artists"},
	}
	pages := []string{"/", "/?trash=true", "/?fix=true", "/?q=secret", "/import", "/activity", "/albums/" + id.String(), "/nope"}

	// Another API's live session, a well-formed random id, a malformed one.
	otherAPI, err := New(Config{PublicOrigin: testOrigin, Password: testPassword, RenderVersion: testRender,
		Fatal: func(error) {}, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := otherAPI.sessions.create(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range []string{"", sessionCookie + "=" + foreign, sessionCookie + "=" + strings.Repeat("A", sessionIDLen),
		sessionCookie + "=x", sessionCookie + "=", "other=" + e.session} {
		for _, r := range routes {
			headers := []string{"If-Match", ETag(KindAlbum, id, 1), "Content-Type", "application/json"}
			if cookie != "" {
				headers = append(headers, "Cookie", cookie)
			}
			if r[0] != "GET" {
				headers = append(headers, RequestHeader, "1")
			}
			wantLoginRequired(t, send(t, e, r[0], r[1], `{"name":"X"}`, headers...), r[0]+" "+r[1]+" with "+cookie, title)
		}
		head := send(t, e, "HEAD", "/api/artists", "", "Cookie", cookie)
		if head.status != nethttp.StatusUnauthorized || head.body != "" {
			t.Fatalf("HEAD /api/artists with %q: %d %q", cookie, head.status, head.body)
		}
		for _, p := range pages {
			for _, m := range []string{"GET", "HEAD"} {
				a := send(t, e, m, p, "", "Cookie", cookie)
				if a.status != nethttp.StatusSeeOther || a.header.Get("Location") != "/login" ||
					a.header.Get("Cache-Control") != "no-store" || strings.Contains(a.body, "Secret") {
					t.Fatalf("%s %s with %q: %d %v %s", m, p, cookie, a.status, a.header, a.body)
				}
			}
		}
	}
	if n := e.count(`SELECT count(*) FROM artists`); n != 1 {
		t.Fatalf("%d artists: a refused request wrote", n)
	}

	// With the session.
	cookie := session(t, e)
	if a := send(t, e, "GET", "/api/artists", "", "Cookie", cookie); a.status != ok || !strings.Contains(a.body, "Secret Artist") {
		t.Fatalf("GET /api/artists signed in: %d %s", a.status, a.body)
	}
	if a := send(t, e, "GET", "/", "", "Cookie", cookie); a.status != ok || !strings.Contains(a.body, title) {
		t.Fatalf("GET / signed in: %d", a.status)
	}
	// A second cookie of the same name that is not live does not hide a
	// live one.
	if a := send(t, e, "GET", "/api/artists", "", "Cookie", sessionCookie+"=x; "+cookie); a.status != ok {
		t.Fatalf("a dead and a live cookie: %d", a.status)
	}
}

// An upload without a session is refused before its body is read: the
// answer comes although the declared body never does, no upload slot is
// taken and nothing is written.
func TestAuthUploadRefusedBeforeBody(t *testing.T) {
	e := authEnv(t)
	id := e.seed("Miles Davis", "Kind of Blue")
	tag := ETag(KindAlbum, id, e.revision(id))
	for _, head := range [][2]string{
		{"PUT", "/api/albums/" + id.String() + "/cover"},
		{"POST", "/api/albums/" + id.String() + "/attachments?path=a.pdf"},
		{"PUT", "/api/albums/" + id.String() + "/tracks/" + uuid.New().String() + "/lyrics"},
	} {
		res := e.rawRequest(head[0], head[1], map[string]string{
			"If-Match": tag, "Content-Type": UploadMediaType, "Content-Length": "1000000"})
		b, err := io.ReadAll(res.Body)
		if cerr := res.Body.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != nethttp.StatusUnauthorized || !strings.Contains(string(b), `"code":"login_required"`) {
			t.Fatalf("%s %s: %d %s", head[0], head[1], res.StatusCode, b)
		}
	}
	if n := len(e.api.uploads); n != 0 {
		t.Fatalf("%d upload slots taken", n)
	}
	if _, temps := e.blobFiles(); temps != 0 {
		t.Fatalf("%d blob temporaries", temps)
	}
	if e.revision(id) != 1 {
		t.Fatal("a refused upload changed the album")
	}
}

// /login and the static UI files need no session; nothing else under
// /static/ is served.
func TestAuthExemptions(t *testing.T) {
	e := authEnv(t)
	for _, m := range []string{"GET", "HEAD"} {
		a := send(t, e, m, "/login", "")
		if a.status != ok || a.header.Get("Content-Type") != "text/html; charset=utf-8" || a.header.Get("Cache-Control") != "no-store" ||
			!strings.Contains(a.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") || len(a.cookies) != 0 {
			t.Fatalf("%s /login: %d %v", m, a.status, a.header)
		}
		if m == "GET" && (!strings.Contains(a.body, `<form class="login-form" method="post" action="/login">`) ||
			!strings.Contains(a.body, `name="password" type="password" autocomplete="current-password"`) ||
			strings.Contains(a.body, "Wrong password") || strings.Contains(a.body, `id="sidebar"`)) {
			t.Fatalf("GET /login: %s", a.body)
		}
	}
	for name := range staticAssets {
		if a := send(t, e, "GET", "/static/"+name, ""); a.status != ok || a.header.Get("Content-Type") != staticAssets[name] {
			t.Fatalf("/static/%s: %d %v", name, a.status, a.header)
		}
	}
	for _, p := range []string{"/static/layout.html", "/static/login.html", "/static/nope", "/static/"} {
		if a := send(t, e, "GET", p, ""); a.status != notFound404 {
			t.Fatalf("%s: %d", p, a.status)
		}
	}
	// Signed in already, the sign-in page sends the browser on.
	cookie := session(t, e)
	if a := send(t, e, "GET", "/login", "", "Cookie", cookie); a.status != nethttp.StatusSeeOther || a.header.Get("Location") != "/" {
		t.Fatalf("GET /login signed in: %d %v", a.status, a.header)
	}
}

var sessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// The right password: 303 to the Library and one session cookie with the
// fixed flags; a new id at every sign-in, each one live.
func TestSignIn(t *testing.T) {
	e := authEnv(t)
	var ids []string
	for range 2 {
		a := signIn(t, e, testPassword)
		if a.status != nethttp.StatusSeeOther || a.header.Get("Location") != "/" || len(a.cookies) != 1 ||
			len(a.header.Values("Set-Cookie")) != 1 || a.elapsed >= signInDelay {
			t.Fatalf("sign-in: %d %v in %v", a.status, a.header, a.elapsed)
		}
		c := a.cookies[0]
		if c.Name != sessionCookie || !sessionID.MatchString(c.Value) || c.Path != "/" || c.Domain != "" ||
			c.MaxAge != 30*24*3600 || !c.HttpOnly || c.Secure || c.SameSite != nethttp.SameSiteStrictMode {
			t.Fatalf("cookie %q", a.header.Get("Set-Cookie"))
		}
		ids = append(ids, c.Value)
	}
	if ids[0] == ids[1] {
		t.Fatal("two sign-ins gave the same id")
	}
	for _, id := range ids {
		if a := send(t, e, "GET", "/api/artists", "", "Cookie", sessionCookie+"="+id); a.status != ok {
			t.Fatalf("GET /api/artists with a new session: %d", a.status)
		}
		if a := send(t, e, "GET", "/", "", "Cookie", sessionCookie+"="+id); a.status != ok || !strings.Contains(a.body, `action="/logout"`) {
			t.Fatalf("GET / with a new session: %d", a.status)
		}
	}
	logs := e.logText()
	if strings.Count(logs, `"msg":"signed in"`) != 2 || strings.Contains(logs, testPassword) || strings.Contains(logs, ids[0]) {
		t.Fatalf("logs: %s", logs)
	}
}

// Anything but the exact password is refused after the delay, with the
// sign-in page and no cookie; a malformed form is refused the same way. A
// form of another type or too large is refused at once, without taking
// the slot.
func TestSignInRefused(t *testing.T) {
	e := authEnv(t)
	for _, tc := range []struct{ name, path, body string }{
		{"wrong", "/login", "password=wrong-password-123"},
		{"empty", "/login", "password="},
		{"no field", "/login", "other=" + testPassword},
		{"a prefix", "/login", "password=" + url.QueryEscape(testPassword[:len(testPassword)-1])},
		{"one more character", "/login", "password=" + url.QueryEscape(testPassword+"x")},
		{"a trailing line break", "/login", "password=" + url.QueryEscape(testPassword+"\n")},
		{"in the URL only", "/login?password=" + url.QueryEscape(testPassword), ""},
		{"malformed", "/login", "password=%zz" + testPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := send(t, e, "POST", tc.path, tc.body, "Content-Type", formType)
			if a.status != nethttp.StatusUnauthorized || a.header.Get("Content-Type") != "text/html; charset=utf-8" ||
				!strings.Contains(a.body, `role="alert">Wrong password. Try again.`) || len(a.cookies) != 0 ||
				a.header.Get("Location") != "" || a.elapsed < signInDelay {
				t.Fatalf("%d %v in %v: %s", a.status, a.header, a.elapsed, a.body)
			}
		})
	}
	// Only the env's own session (newEnvOn).
	if e.api.sessions.len() != 1 {
		t.Fatal("a refused attempt created a session")
	}
	for _, tc := range []struct {
		name, ctype, body string
		status            int
		code              string
	}{
		{"JSON", "application/json", `{"password":"` + testPassword + `"}`, nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType},
		{"no type", "", "password=" + testPassword, nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType},
		{"too large", formType, "password=" + testPassword + "&x=" + strings.Repeat("a", signInBodyBytes), nethttp.StatusRequestEntityTooLarge, CodeBodyTooLarge},
	} {
		headers := []string{}
		if tc.ctype != "" {
			headers = append(headers, "Content-Type", tc.ctype)
		}
		a := send(t, e, "POST", "/login", tc.body, headers...)
		if a.status != tc.status || !strings.Contains(a.body, `"code":"`+tc.code+`"`) || len(a.cookies) != 0 || a.elapsed >= signInDelay/2 {
			t.Fatalf("%s: %d in %v: %s", tc.name, a.status, a.elapsed, a.body)
		}
	}
	logs := e.logText()
	if strings.Count(logs, `"msg":"sign-in refused"`) != 8 || strings.Contains(logs, testPassword[:8]) || strings.Contains(logs, "wrong-password") {
		t.Fatalf("logs: %s", logs)
	}
}

// The whole digest is compared: a stored digest that differs from the
// password's in its first or its last byte alone refuses the password.
func TestSignInComparesTheWholeDigest(t *testing.T) {
	for _, i := range []int{-1, 0, sha256.Size - 1} {
		a, err := New(Config{PublicOrigin: testOrigin, Password: testPassword, RenderVersion: testRender,
			Fatal: func(error) {}, Log: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		want := nethttp.StatusSeeOther
		if i >= 0 {
			a.password[i] ^= 1
			want = nethttp.StatusUnauthorized
		}
		// The server starts after the change: its handlers see it.
		srv := httptest.NewServer(mount(a))
		got := signIn(t, &env{srv: srv}, testPassword)
		srv.Close()
		if got.status != want {
			t.Fatalf("digest byte %d changed: %d, want %d", i, got.status, want)
		}
	}
}

// Attempts go one at a time, whatever the concurrency: three wrong ones
// take three delays, and a right one waiting among them still succeeds.
func TestSignInSerialized(t *testing.T) {
	e := authEnv(t)
	start := time.Now()
	var wg sync.WaitGroup
	results := make([]answer, 4)
	errs := make([]error, 4)
	for i := range results {
		password := "wrong-password-123"
		if i == 3 {
			password = testPassword
			time.Sleep(50 * time.Millisecond)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = sendCtx(context.Background(), e.srv.URL, "POST", "/login", "password="+url.QueryEscape(password),
				"Content-Type", formType)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	for _, a := range results[:3] {
		if a.status != nethttp.StatusUnauthorized {
			t.Fatalf("a wrong attempt: %d", a.status)
		}
	}
	if results[3].status != nethttp.StatusSeeOther || len(results[3].cookies) != 1 {
		t.Fatalf("the right attempt: %d", results[3].status)
	}
	if elapsed := time.Since(start); elapsed < 3*signInDelay {
		t.Fatalf("three refused attempts took %v: they were not serialized", elapsed)
	}
}

// A refused attempt holds the slot for the whole delay even when its client
// leaves: closing the connection early reveals nothing and frees nothing.
// A client that leaves while waiting for the slot leaves the queue.
func TestSignInSlotHeldAfterDisconnect(t *testing.T) {
	e := authEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := sendCtx(ctx, e.srv.URL, "POST", "/login", "password=wrong-password-123", "Content-Type", formType)
		first <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("the first attempt: %v", err)
	}
	a := send(t, e, "POST", "/login", "password=wrong-password-456", "Content-Type", formType)
	if a.status != nethttp.StatusUnauthorized || a.elapsed < signInDelay*3/2 {
		t.Fatalf("the second attempt: %d in %v; the first one's delay was cut short", a.status, a.elapsed)
	}
	// A waiting client that leaves does not keep its turn.
	hold := make(chan struct{})
	e.api.signIns <- struct{}{}
	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() {
		defer close(hold)
		_, _ = sendCtx(ctx, e.srv.URL, "POST", "/login", "password=wrong-password-789", "Content-Type", formType)
	}()
	<-hold
	time.Sleep(100 * time.Millisecond)
	<-e.api.signIns
	if b := signIn(t, e, testPassword); b.status != nethttp.StatusSeeOther || b.elapsed >= signInDelay {
		t.Fatalf("after a waiting client left: %d in %v", b.status, b.elapsed)
	}
}

// The Host and Origin checks come first on the forms too; only POST
// /login and POST /logout go without X-Musiclib-Request.
func TestSignInBoundary(t *testing.T) {
	e := authEnv(t)
	for _, tc := range []struct {
		name    string
		headers []string
		status  int
		code    string
	}{
		{"another Host", []string{"Host", "evil.test:8080"}, nethttp.StatusMisdirectedRequest, CodeHostNotAllowed},
		{"another Origin", []string{"Origin", "http://evil.test"}, nethttp.StatusForbidden, CodeOriginNotAllowed},
		{"Origin null", []string{"Origin", "null"}, nethttp.StatusForbidden, CodeOriginNotAllowed},
	} {
		a := signIn(t, e, testPassword, tc.headers...)
		if a.status != tc.status || !strings.Contains(a.body, `"code":"`+tc.code+`"`) || len(a.cookies) != 0 || a.elapsed >= signInDelay {
			t.Fatalf("%s: %d %s", tc.name, a.status, a.body)
		}
		cookie := session(t, e)
		lo := send(t, e, "POST", "/logout", "", append([]string{"Cookie", cookie}, tc.headers...)...)
		if lo.status != tc.status || len(lo.cookies) != 0 {
			t.Fatalf("logout, %s: %d", tc.name, lo.status)
		}
		if a := send(t, e, "GET", "/api/artists", "", "Cookie", cookie); a.status != ok {
			t.Fatalf("a refused sign-out ended the session: %d", a.status)
		}
	}
	for _, origin := range []string{"", testOrigin} {
		headers := []string{}
		if origin != "" {
			headers = append(headers, "Origin", origin)
		}
		if a := signIn(t, e, testPassword, headers...); a.status != nethttp.StatusSeeOther {
			t.Fatalf("a same-origin sign-in without the header (Origin %q): %d %s", origin, a.status, a.body)
		}
	}
	for _, tc := range []struct {
		method, path string
		header       bool
		status       int
		code         string
	}{
		{"POST", "/login/", false, nethttp.StatusForbidden, CodeRequestHeaderRequired},
		{"POST", "/Login", false, nethttp.StatusForbidden, CodeRequestHeaderRequired},
		{"POST", "/", false, nethttp.StatusForbidden, CodeRequestHeaderRequired},
		{"POST", "/api/artists", false, nethttp.StatusForbidden, CodeRequestHeaderRequired},
		{"PUT", "/login", false, nethttp.StatusForbidden, CodeRequestHeaderRequired},
		{"OPTIONS", "/login", false, nethttp.StatusForbidden, CodeRequestHeaderRequired},
		{"PUT", "/login", true, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed},
		{"DELETE", "/logout", true, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed},
	} {
		headers := []string{"Content-Type", formType}
		if tc.header {
			headers = append(headers, RequestHeader, "1")
		}
		a := send(t, e, tc.method, tc.path, "password="+testPassword, headers...)
		if a.status != tc.status || !strings.Contains(a.body, `"code":"`+tc.code+`"`) || len(a.cookies) != 0 {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, a.status, a.body)
		}
		if tc.status == nethttp.StatusMethodNotAllowed && a.header.Get("Allow") == "" {
			t.Fatalf("%s %s: no Allow", tc.method, tc.path)
		}
	}
}

// Sign-out ends the session on the server and clears the cookie; only a
// POST signs out, and it answers the same without a session.
func TestSignOut(t *testing.T) {
	e := authEnv(t)
	cookie := session(t, e)
	if a := send(t, e, "GET", "/logout", "", "Cookie", cookie); a.status != nethttp.StatusMethodNotAllowed ||
		a.header.Get("Allow") != "POST" || len(a.cookies) != 0 {
		t.Fatalf("GET /logout: %d %v", a.status, a.header)
	}
	if a := send(t, e, "GET", "/api/artists", "", "Cookie", cookie); a.status != ok {
		t.Fatalf("GET /logout signed out: %d", a.status)
	}
	a := send(t, e, "POST", "/logout", "", "Cookie", cookie)
	if a.status != nethttp.StatusSeeOther || a.header.Get("Location") != "/login" || len(a.cookies) != 1 {
		t.Fatalf("POST /logout: %d %v", a.status, a.header)
	}
	c := a.cookies[0]
	if set := a.header.Get("Set-Cookie"); c.Name != sessionCookie || c.Value != "" || c.MaxAge >= 0 || c.Path != "/" ||
		!c.HttpOnly || c.SameSite != nethttp.SameSiteStrictMode || !strings.Contains(set, "Max-Age=0") {
		t.Fatalf("the cleared cookie: %q", set)
	}
	// The old id is dead on the server, whatever the client keeps.
	wantLoginRequired(t, send(t, e, "GET", "/api/artists", "", "Cookie", cookie), "the old session", "\x00")
	if a := send(t, e, "GET", "/", "", "Cookie", cookie); a.status != nethttp.StatusSeeOther {
		t.Fatalf("GET / with the old session: %d", a.status)
	}
	if e.api.sessions.has(cookie) {
		t.Fatal("the session is still stored")
	}
	if a := send(t, e, "POST", "/logout", ""); a.status != nethttp.StatusSeeOther || a.header.Get("Location") != "/login" {
		t.Fatalf("POST /logout without a session: %d", a.status)
	}
}

// The cookie is Secure exactly when PUBLIC_ORIGIN is https, on sign-in and
// on sign-out.
func TestSessionCookieFlags(t *testing.T) {
	a, err := New(Config{PublicOrigin: "https://music.test", Password: testPassword, RenderVersion: testRender,
		Fatal: func(error) {}, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mount(a))
	t.Cleanup(srv.Close)
	e := &env{srv: srv}
	in := signIn(t, e, testPassword, "Host", "music.test")
	out := send(t, e, "POST", "/logout", "", "Host", "music.test")
	for _, r := range []answer{in, out} {
		if r.status != nethttp.StatusSeeOther || len(r.cookies) != 1 || !r.cookies[0].Secure || !r.cookies[0].HttpOnly {
			t.Fatalf("https: %d %v", r.status, r.header)
		}
	}
	// The http origin of the other tests: not Secure.
	h := authEnv(t)
	in, out = signIn(t, h, testPassword), send(t, h, "POST", "/logout", "")
	for _, r := range []answer{in, out} {
		if r.status != nethttp.StatusSeeOther || len(r.cookies) != 1 || r.cookies[0].Secure {
			t.Fatalf("http: %d %v", r.status, r.header)
		}
	}
}

// A session lives 30 days from the sign-in, not from its last use; an
// expired one is refused and forgotten, and every sign-in forgets the
// other expired ones.
func TestSessionExpiry(t *testing.T) {
	e := authEnv(t)
	cookie := session(t, e)
	key := sha256.Sum256([]byte(strings.TrimPrefix(cookie, sessionCookie+"=")))
	e.api.sessions.mu.Lock()
	e.api.sessions.m[key] = time.Now().Add(-time.Second)
	e.api.sessions.mu.Unlock()
	wantLoginRequired(t, send(t, e, "GET", "/api/artists", "", "Cookie", cookie), "an expired session", "\x00")
	if e.api.sessions.has(cookie) {
		t.Fatal("the expired session is still stored")
	}

	s := sessions{m: map[[32]byte]time.Time{}}
	now := time.Now()
	live, err := s.create(now)
	if err != nil {
		t.Fatal(err)
	}
	if !s.valid(live, now.Add(sessionMaxAge-time.Second)) || s.valid(live, now.Add(sessionMaxAge)) {
		t.Fatal("the maximum age is not 30 days from the sign-in")
	}
	a, err := s.create(now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.create(now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.create(now.Add(sessionMaxAge + time.Minute)); err != nil {
		t.Fatal(err)
	}
	if s.len() != 2 || s.valid(a, now) || !s.valid(b, now.Add(sessionMaxAge)) {
		t.Fatalf("a sign-in did not forget the expired sessions: %d left", s.len())
	}
}

// New refuses a password that could never be typed into the form; the
// message never contains it.
func TestNewRefusesPassword(t *testing.T) {
	cfg := func(p string) Config {
		return Config{PublicOrigin: testOrigin, Password: p, RenderVersion: testRender, Fatal: func(error) {},
			Log: slog.New(slog.DiscardHandler)}
	}
	for _, p := range []string{"", "abcdefghijk", strings.Repeat("é", 11), "abcdefghijkl\n", "abc\tdefghijkl",
		"abcdefghijkl\xff", strings.Repeat("a", MaxPasswordBytes+1)} {
		_, err := New(cfg(p))
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeInvalidConfig || (p != "" && strings.Contains(e.Message, p)) {
			t.Fatalf("New with %q: %v", p, err)
		}
	}
	for _, p := range []string{"abcdefghijkl", strings.Repeat("é", 12), strings.Repeat("a", MaxPasswordBytes)} {
		if _, err := New(cfg(p)); err != nil {
			t.Fatalf("New with %q: %v", p, err)
		}
	}
}

// len is the number of stored sessions, expired ones included.
func (s *sessions) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// has reports whether the session of a "Cookie" header value is stored,
// live or expired.
func (s *sessions) has(cookie string) bool {
	k := sha256.Sum256([]byte(strings.TrimPrefix(cookie, sessionCookie+"=")))
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[k]
	return ok
}
