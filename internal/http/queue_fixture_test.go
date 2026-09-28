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

	"musiclib/internal/store"
)

// The Import and Activity views' fixture (round 21, NOTES.md N-286 to
// N-291): a music folder on ext4 with folders, tracks, images and a link;
// albums with real covers; an import with albums imported, already there
// and needing attention (the owner's own failures, N-285); and the queue
// in every state.

// batchJob inserts an import job of batch in state, as the scan and the
// import would have left it: failed with code and message, or done and
// skipped with their album.
func (e *env) batchJob(batch uuid.UUID, source, state, code, message string, album uuid.UUID) uuid.UUID {
	e.t.Helper()
	id := store.NewID()
	e.exec(`INSERT INTO jobs (id, kind, batch_id, source_rel, state, queued_at, updated_at)
		VALUES ($1, 'import', $2, $3, 'pending', now(), now())`, id, batch, source)
	switch state {
	case "failed":
		e.exec(`UPDATE jobs SET state = 'failed', error_code = $2, error_message = $3 WHERE id = $1`, id, code, message)
	case "done":
		e.exec(`UPDATE jobs SET state = 'done', result_album_id = $2,
			warnings = '[{"code":"tracks_renumbered","message":"tracks numbered by name order"}]' WHERE id = $1`, id, album)
	case "skipped":
		e.exec(`UPDATE jobs SET state = 'skipped', result_album_id = $2, error_code = 'duplicate_import',
			error_message = 'already imported' WHERE id = $1`, id, album)
	case "running":
		e.exec(`UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1`, id)
	}
	return id
}

// files writes empty files (and their folders) under /import: the views
// read names and types only.
func (e *env) files(names ...string) {
	e.t.Helper()
	for _, n := range names {
		p := filepath.Join(e.imports, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
}

// newImport is an import batch of root through the API, its scan done.
func (e *env) newImport(root string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	e.must(req{method: "POST", path: "/api/imports", body: importBody(id, root)}, 201)
	e.exec(`UPDATE jobs SET state = 'done', warnings = '[{"code":"unassigned_file","message":"not part of any album","path":"Jazz/notes.txt"}]'
		WHERE kind = 'scan' AND batch_id = $1`, id)
	return id
}

type queueFixture struct {
	batch                          uuid.UUID
	mixed, ambiguous, changed      uuid.UUID
	kindOfBlue, portrait, bitches  uuid.UUID
	running, pending, brokenRender uuid.UUID
}

func (e *env) queueFixture(t *testing.T) queueFixture {
	t.Helper()
	e.files("Jazz/Miles Davis/Kind of Blue/01 So What.flac", "Jazz/Miles Davis/Kind of Blue/02 Freddie Freeloader.flac",
		"Jazz/Miles Davis/Kind of Blue/03 Blue in Green.flac", "Jazz/Miles Davis/Kind of Blue/cover.jpg",
		"Jazz/Miles Davis/Kind of Blue/Scans/booklet.pdf", "Jazz/Miles Davis/Bitches Brew/CD1/01 Pharaoh's Dance.flac",
		"Jazz/Miles Davis/Bitches Brew/CD2/01 Spanish Key.flac", "Jazz/Miles Davis/notes.txt",
		"Jazz/Bill Evans/Portrait in Jazz/01 Come Rain or Come Shine.flac", "Jazz/Bill Evans/Portrait in Jazz/folder.jpg",
		"Jazz/Álvaro Soler/01 Sofia.mp3", "Jazz/Álvaro Soler/A Contracorriente/01 Ferrari.mp3",
		"Jazz/Chet Baker/Chet/01 Alone Together.m4a", "Rock/Radiohead/In Rainbows/01 15 Step.flac", "Rock/Radiohead/In Rainbows/cover.png")
	if err := os.Symlink("Jazz", filepath.Join(e.imports, "Old link")); err != nil {
		t.Fatal(err)
	}
	rgb := func(v uint32) color.RGBA { return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff} }
	album := func(artist, title string, bg, fg uint32) uuid.UUID {
		id := e.seed(artist, title)
		e.setCover(id, coverPNG(t, rgb(bg), rgb(fg), 0.38))
		return id
	}
	var f queueFixture
	f.kindOfBlue = album("Miles Davis", "Kind of Blue", 0x1f3a5f, 0xe8e2d0)
	f.bitches = album("Miles Davis", "Bitches Brew", 0x2b2b2b, 0xd4b483)
	f.portrait = album("Bill Evans", "Portrait in Jazz", 0xd9c7b8, 0x3b2a26)
	f.running = album("Radiohead", "In Rainbows", 0xf2a007, 0x1a1a1a)
	f.pending = album("Björk", "Homogenic", 0x8fa3a8, 0x2d3a3d)
	other := e.seed("Caetano Veloso", "Transa")
	e.exec(`UPDATE albums SET cover_hash = NULL WHERE id = $1`, other)
	f.brokenRender = album("Nina Simone", "Pastel Blues", 0xc94f3b, 0xf5e9dc)
	// The seed's own imports are not part of the story.
	e.exec(`DELETE FROM jobs WHERE kind <> 'render'`)
	e.exec(`DELETE FROM import_batches`)
	for _, id := range []uuid.UUID{f.kindOfBlue, f.bitches, f.portrait} {
		e.align(id)
	}
	e.runRender(f.running)
	e.failRender(f.brokenRender)
	e.exec(`UPDATE jobs SET queued_at = now() - interval '3 minutes', updated_at = now() - interval '2 minutes' WHERE kind = 'render'`)

	old := e.newImport("Rock")
	e.exec(`UPDATE import_batches SET created_at = now() - interval '3 days' WHERE id = $1`, old)
	e.batchJob(old, "Rock/Radiohead/In Rainbows", "done", "", "", f.running)

	f.batch = e.newImport("Jazz")
	e.batchJob(f.batch, "Jazz/Miles Davis/Kind of Blue", "done", "", "", f.kindOfBlue)
	e.batchJob(f.batch, "Jazz/Miles Davis/Bitches Brew", "done", "", "", f.bitches)
	e.batchJob(f.batch, "Jazz/Bill Evans/Portrait in Jazz", "skipped", "", "", f.portrait)
	f.mixed = e.batchJob(f.batch, "Jazz/Álvaro Soler/A Contracorriente", "failed", "mixed_album", `two album tags: "A Contracorriente", "Ferrari"`, uuid.Nil)
	f.ambiguous = e.batchJob(f.batch, "Jazz/Álvaro Soler", "failed", "ambiguous_candidate", "audio files here and further down: import its subdirectories separately", uuid.Nil)
	f.changed = e.batchJob(f.batch, "Jazz/Chet Baker/Chet", "failed", "source_changed", "01 Alone Together.m4a changed during the import", uuid.Nil)
	e.exec(`UPDATE jobs SET updated_at = now() - interval '4 minutes' WHERE kind = 'import'`)
	e.exec(`UPDATE import_batches SET created_at = now() - interval '5 minutes' WHERE id = $1`, f.batch)
	return f
}

// TestBrowserQueueScreenshots writes review screenshots of Import and
// Activity (N-293) when MUSICLIB_UI_SHOTS names a directory; it is a design
// review aid, not a gate, and is skipped otherwise.
func TestBrowserQueueScreenshots(t *testing.T) {
	dir := os.Getenv("MUSICLIB_UI_SHOTS")
	if dir == "" {
		t.Skip("MUSICLIB_UI_SHOTS is not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e, root := browserEnv(t)
	f := e.queueFixture(t)
	// A second import, still running, for «during an import».
	during := e.newImport("Rock")
	e.exec(`UPDATE jobs SET warnings = '[]' WHERE kind = 'scan' AND batch_id = $1`, during)
	e.batchJob(during, "Rock/Radiohead/In Rainbows", "done", "", "", f.running)
	e.batchJob(during, "Rock/Radiohead/OK Computer", "running", "", "", uuid.Nil)
	e.batchJob(during, "Rock/Radiohead/Kid A", "pending", "", "", uuid.Nil)
	empty, rootEmpty := browserEnv(t)
	shoot := func(root context.Context, url, name string, width, height int64, dark bool, then string) {
		t.Helper()
		tab, problems := browserTab(t, root)
		scheme, sidebar := "light", "expanded"
		if dark {
			scheme = "dark"
		}
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
		browserWait(t, tab, `document.fonts.status === 'loaded' && [...document.images].every(i => i.complete || !i.checkVisibility())`)
		if then != "" {
			browserEval(t, tab, then+`;''`)
			time.Sleep(500 * time.Millisecond)
		}
		if p := problems(); len(p) != 0 {
			t.Errorf("%s: console: %q", name, p)
		}
		var shot []byte
		if err := chromedp.Run(tab, chromedp.CaptureScreenshot(&shot)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%d-%s.png", name, width, scheme)), shot, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	results := e.srv.URL + "/import?batch=" + f.batch.String()
	for _, dark := range []bool{false, true} {
		for _, w := range []int64{1280, 390} {
			h := int64(900)
			if w < 600 {
				h = 844
			}
			shoot(root, e.srv.URL+"/import", "import-root", w, h, dark, "")
			shoot(root, e.srv.URL+"/import?path=Jazz%2FMiles+Davis", "import-browse", w, h, dark, "")
			shoot(root, e.srv.URL+"/import?path=Jazz%2FMiles+Davis%2FKind+of+Blue", "import-album", w, h, dark, "")
			shoot(root, e.srv.URL+"/import?batch="+during.String(), "import-during", w, h, dark, "")
			shoot(root, results, "import-attention", w, h, dark, "")
			shoot(root, results, "import-details", w, h, dark, `document.querySelectorAll('#rows-attention details').forEach((d,i)=>{if(i===0)d.open=true})`)
			shoot(root, results+"&tab=imported", "import-imported", w, h, dark, "")
			shoot(root, results+"&tab=present", "import-present", w, h, dark, "")
			shoot(rootEmpty, empty.srv.URL+"/import", "import-empty", w, h, dark, "")
			shoot(root, e.srv.URL+"/activity", "activity", w, h, dark, "")
			shoot(root, e.srv.URL+"/activity", "activity-advanced", w, h, dark, `document.querySelector('.advanced').open=true;document.querySelector('.advanced').scrollIntoView()`)
			shoot(rootEmpty, empty.srv.URL+"/activity", "activity-empty", w, h, dark, "")
			if w > 600 {
				shoot(root, e.srv.URL+"/activity", "activity-collapsed", w, h, dark, "")
				shoot(root, results, "import-collapsed", w, h, dark, "")
			}
		}
	}
}
