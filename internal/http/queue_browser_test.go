package http

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

func TestBrowserImportReportAndRetry(t *testing.T) {
	e, root := browserEnv(t)
	hostile := `Album <script>" &`
	if err := os.Mkdir(filepath.Join(e.imports, hostile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.imports, hostile, `file <script> &.txt`), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hostile, filepath.Join(e.imports, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(e.imports, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.imports, "bad\xff"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	tab, stop := chromedp.NewContext(root)
	defer stop()
	if err := chromedp.Run(tab, chromedp.Navigate(e.srv.URL+"/import")); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#source-entries')?.textContent.includes('Album <script>')`)
	// The unsafe entries are words, never links (N-271: their type is a data attribute).
	if got := browserEval(t, tab, `JSON.stringify([...document.querySelectorAll('#source-entries li[data-type]')].map(li=>li.dataset.type+':'+(li.querySelector('a')!==null)+':'+li.querySelector('.entry-kind').textContent).sort())`); got != `["invalid_name:false:Unreadable name, skipped","special:false:Special file, skipped","symlink:false:Link, not followed"]` {
		t.Fatal("unsafe source entry navigable: " + got)
	}
	if got := browserEval(t, tab, `String(document.querySelector('#source-entries script') === null)`); got != "true" {
		t.Fatal("unescaped source")
	}
	browserEval(t, tab, `document.querySelector('#source-entries a').click(); ''`)
	browserWait(t, tab, `document.querySelector('#source-path')?.textContent.includes('Album <script>')`)
	browserEval(t, tab, `document.querySelector('#import-form button').click(); ''`)
	browserWait(t, tab, `location.search.startsWith('?batch=')`)
	id := browserEval(t, tab, `new URLSearchParams(location.search).get('batch')`)
	if got := e.count(`SELECT count(*) FROM import_batches WHERE id=$1`, uuid.MustParse(id)); got != 1 {
		t.Fatalf("%d batches", got)
	}
	if got := browserEval(t, tab, `String(document.querySelector('#report script') === null)`); got != "true" {
		t.Fatal("unescaped report")
	}
	// Execute the real scan on the hostile source: no audio is a completed
	// failure with an unassigned-file warning, not an empty success.
	tools, err := media.NewTools(t.Context(), media.NewRunner(2), media.FFmpegPath, media.FFprobePath, media.TagsPath)
	if err != nil {
		t.Fatal(err)
	}
	im, err := importer.New(importer.Config{Catalog: e.svc, Tools: tools, Blobs: e.blobs, Source: e.source, Work: e.work, Budget: e.budget, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := jobs.ClaimNext(t.Context(), e.db, testRender)
	if err != nil || claim == nil || claim.Kind != jobs.KindScan {
		t.Fatalf("claim: %v %+v", err, claim)
	}
	if err := im.ExecuteScan(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#batch-state')?.textContent.includes('Finished')`)
	if got := browserEval(t, tab, `document.querySelector('#candidates').textContent`); !strings.Contains(got, "No albums found") {
		t.Fatal(got)
	}
	if got := browserEval(t, tab, `String(document.querySelector('#report script') === null && document.querySelector('#scan-warnings').textContent.includes('<script>'))`); got != "true" {
		t.Fatal("unescaped warning")
	}
	browserEval(t, tab, `window.reportFetches=0;const prior=window.fetch;window.fetch=(...args)=>{if(String(args[0]).startsWith('/api/imports/'))window.reportFetches++;return prior(...args)};''`)
	time.Sleep(4300 * time.Millisecond)
	if got := browserEval(t, tab, `String(window.reportFetches)`); got != "0" {
		t.Fatalf("completed report kept polling: %s", got)
	}
}

// A failed candidate stays editable while a second candidate keeps the report active.
func TestBrowserImportDraftSurvivesPolling(t *testing.T) {
	e, root := browserEnv(t)
	batch := e.newBatch()
	e.exec(`UPDATE jobs SET state='done' WHERE kind='scan' AND batch_id=$1`, batch)
	failed := e.importJob(batch, "in/failed", "failed")
	e.importJob(batch, "in/pending", "pending")
	tab, stop := chromedp.NewContext(root)
	defer stop()
	if err := chromedp.Run(tab, chromedp.Navigate(e.srv.URL+"/import?batch="+batch.String())); err != nil {
		t.Fatal(err)
	}
	artist := `#candidates input[name=artist]`
	title := `#candidates input[name=title]`
	browserWait(t, tab, `document.querySelector('#batch-state')?.textContent.includes('Importing') && document.querySelector('#candidates form') !== null`)
	if err := chromedp.Run(tab, chromedp.SendKeys(artist, "A careful override"), chromedp.SendKeys(title, "Another careful title")); err != nil {
		t.Fatal(err)
	}
	// Place the caret inside the title, not at the end: after the poll we must
	// be able to continue typing at exactly the same position.
	browserEval(t, tab, `const input=document.querySelector('#candidates input[name=title]');input.setSelectionRange(8,8);window.reportFetches=0;window.retryBodies=[];const originalFetch=window.fetch;window.fetch=(url,options)=>{if(String(url).startsWith('/api/imports/'))window.reportFetches++;if(String(url).endsWith('/retry'))window.retryBodies.push(JSON.parse(options.body));return originalFetch(url,options)};''`)
	time.Sleep(4500 * time.Millisecond)
	browserWait(t, tab, `window.reportFetches >= 2`)
	checkDraft := func() {
		t.Helper()
		if got := browserEval(t, tab, `String(document.querySelector('#candidates input[name=artist]')?.value==='A careful override' && document.querySelector('#candidates input[name=title]')?.value==='Another careful title' && document.activeElement===document.querySelector('#candidates input[name=title]') && document.activeElement.selectionStart===8 && document.activeElement.selectionEnd===8)`); got != "true" {
			t.Fatalf("poll lost draft, focus or caret: %s", got)
		}
	}
	checkDraft()
	// Exercise an unsuccessful background poll too; the alert must not steal
	// focus, and the next successful poll must still keep the draft.
	browserEval(t, tab, `const previousFetch=window.fetch;window.fetch=(url,options)=>{if(String(url).startsWith('/api/imports/') && !window.pollFailed){window.pollFailed=true;return Promise.resolve(new Response(JSON.stringify({code:'test_unavailable',message:'retry later'}),{status:503,headers:{'Content-Type':'application/json'}}))}return previousFetch(url,options)};''`)
	browserWait(t, tab, `window.pollFailed && !document.querySelector('#error').hidden`)
	checkDraft()
	browserWait(t, tab, `document.querySelector('#error').hidden && window.reportFetches >= 3`)
	checkDraft()
	if err := chromedp.Run(tab, chromedp.SendKeys(title, " steady")); err != nil {
		t.Fatal(err)
	}
	if got := browserEval(t, tab, `document.querySelector('#candidates input[name=title]').value`); got != "Another  steadycareful title" {
		t.Fatalf("typing after poll moved caret: %q", got)
	}
	browserEval(t, tab, `document.querySelector('#candidates form button').click();''`)
	browserWait(t, tab, `window.retryBodies.length===1 && document.querySelector('#candidates form')===null`)
	if got := browserEval(t, tab, `JSON.stringify(window.retryBodies[0])`); got != `{"artist":"A careful override","title":"Another  steadycareful title"}` {
		t.Fatalf("retry lost draft: %s", got)
	}
	if got := e.must(req{method: "GET", path: "/api/imports/" + batch.String()}, 200).body["candidates"].([]any); len(got) != 2 {
		t.Fatalf("report lost candidates: %v", got)
	}
	if e.count(`SELECT count(*) FROM jobs WHERE id=$1 AND state='pending' AND overrides->>'artist'='A careful override' AND overrides->>'title'='Another  steadycareful title'`, failed) != 1 {
		t.Fatal("retry did not persist the draft overrides")
	}
}

// A report captured before retry must not overwrite the new attempt, and the
// new attempt's normalized overrides must not be replaced by the old form.
func TestBrowserImportRetryInvalidatesPreviousAttempt(t *testing.T) {
	e, root := browserEnv(t)
	batch := e.newBatch()
	e.exec(`UPDATE jobs SET state='done' WHERE kind='scan' AND batch_id=$1`, batch)
	job := e.importJob(batch, "in/failed", "failed")
	e.importJob(batch, "in/pending", "pending") // Keep report polling active.
	tab, stop := chromedp.NewContext(root)
	defer stop()
	if err := chromedp.Run(tab, chromedp.Navigate(e.srv.URL+"/import?batch="+batch.String())); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#candidates form') !== null`)
	browserEval(t, tab, `const form=document.querySelector('#candidates form');form.elements.namedItem('artist').value=' Old draft ';form.elements.namedItem('title').value=' Old title ';
window.reportRequests=0;window.retryDelivered=false;window.oldDelivered=false;window.failRetry=true;
const realFetch=window.fetch;
window.fetch=async (url,options)=>{
  if(String(url).endsWith('/retry')) {
    if(window.failRetry) { window.failRetry=false;return new Response(JSON.stringify({code:'test_unavailable',message:'retry later'}),{status:503,headers:{'Content-Type':'application/json'}}); }
    const response=await realFetch(url,options);
    return new Promise(resolve=>{window.releaseRetry=()=>{window.retryDelivered=true;resolve(response)};});
  }
  if(String(url).startsWith('/api/imports/')) {
    window.reportRequests++;
    const afterRetry=window.retryDelivered;
    const response=await realFetch(url,options);
    const snapshot=new Response(await response.text(),{status:response.status,headers:response.headers});
    if(!afterRetry && !window.releaseOld) return new Promise(resolve=>{window.releaseOld=()=>{resolve(snapshot);setTimeout(()=>window.oldDelivered=true,0)};});
    if(afterRetry && !window.releaseFresh) return new Promise(resolve=>{window.releaseFresh=()=>resolve(snapshot);});
    return new Promise(()=>{}); // No later poll can change the assertion.
  }
  return realFetch(url,options);
};''`)
	browserEval(t, tab, `document.querySelector('#candidates form button').click();''`)
	browserWait(t, tab, `document.querySelector('#error').textContent.includes('test_unavailable')`)
	if got := browserEval(t, tab, `String(document.querySelector('#candidates input[name=artist]').value===' Old draft ' && document.querySelector('#candidates input[name=title]').value===' Old title ')`); got != "true" {
		t.Fatal("rejected retry lost the draft: " + got)
	}
	browserWait(t, tab, `typeof window.releaseOld==='function'`)
	browserEval(t, tab, `document.querySelector('#candidates form button').click();''`)
	browserWait(t, tab, `typeof window.releaseRetry==='function'`)
	if e.count(`SELECT count(*) FROM jobs WHERE id=$1 AND state='pending' AND overrides->>'artist'='Old draft' AND overrides->>'title'='Old title'`, job) != 1 {
		t.Fatal("retry did not persist normalized overrides")
	}
	// The retry response is still held in Chromium. Complete the new attempt
	// before the post-retry GET starts; its report must contain these values.
	e.exec(`UPDATE jobs SET state='failed', error_code='mixed_album', error_message='second attempt', updated_at=now() WHERE id=$1`, job)
	browserEval(t, tab, `window.releaseRetry();''`)
	browserWait(t, tab, `typeof window.releaseFresh==='function'`)
	browserEval(t, tab, `window.releaseFresh();''`)
	browserWait(t, tab, `document.querySelector('#candidates').textContent.includes('second attempt')`)
	checkNew := func() {
		t.Helper()
		if got := browserEval(t, tab, `String(document.querySelector('#candidates input[name=artist]')?.value==='Old draft' && document.querySelector('#candidates input[name=title]')?.value==='Old title' && document.querySelector('#candidates').textContent.includes('second attempt'))`); got != "true" {
			t.Fatal("previous attempt overwrote the new report: " + got)
		}
	}
	checkNew()
	// Deliver the pre-retry GET last: its old error and draft must be ignored.
	browserEval(t, tab, `window.releaseOld();''`)
	browserWait(t, tab, `window.oldDelivered`)
	checkNew()
}

func TestBrowserActivityActionsAndIdle(t *testing.T) {
	e, root := browserEnv(t)
	runningAlbum := e.seed("Running job", "Running album")
	running, err := jobs.ClaimNext(t.Context(), e.db, testRender)
	if err != nil || running == nil || running.Kind != jobs.KindRender {
		t.Fatalf("claim render: %v %+v", err, running)
	}
	album := e.seed("Browser job", "Activity album")
	batch := e.newBatch()
	failed := e.importJob(batch, "in/mixed", "failed")
	pending := e.importJob(batch, "in/pending", "pending")
	tab, stop := chromedp.NewContext(root)
	defer stop()
	if err := chromedp.Run(tab, chromedp.Navigate(e.srv.URL+"/activity")); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#jobs')?.textContent.includes('mixed_album')`)
	if got := browserEval(t, tab, `String([...document.querySelectorAll('#jobs li')].filter(li=>/pending|running/.test(li.querySelector('.badge')?.dataset.state)).every(li=>!li.querySelector('button')) && [...document.querySelectorAll('#jobs .badge')].filter(b=>/pending|running/.test(b.dataset.state)).length===`+strconv.Itoa(e.count(`SELECT count(*) FROM jobs WHERE state IN ('pending', 'running')`))+`)`); got != "true" {
		t.Fatal("nonfailed job has retry")
	}
	browserEval(t, tab, `document.querySelector('#jobs button').click(); ''`)
	browserWait(t, tab, `!document.querySelector('#jobs').textContent.includes('mixed_album')`)
	if n := e.count(`SELECT count(*) FROM jobs WHERE id=$1 AND state='pending'`, failed); n != 1 {
		t.Fatal("retry failed")
	}
	e.exec(`UPDATE jobs SET state='failed', error_code='render_io', error_message='disk' WHERE kind='render' AND album_id=$1`, album)
	other := e.importJob(batch, "in/other", "failed")
	before := e.count(`SELECT requested FROM jobs WHERE id=$1`, pending)
	runningID := running.Attempt.JobID
	runningRequested := e.count(`SELECT requested FROM jobs WHERE id=$1`, runningID)
	runningClaimed := e.count(`SELECT claimed FROM jobs WHERE id=$1`, runningID)
	browserEval(t, tab, `document.querySelector('#retry-failed').click(); ''`)
	browserWait(t, tab, `document.querySelector('#jobs')?.textContent.includes('in/other') && !document.querySelector('#jobs').textContent.includes('render_io') && !document.querySelector('#jobs').textContent.includes('mixed_album')`)
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind='render' AND album_id=$1 AND state='pending'`, album); n != 1 {
		t.Fatal("retry all failed")
	}
	if e.count(`SELECT count(*) FROM jobs WHERE id=$1 AND state='pending'`, other) != 1 || e.count(`SELECT requested FROM jobs WHERE id=$1`, pending) != before {
		t.Fatal("retry-failed changed a nonfailed job or missed a failed one")
	}
	if e.count(`SELECT count(*) FROM jobs WHERE id=$1 AND album_id=$2 AND state='running'`, runningID, runningAlbum) != 1 ||
		e.count(`SELECT requested FROM jobs WHERE id=$1`, runningID) != runningRequested ||
		e.count(`SELECT claimed FROM jobs WHERE id=$1`, runningID) != runningClaimed ||
		e.count(`SELECT count(*) FROM jobs WHERE kind='render' AND album_id=$1`, runningAlbum) != 1 {
		t.Fatal("retry-failed changed or duplicated running job")
	}
	if got := browserEval(t, tab, `String([...document.querySelectorAll('#jobs li')].filter(li=>li.querySelector('a[href="/albums/`+runningAlbum.String()+`"]')).length===1 && [...document.querySelectorAll('#jobs li')].filter(li=>li.querySelector('a[href="/albums/`+runningAlbum.String()+`"]')).every(li=>li.querySelector('.badge').dataset.state==='running'&&li.querySelector('.badge').textContent==='In progress'&&!li.querySelector('button')))`); got != "true" {
		t.Fatal("running job row gained retry or changed state: " + got)
	}
	e.exec(`DELETE FROM jobs`)
	browserWait(t, tab, `document.querySelector('#activity-state')?.textContent === 'Nothing in progress.'`)
	browserEval(t, tab, `window.jobFetches=0;const prior=window.fetch;window.fetch=(...args)=>{if(String(args[0]).startsWith('/api/jobs?'))window.jobFetches++;return prior(...args)};window.confirm=()=>false;document.querySelector('#render-all').click();''`)
	time.Sleep(4300 * time.Millisecond)
	if got := browserEval(t, tab, `String(window.jobFetches)`); got != "0" {
		t.Fatalf("idle polling: %s", got)
	}
	if n := e.count(`SELECT count(*) FROM jobs`); n != 0 {
		t.Fatal("cancelled render-all enqueued")
	}
	browserEval(t, tab, `window.confirm=()=>true;document.querySelector('#render-all').click();''`)
	browserWait(t, tab, `document.querySelector('#activity-state')?.textContent === 'Updating your library.'`)
}
