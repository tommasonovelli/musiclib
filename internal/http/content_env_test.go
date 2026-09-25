package http

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"math/rand/v2"
	"net"
	nethttp "net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// Fixtures and helpers of the content endpoints (round 14): real blobs in
// the real blob store on ext4, real images encoded by the standard
// library, and requests whose body is a file.

// realAlbum is an album committed with real blobs: every hash names a
// file in originals/, so that the content endpoints can read it.
type realAlbum struct {
	id      uuid.UUID
	tracks  []uuid.UUID // by disc and number
	audio   [][]byte
	lrc     []byte
	cover   []byte
	booklet []byte
	// attachments by rel_path
	attachments map[string]uuid.UUID
}

// Names of the first track and of the attachments of seedReal: a quote,
// a backslash, CR/LF, a percent sign, a semicolon and non-ASCII, to test
// the Content-Disposition of the downloads.
const (
	hostileTrackName = "CD1/01 \"So\" What\\ \r\nX-Evil: 1; %41 é ☃.flac"
	hostileLRCName   = "CD1/01 \"So\" What\\ \r\nX-Evil: 1; %41 é ☃.lrc"
)

// put pins b in the env's blob store.
func (e *env) put(b []byte) catalog.Blob {
	e.t.Helper()
	pb, err := e.blobs.Put(context.Background(), bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	return catalog.Blob{Hash: pb.SHA256, Size: pb.Size}
}

// seedReal imports an album of two tracks of audioFormat (the bytes are
// not audio: the catalog never reads them, the API only serves them), an
// LRC for the first, a JPEG cover kept as an attachment (§7.4), a PDF
// booklet, and plain notes.
func (e *env) seedReal(artist, title, audioFormat string) realAlbum {
	e.t.Helper()
	ctx := context.Background()
	a := realAlbum{
		audio:   [][]byte{randomBytes(3000), randomBytes(2500)},
		lrc:     []byte("[00:01.00]So what\n[00:02.00]é ☃\n"),
		cover:   jpegImage(e.t, 16, 16),
		booklet: append([]byte("%PDF-1.7\n"), randomBytes(1000)...),
	}
	notes := []byte("<script>alert(1)</script>")
	b1, b2, lrc, cover, booklet, nb := e.put(a.audio[0]), e.put(a.audio[1]), e.put(a.lrc), e.put(a.cover), e.put(a.booklet), e.put(notes)
	b1.Format, b2.Format, cover.Format = audioFormat, audioFormat, catalog.FormatJPEG
	batch, job := store.NewID(), store.NewID()
	e.exec(`INSERT INTO import_batches (id, root_rel, created_at) VALUES ($1, 'in', now())`, batch)
	e.exec(`INSERT INTO jobs (id, kind, batch_id, source_rel, state, queued_at, updated_at)
		VALUES ($1, 'import', $2, $3, 'pending', now(), now())`, job, batch, "src-"+job.String())
	var ticket int64
	if err := e.db.QueryRow(ctx, `UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1 RETURNING claimed`,
		job).Scan(&ticket); err != nil {
		e.t.Fatal(err)
	}
	blobs := []catalog.Blob{b1, b2, lrc, cover, booklet, nb}
	if b1.Hash == b2.Hash {
		e.t.Fatal("two equal random tracks")
	}
	out, err := e.svc.CommitImport(ctx, catalog.ImportCandidate{
		Attempt: jobs.Attempt{JobID: job, Ticket: ticket}, Fingerprint: newHash(), Blobs: blobs,
		Artist: artist, Title: title, CoverHash: &cover.Hash,
		Tracks: []catalog.ImportTrack{
			{SourcePath: hostileTrackName, Disc: 1, No: 1, Title: "So What", BlobHash: b1.Hash,
				Lyrics: &catalog.ImportLyrics{SourcePath: hostileLRCName, BlobHash: lrc.Hash}},
			{SourcePath: "CD1/02 Freddie.flac", Disc: 1, No: 2, Title: "Freddie Freeloader", BlobHash: b2.Hash},
		},
		Attachments: []catalog.ImportAttachment{
			{RelPath: "Scans/Booklet.pdf", BlobHash: booklet.Hash},
			{RelPath: "cover.jpg", BlobHash: cover.Hash},
			{RelPath: "notes.html", BlobHash: nb.Hash},
		},
	})
	if err != nil || out.State != jobs.StateDone {
		e.t.Fatalf("CommitImport: %+v %v", out, err)
	}
	a.id = out.AlbumID
	body, _ := e.album(a.id)
	for _, t := range tracksOf(body) {
		id, _ := uuid.Parse(t["id"].(string))
		a.tracks = append(a.tracks, id)
	}
	a.attachments = map[string]uuid.UUID{}
	for _, raw := range body["attachments"].([]any) {
		at := raw.(map[string]any)
		id, _ := uuid.Parse(at["id"].(string))
		a.attachments[at["rel_path"].(string)] = id
	}
	e.exec(`DELETE FROM jobs WHERE kind = 'render'`)
	return a
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(hashSeq.Add(1))))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// noise is a w×h image of random pixels, so that encodings differ.
func noise(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(hashSeq.Add(1))))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(r.Uint32()), uint8(r.Uint32()), uint8(r.Uint32()), 255})
		}
	}
	return img
}

func jpegImage(t testing.TB, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, noise(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func pngImage(t testing.TB, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, noise(w, h)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngChunk is one PNG chunk with its CRC.
func pngChunk(typ string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(append(out, typ...), data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(typ), data...)))
}

// pngOfSize is a valid PNG of exactly size bytes: a small image with a
// private ancillary chunk ("prVt", ignored by decoders) before IEND.
func pngOfSize(t testing.TB, size int) []byte {
	t.Helper()
	img := pngImage(t, 8, 8)
	iend := len(img) - 12
	pad := size - len(img) - 12
	if pad < 0 {
		t.Fatalf("a PNG of %d bytes is too small", size)
	}
	out := append(append(append([]byte(nil), img[:iend]...), pngChunk("prVt", make([]byte, pad))...), img[iend:]...)
	if len(out) != size {
		t.Fatalf("pngOfSize: %d bytes", len(out))
	}
	return out
}

// pngHeader is a PNG signature and IHDR declaring w×h pixels, then IEND:
// DecodeConfig reads the size, a decode fails.
func pngHeader(w, h int) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, uint32(w))
	ihdr = binary.BigEndian.AppendUint32(ihdr, uint32(h))
	ihdr = append(ihdr, 8, 2, 0, 0, 0)
	out := append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", ihdr)...)
	return append(out, pngChunk("IEND", nil)...)
}

// upload is a request whose body is a file.
func upload(method, path, ifMatch string, body any) req {
	return req{method: method, path: path, ifMatch: ifMatch, body: body,
		headers: map[string][]string{"Content-Type": {UploadMediaType}}}
}

// blobFiles counts the pinned blobs and the temporaries of the store.
func (e *env) blobFiles() (pinned, temps int) {
	e.t.Helper()
	err := filepath.WalkDir(filepath.Join(e.data, "originals"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			pinned++
		}
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	ents, err := os.ReadDir(filepath.Join(e.data, "work", "blobs"))
	if err != nil {
		e.t.Fatal(err)
	}
	return pinned, len(ents)
}

// pinnedBytes reads a pinned blob.
func (e *env) pinnedBytes(hash string) []byte {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.data, "originals", hash[:2], hash[2:4], hash))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// rawRequest sends a request head with the given headers and no body at
// all, and reads the answer: a server that tried to read the declared
// body would never answer, so an answer proves that the request was
// refused before its body was read.
func (e *env) rawRequest(method, path string, headers map[string]string) *nethttp.Response {
	e.t.Helper()
	conn, err := net.DialTimeout("tcp", e.srv.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		e.t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: %s\r\n%s: 1\r\n", method, path, testHost, RequestHeader)
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		e.t.Fatal(err)
	}
	res, err := nethttp.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		e.t.Fatalf("%s %s: no answer before the body: %v", method, path, err)
	}
	return res
}

// patterned is a reader of n bytes that are a pure function of their
// offset, generated as they are read: a large body never held in memory.
type patterned struct {
	n, off int64
}

func (p *patterned) Read(b []byte) (int, error) {
	if p.off >= p.n {
		return 0, io.EOF
	}
	if rest := p.n - p.off; int64(len(b)) > rest {
		b = b[:rest]
	}
	for i := range b {
		o := p.off + int64(i)
		b[i] = byte(o*7 + o>>11)
	}
	p.off += int64(len(b))
	return len(b), nil
}

// stream hides the length of b from the client, which then sends it
// chunked, without Content-Length.
func stream(b []byte) io.Reader { return struct{ io.Reader }{bytes.NewReader(b)} }

// declared sends an upload head that announces n bytes and no body, and
// checks that it is refused with 413 before the body is read.
func (e *env) declared(method, path, tag string, n int, limit int) {
	e.t.Helper()
	res := e.rawRequest(method, path, map[string]string{
		"If-Match": tag, "Content-Type": UploadMediaType, "Content-Length": fmt.Sprint(n)})
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	if res.StatusCode != nethttp.StatusRequestEntityTooLarge || !strings.Contains(string(b), `"code":"body_too_large"`) ||
		!strings.Contains(string(b), fmt.Sprintf(`"limit":%d`, limit)) {
		e.t.Fatalf("%s %s announcing %d bytes: %d %s", method, path, n, res.StatusCode, b)
	}
}
