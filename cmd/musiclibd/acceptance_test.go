package main

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"musiclib/internal/blobstore"
	"musiclib/internal/fsops"
	"musiclib/internal/maintenance"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

// DESIGN.md §12.3: a real-process release collection, two SIGKILLs and an
// offline backup/restore on completely new destinations. No SQL repair.
func TestReleaseCollectionInterruptedAndRestored(t *testing.T) {
	url := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeAlbum(t, p.imports, "Normal FLAC", "Alice", "One", 2, 2)
	writeMixedAlbum(t, p.imports, "Normal MP3", "Bob", "Two")
	writeMixedAlbum(t, p.imports, "Normal M4A", "Cara", "Three")
	for _, name := range []string{"01.flac", "03.m4a"} {
		if err := os.Remove(filepath.Join(p.imports, "Normal MP3", name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"01.flac", "02.mp3"} {
		if err := os.Remove(filepath.Join(p.imports, "Normal M4A", name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, disc := range []string{"CD1", "Disc 2"} {
		writeFLAC(t, filepath.Join(p.imports, "Multi", disc, "01.flac"), 350, 0.4,
			map[string]string{"ARTIST": "Dan", "ALBUM": "Four", "TITLE": "Movement", "TRACKNUMBER": "1"})
	}
	for i, artist := range []string{"Eva", "Finn"} {
		writeFLAC(t, filepath.Join(p.imports, "Compilation", artist+".flac"), 400+i*50, 0.4,
			map[string]string{"ARTIST": artist, "ALBUM": "Five", "TITLE": artist, "TRACKNUMBER": string(rune('1' + i))})
	}
	writeAlbum(t, p.imports, "Edition Mono", "Gwen", "Six (Mono)", 1, 0.4)
	writeAlbum(t, p.imports, "Edition Stereo", "Gwen", "Six (Stereo)", 1, 0.4)
	writeFile(t, filepath.Join(p.imports, "Normal FLAC", "booklet.txt"), "notes")
	writeFile(t, filepath.Join(p.imports, "Normal FLAC", "01.lrc"), "[00:01.00]a line\n")
	if err := os.WriteFile(filepath.Join(p.imports, "Normal FLAC", "cover.jpg"), testJPEG(t, 32, 32, 4), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startServerProcess(t, url, p, 0o022)
	s.waitHealthy(t)
	d := &testDaemon{base: "http://" + s.addr, logs: s.stderr}
	batch := uuid.NewString()
	d.mustAPI(t, http.MethodPost, "/api/imports", "", map[string]string{"id": batch, "path": ""}, http.StatusCreated)
	db := dbPool(t, url)
	waitFor(t, "import active before crash", func() bool {
		return queryInt(t, db, `SELECT count(*) FROM jobs WHERE kind='import' AND state='running'`) > 0
	})
	if err := s.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-s.done
	s = startServerProcess(t, url, p, 0o022)
	s.waitHealthy(t)
	d = &testDaemon{base: "http://" + s.addr, logs: s.stderr}
	waitFor(t, "seven albums imported and published after crash", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE deleted_at IS NULL AND published_revision=revision AND published_path IS NOT NULL`) == 7
	})
	if n := queryInt(t, db, `SELECT count(*) FROM jobs WHERE state='failed'`); n != 0 {
		t.Fatalf("failed jobs: %d; logs: %s", n, s.stderr)
	}
	// Every imported track has its duration (N-300).
	if n := queryInt(t, db, `SELECT count(*) FROM tracks t JOIN blobs b ON b.hash = t.blob_hash WHERE b.duration_ms IS NULL OR b.duration_ms <= 0`); n != 0 {
		t.Fatalf("%d tracks without a duration", n)
	}
	var id, artist uuid.UUID
	if err := db.QueryRow(t.Context(), `SELECT a.id,a.artist_id FROM albums a WHERE a.title='One'`).Scan(&id, &artist); err != nil {
		t.Fatal(err)
	}
	artistPath := "/api/artists/" + artist.String()
	ar := d.mustAPI(t, http.MethodGet, artistPath, "", nil, http.StatusOK)
	d.mustAPI(t, http.MethodPut, artistPath, ar.header.Get("ETag"), map[string]string{"name": "Alice Renamed"}, http.StatusOK)
	albumPath := "/api/albums/" + id.String()
	current := d.mustAPI(t, http.MethodGet, albumPath, "", nil, http.StatusOK)
	body := acceptancePut(current.body)
	body["title"] = "One Edited"
	body["tracks"].([]any)[0].(map[string]any)["title"] = "Edited Track"
	d.mustAPI(t, http.MethodPut, albumPath, current.header.Get("ETag"), body, http.StatusOK)
	current = d.mustAPI(t, http.MethodGet, albumPath, "", nil, http.StatusOK)
	d.upload(t, http.MethodPut, albumPath+"/cover", current.header.Get("ETag"), testJPEG(t, 40, 40, 5), http.StatusOK)
	current = d.mustAPI(t, http.MethodGet, albumPath, "", nil, http.StatusOK)
	d.mustAPI(t, http.MethodDelete, albumPath+"/cover", current.header.Get("ETag"), nil, http.StatusOK)
	current = d.mustAPI(t, http.MethodGet, albumPath, "", nil, http.StatusOK)
	attachment := d.upload(t, http.MethodPost, albumPath+"/attachments?path=Scans%2Fnew.txt", current.header.Get("ETag"), []byte("added after import"), http.StatusCreated)
	attID := attachmentID(t, attachment.body, "Scans/new.txt")
	current = d.mustAPI(t, http.MethodGet, albumPath, "", nil, http.StatusOK)
	d.mustAPI(t, http.MethodDelete, albumPath+"/attachments/"+attID, current.header.Get("ETag"), nil, http.StatusOK)
	current = d.mustAPI(t, http.MethodGet, albumPath, "", nil, http.StatusOK)
	d.mustAPI(t, http.MethodDelete, albumPath, current.header.Get("ETag"), nil, http.StatusOK)
	current = d.mustAPI(t, http.MethodGet, albumPath, "", nil, http.StatusOK)
	d.mustAPI(t, http.MethodPost, albumPath+"/restore", current.header.Get("ETag"), nil, http.StatusOK)
	waitFor(t, "edits published", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE deleted_at IS NULL AND published_revision=revision AND published_path IS NOT NULL`) == 7
	})
	// Interrupt a real in-progress render, not merely an idle process.
	d.mustAPI(t, http.MethodPost, "/api/render-all", "", nil, http.StatusAccepted)
	waitFor(t, "render active before crash", func() bool {
		return queryInt(t, db, `SELECT count(*) FROM jobs WHERE kind='render' AND state='running'`) > 0
	})
	if err := s.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-s.done
	s = startServerProcess(t, url, p, 0o022)
	s.waitHealthy(t)
	d = &testDaemon{base: "http://" + s.addr, logs: s.stderr}
	waitFor(t, "all seven republished", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE deleted_at IS NULL AND published_revision=revision AND published_path IS NOT NULL`) == 7
	})
	// One edition stays in the trash across backup and restore: it keeps its
	// catalog rows and originals but is neither claimed nor re-rendered.
	var mono uuid.UUID
	if err := db.QueryRow(t.Context(), `SELECT id FROM albums WHERE title='Six (Mono)'`).Scan(&mono); err != nil {
		t.Fatal(err)
	}
	monoPath := "/api/albums/" + mono.String()
	current = d.mustAPI(t, http.MethodGet, monoPath, "", nil, http.StatusOK)
	d.mustAPI(t, http.MethodDelete, monoPath, current.header.Get("ETag"), nil, http.StatusOK)
	waitFor(t, "trashed edition unpublished", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_path IS NOT NULL`) == 6
	})
	before := d.mustAPI(t, http.MethodGet, albumPath, "", nil, http.StatusOK)
	albumRows, err := db.Query(t.Context(), `SELECT id FROM albums ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var albumIDs []uuid.UUID
	for albumRows.Next() {
		var aid uuid.UUID
		if err := albumRows.Scan(&aid); err != nil {
			t.Fatal(err)
		}
		albumIDs = append(albumIDs, aid)
	}
	if err := albumRows.Err(); err != nil {
		t.Fatal(err)
	}
	albumRows.Close()
	catalog := make(map[uuid.UUID]map[string]any)
	for _, aid := range albumIDs {
		catalog[aid] = d.mustAPI(t, http.MethodGet, "/api/albums/"+aid.String(), "", nil, http.StatusOK).body
	}
	reportBefore := d.mustAPI(t, http.MethodGet, "/api/imports/"+batch, "", nil, http.StatusOK).body
	if err := s.cmd.Process.Signal(unix.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := s.wait(t); code != exitOK {
		t.Fatalf("shutdown %d", code)
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
	archive := filepath.Join(t.TempDir(), "release backup")
	if err := maintenance.Backup(t.Context(), db, v, archive, url, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
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
	s = startServerProcess(t, freshURL, freshPaths, 0o022)
	s.waitHealthy(t)
	d = &testDaemon{base: "http://" + s.addr, logs: s.stderr}
	waitFor(t, "restored six publications", func() bool {
		return idle(t, freshDB) && queryInt(t, freshDB, `SELECT count(*) FROM albums WHERE deleted_at IS NULL AND published_revision=revision AND published_path IS NOT NULL`) == 6 && queryInt(t, freshDB, `SELECT count(*) FROM albums WHERE published_path IS NOT NULL`) == 6
	})
	after := d.mustAPI(t, http.MethodGet, albumPath, "", nil, http.StatusOK)
	if !reflect.DeepEqual(before.body, after.body) {
		t.Fatalf("catalog changed after restore: %v != %v", before.body, after.body)
	}
	for _, aid := range albumIDs {
		restored := d.mustAPI(t, http.MethodGet, "/api/albums/"+aid.String(), "", nil, http.StatusOK).body
		if !reflect.DeepEqual(catalog[aid], restored) {
			t.Fatalf("album %s changed after restore", aid)
		}
	}
	if reportAfter := d.mustAPI(t, http.MethodGet, "/api/imports/"+batch, "", nil, http.StatusOK).body; !reflect.DeepEqual(reportBefore, reportAfter) {
		t.Fatalf("import report changed after restore")
	}
	if err := s.cmd.Process.Signal(unix.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := s.wait(t); code != exitOK {
		t.Fatalf("shutdown %d", code)
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
	// Each restored original must have its source hash and exact catalog size.
	rows, err := db.Query(t.Context(), `SELECT hash,size FROM blobs ORDER BY hash`)
	if err != nil {
		t.Fatal(err)
	}
	oldBlobs := map[string]int64{}
	for rows.Next() {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			t.Fatal(err)
		}
		oldBlobs[hash] = size
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for hash, size := range oldBlobs {
		got, err := blobstore.OpenReadOnly(fresh.Originals()).Verify(t.Context(), hash)
		if err != nil || got != size {
			t.Fatalf("restored original %s: %d != %d: %v", hash, got, size, err)
		}
	}
	r, err := maintenance.Doctor(t.Context(), freshDB, fresh, true)
	if err != nil || r.HasErrors() {
		t.Fatalf("doctor after release restore: %+v %v", r, err)
	}
	for _, f := range r.Findings {
		if f.Code != "doctor_unreferenced_blob" || f.Severity != "info" {
			t.Fatalf("unexpected restored finding: %+v", f)
		}
	}
	for _, path := range []string{
		"Alice Renamed/One Edited/Extras/booklet.txt", "Alice Renamed/One Edited/01 - Edited Track.lrc",
		"Dan/Four/Disc 1/01 - Movement.flac", "Dan/Four/Disc 2/01 - Movement.flac",
		"Various Artists/Five/01 - Eva.flac", "Gwen/Six (Stereo)/.musiclib.json",
	} {
		if _, err := fresh.Library().Stat(path); err != nil {
			t.Fatalf("§5.1 layout %s: %v", path, err)
		}
	}
	// The trashed edition kept its catalog (compared above) but no output.
	if _, err := fresh.Library().Stat("Gwen/Six (Mono)"); fsops.Code(err) != fsops.CodeNotFound {
		t.Fatalf("trashed edition published after restore: %v", err)
	}
}

func acceptancePut(a map[string]any) map[string]any {
	tracks := make([]any, 0, len(a["tracks"].([]any)))
	for _, raw := range a["tracks"].([]any) {
		t := raw.(map[string]any)
		tracks = append(tracks, map[string]any{"id": t["id"], "disc": t["disc"], "no": t["no"], "title": t["title"], "artist": t["artist"], "genre": t["genre"]})
	}
	return map[string]any{"artist_id": a["artist_id"], "new_artist": nil, "title": a["title"], "year": a["year"], "genre": a["genre"], "compilation": a["compilation"], "tracks": tracks}
}
