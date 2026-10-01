package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/faulttest"
	"musiclib/internal/fsops"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// The API tests run the real handler behind a real HTTP server
// (httptest), over the real catalog on a real PostgreSQL 17 (§12.1): no
// mock of SQL, of the transactions or of the revision check.

const (
	testOrigin = "http://music.test:8080"
	testHost   = "music.test:8080"
	testRender = "musiclib-render/test"
	// testPassword is the sign-in password of the test API.
	testPassword = "test-password-1234"
)

type env struct {
	t    *testing.T
	db   *pgxpool.Pool
	api  *API
	srv  *httptest.Server
	host string // browser tests use the listener's ephemeral port
	svc  *catalog.Service
	// session is a live session of api, which signedIn adds to requests.
	session string

	// The blob store on the ext4 TMPDIR (§12.1), the process budget, and
	// the failpoint of the uploads.
	data      string
	originals *fsops.Root
	work      *fsops.Root
	blobs     *blobstore.Store
	budget    *jobs.Budget
	hooks     faulttest.Switch
	// imports is the test's /import, source its root (round 16).
	imports string
	source  *fsops.Root
	// tracks reads uploaded tracks: fakeTracks, or the real importer.
	tracks TrackReader

	mu     sync.Mutex
	fatals []error
	logs   bytes.Buffer
}

// newEnv is an enabled API on a new migrated database.
func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvOn(t, pgtest.New(t), true)
}

func newEnvOn(t *testing.T, db *pgxpool.Pool, enable bool) *env {
	t.Helper()
	e := &env{t: t, db: db}
	api, err := New(Config{
		PublicOrigin:  testOrigin,
		Password:      testPassword,
		RenderVersion: testRender,
		Fatal: func(err error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.fatals = append(e.fatals, err)
		},
		Log:        slog.New(slog.NewJSONHandler(&lockedWriter{mu: &e.mu, w: &e.logs}, nil)),
		Failpoints: e.hooks.Hook(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.api = api
	e.svc, err = catalog.New(db, nil, importer.CoverFits, importer.GenreFits)
	if err != nil {
		t.Fatal(err)
	}
	e.openStore()
	e.tracks = fakeTracks{e}
	if enable {
		api.Enable(e.backend())
	}
	if e.session, err = api.sessions.create(time.Now()); err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(e.signedIn(api))
	t.Cleanup(e.srv.Close)
	return e
}

// signedIn serves h as the browser of a signed-in user would reach it: a
// request without a session cookie gets the env's live session, except
// /login and /logout. The API still checks the session; the tests of the
// sign-in itself serve the API without this wrapper (auth_test.go).
func (e *env) signedIn(h nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if r.URL.Path != "/login" && r.URL.Path != "/logout" && len(r.CookiesNamed(sessionCookie)) == 0 {
			r.AddCookie(&nethttp.Cookie{Name: sessionCookie, Value: e.session})
		}
		h.ServeHTTP(w, r)
	})
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (e *env) fatalCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.fatals)
}

// resp is an answer, with its body decoded when it is JSON.
type resp struct {
	status int
	header nethttp.Header
	raw    []byte
	body   map[string]any
}

func (r resp) code() string {
	s, _ := r.body["code"].(string)
	return s
}

func (r resp) details() map[string]any {
	d, _ := r.body["details"].(map[string]any)
	return d
}

// req is one request. By default it is a well-behaved client of §10.4:
// the right Host, no Origin, X-Musiclib-Request on a mutation, a JSON
// Content-Type with a body. headers overrides; a value "" removes.
type req struct {
	method  string
	path    string
	body    any // string or []byte sent as is, an io.Reader streamed (chunked unless length is set); anything else marshalled
	length  int64
	ifMatch string
	headers map[string][]string
}

func (e *env) do(r req) resp {
	e.t.Helper()
	var body io.Reader
	switch b := r.body.(type) {
	case nil:
	case string:
		body = bytes.NewReader([]byte(b))
	case []byte:
		body = bytes.NewReader(b)
	case io.Reader:
		body = b
	default:
		j, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		body = bytes.NewReader(j)
	}
	hr, err := nethttp.NewRequest(r.method, e.srv.URL+r.path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if r.length > 0 {
		hr.ContentLength = r.length
	}
	hr.Host = testHost
	if e.host != "" {
		hr.Host = e.host
	}
	if r.method != nethttp.MethodGet && r.method != nethttp.MethodHead {
		hr.Header.Set(RequestHeader, "1")
	}
	if r.body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	if r.ifMatch != "" {
		hr.Header.Set("If-Match", r.ifMatch)
	}
	for k, vs := range r.headers {
		if k == "Host" {
			hr.Host = vs[0]
			continue
		}
		hr.Header.Del(k)
		for _, v := range vs {
			if v != "" {
				hr.Header.Add(k, v)
			}
		}
	}
	res, err := e.srv.Client().Do(hr)
	if err != nil {
		e.t.Fatalf("%s %s: %v", r.method, r.path, err)
	}
	raw, err := io.ReadAll(res.Body)
	if cerr := res.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		e.t.Fatal(err)
	}
	out := resp{status: res.StatusCode, header: res.Header, raw: raw}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			e.t.Fatalf("%s %s: the body is not a JSON object: %q", r.method, r.path, raw)
		}
	}
	return out
}

// must checks the status of an answer and returns it.
func (e *env) must(r req, status int) resp {
	e.t.Helper()
	out := e.do(r)
	if out.status != status {
		e.t.Fatalf("%s %s = %d %s, want %d", r.method, r.path, out.status, out.raw, status)
	}
	return out
}

// wantError checks an error answer: status, code, and the §10.1 shape.
func (e *env) wantError(r req, status int, code string) resp {
	e.t.Helper()
	out := e.must(r, status)
	if out.code() != code {
		e.t.Fatalf("%s %s: code %q, want %q; body %s", r.method, r.path, out.code(), code, out.raw)
	}
	if _, ok := out.body["message"].(string); !ok || out.details() == nil || len(out.body) != 3 {
		e.t.Fatalf("%s %s: not a {code, message, details} body: %s", r.method, r.path, out.raw)
	}
	return out
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

var hashSeq atomic.Int64

func newHash() string {
	s := sha256.Sum256(fmt.Appendf(nil, "%d-%d", time.Now().UnixNano(), hashSeq.Add(1)))
	return hex.EncodeToString(s[:])
}

func ptr[T any](v T) *T { return &v }

// seed imports an album through the catalog's import commit, as the
// importer would after its copies: a batch and a running import job
// written by SQL (the scan's part), then catalog.CommitImport. Two tracks
// (the first with an LRC and a duration of 9:05, the second's unknown), a
// cover kept as an attachment, a booklet.
func (e *env) seed(artist, title string) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	batch, job := store.NewID(), store.NewID()
	e.exec(`INSERT INTO import_batches (id, root_rel, created_at) VALUES ($1, 'in', now())`, batch)
	e.exec(`INSERT INTO jobs (id, kind, batch_id, source_rel, state, queued_at, updated_at)
		VALUES ($1, 'import', $2, $3, 'pending', now(), now())`, job, batch, "src-"+job.String())
	var ticket int64
	if err := e.db.QueryRow(ctx, `UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1 RETURNING claimed`,
		job).Scan(&ticket); err != nil {
		e.t.Fatal(err)
	}
	a1, a2, lrc, cover, booklet := newHash(), newHash(), newHash(), newHash(), newHash()
	out, err := e.svc.CommitImport(ctx, catalog.ImportCandidate{
		Attempt:     jobs.Attempt{JobID: job, Ticket: ticket},
		Fingerprint: newHash(),
		Blobs: []catalog.Blob{
			{Hash: a1, Size: 3000, Format: catalog.FormatFLAC, DurationMS: ptr(int64(545_499))},
			{Hash: a2, Size: 2500, Format: catalog.FormatFLAC},
			{Hash: lrc, Size: 20},
			{Hash: cover, Size: 500, Format: catalog.FormatJPEG},
			{Hash: booklet, Size: 1000},
		},
		Artist: artist, Title: title, Year: ptr(1959), Genre: ptr("Jazz"), CoverHash: &cover,
		Tracks: []catalog.ImportTrack{
			{SourcePath: "01 So What.flac", Disc: 1, No: 1, Title: "So What", BlobHash: a1,
				Lyrics: &catalog.ImportLyrics{SourcePath: "01 So What.lrc", BlobHash: lrc}},
			{SourcePath: "02 Freddie.flac", Disc: 1, No: 2, Title: "Freddie Freeloader",
				Artist: ptr("Miles & Cannonball"), Genre: ptr(""), BlobHash: a2},
		},
		Attachments: []catalog.ImportAttachment{
			{RelPath: "Scans/Booklet.pdf", BlobHash: booklet},
			{RelPath: "cover.jpg", BlobHash: cover},
		},
	})
	if err != nil || out.State != jobs.StateDone {
		e.t.Fatalf("CommitImport: %+v %v", out, err)
	}
	return out.AlbumID
}

// album is GET /api/albums/{id}: the body and its ETag.
func (e *env) album(id uuid.UUID) (map[string]any, string) {
	e.t.Helper()
	r := e.must(req{method: "GET", path: "/api/albums/" + id.String()}, nethttp.StatusOK)
	return r.body, r.header.Get("ETag")
}

// putBody is the PUT body that saves the album exactly as the GET shows it.
func putBody(a map[string]any) map[string]any {
	var tracks []any
	for _, raw := range a["tracks"].([]any) {
		t := raw.(map[string]any)
		tracks = append(tracks, map[string]any{
			"id": t["id"], "disc": t["disc"], "no": t["no"], "title": t["title"], "artist": t["artist"], "genre": t["genre"],
		})
	}
	return map[string]any{
		"artist_id": a["artist_id"], "new_artist": nil, "title": a["title"], "year": a["year"], "genre": a["genre"],
		"compilation": a["compilation"], "tracks": tracks,
	}
}

func tracksOf(body map[string]any) []map[string]any {
	var out []map[string]any
	for _, t := range body["tracks"].([]any) {
		out = append(out, t.(map[string]any))
	}
	return out
}

// revision reads an album's revision by SQL.
func (e *env) revision(id uuid.UUID) int64 {
	e.t.Helper()
	var r int64
	if err := e.db.QueryRow(context.Background(), `SELECT revision FROM albums WHERE id = $1`, id).Scan(&r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) artistOf(album uuid.UUID) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	if err := e.db.QueryRow(context.Background(), `SELECT artist_id FROM albums WHERE id = $1`, album).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// openStore makes originals/ and work/ on the ext4 TMPDIR, as the volume
// lays them out (§3.1), and the blob store over them.
func (e *env) openStore() {
	e.t.Helper()
	e.data = e.t.TempDir()
	for _, d := range []string{"originals", "work"} {
		if err := os.Mkdir(filepath.Join(e.data, d), 0o755); err != nil {
			e.t.Fatal(err)
		}
	}
	var err error
	if e.originals, err = fsops.OpenRoot(filepath.Join(e.data, "originals")); err != nil {
		e.t.Fatal(err)
	}
	if e.work, err = fsops.OpenRoot(filepath.Join(e.data, "work")); err != nil {
		e.t.Fatal(err)
	}
	e.imports = e.t.TempDir()
	if e.source, err = fsops.OpenRoot(e.imports); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		for _, r := range []*fsops.Root{e.originals, e.work, e.source} {
			if err := r.Close(); err != nil {
				e.t.Error(err)
			}
		}
	})
	if e.blobs, err = blobstore.New(e.originals, e.work); err != nil {
		e.t.Fatal(err)
	}
	e.budget = jobs.NewBudget()
}

// backend is what the API serves in the tests: the real catalog, the
// real blob store, and the env's track reader.
func (e *env) backend() Backend {
	return Backend{Catalog: e.svc, Blobs: e.blobs, Budget: e.budget, Work: e.work, Source: e.source, Tracks: e.tracks}
}
