package http

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// The Import view in a real Chromium against the real server, PostgreSQL
// and importer (round 21, NOTES.md N-286 to N-291).

// testImporter is the real importer over the test's /import.
func (e *env) testImporter(t *testing.T) *importer.Importer {
	t.Helper()
	tools, err := media.NewTools(t.Context(), media.NewRunner(2), media.FFmpegPath, media.FFprobePath, media.TagsPath)
	if err != nil {
		t.Fatal(err)
	}
	im, err := importer.New(importer.Config{Catalog: e.svc, Tools: tools, Blobs: e.blobs, Source: e.source, Work: e.work, Budget: e.budget, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return im
}

// runNext claims the next job, which must be of kind, and executes it with
// the real importer; the pending renders are parked so that they do not
// come first.
func (e *env) runNext(t *testing.T, im *importer.Importer, kind jobs.Kind) {
	t.Helper()
	e.exec(`UPDATE jobs SET state='failed', error_code='test_parked', error_message='parked by test' WHERE kind='render' AND state='pending'`)
	c, err := jobs.ClaimNext(t.Context(), e.db, testRender)
	if err != nil || c == nil || c.Kind != kind {
		t.Fatalf("claim %v: %+v", err, c)
	}
	switch kind {
	case jobs.KindScan:
		err = im.ExecuteScan(context.Background(), c)
	case jobs.KindImport:
		err = im.ExecuteImport(context.Background(), c)
	}
	if err != nil {
		t.Fatalf("execute %s: %v", kind, err)
	}
}

func flac(t *testing.T, path, album string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=0.15", "-c:a", "flac", "-metadata", "album=" + album, "-metadata", "artist=Browser artist", "-y", path}
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

// Browsing the music folder: folders as rows with what they hold, the
// breadcrumb, the keyboard, the entries never followed, escaping; the same
// pages without JavaScript; the empty and missing folders.
func TestBrowserImportBrowse(t *testing.T) {
	e, root := browserEnv(t)
	hostile := `Album <script>" &`
	e.files("Jazz/Miles Davis/Kind of Blue/01 So What.flac", "Jazz/Miles Davis/Kind of Blue/02.flac", "Jazz/Miles Davis/Kind of Blue/cover.jpg",
		"Jazz/Miles Davis/Kind of Blue/booklet.pdf", "Jazz/Bill Evans/x.mp3", hostile+"/file <script> &.txt", "Jazz/readme.txt")
	if err := os.Symlink("Jazz", filepath.Join(e.imports, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(e.imports, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.imports, "bad\xff"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tab, problems := browserTab(t, root)
	browserGo(t, tab, e.srv.URL+"/import")
	rows := `JSON.stringify([...document.querySelectorAll('.folders > li')].map(li=>[li.querySelector('.folder-name').textContent, li.querySelector('.folder-sum').textContent, li.querySelector('a')?.getAttribute('href') ?? null]))`
	browserWait(t, tab, `document.querySelector('#import-start') && !document.querySelector('#import-start').hidden`)
	if got := browserEval(t, tab, rows); got != `[["Album <script>\" &","1 other file","/import?path=Album+%3Cscript%3E%22+%26"],["Jazz","2 folders, 1 other file","/import?path=Jazz"],["bad�","Unreadable name, skipped",null],["link","Link, not followed",null],["pipe","Special file, skipped",null]]` {
		t.Fatalf("the music folder: %s", got)
	}
	if got := browserEval(t, tab, `document.querySelector('.crumbs').textContent+'|'+document.querySelector('#import-start').textContent+'|'+document.querySelectorAll('main script').length`); got != "Music folder|Import everything in the music folder|0" {
		t.Fatalf("root: %s", got)
	}
	// The keyboard: Tab to the first folder, arrows between folders, Enter opens.
	browserEval(t, tab, `document.querySelector('a.folder-row').focus();''`)
	browserKey(t, tab, kb.ArrowDown)
	if got := browserEval(t, tab, `document.activeElement.querySelector('.folder-name').textContent`); got != "Jazz" {
		t.Fatalf("arrow down: %s", got)
	}
	browserKey(t, tab, kb.Enter)
	browserWait(t, tab, `location.search === '?path=Jazz'`)
	browserEval(t, tab, `[...document.querySelectorAll('a.folder-row')].find(a=>a.textContent.includes('Miles Davis')).click();''`)
	browserWait(t, tab, `location.search.startsWith('?path=Jazz%2FMiles')`)
	browserEval(t, tab, `document.querySelector('a.folder-row').click();''`)
	browserWait(t, tab, `document.querySelector('.crumbs [aria-current]')?.textContent === 'Kind of Blue'`)
	if got := browserEval(t, tab, `JSON.stringify([[...document.querySelectorAll('.crumbs a')].map(a=>a.textContent+'='+a.getAttribute('href')), document.querySelector('.folder-files').textContent, document.querySelectorAll('.folders').length, document.querySelector('#import-start').textContent, document.querySelector('.start-note').textContent])`); got != `[["Music folder=/import","Jazz=/import?path=Jazz","Miles Davis=/import?path=Jazz%2FMiles+Davis"],"2 tracks, 1 image, 1 other file",0,"Import everything in Kind of Blue","The originals are never changed. Don’t move them until the import has finished."]` {
		t.Fatalf("an album folder: %s", got)
	}
	// Up again through the breadcrumb.
	browserEval(t, tab, `document.querySelectorAll('.crumbs a')[1].click();''`)
	browserWait(t, tab, `location.search === '?path=Jazz'`)
	if got := browserEval(t, tab, `JSON.stringify(window.cspViolations)`); got != "[]" {
		t.Fatalf("CSP violations: %s", got)
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}

	// Without JavaScript: the same links, no button, a sentence instead.
	plain, _ := browserTab(t, root)
	if err := chromedp.Run(plain, emulation.SetScriptExecutionDisabled(true)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, plain, e.srv.URL+"/import")
	if got := browserEval(t, plain, rows); !strings.Contains(got, `["Jazz","2 folders, 1 other file","/import?path=Jazz"]`) {
		t.Fatalf("no-JS rows: %s", got)
	}
	if err := chromedp.Run(plain, chromedp.Click(`a.folder-row[href="/import?path=Jazz"]`, chromedp.NodeVisible)); err != nil {
		t.Fatal(err)
	}
	browserWait(t, plain, `location.search === '?path=Jazz' && document.querySelector('.crumbs [aria-current]').textContent === 'Jazz'`)
	if got := browserEval(t, plain, `String(document.querySelector('#import-start').checkVisibility())+'|'+document.querySelector('noscript').textContent`); !strings.HasPrefix(got, "false|") || !strings.Contains(got, "Turn on JavaScript to import.") {
		t.Fatalf("no-JS button: %s", got)
	}

	// A folder that is gone, and a link: one sentence, the code in Details.
	browserGo(t, tab, e.srv.URL+"/import?path=Gone")
	if got := browserEval(t, tab, `document.querySelector('main .notice p').textContent+'|'+document.querySelector('main .notice details').open+'|'+document.querySelector('main .notice pre').textContent.startsWith('source_not_found')+'|'+(document.querySelector('#import-start')===null)`); got != "This folder isn’t there any more.|false|true|true" {
		t.Fatalf("a missing folder: %s", got)
	}
	browserGo(t, tab, e.srv.URL+"/import?path=link")
	if got := browserEval(t, tab, `document.querySelector('main .notice p').textContent`); got != "This is a link, and links aren’t followed." {
		t.Fatalf("a link: %s", got)
	}
	// An empty music folder.
	empty, emptyRoot := browserEnv(t)
	et, _ := browserTab(t, emptyRoot)
	browserGo(t, et, empty.srv.URL+"/import")
	if got := browserEval(t, et, `document.querySelector('.empty').textContent+'|'+(document.querySelector('#import-start')===null)+'|'+(document.querySelector('.recent')===null)`); got != "Your music folder is empty.Put your albums in it, then reload this page.|true|true" {
		t.Fatalf("an empty music folder: %s", got)
	}
}

// The browser starts the import of the open folder; the real importer scans
// and imports; the progress row, then the results in tabs that open on the
// first one that is not empty; a row needing attention says its problem
// and offers its one fix; the title override fixes it.
func TestBrowserImportAndOverride(t *testing.T) {
	e, root := browserEnv(t)
	dir := filepath.Join(e.imports, "Source")
	flac(t, filepath.Join(dir, "Good/01.flac"), "Good")
	flac(t, filepath.Join(dir, "Mixed/01.flac"), "First")
	flac(t, filepath.Join(dir, "Mixed/02.flac"), "Second")
	flac(t, filepath.Join(dir, "Ambiguous/01.flac"), "Ambiguous")
	flac(t, filepath.Join(dir, "Ambiguous/Bonus/02.flac"), "Ambiguous")
	if err := os.WriteFile(filepath.Join(dir, "unassigned <script> &.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	im := e.testImporter(t)
	tab, problems := browserTab(t, root)
	browserGo(t, tab, e.srv.URL+"/import")
	browserWait(t, tab, `document.querySelector('a.folder-row')?.textContent.includes('Source')`)
	browserEval(t, tab, `document.querySelector('a.folder-row').click();''`)
	browserWait(t, tab, `document.querySelector('#import-start')?.textContent === 'Import everything in Source' && !document.querySelector('#import-start').hidden`)
	browserEval(t, tab, `document.querySelector('#import-start').click();''`)
	browserWait(t, tab, `location.search.startsWith('?batch=')`)
	id := uuid.MustParse(browserEval(t, tab, `new URLSearchParams(location.search).get('batch')`))
	if n := e.count(`SELECT count(*) FROM import_batches WHERE id=$1 AND root_rel='Source'`, id); n != 1 {
		t.Fatalf("%d batches", n)
	}
	// While the scan runs: one progress row, no tabs, the activity dot.
	if got := browserEval(t, tab, `document.querySelector('#progress').textContent+'|'+document.querySelector('#tabs').hidden+'|'+document.querySelector('#nav-active').hidden`); got != "Looking for albums in Source…|true|false" {
		t.Fatalf("scanning: %s", got)
	}
	e.runNext(t, im, jobs.KindScan)
	browserWait(t, tab, `document.querySelector('#progress')?.textContent === 'Importing album 2 of 3' && !document.querySelector('#tabs').hidden`)
	// The ambiguous branch failed at the scan: it is there already, and the
	// view opens on it.
	if got := browserEval(t, tab, `document.querySelector('[aria-selected=true]').id+'|'+document.querySelector('#count-attention').textContent`); got != "tab-attention|1" {
		t.Fatalf("first results: %s", got)
	}
	e.runNext(t, im, jobs.KindImport)
	e.runNext(t, im, jobs.KindImport)
	browserWait(t, tab, `document.querySelector('#progress').hidden && document.querySelector('#nav-active').hidden && document.querySelector('#count-attention').textContent === '2' && document.querySelector('#count-imported').textContent === '1'`)
	rows := `JSON.stringify([...document.querySelectorAll('#rows-attention > li')].map(li=>[li.querySelector('.row-title').textContent, li.querySelector('.row-sentence').textContent, [...li.querySelectorAll('.row-actions button, .row-actions input:not([type=hidden])')].map(c=>c.textContent||c.name), li.querySelector(':scope > details').open, li.querySelector(':scope > details pre').textContent.split(':')[0]]))`
	if got := browserEval(t, tab, rows); got != `[["Ambiguous","This folder has tracks, and more tracks in its subfolders. Import the subfolders one by one.",["Dismiss"],false,"ambiguous_candidate"],["Mixed","The tracks have different album names. Give the album one title.",["title","Import with this title","Dismiss"],false,"mixed_album"]]` {
		t.Fatalf("rows needing attention: %s", got)
	}
	if got := browserEval(t, tab, `String(document.querySelector('#skipped summary').textContent==='1 file wasn’t imported' && document.querySelector('#skipped li').textContent.includes('<script>') && document.querySelector('main script')===null)`); got != "true" {
		t.Fatal("the file without an album is not listed, or not escaped")
	}
	// The title fixes mixed_album; the retry carries exactly the two
	// overrides of §7.3, the real executor revalidates the source.
	browserEval(t, tab, `window.retryBodies=[];const realFetch=window.fetch;window.fetch=(url,opts)=>{if(String(url).endsWith('/retry'))window.retryBodies.push(JSON.parse(opts.body));return realFetch(url,opts)};''`)
	if err := chromedp.Run(tab, chromedp.SendKeys(`#rows-attention form.fix input[name=title]`, "Fixed title"), chromedp.Click(`#rows-attention form.fix button`)); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `window.retryBodies.length===1 && document.querySelector('#count-attention').textContent === '1' && !document.querySelector('#nav-active').hidden`)
	if got := browserEval(t, tab, `JSON.stringify(window.retryBodies[0])`); got != `{"artist":null,"title":"Fixed title"}` {
		t.Fatalf("retry body: %s", got)
	}
	e.runNext(t, im, jobs.KindImport)
	browserWait(t, tab, `document.querySelector('#count-imported').textContent === '2' && document.querySelector('#nav-active').hidden`)
	browserEval(t, tab, `document.querySelector('#tab-imported').click();''`)
	browserWait(t, tab, `!document.querySelector('#panel-imported').hidden && location.search.endsWith('tab=imported')`)
	if got := browserEval(t, tab, `JSON.stringify([...document.querySelectorAll('#rows-imported .row-title')].map(n=>n.textContent).sort())+'|'+document.querySelector('#rows-imported a').getAttribute('href').startsWith('/albums/')`); got != `["Fixed title","Good"]|true` {
		t.Fatalf("imported: %s", got)
	}
	if n := e.count(`SELECT count(*) FROM albums WHERE title='Fixed title'`); n != 1 {
		t.Fatalf("override album count %d", n)
	}
	// The last problem dismissed: the empty tab's sentence goes in, and it
	// was not in the page while the tab had rows (N-296).
	if got := browserEval(t, tab, `String(document.querySelector('#empty-attention'))`); got != "null" {
		t.Fatalf("an empty-tab sentence beside rows: %s", got)
	}
	browserEval(t, tab, `document.querySelector('#rows-attention [data-dismiss]').click();''`)
	browserWait(t, tab, `document.querySelector('#count-attention').textContent === '0' && document.querySelector('#rows-attention + #empty-attention')?.textContent === 'Nothing needs attention.'`)
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}
}

// A lost answer: the same request id is sent again, one import is made. A
// refusal says one sentence, its code closed in «Details».
func TestBrowserImportLostAnswerAndConflict(t *testing.T) {
	e, root := browserEnv(t)
	e.files("Rock/01.flac")
	tab, _ := browserTab(t, root)
	browserGo(t, tab, e.srv.URL+"/import?path=Rock")
	browserWait(t, tab, `!document.querySelector('#import-start').hidden`)
	// Deliver the POST to the server and discard its answer once; the second
	// answer is a 409, as if the id had been used for another folder.
	browserEval(t, tab, `window.posts=[];const send=window.fetch;window.fetch=async (url,opts)=>{if(url==='/api/imports'){window.posts.push(JSON.parse(opts.body));sessionStorage.setItem('posts',JSON.stringify(window.posts));const result=await send(url,opts);if(window.posts.length===1)throw Error('lost answer');if(window.posts.length===2)return new Response(JSON.stringify({code:'import_batch_conflict',message:'request is already the import of "x"',details:{path:'x'}}),{status:409,headers:{'Content-Type':'application/json'}});return result}return send(url,opts)};document.querySelector('#import-start').click();''`)
	notice := `document.querySelector('.start .notice')`
	browserWait(t, tab, notice+`?.textContent.startsWith('The library isn’t responding.') && document.activeElement === `+notice)
	if got := browserEval(t, tab, notice+`.querySelector('details').hidden+'|'+document.querySelector('#import-start').disabled`); got != "true|false" {
		t.Fatalf("network error: %s", got)
	}
	browserEval(t, tab, `document.querySelector('#import-start').click();''`)
	browserWait(t, tab, notice+`.querySelector('p').textContent === 'The import didn’t start. Try again.' && !`+notice+`.querySelector('details').hidden`)
	if got := browserEval(t, tab, notice+`.querySelector('details').open+'|'+`+notice+`.querySelector('p').textContent.includes('import_batch')+'|'+`+notice+`.querySelector('pre').textContent.includes('import_batch_conflict')`); got != "false|false|true" {
		t.Fatalf("refusal: %s", got)
	}
	browserEval(t, tab, `document.querySelector('#import-start').click();''`)
	browserWait(t, tab, `location.search.startsWith('?batch=')`)
	if got := browserEval(t, tab, `(()=>{const posts=JSON.parse(sessionStorage.getItem('posts'));return String(posts.length===3 && posts.every(p=>p.id===posts[0].id && p.path==='Rock'))})()`); got != "true" {
		t.Fatalf("request UUID not retained: %s", got)
	}
	if n := e.count(`SELECT count(*) FROM import_batches`); n != 1 {
		t.Fatalf("%d batches", n)
	}
}
