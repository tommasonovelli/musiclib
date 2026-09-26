package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/store/pgtest"
)

// Round 16 end to end: an import driven only through the API of §10.2 on
// the real server with two workers: the /import listing, the batch, the
// real scan and imports, the report per candidate (a failed candidate, an
// ambiguous branch, an unassigned file), the retries with and without the
// overrides of §7.3 (the candidate revalidated each time, §7.2), a batch
// with no valid candidate, and render-all.

// report is GET /api/imports/{id}.
func (d *testDaemon) report(t *testing.T, id uuid.UUID) map[string]any {
	t.Helper()
	return d.mustAPI(t, http.MethodGet, "/api/imports/"+id.String(), "", nil, http.StatusOK).body
}

// waitCompleted polls the report until the batch is completed.
func (d *testDaemon) waitCompleted(t *testing.T, id uuid.UUID) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		r := d.report(t, id)
		if r["state"] == "completed" {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch %s not completed within 2 minutes: %v; logs:\n%s", id, r, d.logs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// candidate is the report's job for source.
func candidate(t *testing.T, report map[string]any, source string) map[string]any {
	t.Helper()
	for _, raw := range report["candidates"].([]any) {
		if c := raw.(map[string]any); c["source_rel"] == source {
			return c
		}
	}
	t.Fatalf("no candidate %q in %v", source, report["candidates"])
	return nil
}

func sourceEntries(t *testing.T, body map[string]any) []string {
	t.Helper()
	var out []string
	for _, raw := range body["entries"].([]any) {
		e := raw.(map[string]any)
		out = append(out, fmt.Sprint(e["name"], ":", e["type"]))
	}
	return out
}

func TestEndToEndImportThroughAPI(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeAlbum(t, p.imports, "batch/Good", "Miles Davis", "Kind of Blue", 2, 0.5)
	// mixed_album (§7.2): two album tags in one directory.
	writeFLAC(t, filepath.Join(p.imports, "batch/Mixed/01.flac"), 300, 0.5, map[string]string{
		"ARTIST": "Bill Evans", "ALBUM": "Portrait in Jazz", "TITLE": "One", "TRACKNUMBER": "1"})
	writeFLAC(t, filepath.Join(p.imports, "batch/Mixed/02.flac"), 350, 0.5, map[string]string{
		"ARTIST": "Bill Evans", "ALBUM": "Sunday at the Village", "TITLE": "Two", "TRACKNUMBER": "2"})
	// Rule 4: direct audio together with audio further down.
	writeAlbum(t, p.imports, "batch/Amb", "Chet Baker", "Chet", 1, 0.5)
	writeFLAC(t, filepath.Join(p.imports, "batch/Amb/sub/x.flac"), 400, 0.5, map[string]string{"TITLE": "X"})
	writeFile(t, filepath.Join(p.imports, "batch/loose.txt"), "not in any album")
	writeFile(t, filepath.Join(p.imports, "empty/readme.txt"), "no audio here")

	cfg := testConfig(dbURL)
	cfg.Workers = 2
	d := startDaemon(t, cfg, p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	db := dbPool(t, dbURL)

	// §7.1: the UI explores /import only.
	src := d.mustAPI(t, http.MethodGet, "/api/import-source", "", nil, http.StatusOK)
	if got := sourceEntries(t, src.body); !slices.Equal(got, []string{"batch:directory", "empty:directory"}) {
		t.Fatalf("/import: %q", got)
	}
	src = d.mustAPI(t, http.MethodGet, "/api/import-source?path=batch", "", nil, http.StatusOK)
	if got := sourceEntries(t, src.body); !slices.Equal(got, []string{"Amb:directory", "Good:directory", "Mixed:directory", "loose.txt:file"}) {
		t.Fatalf("batch: %q", got)
	}

	id := uuid.New()
	body := map[string]string{"id": id.String(), "path": "batch"}
	d.mustAPI(t, http.MethodPost, "/api/imports", "", body, http.StatusCreated)
	d.mustAPI(t, http.MethodPost, "/api/imports", "", body, http.StatusOK)
	r := d.waitCompleted(t, id)

	scan := r["scan"].(map[string]any)
	ws := scan["warnings"].([]any)
	if scan["state"] != "done" || len(ws) != 1 || ws[0].(map[string]any)["code"] != "unassigned_file" ||
		ws[0].(map[string]any)["path"] != "batch/loose.txt" {
		t.Fatalf("scan %v", scan)
	}
	if n := len(r["candidates"].([]any)); n != 3 {
		t.Fatalf("%d candidates: %v", n, r["candidates"])
	}
	good := candidate(t, r, "batch/Good")
	mixed := candidate(t, r, "batch/Mixed")
	amb := candidate(t, r, "batch/Amb")
	if good["state"] != "done" || good["result_album_id"] == nil {
		t.Fatalf("Good: %v", good)
	}
	if mixed["state"] != "failed" || mixed["error_code"] != "mixed_album" || mixed["error_message"] == nil {
		t.Fatalf("Mixed: %v", mixed)
	}
	if amb["state"] != "failed" || amb["error_code"] != "ambiguous_candidate" {
		t.Fatalf("Amb: %v", amb)
	}
	failed := d.mustAPI(t, http.MethodGet, "/api/jobs?state=failed&kind=import", "", nil, http.StatusOK)
	if n := len(failed.body["jobs"].([]any)); n != 2 {
		t.Fatalf("failed imports: %v", failed.body)
	}

	// The overrides of §7.3 fix mixed_album through the retry.
	mixedID := mixed["id"].(string)
	rr := d.mustAPI(t, http.MethodPost, "/api/jobs/"+mixedID+"/retry", "",
		map[string]any{"artist": nil, "title": "Portrait and Sunday"}, http.StatusAccepted)
	if rr.body["state"] != "pending" || rr.body["ticket"].(float64) <= mixed["ticket"].(float64) {
		t.Fatalf("retry: %v", rr.body)
	}
	r = d.waitCompleted(t, id)
	mixed = candidate(t, r, "batch/Mixed")
	if mixed["state"] != "done" {
		t.Fatalf("Mixed after the retry: %v", mixed)
	}
	album := d.mustAPI(t, http.MethodGet, "/api/albums/"+mixed["result_album_id"].(string), "", nil, http.StatusOK)
	if album.body["title"] != "Portrait and Sunday" || album.body["artist_name"] != "Bill Evans" {
		t.Fatalf("the album of the retried import: %v", album.body)
	}

	// A retry revalidates the current candidate (§7.2): unchanged, it fails
	// again; with the nested audio gone, it imports.
	ambID := amb["id"].(string)
	d.mustAPI(t, http.MethodPost, "/api/jobs/"+ambID+"/retry", "", nil, http.StatusAccepted)
	r = d.waitCompleted(t, id)
	again := candidate(t, r, "batch/Amb")
	if again["state"] != "failed" || again["error_code"] != "ambiguous_candidate" || again["ticket"].(float64) <= amb["ticket"].(float64) {
		t.Fatalf("Amb retried unchanged: %v", again)
	}
	if err := os.RemoveAll(filepath.Join(p.imports, "batch/Amb/sub")); err != nil {
		t.Fatal(err)
	}
	d.mustAPI(t, http.MethodPost, "/api/jobs/"+ambID+"/retry", "", nil, http.StatusAccepted)
	r = d.waitCompleted(t, id)
	if c := candidate(t, r, "batch/Amb"); c["state"] != "done" || c["result_album_id"] == nil {
		t.Fatalf("Amb fixed and retried: %v", c)
	}
	// A done job is not retried.
	d.mustAPI(t, http.MethodPost, "/api/jobs/"+ambID+"/retry", "", nil, http.StatusConflict)

	// No valid candidate: completed with an explanation (§7.2).
	empty := uuid.New()
	d.mustAPI(t, http.MethodPost, "/api/imports", "", map[string]string{"id": empty.String(), "path": "empty"}, http.StatusCreated)
	r = d.waitCompleted(t, empty)
	if s := r["scan"].(map[string]any); s["state"] != "failed" || s["error_code"] != "no_valid_candidate" ||
		s["error_message"] == nil || len(r["candidates"].([]any)) != 0 {
		t.Fatalf("empty batch: %v", r)
	}

	// Three albums, all published; render-all builds each again.
	waitFor(t, "the albums to be published", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision = revision`) == 3
	})
	list := d.mustAPI(t, http.MethodGet, "/api/albums", "", nil, http.StatusOK)
	if n := len(list.body["albums"].([]any)); n != 3 {
		t.Fatalf("library: %v", list.body)
	}
	ra := d.mustAPI(t, http.MethodPost, "/api/render-all", "", nil, http.StatusAccepted)
	if ra.body["enqueued"] != float64(3) {
		t.Fatalf("render-all: %v", ra.body)
	}
	before := map[string]bool{}
	rows, err := db.Query(t.Context(), `SELECT published_build::text FROM albums`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		before[b] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "render-all to publish again", func() bool {
		if !idle(t, db) {
			return false
		}
		rs, err := db.Query(t.Context(), `SELECT published_build::text FROM albums`)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		for rs.Next() {
			var b string
			if err := rs.Scan(&b); err != nil {
				t.Fatal(err)
			}
			if before[b] {
				return false
			}
		}
		return true
	})
	// The only failed job left is the explanation of the empty batch.
	if n := queryInt(t, db, `SELECT count(*) FROM jobs WHERE state = 'failed' AND batch_id IS DISTINCT FROM $1`, empty); n != 0 {
		t.Fatalf("%d failed jobs; logs:\n%s", n, d.logs)
	}
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}
}
