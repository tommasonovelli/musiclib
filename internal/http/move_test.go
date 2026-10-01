package http

import (
	nethttp "net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
)

// POST /api/albums/{id}/move-tracks: the strict body, the source's
// If-Match, both albums in the answer; a refusal changes neither album.
func TestMoveTracksAPI(t *testing.T) {
	e := pageEnv(t)
	src := e.seed("Mile Davis", "Kind of Blue")
	dst := e.seed("Miles Davis", "Kind of Blue")
	gone := e.seed("Chet Baker", "Chet")
	e.must(req{method: "DELETE", path: albumPath(gone), ifMatch: ETag(KindAlbum, gone, 1)}, nethttp.StatusOK)
	e.exec(`DELETE FROM jobs WHERE kind = 'render'`)
	path := albumPath(src) + "/move-tracks"
	a, tag := e.album(src)
	ts := tracksOf(a)
	first, second := ts[0]["id"].(string), ts[1]["id"].(string)
	d, _ := e.album(dst)
	move := func(tag string, body any) req {
		return req{method: "POST", path: path, ifMatch: tag, body: body}
	}
	ok := map[string]any{"tracks": []string{second}, "to": dst.String()}
	srcBefore, _ := e.album(src)
	unchanged := func() {
		t.Helper()
		for id, before := range map[uuid.UUID]map[string]any{src: srcBefore, dst: d} {
			now, _ := e.album(id)
			if now["revision"] != before["revision"] || len(tracksOf(now)) != len(tracksOf(before)) || now["trashed"] != before["trashed"] {
				t.Fatalf("album %s changed: %v", id, now)
			}
		}
		if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`); n != 0 {
			t.Fatalf("%d renders enqueued", n)
		}
	}

	// The source's precondition.
	e.wantError(move("", ok), nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired)
	e.wantError(move(ETag(KindAlbum, src, 2), ok), nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
	e.wantError(move(ETag(KindAlbum, dst, 1), ok), nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
	e.wantError(req{method: "POST", path: albumPath(uuid.New()) + "/move-tracks", ifMatch: tag, body: ok},
		nethttp.StatusNotFound, catalog.CodeAlbumNotFound)
	// The strict body.
	to := `"to":"` + dst.String() + `"`
	for _, c := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"tracks":["` + second + `"],` + to + `,"x":1}`, nethttp.StatusUnprocessableEntity, CodeUnknownField},
		{`{"tracks":["` + second + `"],` + to + `,` + to + `}`, nethttp.StatusBadRequest, CodeDuplicateKey},
		{`{"tracks":["` + second + `"]}`, nethttp.StatusUnprocessableEntity, CodeMissingField},
		{`{` + to + `}`, nethttp.StatusUnprocessableEntity, CodeMissingField},
		{`{"tracks":[],` + to + `}`, nethttp.StatusUnprocessableEntity, CodeInvalidField},
		{`{"tracks":null,` + to + `}`, nethttp.StatusUnprocessableEntity, CodeInvalidField},
		{`{"tracks":"` + second + `",` + to + `}`, nethttp.StatusUnprocessableEntity, CodeInvalidField},
		{`{"tracks":["` + strings.ToUpper(second) + `"],` + to + `}`, nethttp.StatusUnprocessableEntity, CodeInvalidField},
		{`{"tracks":[1],` + to + `}`, nethttp.StatusUnprocessableEntity, CodeInvalidField},
		{`{"tracks":["` + second + `","` + second + `"],` + to + `}`, nethttp.StatusUnprocessableEntity, CodeDuplicateID},
		{`{"tracks":["` + second + `"],"to":null}`, nethttp.StatusUnprocessableEntity, CodeInvalidField},
		{`[]`, nethttp.StatusUnprocessableEntity, CodeInvalidField},
		{`{"tracks":["` + second + `"],` + to, nethttp.StatusBadRequest, CodeInvalidJSON},
	} {
		e.wantError(move(tag, c.body), c.status, c.code)
	}
	r := move(tag, ok)
	r.headers = map[string][]string{"Content-Type": {"text/plain"}}
	e.wantError(r, nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	// The catalog's refusals.
	e.wantError(move(tag, map[string]any{"tracks": []string{second}, "to": src.String()}), nethttp.StatusUnprocessableEntity, catalog.CodeSameAlbum)
	missing := uuid.New()
	nf := e.wantError(move(tag, map[string]any{"tracks": []string{second}, "to": missing.String()}), nethttp.StatusNotFound, catalog.CodeAlbumNotFound)
	if nf.details()["album_id"] != missing.String() {
		t.Fatalf("details %s", nf.raw)
	}
	tr := e.wantError(move(tag, map[string]any{"tracks": []string{second}, "to": gone.String()}), nethttp.StatusConflict, catalog.CodeAlbumTrashed)
	if tr.details()["album_id"] != gone.String() {
		t.Fatalf("details %s", tr.raw)
	}
	e.wantError(move(tag, map[string]any{"tracks": []string{tracksOf(d)[0]["id"].(string)}, "to": dst.String()}),
		nethttp.StatusNotFound, catalog.CodeTrackNotFound)
	unchanged()

	// One track: 200 with both albums at their new revisions; it is
	// appended to the destination, with its lyrics.
	res := e.must(move(tag, ok), nethttp.StatusOK)
	from, _ := res.body["from"].(map[string]any)
	dest, _ := res.body["to"].(map[string]any)
	if len(res.body) != 2 || from == nil || dest == nil {
		t.Fatalf("answer %s", res.raw)
	}
	if from["etag"] != ETag(KindAlbum, src, 2) || from["trashed"] != false || len(tracksOf(from)) != 1 {
		t.Fatalf("from %v", from)
	}
	if dest["etag"] != ETag(KindAlbum, dst, 2) || len(tracksOf(dest)) != 3 {
		t.Fatalf("to %v", dest)
	}
	moved := tracksOf(dest)[2]
	if moved["id"] != second || moved["no"] != float64(3) || moved["artist"] != "Miles & Cannonball" {
		t.Fatalf("moved track %v", moved)
	}
	if e.count(`SELECT count(*) FROM jobs WHERE kind = 'render' AND state = 'pending' AND album_id IN ($1, $2)`, src, dst) != 2 {
		t.Fatal("both renders")
	}
	if res.header.Get("ETag") != "" {
		t.Fatal("a change answered with an ETag")
	}

	// The last one: the source goes to the trash, with no tracks; it
	// cannot be restored; its page and the Trash still show it.
	res = e.must(move(from["etag"].(string), map[string]any{"tracks": []string{first}, "to": dst.String()}), nethttp.StatusOK)
	from = res.body["from"].(map[string]any)
	if from["trashed"] != true || from["tracks"] == nil || len(tracksOf(from)) != 0 || len(tracksOf(res.body["to"].(map[string]any))) != 4 {
		t.Fatalf("answer %s", res.raw)
	}
	e.wantError(req{method: "POST", path: albumPath(src) + "/restore", ifMatch: from["etag"].(string)},
		nethttp.StatusUnprocessableEntity, catalog.CodeNoTracks)
	status, _, page := pageRequest(t, e, "/albums/"+src.String(), testHost)
	if status != nethttp.StatusOK || !strings.Contains(page, "This album is in the trash.") || !strings.Contains(page, ">0 tracks<") {
		t.Fatalf("album page %d", status)
	}
	if status, _, page = pageRequest(t, e, "/?trash=true", testHost); status != nethttp.StatusOK || !strings.Contains(page, "Mile Davis") {
		t.Fatalf("trash page %d", status)
	}
}
