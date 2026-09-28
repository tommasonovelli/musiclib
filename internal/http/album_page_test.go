package http

import (
	"regexp"
	"strings"
	"testing"

	"musiclib/internal/catalog"
	"musiclib/internal/media"
)

// The album page as the server renders it (round 20, NOTES.md N-256 to
// N-267), on real PostgreSQL.

func TestCoverCandidate(t *testing.T) {
	jpeg, png, flac := catalog.FormatJPEG, catalog.FormatPNG, catalog.FormatFLAC
	for _, tc := range []struct {
		name   string
		format *string
		want   bool
	}{
		{"cover.jpg", &jpeg, true},
		{"scan.png", &png, true},
		// A known format wins over the name, either way.
		{"cover.jpg", &flac, false},
		{"notes.txt", &jpeg, true},
		// Without a format hint (an upload, N-118), the name decides.
		{"Scans/front.PNG", nil, true},
		{"Scans/back.jpeg", nil, true},
		{"Scans/back.JPG", nil, true},
		{"Scans/booklet.pdf", nil, false},
		{"image.gif", nil, false},
		{"image.webp", nil, false},
		{"jpg", nil, false},
		{"notes.jpg.txt", nil, false},
	} {
		if got := coverCandidate(attachmentJSON{RelPath: tc.name, Blob: blobJSON{Format: tc.format}}); got != tc.want {
			t.Errorf("%s (%v) = %v", tc.name, tc.format, got)
		}
	}
}

func TestCoverMB(t *testing.T) {
	flacJPEG, ok := media.MaxEmbeddedCover(media.FormatFLAC, media.FormatJPEG)
	if !ok || flacJPEG != 16_777_173 {
		t.Fatalf("the FLAC limit moved: %d", flacJPEG)
	}
	for _, tc := range []struct {
		formats []string
		want    int64
	}{
		{[]string{catalog.FormatFLAC}, 16},
		{[]string{catalog.FormatMP3, catalog.FormatFLAC}, 16},
		{[]string{catalog.FormatMP3}, 20},
		{[]string{catalog.FormatM4AAAC, catalog.FormatM4AALAC}, 20},
		{nil, 20},
	} {
		if got := coverMB(tc.formats); got != tc.want {
			t.Errorf("%v = %d, want %d", tc.formats, got, tc.want)
		}
	}
	// The words never promise more than the server takes.
	if coverMB(nil)*1_000_000 > MaxCoverBytes || coverMB([]string{catalog.FormatFLAC})*1_000_000 > flacJPEG {
		t.Fatal("the cover limit in words is above the real one")
	}
}

func TestAlbumPageMarkup(t *testing.T) {
	e := pageEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue(), map[string][]byte{
		"Scans/Booklet.pdf": []byte("%PDF-1.7\n"), "Scans/back.jpg": jpegImage(t, 8, 8), "Lyrics/So What.lrc": []byte("[00:01.00]x\n"),
	})
	_, etag := e.album(id)
	e.must(upload("POST", "/api/albums/"+id.String()+"/attachments?path=Scans/front.png", etag, pngImage(t, 8, 8)), 201)
	_, etag = e.album(id)
	e.must(upload("POST", "/api/albums/"+id.String()+"/attachments?path=notes.md", etag, []byte("# notes")), 201)
	e.runRender(id)
	_, _, body := pageRequest(t, e, "/albums/"+id.String(), testHost)
	// The cover picker: only images, as thumbnails.
	picker := regexp.MustCompile(`(?s)<dialog id="cover-picker".*?</dialog>`).FindString(body)
	names := regexp.MustCompile(`<span class="file-name">([^<]*)</span>`).FindAllStringSubmatch(picker, -1)
	var got []string
	for _, m := range names {
		got = append(got, m[1])
	}
	if strings.Join(got, ",") != "cover.jpg,Scans/back.jpg,Scans/front.png" || strings.Count(picker, "<img ") != 3 {
		t.Fatalf("picker: %v in %s", got, picker)
	}
	for _, want := range []string{
		// One markup, readonly until the module arms it.
		`<textarea id="album-title" name="title" rows="1" aria-label="Title" required readonly data-js>Kind of Blue</textarea>`,
		// English status word with the Library's dot (N-248).
		`data-status="Processing"><span class="dot" aria-hidden="true"></span>Updating</p>`,
		// The cover limit in plain words, for an album of FLAC tracks.
		`JPEG or PNG, up to 16 MB and 40 megapixels`,
		// Disc headings: more than one disc.
		`<h3 class="disc">Disc 1</h3>`, `<h3 class="disc">Disc 2</h3>`,
		// Inherited values are placeholders; «No genre» is explicit.
		`name="artist" value="" placeholder="Miles Davis"`,
		`name="artist" value="Miles Davis &amp; Wynton Kelly" placeholder="Miles Davis"`,
		`name="genre" value="" placeholder="Jazz"`, `name="genre" value="Modal jazz" placeholder="Jazz"`,
		`name="genre" value="" placeholder="No genre"`, `<input name="nogenre" type="checkbox" checked disabled data-js>No genre`,
		// The lyrics picker lists the .lrc attachments.
		`<span class="file-name">Lyrics/So What.lrc</span>`,
		// «Update in library» stays, in the album's menu (N-263).
		`data-method="POST" data-url="/api/albums/` + id.String() + `/render">Update in library</button>`,
		`data-ask="Move “Kind of Blue” to the trash? You can restore it.">Move to trash</button>`,
		// An active album leads back to the Library (N-281).
		`<a class="backlink" href="/">Library</a>`,
		// Downloads work without the module, and are not a navigation
		// that the unsaved-edits prompt would stop (N-280).
		`/original" download>Download original</a>`,
		`<a href="/api/albums/` + id.String() + `/tracks/`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("album page lacks %s", want)
		}
	}
	// The artists to choose from carry their ETags, for the rename.
	artist := e.artistOf(id)
	if !strings.Contains(body, `<option value="Miles Davis" data-id="`+artist.String()+`" data-etag="&#34;artist:`+artist.String()+`:1&#34;">`) {
		t.Errorf("artist option: %s", regexp.MustCompile(`<datalist.*?</datalist>`).FindString(body))
	}
	if strings.Contains(body, `class="band"`) || strings.Contains(body, `<header class="page-head">`) {
		t.Error("an active album has no trash band, and the page draws its own head")
	}

	// A single disc has no heading; a trashed album has its band and no
	// «Move to trash».
	single := e.seedAlbum("Bill Evans", "Portrait", kindOfBlue()[:2], nil)
	_, etag = e.album(single)
	e.must(req{method: "DELETE", path: "/api/albums/" + single.String(), ifMatch: etag}, 200)
	_, _, body = pageRequest(t, e, "/albums/"+single.String(), testHost)
	if strings.Contains(body, `class="disc"`) || !strings.Contains(body, `<p class="band">This album is in the trash.<button`) || !strings.Contains(body, `<a class="backlink" href="/?trash=true">Trash</a>`) || strings.Contains(body, "Move to trash") || !strings.Contains(body, `aria-current="page"><svg class="icon" width="20" height="20" aria-hidden="true"><use href="#i-trash"/></svg><span class="nav-label">Trash</span>`) {
		t.Errorf("trashed single-disc page: %s", body)
	}
}
