package main

import (
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/maintenance"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

// DESIGN.md §11.4, §12.2: a release-gate restore on a new database and a
// new data volume, followed by real boot, publication and deep inspection.
func TestBackupLossRestoreBootAndDoctor(t *testing.T) {
	url := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeMixedAlbum(t, p.imports, "Mixed", "Art Blakey", "Moanin'")
	d := startDaemon(t, testConfig(url), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	d.mustAPI(t, http.MethodPost, "/api/imports", "", map[string]string{"id": uuid.NewString(), "path": ""}, http.StatusCreated)
	db := dbPool(t, url)
	waitFor(t, "original publication", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision=revision AND published_path IS NOT NULL`) == 1
	})
	var album uuid.UUID
	if err := db.QueryRow(t.Context(), `SELECT id FROM albums`).Scan(&album); err != nil {
		t.Fatal(err)
	}
	path := "/api/albums/" + album.String()
	before := d.mustAPI(t, http.MethodGet, path, "", nil, http.StatusOK)
	d.upload(t, http.MethodPost, path+"/attachments?path=Notes%2Fbooklet.txt", before.header.Get("ETag"), []byte("a restored attachment"), http.StatusCreated)
	waitFor(t, "attachment publication", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision=revision AND published_path IS NOT NULL`) == 1
	})
	before = d.mustAPI(t, http.MethodGet, path, "", nil, http.StatusOK)
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}
	v, err := volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := v.OpenExistingLayout(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup with spaces")
	if err := maintenance.Backup(t.Context(), db, v, archive, url, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	// Both destinations really are new. The old volume and database remain
	// available only for assertions; neither is reused by the restored app.
	freshURL := pgtest.EmptyDB(t)
	freshDB := pgtest.Pool(t, freshURL)
	freshPaths := testPaths(t)
	fresh, err := volume.Acquire(freshPaths.data)
	if err != nil {
		t.Fatal(err)
	}
	if err := maintenance.Restore(t.Context(), freshDB, fresh, archive, freshURL, nil); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	up := startDaemon(t, testConfig(freshURL), freshPaths)
	up.waitStatus(t, "/health/ready", http.StatusOK)
	waitFor(t, "restored publication", func() bool {
		return idle(t, freshDB) && queryInt(t, freshDB, `SELECT count(*) FROM albums WHERE published_revision=revision AND published_path IS NOT NULL`) == 1
	})
	after := up.mustAPI(t, http.MethodGet, path, "", nil, http.StatusOK)
	if !reflect.DeepEqual(before.body, after.body) || before.header.Get("ETag") != after.header.Get("ETag") {
		t.Fatalf("restored catalog differs: before=%v after=%v", before.body, after.body)
	}
	if err := up.stop(t); err != nil {
		t.Fatal(err)
	}
	fresh, err = volume.Acquire(freshPaths.data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := fresh.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := fresh.IdentifyExisting(t.Context(), freshDB); err != nil {
		t.Fatal(err)
	}
	if err := fresh.OpenExistingLayout(); err != nil {
		t.Fatal(err)
	}
	r, err := maintenance.Doctor(t.Context(), freshDB, fresh, true)
	if err != nil || r.HasErrors() || len(r.Findings) != 0 {
		t.Fatalf("restored deep doctor: %+v %v", r, err)
	}
}
