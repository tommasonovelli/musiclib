package http

import (
	"bytes"
	"encoding/json"
	"maps"
	nethttp "net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

const (
	ok          = nethttp.StatusOK
	created     = nethttp.StatusCreated
	accepted    = nethttp.StatusAccepted
	badRequest  = nethttp.StatusBadRequest
	notFound404 = nethttp.StatusNotFound
	conflict    = nethttp.StatusConflict
	unprocess   = nethttp.StatusUnprocessableEntity
	failed412   = nethttp.StatusPreconditionFailed
	required428 = nethttp.StatusPreconditionRequired
	unavailable = nethttp.StatusServiceUnavailable
)

// §10.2: the artists. The list hides artists without albums (§4.3), the
// creation answers 409 with the existing artist, the rename needs the
// artist's ETag and bumps every album (§4.3).
func TestArtists(t *testing.T) {
	e := newEnv(t)
	album := e.seed("Miles Davis", "Kind of Blue")
	miles := e.artistOf(album)
	trashOnly := e.seed("Trash Only", "Gone")
	e.exec(`UPDATE albums SET deleted_at = now() WHERE id = $1`, trashOnly)

	// Create.
	r := e.must(req{method: "POST", path: "/api/artists", body: `{"name":"  Bill Evans "}`}, created)
	bill := r.body
	if bill["name"] != "Bill Evans" || bill["revision"] != 1.0 {
		t.Fatalf("created %s", r.raw)
	}
	billID := uuid.MustParse(bill["id"].(string))
	if r.header.Get("Location") != "/api/artists/"+billID.String() || bill["etag"] != ETag(KindArtist, billID, 1) {
		t.Fatalf("Location %q, body %s", r.header.Get("Location"), r.raw)
	}
	if r.header.Get("ETag") != "" {
		t.Fatal("a POST answer carries an ETag field")
	}

	// Get: name, revision, ETag, nothing derived.
	r = e.must(req{method: "GET", path: "/api/artists/" + billID.String()}, ok)
	if r.header.Get("ETag") != ETag(KindArtist, billID, 1) || !reflect.DeepEqual(r.body, bill) {
		t.Fatalf("GET = %s, ETag %s", r.raw, r.header.Get("ETag"))
	}
	if !reflect.DeepEqual(keysOf(r.body), []string{"etag", "id", "name", "revision"}) {
		t.Fatalf("artist fields %v", keysOf(r.body))
	}

	// The list: every artist, by folder key (owner decision N-146): the
	// new artist without albums and the trash-only one included.
	r = e.must(req{method: "GET", path: "/api/artists"}, ok)
	var listed []string
	for _, a := range r.body["artists"].([]any) {
		listed = append(listed, a.(map[string]any)["name"].(string))
	}
	if !reflect.DeepEqual(listed, []string{"Bill Evans", "Miles Davis", "Trash Only"}) {
		t.Fatalf("list %v", listed)
	}

	// Conflicts: the same artist after NFC, trim and casefold; a name that
	// shares the folder only through sanitization. Both 409 with the
	// existing artist.
	r = e.wantError(req{method: "POST", path: "/api/artists", body: `{"name":"MILES DAVIS"}`}, conflict, catalog.CodeArtistExists)
	existing := r.details()["artist"].(map[string]any)
	if existing["id"] != miles.String() || existing["name"] != "Miles Davis" || existing["etag"] != ETag(KindArtist, miles, 1) ||
		r.details()["artist_id"] != miles.String() {
		t.Fatalf("409 details %s", r.raw)
	}
	e.must(req{method: "POST", path: "/api/artists", body: `{"name":"AC/DC"}`}, created)
	r = e.wantError(req{method: "POST", path: "/api/artists", body: `{"name":"AC_DC"}`}, conflict, catalog.CodeArtistFolderConflict)
	if r.details()["artist"].(map[string]any)["name"] != "AC/DC" ||
		!reflect.DeepEqual(r.details()["names"], []any{"AC_DC", "AC/DC"}) {
		t.Fatalf("409 details %s", r.raw)
	}
	e.wantError(req{method: "POST", path: "/api/artists", body: `{"name":" "}`}, unprocess, names.CodeTextEmpty)
	e.wantError(req{method: "POST", path: "/api/artists", body: `{"name":"a\u0007b"}`}, unprocess, names.CodeTextControlChar)
	e.wantError(req{method: "POST", path: "/api/artists", body: `{}`}, unprocess, CodeMissingField)
	if n := e.count(`SELECT count(*) FROM artists`); n != 4 {
		t.Fatalf("%d artists, want 4: a refused creation wrote a row", n)
	}

	// Rename: If-Match required, stale refused, the albums bumped and
	// enqueued atomically (§4.3).
	path := "/api/artists/" + miles.String()
	e.wantError(req{method: "PUT", path: path, body: `{"name":"Miles Dewey Davis"}`}, required428, catalog.CodePreconditionRequired)
	r = e.wantError(req{method: "PUT", path: path, body: `{"name":"Miles Dewey Davis"}`, ifMatch: ETag(KindArtist, miles, 7)},
		failed412, catalog.CodePreconditionFailed)
	if r.details()["revision"] != 1.0 {
		t.Fatalf("412 details %s", r.raw)
	}
	_, albumTag := e.album(album)
	e.exec(`DELETE FROM jobs WHERE kind = 'render'`)
	r = e.must(req{method: "PUT", path: path, body: `{"name":"Miles Dewey Davis"}`, ifMatch: ETag(KindArtist, miles, 1)}, ok)
	if r.body["name"] != "Miles Dewey Davis" || r.body["revision"] != 2.0 || r.body["etag"] != ETag(KindArtist, miles, 2) {
		t.Fatalf("rename = %s", r.raw)
	}
	if e.revision(album) != 2 || e.count(`SELECT count(*) FROM jobs WHERE kind = 'render' AND album_id = $1`, album) != 1 {
		t.Fatal("the rename did not bump and enqueue the album")
	}
	a, _ := e.album(album)
	if a["artist_name"] != "Miles Dewey Davis" {
		t.Fatalf("album after the rename: %v", a)
	}
	// The album's old ETag is stale now.
	e.wantError(req{method: "DELETE", path: "/api/albums/" + album.String(), ifMatch: albumTag}, failed412, catalog.CodePreconditionFailed)
	// The rename conflicts like a creation, and changes nothing.
	e.wantError(req{method: "PUT", path: path, body: `{"name":"bill evans"}`, ifMatch: ETag(KindArtist, miles, 2)},
		conflict, catalog.CodeArtistFolderConflict)
	e.wantError(req{method: "PUT", path: path, body: `{"name":1}`, ifMatch: ETag(KindArtist, miles, 2)}, unprocess, CodeInvalidField)

	// Unknown and malformed ids.
	e.wantError(req{method: "GET", path: "/api/artists/" + store.NewID().String()}, notFound404, catalog.CodeArtistNotFound)
	e.wantError(req{method: "GET", path: "/api/artists/not-an-id"}, notFound404, catalog.CodeArtistNotFound)
	e.wantError(req{method: "GET", path: "/api/artists/" + strings.ToUpper(miles.String())}, notFound404, catalog.CodeArtistNotFound)
	e.wantError(req{method: "PUT", path: "/api/artists/" + billID.String(), body: `{"name":"x"}`,
		ifMatch: ETag(KindArtist, miles, 2)}, failed412, catalog.CodePreconditionFailed)
	e.wantError(req{method: "PUT", path: "/api/artists/" + store.NewID().String(), body: `{"name":"x"}`,
		ifMatch: ETag(KindArtist, miles, 2)}, notFound404, catalog.CodeArtistNotFound)
}

// §10.2: eight concurrent creations of one artist: one 201, seven 409 with
// that artist, never a database error.
func TestCreateArtistConcurrently(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	results := make([]resp, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.do(req{method: "POST", path: "/api/artists", body: `{"name":"Nina Simone"}`})
		}()
	}
	wg.Wait()
	var id string
	n201 := 0
	for _, r := range results {
		switch r.status {
		case created:
			n201++
			id = r.body["id"].(string)
		case conflict:
		default:
			t.Fatalf("answer %d %s", r.status, r.raw)
		}
	}
	for _, r := range results {
		if r.status == conflict && r.details()["artist"].(map[string]any)["id"] != id {
			t.Fatalf("409 names another artist: %s", r.raw)
		}
	}
	if n201 != 1 || e.count(`SELECT count(*) FROM artists`) != 1 {
		t.Fatalf("%d created", n201)
	}
}

// §10.1, §10.2: the album's desired aggregate, its ETag, deterministic
// bytes; the status is a separate resource without a catalog ETag.
func TestGetAlbumAndStatus(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Miles Davis", "Kind of Blue")
	artist := e.artistOf(id)
	r := e.must(req{method: "GET", path: "/api/albums/" + id.String()}, ok)
	if r.header.Get("ETag") != ETag(KindAlbum, id, 1) || r.body["etag"] != ETag(KindAlbum, id, 1) {
		t.Fatalf("ETag %q, body etag %v", r.header.Get("ETag"), r.body["etag"])
	}
	if r.header.Get("Cache-Control") != "no-store" || r.header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("headers %v", r.header)
	}
	// The field order is the declared one, and the same state is the same
	// bytes.
	wantOrder := []string{"id", "revision", "etag", "artist_id", "artist_name", "title", "year", "genre", "compilation",
		"trashed", "cover", "tracks", "attachments"}
	if got := topLevelKeys(t, r.raw); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("field order %v", got)
	}
	again := e.must(req{method: "GET", path: "/api/albums/" + id.String()}, ok)
	if !bytes.Equal(r.raw, again.raw) {
		t.Fatalf("two GETs of the same state differ:\n%s\n%s", r.raw, again.raw)
	}
	a := r.body
	if a["artist_id"] != artist.String() || a["artist_name"] != "Miles Davis" || a["title"] != "Kind of Blue" ||
		a["year"] != 1959.0 || a["genre"] != "Jazz" || a["compilation"] != false || a["trashed"] != false || a["revision"] != 1.0 {
		t.Fatalf("album %s", r.raw)
	}
	cover := a["cover"].(map[string]any)
	if cover["format"] != "jpeg" || cover["size"] != 500.0 || len(cover["hash"].(string)) != 64 {
		t.Fatalf("cover %v", cover)
	}
	ts := tracksOf(a)
	if len(ts) != 2 || ts[0]["no"] != 1.0 || ts[0]["artist"] != nil || ts[0]["genre"] != nil || ts[0]["lyrics_hash"] == nil ||
		ts[0]["source_path"] != "01 So What.flac" || ts[0]["blob"].(map[string]any)["format"] != "flac" ||
		ts[1]["artist"] != "Miles & Cannonball" || ts[1]["genre"] != "" || ts[1]["lyrics_hash"] != nil {
		t.Fatalf("tracks %v", ts)
	}
	atts := a["attachments"].([]any)
	// By path key: "cover.jpg" before "scans/booklet.pdf".
	if len(atts) != 2 || atts[0].(map[string]any)["rel_path"] != "cover.jpg" ||
		atts[1].(map[string]any)["blob"].(map[string]any)["format"] != nil || atts[1].(map[string]any)["rel_path"] != "Scans/Booklet.pdf" {
		t.Fatalf("attachments %v", atts)
	}
	// HEAD: the headers of the GET, no body.
	h := e.must(req{method: "HEAD", path: "/api/albums/" + id.String()}, ok)
	if len(h.raw) != 0 || h.header.Get("ETag") != ETag(KindAlbum, id, 1) {
		t.Fatalf("HEAD: %q, %v", h.raw, h.header)
	}

	// Status.
	s := e.must(req{method: "GET", path: "/api/albums/" + id.String() + "/status"}, ok)
	if s.header.Get("ETag") != "" || s.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status headers %v", s.header)
	}
	job := s.body["job"].(map[string]any)
	if s.body["album_id"] != id.String() || s.body["revision"] != 1.0 || s.body["published_revision"] != 0.0 ||
		s.body["published_renderer"] != nil || s.body["published_path"] != nil || s.body["renderer"] != testRender ||
		s.body["trashed"] != false || job["state"] != "pending" || job["error_code"] != nil {
		t.Fatalf("status %s", s.raw)
	}
	// A published album without a job; a failed render with a database
	// error: the stored text never reaches the client (N-150).
	e.exec(`UPDATE albums SET published_path = 'Miles Davis/Kind of Blue', published_revision = 1,
		published_renderer = 'rv-old', published_build = $2, published_receipt_hash = $3 WHERE id = $1`, id, store.NewID(), newHash())
	e.exec(`UPDATE jobs SET state = 'failed', error_code = 'catalog_db',
		error_message = 'reading album: ERROR: relation "albums" does not exist (SQLSTATE 42P01)' WHERE album_id = $1`, id)
	s = e.must(req{method: "GET", path: "/api/albums/" + id.String() + "/status"}, ok)
	job = s.body["job"].(map[string]any)
	if s.body["published_path"] != "Miles Davis/Kind of Blue" || s.body["published_renderer"] != "rv-old" ||
		job["state"] != "failed" || job["error_code"] != "catalog_db" || job["error_message"] != catalog.DatabaseJobMessage {
		t.Fatalf("status %s", s.raw)
	}
	e.exec(`UPDATE jobs SET error_code = 'media_decode', error_message = 'track 01: the decoder refused the stream' WHERE album_id = $1`, id)
	s = e.must(req{method: "GET", path: "/api/albums/" + id.String() + "/status"}, ok)
	if s.body["job"].(map[string]any)["error_message"] != "track 01: the decoder refused the stream" {
		t.Fatalf("a content error's message must stay: %s", s.raw)
	}
	e.exec(`DELETE FROM jobs WHERE album_id = $1`, id)
	s = e.must(req{method: "GET", path: "/api/albums/" + id.String() + "/status"}, ok)
	if s.body["job"] != nil {
		t.Fatalf("status %s", s.raw)
	}

	e.wantError(req{method: "GET", path: "/api/albums/" + store.NewID().String()}, notFound404, catalog.CodeAlbumNotFound)
	e.wantError(req{method: "GET", path: "/api/albums/" + store.NewID().String() + "/status"}, notFound404, catalog.CodeAlbumNotFound)
	e.wantError(req{method: "GET", path: "/api/albums/{" + id.String() + "}"}, notFound404, catalog.CodeAlbumNotFound)
}

// §10.2 PUT /api/albums/{id}: metadata and tracks in one transaction; the
// list is exactly the current tracks; blobs and ids are not fields; 428,
// 412 and 404 in the right order (§10.1).
func TestUpdateAlbum(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Miles Davis", "Kind of Blue")
	path := "/api/albums/" + id.String()
	a, tag := e.album(id)
	body := putBody(a)

	// Saving it as it is changes nothing: same revision, no new render.
	e.exec(`DELETE FROM jobs WHERE kind = 'render'`)
	r := e.must(req{method: "PUT", path: path, body: body, ifMatch: tag}, ok)
	if r.body["revision"] != 1.0 || e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`) != 0 {
		t.Fatalf("a no-op save: %s", r.raw)
	}
	if r.header.Get("ETag") != "" {
		t.Fatal("a PUT answer carries an ETag field (RFC 9110 §9.3.4)")
	}

	// A change: title normalized, a track renumbered and its genre set to
	// inherit, both tracks swapped (§12.2 "scambio numeri traccia").
	body["title"] = " Kind of Blue (Mono) "
	ts := body["tracks"].([]any)
	ts[0].(map[string]any)["no"], ts[1].(map[string]any)["no"] = 2, 1
	ts[1].(map[string]any)["genre"] = nil
	r = e.must(req{method: "PUT", path: path, body: body, ifMatch: tag}, ok)
	if r.body["revision"] != 2.0 || r.body["title"] != "Kind of Blue (Mono)" || r.body["etag"] != ETag(KindAlbum, id, 2) {
		t.Fatalf("after the change: %s", r.raw)
	}
	got := tracksOf(r.body)
	if got[0]["title"] != "Freddie Freeloader" || got[0]["no"] != 1.0 || got[0]["genre"] != nil || got[1]["no"] != 2.0 {
		t.Fatalf("tracks after the swap: %v", got)
	}
	if e.count(`SELECT count(*) FROM jobs WHERE kind = 'render' AND album_id = $1`, id) != 1 {
		t.Fatal("no render enqueued with the change")
	}
	a, tag = e.album(id)
	if !reflect.DeepEqual(a, r.body) {
		t.Fatalf("the PUT answer is not the album:\n%v\n%v", r.body, a)
	}

	// Preconditions: none, stale, another resource's, weak; 404 before 412.
	body = putBody(a)
	e.wantError(req{method: "PUT", path: path, body: body}, required428, catalog.CodePreconditionRequired)
	e.wantError(req{method: "PUT", path: path, body: body, ifMatch: "*"}, required428, catalog.CodePreconditionRequired)
	r = e.wantError(req{method: "PUT", path: path, body: body, ifMatch: ETag(KindAlbum, id, 1)}, failed412, catalog.CodePreconditionFailed)
	if want := "album " + id.String() + " is at revision 2, not 1: reload it"; r.details()["revision"] != 2.0 || r.body["message"] != want {
		t.Fatalf("412 %s, want the message %q", r.raw, want)
	}
	other := e.seed("Bill Evans", "Portrait in Jazz")
	for _, m := range []string{ETag(KindAlbum, other, 2), ETag(KindArtist, id, 2), "W/" + tag, `"abc"`} {
		r = e.wantError(req{method: "PUT", path: path, body: body, ifMatch: m}, failed412, catalog.CodePreconditionFailed)
		// The whole message, so that the sentinel revision of N-147 can
		// never show (a substring check would also match the album's id).
		if want := "album " + id.String() + " has changed or does not match the ETag sent: reload it"; r.details()["revision"] != 2.0 ||
			r.body["message"] != want {
			t.Fatalf("If-Match %s: %s, want the message %q", m, r.raw, want)
		}
	}
	e.wantError(req{method: "PUT", path: path, body: body, ifMatch: tag + ", " + ETag(KindAlbum, id, 1)}, badRequest, CodeInvalidIfMatch)
	e.wantError(req{method: "PUT", path: path, body: body, ifMatch: "garbage"}, badRequest, CodeInvalidIfMatch)
	missing := store.NewID()
	e.wantError(req{method: "PUT", path: "/api/albums/" + missing.String(), body: body, ifMatch: ETag(KindAlbum, missing, 1)},
		notFound404, catalog.CodeAlbumNotFound)
	e.wantError(req{method: "PUT", path: "/api/albums/" + missing.String(), body: body, ifMatch: tag},
		notFound404, catalog.CodeAlbumNotFound)
	if e.revision(id) != 2 {
		t.Fatal("a refused change bumped the revision")
	}

	// Content: every refusal leaves the album as it was.
	trackID := tracksOf(a)[0]["id"].(string)
	for _, tc := range []struct {
		name   string
		mutate func(b map[string]any)
		status int
		code   string
	}{
		{"missing track", func(b map[string]any) { b["tracks"] = b["tracks"].([]any)[:1] }, unprocess, catalog.CodeTrackListMismatch},
		{"unknown track", func(b map[string]any) {
			b["tracks"].([]any)[1].(map[string]any)["id"] = store.NewID().String()
		}, unprocess, catalog.CodeTrackListMismatch},
		{"track listed twice", func(b map[string]any) {
			b["tracks"].([]any)[1].(map[string]any)["id"] = trackID
		}, unprocess, CodeDuplicateID},
		{"same number", func(b map[string]any) { b["tracks"].([]any)[1].(map[string]any)["no"] = 1 }, unprocess, catalog.CodeDuplicateTrackNumber},
		{"disc 100", func(b map[string]any) { b["tracks"].([]any)[0].(map[string]any)["disc"] = 100 }, unprocess, catalog.CodeInvalidDisc},
		{"year 0", func(b map[string]any) { b["year"] = 0 }, unprocess, catalog.CodeInvalidYear},
		{"empty title", func(b map[string]any) { b["title"] = "  " }, unprocess, names.CodeTextEmpty},
		{"empty track artist", func(b map[string]any) { b["tracks"].([]any)[0].(map[string]any)["artist"] = "" }, unprocess, names.CodeTextEmpty},
		{"unknown artist", func(b map[string]any) { b["artist_id"] = store.NewID().String() }, unprocess, catalog.CodeArtistNotFound},
		{"a blob is not a field", func(b map[string]any) {
			b["tracks"].([]any)[0].(map[string]any)["blob_hash"] = newHash()
		}, unprocess, CodeUnknownField},
		{"the id is not a field", func(b map[string]any) { b["id"] = id.String() }, unprocess, CodeUnknownField},
		{"year omitted", func(b map[string]any) { delete(b, "year") }, unprocess, CodeMissingField},
		{"genre omitted in a track", func(b map[string]any) { delete(b["tracks"].([]any)[0].(map[string]any), "genre") }, unprocess, CodeMissingField},
		{"compilation null", func(b map[string]any) { b["compilation"] = nil }, unprocess, CodeInvalidField},
		{"year as string", func(b map[string]any) { b["year"] = "1959" }, unprocess, CodeInvalidField},
		{"tracks null", func(b map[string]any) { b["tracks"] = nil }, unprocess, CodeInvalidField},
		{"non-canonical track id", func(b map[string]any) {
			b["tracks"].([]any)[0].(map[string]any)["id"] = strings.ToUpper(trackID)
		}, unprocess, CodeInvalidField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := putBody(a)
			tc.mutate(b)
			e.wantError(req{method: "PUT", path: path, body: b, ifMatch: tag}, tc.status, tc.code)
			if now, _ := e.album(id); !reflect.DeepEqual(now, a) {
				t.Fatal("a refused change changed the album")
			}
		})
	}
}

// §4.3: reassigning an album to another artist, with the folder check.
func TestAlbumReassignment(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Miles Davis", "Kind of Blue")
	blocker := e.seed("Bill Evans", "Kind of Blue")
	bill := e.artistOf(blocker)
	a, tag := e.album(id)
	body := putBody(a)
	body["artist_id"] = bill.String()
	r := e.wantError(req{method: "PUT", path: "/api/albums/" + id.String(), body: body, ifMatch: tag},
		conflict, catalog.CodeAlbumFolderConflict)
	if r.details()["album_id"] != blocker.String() {
		t.Fatalf("409 %s", r.raw)
	}
	body["title"] = "Kind of Blue (Bill's)"
	r = e.must(req{method: "PUT", path: "/api/albums/" + id.String(), body: body, ifMatch: tag}, ok)
	if r.body["artist_id"] != bill.String() || r.body["artist_name"] != "Bill Evans" || r.body["revision"] != 2.0 {
		t.Fatalf("reassigned: %s", r.raw)
	}
}

// §12.2 "Due finestre UI salvano revisioni diverse", at the API: two
// clients read the same revision and save different changes at once.
// Exactly one gets 412; the other's change is saved; the loser reloads,
// sees the winner's change, applies its own, and both are kept. Nothing
// is lost silently.
func TestTwoClientsSaveDifferentRevisions(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Miles Davis", "Kind of Blue")
	path := "/api/albums/" + id.String()
	for round := range 20 {
		a, tag := e.album(id)
		titleChange := putBody(a)
		titleChange["title"] = "Title " + string(rune('A'+round))
		trackChange := putBody(a)
		trackChange["tracks"].([]any)[0].(map[string]any)["title"] = "Track " + string(rune('A'+round))

		var wg sync.WaitGroup
		results := make([]resp, 2)
		for i, b := range []map[string]any{titleChange, trackChange} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = e.do(req{method: "PUT", path: path, body: b, ifMatch: tag})
			}()
		}
		wg.Wait()
		statuses := []int{results[0].status, results[1].status}
		if !(statuses[0] == ok && statuses[1] == failed412) && !(statuses[0] == failed412 && statuses[1] == ok) {
			t.Fatalf("round %d: statuses %v (%s | %s)", round, statuses, results[0].raw, results[1].raw)
		}
		loser := 0
		if statuses[0] == ok {
			loser = 1
		}
		if results[loser].code() != catalog.CodePreconditionFailed ||
			results[loser].details()["revision"] != results[1-loser].body["revision"] {
			t.Fatalf("round %d: the 412 %s does not name the winner's revision", round, results[loser].raw)
		}
		// The loser reloads and applies its change on the new revision.
		now, newTag := e.album(id)
		redo := putBody(now)
		if loser == 0 {
			redo["title"] = titleChange["title"]
		} else {
			redo["tracks"].([]any)[0].(map[string]any)["title"] = trackChange["tracks"].([]any)[0].(map[string]any)["title"]
		}
		e.must(req{method: "PUT", path: path, body: redo, ifMatch: newTag}, ok)
		final, _ := e.album(id)
		if final["title"] != titleChange["title"] || tracksOf(final)[0]["title"] != trackChange["tracks"].([]any)[0].(map[string]any)["title"] {
			t.Fatalf("round %d: a change was lost: %v", round, final)
		}
		if final["revision"] != float64(1+2*(round+1)) {
			t.Fatalf("round %d: revision %v", round, final["revision"])
		}
	}
}

// §4.3, §10.2: trash, restore and the forced render, each with If-Match.
func TestTrashRestoreRender(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Miles Davis", "Kind of Blue")
	path := "/api/albums/" + id.String()
	_, tag := e.album(id)

	e.wantError(req{method: "DELETE", path: path}, required428, catalog.CodePreconditionRequired)
	e.wantError(req{method: "DELETE", path: path, ifMatch: tag, body: `{}`}, badRequest, CodeBodyNotAllowed)
	r := e.must(req{method: "DELETE", path: path, ifMatch: tag}, ok)
	if r.body["trashed"] != true || r.body["revision"] != 2.0 {
		t.Fatalf("trashed: %s", r.raw)
	}
	// Trashing again with the new ETag is a no-op.
	r = e.must(req{method: "DELETE", path: path, ifMatch: r.body["etag"].(string)}, ok)
	if r.body["revision"] != 2.0 {
		t.Fatalf("trashed twice: %s", r.raw)
	}
	// Another active album takes the folder: the restore is a conflict.
	blocker := e.seed("Miles Davis", "Kind of Blue")
	e.wantError(req{method: "POST", path: path + "/restore", ifMatch: ETag(KindAlbum, id, 2)}, conflict, catalog.CodeAlbumFolderConflict)
	e.must(req{method: "DELETE", path: "/api/albums/" + blocker.String(), ifMatch: ETag(KindAlbum, blocker, 1)}, ok)
	e.wantError(req{method: "POST", path: path + "/restore", ifMatch: ETag(KindAlbum, id, 1)}, failed412, catalog.CodePreconditionFailed)
	r = e.must(req{method: "POST", path: path + "/restore", ifMatch: ETag(KindAlbum, id, 2)}, ok)
	if r.body["trashed"] != false || r.body["revision"] != 3.0 {
		t.Fatalf("restored: %s", r.raw)
	}

	// The forced render: the seen revision, no new one, a job.
	e.exec(`DELETE FROM jobs WHERE kind = 'render'`)
	e.wantError(req{method: "POST", path: path + "/render"}, required428, catalog.CodePreconditionRequired)
	e.wantError(req{method: "POST", path: path + "/render", ifMatch: ETag(KindAlbum, id, 2)}, failed412, catalog.CodePreconditionFailed)
	if e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`) != 0 {
		t.Fatal("a refused render enqueued a job")
	}
	r = e.must(req{method: "POST", path: path + "/render", ifMatch: ETag(KindAlbum, id, 3)}, accepted)
	if r.body["revision"] != 3.0 || r.body["job"].(map[string]any)["state"] != "pending" || e.revision(id) != 3 {
		t.Fatalf("render: %s", r.raw)
	}
	// Again: the same row, a new ticket (§6.3).
	var before, after int64
	if err := e.db.QueryRow(t.Context(), `SELECT requested FROM jobs WHERE album_id = $1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	e.must(req{method: "POST", path: path + "/render", ifMatch: ETag(KindAlbum, id, 3)}, accepted)
	if err := e.db.QueryRow(t.Context(), `SELECT requested FROM jobs WHERE album_id = $1`, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after <= before || e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`) != 1 {
		t.Fatalf("second render: ticket %d -> %d", before, after)
	}
	missing := store.NewID()
	e.wantError(req{method: "POST", path: "/api/albums/" + missing.String() + "/render", ifMatch: ETag(KindAlbum, missing, 1)},
		notFound404, catalog.CodeAlbumNotFound)
}

func keysOf(m map[string]any) []string {
	out := slices.Collect(maps.Keys(m))
	slices.Sort(out)
	return out
}

// topLevelKeys returns the keys of a JSON object in their order.
func topLevelKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}
