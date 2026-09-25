package http

import (
	nethttp "net/http"
	"reflect"
	"testing"
)

// Owner decision N-162: an album with an MP3 track cannot be saved with an
// album or track genre that an MP3 reads back as an ID3v1 genre reference;
// 422 genre_not_writable naming the genre, nothing changed. Without an MP3
// track the same genres are fine.
func TestUpdateAlbumGenreNotWritable(t *testing.T) {
	e := newEnv(t)
	flacOnly := e.seed("Miles Davis", "Kind of Blue")
	withMP3 := e.seed("Miles Davis", "Milestones")
	e.exec(`UPDATE blobs SET format = 'mp3' WHERE hash = (SELECT blob_hash FROM tracks WHERE album_id = $1 AND no = 2)`, withMP3)

	for _, tc := range []struct {
		name  string
		edit  func(body map[string]any)
		genre string
	}{
		{"album genre", func(b map[string]any) { b["genre"] = "(Rock)" }, "(Rock)"},
		{"album genre, a number", func(b map[string]any) { b["genre"] = "13" }, "13"},
		{"first track genre", func(b map[string]any) { b["tracks"].([]any)[0].(map[string]any)["genre"] = "(13)" }, "(13)"},
		{"MP3 track genre", func(b map[string]any) { b["tracks"].([]any)[1].(map[string]any)["genre"] = "101" }, "101"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/albums/" + withMP3.String()
			a, tag := e.album(withMP3)
			rev := e.revision(withMP3)
			body := putBody(a)
			tc.edit(body)
			r := e.wantError(req{method: "PUT", path: path, body: body, ifMatch: tag}, nethttp.StatusUnprocessableEntity, "genre_not_writable")
			if !reflect.DeepEqual(r.details()["names"], []any{tc.genre}) {
				t.Fatalf("details %v", r.details())
			}
			if e.revision(withMP3) != rev {
				t.Fatal("the refused save changed the album")
			}
			// The same save of the FLAC album is fine.
			f, ftag := e.album(flacOnly)
			fb := putBody(f)
			tc.edit(fb)
			e.must(req{method: "PUT", path: "/api/albums/" + flacOnly.String(), body: fb, ifMatch: ftag}, nethttp.StatusOK)
		})
	}
	// Genres an MP3 holds are saved.
	a, tag := e.album(withMP3)
	body := putBody(a)
	body["genre"] = "Rock (live)"
	body["tracks"].([]any)[1].(map[string]any)["genre"] = "200"
	e.must(req{method: "PUT", path: "/api/albums/" + withMP3.String(), body: body, ifMatch: tag}, nethttp.StatusOK)
}

// N-162, the order of the checks: an album that already holds a genre an
// MP3 cannot hold (the importer keeps such a genre, with a warning) can be
// saved unchanged, a no-op; any real change must fix the genre in the same
// save, since it would enqueue a render that fails.
func TestUpdateAlbumStoredGenreNotWritable(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Miles Davis", "Milestones")
	e.exec(`UPDATE blobs SET format = 'mp3' WHERE hash = (SELECT blob_hash FROM tracks WHERE album_id = $1 AND no = 2)`, id)
	e.exec(`UPDATE albums SET genre = '(Rock)' WHERE id = $1`, id)
	e.exec(`DELETE FROM jobs WHERE kind = 'render'`)
	path := "/api/albums/" + id.String()
	a, tag := e.album(id)
	rev := e.revision(id)

	r := e.must(req{method: "PUT", path: path, body: putBody(a), ifMatch: tag}, nethttp.StatusOK)
	if r.body["revision"] != float64(rev) || e.revision(id) != rev ||
		e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`) != 0 {
		t.Fatalf("an unchanged save: %s", r.raw)
	}

	body := putBody(a)
	body["title"] = "Milestones (Remastered)"
	e.wantError(req{method: "PUT", path: path, body: body, ifMatch: tag}, nethttp.StatusUnprocessableEntity, "genre_not_writable")
	if e.revision(id) != rev || e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`) != 0 {
		t.Fatal("the refused change changed the album or enqueued a render")
	}

	body["genre"] = "Rock"
	e.must(req{method: "PUT", path: path, body: body, ifMatch: tag}, nethttp.StatusOK)
}
