package http

import (
	nethttp "net/http"
	"testing"

	"musiclib/internal/catalog"
)

// POST /api/trash/empty: no body, no If-Match; 200 with the albums
// deleted and those still waiting for their removal from library/.
func TestEmptyTrashAPI(t *testing.T) {
	e := newEnv(t)
	done := e.seed("Miles Davis", "Kind of Blue")
	waiting := e.seed("Bill Evans", "Portrait in Jazz")
	active := e.seed("Chet Baker", "Chet")
	for _, id := range []string{done.String(), waiting.String()} {
		e.must(req{method: "DELETE", path: "/api/albums/" + id, ifMatch: `"album:` + id + `:1"`}, ok)
	}
	empty := func(wantDeleted, wantWaiting float64) {
		t.Helper()
		r := e.must(req{method: "POST", path: "/api/trash/empty"}, ok)
		if r.body["deleted"] != wantDeleted || r.body["waiting"] != wantWaiting || len(r.body) != 2 {
			t.Fatalf("empty trash: %s, want %v deleted, %v waiting", r.raw, wantDeleted, wantWaiting)
		}
	}
	empty(0, 2) // both removals are pending
	// The removal of one is published, as FINALIZE records it.
	e.exec(`DELETE FROM jobs WHERE kind = 'render' AND album_id = $1`, done)
	e.exec(`UPDATE albums SET published_revision = revision, published_renderer = 'rv-test' WHERE id = $1`, done)
	empty(1, 1)
	e.wantError(req{method: "GET", path: "/api/albums/" + done.String()}, nethttp.StatusNotFound, catalog.CodeAlbumNotFound)
	for _, id := range []string{waiting.String(), active.String()} {
		e.must(req{method: "GET", path: "/api/albums/" + id}, ok)
	}

	e.wantError(req{method: "POST", path: "/api/trash/empty", body: `{}`}, nethttp.StatusBadRequest, CodeBodyNotAllowed)
	e.wantError(req{method: "GET", path: "/api/trash/empty"}, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed)
	e.wantError(req{method: "POST", path: "/api/trash/empty", headers: map[string][]string{RequestHeader: {""}}},
		nethttp.StatusForbidden, CodeRequestHeaderRequired)
	empty(0, 1)
}
