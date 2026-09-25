package http

import (
	"bytes"
	"io"
	"mime"
	nethttp "net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
)

// Downloads by entity id (§10.2, N-175): the bytes, the headers, and the
// ownership of every id by the album of the path.

// dl is a download's answer: its status, headers and raw bytes.
type dl struct {
	status int
	header nethttp.Header
	body   []byte
}

func (e *env) download(method, path string, headers ...string) dl {
	e.t.Helper()
	hr, err := nethttp.NewRequest(method, e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	hr.Host = testHost
	for i := 0; i+1 < len(headers); i += 2 {
		hr.Header.Set(headers[i], headers[i+1])
	}
	res, err := e.srv.Client().Do(hr)
	if err != nil {
		e.t.Fatal(err)
	}
	b, err := io.ReadAll(res.Body)
	if cerr := res.Body.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return dl{status: res.StatusCode, header: res.Header, body: b}
}

// wantFile checks a successful download: the bytes, the type, the
// disposition and its file name as a browser decodes it, nosniff and
// no-store.
func (e *env) wantFile(path string, want []byte, ctype, disposition, name string) {
	e.t.Helper()
	d := e.download("GET", path)
	if d.status != nethttp.StatusOK || !bytes.Equal(d.body, want) {
		e.t.Fatalf("GET %s: %d, %d bytes (want %d)", path, d.status, len(d.body), len(want))
	}
	h := d.header
	if h.Get("Content-Type") != ctype || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Cache-Control") != "no-store" || len(h.Values("Content-Disposition")) != 1 {
		e.t.Fatalf("GET %s: headers %v", path, h)
	}
	cd := h.Get("Content-Disposition")
	if strings.ContainsAny(cd, "\r\n") {
		e.t.Fatalf("GET %s: a control character in %q", path, cd)
	}
	kind, params, err := mime.ParseMediaType(cd)
	if err != nil || kind != disposition || params["filename"] != name {
		e.t.Fatalf("GET %s: Content-Disposition %q parses as %q %q (%v), want %s %q", path, cd, kind, params, err, disposition, name)
	}
	// HEAD: the same headers, no body.
	hd := e.download("HEAD", path)
	if hd.status != nethttp.StatusOK || len(hd.body) != 0 || hd.header.Get("Content-Disposition") != cd ||
		hd.header.Get("Content-Type") != ctype {
		e.t.Fatalf("HEAD %s: %d %v", path, hd.status, hd.header)
	}
}

func TestDownloads(t *testing.T) {
	e := newEnv(t)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	b := e.seedReal("Bill Evans", "Portrait in Jazz", catalog.FormatMP3)
	base := albumPath(a.id)

	// The original of a track, named as it was imported: quotes, a
	// backslash, CR/LF, a semicolon, a percent sign and non-ASCII come
	// back intact through filename*, and never break the header.
	e.wantFile(base+"/tracks/"+a.tracks[0].String()+"/original", a.audio[0], "audio/flac", "attachment",
		"01 \"So\" What\\ \r\nX-Evil: 1; %41 é ☃.flac")
	e.wantFile(albumPath(b.id)+"/tracks/"+b.tracks[1].String()+"/original", b.audio[1], "audio/mpeg", "attachment", "02 Freddie.flac")
	// Its lyrics: UTF-8 text, named after the track, as an attachment.
	e.wantFile(base+"/tracks/"+a.tracks[0].String()+"/lyrics", a.lrc, "text/plain; charset=utf-8", "attachment",
		"01 \"So\" What\\ \r\nX-Evil: 1; %41 é ☃.lrc")
	e.wantError(req{method: "GET", path: base + "/tracks/" + a.tracks[1].String() + "/lyrics"}, nethttp.StatusNotFound, CodeLyricsNotFound)
	// The cover: a validated JPEG, inline.
	e.wantFile(base+"/cover", a.cover, "image/jpeg", "inline", "cover.jpg")
	// Attachments: a PDF is a PDF, still an attachment; HTML is bytes; the
	// cover's JPEG, validated, is inline.
	e.wantFile(base+"/attachments/"+a.attachments["Scans/Booklet.pdf"].String()+"/content", a.booklet,
		"application/pdf", "attachment", "Booklet.pdf")
	e.wantFile(base+"/attachments/"+a.attachments["notes.html"].String()+"/content", []byte("<script>alert(1)</script>"),
		"application/octet-stream", "attachment", "notes.html")
	e.wantFile(base+"/attachments/"+a.attachments["cover.jpg"].String()+"/content", a.cover, "image/jpeg", "inline", "cover.jpg")
	// An image never validated (N-118) is not shown inline.
	img := pngImage(t, 5, 5)
	_, tag := e.album(a.id)
	r := e.must(upload("POST", base+"/attachments?path=Scans%2Fback.png", tag, img), nethttp.StatusCreated)
	e.wantFile(base+"/attachments/"+attachmentIDOf(t, r.body, "Scans/back.png")+"/content", img,
		"application/octet-stream", "attachment", "back.png")
	// An M4A original.
	m := e.seedReal("Wayne Shorter", "Speak No Evil", catalog.FormatM4AALAC)
	e.wantFile(albumPath(m.id)+"/tracks/"+m.tracks[1].String()+"/original", m.audio[1], "audio/mp4", "attachment", "02 Freddie.flac")

	// A range of the original (audio players seek).
	d := e.download("GET", base+"/tracks/"+a.tracks[1].String()+"/original", "Range", "bytes=10-19")
	if d.status != nethttp.StatusPartialContent || !bytes.Equal(d.body, a.audio[1][10:20]) {
		t.Fatalf("range: %d %d bytes", d.status, len(d.body))
	}

	// Every id belongs to the album of the path: another album's track,
	// attachment and lyrics are 404, as are unknown and malformed ids.
	for path, code := range map[string]string{
		base + "/tracks/" + b.tracks[0].String() + "/original":                                      catalog.CodeTrackNotFound,
		base + "/tracks/" + b.tracks[0].String() + "/lyrics":                                        catalog.CodeTrackNotFound,
		base + "/attachments/" + b.attachments["cover.jpg"].String() + "/content":                   catalog.CodeAttachmentNotFound,
		albumPath(b.id) + "/attachments/" + a.attachments["notes.html"].String() + "/content":       catalog.CodeAttachmentNotFound,
		base + "/tracks/" + uuid.NewString() + "/original":                                          catalog.CodeTrackNotFound,
		base + "/attachments/" + uuid.NewString() + "/content":                                      catalog.CodeAttachmentNotFound,
		base + "/attachments/" + strings.ToUpper(a.attachments["notes.html"].String()) + "/content": catalog.CodeAttachmentNotFound,
		base + "/tracks/%2e%2e/original":                                                            CodeNotFound,
		albumPath(uuid.New()) + "/cover":                                                            catalog.CodeAlbumNotFound,
		"/api/albums/x/tracks/" + a.tracks[0].String() + "/original":                                catalog.CodeAlbumNotFound,
	} {
		d := e.download("GET", path)
		if d.status != nethttp.StatusNotFound || !bytes.Contains(d.body, []byte(`"code":"`+code+`"`)) ||
			d.header.Get("Content-Disposition") != "" || !strings.HasPrefix(d.header.Get("Content-Type"), "application/json") {
			t.Fatalf("GET %s: %d %s %v", path, d.status, d.body, d.header)
		}
	}
	// A path that names a file is not a route.
	e.wantError(req{method: "GET", path: base + "/attachments/Scans/Booklet.pdf"}, nethttp.StatusNotFound, CodeNotFound)
}

// A blob the catalog references but the store does not hold is the
// store's damage (§11.3): 500, with nothing of the cause.
func TestDownloadMissingBlob(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Miles Davis", "Kind of Blue") // blobs in the catalog only
	d := e.download("GET", albumPath(id)+"/cover")
	if d.status != nethttp.StatusInternalServerError || !bytes.Contains(d.body, []byte(`"code":"internal"`)) ||
		bytes.Contains(d.body, []byte("originals")) || d.header.Get("Content-Disposition") != "" {
		t.Fatalf("%d %s %v", d.status, d.body, d.header)
	}
}

// RFC 6266 and RFC 8187: the file name never escapes its parameter.
func TestContentDisposition(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inline bool
		want   string
	}{
		{"Booklet.pdf", false, `attachment; filename="Booklet.pdf"; filename*=UTF-8''Booklet.pdf`},
		{"cover.png", true, `inline; filename="cover.png"; filename*=UTF-8''cover.png`},
		{`a"b\c.txt`, false, `attachment; filename="a_b_c.txt"; filename*=UTF-8''a%22b%5Cc.txt`},
		{"x\r\nSet-Cookie: a=b", false, `attachment; filename="x__Set-Cookie: a=b"; filename*=UTF-8''x%0D%0ASet-Cookie%3A%20a%3Db`},
		{"é ☃.flac", false, `attachment; filename="_ _.flac"; filename*=UTF-8''%C3%A9%20%E2%98%83.flac`},
		{"100%.txt;x=y", false, `attachment; filename="100_.txt;x=y"; filename*=UTF-8''100%25.txt%3Bx%3Dy`},
		{"", false, `attachment; filename="download"; filename*=UTF-8''download`},
		{"bad\xff", false, `attachment; filename="download"; filename*=UTF-8''download`},
		{"tab\there\x7f", false, `attachment; filename="tab_here_"; filename*=UTF-8''tab%09here%7F`},
	} {
		got := contentDisposition(tc.inline, tc.name)
		if got != tc.want {
			t.Errorf("%q: %s, want %s", tc.name, got, tc.want)
		}
		if strings.ContainsAny(got, "\r\n\x00\x7f") {
			t.Errorf("%q: a control character in %q", tc.name, got)
		}
		_, params, err := mime.ParseMediaType(got)
		if want := tc.name; err != nil || (want != "" && want != "bad\xff" && params["filename"] != want) {
			t.Errorf("%q: parses as %q, %v", tc.name, params, err)
		}
	}
}
