package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	for _, rel := range []string{volume.StoreMarker, volume.Originals, volume.Library, volume.Work, "work/blobs"} {
		if !exists(t, filepath.Join(p.data, rel)) {
			t.Fatalf("%s missing after the boot", rel)
		}
	}

	// Only the health endpoints exist, and only for GET.
	if st, _ := d.get(t, "/api/albums"); st != http.StatusNotFound {
		t.Fatalf("GET /api/albums: %d", st)
	}
	resp, err := http.Post(d.base+"/health/live", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /health/live: %d", resp.StatusCode)
	}

	// The database goes away and comes back: readiness follows it, the
	// process stays alive.
	allowConnections(t, dbURL, false)
	d.waitStatus(t, "/health/ready", http.StatusServiceUnavailable)
	if _, code := d.get(t, "/health/ready"); code != "db_unavailable" {
		t.Fatalf("ready without the database: %s", code)
	}
	if st, _ := d.get(t, "/health/live"); st != http.StatusOK {
		t.Fatalf("live without the database: %d", st)
	}
	allowConnections(t, dbURL, true)
	d.waitStatus(t, "/health/ready", http.StatusOK)

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

// §11.1 step 5 as far as it exists: leftover blob temporaries and probe
// directories in work/ are removed; nothing else is.
func TestBootCleansWork(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	bootOnce(t, dbURL, p)

	work := filepath.Join(p.data, volume.Work)
	writeFile(t, filepath.Join(work, "blobs", "0123.tmp"), "partial")
	writeFile(t, filepath.Join(work, "blobs", "keep"), "not a temporary")
	writeFile(t, filepath.Join(work, ".musiclib-probe-a0123", "marker"), "b")
	writeFile(t, filepath.Join(work, "render", "build", "01.flac"), "phase 2")

	d := startDaemon(t, testConfig(dbURL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	for rel, want := range map[string]bool{
		"blobs/0123.tmp":        false,
		".musiclib-probe-a0123": false,
		"blobs/keep":            true,
		"render/build/01.flac":  true,
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
