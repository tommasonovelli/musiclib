package http

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"github.com/google/uuid"
)

// The Library in a real Chromium against the real server and PostgreSQL
// (round 19, NOTES.md N-243–N-252).

// coverPNG is a cover of two colours: fg fills the top fraction of the
// rows, bg the rest.
func coverPNG(t testing.TB, bg, fg color.RGBA, fraction float64) []byte {
	t.Helper()
	const size = 64
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			c := bg
			if float64(y) < fraction*size {
				c = fg
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// setCover uploads a real cover through the API.
func (e *env) setCover(id uuid.UUID, img []byte) {
	e.t.Helper()
	_, etag := e.album(id)
	e.must(upload("PUT", "/api/albums/"+id.String()+"/cover", etag, img), 200)
}

// browserTab opens a tab that records uncaught exceptions, console errors
// and CSP violations from the first script of every document.
func browserTab(t *testing.T, root context.Context) (context.Context, func() []string) {
	t.Helper()
	tab, stop := chromedp.NewContext(root)
	t.Cleanup(stop)
	var mu sync.Mutex
	var problems []string
	chromedp.ListenTarget(tab, func(ev any) {
		mu.Lock()
		defer mu.Unlock()
		switch ev := ev.(type) {
		case *runtime.EventExceptionThrown:
			problems = append(problems, "exception: "+ev.ExceptionDetails.Error())
		case *runtime.EventConsoleAPICalled:
			if ev.Type == runtime.APITypeError || ev.Type == runtime.APITypeWarning {
				problems = append(problems, fmt.Sprintf("console %s", ev.Type))
			}
		case *log.EventEntryAdded:
			// The favicon request is the browser's, not the page's.
			if ev.Entry.Level == log.LevelError && !strings.Contains(ev.Entry.URL, "favicon.ico") {
				problems = append(problems, "log: "+ev.Entry.Text)
			}
		}
	})
	err := chromedp.Run(tab, log.Enable(), chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(`window.cspViolations=[];document.addEventListener('securitypolicyviolation',e=>window.cspViolations.push(e.violatedDirective+' '+e.blockedURI));`).Do(ctx)
		return err
	}))
	if err != nil {
		t.Fatal(err)
	}
	return tab, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), problems...)
	}
}

func browserGo(t *testing.T, tab context.Context, url string) {
	t.Helper()
	if err := chromedp.Run(tab, chromedp.Navigate(url)); err != nil {
		t.Fatal(err)
	}
}

func browserKey(t *testing.T, tab context.Context, key string) {
	t.Helper()
	if err := chromedp.Run(tab, chromedp.KeyEvent(key)); err != nil {
		t.Fatal(err)
	}
}

// contrastJS is an independent WCAG 2 contrast of two computed colours,
// written for the test, not taken from the module under test.
const contrastJS = `(()=>{const rgb=s=>s.match(/[\d.]+/g).slice(0,3).map(Number);const lum=c=>{const [r,g,b]=c.map(v=>{v/=255;return v<=0.04045?v/12.92:((v+0.055)/1.055)**2.4});return 0.2126*r+0.7152*g+0.0722*b};window.contrastOf=(a,b)=>{const x=lum(rgb(a)),y=lum(rgb(b));return (Math.max(x,y)+0.05)/(Math.min(x,y)+0.05)};})();`

func TestBrowserLibraryGridPanelAndKeys(t *testing.T) {
	e, root := browserEnv(t)
	red := color.RGBA{0x7a, 0x1f, 0x2b, 0xff}
	cream := color.RGBA{0xf2, 0xe6, 0xc9, 0xff}
	kob := e.seed("Miles Davis", "Kind of Blue")
	e.setCover(kob, coverPNG(t, red, cream, 0.3))
	e.align(kob)
	grey := e.seed("Miles Davis", "Grey Days")
	e.setCover(grey, coverPNG(t, color.RGBA{0x77, 0x77, 0x77, 0xff}, color.RGBA{0x77, 0x77, 0x77, 0xff}, 0))
	e.align(grey)
	muted := e.seed("Miles Davis", "Muted Colours")
	e.setCover(muted, coverPNG(t, red, color.RGBA{0x8a, 0x2f, 0x3b, 0xff}, 0.3))
	e.runRender(muted)
	broken := e.seed("Miles Davis", "No Cover Broken")
	e.exec(`UPDATE albums SET cover_hash = NULL WHERE id = $1`, broken)
	e.failRender(broken)
	hostile := e.seed(`Art <b>&"`, `<script>" &`)
	e.exec(`UPDATE albums SET cover_hash = NULL WHERE id = $1`, hostile)
	tab, problems := browserTab(t, root)
	if err := chromedp.Run(tab, chromedp.EmulateViewport(1280, 900)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, tab, e.srv.URL+"/")
	browserWait(t, tab, `document.querySelectorAll('#grid > .tile').length === 5 && [...document.images].every(i => i.complete)`)
	browserEval(t, tab, contrastJS+`''`)
	tile := func(id uuid.UUID) string { return `document.querySelector('[data-album="` + id.String() + `"]')` }
	link := func(id uuid.UUID) string { return tile(id) + `.querySelector('.tile-link')` }

	// Covers, initials, dots and words: Error a red dot, Queued a still
	// graphite one, Processing a pulsing orange one, Aligned none (N-248).
	dot := func(id uuid.UUID) string {
		return `(s=>s.backgroundColor+' '+s.animationName)(getComputedStyle(` + tile(id) + `.querySelector('.dot'))), ` + tile(id) + `.querySelector('.tile-status').textContent`
	}
	if got := browserEval(t, tab, `JSON.stringify([document.querySelectorAll('#grid img').length, [...document.querySelectorAll('.initials')].map(n=>n.textContent), `+dot(broken)+`, `+dot(hostile)+`, `+dot(muted)+`, `+tile(kob)+`.querySelector('.dot, .tile-status')===null])`); got != `[3,["S","NB"],"rgb(215, 0, 21) none","Needs attention","rgb(110, 110, 115) none","Waiting","rgb(201, 52, 0) pulse","Updating",true]` {
		t.Fatalf("grid state: %s", got)
	}
	// The font is ours, and the escaping held.
	browserWait(t, tab, `document.fonts.status === 'loaded'`)
	if got := browserEval(t, tab, `String(document.fonts.check('15px "Hanken Grotesk"') && [...document.fonts].some(f => f.family === 'Hanken Grotesk' && f.status === 'loaded') && getComputedStyle(document.body).fontFamily.startsWith('"Hanken Grotesk"'))`); got != "true" {
		t.Fatalf("font: %s", got)
	}
	if got := browserEval(t, tab, tile(hostile)+`.querySelector('.tile-title').textContent + '|' + `+tile(hostile)+`.querySelector('.tile-artist').textContent + '|' + document.querySelectorAll('main script, main b').length`); got != `<script>" &|Art <b>&"|0` {
		t.Fatalf("escaping: %s", got)
	}

	// Open with a click: tracks, colours with contrast, focus in the panel.
	browserEval(t, tab, link(kob)+`.click();''`)
	browserWait(t, tab, `document.querySelector('#album-panel.is-open')?.dataset.painted === 'true' && document.querySelectorAll('#album-panel .panel-tracks li').length === 2`)
	if got := browserEval(t, tab, `JSON.stringify([`+link(kob)+`.getAttribute('aria-expanded'), `+link(kob)+`.getAttribute('aria-controls'), document.activeElement.className, document.querySelector('#album-panel .panel-title').textContent, document.querySelector('#album-panel .panel-meta').textContent, document.querySelector('#album-panel .panel-tracks').textContent, document.querySelector('#album-panel .panel-edit').getAttribute('href'), document.querySelector('#album-panel').previousElementSibling.classList.contains('tile')])`); got != `["true","album-panel","panel-title","Kind of Blue","Jazz, 1959","1So WhatDuration 9:052Freddie Freeloader Miles & Cannonball–Duration unknown","/albums/`+kob.String()+`",true]` {
		t.Fatalf("open panel: %s", got)
	}
	colours := func() (string, string, float64) {
		t.Helper()
		got := browserEval(t, tab, `(()=>{const p=document.querySelector('#album-panel');const s=getComputedStyle(p.querySelector('.panel-body'));return [p.style.getPropertyValue('--cover-bg'),p.style.getPropertyValue('--cover-ink'),s.backgroundColor,s.color,contrastOf(s.backgroundColor,s.color)].join('|')})()`)
		parts := strings.Split(got, "|")
		var ratio float64
		if _, err := fmt.Sscan(parts[4], &ratio); err != nil {
			t.Fatal(got, err)
		}
		if parts[2] != strings.ReplaceAll(parts[0], " ", ", ") || parts[3] != strings.ReplaceAll(parts[1], " ", ", ") {
			t.Fatalf("the panel does not use the variables: %s", got)
		}
		return parts[0], parts[1], ratio
	}
	if bg, ink, ratio := colours(); bg != "rgb(122 31 43)" || ink != "rgb(242 230 201)" || ratio < 4.5 {
		t.Fatalf("cover colours %s on %s: %.2f", ink, bg, ratio)
	}
	// A track's own artist sits on its own line, in the panel's ink (N-254).
	if got := browserEval(t, tab, `(()=>{const a=document.querySelector('#album-panel .track-artist'),s=getComputedStyle(a),t=a.parentElement;return [a.textContent,s.display,s.fontSize,s.color===getComputedStyle(document.querySelector('#album-panel .panel-body')).color,a.getBoundingClientRect().top>=t.getBoundingClientRect().top+10].join('|')})()`); got != "Miles & Cannonball|block|13px|true|true" {
		t.Fatalf("track artist: %s", got)
	}
	// A disc caption (library.js's li.disc) keeps the panel's ink too: the
	// album page's graphite .disc stays out of it (N-278).
	if got := browserEval(t, tab, `(()=>{const l=document.createElement('li');l.className='disc';l.textContent='Disc 1';document.querySelector('#album-panel .panel-tracks').prepend(l);const c=getComputedStyle(l).color===getComputedStyle(document.querySelector('#album-panel .panel-body')).color;l.remove();return String(c)})()`); got != "true" {
		t.Fatal("the panel's disc caption is not in the panel's ink")
	}

	// Esc closes and gives the focus back to the cover.
	browserKey(t, tab, kb.Escape)
	browserWait(t, tab, `document.querySelector('.album-panel') === null && document.activeElement === `+link(kob)+` && `+link(kob)+`.getAttribute('aria-expanded') === 'false'`)

	// Enter opens; another cover moves the panel; a second click closes.
	browserKey(t, tab, kb.Enter)
	browserWait(t, tab, `document.querySelector('#album-panel.is-open') !== null`)
	browserEval(t, tab, link(broken)+`.click();''`)
	browserWait(t, tab, `document.querySelectorAll('.album-panel').length === 1 && document.querySelector('#album-panel .panel-title')?.textContent === 'No Cover Broken' && document.querySelector('#album-panel .panel-tracks') !== null`)
	if got := browserEval(t, tab, `JSON.stringify([`+link(kob)+`.getAttribute('aria-expanded'), `+link(broken)+`.getAttribute('aria-expanded'), document.querySelector('#album-panel').style.getPropertyValue('--cover-bg'), 'cover' in document.querySelector('#album-panel').dataset, getComputedStyle(document.querySelector('#album-panel .panel-body')).backgroundColor, document.querySelector('#album-panel .panel-status').textContent, document.activeElement.className])`); got != `["false","true","",false,"rgb(255, 255, 255)","Needs attention","panel-title"]` {
		t.Fatalf("moved panel: %s", got)
	}
	browserEval(t, tab, link(broken)+`.click();''`)
	browserWait(t, tab, `document.querySelector('.album-panel') === null && `+link(broken)+`.getAttribute('aria-expanded') === 'false'`)

	// Contrast fallbacks: one colour only (black on #777, since white is
	// 4.48:1), and a second colour too close to the first (white).
	browserEval(t, tab, link(grey)+`.click();''`)
	browserWait(t, tab, `document.querySelector('#album-panel')?.dataset.painted === 'true'`)
	if bg, ink, ratio := colours(); bg != "rgb(119 119 119)" || ink != "rgb(0 0 0)" || ratio < 4.5 {
		t.Fatalf("single colour %s on %s: %.2f", ink, bg, ratio)
	}
	browserEval(t, tab, link(muted)+`.click();''`)
	browserWait(t, tab, `document.querySelector('#album-panel .panel-title')?.textContent === 'Muted Colours' && document.querySelector('#album-panel').dataset.painted === 'true'`)
	if bg, ink, ratio := colours(); bg != "rgb(122 31 43)" || ink != "rgb(255 255 255)" || ratio < 4.5 {
		t.Fatalf("close colours %s on %s: %.2f", ink, bg, ratio)
	}
	browserKey(t, tab, kb.Escape)
	browserWait(t, tab, `document.querySelector('.album-panel') === null`)

	// The hostile album's panel stays text.
	browserEval(t, tab, link(hostile)+`.focus();''`)
	browserKey(t, tab, " ")
	browserWait(t, tab, `document.querySelector('#album-panel .panel-tracks') !== null`)
	if got := browserEval(t, tab, `document.querySelector('#album-panel .panel-title').textContent + '|' + document.querySelector('#album-panel .panel-artist').textContent + '|' + document.querySelectorAll('#album-panel script, #album-panel b').length`); got != `<script>" &|Art <b>&"|0` {
		t.Fatalf("panel escaping: %s", got)
	}
	browserKey(t, tab, kb.Escape)
	browserWait(t, tab, `document.querySelector('.album-panel') === null && document.activeElement === `+link(hostile))

	// Arrow keys, Home and End move between covers in the grid's order.
	order := browserEval(t, tab, `[...document.querySelectorAll('#grid > .tile')].map(t=>t.dataset.album).join(',')`)
	ids := strings.Split(order, ",")
	focused := func() string {
		return browserEval(t, tab, `document.activeElement.closest('.tile')?.dataset.album || ''`)
	}
	browserEval(t, tab, `document.querySelector('#grid .tile-link').focus();''`)
	for _, step := range []struct {
		key  string
		want int
	}{{kb.ArrowRight, 1}, {kb.ArrowRight, 2}, {kb.ArrowLeft, 1}, {kb.End, 4}, {kb.ArrowLeft, 3}, {kb.Home, 0}, {kb.ArrowLeft, 0}, {kb.ArrowDown, 4}, {kb.ArrowUp, 0}} {
		browserKey(t, tab, step.key)
		if got := focused(); got != ids[step.want] {
			t.Fatalf("%q: focus on %s, want %s", step.key, got, ids[step.want])
		}
	}

	// «Edit album» names only this cover for the view transition.
	browserEval(t, tab, link(kob)+`.click();''`)
	browserWait(t, tab, `document.querySelector('#album-panel.is-open .panel-edit') !== null`)
	if got := browserEval(t, tab, `addEventListener('click', e => { window.named = [...document.querySelectorAll('*')].filter(n => n.style.viewTransitionName).map(n => n.className + ':' + n.style.viewTransitionName); e.preventDefault(); }, { once: true }); document.querySelector('#album-panel .panel-edit').click(); JSON.stringify(window.named)`); got != `["panel-cover:album-cover"]` {
		t.Fatalf("view transition names: %s", got)
	}
	browserEval(t, tab, `document.querySelector('#album-panel .panel-edit').click();''`)
	browserWait(t, tab, `location.pathname === '/albums/`+kob.String()+`' && document.querySelector('#editor:not([hidden])') !== null`)
	if got := browserEval(t, tab, `[...document.querySelectorAll('.cover')].filter(n => n.checkVisibility()).map(n => getComputedStyle(n).viewTransitionName).join(',')`); got != "album-cover" {
		t.Fatalf("editor cover name: %q", got)
	}
	if got := browserEval(t, tab, `JSON.stringify(window.cspViolations)`); got != "[]" {
		t.Fatalf("CSP violations: %s", got)
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}
}

// Live search, the artist chosen through it, loading on scroll with the
// real cursor, and the same page without JavaScript.
func TestBrowserLibrarySearchAndScroll(t *testing.T) {
	e, root := browserEnv(t)
	for i := range 51 {
		e.seed("Cursor Artist", fmt.Sprintf("Cursor %03d", i))
	}
	other := e.seed("Zed Other", "Other Title")
	hostile := e.seed("Zed Hostile", `Evil <script>" & title`)
	// The seed fixture records covers without bytes: they would be 500s.
	e.exec(`UPDATE albums SET cover_hash = NULL`)
	tab, problems := browserTab(t, root)
	browserGo(t, tab, e.srv.URL+"/")
	browserWait(t, tab, `document.querySelectorAll('#grid > .tile').length === 50 && document.querySelector('#more') !== null`)
	first := browserEval(t, tab, `document.querySelector('#more').getAttribute('href')`)
	// The sticky search head draws its hairline only once it is stuck (N-253).
	hairline := `(s=>s.content+' '+s.borderBottomWidth)(getComputedStyle(document.querySelector('.page-title'), '::after'))`
	if got := browserEval(t, tab, hairline); got != "none 0px" {
		t.Fatalf("hairline before scrolling: %s", got)
	}
	browserEval(t, tab, `scrollTo(0, 300);''`)
	browserWait(t, tab, hairline+` === '"" 1px'`)
	browserEval(t, tab, `scrollTo(0, 0);''`)
	browserWait(t, tab, hairline+` === 'none 0px'`)
	browserEval(t, tab, `window.marker='kept';window.pageFetches=[];const f=window.fetch;window.fetch=(u,o)=>{if(!String(u).startsWith('/api/'))window.pageFetches.push(String(u));return f(u,o)};''`)

	// Scrolling to the sentinel loads the next page with the server's cursor.
	browserEval(t, tab, `document.querySelector('#more').scrollIntoView();''`)
	browserWait(t, tab, `document.querySelectorAll('#grid > .tile').length === 53 && document.querySelector('#more') === null`)
	if got := browserEval(t, tab, `JSON.stringify([window.marker, window.pageFetches, new Set([...document.querySelectorAll('#grid > .tile')].map(t=>t.dataset.album)).size, [...document.querySelectorAll('#grid > .tile')].slice(-3).map(t=>t.querySelector('.tile-title').textContent)])`); got != `["kept",["`+first+`"],53,["Cursor 050","Evil <script>\" & title","Other Title"]]` {
		t.Fatalf("infinite scroll: %s", got)
	}
	if !strings.Contains(first, "after=") {
		t.Fatalf("no cursor in %s", first)
	}

	// Typing filters in place, the URL follows, and the page is not reloaded.
	if err := chromedp.Run(tab, chromedp.SendKeys("#filter-q", "other")); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelectorAll('#grid > .tile').length === 1 && location.search === '?q=other'`)
	if got := browserEval(t, tab, `JSON.stringify([window.marker, document.querySelector('#grid .tile').dataset.album, document.querySelector('#results-status').textContent, document.querySelector('.artist-hits a').textContent, document.querySelector('#grid .tile-link').getAttribute('role')])`); got != `["kept","`+other.String()+`","1 album","Zed Other","button"]` {
		t.Fatalf("live search: %s", got)
	}
	// The artist is chosen through the same search.
	browserEval(t, tab, `document.querySelector('.artist-hits a').click();''`)
	browserWait(t, tab, `location.search.startsWith('?artist=') && document.querySelector('h1').textContent === 'Zed Other'`)
	browserGo(t, tab, e.srv.URL+"/")
	browserWait(t, tab, `document.querySelectorAll('#grid > .tile').length === 50`)
	if err := chromedp.Run(tab, chromedp.SendKeys("#filter-q", "<script>")); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelectorAll('#grid > .tile').length === 1`)
	if got := browserEval(t, tab, `document.querySelector('#grid .tile').dataset.album + '|' + document.querySelector('#grid .tile-title').textContent + '|' + document.querySelectorAll('main script').length`); got != hostile.String()+`|Evil <script>" & title|0` {
		t.Fatalf("hostile search: %s", got)
	}
	if err := chromedp.Run(tab, chromedp.SendKeys("#filter-q", "zzz")); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#results .empty-title')?.textContent === 'No albums for “<script>zzz”.'`)
	if got := browserEval(t, tab, `JSON.stringify(window.cspViolations)`); got != "[]" {
		t.Fatalf("CSP violations: %s", got)
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}

	// Without JavaScript: links, a real «Load more», a GET form.
	plain, _ := browserTab(t, root)
	if err := chromedp.Run(plain, emulation.SetScriptExecutionDisabled(true)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, plain, e.srv.URL+"/")
	if got := browserEval(t, plain, `document.querySelectorAll('#grid > .tile').length + '|' + document.querySelector('#grid .tile-link').hasAttribute('role')`); got != "50|false" {
		t.Fatalf("no-JS grid: %s", got)
	}
	if err := chromedp.Run(plain, chromedp.Click("#more", chromedp.NodeVisible), chromedp.WaitReady(`#grid`)); err != nil {
		t.Fatal(err)
	}
	browserWait(t, plain, `location.search.includes('after=') && document.querySelectorAll('#grid > .tile').length === 3`)
	if err := chromedp.Run(plain, chromedp.SetValue("#filter-q", "other"), chromedp.Submit("#filter-q")); err != nil {
		t.Fatal(err)
	}
	browserWait(t, plain, `location.search === '?q=other' && document.querySelectorAll('#grid > .tile').length === 1`)
}

// The «Needs attention» entry and Trash from the sidebar.
func TestBrowserLibraryFixAndTrash(t *testing.T) {
	e, root := browserEnv(t)
	broken := e.seed("Artist", "Broken")
	e.failRender(broken)
	e.seed("Artist", "Fine")
	gone := e.seed("Artist", "Gone")
	_, etag := e.album(gone)
	e.must(req{method: "DELETE", path: "/api/albums/" + gone.String(), ifMatch: etag}, 200)
	e.exec(`UPDATE albums SET cover_hash = NULL`)
	tab, problems := browserTab(t, root)
	browserGo(t, tab, e.srv.URL+"/activity")
	browserWait(t, tab, `document.querySelector('.fix-filter .count')?.textContent === ': 1'`)
	browserEval(t, tab, `document.querySelector('.fix-filter').click();''`)
	browserWait(t, tab, `location.search === '?fix=true' && document.querySelectorAll('#grid > .tile').length === 1`)
	if got := browserEval(t, tab, `document.querySelector('h1').textContent + '|' + document.querySelector('#grid .tile').dataset.album + '|' + document.querySelector('.fix-filter').getAttribute('aria-current')`); got != "Needs attention|"+broken.String()+"|page" {
		t.Fatalf("fix view: %s", got)
	}
	browserEval(t, tab, `[...document.querySelectorAll('.nav a')].find(a => a.textContent === 'Trash').click();''`)
	browserWait(t, tab, `location.search === '?trash=true' && document.querySelector('h1').textContent === 'Trash' && document.querySelectorAll('#grid > .tile').length === 1`)
	if got := browserEval(t, tab, `document.querySelector('#grid .tile').dataset.album + '|' + (document.querySelector('#grid .dot') === null)`); got != gone.String()+"|false" {
		// A trashed album whose removal is still queued shows its activity.
		t.Fatalf("trash view: %s", got)
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}
}

// TestBrowserLibraryScreenshots writes review screenshots of the Library
// (grid, open panel, empty state; light and dark; 1280 and 390 px wide)
// when MUSICLIB_UI_SHOTS names a directory. It is a design review aid, not
// a gate: without the variable it is skipped.
func TestBrowserLibraryScreenshots(t *testing.T) {
	dir := os.Getenv("MUSICLIB_UI_SHOTS")
	if dir == "" {
		t.Skip("MUSICLIB_UI_SHOTS is not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e, root := browserEnv(t)
	type album struct {
		artist, title string
		bg, fg        color.RGBA
		cover         bool
	}
	rgb := func(v uint32) color.RGBA { return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff} }
	var first uuid.UUID
	for i, a := range []album{
		{"Miles Davis", "Kind of Blue", rgb(0x1f3a5f), rgb(0xe8e2d0), true},
		{"John Coltrane", "Blue Train", rgb(0x0d2b45), rgb(0x6fb3d2), true},
		{"Björk", "Homogenic", rgb(0xd9c7b8), rgb(0x3b2a26), true},
		{"Radiohead", "In Rainbows", rgb(0xf2a007), rgb(0x1a1a1a), true},
		{"Sigur Rós", "Ágætis byrjun", rgb(0x8fa3a8), rgb(0x2d3a3d), true},
		{"Nina Simone", "Pastel Blues", rgb(0xc94f3b), rgb(0xf5e9dc), true},
		{"Bill Evans", "Sunday at the Village Vanguard", rgb(0x2b2b2b), rgb(0xd4b483), true},
		{"Caetano Veloso", "Transa", rgb(0), rgb(0), false},
		{"Kraftwerk", "Computer World", rgb(0xe3dd2b), rgb(0x111111), true},
		{"坂本龍一", "音楽図鑑", rgb(0xeeeeee), rgb(0xb22222), true},
		{"Joni Mitchell", "Blue", rgb(0x243b6b), rgb(0x9fb8d9), true},
		{"Fela Kuti", "Zombie", rgb(0), rgb(0), false},
	} {
		id := e.seed(a.artist, a.title)
		if i == 0 {
			first = id
		}
		if a.cover {
			e.setCover(id, coverPNG(t, a.bg, a.fg, 0.38))
		} else {
			e.exec(`UPDATE albums SET cover_hash = NULL WHERE id = $1`, id)
		}
		// Every album its own year, so a year bug cannot hide behind 1959.
		e.exec(`UPDATE albums SET year = $2 WHERE id = $1`, id, 1961+3*i)
		// One album in each state that shows: Error, Processing, Queued.
		switch i {
		case 4:
			e.failRender(id)
		case 6:
			e.runRender(id)
		case 9:
		default:
			e.align(id)
		}
	}
	empty, rootEmpty := browserEnv(t)
	shoot := func(root context.Context, url, name string, width, height int64, dark bool, then string) {
		t.Helper()
		tab, stop := chromedp.NewContext(root)
		defer stop()
		scheme := "light"
		if dark {
			scheme = "dark"
		}
		var shot []byte
		err := chromedp.Run(tab,
			emulation.SetDeviceMetricsOverride(width, height, 2, width < 600),
			emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: scheme}, {Name: "prefers-reduced-motion", Value: "reduce"}}),
			// Every shot starts from the default, expanded sidebar (N-272).
			chromedp.ActionFunc(func(ctx context.Context) error {
				_, err := page.AddScriptToEvaluateOnNewDocument(`try{localStorage.removeItem('musiclib.sidebar')}catch{}`).Do(ctx)
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
			if name == "sidebar-tooltip" {
				// From the focused toggle, past Library, to Import: a keyboard focus.
				browserKey(t, tab, kb.Tab)
				browserKey(t, tab, kb.Tab)
			}
			browserWait(t, tab, `!document.querySelector('.album-panel') || (document.querySelector('.album-panel .panel-tracks') && (!document.querySelector('.album-panel[data-cover]') || document.querySelector('.album-panel').dataset.painted))`)
			time.Sleep(600 * time.Millisecond)
		}
		if err := chromedp.Run(tab, chromedp.CaptureScreenshot(&shot)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%d-%s.png", name, width, scheme)), shot, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	open := `document.querySelector('[data-album="` + first.String() + `"] .tile-link').click()`
	for _, dark := range []bool{false, true} {
		for _, w := range []int64{1280, 390} {
			h := int64(900)
			if w < 600 {
				h = 844
			}
			shoot(root, e.srv.URL+"/", "grid", w, h, dark, "")
			shoot(root, e.srv.URL+"/", "scrolled", w, h, dark, "scrollTo(0, 260)")
			shoot(root, e.srv.URL+"/", "panel", w, h, dark, open)
			shoot(rootEmpty, empty.srv.URL+"/", "empty", w, h, dark, "")
			shoot(root, e.srv.URL+"/albums/"+first.String(), "album", w, h, dark, "")
			shoot(root, e.srv.URL+"/activity", "activity", w, h, dark, "")
			shoot(root, e.srv.URL+"/import", "import", w, h, dark, "")
			if w > 600 {
				// The sidebar: expanded on Activity, collapsed, and collapsed
				// with the tooltip of a focused entry.
				toggle := `document.querySelector('#sidebar-toggle').click()`
				shoot(root, e.srv.URL+"/activity", "sidebar-expanded", w, h, dark, "")
				shoot(root, e.srv.URL+"/", "sidebar-collapsed", w, h, dark, toggle)
				shoot(root, e.srv.URL+"/", "sidebar-tooltip", w, h, dark, toggle+`;document.querySelector('#sidebar-toggle').focus()`)
			}
		}
	}
}
