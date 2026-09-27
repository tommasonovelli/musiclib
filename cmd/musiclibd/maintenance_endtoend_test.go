package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/fsops"
	"musiclib/internal/maintenance"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

// DESIGN.md §11.3 and §12.2: prove that a real imported album whose bytes
// changed without changing size or mtime is detected, then repaired by the
// real renderer. Rebuild must independently regenerate every active album.
func TestOfflineDoctorRepairAndRebuildConverge(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeAlbum(t, p.imports, "Blue", "Miles Davis", "Kind of Blue", 2, 0.3)
	writeAlbum(t, p.imports, "Portrait", "Bill Evans", "Portrait", 1, 0.3)
	d := startDaemon(t, testConfig(dbURL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	db := dbPool(t, dbURL)
	d.mustAPI(t, http.MethodPost, "/api/imports", "", map[string]string{"id": uuid.NewString(), "path": ""}, http.StatusCreated)
	waitFor(t, "initial publications", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision=revision AND published_path IS NOT NULL`) == 2
	})
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}
	v, err := volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err = v.OpenExistingLayout(); err != nil {
		t.Fatal(err)
	}
	if err = v.CheckFilesystem(); err != nil {
		t.Fatal(err)
	}
	id := v.StoreID()
	var rel string
	if err = db.QueryRow(t.Context(), `SELECT published_path FROM albums WHERE title='Kind of Blue'`).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	file := rel + "/01 - Track 1.flac"
	fi, err := v.Library().Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	f, err := v.Library().OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	byteAt := make([]byte, 1)
	if _, err := f.ReadAt(byteAt, 0); err != nil {
		t.Fatal(err)
	}
	byteAt[0] ^= 1
	if _, err := f.WriteAt(byteAt, 0); err != nil {
		t.Fatal(err)
	}
	if err := fsops.SyncAndClose(f); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(p.data, "library", filepath.FromSlash(file)), fi.MTime, fi.MTime); err != nil {
		t.Fatal(err)
	}
	normal, err := maintenance.Doctor(t.Context(), db, v, false)
	if err != nil || normal.HasErrors() {
		t.Fatalf("normal doctor: %+v %v", normal, err)
	}
	deep, err := maintenance.Doctor(t.Context(), db, v, true)
	if err != nil || !hasDoctorCode(deep, "doctor_output_hash") {
		t.Fatalf("deep doctor: %+v %v", deep, err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	d = startDaemon(t, testConfig(dbURL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	var albumID uuid.UUID
	if err := db.QueryRow(t.Context(), `SELECT id FROM albums WHERE title='Kind of Blue'`).Scan(&albumID); err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/albums/" + albumID.String()
	cur := d.mustAPI(t, http.MethodGet, endpoint, "", nil, http.StatusOK)
	before := *published(t, db, albumID).PublishedBuild
	d.mustAPI(t, http.MethodPost, endpoint+"/render", cur.header.Get("ETag"), nil, http.StatusAccepted)
	waitFor(t, "repair render", func() bool { return idle(t, db) && *published(t, db, albumID).PublishedBuild != before })
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}
	v, err = volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err = v.OpenExistingLayout(); err != nil {
		t.Fatal(err)
	}
	repaired, err := maintenance.Doctor(t.Context(), db, v, true)
	if err != nil || repaired.HasErrors() {
		t.Fatalf("repaired doctor: %+v %v", repaired, err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	v, err = volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err = maintenance.Rebuild(t.Context(), db, v, id, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	d = startDaemon(t, testConfig(dbURL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	waitFor(t, "rebuild publications", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision=revision AND published_path IS NOT NULL`) == 2
	})
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}
	v, err = volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err = v.OpenExistingLayout(); err != nil {
		t.Fatal(err)
	}
	clean, err := maintenance.Doctor(t.Context(), db, v, true)
	if err != nil || clean.HasErrors() {
		t.Fatalf("rebuilt doctor: %+v %v", clean, err)
	}
}

func hasDoctorCode(r maintenance.Report, code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}
