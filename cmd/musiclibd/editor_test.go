package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/media"
	"musiclib/internal/render"
	"musiclib/internal/store/pgtest"
)

// The album editor's content end to end (§10.2, round 14): a FLAC, an MP3
// and an M4A track imported by the server, then every content operation
// through the server's API, each published by the server's own workers,
// and each checked in library/: the files of the receipt, and the covers
// the tag helper reads back from each format.

const lamePath = "/usr/local/bin/lame"

// runTool runs a pinned tool of the test image.
func runTool(t *testing.T, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", filepath.Base(name), err, out)
	}
}

// writeMixedAlbum writes a FLAC, an MP3 and an M4A track of one album in
// imports/dir, tagged by ffmpeg and LAME.
func writeMixedAlbum(t *testing.T, imports, dir, artist, album string) {
	t.Helper()
	d := filepath.Join(imports, dir)
	writeFLAC(t, filepath.Join(d, "01.flac"), 300, 0.5, map[string]string{
		"ARTIST": artist, "ALBUM": album, "TITLE": "Track 1", "TRACKNUMBER": "1", "DATE": "1958"})
	wav := filepath.Join(t.TempDir(), "a.wav")
	runTool(t, media.FFmpegPath, "-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "sine=frequency=400:duration=0.5:sample_rate=44100", "-ac", "2", "-c:a", "pcm_s16le", wav)
	runTool(t, lamePath, "--quiet", "-V2", "--add-id3v2", "--tt", "Track 2", "--ta", artist, "--tl", album,
		"--tn", "2", "--ty", "1958", wav, filepath.Join(d, "02.mp3"))
	runTool(t, media.FFmpegPath, "-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "sine=frequency=500:duration=0.5:sample_rate=44100", "-ac", "2", "-c:a", "aac",
		"-metadata", "title=Track 3", "-metadata", "artist="+artist, "-metadata", "album="+album,
		"-metadata", "track=3", "-metadata", "date=1958", filepath.Join(d, "03.m4a"))
}

// testJPEG is a JPEG of random pixels.
func testJPEG(t *testing.T, w, h int, seed byte) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{byte(x*7) ^ seed, byte(y*13) + seed, byte(x * y), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// upload sends a file as the body of an API request (§10.2), as a
// well-behaved client: the Host of PUBLIC_ORIGIN, X-Musiclib-Request,
// If-Match, Content-Type application/octet-stream.
func (d *testDaemon) upload(t *testing.T, method, path, ifMatch string, body []byte, status int) apiResponse {
	t.Helper()
	req, err := http.NewRequest(method, d.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-Musiclib-Request", "1")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("If-Match", ifMatch)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	out := apiResponse{status: resp.StatusCode, header: resp.Header}
	if err := json.NewDecoder(resp.Body).Decode(&out.body); err != nil {
		t.Fatalf("%s %s: the body is not JSON: %v", method, path, err)
	}
	if out.status != status {
		t.Fatalf("%s %s = %d %v, want %d", method, path, out.status, out.body, status)
	}
	return out
}

// fetch is a GET of a download.
func (d *testDaemon) fetch(t *testing.T, path string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, d.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:8080"
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

// editor is the album under edit through the server's API.
type editor struct {
	t    *testing.T
	d    *testDaemon
	db   *pgxpool.Pool
	data string
	id   uuid.UUID
	path string
}

// etag reads the album and returns its ETag and body.
func (ed *editor) etag() (string, map[string]any) {
	r := ed.d.mustAPI(ed.t, http.MethodGet, ed.path, "", nil, http.StatusOK)
	return r.header.Get("ETag"), r.body
}

// published waits for the album's current revision to be published by
// the workers, with no failed job, and returns the files of its receipt.
func (ed *editor) published() []string {
	ed.t.Helper()
	waitFor(ed.t, "the change to be published", func() bool {
		a := published(ed.t, ed.db, ed.id)
		return idle(ed.t, ed.db) && a.PublishedRevision == a.Revision
	})
	if n := queryInt(ed.t, ed.db, `SELECT count(*) FROM jobs WHERE state = 'failed'`); n != 0 {
		ed.t.Fatalf("%d failed jobs; logs:\n%s", n, ed.d.logs)
	}
	a := published(ed.t, ed.db, ed.id)
	b, err := os.ReadFile(filepath.Join(ed.dir(), render.ReceiptName))
	if err != nil {
		ed.t.Fatal(err)
	}
	r, err := render.ParseReceipt(b)
	if err != nil || r.AlbumRevision != a.Revision || render.ReceiptHash(b) != *a.PublishedReceiptHash {
		ed.t.Fatalf("receipt %+v %v", r, err)
	}
	var files []string
	for _, f := range r.Files {
		files = append(files, f.RelativePath)
		c, err := os.ReadFile(filepath.Join(ed.dir(), filepath.FromSlash(f.RelativePath)))
		if err != nil || sum(c) != f.SHA256 {
			ed.t.Fatalf("%s does not match the receipt", f.RelativePath)
		}
	}
	// Exactly the receipt's files are in the directory.
	var onDisk []string
	err = filepath.WalkDir(ed.dir(), func(p string, de os.DirEntry, err error) error {
		if err == nil && de.Type().IsRegular() && de.Name() != render.ReceiptName {
			rel, _ := filepath.Rel(ed.dir(), p)
			onDisk = append(onDisk, filepath.ToSlash(rel))
		}
		return err
	})
	slices.Sort(onDisk)
	if err != nil || !slices.Equal(onDisk, files) {
		ed.t.Fatalf("on disk %q, receipt %q (%v)", onDisk, files, err)
	}
	return files
}

func (ed *editor) dir() string {
	return filepath.Join(ed.data, "library", "Art Blakey", "Moanin'")
}

// covers reads the pictures of the three tracks with the tag helper: the
// SHA-256 of each track's pictures.
func (ed *editor) covers(tracks map[string]string) map[string][]string {
	ed.t.Helper()
	tools, err := media.NewTools(context.Background(), media.NewRunner(1), media.FFmpegPath, media.FFprobePath, media.TagsPath)
	if err != nil {
		ed.t.Fatal(err)
	}
	root, err := fsops.OpenRoot(ed.dir())
	if err != nil {
		ed.t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			ed.t.Error(err)
		}
	}()
	out := map[string][]string{}
	for name, format := range tracks {
		f, err := root.Open(name)
		if err != nil {
			ed.t.Fatal(err)
		}
		in, err := tools.Inspect(context.Background(), f, format)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			ed.t.Fatalf("%s: %v", name, err)
		}
		for _, p := range in.Pictures {
			out[name] = append(out[name], p.MIME+" "+p.SHA256)
		}
		if len(in.Opaque) > 0 {
			ed.t.Fatalf("%s: opaque fields %+v", name, in.Opaque)
		}
	}
	return out
}

func attachmentID(t *testing.T, body map[string]any, relPath string) string {
	t.Helper()
	for _, raw := range body["attachments"].([]any) {
		a := raw.(map[string]any)
		if a["rel_path"] == relPath {
			return a["id"].(string)
		}
	}
	t.Fatalf("no attachment %q", relPath)
	return ""
}

func trackIDs(body map[string]any) []string {
	var out []string
	for _, raw := range body["tracks"].([]any) {
		out = append(out, raw.(map[string]any)["id"].(string))
	}
	return out
}

func TestEndToEndEditorContent(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeMixedAlbum(t, p.imports, "Moanin", "Art Blakey", "Moanin'")
	source := map[string][]byte{}
	for _, n := range []string{"01.flac", "02.mp3", "03.m4a"} {
		b, err := os.ReadFile(filepath.Join(p.imports, "Moanin", n))
		if err != nil {
			t.Fatal(err)
		}
		source[n] = b
	}
	cfg := testConfig(dbURL)
	cfg.Workers = 2
	d := startDaemon(t, cfg, p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	db := dbPool(t, dbURL)
	if _, err := catalogOn(t, db).CreateImportBatch(context.Background(), uuid.New(), ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the album to be published", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision > 0`) == 1
	})
	ed := &editor{t: t, d: d, db: db, data: p.data}
	if err := db.QueryRow(context.Background(), `SELECT id FROM albums`).Scan(&ed.id); err != nil {
		t.Fatal(err)
	}
	ed.path = "/api/albums/" + ed.id.String()
	tracks := map[string]string{"01 - Track 1.flac": catalog.FormatFLAC, "02 - Track 2.mp3": catalog.FormatMP3,
		"03 - Track 3.m4a": catalog.FormatM4AAAC}
	music := []string{"01 - Track 1.flac", "02 - Track 2.mp3", "03 - Track 3.m4a"}
	if files := ed.published(); !slices.Equal(files, music) {
		t.Fatalf("imported: %q", files)
	}
	for name, pics := range ed.covers(tracks) {
		t.Fatalf("%s has pictures before any cover: %q", name, pics)
	}
	tag, body := ed.etag()
	ids := trackIDs(body)

	// The originals can be downloaded by id, byte for byte (§10.2).
	for i, n := range []string{"01.flac", "02.mp3", "03.m4a"} {
		status, h, b := d.fetch(t, ed.path+"/tracks/"+ids[i]+"/original")
		if status != http.StatusOK || !bytes.Equal(b, source[n]) || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("original %s: %d, %d bytes, %v", n, status, len(b), h)
		}
	}

	// A cover: cover.jpg, and the one embedded front cover of every format.
	cover := testJPEG(t, 48, 32, 1)
	d.upload(t, http.MethodPut, ed.path+"/cover", tag, cover, http.StatusOK)
	if files := ed.published(); !slices.Equal(files, append(slices.Clone(music), "cover.jpg")) {
		t.Fatalf("with a cover: %q", files)
	}
	if b, _ := os.ReadFile(filepath.Join(ed.dir(), "cover.jpg")); !bytes.Equal(b, cover) {
		t.Fatal("cover.jpg is not the upload")
	}
	for _, name := range music {
		if got := ed.covers(map[string]string{name: tracks[name]})[name]; !slices.Equal(got, []string{"image/jpeg " + sum(cover)}) {
			t.Fatalf("%s: pictures %q", name, got)
		}
	}
	// Removed: no cover file, no picture in any format (§10.2: "anche nei
	// tag futuri").
	tag, _ = ed.etag()
	d.mustAPI(t, http.MethodDelete, ed.path+"/cover", tag, nil, http.StatusOK)
	if files := ed.published(); !slices.Equal(files, music) {
		t.Fatalf("after the removal: %q", files)
	}
	for name, pics := range ed.covers(tracks) {
		t.Fatalf("%s still has pictures: %q", name, pics)
	}

	// An attachment under Extras/, then removed.
	pdf := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("booklet "), 500)...)
	tag, _ = ed.etag()
	r := d.upload(t, http.MethodPost, ed.path+"/attachments?path="+url.QueryEscape("Scans/Booklet (1958).pdf"), tag, pdf, http.StatusCreated)
	booklet := attachmentID(t, r.body, "Scans/Booklet (1958).pdf")
	if files := ed.published(); !slices.Equal(files, append(slices.Clone(music), "Extras/Scans/Booklet (1958).pdf")) {
		t.Fatalf("with an attachment: %q", files)
	}
	status, h, b := d.fetch(t, ed.path+"/attachments/"+booklet+"/content")
	if status != http.StatusOK || !bytes.Equal(b, pdf) || h.Get("Content-Type") != "application/pdf" {
		t.Fatalf("the attachment's download: %d %v", status, h)
	}
	tag, _ = ed.etag()
	d.mustAPI(t, http.MethodDelete, ed.path+"/attachments/"+booklet, tag, nil, http.StatusOK)
	if files := ed.published(); !slices.Equal(files, music) || exists(t, filepath.Join(ed.dir(), "Extras")) {
		t.Fatalf("after removing the attachment: %q", files)
	}

	// Lyrics: an upload for the FLAC track, an .lrc attachment assigned
	// to the MP3 track (it stays an attachment, N-177); each next to its
	// track with its basename (§5.1).
	one := []byte("[00:00.10]one ☃\n")
	two := []byte("[00:00.20]two\n")
	tag, _ = ed.etag()
	d.upload(t, http.MethodPut, ed.path+"/tracks/"+ids[0]+"/lyrics", tag, one, http.StatusOK)
	tag, _ = ed.etag()
	r = d.upload(t, http.MethodPost, ed.path+"/attachments?path=Lyrics%2Ftwo.lrc", tag, two, http.StatusCreated)
	twoAtt := attachmentID(t, r.body, "Lyrics/two.lrc")
	tag, _ = ed.etag()
	d.mustAPI(t, http.MethodPut, ed.path+"/tracks/"+ids[1]+"/lyrics", tag, map[string]string{"attachment_id": twoAtt}, http.StatusOK)
	want := []string{"01 - Track 1.flac", "01 - Track 1.lrc", "02 - Track 2.lrc", "02 - Track 2.mp3", "03 - Track 3.m4a", "Extras/Lyrics/two.lrc"}
	if files := ed.published(); !slices.Equal(files, want) {
		t.Fatalf("with lyrics: %q", files)
	}
	for name, content := range map[string][]byte{"01 - Track 1.lrc": one, "02 - Track 2.lrc": two, "Extras/Lyrics/two.lrc": two} {
		if b, _ := os.ReadFile(filepath.Join(ed.dir(), filepath.FromSlash(name))); !bytes.Equal(b, content) {
			t.Fatalf("%s: %q", name, b)
		}
	}
	status, h, b = d.fetch(t, ed.path+"/tracks/"+ids[0]+"/lyrics")
	if status != http.StatusOK || !bytes.Equal(b, one) || h.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("the lyrics' download: %d %v", status, h)
	}
	tag, _ = ed.etag()
	d.mustAPI(t, http.MethodDelete, ed.path+"/tracks/"+ids[0]+"/lyrics", tag, nil, http.StatusOK)
	if files := ed.published(); slices.Contains(files, "01 - Track 1.lrc") {
		t.Fatalf("the removed lyrics are published: %q", files)
	}

	// Tracks: the M4A and the MP3 removed, the last one refused (§4.3).
	tag, _ = ed.etag()
	d.mustAPI(t, http.MethodDelete, ed.path+"/tracks/"+ids[2], tag, nil, http.StatusOK)
	if files := ed.published(); slices.Contains(files, "03 - Track 3.m4a") || !slices.Contains(files, "02 - Track 2.mp3") {
		t.Fatalf("after removing track 3: %q", files)
	}
	tag, _ = ed.etag()
	d.mustAPI(t, http.MethodDelete, ed.path+"/tracks/"+ids[1], tag, nil, http.StatusOK)
	if files := ed.published(); !slices.Equal(files, []string{"01 - Track 1.flac", "Extras/Lyrics/two.lrc"}) {
		t.Fatalf("after removing track 2: %q", files)
	}
	tag, _ = ed.etag()
	if r := d.mustAPI(t, http.MethodDelete, ed.path+"/tracks/"+ids[0], tag, nil, http.StatusUnprocessableEntity); r.code() != catalog.CodeNoTracks {
		t.Fatalf("the last track: %v", r.body)
	}
	// The removed tracks' originals stay (§4.3: the blob stays), and so
	// does /import.
	for _, n := range []string{"02.mp3", "03.m4a"} {
		if !exists(t, filepath.Join(p.data, "originals", sum(source[n])[:2], sum(source[n])[2:4], sum(source[n]))) {
			t.Fatalf("the original of %s went", n)
		}
	}
	for _, dir := range []string{"render", "retired", "blobs"} {
		if ents, _ := os.ReadDir(filepath.Join(p.data, "work", dir)); len(ents) != 0 {
			t.Fatalf("work/%s holds %d entries", dir, len(ents))
		}
	}
}

// mp4Box is an ISO BMFF box with a 32-bit size.
func mp4Box(typ string, payload ...[]byte) []byte {
	body := slices.Concat(payload...)
	return slices.Concat(binary.BigEndian.AppendUint32(nil, uint32(8+len(body))), []byte(typ), body)
}

// itunesData is an iTunes "data" atom of the given type, locale 0.
func itunesData(typ uint32, value []byte) []byte {
	return mp4Box("data", binary.BigEndian.AppendUint32(nil, typ), make([]byte, 4), value)
}

// withCoverBeforeITunSMPB appends to the ilst of an M4A whose moov follows
// its media data a JPEG covr item (data type 13, which makes FFmpeg create
// an attached-picture stream), then an iTunes gapless item iTunSMPB with a
// priming of 1,024 samples: the layout of NOTES.md N-168, where FFmpeg
// gives the priming to the picture's stream and not to the audio.
func withCoverBeforeITunSMPB(t *testing.T, m4a, cover []byte) []byte {
	t.Helper()
	smpb := fmt.Sprintf(" 00000000 %08X %08X %016X 00000000 00000000", 1024, 0, 0)
	items := slices.Concat(
		mp4Box("covr", itunesData(13, cover)),
		mp4Box("----", mp4Box("mean", make([]byte, 4), []byte("com.apple.iTunes")),
			mp4Box("name", make([]byte, 4), []byte("iTunSMPB")), itunesData(1, []byte(smpb))))
	// The chain moov/udta/meta/ilst: each size grows by len(items).
	var chain []int // offsets of the boxes to grow
	find := func(start, end int, typ string) int {
		for off := start; off+8 <= end; {
			size := int(binary.BigEndian.Uint32(m4a[off:]))
			if size < 8 || off+size > end {
				t.Fatalf("box at %d: size %d", off, size)
			}
			if string(m4a[off+4:off+8]) == typ {
				return off
			}
			off += size
		}
		t.Fatalf("no %s box", typ)
		return 0
	}
	sizeAt := func(off int) int { return int(binary.BigEndian.Uint32(m4a[off:])) }
	moov := find(0, len(m4a), "moov")
	if find(0, len(m4a), "mdat") > moov {
		t.Fatal("moov is before the media data")
	}
	udta := find(moov+8, moov+sizeAt(moov), "udta")
	meta := find(udta+8, udta+sizeAt(udta), "meta")
	ilst := find(meta+12, meta+sizeAt(meta), "ilst") // meta is a full box
	chain = append(chain, moov, udta, meta, ilst)
	end := ilst + sizeAt(ilst)
	out := slices.Concat(m4a[:end], items, m4a[end:])
	for _, off := range chain {
		binary.BigEndian.PutUint32(out[off:], uint32(sizeAt(off)+len(items)))
	}
	return out
}

// N-168's accepted residual, through the whole server: an M4A without an
// edit list whose JPEG covr precedes iTunSMPB is imported (its covr is
// the album's cover) and published with the cover kept in place. Removing
// the cover changes FFmpeg's decode of the file (the priming now trims the
// audio): the render's §9.1 step 6 comparison refuses it, the render job
// fails with render_audio_changed, the published output stays the
// previous one, untouched. Setting a cover again renders and publishes.
func TestM4ACoverRemovalResidual(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	dir := filepath.Join(p.imports, "Gapless")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(t.TempDir(), "raw.m4a")
	runTool(t, media.FFmpegPath, "-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "sine=frequency=440:duration=0.5:sample_rate=44100", "-ac", "2", "-c:a", "aac", "-use_editlist", "0",
		"-metadata", "title=Track 1", "-metadata", "artist=Wayne Shorter", "-metadata", "album=Speak No Evil",
		"-metadata", "track=1", raw)
	b, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	embedded := testJPEG(t, 24, 24, 7)
	if err := os.WriteFile(filepath.Join(dir, "01.m4a"), withCoverBeforeITunSMPB(t, b, embedded), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dbURL)
	d := startDaemon(t, cfg, p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	db := dbPool(t, dbURL)
	if _, err := catalogOn(t, db).CreateImportBatch(context.Background(), uuid.New(), ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the album to be published", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision > 0`) == 1
	})
	var id uuid.UUID
	var coverHash *string
	if err := db.QueryRow(context.Background(), `SELECT id, cover_hash FROM albums`).Scan(&id, &coverHash); err != nil {
		t.Fatal(err)
	}
	if coverHash == nil || *coverHash != sum(embedded) {
		t.Fatalf("the embedded covr is not the album's cover: %v", coverHash)
	}
	path := "/api/albums/" + id.String()
	lib := filepath.Join(p.data, "library", "Wayne Shorter", "Speak No Evil")
	before := hashTree(t, lib)
	a := published(t, db, id)

	r := d.mustAPI(t, http.MethodGet, path, "", nil, http.StatusOK)
	d.mustAPI(t, http.MethodDelete, path+"/cover", r.header.Get("ETag"), nil, http.StatusOK)
	waitFor(t, "the render to fail", func() bool {
		return queryInt(t, db, `SELECT count(*) FROM jobs WHERE kind = 'render' AND state = 'failed'`) == 1
	})
	st := d.mustAPI(t, http.MethodGet, path+"/status", "", nil, http.StatusOK)
	job, _ := st.body["job"].(map[string]any)
	if job == nil || job["error_code"] != render.CodeAudioChanged {
		t.Fatalf("status %v", st.body)
	}
	now := published(t, db, id)
	if now.PublishedRevision != a.PublishedRevision || *now.PublishedBuild != *a.PublishedBuild || now.Revision != a.Revision+1 {
		t.Fatalf("published %d/%v, revision %d", now.PublishedRevision, now.PublishedBuild, now.Revision)
	}
	if !sameTree(before, hashTree(t, lib)) {
		t.Fatal("the published album changed")
	}
	if ents, _ := os.ReadDir(filepath.Join(p.data, "work", "render")); len(ents) != 0 {
		t.Fatalf("work/render holds %d entries", len(ents))
	}

	// A cover again: rendered in the covr's place, published.
	r = d.mustAPI(t, http.MethodGet, path, "", nil, http.StatusOK)
	another := testJPEG(t, 30, 20, 9)
	d.upload(t, http.MethodPut, path+"/cover", r.header.Get("ETag"), another, http.StatusOK)
	waitFor(t, "the new cover to be published", func() bool {
		a := published(t, db, id)
		return idle(t, db) && a.PublishedRevision == a.Revision
	})
	if b, _ := os.ReadFile(filepath.Join(lib, "cover.jpg")); !bytes.Equal(b, another) {
		t.Fatal("cover.jpg is not the new cover")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM jobs`); n != 2 { // the scan and the import
		t.Fatalf("%d jobs left", n)
	}
}
