package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"musiclib/internal/fsops"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

func doctorEnv(url string) func(string) string {
	return func(key string) string {
		switch key {
		case "DATABASE_URL":
			return url
		case "PUBLIC_ORIGIN":
			return "http://127.0.0.1:8080"
		default:
			return ""
		}
	}
}

func TestDoctorReadOnlyAndRefusals(t *testing.T) {
	url := pgtest.EmptyDB(t)
	p := testPaths(t)
	var stdout, logs bytes.Buffer
	check := func(want int, code string) {
		t.Helper()
		stdout.Reset()
		logs.Reset()
		got := runDoctor(doctorEnv(url), p, true, &stdout, newLogger(&logs))
		if got != want || !strings.Contains(logs.String(), `"code":"`+code+`"`) {
			t.Fatalf("doctor exit=%d stdout=%q logs=%q; want %d %s", got, stdout.String(), logs.String(), want, code)
		}
	}
	// An unmigrated database is refused by the schema check, which must not
	// create even goose's version table (N-217, N-225).
	check(exitUsage, "maintenance_schema")
	if objects, err := store.New(pgtest.Pool(t, url)).RestoreDatabaseObjects(t.Context()); err != nil || objects != 0 {
		t.Fatalf("doctor created goose metadata in empty database: %d %v", objects, err)
	}
	// A current schema without a store marker: refuse, never initialize.
	// The ordinary server boot is responsible for initialization.
	url = pgtest.New(t).Config().ConnString()
	check(exitUsage, "volume_marker_missing")
	var initialized bool
	if err := pgtest.Pool(t, url).QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM settings)`).Scan(&initialized); err != nil || initialized {
		t.Fatalf("doctor wrote settings: %v %v", initialized, err)
	}
	url = "postgres://invalid:secret@127.0.0.1:1/db?sslmode=disable"
	check(exitUsage, "maintenance_database")
	if strings.Contains(logs.String(), "secret") {
		t.Fatal("database password leaked to logs")
	}
	root, err := fsops.OpenRoot(p.data)
	if err != nil {
		t.Fatal(err)
	}
	entries, readErr := root.ReadDir("")
	if readErr != nil || len(entries) != 1 || entries[0].Name != ".lock" {
		t.Fatalf("doctor initialized an empty volume: %+v, %v", entries, readErr)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

// dataTree lists every entry under dir with its mode, size and mtime, so a
// read-only command that created, removed or rewrote anything is caught.
func dataTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(&b, "%s %v %d %d\n", rel, info.Mode(), info.Size(), info.ModTime().UnixNano())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestDoctorCommandCleanExistingStore(t *testing.T) {
	db := pgtest.New(t)
	p := testPaths(t)
	v, err := volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Identify(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := v.OpenLayout(); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	// .lock already exists; nothing else may change, including work/
	// (blobstore.New, unlike OpenReadOnly, would create work/blobs).
	before := dataTree(t, p.data)
	var stdout, logs bytes.Buffer
	url := db.Config().ConnString()
	if got := runDoctor(doctorEnv(url), p, true, &stdout, newLogger(&logs)); got != exitOK || !strings.Contains(stdout.String(), "no damage found") {
		t.Fatalf("doctor code %d output %q logs %q", got, stdout.String(), logs.String())
	}
	if after := dataTree(t, p.data); after != before {
		t.Fatalf("doctor changed the volume:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDoctorLockRefusal(t *testing.T) {
	p := testPaths(t)
	v, err := volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	var out, logs bytes.Buffer
	if code := runDoctor(doctorEnv("postgres://invalid:secret@127.0.0.1:1/db"), p, false, &out, newLogger(&logs)); code != exitUsage || !strings.Contains(logs.String(), "volume_locked") || strings.Contains(logs.String(), "secret") || out.Len() != 0 {
		t.Fatalf("lock refusal: code=%d stdout=%q logs=%q", code, out.String(), logs.String())
	}
}
