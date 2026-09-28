package http

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// The album editor in a real Chromium against the real server and
// PostgreSQL (round 20, NOTES.md N-256 to N-267).

// fixtureTrack is one track of seedAlbum. lyrics imports an LRC with it;
// ms is its duration (N-300), nil for unknown.
type fixtureTrack struct {
	disc, no      int
	title         string
	artist, genre *string
	lyrics        bool
	ms            *int64
}

// seedAlbum imports an album of real (random, non-audio) FLAC blobs through
// the catalog's import commit, with a real JPEG cover kept as cover.jpg and
// the given extra attachments (name → bytes; a .jpg or .png is recorded as
// an image, as the importer would).
func (e *env) seedAlbum(artist, title string, tracks []fixtureTrack, extras map[string][]byte) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	cover := e.put(jpegImage(e.t, 32, 32))
	cover.Format = catalog.FormatJPEG
	blobs := []catalog.Blob{cover}
	var its []catalog.ImportTrack
	for _, t := range tracks {
		b := e.put(randomBytes(2000 + t.no))
		b.Format, b.DurationMS = catalog.FormatFLAC, t.ms
		blobs = append(blobs, b)
		it := catalog.ImportTrack{SourcePath: fmt.Sprintf("CD%d/%02d %s.flac", t.disc, t.no, t.title), Disc: t.disc, No: t.no,
			Title: t.title, Artist: t.artist, Genre: t.genre, BlobHash: b.Hash}
		if t.lyrics {
			l := e.put([]byte("[00:01.00]" + t.title + "\n"))
			blobs = append(blobs, l)
			it.Lyrics = &catalog.ImportLyrics{SourcePath: fmt.Sprintf("CD%d/%02d %s.lrc", t.disc, t.no, t.title), BlobHash: l.Hash}
		}
		its = append(its, it)
	}
	atts := []catalog.ImportAttachment{{RelPath: "cover.jpg", BlobHash: cover.Hash}}
	for name, data := range extras {
		b := e.put(data)
		switch strings.ToLower(filepath.Ext(name)) {
		case ".jpg":
			b.Format = catalog.FormatJPEG
		case ".png":
			b.Format = catalog.FormatPNG
		}
		blobs = append(blobs, b)
		atts = append(atts, catalog.ImportAttachment{RelPath: name, BlobHash: b.Hash})
	}
	batch, job := store.NewID(), store.NewID()
	e.exec(`INSERT INTO import_batches (id, root_rel, created_at) VALUES ($1, 'in', now())`, batch)
	e.exec(`INSERT INTO jobs (id, kind, batch_id, source_rel, state, queued_at, updated_at)
		VALUES ($1, 'import', $2, $3, 'pending', now(), now())`, job, batch, "src-"+job.String())
	var ticket int64
	if err := e.db.QueryRow(ctx, `UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1 RETURNING claimed`,
		job).Scan(&ticket); err != nil {
		e.t.Fatal(err)
	}
	out, err := e.svc.CommitImport(ctx, catalog.ImportCandidate{
		Attempt: jobs.Attempt{JobID: job, Ticket: ticket}, Fingerprint: newHash(), Blobs: blobs,
		Artist: artist, Title: title, Year: ptr(1959), Genre: ptr("Jazz"), CoverHash: &cover.Hash,
		Tracks: its, Attachments: atts,
	})
	if err != nil || out.State != jobs.StateDone {
		e.t.Fatalf("CommitImport: %+v %v", out, err)
	}
	return out.AlbumID
}

// kindOfBlue is the screenshot and behaviour fixture: two discs, a
// track artist, an explicit «no genre», lyrics on the first track, the
// record's durations and one still unknown (the alternate take).
func kindOfBlue() []fixtureTrack {
	return []fixtureTrack{
		{disc: 1, no: 1, title: "So What", lyrics: true, ms: ptr(int64(562_000))},
		{disc: 1, no: 2, title: "Freddie Freeloader", artist: ptr("Miles Davis & Wynton Kelly"), ms: ptr(int64(585_600))},
		{disc: 1, no: 3, title: "Blue in Green", ms: ptr(int64(337_499))},
		{disc: 1, no: 4, title: "All Blues", genre: ptr("Modal jazz"), ms: ptr(int64(693_000))},
		{disc: 1, no: 5, title: "Flamenco Sketches", ms: ptr(int64(566_000))},
		{disc: 2, no: 1, title: "Flamenco Sketches (Alternate Take)", genre: ptr("")},
	}
}

// albumTab opens the album page in a new tab that records console errors
// and CSP violations, and waits until the editor is armed.
func albumTab(t *testing.T, root context.Context, url string) (context.Context, func() []string) {
	t.Helper()
	tab, problems := browserTab(t, root)
	browserGo(t, tab, url)
	browserWait(t, tab, `document.querySelector('#album-title') && !document.querySelector('#album-title').readOnly`)
	return tab, problems
}

// TestBrowserAlbumScreenshots writes review screenshots of the album
// editor (default, dirty with the Save bar, conflict, trash band, ⋯ menu
// open) in light and dark, at 1280 and 390 px, and dirty with the sidebar
// collapsed at 1280 px, when MUSICLIB_UI_SHOTS names
// a directory (N-252). Skipped otherwise.
func TestBrowserAlbumScreenshots(t *testing.T) {
	dir := os.Getenv("MUSICLIB_UI_SHOTS")
	if dir == "" {
		t.Skip("MUSICLIB_UI_SHOTS is not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue(), map[string][]byte{
		"Scans/Booklet.pdf": []byte("%PDF-1.7\n"), "Scans/back.jpg": jpegImage(t, 40, 40), "01 So What.lrc": []byte("[00:01.00]So what\n"),
	})
	e.setCover(id, coverPNG(t, color.RGBA{0x1f, 0x3a, 0x5f, 0xff}, color.RGBA{0xe8, 0xe2, 0xd0, 0xff}, 0.38))
	e.align(id)
	gone := e.seedAlbum("Nina Simone", "Pastel Blues", kindOfBlue()[:3], nil)
	e.setCover(gone, coverPNG(t, color.RGBA{0xc9, 0x4f, 0x3b, 0xff}, color.RGBA{0xf5, 0xe9, 0xdc, 0xff}, 0.38))
	_, etag := e.album(gone)
	e.must(req{method: "DELETE", path: "/api/albums/" + gone.String(), ifMatch: etag}, 200)
	// Archived: the removal is done, nothing to show but the band.
	e.exec(`DELETE FROM jobs WHERE kind = 'render' AND album_id = $1`, gone)
	url := e.srv.URL + "/albums/" + id.String()
	edit := `(()=>{const t=document.querySelector('#album-title');t.value='Kind of Blue (Legacy Edition)';t.dispatchEvent(new Event('input',{bubbles:true}));const a=document.querySelectorAll('[name=artist]')[2];a.value='Bill Evans';a.dispatchEvent(new Event('input',{bubbles:true}));})()`
	shoot := func(url, name string, width, height int64, dark bool, then string) {
		t.Helper()
		tab, problems := browserTab(t, root)
		scheme := "light"
		if dark {
			scheme = "dark"
		}
		// The sidebar is expanded but in the "-collapsed" shots (N-272).
		sidebar := "expanded"
		if strings.HasSuffix(name, "-collapsed") {
			sidebar = "collapsed"
		}
		err := chromedp.Run(tab,
			emulation.SetDeviceMetricsOverride(width, height, 2, width < 600),
			emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: scheme}, {Name: "prefers-reduced-motion", Value: "reduce"}}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				_, err := page.AddScriptToEvaluateOnNewDocument(`try{localStorage.setItem('musiclib.sidebar','` + sidebar + `')}catch{}`).Do(ctx)
				return err
			}),
			chromedp.Navigate(url),
		)
		if err != nil {
			t.Fatal(err)
		}
		browserWait(t, tab, `document.fonts.status === 'loaded' && [...document.images].every(i => i.complete || !i.checkVisibility()) && !document.querySelector('#album-title').readOnly`)
		if then != "" {
			browserEval(t, tab, then+`;''`)
			time.Sleep(700 * time.Millisecond)
		}
		if p := problems(); len(p) != 0 && name != "conflict" {
			t.Errorf("%s: console: %q", name, p)
		}
		var shot []byte
		if err := chromedp.Run(tab, chromedp.CaptureScreenshot(&shot)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("album-%s-%d-%s.png", name, width, scheme)), shot, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, dark := range []bool{false, true} {
		for _, w := range []int64{1280, 390} {
			h := int64(900)
			if w < 600 {
				h = 844
			}
			shoot(url, "default", w, h, dark, "")
			shoot(url, "dirty", w, h, dark, edit)
			// Another window saves first: this one meets the 412.
			shoot(url, "conflict", w, h, dark, edit+`;fetch('/api/albums/`+id.String()+`').then(r=>r.json()).then(a=>fetch('/api/albums/`+id.String()+`',{method:'PUT',headers:{'X-Musiclib-Request':'1','If-Match':a.etag,'Content-Type':'application/json'},body:JSON.stringify({artist_id:a.artist_id,new_artist:null,title:a.title,year:a.year===1959?1960:1959,genre:a.genre,compilation:a.compilation,tracks:a.tracks.map(t=>({id:t.id,disc:t.disc,no:t.no,title:t.title,artist:t.artist,genre:t.genre}))})})).then(()=>document.querySelector('#save').click())`)
			shoot(url, "menu", w, h, dark, `document.querySelectorAll('.track .dots')[1].click()`)
			// Round 22: the track table with its durations and the total
			// line (N-302), and a new artist staged (N-298).
			shoot(url, "tracks", w, h, dark, `document.querySelector('#tracks-title').scrollIntoView()`)
			shoot(url, "staged", w, h, dark, `(()=>{const a=document.querySelector('#artist-name');a.value='Cannonball Adderley';a.dispatchEvent(new Event('input',{bubbles:true}));document.querySelector('#create-artist').click()})()`)
			shoot(e.srv.URL+"/albums/"+gone.String(), "trash", w, h, dark, "")
			if w > 600 {
				// The Save bar starts at the collapsed sidebar's edge.
				shoot(url, "dirty-collapsed", w, h, dark, edit)
			}
		}
	}
}
