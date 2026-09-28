package http

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/google/uuid"

	"musiclib/internal/jobs"
)

// The import results and Activity in a real Chromium against the real
// server and PostgreSQL (round 21, NOTES.md N-285 to N-291): rows updated
// in place, Retry only where it can work, the stale failures, Activity's
// groups, times and actions.

// A folder without any album: the import completes with one sentence, the
// file listed and escaped, and the page stops polling.
func TestBrowserImportReportAndRetry(t *testing.T) {
	e, root := browserEnv(t)
	hostile := `Album <script>" &`
	e.files(hostile + `/file <script> &.txt`)
	im := e.testImporter(t)
	tab, problems := browserTab(t, root)
	browserGo(t, tab, e.srv.URL+"/import?path="+url.QueryEscape(hostile))
	browserWait(t, tab, `!document.querySelector('#import-start').hidden`)
	browserEval(t, tab, `document.querySelector('#import-start').click();''`)
	browserWait(t, tab, `location.search.startsWith('?batch=')`)
	if got := browserEval(t, tab, `document.querySelector('.results-title').textContent+'|'+document.querySelectorAll('main script').length`); got != hostile+"|0" {
		t.Fatalf("results head: %s", got)
	}
	e.runNext(t, im, jobs.KindScan)
	browserWait(t, tab, `document.querySelector('#nav-active').hidden && document.querySelector('#count-attention').textContent === '1'`)
	if got := browserEval(t, tab, `document.querySelector('#rows-attention .row-sentence').textContent+'|'+(document.querySelector('#rows-attention [data-retry]')===null)+'|'+document.querySelector('#skipped li').textContent.includes('file <script> &.txt')+'|'+document.querySelectorAll('main script').length`); got != "No albums found. Put each album’s tracks in a folder of its own, then import the folder again.|true|true|0" {
		t.Fatalf("no albums: %s", got)
	}
	browserEval(t, tab, `window.pageFetches=0;const prior=window.fetch;window.fetch=(...args)=>{if(String(args[0]).startsWith('/import'))window.pageFetches++;return prior(...args)};''`)
	time.Sleep(4300 * time.Millisecond)
	if got := browserEval(t, tab, `String(window.pageFetches)`); got != "0" {
		t.Fatalf("a completed import kept polling: %s", got)
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}
}

// Rows update in place (N-291): while the page polls, a half-typed title
// keeps its value, focus and caret; an open «Details» stays open; the
// scroll stays; unchanged rows are the same DOM nodes; a changed row is
// replaced alone, a finished one moves to its tab and the counts follow; a
// failed poll says so without taking the focus; Dismiss takes out one row.
func TestBrowserImportDraftSurvivesPolling(t *testing.T) {
	e, root := browserEnv(t)
	album := e.seed("Miles Davis", "Kind of Blue")
	e.exec(`UPDATE albums SET cover_hash = NULL`) // the seed records a cover without its bytes
	batch := e.newImport("in")
	edited := e.batchJob(batch, "in/00 Edited", "failed", "mixed_album", "two album tags: A, B", uuid.Nil)
	opened := e.batchJob(batch, "in/01 Opened", "failed", "source_changed", "changed", uuid.Nil)
	changing := e.batchJob(batch, "in/02 Changing", "failed", "source_changed", "first message", uuid.Nil)
	gone := e.batchJob(batch, "in/03 Dismissed", "failed", "corrupt_audio", "damaged", uuid.Nil)
	for i := range 14 {
		e.batchJob(batch, "in/1"+strconv.Itoa(i/10)+strconv.Itoa(i%10)+" Filler", "failed", "corrupt_audio", "damaged", uuid.Nil)
	}
	pending := e.batchJob(batch, "in/99 Pending", "pending", "", "", uuid.Nil)
	e.batchJob(batch, "in/98 Waiting", "pending", "", "", uuid.Nil) // keeps the page polling
	tab, problems := browserTab(t, root)
	if err := chromedp.Run(tab, emulation.SetDeviceMetricsOverride(1280, 600, 1, false)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, tab, e.srv.URL+"/import?batch="+batch.String())
	title := `#rows-attention li[data-key="` + edited.String() + `"] input[name=title]`
	browserWait(t, tab, `document.querySelector('`+title+`') && !document.querySelector('#nav-active').hidden`)
	if err := chromedp.Run(tab, chromedp.SendKeys(title, "Another careful title")); err != nil {
		t.Fatal(err)
	}
	// The caret inside the value, a Details open, the page scrolled, and
	// every row marked so that a rebuilt row would show.
	browserEval(t, tab, `const input=document.querySelector('`+title+`');input.setSelectionRange(8,8);
document.querySelector('li[data-key="`+opened.String()+`"] > details').open=true;
for(const li of document.querySelectorAll('#rows-attention > li'))li.marker=li.dataset.key;
scrollTo(0,300);window.fetches=0;window.retryBodies=[];const originalFetch=window.fetch;window.fetch=(url,options)=>{if(String(url).startsWith('/import?'))window.fetches++;if(String(url).endsWith('/retry'))window.retryBodies.push(JSON.parse(options.body));return originalFetch(url,options)};''`)
	top := browserEval(t, tab, `String(document.querySelector('li[data-key="`+edited.String()+`"]').getBoundingClientRect().top)`)
	// Meanwhile one row changes, the pending one gets imported.
	e.exec(`UPDATE jobs SET error_message = 'second message', updated_at = now() WHERE id = $1`, changing)
	e.exec(`UPDATE jobs SET state = 'done', result_album_id = $2, updated_at = now() WHERE id = $1`, pending, album)
	browserWait(t, tab, `window.fetches >= 2 && document.querySelector('#count-imported').textContent === '1'`)
	check := func(stage string) {
		t.Helper()
		got := browserEval(t, tab, `JSON.stringify({value:document.querySelector('`+title+`').value, focused:document.activeElement===document.querySelector('`+title+`'), caret:document.activeElement.selectionStart+'-'+document.activeElement.selectionEnd, open:document.querySelector('li[data-key="`+opened.String()+`"] > details').open, top:String(document.querySelector('li[data-key="`+edited.String()+`"]').getBoundingClientRect().top), kept:[...document.querySelectorAll('#rows-attention > li')].filter(li=>li.marker===li.dataset.key).length, rows:document.querySelectorAll('#rows-attention > li').length})`)
		want := `{"value":"Another careful title","focused":true,"caret":"8-8","open":true,"top":"` + top + `","kept":`
		if !strings.HasPrefix(got, want) {
			t.Fatalf("%s: the poll disturbed the page: %s", stage, got)
		}
	}
	check("after two polls")
	// The changed row alone was replaced; the others are the same nodes.
	if got := browserEval(t, tab, `[...document.querySelectorAll('#rows-attention > li')].filter(li=>li.marker!==li.dataset.key).map(li=>li.dataset.key).join()+'|'+document.querySelector('li[data-key="`+changing.String()+`"] > details pre').textContent`); got != changing.String()+"|source_changed: second message" {
		t.Fatalf("replaced rows: %s", got)
	}
	// A failed poll says so, without taking the focus; the next one hides it.
	browserEval(t, tab, `const previousFetch=window.fetch;window.fetch=(url,options)=>{if(String(url).startsWith('/import?') && !window.pollFailed){window.pollFailed=true;return Promise.resolve(new Response('unavailable',{status:503}))}return previousFetch(url,options)};''`)
	browserWait(t, tab, `window.pollFailed && document.querySelector('#progress + .notice') !== null`)
	check("after a failed poll")
	browserWait(t, tab, `document.querySelector('#progress + .notice') === null`)
	check("after the next poll")
	// Dismiss another row mid-poll: that row goes, nothing else moves.
	browserEval(t, tab, `document.querySelector('li[data-key="`+gone.String()+`"] [data-dismiss]').click();document.querySelector('`+title+`').focus();''`)
	browserWait(t, tab, `document.querySelector('li[data-key="`+gone.String()+`"]')===null && document.querySelector('#count-attention').textContent === '17'`)
	if e.count(`SELECT count(*) FROM jobs WHERE id=$1 AND dismissed_at IS NOT NULL AND state='failed'`, gone) != 1 {
		t.Fatal("the dismissal was not stored")
	}
	if got := browserEval(t, tab, `[...document.querySelectorAll('#rows-attention > li')].filter(li=>li.marker===li.dataset.key).length+'|'+document.querySelector('`+title+`').value`); got != "16|Another careful title" {
		t.Fatalf("the dismissal rebuilt the list: %s", got)
	}
	// A row that a poll takes away from under the focus hands the focus to
	// its neighbour, not to the page (N-295).
	focusedRow := `document.activeElement.closest('#rows-attention > li')?.dataset.key ?? document.activeElement.tagName`
	browserEval(t, tab, `document.querySelector('li[data-key="`+opened.String()+`"] [data-retry]').focus();''`)
	e.exec(`UPDATE jobs SET state = 'skipped', result_album_id = $2, error_code = 'duplicate_import', error_message = 'already imported',
		updated_at = now() WHERE id = $1`, opened, album)
	browserWait(t, tab, `document.querySelector('#rows-attention li[data-key="`+opened.String()+`"]')===null`)
	if got := browserEval(t, tab, focusedRow); got != changing.String() {
		t.Fatalf("the focus after its row left: %s", got)
	}
	// Typing goes on where the caret was; the retry sends the draft.
	if err := chromedp.Run(tab, chromedp.Click(title), chromedp.Evaluate(`document.querySelector('`+title+`').setSelectionRange(8,8)`, nil), chromedp.SendKeys(title, "steady ")); err != nil {
		t.Fatal(err)
	}
	browserEval(t, tab, `document.querySelector('li[data-key="`+edited.String()+`"] form button').click();''`)
	browserWait(t, tab, `window.retryBodies.length===1 && document.querySelector('li[data-key="`+edited.String()+`"]')===null`)
	if got := browserEval(t, tab, `JSON.stringify(window.retryBodies[0])`); got != `{"artist":null,"title":"Another steady careful title"}` {
		t.Fatalf("retry lost the draft: %s", got)
	}
	if e.count(`SELECT count(*) FROM jobs WHERE id=$1 AND state='pending' AND overrides->>'title'='Another steady careful title'`, edited) != 1 {
		t.Fatal("the retry did not store the draft")
	}
	// The retried row left the tab with the focus in it: the focus is on
	// the next row.
	if got := browserEval(t, tab, focusedRow); got != changing.String() {
		t.Fatalf("the focus after the retry: %s", got)
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}
}

// A report captured before an accepted retry must not overwrite the new
// attempt, and the new attempt's stored override must replace the old
// form (N-210, kept).
func TestBrowserImportRetryInvalidatesPreviousAttempt(t *testing.T) {
	e, root := browserEnv(t)
	batch := e.newImport("in")
	job := e.batchJob(batch, "in/failed", "failed", "mixed_album", "first attempt", uuid.Nil)
	e.batchJob(batch, "in/pending", "pending", "", "", uuid.Nil) // Keep the page polling.
	tab, _ := browserTab(t, root)
	browserGo(t, tab, e.srv.URL+"/import?batch="+batch.String())
	title := `#rows-attention input[name=title]`
	browserWait(t, tab, `document.querySelector('`+title+`') !== null`)
	browserEval(t, tab, `document.querySelector('`+title+`').value=' Old title ';
window.retryDelivered=false;window.oldDelivered=false;window.failRetry=true;
const realFetch=window.fetch;
window.fetch=async (url,options)=>{
  if(String(url).endsWith('/retry')) {
    if(window.failRetry) { window.failRetry=false;return new Response(JSON.stringify({code:'test_unavailable',message:'retry later'}),{status:503,headers:{'Content-Type':'application/json'}}); }
    const response=await realFetch(url,options);
    return new Promise(resolve=>{window.releaseRetry=()=>{window.retryDelivered=true;resolve(response)};});
  }
  if(String(url).startsWith('/import?')) {
    const afterRetry=window.retryDelivered;
    const response=await realFetch(url,options);
    const snapshot=new Response(await response.text(),{status:response.status,headers:response.headers});
    if(!afterRetry && !window.releaseOld) return new Promise(resolve=>{window.releaseOld=()=>{resolve(snapshot);setTimeout(()=>window.oldDelivered=true,0)};});
    if(afterRetry && !window.releaseFresh) return new Promise(resolve=>{window.releaseFresh=()=>resolve(snapshot);});
    return new Promise(()=>{}); // No later poll can change the assertion.
  }
  return realFetch(url,options);
};''`)
	browserEval(t, tab, `document.querySelector('#rows-attention form button').click();''`)
	notice := `document.querySelector('#rows-attention .notice')`
	browserWait(t, tab, `!`+notice+`.hidden && `+notice+`.querySelector('pre').textContent.includes('test_unavailable')`)
	if got := browserEval(t, tab, notice+`.querySelector('p').textContent+'|'+document.querySelector('`+title+`').value`); got != "The import didn’t start again. Try again.| Old title " {
		t.Fatal("a refused retry lost the draft: " + got)
	}
	browserWait(t, tab, `typeof window.releaseOld==='function'`)
	browserEval(t, tab, `document.querySelector('#rows-attention form button').click();''`)
	browserWait(t, tab, `typeof window.releaseRetry==='function'`)
	if e.count(`SELECT count(*) FROM jobs WHERE id=$1 AND state='pending' AND overrides->>'title'='Old title'`, job) != 1 {
		t.Fatal("the retry did not store the normalized title")
	}
	// The new attempt fails again before the page hears of the retry.
	e.exec(`UPDATE jobs SET state='failed', error_code='mixed_album', error_message='second attempt', updated_at=now() WHERE id=$1`, job)
	browserEval(t, tab, `window.releaseRetry();''`)
	browserWait(t, tab, `typeof window.releaseFresh==='function'`)
	browserEval(t, tab, `window.releaseFresh();''`)
	browserWait(t, tab, `document.querySelector('#rows-attention').textContent.includes('second attempt')`)
	checkNew := func() {
		t.Helper()
		if got := browserEval(t, tab, `String(document.querySelector('`+title+`')?.value==='Old title' && document.querySelector('#rows-attention').textContent.includes('second attempt'))`); got != "true" {
			t.Fatal("the previous attempt overwrote the new one: " + got)
		}
	}
	checkNew()
	// The report requested before the retry arrives last: ignored.
	browserEval(t, tab, `window.releaseOld();''`)
	browserWait(t, tab, `window.oldDelivered`)
	checkNew()
}

// Each row needing attention offers the one fix of its code, and Retry
// only where trying again can work (N-287): the owner's own failures
// among them. A kept artist override goes back with a new title.
func TestBrowserImportRetryOnlyWhenItCanWork(t *testing.T) {
	e, root := browserEnv(t)
	batch := e.newImport("")
	rows := map[string]uuid.UUID{}
	for source, code := range map[string]string{
		"A Contracorriente": "mixed_album", "Various": "ambiguous_album_artist", "Álvaro Soler": "ambiguous_candidate",
		"Root": "not_a_candidate", "Changed": "source_changed", "Slow": "media_timeout", "Damaged": "corrupt_audio",
	} {
		rows[code] = e.batchJob(batch, source, "failed", code, "technical words", uuid.Nil)
	}
	e.exec(`UPDATE jobs SET overrides = '{"artist": "Kept artist"}' WHERE id = $1`, rows["mixed_album"])
	tab, problems := browserTab(t, root)
	browserGo(t, tab, e.srv.URL+"/import?batch="+batch.String())
	browserWait(t, tab, `document.querySelectorAll('#rows-attention > li').length === 7 && !document.querySelector('.row-actions').hidden`)
	actions := func(code string) string {
		return browserEval(t, tab, `(li=>[...li.querySelectorAll('.row-actions button, .row-actions input:not([type=hidden])')].map(c=>c.getAttribute('name')||c.textContent).join()+'|'+li.querySelector('.row-sentence').textContent.includes('_')+'|'+li.querySelector(':scope > details').open)(document.querySelector('li[data-key="`+rows[code].String()+`"]'))`)
	}
	for code, want := range map[string]string{
		"mixed_album": "title,Import with this title,Dismiss|false|false", "ambiguous_album_artist": "artist,Import with this artist,Dismiss|false|false",
		"ambiguous_candidate": "Dismiss|false|false", "not_a_candidate": "Dismiss|false|false", "corrupt_audio": "Dismiss|false|false",
		"source_changed": "Retry,Dismiss|false|false", "media_timeout": "Retry,Dismiss|false|false",
	} {
		if got := actions(code); got != want {
			t.Errorf("%s: %s, want %s", code, got, want)
		}
	}
	// Retry sends no body (the stored overrides are kept, N-195).
	browserEval(t, tab, `window.bodies=[];const f=window.fetch;window.fetch=(u,o)=>{if(String(u).endsWith('/retry'))window.bodies.push(o.body===undefined?'none':o.body);return f(u,o)};document.querySelector('li[data-key="`+rows["source_changed"].String()+`"] [data-retry]').click();''`)
	browserWait(t, tab, `window.bodies.length===1 && document.querySelector('li[data-key="`+rows["source_changed"].String()+`"]')===null`)
	// The title override keeps the stored artist.
	if err := chromedp.Run(tab, chromedp.SendKeys(`li[data-key="`+rows["mixed_album"].String()+`"] input[name=title]`, "A Contracorriente")); err != nil {
		t.Fatal(err)
	}
	browserKey(t, tab, kb.Enter)
	browserWait(t, tab, `window.bodies.length===2`)
	if got := browserEval(t, tab, `JSON.stringify(window.bodies)`); got != `["none","{\"artist\":\"Kept artist\",\"title\":\"A Contracorriente\"}"]` {
		t.Fatalf("retry bodies: %s", got)
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}
}

// Stale failures (owner, N-285) with the owner's own rows: the root that is
// not an album, the folder with tracks and subfolders and the search that
// found no album are superseded by later imports of subfolders; the mixed
// album by a later import of itself, already there. The one left is
// dismissed from Activity, and «Retry all» leaves them all alone.
func TestBrowserStaleFailures(t *testing.T) {
	e, root := browserEnv(t)
	album := e.seed("James Hype", "Ferrari")
	e.exec(`UPDATE albums SET cover_hash = NULL`)
	e.align(album)
	e.exec(`DELETE FROM jobs WHERE kind <> 'render'`)
	e.exec(`DELETE FROM import_batches`)
	first := e.newImport("")
	e.exec(`UPDATE jobs SET state='failed', warnings='[]', error_code='no_valid_candidate', error_message='1 album candidate under the import root, none importable' WHERE kind='scan' AND batch_id=$1`, first)
	search := e.count(`SELECT count(*) FROM jobs WHERE kind='scan' AND state='failed'`)
	second := e.newImport("")
	rootJob := e.batchJob(second, "", "failed", "not_a_candidate", "the root has no audio directly", uuid.Nil)
	ambiguous := e.batchJob(second, "Álvaro Soler", "failed", "ambiguous_candidate", "audio files here and further down", uuid.Nil)
	mixed := e.batchJob(second, "Álvaro Soler/A Contracorriente", "failed", "mixed_album", `2 album tags: "A Contracorriente", "Ferrari"`, uuid.Nil)
	other := e.batchJob(second, "Other", "failed", "corrupt_audio", "damaged", uuid.Nil)
	if search != 1 {
		t.Fatalf("%d failed searches", search)
	}
	tab, problems := browserTab(t, root)
	browserGo(t, tab, e.srv.URL+"/activity")
	titles := `JSON.stringify([...document.querySelectorAll('#rows-failed .row-title')].map(n=>n.textContent).sort())`
	if got := browserEval(t, tab, titles+`+'|'+document.querySelector('#total-failed').textContent`); got != `["A Contracorriente","Music folder","Music folder","Other","Álvaro Soler"]|5` {
		t.Fatalf("before: %s", got)
	}
	// The owner reorganised the folders and imported the subfolders: the
	// structural failures above them go; the mixed album stays.
	later := e.newImport("James Hype")
	e.exec(`UPDATE jobs SET warnings='[]' WHERE kind='scan' AND batch_id=$1`, later)
	e.batchJob(later, "James Hype/Ferrari (Remix)", "done", "", "", album)
	e.batchJob(later, "Álvaro Soler/Ferrari", "done", "", "", album)
	browserGo(t, tab, e.srv.URL+"/activity")
	if got := browserEval(t, tab, titles); got != `["A Contracorriente","Other"]` {
		t.Fatalf("after the subfolders: %s", got)
	}
	// The mixed album imported later finds itself already there.
	again := e.newImport("Álvaro Soler/A Contracorriente")
	e.exec(`UPDATE jobs SET warnings='[]' WHERE kind='scan' AND batch_id=$1`, again)
	e.batchJob(again, "Álvaro Soler/A Contracorriente", "skipped", "", "", album)
	browserGo(t, tab, e.srv.URL+"/activity")
	if got := browserEval(t, tab, titles+`+'|'+document.querySelector('#total-failed').textContent`); got != `["Other"]|1` {
		t.Fatalf("after already there: %s", got)
	}
	// The import's own results agree.
	browserGo(t, tab, e.srv.URL+"/import?batch="+second.String())
	if got := browserEval(t, tab, `document.querySelector('#count-attention').textContent+'|'+document.querySelectorAll('#rows-attention > li').length`); got != "1|1" {
		t.Fatalf("the import's results: %s", got)
	}
	// Dismiss the last one on Activity: gone, and still gone after a reload.
	browserGo(t, tab, e.srv.URL+"/activity")
	browserWait(t, tab, `!document.querySelector('#rows-failed [data-dismiss]').closest('.row-actions').hidden`)
	browserEval(t, tab, `document.querySelector('#rows-failed [data-dismiss]').click();''`)
	browserWait(t, tab, `document.querySelector('#group-failed') === null && document.querySelector('#activity-empty') !== null`)
	browserGo(t, tab, e.srv.URL+"/activity")
	if got := browserEval(t, tab, `(document.querySelector('#group-failed') === null)+'|'+document.querySelector('#activity-empty').textContent`); got != "true|Nothing in progress.Your library is up to date." {
		t.Fatalf("after the reload: %s", got)
	}
	// Retry all (through the API: the button is gone with the group)
	// retries none of them.
	if r := e.must(req{method: "POST", path: "/api/jobs/retry-failed"}, 202); r.body["retried"] != float64(0) {
		t.Fatalf("retry-failed retried settled failures: %s", r.raw)
	}
	for _, id := range []uuid.UUID{rootJob, ambiguous, mixed, other} {
		if e.count(`SELECT count(*) FROM jobs WHERE id=$1 AND state='failed'`, id) != 1 {
			t.Fatal("a settled failure was retried")
		}
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}
}

// Activity: three groups with their dots and totals; each row the album's
// cover or initials and name, linking to the editor (a folder to its
// import); relative times with the full date; «Retry all» with the count
// it queued; Advanced «Rebuild the library folder» with its confirmation
// and count; polling only while there is work; the empty state.
func TestBrowserActivityActionsAndIdle(t *testing.T) {
	e, root := browserEnv(t)
	f := e.queueFixture(t)
	hostile := e.seed(`Evil <script>" &`, `Title <script>" &`)
	e.exec(`UPDATE albums SET cover_hash = NULL WHERE id = $1`, hostile)
	e.failRender(hostile)
	e.exec(`UPDATE jobs SET error_message = 'disk <script>" & full', updated_at = now() - interval '2 hours' WHERE kind='render' AND album_id=$1`, hostile)
	runningRequested := e.count(`SELECT requested FROM jobs WHERE kind='render' AND album_id=$1`, f.running)
	tab, problems := browserTab(t, root)
	browserGo(t, tab, e.srv.URL+"/activity")
	browserWait(t, tab, `!document.querySelector('#retry-all').hidden`)
	if got := browserEval(t, tab, `JSON.stringify([...document.querySelectorAll('.group')].map(g=>[g.dataset.state, g.querySelector('.group-title').textContent, getComputedStyle(g.querySelector('.dot')).backgroundColor, g.querySelectorAll('.row').length]))`); got != `[["running","In progress 1","rgb(201, 52, 0)",1],["pending","Waiting 2","rgb(110, 110, 115)",2],["failed","Needs attention 5","rgb(215, 0, 21)",5]]` {
		t.Fatalf("groups: %s", got)
	}
	row := func(id uuid.UUID) string {
		return browserEval(t, tab, `(li=>JSON.stringify([li.querySelector('.row-title').textContent, li.querySelector('.row-note').textContent, li.querySelector('a').getAttribute('href'), li.querySelector('img')?.getAttribute('src') ?? li.querySelector('.thumb').textContent, li.querySelector('img')?.loading ?? '', li.querySelector('.row-time').textContent]))([...document.querySelectorAll('.row')].find(li=>li.querySelector('a').getAttribute('href')==='/albums/`+id.String()+`'))`)
	}
	if got := row(f.running); got != `["In Rainbows","Radiohead","/albums/`+f.running.String()+`","/api/albums/`+f.running.String()+`/cover","lazy","Started 2 minutes ago"]` {
		t.Fatalf("running row: %s", got)
	}
	if got := row(hostile); got != `["Title <script>\" &","Evil <script>\" &","/albums/`+hostile.String()+`","TS","","2 hours ago"]` {
		t.Fatalf("hostile row: %s", got)
	}
	// Dismiss is offered on an import that needs attention, never on an
	// album whose update failed: that is the album's own state (N-285).
	if got := browserEval(t, tab, `JSON.stringify([...document.querySelectorAll('#rows-failed > li')].map(li=>[li.querySelector('a').getAttribute('href').startsWith('/albums/'), li.querySelector('[data-dismiss]')!==null]).sort())`); got != `[[false,true],[false,true],[false,true],[true,false],[true,false]]` {
		t.Fatalf("Dismiss on the rows (album row, dismissable): %s", got)
	}
	if got := browserEval(t, tab, `document.querySelectorAll('main script').length+'|'+[...document.querySelectorAll('#rows-failed pre')].some(p=>p.textContent==='render_io: disk <script>" & full')`); got != "0|true" {
		t.Fatalf("escaping: %s", got)
	}
	// An import's row names its folder and leads to its results.
	if got := browserEval(t, tab, `(li=>li.querySelector('.row-title').textContent+'|'+li.querySelector('a').getAttribute('href')+'|'+li.querySelector('.row-sentence').textContent)(document.querySelector('li[data-key="`+f.mixed.String()+`"]'))`); got != "A Contracorriente|/import?batch="+f.batch.String()+"|The tracks have different album names. Give the album one title." {
		t.Fatalf("an import's row: %s", got)
	}
	// Times: ISO in datetime, the full local date in title, shown on focus.
	if got := browserEval(t, tab, `(t=>t.dateTime.endsWith('Z')+'|'+/^\d{1,2} \w+ \d{4} at \d\d:\d\d$/.test(t.title)+'|'+(t.title===new Intl.DateTimeFormat('en-GB',{dateStyle:'long',timeStyle:'short'}).format(new Date(t.dateTime))))(document.querySelector('#rows-running time'))`); got != "true|true|true" {
		t.Fatalf("time: %s", got)
	}
	browserEval(t, tab, `document.querySelector('#retry-all').focus();''`)
	browserKey(t, tab, kb.Tab)
	if got := browserEval(t, tab, `(t=>{const c=getComputedStyle(t,'::after').content;return String(c===JSON.stringify(t.title)||c==='attr(title)')})(document.activeElement.closest('.row').querySelector('time'))`); got != "true" {
		t.Fatalf("the full date on focus: %s", got)
	}
	// Retry all: the answer's count, not the page's rows. The polls are held
	// while one more failure arrives, so the page shows 5 and 6 are retried.
	browserEval(t, tab, `window.holdPolls=true;const held=window.fetch;window.fetch=(u,o)=>String(u).startsWith('/activity')&&window.holdPolls?new Promise(r=>{const w=setInterval(()=>{if(!window.holdPolls){clearInterval(w);r(held(u,o))}},50)}):held(u,o);''`)
	e.batchJob(f.batch, "Jazz/Late", "failed", "source_changed", "late", uuid.Nil)
	browserEval(t, tab, `document.querySelector('#retry-all').click();''`)
	browserWait(t, tab, `document.querySelector('#activity-note').textContent === '6 albums queued again'`)
	if got := browserEval(t, tab, `String(document.querySelectorAll('#rows-failed > li').length)`); got != "5" {
		t.Fatalf("the page polled while held: %s rows", got)
	}
	browserEval(t, tab, `window.holdPolls=false;''`)
	browserWait(t, tab, `document.querySelector('#group-failed') === null`)
	if e.count(`SELECT count(*) FROM jobs WHERE state='failed'`) != 0 || e.count(`SELECT requested FROM jobs WHERE kind='render' AND album_id=$1`, f.running) != runningRequested ||
		e.count(`SELECT count(*) FROM jobs WHERE kind='render' AND album_id=$1 AND state='running'`, f.running) != 1 {
		t.Fatal("retry all missed a failure or touched the running render")
	}
	// Everything done: the empty state, and no more polling.
	e.exec(`DELETE FROM jobs WHERE kind = 'render'`)
	e.exec(`UPDATE jobs SET state='done', claimed=NULL WHERE kind IN ('scan','import') AND state IN ('pending','running')`)
	browserWait(t, tab, `document.querySelector('#activity-empty') !== null && document.querySelector('#nav-active').hidden`)
	// Idle and settled: the empty sentence alone, nothing hidden behind it (N-296).
	if got := browserEval(t, tab, `document.querySelectorAll('main :is(.group, .notice, [id^=more-])').length+'|'+JSON.stringify(document.querySelector('#nav-active').textContent)`); got != `0|""` {
		t.Fatalf("idle Activity: %s", got)
	}
	browserEval(t, tab, `window.pageFetches=0;const prior=window.fetch;window.fetch=(...a)=>{if(String(a[0]).startsWith('/activity'))window.pageFetches++;return prior(...a)};''`)
	time.Sleep(4300 * time.Millisecond)
	if got := browserEval(t, tab, `String(window.pageFetches)`); got != "0" {
		t.Fatalf("an idle page kept polling: %s", got)
	}
	// Rebuild the library folder: closed Advanced, a confirmation with the
	// count; Cancel changes nothing, Rebuild queues and says how many.
	albums := e.count(`SELECT count(*) FROM albums WHERE deleted_at IS NULL`)
	if got := browserEval(t, tab, `document.querySelector('.advanced').open+'|'+document.querySelector('#rebuild-text').textContent`); got != "false|"+strconv.Itoa(albums)+" albums are written to the library folder again. This takes time and disk space." {
		t.Fatalf("advanced: %s", got)
	}
	browserEval(t, tab, `document.querySelector('.advanced').open=true;document.querySelector('#rebuild').click();''`)
	browserWait(t, tab, `document.querySelector('#rebuild-ask').open`)
	browserKey(t, tab, kb.Escape)
	browserWait(t, tab, `!document.querySelector('#rebuild-ask').open`)
	if e.count(`SELECT count(*) FROM jobs WHERE kind='render'`) != 0 {
		t.Fatal("a cancelled rebuild queued renders")
	}
	// One more album arrives while the page is idle: the sheet still says
	// the count it was rendered with, the note says the answer's.
	e.seed("Late artist", "Late album")
	e.exec(`UPDATE albums SET cover_hash = NULL WHERE title = 'Late album'`)
	albums++
	browserEval(t, tab, `document.querySelector('#rebuild').click();document.querySelector('#rebuild-ask button[value=ok]').click();''`)
	browserWait(t, tab, `document.querySelector('#rebuild-note').textContent === '`+strconv.Itoa(albums)+` albums queued' && document.querySelector('#group-pending') !== null`)
	// The groups come and go with the polls (N-296): an import fails while
	// the renders wait; its group goes in after Waiting with its own «Retry
	// all», which works, and the group goes again.
	e.batchJob(f.batch, "Jazz/Later", "failed", "source_changed", "late", uuid.Nil)
	browserWait(t, tab, `document.querySelector('#group-failed') !== null && !document.querySelector('#retry-all').hidden`)
	if got := browserEval(t, tab, `[...document.querySelectorAll('main > :is(.group, .empty, .advanced)')].map(e=>e.id).join()+'|'+document.querySelector('#total-failed').textContent+'|'+document.querySelector('#nav-active').textContent`); got != "group-pending,group-failed,advanced|1|, in progress" {
		t.Fatalf("a group that arrived: %s", got)
	}
	browserEval(t, tab, `document.querySelector('#retry-all').click();''`)
	browserWait(t, tab, `document.querySelector('#activity-note').textContent === '1 album queued again' && document.querySelector('#group-failed') === null`)
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind='render' AND state='pending'`); n != albums {
		t.Fatalf("%d renders queued, want %d", n, albums)
	}
	if got := browserEval(t, tab, `JSON.stringify(window.cspViolations)`); got != "[]" {
		t.Fatalf("CSP violations: %s", got)
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}
	// Without JavaScript: readable, no actions.
	plain, _ := browserTab(t, root)
	if err := chromedp.Run(plain, emulation.SetScriptExecutionDisabled(true)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, plain, e.srv.URL+"/activity")
	if got := browserEval(t, plain, `document.querySelectorAll('#rows-pending > li').length+'|'+document.querySelector('#rebuild').hidden+'|'+/ago|just now/.test(document.querySelector('#rows-pending time').textContent)`); got != strconv.Itoa(min(albums+1, activityRows))+"|true|true" {
		t.Fatalf("no-JS activity: %s", got)
	}
}

// The tabs follow the ARIA pattern: arrows, Home and End move and select;
// without JavaScript they are links to the other tab.
func TestBrowserImportTabs(t *testing.T) {
	e, root := browserEnv(t)
	album := e.seed("Miles Davis", "Kind of Blue")
	e.exec(`UPDATE albums SET cover_hash = NULL`) // the seed records a cover without its bytes
	batch := e.newImport("Jazz")
	e.batchJob(batch, "Jazz/A", "done", "", "", album)
	e.batchJob(batch, "Jazz/B", "skipped", "", "", album)
	tab, problems := browserTab(t, root)
	browserGo(t, tab, e.srv.URL+"/import?batch="+batch.String())
	state := `JSON.stringify([document.querySelector('.tablist').getAttribute('role'), ...[...document.querySelectorAll('.tab')].map(t=>t.getAttribute('role')+':'+t.getAttribute('aria-selected')+':'+t.tabIndex+':'+document.getElementById(t.getAttribute('aria-controls')).hidden)])`
	browserWait(t, tab, `document.querySelector('.tab[aria-selected]')`)
	if got := browserEval(t, tab, state); got != `["tablist","tab:false:-1:true","tab:true:0:false","tab:false:-1:true"]` {
		t.Fatalf("opens on Imported: %s", got)
	}
	browserEval(t, tab, `document.querySelector('#tab-imported').focus();''`)
	browserKey(t, tab, kb.ArrowRight)
	if got := browserEval(t, tab, `document.activeElement.id+'|'+location.search.endsWith('tab=present')+'|'+document.querySelector('#panel-present').getAttribute('role')`); got != "tab-present|true|tabpanel" {
		t.Fatalf("arrow right: %s", got)
	}
	browserKey(t, tab, kb.ArrowRight)
	if got := browserEval(t, tab, `document.activeElement.id`); got != "tab-attention" {
		t.Fatalf("arrow right wraps: %s", got)
	}
	browserKey(t, tab, kb.End)
	browserKey(t, tab, kb.Home)
	if got := browserEval(t, tab, state); got != `["tablist","tab:true:0:false","tab:false:-1:true","tab:false:-1:true"]` {
		t.Fatalf("Home: %s", got)
	}
	if got := browserEval(t, tab, `document.querySelector('#empty-attention').textContent+'|'+document.querySelector('#empty-attention').hidden`); got != "Nothing needs attention.|false" {
		t.Fatalf("empty tab: %s", got)
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}
	plain, _ := browserTab(t, root)
	if err := chromedp.Run(plain, emulation.SetScriptExecutionDisabled(true)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, plain, e.srv.URL+"/import?batch="+batch.String())
	if err := chromedp.Run(plain, chromedp.Click(`#tab-present`, chromedp.NodeVisible)); err != nil {
		t.Fatal(err)
	}
	browserWait(t, plain, `location.search.endsWith('tab=present') && !document.querySelector('#panel-present').hidden && document.querySelector('#tab-present').getAttribute('aria-current')==='true'`)
	if got := browserEval(t, plain, `document.querySelector('#rows-present .row-title').textContent+'|'+document.querySelector('#rows-present a').getAttribute('href')`); got != "Kind of Blue|/albums/"+album.String() {
		t.Fatalf("no-JS tab: %s", got)
	}
}
