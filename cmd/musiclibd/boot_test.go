package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/fsops"
	"musiclib/internal/media"
	"musiclib/internal/render"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

// §11.1: HTTP opens first with negative readiness and the lock taken; the
// boot waits for PostgreSQL; readiness turns positive at the end of the boot,
// follows the database afterwards, and the shutdown releases the lock last.
func TestBootReadinessLifecycle(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	allowConnections(t, dbURL, false)
	p := testPaths(t)
	d := startDaemon(t, testConfig(dbURL), p)

	// Boot blocked at step 2.
	d.logs.waitLog(t, "database not reachable yet")
	if st, _ := d.get(t, "/health/live"); st != http.StatusOK {
		t.Fatalf("live during boot: %d", st)
	}
	if st, code := d.get(t, "/health/ready"); st != http.StatusServiceUnavailable || code != "not_ready" {
		t.Fatalf("ready during boot: %d %s", st, code)
	}
	// §10.1, §11.1: the API is mounted from step 1 and answers 503 until
	// the end of the boot, behind the boundary of §10.4.
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		if r := d.api(t, m, "/api/artists", "", nil); r.status != http.StatusServiceUnavailable || r.code() != "not_ready" {
			t.Fatalf("%s /api/artists during boot: %d %v", m, r.status, r.body)
		}
	}
	assertLockHeld(t, p.data)
	if exists(t, filepath.Join(p.data, volume.StoreMarker)) {
		t.Fatal("the volume was initialized before the database answered")
	}

	// PostgreSQL comes up: the boot completes.
	allowConnections(t, dbURL, true)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	if !d.logs.has(t, "ready") {
		t.Fatal("no ready event")
	}
	// Step 3 verified the pinned tools and logged the versions it read.
	verified := false
	for _, ev := range d.logs.events(t) {
		if ev["msg"] == "media tools verified" {
			verified = ev["ffmpeg"] == media.PinnedVersion && ev["ffprobe"] == media.PinnedVersion &&
				ev["musiclib_tags"] == media.PinnedTagsVersion && ev["taglib"] == media.PinnedTagLibVersion &&
				ev["render_version"] == render.Version
		}
	}
	if !verified {
		t.Fatalf("no media tools verified event with the pinned versions; logs:\n%s", d.logs)
	}
	for _, rel := range []string{volume.StoreMarker, volume.Originals, volume.Library, volume.Work, "work/blobs"} {
		if !exists(t, filepath.Join(p.data, rel)) {
			t.Fatalf("%s missing after the boot", rel)
		}
	}

	// The API serves now. The health endpoints are outside its Host check
	// (N-145): d.get addresses the server by its loopback port, not by
	// PUBLIC_ORIGIN, and the API refuses exactly that.
	if r := d.mustAPI(t, http.MethodGet, "/api/artists", "", nil, http.StatusOK); r.body["artists"] == nil {
		t.Fatalf("GET /api/artists: %v", r.body)
	}
	if st, code := d.get(t, "/api/artists"); st != http.StatusMisdirectedRequest || code != "host_not_allowed" {
		t.Fatalf("GET /api/artists with the loopback Host: %d %s", st, code)
	}
	if st, code := d.get(t, "/api/albums"); st != http.StatusMisdirectedRequest || code != "host_not_allowed" {
		t.Fatalf("GET /api/albums with the loopback Host: %d %s", st, code)
	}
	if r := d.mustAPI(t, http.MethodGet, "/api/albums", "", nil, http.StatusOK); r.body["albums"] == nil {
		t.Fatalf("GET /api/albums: %v", r.body)
	}
	// Without a session, with the right Host: the API is 401 and a page
	// sends the browser to /login. The health endpoints need no session.
	noSession := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for path, want := range map[string]int{"/api/artists": http.StatusUnauthorized, "/": http.StatusSeeOther} {
		req, err := http.NewRequest(http.MethodGet, d.base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "127.0.0.1:8080"
		resp, err := noSession.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want || (want == http.StatusSeeOther && resp.Header.Get("Location") != "/login") {
			t.Fatalf("GET %s without a session: %d %v", path, resp.StatusCode, resp.Header)
		}
	}
	for _, path := range []string{"/health/live", "/health/ready"} {
		if st, _ := d.get(t, path); st != http.StatusOK {
			t.Fatalf("GET %s without a session: %d", path, st)
		}
	}
	// The health endpoints: GET only, nosniff, no CORS.
	resp, err := http.Post(d.base+"/health/live", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("POST /health/live: %d %v", resp.StatusCode, resp.Header)
	}
	resp, err = http.Get(d.base + "/health/ready")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("GET /health/ready headers %v", resp.Header)
	}

	// Shutdown.
	assertLockHeld(t, p.data)
	if err := d.stop(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertShutdownOrder(t, d.logs, true)
	assertLockFree(t, p.data)
	if st, _ := d.get(t, "/health/live"); st != 0 {
		t.Fatalf("live after shutdown: %d", st)
	}
}

// §6.4 with the workers running (N-070): when the database goes away,
// readiness turns negative, and the next statement of a worker (its 2 s
// poll) finds the connection lost, which is fatal: the workers stop and
// run returns the fatal error, so that the process exits non-zero and
// Docker restarts it; the lock is released last. A restart waits for the
// database and boots normally.
func TestDatabaseLossStopsTheProcess(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	d := startDaemon(t, testConfig(dbURL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)

	allowConnections(t, dbURL, false)
	// The API answers 503 with a message of its own, whichever of the API
	// and the pool meets the loss first; or nothing once the run ended.
	r := d.api(t, http.MethodGet, "/api/artists", "", nil)
	switch {
	case r.status == 0:
	case r.status == http.StatusServiceUnavailable && (r.code() == store.CodeConnectionLost || r.code() == "shutting_down"):
		if msg, _ := r.body["message"].(string); strings.Contains(msg, "SQLSTATE") || strings.Contains(msg, "musiclib_test") {
			t.Fatalf("database text in the answer: %q", msg)
		}
	default:
		t.Fatalf("GET /api/artists after the loss: %d %v", r.status, r.body)
	}
	err := d.wait(t)
	if !store.IsFatal(err) || codeOf(err) != store.CodeConnectionLost {
		t.Fatalf("run: %v (code %q), want %s", err, codeOf(err), store.CodeConnectionLost)
	}
	// Whichever met the loss first stopped the run.
	if !d.logs.has(t, "fatal failure, stopping the workers") && !d.logs.has(t, "fatal failure in an API request, stopping") {
		t.Fatalf("no fatal event; logs:\n%s", d.logs)
	}
	assertShutdownOrder(t, d.logs, true)
	assertLockFree(t, p.data)

	allowConnections(t, dbURL, true)
	again := startDaemon(t, testConfig(dbURL), p)
	again.waitStatus(t, "/health/ready", http.StatusOK)
}

// A termination while the boot waits for PostgreSQL is a normal stop: no
// error, nothing written to the volume, the lock released.
func TestStopWhileWaitingForDatabase(t *testing.T) {
	p := testPaths(t)
	d := startDaemon(t, testConfig("postgres://musiclib@127.0.0.1:1/none?connect_timeout=1"), p)
	d.logs.waitLog(t, "database not reachable yet")
	if err := d.stop(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertShutdownOrder(t, d.logs, true)
	assertLockFree(t, p.data)
	if exists(t, filepath.Join(p.data, volume.StoreMarker)) || exists(t, filepath.Join(p.data, volume.Work)) {
		t.Fatal("the volume was written without a database")
	}
}

// A restart on the same volume and database finds the same identity.
func TestRestartOnSamePair(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	var ids []string
	for range 2 {
		d := startDaemon(t, testConfig(dbURL), p)
		d.waitStatus(t, "/health/ready", http.StatusOK)
		if err := d.stop(t); err != nil {
			t.Fatal(err)
		}
		for _, ev := range d.logs.events(t) {
			if ev["msg"] == "ready" {
				ids = append(ids, ev["store_id"].(string))
			}
		}
	}
	if len(ids) != 2 || ids[0] != ids[1] || ids[0] == "" {
		t.Fatalf("store ids across restarts: %q", ids)
	}
}

// Every refusal is fatal, returns its code, never makes readiness positive
// and releases the lock.
func TestBootRefusals(t *testing.T) {
	unreachable := "postgres://musiclib@127.0.0.1:1/none?connect_timeout=1"
	for _, tc := range []struct {
		name  string
		code  string
		setup func(t *testing.T, p *paths) (dbURL string)
	}{
		{
			// The maintenance marker is checked before the database: the
			// boot fails at once even though PostgreSQL is unreachable.
			name: "maintenance marker", code: volume.CodeMaintenance,
			setup: func(t *testing.T, p *paths) string {
				m := volume.Maintenance{Operation: volume.OpRebuild, StoreID: store.NewID()}
				writeFile(t, filepath.Join(p.data, volume.MaintenanceMarker), string(m.Encode()))
				return unreachable
			},
		},
		{
			name: "malformed maintenance marker", code: volume.CodeMaintenanceMalformed,
			setup: func(t *testing.T, p *paths) string {
				writeFile(t, filepath.Join(p.data, volume.MaintenanceMarker), "rebuild\n")
				return unreachable
			},
		},
		{
			name: "store mismatch", code: volume.CodeStoreMismatch,
			setup: func(t *testing.T, p *paths) string {
				dbURL := pgtest.EmptyDB(t)
				bootOnce(t, dbURL, *p)
				marker := filepath.Join(p.data, volume.StoreMarker)
				if err := os.Chmod(marker, 0o644); err != nil {
					t.Fatal(err)
				}
				writeFile(t, marker, string(volume.EncodeStoreMarker(store.NewID())))
				return dbURL
			},
		},
		{
			name: "new database, initialized volume", code: volume.CodeDBUninitialized,
			setup: func(t *testing.T, p *paths) string {
				bootOnce(t, pgtest.EmptyDB(t), *p)
				return pgtest.EmptyDB(t)
			},
		},
		{
			name: "volume locked", code: volume.CodeLocked,
			setup: func(t *testing.T, p *paths) string {
				v, err := volume.Acquire(p.data)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := v.Close(); err != nil {
						t.Error(err)
					}
				})
				return unreachable
			},
		},
		{
			name: "missing data volume", code: volume.CodeUnavailable,
			setup: func(t *testing.T, p *paths) string {
				p.data = filepath.Join(p.data, "missing")
				return unreachable
			},
		},
		{
			name: "missing import source", code: codeImport,
			setup: func(t *testing.T, p *paths) string {
				p.imports = filepath.Join(p.imports, "missing")
				return pgtest.EmptyDB(t)
			},
		},
		{
			// §7.1: "La root non può essere /data" (N-197): /import mounted
			// on the data volume itself, or on one of its directories.
			name: "import source is the data volume", code: codeImportIsData,
			setup: func(t *testing.T, p *paths) string {
				p.imports = p.data
				return pgtest.EmptyDB(t)
			},
		},
		{
			name: "import source is library/", code: codeImportIsData,
			setup: func(t *testing.T, p *paths) string {
				dbURL := pgtest.EmptyDB(t)
				bootOnce(t, dbURL, *p)
				p.imports = filepath.Join(p.data, "library")
				return dbURL
			},
		},
		{
			// A bind mount is another path to the same directory: a
			// symlink stands for it here (OpenRoot follows the host path
			// it is given), and the identity is what is compared.
			name: "import source is originals/ by another path", code: codeImportIsData,
			setup: func(t *testing.T, p *paths) string {
				dbURL := pgtest.EmptyDB(t)
				bootOnce(t, dbURL, *p)
				link := filepath.Join(t.TempDir(), "import")
				if err := os.Symlink(filepath.Join(p.data, "originals"), link); err != nil {
					t.Fatal(err)
				}
				p.imports = link
				return dbURL
			},
		},
		{
			// §2.1, §11.1 step 3: the pinned tools are a required primitive.
			name: "missing ffprobe", code: media.CodeToolUnavailable,
			setup: func(t *testing.T, p *paths) string {
				p.ffprobe = filepath.Join(t.TempDir(), "ffprobe")
				return pgtest.EmptyDB(t)
			},
		},
		{
			name: "missing musiclib-tags", code: media.CodeToolUnavailable,
			setup: func(t *testing.T, p *paths) string {
				p.tags = filepath.Join(t.TempDir(), "musiclib-tags")
				return pgtest.EmptyDB(t)
			},
		},
		{
			// The helper at another version: a TagLib bump without the
			// matching media.PinnedTagLibVersion.
			name: "musiclib-tags of another version", code: media.CodeToolVersion,
			setup: func(t *testing.T, p *paths) string {
				p.tags = filepath.Join(t.TempDir(), "musiclib-tags")
				writeFile(t, p.tags, `#!/bin/sh
echo '{"helper":"2","taglib":"2.3.1-musiclib1"}'
`)
				if err := os.Chmod(p.tags, 0o755); err != nil {
					t.Fatal(err)
				}
				return pgtest.EmptyDB(t)
			},
		},
		{
			name: "ffmpeg of another version", code: media.CodeToolVersion,
			setup: func(t *testing.T, p *paths) string {
				p.ffmpeg = filepath.Join(t.TempDir(), "ffmpeg")
				writeFile(t, p.ffmpeg, "#!/bin/sh\necho 'ffmpeg version 8.1.3 Copyright (c) 2000-2026 the FFmpeg developers'\n")
				if err := os.Chmod(p.ffmpeg, 0o755); err != nil {
					t.Fatal(err)
				}
				return pgtest.EmptyDB(t)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			dbURL := tc.setup(t, &p)
			var before []byte
			marker := filepath.Join(p.data, volume.StoreMarker)
			if exists(t, marker) {
				var err error
				if before, err = os.ReadFile(marker); err != nil {
					t.Fatal(err)
				}
			}
			d := startDaemon(t, testConfig(dbURL), p)
			err := d.wait(t)
			if got := codeOf(err); got != tc.code {
				t.Fatalf("run: code %q, want %q (error: %v)", got, tc.code, err)
			}
			if d.logs.has(t, "ready") {
				t.Fatal("readiness turned positive on a refused boot")
			}
			if before != nil {
				if after, err := os.ReadFile(marker); err != nil || string(after) != string(before) {
					t.Fatalf("the volume marker changed: %q -> %q (%v)", before, after, err)
				}
			}
			if tc.code != volume.CodeLocked && tc.code != volume.CodeUnavailable {
				assertShutdownOrder(t, d.logs, tc.code != volume.CodeMaintenance && tc.code != volume.CodeMaintenanceMalformed)
				assertLockFree(t, p.data)
			}
		})
	}
}

// bootOnce runs a complete boot and stops.
func bootOnce(t *testing.T, dbURL string, p paths) {
	t.Helper()
	d := startDaemon(t, testConfig(dbURL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}
}

// §11.1 step 5: leftover blob temporaries, probe directories, the
// importer's work/import content, and the builds and retired directories
// that no journal references are removed; nothing else is.
func TestBootCleansWork(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	bootOnce(t, dbURL, p)

	work := filepath.Join(p.data, volume.Work)
	writeFile(t, filepath.Join(work, "blobs", "0123.tmp"), "partial")
	writeFile(t, filepath.Join(work, "blobs", "keep"), "not a temporary")
	writeFile(t, filepath.Join(work, ".musiclib-probe-a0123", "marker"), "b")
	writeFile(t, filepath.Join(work, "render", "build", "album", "01.flac"), "a build")
	writeFile(t, filepath.Join(work, "retired", "old", "01.flac"), "a retired album")
	writeFile(t, filepath.Join(work, "import", "x.img"), "an interrupted import")

	d := startDaemon(t, testConfig(dbURL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	for rel, want := range map[string]bool{
		"blobs/0123.tmp":        false,
		".musiclib-probe-a0123": false,
		"blobs/keep":            true,
		"render/build":          false,
		"retired/old":           false,
		"render":                true,
		"retired":               true,
		"import/x.img":          false,
		"import":                true,
	} {
		if got := exists(t, filepath.Join(work, rel)); got != want {
			t.Fatalf("work/%s exists = %v, want %v", rel, got, want)
		}
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".musiclib-probe-") {
			t.Fatalf("probe directory left in work/: %s", e.Name())
		}
	}
	if !d.logs.has(t, "work cleaned") {
		t.Fatal("no work cleaned event")
	}
}

// §6.4 through the API: a change whose COMMIT answer is lost has an
// unknown outcome. The API answers 503 store_commit_uncertain, refuses
// everything after it, and the run ends with that fatal error, so that the
// process exits 1 and Docker restarts it; the change is durable.
//
// The workers' idle polls commit through the same proxy every 2 s (§6.4),
// so the armed loss could reach one of them instead of the API. A lock on
// jobs, held from a direct connection while the API request runs, keeps
// them out: LOCK TABLE returns only once a poll already running has
// ended, and every later poll waits at its first read of jobs, before its
// COMMIT. The only COMMIT through the proxy is then the API's (N-320).
func TestAPIFatalErrorStopsTheProcess(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	proxy := pgtest.NewProxy(t, dbURL)
	p := testPaths(t)
	d := startDaemon(t, testConfig(proxy.URL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)

	release := holdJobsTable(t, dbPool(t, dbURL))
	proxy.LoseNextCommitAck()
	r := d.api(t, http.MethodPost, "/api/artists", "", map[string]string{"name": "Lost Answer"})
	release()
	err := d.wait(t)
	if codeOf(err) != store.CodeCommitUncertain {
		t.Fatalf("run: %v (code %q), want %s", err, codeOf(err), store.CodeCommitUncertain)
	}
	if r.status != http.StatusServiceUnavailable || r.code() != store.CodeCommitUncertain ||
		!d.logs.has(t, "fatal failure in an API request, stopping") {
		t.Fatalf("API answer %d %v; logs:\n%s", r.status, r.body, d.logs)
	}
	if d.logs.has(t, "fatal failure, stopping the workers") {
		t.Fatalf("a worker met the lost answer; logs:\n%s", d.logs)
	}
	assertShutdownOrder(t, d.logs, true)
	assertLockFree(t, p.data)
	if n := queryInt(t, dbPool(t, dbURL), `SELECT count(*) FROM artists WHERE name = 'Lost Answer'`); n != 1 {
		t.Fatalf("%d artists: the change must be durable", n)
	}
}

// holdJobsTable takes an ACCESS EXCLUSIVE lock on jobs in a transaction of
// db and returns the function that rolls it back. It returns once a claim
// already running has ended; the claims that follow wait at their first
// read of jobs until the release. A test that fails before releasing it
// releases it at cleanup, before db is closed.
func holdJobsTable(t *testing.T, db *pgxpool.Pool) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	release = func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}
	t.Cleanup(release)
	if _, err := tx.Exec(ctx, `LOCK TABLE jobs IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	return release
}

// A data directory that cannot be described while /import is compared
// with it (N-197) stops the boot with the import code, not a raw fsops
// error: here the volume's roots are closed under the check.
func TestImportIsNotDataStatFailure(t *testing.T) {
	p := testPaths(t)
	v, err := volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	in, err := fsops.OpenRoot(p.imports)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := in.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	d := &daemon{paths: p, vol: v}
	err = d.importIsNotData(in)
	var be *bootError
	if !errors.As(err, &be) || codeOf(err) != codeImport {
		t.Fatalf("importIsNotData: %v (code %s), want a bootError %s", err, codeOf(err), codeImport)
	}
	if fsops.Code(err) != fsops.CodeRootClosed {
		t.Fatalf("the cause is lost: %v", err)
	}
}
