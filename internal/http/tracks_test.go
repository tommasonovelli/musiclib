package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	nethttp "net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// POST /api/albums/{id}/tracks: a file added to an existing album as a
// track. The contract tests read the pinned file with fakeTracks, which
// stands for the importer's ReadTrack without the pinned tools;
// TestPostTrackReal reads it with the real importer.

// fakeTracks reads a pinned blob as fakeTrack wrote it: "fLaC|title|
// artist|genre|disc|no|warning" on its first line. Anything else is
// refused as the importer refuses a file that is not audio.
type fakeTracks struct{ e *env }

func (f fakeTracks) ReadTrack(_ context.Context, b blobstore.Blob, name string) (importer.TrackInfo, []jobs.Warning, error) {
	r, err := f.e.blobs.Open(b.SHA256)
	if err != nil {
		return importer.TrackInfo{}, nil, err
	}
	data, err := io.ReadAll(r)
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return importer.TrackInfo{}, nil, err
	}
	line, _, _ := bytes.Cut(data, []byte("\n"))
	p := strings.Split(string(line), "|")
	if len(p) != 7 || p[0] != "fLaC" {
		code := importer.CodeUnsupportedAudio
		if media.HasKnownAudioExtension(name) {
			code = importer.CodeCorruptAudio
		}
		return importer.TrackInfo{}, nil, &importer.Error{Code: code, Path: name, Message: fmt.Sprintf("%q is not audio", name)}
	}
	disc, _ := strconv.Atoi(p[4])
	no, _ := strconv.Atoi(p[5])
	var ws []jobs.Warning
	if p[6] != "" {
		ws = append(ws, jobs.Warning{Code: jobs.WarnFLACID3, Path: name, Message: p[6]})
	}
	return importer.TrackInfo{Format: media.FormatFLAC, Duration: 1500 * time.Millisecond, Title: p[1], Artist: p[2], Genre: p[3],
		Disc: disc, No: no}, ws, nil
}

// fakeTrack is a file fakeTracks reads as a FLAC track, unique by its
// random tail.
func fakeTrack(title, artist, genre string, disc, no int, warning string) []byte {
	head := fmt.Sprintf("fLaC|%s|%s|%s|%d|%d|%s\n", title, artist, genre, disc, no, warning)
	return append([]byte(head), randomBytes(2000)...)
}

func trackTitled(t *testing.T, body map[string]any, title string) map[string]any {
	t.Helper()
	for _, tr := range tracksOf(body) {
		if tr["title"] == title {
			return tr
		}
	}
	t.Fatalf("no track %q in %v", title, body["tracks"])
	return nil
}

func TestPostTrack(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC) // tracks 1 and 2 on disc 1
	b := e.seedReal("Bill Evans", "Portrait in Jazz", catalog.FormatFLAC)
	base := albumPath(a.id) + "/tracks"
	post := func(name, tag string, body any) req {
		return upload("POST", base+"?name="+url.QueryEscape(name), tag, body)
	}
	_, tag := e.album(a.id)
	rev := e.revision(a.id)
	file := fakeTrack("Blue in Green", "Bill Evans", "", 0, 3, "")
	before := e.state(a.id)

	// Preconditions, media type and query, before anything is read.
	e.wantError(post("x.flac", "", file), nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired)
	e.wantError(post("x.flac", ETag(KindAlbum, a.id, rev+1), file), nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
	e.wantError(post("x.flac", ETag(KindAlbum, b.id, 1), file), nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
	e.wantError(upload("POST", albumPath(uuid.New())+"/tracks?name=x.flac", tag, file), nethttp.StatusNotFound, catalog.CodeAlbumNotFound)
	r := post("x.flac", tag, file)
	r.headers["Content-Type"] = []string{"audio/flac"}
	e.wantError(r, nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	e.wantError(upload("POST", base, tag, file), nethttp.StatusUnprocessableEntity, CodeMissingField)
	e.wantError(upload("POST", base+"?name=a&name=b", tag, file), nethttp.StatusUnprocessableEntity, CodeInvalidField)
	e.wantError(upload("POST", base+"?name=a&path=b", tag, file), nethttp.StatusUnprocessableEntity, CodeUnknownField)
	for name, code := range map[string]string{"": "path_empty", ".": "path_dot_segment", "a/..": "path_dot_segment", "/": "path_absolute",
		"a\x00b": "path_nul_byte", "\xff.flac": "invalid_utf8"} {
		e.wantError(post(name, tag, file), nethttp.StatusUnprocessableEntity, code)
	}
	// Answered without reading a body that is never sent: 428, 412, 404,
	// an invalid name, and a declared length over the limit (413).
	for _, c := range []struct {
		path, ifMatch string
		status        int
	}{
		{base + "?name=x.flac", "", nethttp.StatusPreconditionRequired},
		{base + "?name=x.flac", ETag(KindAlbum, a.id, rev+1), nethttp.StatusPreconditionFailed},
		{albumPath(uuid.New()) + "/tracks?name=x.flac", tag, nethttp.StatusNotFound},
		{base + "?name=.", tag, nethttp.StatusUnprocessableEntity},
	} {
		h := map[string]string{"Content-Type": UploadMediaType, "Content-Length": "1000000"}
		if c.ifMatch != "" {
			h["If-Match"] = c.ifMatch
		}
		if res := e.rawRequest("POST", c.path, h); res.StatusCode != c.status {
			t.Fatalf("%s without a body: %d, want %d", c.path, res.StatusCode, c.status)
		}
	}
	e.declared("POST", base+"?name=big.flac", tag, MaxTrackBytes+1, MaxTrackBytes)
	e.sameState(a.id, before)
	if e.budget.Reserved() != 0 {
		t.Fatalf("%d bytes still reserved", e.budget.Reserved())
	}

	// Not audio: 422 with the importer's code; the album is unchanged.
	pinned, _ := e.blobFiles()
	e.wantError(post("notes.txt", tag, []byte("hello")), nethttp.StatusUnprocessableEntity, importer.CodeUnsupportedAudio)
	e.wantError(post("broken.flac", tag, []byte("hello")), nethttp.StatusUnprocessableEntity, importer.CodeCorruptAudio)
	if e.revision(a.id) != rev || e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`) != 0 {
		t.Fatal("a refused file changed the album")
	}
	// The same audio as a track of the album: 409 naming it, before it is
	// read.
	dup := e.wantError(post("again.flac", tag, a.audio[0]), nethttp.StatusConflict, catalog.CodeTrackExists)
	if dup.details()["track_id"] != a.tracks[0].String() {
		t.Fatalf("details %v", dup.details())
	}
	if p, temps := e.blobFiles(); p != pinned+1 || temps != 0 {
		t.Fatalf("%d blobs (%d before), %d temporaries", p, pinned, temps)
	}

	// Added: 201, Location, the tags' place, the name reduced to its last
	// segment, the format and duration read.
	created := e.must(post("CD/03 Blue in Green.flac", tag, file), nethttp.StatusCreated)
	body := e.changed(created, a.id, rev)
	rev++
	tr := trackTitled(t, body, "Blue in Green")
	blob := tr["blob"].(map[string]any)
	if tr["disc"] != float64(1) || tr["no"] != float64(3) || tr["artist"] != "Bill Evans" || tr["genre"] != nil ||
		tr["source_path"] != "03 Blue in Green.flac" || tr["duration_ms"] != float64(1500) ||
		blob["hash"] != sha(file) || blob["format"] != "flac" {
		t.Fatalf("track %v", tr)
	}
	if loc := created.header.Get("Location"); loc != base+"/"+tr["id"].(string)+"/original" {
		t.Fatalf("Location %q", loc)
	}
	if ws, ok := body["warnings"].([]any); !ok || len(ws) != 0 || body["etag"] != ETag(KindAlbum, a.id, rev) {
		t.Fatalf("warnings %v, etag %v", body["warnings"], body["etag"])
	}
	if !bytes.Equal(e.pinnedBytes(sha(file)), file) {
		t.Fatal("the pinned track differs")
	}
	// A taken place is appended; the warnings of the reading are reported.
	_, tag = e.album(a.id)
	body = e.changed(e.must(post("x.flac", tag, fakeTrack("All Blues", "", "", 1, 1, "an ID3 tag")), nethttp.StatusCreated), a.id, rev)
	rev++
	if tr = trackTitled(t, body, "All Blues"); tr["no"] != float64(4) || tr["artist"] != nil {
		t.Fatalf("track %v", tr)
	}
	if ws := body["warnings"].([]any); len(ws) != 1 || ws[0].(map[string]any)["code"] != string(jobs.WarnFLACID3) ||
		ws[0].(map[string]any)["path"] != "x.flac" {
		t.Fatalf("warnings %v", ws)
	}
	// The ETag of the answer is the next change's If-Match.
	e.changed(e.must(post("y.flac", body["etag"].(string), fakeTrack("Flamenco Sketches", "", "", 0, 0, "")), nethttp.StatusCreated), a.id, rev)

	// Wrong method: 405 with Allow.
	if res := e.wantError(req{method: "GET", path: base}, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed); res.header.Get("Allow") != "POST" {
		t.Fatalf("Allow %q", res.header.Get("Allow"))
	}
}

// The free-space budget: a track upload reserves its size; when the budget cannot hold it,
// 507 and nothing is written.
func TestPostTrackInsufficientSpace(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	_, tag := e.album(a.id)
	before := e.state(a.id)
	hold, _ := e.budget.Reserve(1<<62, 1<<61)
	if hold == nil {
		t.Fatal("cannot hold the budget")
	}
	file := fakeTrack("Blue in Green", "", "", 0, 0, "")
	e.wantError(upload("POST", albumPath(a.id)+"/tracks?name=x.flac", tag, file), nethttp.StatusInsufficientStorage, CodeInsufficientSpace)
	e.sameState(a.id, before)
	hold.Release()
	rev := e.revision(a.id)
	e.changed(e.must(upload("POST", albumPath(a.id)+"/tracks?name=x.flac", tag, file), nethttp.StatusCreated), a.id, rev)
	if e.budget.Reserved() != 0 {
		t.Fatalf("%d bytes still reserved", e.budget.Reserved())
	}
}

// The real importer reads the upload (the pinned tools: the Docker gate):
// a FLAC encoded by FFmpeg is added with its duration; a text file is not
// a track.
func TestPostTrackReal(t *testing.T) {
	e := newEnv(t)
	e.tracks = e.testImporter(t)
	e.api.Enable(e.backend())
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	_, tag := e.album(a.id)
	rev := e.revision(a.id)
	p := filepath.Join(t.TempDir(), "05 Flamenco Sketches.flac")
	flac(t, p, "Kind of Blue")
	file, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	base := albumPath(a.id) + "/tracks?name="
	e.wantError(upload("POST", base+"notes.txt", tag, []byte("hello")), nethttp.StatusUnprocessableEntity, importer.CodeUnsupportedAudio)
	e.wantError(upload("POST", base+"x.flac", tag, file[:len(file)/2]), nethttp.StatusUnprocessableEntity, importer.CodeCorruptAudio)
	body := e.changed(e.must(upload("POST", base+url.QueryEscape("05 Flamenco Sketches.flac"), tag, file), nethttp.StatusCreated), a.id, rev)
	tr := trackTitled(t, body, "05 Flamenco Sketches")
	if tr["no"] != float64(3) || tr["artist"] != "Browser artist" || tr["blob"].(map[string]any)["format"] != "flac" || tr["duration_ms"] == nil {
		t.Fatalf("track %v", tr)
	}
}
