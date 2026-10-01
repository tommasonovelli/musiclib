package http

import (
	"bytes"
	nethttp "net/http"
	"strings"
	"testing"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// noCORS fails if any response header is a CORS one (§10.4: CORS is never
// enabled).
func noCORS(t *testing.T, r resp) {
	t.Helper()
	for k := range r.header {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Fatalf("CORS header %s: %v", k, r.header)
		}
	}
	if r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("no nosniff: %v", r.header)
	}
}

// §10.4: Host, Origin and X-Musiclib-Request, before anything else; no
// CORS header on any answer; nosniff everywhere.
func TestSecurityBoundary(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Miles Davis", "Kind of Blue")
	_, tag := e.album(id)
	path := "/api/albums/" + id.String()
	h := func(kv ...string) map[string][]string {
		m := map[string][]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = append(m[kv[i]], kv[i+1])
		}
		return m
	}
	for _, tc := range []struct {
		name    string
		r       req
		status  int
		code    string
		allowed bool
	}{
		{"GET without Origin", req{method: "GET", path: path}, ok, "", true},
		{"GET with our Origin", req{method: "GET", path: path, headers: h("Origin", testOrigin)}, ok, "", true},
		{"Host in another case", req{method: "GET", path: path, headers: h("Host", "MUSIC.test:8080")}, ok, "", true},
		{"another Host", req{method: "GET", path: path, headers: h("Host", "evil.test:8080")},
			nethttp.StatusMisdirectedRequest, CodeHostNotAllowed, false},
		{"another port", req{method: "GET", path: path, headers: h("Host", "music.test:8081")},
			nethttp.StatusMisdirectedRequest, CodeHostNotAllowed, false},
		{"no port", req{method: "GET", path: path, headers: h("Host", "music.test")},
			nethttp.StatusMisdirectedRequest, CodeHostNotAllowed, false},
		{"a rebinding host", req{method: "GET", path: path, headers: h("Host", "127.0.0.1:8080")},
			nethttp.StatusMisdirectedRequest, CodeHostNotAllowed, false},
		{"another Origin", req{method: "GET", path: path, headers: h("Origin", "http://evil.test")},
			nethttp.StatusForbidden, CodeOriginNotAllowed, false},
		{"Origin null", req{method: "GET", path: path, headers: h("Origin", "null")},
			nethttp.StatusForbidden, CodeOriginNotAllowed, false},
		{"Origin with a trailing slash", req{method: "GET", path: path, headers: h("Origin", testOrigin+"/")},
			nethttp.StatusForbidden, CodeOriginNotAllowed, false},
		{"Origin https", req{method: "GET", path: path, headers: h("Origin", "https://music.test:8080")},
			nethttp.StatusForbidden, CodeOriginNotAllowed, false},
		{"two Origins", req{method: "GET", path: path, headers: h("Origin", testOrigin, "Origin", testOrigin)},
			nethttp.StatusForbidden, CodeOriginNotAllowed, false},
		{"POST from another Origin", req{method: "POST", path: path + "/render", ifMatch: tag, headers: h("Origin", "http://evil.test")},
			nethttp.StatusForbidden, CodeOriginNotAllowed, false},
		{"POST with Origin null", req{method: "POST", path: path + "/render", ifMatch: tag, headers: h("Origin", "null")},
			nethttp.StatusForbidden, CodeOriginNotAllowed, false},
		{"POST without the header", req{method: "POST", path: path + "/render", ifMatch: tag, headers: h(RequestHeader, "")},
			nethttp.StatusForbidden, CodeRequestHeaderRequired, false},
		{"POST with header 0", req{method: "POST", path: path + "/render", ifMatch: tag, headers: h(RequestHeader, "0")},
			nethttp.StatusForbidden, CodeRequestHeaderRequired, false},
		{"POST with header true", req{method: "POST", path: path + "/render", ifMatch: tag, headers: h(RequestHeader, "true")},
			nethttp.StatusForbidden, CodeRequestHeaderRequired, false},
		{"POST with the header twice", req{method: "POST", path: path + "/render", ifMatch: tag, headers: h(RequestHeader, "1", RequestHeader, "1")},
			nethttp.StatusForbidden, CodeRequestHeaderRequired, false},
		{"DELETE without the header", req{method: "DELETE", path: path, ifMatch: tag, headers: h(RequestHeader, "")},
			nethttp.StatusForbidden, CodeRequestHeaderRequired, false},
		{"PUT without the header", req{method: "PUT", path: "/api/artists/" + e.artistOf(id).String(), body: `{"name":"X"}`,
			ifMatch: ETag(KindArtist, e.artistOf(id), 1), headers: h(RequestHeader, "")},
			nethttp.StatusForbidden, CodeRequestHeaderRequired, false},
		{"a CORS preflight", req{method: "OPTIONS", path: path, headers: h("Origin", "http://evil.test",
			"Access-Control-Request-Method", "PUT", "Access-Control-Request-Headers", "x-musiclib-request")},
			nethttp.StatusForbidden, CodeOriginNotAllowed, false},
		{"a same-origin preflight", req{method: "OPTIONS", path: path, headers: h(RequestHeader, "", "Origin", testOrigin,
			"Access-Control-Request-Method", "PUT")},
			nethttp.StatusForbidden, CodeRequestHeaderRequired, false},
		{"OPTIONS with the header", req{method: "OPTIONS", path: path},
			nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed, false},
		{"a non-browser POST without Origin", req{method: "POST", path: "/api/artists", body: `{"name":"Curl"}`},
			created, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := e.revision(id)
			jobs := e.count(`SELECT count(*) FROM jobs`)
			var r resp
			if tc.code != "" {
				r = e.wantError(tc.r, tc.status, tc.code)
			} else {
				r = e.must(tc.r, tc.status)
			}
			noCORS(t, r)
			if !tc.allowed && (e.revision(id) != before || e.count(`SELECT count(*) FROM jobs`) != jobs) {
				t.Fatal("a refused request changed the catalog")
			}
		})
	}
}

// Routing: JSON 404 and 405 with Allow; no redirect for an unclean path.
func TestRouting(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Miles Davis", "Kind of Blue")
	for _, tc := range []struct {
		method, path string
		status       int
		code, allow  string
	}{
		{"GET", "/api/nothing", notFound404, CodeNotFound, ""},
		{"GET", "/api/", notFound404, CodeNotFound, ""},
		{"GET", "/api", notFound404, CodeNotFound, ""},
		{"GET", "/api/imports/", notFound404, CodeNotFound, ""},
		{"PUT", "/api/albums", nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed, "GET, HEAD"},
		{"GET", "/api/jobs/retry-failed", nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed, "POST"},
		{"GET", "/api/artists/", notFound404, CodeNotFound, ""},
		{"GET", "/api//artists", notFound404, CodeNotFound, ""},
		{"GET", "/api/albums/" + id.String() + "/../" + id.String(), notFound404, CodeNotFound, ""},
		{"GET", "/api/albums/" + id.String() + "/tracks", nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed, "POST"},
		{"DELETE", "/api/artists", nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed, "GET, HEAD, POST"},
		{"POST", "/api/artists/" + id.String(), nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed, "GET, HEAD, PUT"},
		{"POST", "/api/albums/" + id.String(), nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed, "GET, HEAD, PUT, DELETE"},
		{"PUT", "/api/albums/" + id.String() + "/status", nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed, "GET, HEAD"},
		{"GET", "/api/albums/" + id.String() + "/restore", nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed, "POST"},
		{"PATCH", "/api/albums/" + id.String() + "/render", nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed, "POST"},
	} {
		r := e.wantError(req{method: tc.method, path: tc.path}, tc.status, tc.code)
		if r.header.Get("Allow") != tc.allow || r.header.Get("Location") != "" {
			t.Errorf("%s %s: Allow %q, Location %q", tc.method, tc.path, r.header.Get("Allow"), r.header.Get("Location"))
		}
		noCORS(t, r)
	}
}

// §10.1 through the server: the size limit, JSON rules, the media type.
func TestBodyRulesOverHTTP(t *testing.T) {
	e := newEnv(t)
	post := func(body []byte, headers map[string][]string) req {
		return req{method: "POST", path: "/api/artists", body: body, headers: headers}
	}
	exact := bytes.Repeat([]byte(" "), MaxBodyBytes)
	copy(exact, `{"name":"Sixteen"}`)
	e.must(post(exact, nil), created)
	over := append(bytes.Repeat([]byte(" "), MaxBodyBytes), '1')
	copy(over, `{"name":"Seventeen"}`)
	r := e.wantError(post(over, nil), nethttp.StatusRequestEntityTooLarge, CodeBodyTooLarge)
	if r.details()["limit"] != float64(MaxBodyBytes) {
		t.Fatalf("413 %s", r.raw)
	}
	e.wantError(post([]byte(`{"name":"A","name":"B"}`), nil), badRequest, CodeDuplicateKey)
	e.wantError(post([]byte(`{"name":"A"}{"name":"B"}`), nil), badRequest, CodeInvalidJSON)
	e.wantError(post([]byte(`{"name":"A"} []`), nil), badRequest, CodeInvalidJSON)
	e.wantError(post([]byte(`{"name":"A","extra":1}`), nil), unprocess, CodeUnknownField)
	e.wantError(post([]byte(`{"Name":"A"}`), nil), unprocess, CodeUnknownField)
	e.wantError(post([]byte("{\"name\":\"\xff\"}"), nil), badRequest, CodeInvalidUTF8)
	e.wantError(post([]byte(`{"name":"\udc00"}`), nil), badRequest, CodeInvalidUTF8)
	e.wantError(post([]byte(`{"name":"A"}`), map[string][]string{"Content-Type": {"text/plain"}}), nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	e.wantError(post([]byte(`name=A`), map[string][]string{"Content-Type": {"application/x-www-form-urlencoded"}}), nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	e.wantError(req{method: "POST", path: "/api/artists"}, nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	e.wantError(post([]byte(``), nil), badRequest, CodeInvalidJSON)
	if n := e.count(`SELECT count(*) FROM artists`); n != 1 {
		t.Fatalf("%d artists: a refused body wrote something", n)
	}
}

// §10.1, §11.1: 503 before Enable and after Disable, still behind the
// boundary; a request in either state touches nothing.
func TestAvailability(t *testing.T) {
	e := newEnvOn(t, pgtest.New(t), false)
	r := e.wantError(req{method: "GET", path: "/api/artists"}, unavailable, CodeNotReady)
	noCORS(t, r)
	if r.header.Get("Cache-Control") != "no-store" {
		t.Fatal("no Cache-Control on a 503")
	}
	e.wantError(req{method: "POST", path: "/api/artists", body: `{"name":"X"}`}, unavailable, CodeNotReady)
	// The boundary comes first.
	e.wantError(req{method: "GET", path: "/api/artists", headers: map[string][]string{"Host": {"evil.test"}}},
		nethttp.StatusMisdirectedRequest, CodeHostNotAllowed)
	e.wantError(req{method: "POST", path: "/api/artists", body: `{"name":"X"}`, headers: map[string][]string{RequestHeader: {""}}},
		nethttp.StatusForbidden, CodeRequestHeaderRequired)

	e.api.Enable(e.backend())
	e.must(req{method: "POST", path: "/api/artists", body: `{"name":"X"}`}, created)
	e.api.Disable(CodeShuttingDown, "the server is shutting down")
	e.wantError(req{method: "POST", path: "/api/artists", body: `{"name":"Y"}`}, unavailable, CodeShuttingDown)
	e.wantError(req{method: "GET", path: "/api/artists"}, unavailable, CodeShuttingDown)
	if n := e.count(`SELECT count(*) FROM artists`); n != 1 {
		t.Fatalf("%d artists", n)
	}
}

// §6.4 at the API: a lost database is 503 with a message of our own (no
// pgx text), the API disables itself and reports the error once, so that
// the process restarts. The same for a commit whose answer was lost: the
// change is durable, and the client is told to reload.
func TestDatabaseLossIsFatal(t *testing.T) {
	t.Run("connection lost", func(t *testing.T) {
		dbURL := pgtest.EmptyDB(t)
		db := pgtest.Pool(t, dbURL)
		if err := store.Migrate(t.Context(), db); err != nil {
			t.Fatal(err)
		}
		e := newEnvOn(t, db, true)
		e.must(req{method: "GET", path: "/api/artists"}, ok)
		name := pgtest.DBName(t, dbURL)
		pgtest.AdminExec(t, "ALTER DATABASE "+name+" ALLOW_CONNECTIONS false")
		pgtest.AdminExec(t, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '"+name+"'")
		r := e.wantError(req{method: "GET", path: "/api/artists"}, unavailable, store.CodeConnectionLost)
		if strings.Contains(string(r.raw), "SQLSTATE") || strings.Contains(string(r.raw), name) {
			t.Fatalf("database text in the answer: %s", r.raw)
		}
		pgtest.AdminExec(t, "ALTER DATABASE "+name+" ALLOW_CONNECTIONS true")
		// Disabled until the restart, even with the database back.
		e.wantError(req{method: "POST", path: "/api/artists", body: `{"name":"X"}`}, unavailable, store.CodeConnectionLost)
		if e.fatalCount() != 1 {
			t.Fatalf("Fatal called %d times, want once", e.fatalCount())
		}
	})
	t.Run("commit answer lost", func(t *testing.T) {
		dbURL := pgtest.EmptyDB(t)
		direct := pgtest.Pool(t, dbURL)
		if err := store.Migrate(t.Context(), direct); err != nil {
			t.Fatal(err)
		}
		proxy := pgtest.NewProxy(t, dbURL)
		e := newEnvOn(t, pgtest.Pool(t, proxy.URL), true)
		e.db = direct
		proxy.LoseNextCommitAck()
		e.wantError(req{method: "POST", path: "/api/artists", body: `{"name":"Lost Answer"}`}, unavailable, store.CodeCommitUncertain)
		if e.count(`SELECT count(*) FROM artists WHERE name = 'Lost Answer'`) != 1 || e.fatalCount() != 1 {
			t.Fatalf("after a lost answer: fatal %d", e.fatalCount())
		}
		e.wantError(req{method: "GET", path: "/api/artists"}, unavailable, store.CodeCommitUncertain)
	})
}

// Every error of the table has the status of §10.1 (NOTES.md N-149).
func TestStatusTable(t *testing.T) {
	for code, want := range map[string]int{
		catalog.CodePreconditionRequired: 428, catalog.CodePreconditionFailed: 412,
		catalog.CodeAlbumNotFound: 404, catalog.CodeArtistNotFound: 404,
		catalog.CodePathReserved: 409, catalog.CodeAlbumFolderConflict: 409, catalog.CodeArtistFolderConflict: 409,
		catalog.CodeArtistExists: 409, catalog.CodeTrackListMismatch: 422, catalog.CodeInvalidYear: 422,
		store.CodeConnectionLost: 503, store.CodeCommitUncertain: 503,
		// Round 14 (N-172, N-179).
		catalog.CodeAttachmentNotFound: 404, catalog.CodeTrackNotFound: 404, catalog.CodeAttachmentCollision: 409,
		catalog.CodeCoverNotEmbeddable: 422, catalog.CodeInvalidCover: 422, catalog.CodeInvalidLyrics: 422,
		catalog.CodeNoTracks: 422, names.CodePathAbsolute: 422, names.CodePathDotSegment: 422,
		names.CodePathEmptySegment: 422, names.CodePathTooDeep: 422, names.CodePathTooLong: 422,
		names.CodePathNulByte: 422, names.CodePathEmpty: 422,
		// Round 16 (N-193, N-195).
		catalog.CodeImportBatchNotFound: 404, catalog.CodeJobNotFound: 404, catalog.CodeImportBatchConflict: 409,
		jobs.CodeNotRetryable: 409, jobs.CodeInProgress: 409, jobs.CodeOverridesNotAllowed: 422,
		jobs.CodeInvalidOverrides: 422,
		catalog.CodeBlobMismatch:  0, catalog.CodeInvalidBlob: 0,
	} {
		if statusOf[code] != want {
			t.Errorf("%s: %d, want %d", code, statusOf[code], want)
		}
	}
	// A database error that is not fatal, and anything unknown: 500
	// internal, with no text of the cause.
	for _, err := range []error{
		&catalog.Error{Code: catalog.CodeDB, Message: "reading album", Err: errSecret},
		errSecret,
	} {
		e := translate(err)
		if e.Status != 500 || e.Code != CodeInternal || strings.Contains(e.Message, "secret") {
			t.Errorf("translate(%v) = %+v", err, e)
		}
	}
}

var errSecret = &catalog.Error{Code: "no_such_code", Message: "secret SELECT * FROM albums"}
