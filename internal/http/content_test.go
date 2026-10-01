package http

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	nethttp "net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/failpoint"
)

// The editor's content endpoints (§10.2, round 14) through a real server,
// over the real catalog on PostgreSQL 17 and the real blob store on ext4.

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func albumPath(id uuid.UUID) string { return "/api/albums/" + id.String() }

// state is what a refused request must leave as it was: the album's
// representation, the blobs pinned, and no temporary.
type snapshotState struct {
	body   string
	pinned int
}

func (e *env) state(id uuid.UUID) snapshotState {
	e.t.Helper()
	r := e.must(req{method: "GET", path: albumPath(id)}, nethttp.StatusOK)
	pinned, temps := e.blobFiles()
	if temps != 0 {
		e.t.Fatalf("%d temporaries left in work/blobs", temps)
	}
	return snapshotState{body: string(r.raw), pinned: pinned}
}

func (e *env) sameState(id uuid.UUID, before snapshotState) {
	e.t.Helper()
	if after := e.state(id); after != before {
		e.t.Fatalf("the refused request changed something:\nbefore %+v\nafter  %+v", before, after)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`); n != 0 {
		e.t.Fatalf("%d renders enqueued", n)
	}
}

// changed checks a 200 or 201 answer of a change: the album at the next
// revision, a render enqueued, then clears the render.
func (e *env) changed(r resp, id uuid.UUID, rev int64) map[string]any {
	e.t.Helper()
	if r.body["revision"] != float64(rev+1) || e.revision(id) != rev+1 {
		e.t.Fatalf("revision %v, want %d: %s", r.body["revision"], rev+1, r.raw)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'render' AND album_id = $1 AND state = 'pending'`, id); n != 1 {
		e.t.Fatalf("%d pending renders", n)
	}
	if r.header.Get("ETag") != "" {
		e.t.Fatal("a change answered with an ETag (N-147)")
	}
	e.exec(`DELETE FROM jobs WHERE kind = 'render'`)
	return r.body
}

// noop checks a 200 answer of a change that changed nothing.
func (e *env) noop(r resp, id uuid.UUID, rev int64) {
	e.t.Helper()
	if r.body["revision"] != float64(rev) || e.revision(id) != rev ||
		e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`) != 0 {
		e.t.Fatalf("a no-op bumped or enqueued: %s", r.raw)
	}
}

func coverOf(body map[string]any) map[string]any {
	c, _ := body["cover"].(map[string]any)
	return c
}

func TestPutCoverUpload(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	path := albumPath(a.id) + "/cover"
	_, tag := e.album(a.id)
	rev := e.revision(a.id)
	img := pngImage(t, 32, 20)
	before := e.state(a.id)

	// Preconditions and the request's form, before anything is read.
	e.wantError(upload("PUT", path, "", img), nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired)
	e.wantError(upload("PUT", path, "*", img), nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired)
	e.wantError(upload("PUT", path, `"garbage`, img), nethttp.StatusBadRequest, CodeInvalidIfMatch)
	stale := ETag(KindAlbum, a.id, rev+1)
	if r := e.wantError(upload("PUT", path, stale, img), nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed); r.details()["revision"] != float64(rev) {
		t.Fatalf("412 details %v", r.details())
	}
	e.wantError(upload("PUT", albumPath(uuid.New())+"/cover", ETag(KindAlbum, a.id, rev), img), nethttp.StatusNotFound, catalog.CodeAlbumNotFound)
	for _, ct := range []string{"image/png", "image/jpeg", "application/octet-stream; x=1", "multipart/form-data; boundary=x", "text/plain", ""} {
		r := upload("PUT", path, tag, img)
		r.headers["Content-Type"] = []string{ct}
		e.wantError(r, nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	}
	// §8.5: not an image, a truncated JPEG, a header of 40,005,000 pixels, a GIF.
	jpg := jpegImage(t, 64, 64)
	for name, bad := range map[string][]byte{
		"text":           []byte("hello"),
		"truncated jpeg": jpg[:len(jpg)/2],
		"over 40 Mpx":    pngHeader(8000, 5001),
		"header only":    pngHeader(10, 10),
		"gif":            []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"empty":          {},
	} {
		r := e.wantError(upload("PUT", path, tag, bad), nethttp.StatusUnprocessableEntity, catalog.CodeInvalidCover)
		t.Logf("%s: %s", name, r.body["message"])
	}
	e.sameState(a.id, before)

	// The upload: pinned byte for byte, the album's cover, bumped.
	r := e.must(upload("PUT", path, tag, img), nethttp.StatusOK)
	body := e.changed(r, a.id, rev)
	if c := coverOf(body); c["hash"] != sha(img) || c["format"] != "png" || c["size"] != float64(len(img)) {
		t.Fatalf("cover %v", c)
	}
	if !bytes.Equal(e.pinnedBytes(sha(img)), img) {
		t.Fatal("the pinned cover differs from the upload")
	}
	// An uploaded cover is not an attachment (N-174).
	if n := len(body["attachments"].([]any)); n != 3 {
		t.Fatalf("%d attachments", n)
	}
	rev++
	// The same bytes again: a no-op.
	_, tag = e.album(a.id)
	e.noop(e.must(upload("PUT", path, tag, img), nethttp.StatusOK), a.id, rev)

	// A JPEG replaces it.
	r = e.must(upload("PUT", path, tag, jpg), nethttp.StatusOK)
	if c := coverOf(e.changed(r, a.id, rev)); c["hash"] != sha(jpg) || c["format"] != "jpeg" {
		t.Fatalf("cover %v", c)
	}
}

// §8.5's 20 MiB, and N-091 per audio format: a PNG of exactly 20 MiB is
// valid, but a FLAC holds a cover of at most 16,777,174 bytes; an MP3 or
// an M4A holds it. One byte more is 413 whatever the album.
func TestPutCoverLimits(t *testing.T) {
	e := newEnv(t)
	flac := e.seedReal("A", "FLAC album", catalog.FormatFLAC)
	mp3 := e.seedReal("A", "MP3 album", catalog.FormatMP3)
	m4a := e.seedReal("A", "M4A album", catalog.FormatM4AAAC)
	exact := pngOfSize(t, MaxCoverBytes)
	over := pngOfSize(t, MaxCoverBytes+1)
	flacMax := pngOfSize(t, 16_777_174)

	_, tag := e.album(flac.id)
	before := e.state(flac.id)
	e.declared("PUT", albumPath(flac.id)+"/cover", tag, len(over), MaxCoverBytes)
	// Without a Content-Length: 413 as soon as the limit is passed.
	r := e.wantError(upload("PUT", albumPath(flac.id)+"/cover", tag, stream(over)),
		nethttp.StatusRequestEntityTooLarge, CodeBodyTooLarge)
	if r.details()["limit"] != float64(MaxCoverBytes) {
		t.Fatalf("details %v", r.details())
	}
	r = e.wantError(upload("PUT", albumPath(flac.id)+"/cover", tag, exact), nethttp.StatusUnprocessableEntity, catalog.CodeCoverNotEmbeddable)
	if n, _ := r.details()["names"].([]any); len(n) != 1 || n[0] != "flac" {
		t.Fatalf("details %v", r.details())
	}
	e.wantError(upload("PUT", albumPath(flac.id)+"/cover", tag, pngOfSize(t, 16_777_175)), nethttp.StatusUnprocessableEntity, catalog.CodeCoverNotEmbeddable)
	// Refused on the snapshot before the put (N-173): nothing pinned,
	// nothing changed.
	e.sameState(flac.id, before)
	e.must(upload("PUT", albumPath(flac.id)+"/cover", tag, flacMax), nethttp.StatusOK)

	for _, a := range []realAlbum{mp3, m4a} {
		_, tag := e.album(a.id)
		e.declared("PUT", albumPath(a.id)+"/cover", tag, len(over), MaxCoverBytes)
		e.wantError(upload("PUT", albumPath(a.id)+"/cover", tag, stream(over)), nethttp.StatusRequestEntityTooLarge, CodeBodyTooLarge)
		r := e.must(upload("PUT", albumPath(a.id)+"/cover", tag, exact), nethttp.StatusOK)
		if c := coverOf(r.body); c["size"] != float64(MaxCoverBytes) {
			t.Fatalf("cover %v", c)
		}
	}
}

// PUT /cover with {"attachment_id"}: an image attachment of the album
// becomes the cover and stays an attachment (§7.4); anything else is
// refused, and the attachment stays.
func TestPutCoverFromAttachment(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatMP3)
	b := e.seedReal("Bill Evans", "Portrait in Jazz", catalog.FormatMP3)
	path := albumPath(a.id) + "/cover"
	choose := func(tag string, att any) req {
		return req{method: "PUT", path: path, ifMatch: tag, body: map[string]any{"attachment_id": att}}
	}
	// An image uploaded as an attachment has no known format (N-118).
	img := pngImage(t, 40, 30)
	_, tag := e.album(a.id)
	r := e.must(upload("POST", albumPath(a.id)+"/attachments?path=Scans%2Ffront.png", tag, img), nethttp.StatusCreated)
	att := attachmentIDOf(t, r.body, "Scans/front.png")
	e.exec(`DELETE FROM jobs WHERE kind = 'render'`)
	_, tag = e.album(a.id)
	rev := e.revision(a.id)
	before := e.state(a.id)

	e.wantError(choose("", att), nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired)
	e.wantError(choose(ETag(KindAlbum, a.id, rev+1), att), nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
	// Another album's attachment, an unknown one, a malformed id.
	e.wantError(choose(tag, b.attachments["cover.jpg"].String()), nethttp.StatusNotFound, catalog.CodeAttachmentNotFound)
	e.wantError(choose(tag, uuid.NewString()), nethttp.StatusNotFound, catalog.CodeAttachmentNotFound)
	e.wantError(choose(tag, "{"+att+"}"), nethttp.StatusUnprocessableEntity, CodeInvalidField)
	e.wantError(choose(tag, nil), nethttp.StatusUnprocessableEntity, CodeInvalidField)
	e.wantError(req{method: "PUT", path: path, ifMatch: tag, body: map[string]any{"attachment_id": att, "x": 1}},
		nethttp.StatusUnprocessableEntity, CodeUnknownField)
	e.wantError(req{method: "PUT", path: path, ifMatch: tag, body: `{"attachment_id":"` + att + `","attachment_id":"` + att + `"}`},
		nethttp.StatusBadRequest, CodeDuplicateKey)
	// A JSON Content-Type with image bytes is JSON.
	e.wantError(req{method: "PUT", path: path, ifMatch: tag, body: img}, nethttp.StatusBadRequest, CodeInvalidUTF8)
	// Attachments that are not images: a PDF, HTML.
	for _, rel := range []string{"Scans/Booklet.pdf", "notes.html"} {
		r := e.wantError(choose(tag, a.attachments[rel].String()), nethttp.StatusUnprocessableEntity, catalog.CodeInvalidCover)
		if r.details()["attachment_id"] != a.attachments[rel].String() {
			t.Fatalf("details %v", r.details())
		}
	}
	e.sameState(a.id, before)

	r = e.must(choose(tag, att), nethttp.StatusOK)
	body := e.changed(r, a.id, rev)
	if c := coverOf(body); c["hash"] != sha(img) || c["format"] != "png" {
		t.Fatalf("cover %v", c)
	}
	if attachmentIDOf(t, body, "Scans/front.png") != att {
		t.Fatal("the chosen attachment is no longer an attachment (§7.4)")
	}
	// The attachment's blob is now a validated PNG: its download is inline.
	res := e.download("GET", albumPath(a.id)+"/attachments/"+att+"/content")
	if res.header.Get("Content-Type") != "image/png" || !bytes.HasPrefix([]byte(res.header.Get("Content-Disposition")), []byte("inline;")) {
		t.Fatalf("headers %v", res.header)
	}
	// Choosing it again: a no-op.
	rev++
	_, tag = e.album(a.id)
	e.noop(e.must(choose(tag, att), nethttp.StatusOK), a.id, rev)
}

func attachmentIDOf(t *testing.T, body map[string]any, relPath string) string {
	t.Helper()
	for _, raw := range body["attachments"].([]any) {
		at := raw.(map[string]any)
		if at["rel_path"] == relPath {
			return at["id"].(string)
		}
	}
	t.Fatalf("no attachment %q in %v", relPath, body["attachments"])
	return ""
}

func TestDeleteCover(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	path := albumPath(a.id) + "/cover"
	_, tag := e.album(a.id)
	rev := e.revision(a.id)
	e.wantError(req{method: "DELETE", path: path}, nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired)
	e.wantError(req{method: "DELETE", path: path, ifMatch: ETag(KindAlbum, a.id, rev+1)}, nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
	e.wantError(req{method: "DELETE", path: path, ifMatch: tag, body: "x"}, nethttp.StatusBadRequest, CodeBodyNotAllowed)
	e.wantError(req{method: "DELETE", path: albumPath(uuid.New()) + "/cover", ifMatch: tag}, nethttp.StatusNotFound, catalog.CodeAlbumNotFound)
	body := e.changed(e.must(req{method: "DELETE", path: path, ifMatch: tag}, nethttp.StatusOK), a.id, rev)
	if body["cover"] != nil {
		t.Fatalf("cover %v", body["cover"])
	}
	// The attachment holding the same image stays (§7.4), and the blob.
	if attachmentIDOf(t, body, "cover.jpg") == "" || !bytes.Equal(e.pinnedBytes(sha(a.cover)), a.cover) {
		t.Fatal("after the removal")
	}
	rev++
	_, tag = e.album(a.id)
	e.noop(e.must(req{method: "DELETE", path: path, ifMatch: tag}, nethttp.StatusOK), a.id, rev)
	e.wantError(req{method: "GET", path: path}, nethttp.StatusNotFound, CodeCoverNotFound)
}

func TestPostAttachment(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	b := e.seedReal("Bill Evans", "Portrait in Jazz", catalog.FormatFLAC)
	base := albumPath(a.id) + "/attachments"
	post := func(relPath, tag string, body any) req {
		return upload("POST", base+"?path="+url.QueryEscape(relPath), tag, body)
	}
	_, tag := e.album(a.id)
	rev := e.revision(a.id)
	file := randomBytes(5000)
	before := e.state(a.id)

	e.wantError(post("x.bin", "", file), nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired)
	e.wantError(post("x.bin", ETag(KindAlbum, a.id, rev+1), file), nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
	e.wantError(post("x.bin", ETag(KindAlbum, b.id, 1), file), nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
	e.wantError(upload("POST", albumPath(uuid.New())+"/attachments?path=x", tag, file), nethttp.StatusNotFound, catalog.CodeAlbumNotFound)
	r := post("x.bin", tag, file)
	r.headers["Content-Type"] = []string{"application/json"}
	e.wantError(r, nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	// The query: exactly one path, nothing else.
	e.wantError(upload("POST", base, tag, file), nethttp.StatusUnprocessableEntity, CodeMissingField)
	e.wantError(upload("POST", base+"?path=a&path=b", tag, file), nethttp.StatusUnprocessableEntity, CodeInvalidField)
	e.wantError(upload("POST", base+"?path=a&name=b", tag, file), nethttp.StatusUnprocessableEntity, CodeUnknownField)
	e.wantError(upload("POST", base+"?path=a;b", tag, file), nethttp.StatusUnprocessableEntity, CodeInvalidField)
	e.wantError(upload("POST", base+"?path=%zz", tag, file), nethttp.StatusUnprocessableEntity, CodeInvalidField)
	// §5.2: refused before any byte is read or stored.
	for p, code := range map[string]string{
		"":            "path_empty",
		"/etc/passwd": "path_absolute",
		"../../x":     "path_dot_segment",
		"Scans/../x":  "path_dot_segment",
		".":           "path_dot_segment",
		"a//b":        "path_empty_segment",
		"a/":          "path_empty_segment",
		"a\x00b":      "path_nul_byte",
		"\xff.bin":    "invalid_utf8",
		deepPath(17):  "path_too_deep",
	} {
		e.wantError(post(p, tag, file), nethttp.StatusUnprocessableEntity, code)
		// The same with a declared body that is never sent: answered
		// without reading it.
		res := e.rawRequest("POST", base+"?path="+url.QueryEscape(p), map[string]string{
			"If-Match": tag, "Content-Type": UploadMediaType, "Content-Length": "1000000"})
		if res.StatusCode != nethttp.StatusUnprocessableEntity {
			t.Fatalf("%q without a body: %d", p, res.StatusCode)
		}
	}
	// Collisions after normalization with the album's attachments
	// (Scans/Booklet.pdf, cover.jpg, notes.html): 409 naming both.
	for p, other := range map[string]string{
		"Scans/Booklet.pdf":  "Scans/Booklet.pdf",
		"scans/BOOKLET.pdf":  "Scans/Booklet.pdf",
		"Scans":              "Scans/Booklet.pdf",
		"cover.jpg/back.jpg": "cover.jpg",
		"SCANS/other.pdf":    "Scans/Booklet.pdf",
		"NOTES.HTML":         "notes.html",
		"Scans/Booklet.pdf.": "Scans/Booklet.pdf",
		"Scans/Booklet.pdf ": "Scans/Booklet.pdf",
	} {
		r := e.wantError(post(p, tag, file), nethttp.StatusConflict, catalog.CodeAttachmentCollision)
		if n, _ := r.details()["names"].([]any); len(n) != 2 || (n[0] != other && n[1] != other) {
			t.Fatalf("%q: details %v", p, r.details())
		}
	}
	e.sameState(a.id, before)

	// Accepted: 201, Location, the path kept as sent, the bytes pinned.
	rel := "Scans/Liner Notes – ☃ \"1959\".txt"
	created := e.must(post(rel, tag, file), nethttp.StatusCreated)
	body := e.changed(created, a.id, rev)
	id := attachmentIDOf(t, body, rel)
	if loc := created.header.Get("Location"); loc != base+"/"+id+"/content" {
		t.Fatalf("Location %q", loc)
	}
	if !bytes.Equal(e.pinnedBytes(sha(file)), file) {
		t.Fatal("the pinned attachment differs")
	}
	rev++
	// A case variant of it is the same file. An NFD spelling of an NFC
	// name is the same file too, and an NFD directory is the NFC one
	// (§5.2: NFC first), so another file in it is fine.
	_, tag = e.album(a.id)
	e.wantError(post("Scans/Liner Notes – ☃ \"1959\".TXT", tag, file), nethttp.StatusConflict, catalog.CodeAttachmentCollision)
	nfc := e.must(post("Caf\u00e9/x.txt", tag, []byte("a")), nethttp.StatusCreated)
	e.changed(nfc, a.id, rev)
	rev++
	_, tag = e.album(a.id)
	e.wantError(post("Cafe\u0301/x.txt", tag, []byte("b")), nethttp.StatusConflict, catalog.CodeAttachmentCollision)
	e.changed(e.must(post("Cafe\u0301/y.txt", tag, []byte("b")), nethttp.StatusCreated), a.id, rev)
	rev++
	_, tag = e.album(a.id)
	// An empty file is a file.
	e.changed(e.must(post("empty", tag, []byte{}), nethttp.StatusCreated), a.id, rev)
}

func deepPath(levels int) string {
	p := "d"
	for i := 1; i < levels; i++ {
		p += "/d"
	}
	return p
}

// §10.2: attachments of at most 256 MiB. Exactly the limit is stored; one
// byte more is 413, both announced by Content-Length (refused before the
// body is read) and streamed without one (refused as soon as the limit is
// passed, nothing pinned, the temporary removed). The body is generated
// as it is sent and streamed into the put: never held in memory.
func TestPostAttachmentLimit(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	base := albumPath(a.id) + "/attachments?path="
	_, tag := e.album(a.id)
	before := e.state(a.id)

	e.declared("POST", base+"big.bin", tag, MaxAttachmentBytes+1, MaxAttachmentBytes)
	r := e.wantError(upload("POST", base+"big.bin", tag, &patterned{n: MaxAttachmentBytes + 1}),
		nethttp.StatusRequestEntityTooLarge, CodeBodyTooLarge)
	if r.details()["limit"] != float64(MaxAttachmentBytes) {
		t.Fatalf("details %v", r.details())
	}
	e.sameState(a.id, before)
	if e.budget.Reserved() != 0 {
		t.Fatalf("%d bytes still reserved", e.budget.Reserved())
	}

	rev := e.revision(a.id)
	r = e.must(upload("POST", base+"big.bin", tag, &patterned{n: MaxAttachmentBytes}), nethttp.StatusCreated)
	e.changed(r, a.id, rev)
	h := sha256.New()
	p := &patterned{n: MaxAttachmentBytes}
	buf := make([]byte, 1<<20)
	for {
		n, err := p.Read(buf)
		h.Write(buf[:n])
		if err != nil {
			break
		}
	}
	want := hex.EncodeToString(h.Sum(nil))
	for _, raw := range r.body["attachments"].([]any) {
		at := raw.(map[string]any)
		if at["rel_path"] == "big.bin" {
			blob := at["blob"].(map[string]any)
			if blob["hash"] != want || blob["size"] != float64(MaxAttachmentBytes) {
				t.Fatalf("blob %v, want %s", blob, want)
			}
		}
	}
	if e.budget.Reserved() != 0 {
		t.Fatalf("%d bytes still reserved", e.budget.Reserved())
	}
}

// §11.2: an upload reserves its size in the process's budget against the
// free space minus the margin; when the budget cannot hold it, 507 and
// nothing is written.
//
// The hold is a reservation made against an inflated free space (2^62
// bytes, holding 2^61): whatever the real statfs of the shared ext4
// TMPDIR says when the handler asks, and other packages' tests free and
// fill it concurrently, the remainder is negative, so even an upload of
// zero bytes is refused. That the upload reserves its own size is
// TestUploadReservesItsLength.
func TestUploadInsufficientSpace(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	_, tag := e.album(a.id)
	before := e.state(a.id)
	pinned, _ := e.blobFiles()
	hold, _ := e.budget.Reserve(1<<62, 1<<61)
	if hold == nil {
		t.Fatal("cannot hold the budget")
	}
	for _, empty := range []bool{false, true} {
		att, cover := randomBytes(1<<20), jpegImage(t, 64, 64)
		if empty {
			att = []byte{}
		}
		r := e.wantError(upload("POST", albumPath(a.id)+"/attachments?path=x.bin", tag, att),
			nethttp.StatusInsufficientStorage, CodeInsufficientSpace)
		if r.details()["needed"] != float64(len(att)) {
			t.Fatalf("details %v", r.details())
		}
		e.wantError(upload("PUT", albumPath(a.id)+"/cover", tag, cover),
			nethttp.StatusInsufficientStorage, CodeInsufficientSpace)
	}
	e.sameState(a.id, before)
	if p, temps := e.blobFiles(); p != pinned || temps != 0 {
		t.Fatalf("%d blobs (%d before), %d temporaries", p, pinned, temps)
	}
	if e.budget.Reserved() != 1<<61 {
		t.Fatalf("%d bytes reserved, want only the hold", e.budget.Reserved())
	}

	hold.Release()
	rev := e.revision(a.id)
	e.changed(e.must(upload("POST", albumPath(a.id)+"/attachments?path=x.bin", tag, randomBytes(1<<20)), nethttp.StatusCreated), a.id, rev)
	_, tag = e.album(a.id)
	e.changed(e.must(upload("PUT", albumPath(a.id)+"/cover", tag, jpegImage(t, 64, 64)), nethttp.StatusOK), a.id, rev+1)
	if e.budget.Reserved() != 0 {
		t.Fatalf("%d bytes still reserved", e.budget.Reserved())
	}
}

// §11.2: an attachment upload holds its declared Content-Length in the
// budget while its body is being received, and gives it back when the put
// is over. The body is a pipe: the test observes the budget while the
// server is blocked on the rest of the body.
func TestUploadReservesItsLength(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	_, tag := e.album(a.id)
	rev := e.revision(a.id)
	const size = 5 << 20
	data := randomBytes(size)
	pr, pw := io.Pipe()
	seen := make(chan int64, 1)
	go func() {
		// The first MiB reaches the server; the rest waits until the
		// reservation has been observed.
		_, err := pw.Write(data[:1<<20])
		reserved := int64(-1)
		for deadline := time.Now().Add(30 * time.Second); err == nil && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if reserved = e.budget.Reserved(); reserved == size {
				break
			}
		}
		seen <- reserved
		if err == nil {
			_, err = pw.Write(data[1<<20:])
		}
		pw.CloseWithError(err)
	}()
	up := upload("POST", albumPath(a.id)+"/attachments?path=big.bin", tag, pr)
	up.length = size
	r := e.must(up, nethttp.StatusCreated)
	if got := <-seen; got != size {
		t.Fatalf("while the body was received, %d bytes were reserved, want %d", got, size)
	}
	e.changed(r, a.id, rev)
	if e.budget.Reserved() != 0 {
		t.Fatalf("%d bytes still reserved", e.budget.Reserved())
	}
	for _, raw := range r.body["attachments"].([]any) {
		if at := raw.(map[string]any); at["rel_path"] == "big.bin" {
			if blob := at["blob"].(map[string]any); blob["hash"] != sha(data) {
				t.Fatalf("blob %v", blob)
			}
			return
		}
	}
	t.Fatal("no attachment big.bin")
}

func TestDeleteAttachment(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	b := e.seedReal("Bill Evans", "Portrait in Jazz", catalog.FormatFLAC)
	del := func(att, tag string) req {
		return req{method: "DELETE", path: albumPath(a.id) + "/attachments/" + att, ifMatch: tag}
	}
	_, tag := e.album(a.id)
	rev := e.revision(a.id)
	cover := a.attachments["cover.jpg"].String()
	before := e.state(a.id)
	e.wantError(del(cover, ""), nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired)
	e.wantError(del(cover, ETag(KindAlbum, a.id, rev+1)), nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
	// Another album's attachment under this album: 404, both unchanged.
	bBefore := e.state(b.id)
	e.wantError(del(b.attachments["cover.jpg"].String(), tag), nethttp.StatusNotFound, catalog.CodeAttachmentNotFound)
	e.wantError(del(uuid.NewString(), tag), nethttp.StatusNotFound, catalog.CodeAttachmentNotFound)
	e.wantError(del("not-an-id", tag), nethttp.StatusNotFound, catalog.CodeAttachmentNotFound)
	e.wantError(del(strings.ToUpper(cover), tag), nethttp.StatusNotFound, catalog.CodeAttachmentNotFound)
	e.sameState(a.id, before)
	e.sameState(b.id, bBefore)

	// The attachment holding the cover's image: it goes, the cover stays
	// (N-176), and the blob.
	body := e.changed(e.must(del(cover, tag), nethttp.StatusOK), a.id, rev)
	if c := coverOf(body); c["hash"] != sha(a.cover) || len(body["attachments"].([]any)) != 2 {
		t.Fatalf("cover %v, attachments %v", c, body["attachments"])
	}
	if !bytes.Equal(e.pinnedBytes(sha(a.cover)), a.cover) {
		t.Fatal("the blob went")
	}
	_, tag = e.album(a.id)
	e.wantError(del(cover, tag), nethttp.StatusNotFound, catalog.CodeAttachmentNotFound)
}

func TestLyrics(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	b := e.seedReal("Bill Evans", "Portrait in Jazz", catalog.FormatFLAC)
	second := a.tracks[1].String()
	path := albumPath(a.id) + "/tracks/" + second + "/lyrics"
	_, tag := e.album(a.id)
	rev := e.revision(a.id)
	lrc := []byte("[00:00.50]Freddie\n[00:01.00]Freeloader é\n")
	before := e.state(a.id)

	e.wantError(upload("PUT", path, "", lrc), nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired)
	e.wantError(upload("PUT", path, ETag(KindAlbum, a.id, rev+1), lrc), nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
	// A track of another album under this album, an unknown one.
	e.wantError(upload("PUT", albumPath(a.id)+"/tracks/"+b.tracks[0].String()+"/lyrics", tag, lrc), nethttp.StatusNotFound, catalog.CodeTrackNotFound)
	e.wantError(upload("PUT", albumPath(a.id)+"/tracks/"+uuid.NewString()+"/lyrics", tag, lrc), nethttp.StatusNotFound, catalog.CodeTrackNotFound)
	e.wantError(upload("PUT", albumPath(a.id)+"/tracks/x/lyrics", tag, lrc), nethttp.StatusNotFound, catalog.CodeTrackNotFound)
	// Not UTF-8: Latin-1, a lone continuation byte, an encoded surrogate, a truncated rune.
	for _, bad := range [][]byte{[]byte("caf\xe9"), {0x80}, []byte("\xed\xa0\x80"), []byte("ok \xe2\x98")} {
		e.wantError(upload("PUT", path, tag, bad), nethttp.StatusUnprocessableEntity, catalog.CodeInvalidLyrics)
	}
	// 2 MiB: exactly the limit is fine, one byte more is 413.
	e.declared("PUT", path, tag, MaxLyricsBytes+1, MaxLyricsBytes)
	e.wantError(upload("PUT", path, tag, stream(bytes.Repeat([]byte("a"), MaxLyricsBytes+1))), nethttp.StatusRequestEntityTooLarge, CodeBodyTooLarge)
	e.sameState(a.id, before)

	body := e.changed(e.must(upload("PUT", path, tag, lrc), nethttp.StatusOK), a.id, rev)
	if tracksOf(body)[1]["lyrics_hash"] != sha(lrc) {
		t.Fatalf("track %v", tracksOf(body)[1])
	}
	rev++
	_, tag = e.album(a.id)
	e.noop(e.must(upload("PUT", path, tag, lrc), nethttp.StatusOK), a.id, rev)
	big := bytes.Repeat([]byte("é"), MaxLyricsBytes/2)
	e.changed(e.must(upload("PUT", path, tag, big), nethttp.StatusOK), a.id, rev)
	rev++

	// Assigning an attachment: an .lrc file, UTF-8; it stays an attachment
	// (N-177).
	_, tag = e.album(a.id)
	r := e.must(upload("POST", albumPath(a.id)+"/attachments?path=Lyrics%2FFreddie.LRC", tag, lrc), nethttp.StatusCreated)
	e.changed(r, a.id, rev)
	rev++
	lrcAtt := attachmentIDOf(t, r.body, "Lyrics/Freddie.LRC")
	_, tag = e.album(a.id)
	r = e.must(upload("POST", albumPath(a.id)+"/attachments?path=bad.lrc", tag, []byte("caf\xe9")), nethttp.StatusCreated)
	e.changed(r, a.id, rev)
	rev++
	badAtt := attachmentIDOf(t, r.body, "bad.lrc")
	_, tag = e.album(a.id)
	choose := func(att string) req {
		return req{method: "PUT", path: path, ifMatch: tag, body: map[string]any{"attachment_id": att}}
	}
	e.wantError(choose(a.attachments["notes.html"].String()), nethttp.StatusUnprocessableEntity, catalog.CodeInvalidLyrics)
	e.wantError(choose(badAtt), nethttp.StatusUnprocessableEntity, catalog.CodeInvalidLyrics)
	e.wantError(choose(b.attachments["notes.html"].String()), nethttp.StatusNotFound, catalog.CodeAttachmentNotFound)
	body = e.changed(e.must(choose(lrcAtt), nethttp.StatusOK), a.id, rev)
	if tracksOf(body)[1]["lyrics_hash"] != sha(lrc) || attachmentIDOf(t, body, "Lyrics/Freddie.LRC") != lrcAtt {
		t.Fatal("the assigned attachment")
	}
	rev++

	// DELETE, then a no-op.
	_, tag = e.album(a.id)
	e.wantError(req{method: "DELETE", path: path}, nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired)
	body = e.changed(e.must(req{method: "DELETE", path: path, ifMatch: tag}, nethttp.StatusOK), a.id, rev)
	if tracksOf(body)[1]["lyrics_hash"] != nil {
		t.Fatal("the lyrics stayed")
	}
	rev++
	_, tag = e.album(a.id)
	e.noop(e.must(req{method: "DELETE", path: path, ifMatch: tag}, nethttp.StatusOK), a.id, rev)
	e.wantError(req{method: "GET", path: path}, nethttp.StatusNotFound, CodeLyricsNotFound)
}

// An attachment chosen as lyrics is refused on the snapshot, before its
// blob is read, when the track is not the album's or the name does not
// end in .lrc. The blobs are removed from the store to make that
// observable: reading one is 500, as the last request shows.
func TestLyricsAttachmentPrecheck(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	b := e.seedReal("Bill Evans", "Portrait in Jazz", catalog.FormatFLAC)
	_, tag := e.album(a.id)
	lrc := []byte("[00:00.50]So What\n")
	rev := e.revision(a.id)
	r := e.must(upload("POST", albumPath(a.id)+"/attachments?path=so-what.Lrc", tag, lrc), nethttp.StatusCreated)
	e.changed(r, a.id, rev)
	lrcAtt := attachmentIDOf(t, r.body, "so-what.Lrc")
	var htmlHash string
	for _, raw := range r.body["attachments"].([]any) {
		if at := raw.(map[string]any); at["rel_path"] == "notes.html" {
			htmlHash = at["blob"].(map[string]any)["hash"].(string)
		}
	}
	for _, h := range []string{sha(lrc), htmlHash} {
		if err := os.Remove(filepath.Join(e.data, "originals", h[:2], h[2:4], h)); err != nil {
			t.Fatal(err)
		}
	}
	_, tag = e.album(a.id)
	before := e.state(a.id)
	choose := func(track uuid.UUID, att string) req {
		return req{method: "PUT", path: albumPath(a.id) + "/tracks/" + track.String() + "/lyrics", ifMatch: tag,
			body: map[string]any{"attachment_id": att}}
	}
	r = e.wantError(choose(a.tracks[0], a.attachments["notes.html"].String()), nethttp.StatusUnprocessableEntity, catalog.CodeInvalidLyrics)
	if r.details()["path"] != "notes.html" {
		t.Fatalf("details %v", r.details())
	}
	e.wantError(choose(uuid.New(), lrcAtt), nethttp.StatusNotFound, catalog.CodeTrackNotFound)
	e.wantError(choose(b.tracks[0], lrcAtt), nethttp.StatusNotFound, catalog.CodeTrackNotFound)
	e.sameState(a.id, before)
	e.wantError(choose(a.tracks[0], lrcAtt), nethttp.StatusInternalServerError, CodeInternal)
	e.sameState(a.id, before)
}

// §10.2 (N-172): the path is a query parameter, so a "+" is a space, as
// url.ParseQuery and every HTML form read it; a literal plus is %2B.
func TestAttachmentPathPlus(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	for raw, want := range map[string]string{"a+b.pdf": "a b.pdf", "c%2Bd.pdf": "c+d.pdf"} {
		_, tag := e.album(a.id)
		r := e.must(upload("POST", albumPath(a.id)+"/attachments?path="+raw, tag, []byte("%PDF-1.4\n")), nethttp.StatusCreated)
		attachmentIDOf(t, r.body, want)
	}
}

func TestDeleteTrack(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	b := e.seedReal("Bill Evans", "Portrait in Jazz", catalog.FormatFLAC)
	del := func(track, tag string) req {
		return req{method: "DELETE", path: albumPath(a.id) + "/tracks/" + track, ifMatch: tag}
	}
	_, tag := e.album(a.id)
	rev := e.revision(a.id)
	before := e.state(a.id)
	first := a.tracks[0].String()
	e.wantError(del(first, ""), nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired)
	e.wantError(del(first, ETag(KindAlbum, a.id, rev+1)), nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
	e.wantError(del(b.tracks[0].String(), tag), nethttp.StatusNotFound, catalog.CodeTrackNotFound)
	e.wantError(del("1", tag), nethttp.StatusNotFound, catalog.CodeTrackNotFound)
	e.sameState(a.id, before)
	body := e.changed(e.must(del(first, tag), nethttp.StatusOK), a.id, rev)
	if ts := tracksOf(body); len(ts) != 1 || ts[0]["id"] != a.tracks[1].String() {
		t.Fatalf("tracks %v", ts)
	}
	if !bytes.Equal(e.pinnedBytes(sha(a.audio[0])), a.audio[0]) {
		t.Fatal("the blob went")
	}
	_, tag = e.album(a.id)
	e.wantError(del(a.tracks[1].String(), tag), nethttp.StatusUnprocessableEntity, catalog.CodeNoTracks)
	e.wantError(del(first, tag), nethttp.StatusNotFound, catalog.CodeTrackNotFound)
	// Downloads of the removed track: gone.
	e.wantError(req{method: "GET", path: albumPath(a.id) + "/tracks/" + first + "/original"}, nethttp.StatusNotFound, catalog.CodeTrackNotFound)
}

// §12.2 "Due finestre UI" for the uploads: two covers uploaded at once on
// the same revision. Exactly one is saved; the other is 412 naming the
// winner's revision, and its blob stays pinned and unreferenced.
func TestConcurrentCoverUploads(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	for round := 0; round < 15; round++ {
		_, tag := e.album(a.id)
		rev := e.revision(a.id)
		imgs := [][]byte{pngImage(t, 20, 20), jpegImage(t, 20, 20)}
		var wg sync.WaitGroup
		out := make([]resp, 2)
		for i := range imgs {
			wg.Go(func() { out[i] = e.do(upload("PUT", albumPath(a.id)+"/cover", tag, imgs[i])) })
		}
		wg.Wait()
		won := -1
		for i, r := range out {
			switch r.status {
			case nethttp.StatusOK:
				if won >= 0 {
					t.Fatalf("round %d: both saved", round)
				}
				won = i
			case nethttp.StatusPreconditionFailed:
				if r.details()["revision"] != float64(rev+1) {
					t.Fatalf("round %d: 412 details %v", round, r.details())
				}
			default:
				t.Fatalf("round %d: %d %s", round, r.status, r.raw)
			}
		}
		if won < 0 {
			t.Fatalf("round %d: none saved", round)
		}
		if b, _ := e.album(a.id); coverOf(b)["hash"] != sha(imgs[won]) || e.revision(a.id) != rev+1 {
			t.Fatalf("round %d: cover %v", round, coverOf(b))
		}
		// The loser met the 412 either on the snapshot, before its put, or
		// in the transaction, after it: its blob is absent, or pinned
		// intact and unreferenced (N-173); never recorded.
		lost := imgs[1-won]
		if b, err := os.ReadFile(filepath.Join(e.data, "originals", sha(lost)[:2], sha(lost)[2:4], sha(lost))); (err == nil && !bytes.Equal(b, lost)) ||
			(err != nil && !errors.Is(err, fs.ErrNotExist)) || e.count(`SELECT count(*) FROM blobs WHERE hash = $1`, sha(lost)) != 0 {
			t.Fatalf("round %d: the loser's blob: %v", round, err)
		}
		if _, temps := e.blobFiles(); temps != 0 {
			t.Fatalf("round %d: %d temporaries", round, temps)
		}
	}
}

// §10.2: "Se il salvataggio fallisce resta un blob non referenziato, non
// un riferimento rotto". A change commits between the put and the
// transaction (the failpoint upload_pinned): the upload is 412, its blob
// pinned and unreferenced, the album as the other change left it.
func TestUploadFailsAfterThePut(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	for _, tc := range []struct {
		name string
		req  func(tag string) req
		file []byte
	}{
		{"cover", func(tag string) req { return upload("PUT", albumPath(a.id)+"/cover", tag, nil) }, pngImage(t, 9, 9)},
		{"attachment", func(tag string) req { return upload("POST", albumPath(a.id)+"/attachments?path=late.bin", tag, nil) }, randomBytes(777)},
		{"lyrics", func(tag string) req {
			return upload("PUT", albumPath(a.id)+"/tracks/"+a.tracks[1].String()+"/lyrics", tag, nil)
		}, []byte("[00:00.00]late\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tag := e.album(a.id)
			rev := e.revision(a.id)
			var hits int
			e.hooks.Set(func(p failpoint.Point) error {
				if p.Name != "upload_pinned" {
					return nil
				}
				hits++
				// Another client's change commits now.
				_, _, err := e.svc.TrashAlbum(context.Background(), a.id, rev)
				return err
			})
			defer e.hooks.Set(nil)
			r := tc.req(tag)
			r.body = tc.file
			e.wantError(r, nethttp.StatusPreconditionFailed, catalog.CodePreconditionFailed)
			if hits != 1 {
				t.Fatalf("%d hits", hits)
			}
			if !bytes.Equal(e.pinnedBytes(sha(tc.file)), tc.file) || e.count(`SELECT count(*) FROM blobs WHERE hash = $1`, sha(tc.file)) != 0 {
				t.Fatal("the blob is not pinned and unreferenced")
			}
			body, _ := e.album(a.id)
			if body["revision"] != float64(rev+1) || body["trashed"] != true {
				t.Fatalf("album %v", body)
			}
			// Put it back for the next case.
			_, _, err := e.svc.RestoreAlbum(context.Background(), a.id, rev+1)
			if err != nil {
				t.Fatal(err)
			}
			e.exec(`DELETE FROM jobs WHERE kind = 'render'`)
		})
	}
	// A failure of the failpoint itself: 500, nothing referenced.
	e.hooks.Set(func(p failpoint.Point) error { return errors.New("injected") })
	defer e.hooks.Set(nil)
	_, tag := e.album(a.id)
	e.wantError(upload("PUT", albumPath(a.id)+"/cover", tag, pngImage(t, 7, 7)), nethttp.StatusInternalServerError, CodeInternal)
}

// Every mutation of round 14 is behind §10.4's boundary.
func TestContentBoundary(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	_, tag := e.album(a.id)
	before := e.state(a.id)
	for _, r := range []req{
		upload("PUT", albumPath(a.id)+"/cover", tag, pngImage(t, 4, 4)),
		{method: "DELETE", path: albumPath(a.id) + "/cover", ifMatch: tag},
		upload("POST", albumPath(a.id)+"/attachments?path=x", tag, []byte("x")),
		upload("POST", albumPath(a.id)+"/tracks?name=x.flac", tag, []byte("x")),
		{method: "DELETE", path: albumPath(a.id) + "/attachments/" + a.attachments["notes.html"].String(), ifMatch: tag},
		upload("PUT", albumPath(a.id)+"/tracks/"+a.tracks[0].String()+"/lyrics", tag, []byte("x")),
		{method: "DELETE", path: albumPath(a.id) + "/tracks/" + a.tracks[0].String() + "/lyrics", ifMatch: tag},
		{method: "DELETE", path: albumPath(a.id) + "/tracks/" + a.tracks[0].String(), ifMatch: tag},
	} {
		noHeader := r
		noHeader.headers = map[string][]string{RequestHeader: {""}}
		for k, v := range r.headers {
			noHeader.headers[k] = v
		}
		e.wantError(noHeader, nethttp.StatusForbidden, CodeRequestHeaderRequired)
		evil := r
		evil.headers = map[string][]string{"Origin": {"http://evil.test"}}
		for k, v := range r.headers {
			evil.headers[k] = v
		}
		e.wantError(evil, nethttp.StatusForbidden, CodeOriginNotAllowed)
	}
	e.sameState(a.id, before)
	// Wrong methods: 405 with Allow.
	r := e.wantError(req{method: "GET", path: albumPath(a.id) + "/attachments"}, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed)
	if r.header.Get("Allow") != "POST" {
		t.Fatalf("Allow %q", r.header.Get("Allow"))
	}
	r = e.wantError(req{method: "POST", path: albumPath(a.id) + "/cover", body: "{}"}, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed)
	if r.header.Get("Allow") != "GET, HEAD, PUT, DELETE" {
		t.Fatalf("Allow %q", r.header.Get("Allow"))
	}
}

// A body that cannot be read to its end (here a malformed chunked
// encoding) is 400 upload_incomplete; nothing is pinned and the temporary
// is removed.
func TestUploadIncomplete(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	_, tag := e.album(a.id)
	before := e.state(a.id)
	for _, path := range []string{albumPath(a.id) + "/attachments?path=x.bin", albumPath(a.id) + "/cover"} {
		conn, err := net.Dial("tcp", e.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		head := "PUT " + path + " HTTP/1.1\r\n"
		if strings.Contains(path, "attachments") {
			head = "POST " + path + " HTTP/1.1\r\n"
		}
		head += "Host: " + testHost + "\r\n" + RequestHeader + ": 1\r\nIf-Match: " + tag + "\r\nContent-Type: " +
			UploadMediaType + "\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nabcd\r\nzz\r\n"
		if _, err := conn.Write([]byte(head)); err != nil {
			t.Fatal(err)
		}
		res, err := nethttp.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = conn.Close()
		if res.StatusCode != nethttp.StatusBadRequest || !strings.Contains(string(b), `"code":"upload_incomplete"`) {
			t.Fatalf("%s: %d %s", path, res.StatusCode, b)
		}
	}
	e.sameState(a.id, before)
}
