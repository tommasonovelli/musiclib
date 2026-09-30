package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// The sidebar in a real Chromium (round 20b, NOTES.md N-272): it collapses
// to its icons from a real button, remembers it across navigations and
// reloads without moving on load, keeps the labels as accessible names,
// and gives way to the row of views on a phone.

// sidebarProbe records, from the first script of every document, the state
// <html> carries when the sidebar is parsed and every width transition the
// sidebar runs: a transition on load would be the flash.
const sidebarProbe = `window.sidebarRuns=[];document.addEventListener('transitionrun',e=>{if(e.target.id==='sidebar')window.sidebarRuns.push(e.propertyName)});new MutationObserver((records,observer)=>{if(document.getElementById('sidebar')){window.sidebarAtParse=document.documentElement.dataset.sidebar||'none';observer.disconnect()}}).observe(document,{childList:true,subtree:true});`

func onNewDocument(t *testing.T, tab context.Context, script string) {
	t.Helper()
	err := chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
		return err
	}))
	if err != nil {
		t.Fatal(err)
	}
}

// accessibleNames are the names Chromium's accessibility tree computes for
// the elements a selector matches, in document order.
func accessibleNames(t *testing.T, tab context.Context, selector string) []string {
	t.Helper()
	var nodes []*cdp.Node
	if err := chromedp.Run(tab, chromedp.Nodes(selector, &nodes, chromedp.ByQueryAll)); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range nodes {
		err := chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error {
			ax, err := accessibility.GetPartialAXTree().WithBackendNodeID(n.BackendNodeID).WithFetchRelatives(false).Do(ctx)
			if err != nil {
				return err
			}
			var name string
			if len(ax) > 0 && ax[0].Name != nil {
				if err := json.Unmarshal(ax[0].Name.Value, &name); err != nil {
					return err
				}
			}
			// Chromium separates an out-of-flow box from its neighbour
			// with a space ("Needs attention : 1" when only the label is
			// positioned); a screen reader reads both the same.
			names = append(names, strings.ReplaceAll(name, " :", ":"))
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
	}
	return names
}

// sidebarState is the sidebar as the user and the tests see it.
const sidebarState = `(()=>{const b=document.querySelector('#sidebar-toggle');let s;try{s=localStorage.getItem('musiclib.sidebar')}catch{s='blocked'}return [document.documentElement.dataset.sidebar,getComputedStyle(document.querySelector('#sidebar')).width,b.getAttribute('aria-expanded'),b.getAttribute('aria-controls'),b.textContent,s].join('|')})()`

func TestBrowserSidebarCollapse(t *testing.T) {
	e, root := browserEnv(t)
	broken := e.seed("Artist", "Broken")
	e.failRender(broken)
	e.exec(`UPDATE albums SET cover_hash = NULL`)
	tab, problems := browserTab(t, root)
	onNewDocument(t, tab, sidebarProbe)
	if err := chromedp.Run(tab, chromedp.EmulateViewport(1280, 900)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, tab, e.srv.URL+"/")
	browserWait(t, tab, `document.readyState === 'complete'`)
	if got := browserEval(t, tab, sidebarState+`+'|'+window.sidebarAtParse`); got != "expanded|248px|true|sidebar|Collapse sidebar||expanded" {
		t.Fatalf("first visit: %s", got)
	}
	// Expanded: icon and label on one grid, the labels visible.
	if got := browserEval(t, tab, `JSON.stringify([...document.querySelectorAll('.nav a, .fix-filter')].map(a=>{const i=a.querySelector('.icon').getBoundingClientRect(),l=a.querySelector('.nav-label');return [Math.round(i.left),i.width,i.height,Math.round(l.getBoundingClientRect().left),getComputedStyle(l).clipPath]}))`); got != `[[26,20,20,60,"none"],[26,20,20,60,"none"],[26,20,20,60,"none"],[26,20,20,60,"none"],[26,20,20,60,"none"]]` {
		t.Fatalf("expanded rows: %s", got)
	}
	if got := strings.Join(accessibleNames(t, tab, `#sidebar-toggle, .nav a, .fix-filter`), "|"); got != "Library|Import|Activity|Trash|Needs attention: 1|Collapse sidebar" {
		t.Fatalf("expanded accessible names: %s", got)
	}

	// Keyboard: the skip link, the brand, the five entries, then sign-out,
	// the theme button and the toggle at the foot of the sidebar; Enter
	// collapses.
	for range 10 {
		browserKey(t, tab, kb.Tab)
	}
	if got := browserEval(t, tab, `document.activeElement.id`); got != "sidebar-toggle" {
		t.Fatalf("tenth tab stop: %q", got)
	}
	browserKey(t, tab, kb.Enter)
	browserWait(t, tab, `getComputedStyle(document.querySelector('#sidebar')).width === '72px'`)
	if got := browserEval(t, tab, sidebarState+`+'|'+window.sidebarRuns.join()`); got != "collapsed|72px|false|sidebar|Expand sidebar|collapsed|width" {
		t.Fatalf("collapsed: %s", got)
	}
	// Icons only, in the same place; the labels are visually hidden but
	// still the accessible names; the count is a badge on its icon; the
	// active entry stays marked.
	if got := browserEval(t, tab, `JSON.stringify([...document.querySelectorAll('.nav a, .fix-filter')].map(a=>{const i=a.querySelector('.icon').getBoundingClientRect(),l=getComputedStyle(a.querySelector('.nav-label'));return [Math.round(i.left),Math.round(a.getBoundingClientRect().width),l.position,l.clipPath]}))`); got != `[[26,48,"absolute","inset(50%)"],[26,48,"absolute","inset(50%)"],[26,48,"absolute","inset(50%)"],[26,48,"absolute","inset(50%)"],[26,48,"absolute","inset(50%)"]]` {
		t.Fatalf("collapsed rows: %s", got)
	}
	if got := strings.Join(accessibleNames(t, tab, `#sidebar-toggle, .nav a, .fix-filter`), "|"); got != "Library|Import|Activity|Trash|Needs attention: 1|Expand sidebar" {
		t.Fatalf("accessible names: %s", got)
	}
	if got := browserEval(t, tab, `(()=>{const c=document.querySelector('.fix-filter .count'),s=getComputedStyle(c),i=document.querySelector('.fix-filter .icon').getBoundingClientRect(),r=c.getBoundingClientRect(),a=getComputedStyle(document.querySelector('.nav a[aria-current=page]'));return [c.textContent,s.position,s.backgroundColor,r.left<i.right&&r.right>i.right&&r.top<i.top+8,a.backgroundColor!=='rgba(0, 0, 0, 0)',getComputedStyle(document.querySelector('.nav a[aria-current=page] .icon')).color].join('|')})()`); got != ": 1|absolute|rgb(215, 0, 21)|true|true|rgb(29, 29, 31)" {
		t.Fatalf("badge and active entry: %s", got)
	}
	// From the brand (its symbol stays), Tab reaches the first view and
	// shows its label as a tooltip beside the icon.
	browserEval(t, tab, `document.querySelector('.brand').focus();''`)
	browserKey(t, tab, kb.Tab)
	if got := browserEval(t, tab, `(()=>{const a=document.activeElement,l=getComputedStyle(a.querySelector('.nav-label')),r=a.querySelector('.nav-label').getBoundingClientRect();return [a.getAttribute('href'),l.clipPath,r.left>=document.querySelector('#sidebar').getBoundingClientRect().right-8,r.width>40].join('|')})()`); got != "/|none|true|true" {
		t.Fatalf("tooltip on focus: %s", got)
	}

	// Remembered across a navigation and a reload, applied before the
	// sidebar is parsed, and nothing animates on load.
	browserEval(t, tab, `document.querySelector('.nav a[href="/import"]').click();''`)
	browserWait(t, tab, `location.pathname === '/import' && document.readyState === 'complete'`)
	check := func(when string) {
		t.Helper()
		if got := browserEval(t, tab, sidebarState+`+'|'+window.sidebarAtParse+'|'+window.sidebarRuns.length`); got != "collapsed|72px|false|sidebar|Expand sidebar|collapsed|collapsed|0" {
			t.Fatalf("%s: %s", when, got)
		}
	}
	check("after navigating")
	if err := chromedp.Run(tab, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.readyState === 'complete'`)
	check("after reloading")
	if got := browserEval(t, tab, `document.querySelector('.nav a[aria-current=page]').getAttribute('href')`); got != "/import" {
		t.Fatalf("active entry: %s", got)
	}

	// A click expands it again, and that is remembered too.
	browserEval(t, tab, `document.querySelector('#sidebar-toggle').click();''`)
	browserWait(t, tab, `getComputedStyle(document.querySelector('#sidebar')).width === '248px'`)
	if got := browserEval(t, tab, sidebarState); got != "expanded|248px|true|sidebar|Collapse sidebar|expanded" {
		t.Fatalf("expanded again: %s", got)
	}
	if got := browserEval(t, tab, `JSON.stringify(window.cspViolations)`); got != "[]" {
		t.Fatalf("CSP violations: %s", got)
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}

	// With reduced motion the width changes at once.
	quiet, quietProblems := browserTab(t, root)
	onNewDocument(t, quiet, sidebarProbe)
	if err := chromedp.Run(quiet, chromedp.EmulateViewport(1280, 900), emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-reduced-motion", Value: "reduce"}})); err != nil {
		t.Fatal(err)
	}
	browserGo(t, quiet, e.srv.URL+"/activity")
	browserEval(t, quiet, `document.querySelector('#sidebar-toggle').click();''`)
	if got := browserEval(t, quiet, `getComputedStyle(document.querySelector('#sidebar')).width+'|'+window.sidebarRuns.length`); got != "72px|0" {
		t.Fatalf("reduced motion: %s", got)
	}
	browserEval(t, quiet, `document.querySelector('#sidebar-toggle').click();''`)
	if p := quietProblems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}

	// Storage that throws: expanded, and the toggle still works on the page.
	blocked, blockedProblems := browserTab(t, root)
	onNewDocument(t, blocked, `Object.defineProperty(window,'localStorage',{get(){throw new DOMException('blocked','SecurityError')}});`)
	if err := chromedp.Run(blocked, chromedp.EmulateViewport(1280, 900)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, blocked, e.srv.URL+"/")
	browserWait(t, blocked, `document.readyState === 'complete'`)
	if got := browserEval(t, blocked, sidebarState); got != "expanded|248px|true|sidebar|Collapse sidebar|blocked" {
		t.Fatalf("blocked storage: %s", got)
	}
	browserEval(t, blocked, `document.querySelector('#sidebar-toggle').click();''`)
	browserWait(t, blocked, `getComputedStyle(document.querySelector('#sidebar')).width === '72px'`)
	if p := blockedProblems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}

	// Without JavaScript: expanded, working links, no toggle to press.
	plain, _ := browserTab(t, root)
	if err := chromedp.Run(plain, emulation.SetScriptExecutionDisabled(true), chromedp.EmulateViewport(1280, 900)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, plain, e.srv.URL+"/")
	if got := browserEval(t, plain, `[getComputedStyle(document.querySelector('#sidebar')).width,getComputedStyle(document.querySelector('#sidebar-toggle')).visibility,getComputedStyle(document.querySelector('.nav .nav-label')).clipPath,'sidebar' in document.documentElement.dataset].join('|')`); got != "248px|hidden|none|false" {
		t.Fatalf("no-JS sidebar: %s", got)
	}
	if err := chromedp.Run(plain, chromedp.Click(`.nav a[href="/activity"]`, chromedp.NodeVisible)); err != nil {
		t.Fatal(err)
	}
	browserWait(t, plain, `location.pathname === '/activity'`)
}

// On a phone the sidebar is a row of the four views, icon over label, with
// Needs attention as its icon and count, and no toggle, whatever the
// remembered state.
func TestBrowserSidebarPhone(t *testing.T) {
	e, root := browserEnv(t)
	broken := e.seed("Artist", "Broken")
	e.failRender(broken)
	e.exec(`UPDATE albums SET cover_hash = NULL`)
	tab, problems := browserTab(t, root)
	onNewDocument(t, tab, `try{localStorage.setItem('musiclib.sidebar','collapsed')}catch{}`)
	if err := chromedp.Run(tab, chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, tab, e.srv.URL+"/activity")
	browserWait(t, tab, `document.readyState === 'complete'`)
	got := browserEval(t, tab, `(()=>{const s=document.querySelector('#sidebar').getBoundingClientRect(),items=[...document.querySelectorAll('.nav a')].map(a=>{const i=a.querySelector('.icon').getBoundingClientRect(),l=a.querySelector('.nav-label').getBoundingClientRect();return [a.querySelector('.nav-label').textContent,i.width,Math.round(i.top-s.top)===Math.round(document.querySelector('.nav a .icon').getBoundingClientRect().top-s.top),l.top>=i.bottom,Math.abs((i.left+i.right)/2-(l.left+l.right)/2)<1]});return JSON.stringify([getComputedStyle(document.querySelector('.brand')).display,Math.round(s.width),document.documentElement.scrollWidth<=390,items,getComputedStyle(document.querySelector('.fix-filter .nav-label')).clipPath,document.querySelector('.fix-filter').getBoundingClientRect().top<s.bottom])})()`)
	if got != `["none",390,true,[["Library",20,true,true,true],["Import",20,true,true,true],["Activity",20,true,true,true],["Trash",20,true,true,true]],"inset(50%)",true]` {
		t.Fatalf("phone row: %s", got)
	}
	if got := strings.Join(accessibleNames(t, tab, `.nav a, .fix-filter`), "|"); got != "Library|Import|Activity|Trash|Needs attention: 1" {
		t.Fatalf("phone names: %s", got)
	}
	if got := browserEval(t, tab, `JSON.stringify(window.cspViolations)`); got != "[]" {
		t.Fatalf("CSP violations: %s", got)
	}
	if p := problems(); len(p) != 0 {
		t.Fatalf("console: %q", p)
	}
}

// The album page adopts its fresh server HTML after every change (N-260):
// the collapsed sidebar and its toggle's state survive it, the current
// view follows the album into the trash, and the fixed Save bar starts at
// the sidebar's edge in both states and spans the page on a phone
// (N-278).
func TestBrowserSidebarAlbumRefresh(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue(), nil)
	url := e.srv.URL + "/albums/" + id.String()
	tab, problems := browserTab(t, root)
	onNewDocument(t, tab, sidebarProbe)
	if err := chromedp.Run(tab, chromedp.EmulateViewport(1280, 900)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, tab, url)
	browserWait(t, tab, `document.querySelector('#album-title') && !document.querySelector('#album-title').readOnly`)
	edge := `(()=>{const b=document.querySelector('#savebar').getBoundingClientRect(),s=document.querySelector('#sidebar').getBoundingClientRect();return [Math.round(b.left),Math.round(s.right),Math.round(b.right)].join('|')})()`
	if got := browserEval(t, tab, edge); got != "248|248|1280" {
		t.Fatalf("expanded Save bar: %s", got)
	}
	browserEval(t, tab, `document.querySelector('#sidebar-toggle').click();''`)
	browserWait(t, tab, `getComputedStyle(document.querySelector('#sidebar')).width === '72px' && getComputedStyle(document.querySelector('#savebar')).left === '72px'`)
	// From here on no width transition may run: the refresh must not
	// touch the state (the toggle's own transition is not the point, and
	// a background tab may not run it at all).
	browserEval(t, tab, `window.sidebarRuns=[];''`)

	// Save in place: the page adopts its fresh HTML; the sidebar stays
	// collapsed, its toggle says so, and nothing animates.
	js(t, tab, `set($('#album-title'), 'Kind of Blue (Mono)');return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === '1 change'`)
	if got := browserEval(t, tab, edge); got != "72|72|1280" {
		t.Fatalf("collapsed Save bar: %s", got)
	}
	js(t, tab, `$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === 'Saved'`)
	if body, _ := e.album(id); body["title"] != "Kind of Blue (Mono)" {
		t.Fatalf("saved title: %v", body["title"])
	}
	after := func(when, current string) {
		t.Helper()
		if got := browserEval(t, tab, sidebarState+`+'|'+window.sidebarRuns.join()+'|'+document.querySelector('.nav a[aria-current=page]').textContent+'|'+document.querySelectorAll('#sidebar-toggle').length`); got != "collapsed|72px|false|sidebar|Expand sidebar|collapsed||"+current+"|1" {
			t.Fatalf("%s: %s", when, got)
		}
	}
	after("after a save", "Library")

	// Into the trash: the current view becomes Trash, the state holds.
	js(t, tab, `$('.album-head > .dots').click();[...$$('#album-menu button')].find(b=>b.textContent==='Move to trash').click();return ''`)
	browserWait(t, tab, `document.querySelector('#ask').open`)
	js(t, tab, `$('#ask-ok').click();return ''`)
	browserWait(t, tab, `document.querySelector('.band') !== null`)
	after("after the trash", "Trash")

	// The toggle still works on the adopted page, and the bar follows.
	browserEval(t, tab, `document.querySelector('#sidebar-toggle').click();''`)
	browserWait(t, tab, `getComputedStyle(document.querySelector('#sidebar')).width === '248px' && getComputedStyle(document.querySelector('#savebar')).left === '248px'`)
	if got := browserEval(t, tab, sidebarState); got != "expanded|248px|true|sidebar|Collapse sidebar|expanded" {
		t.Fatalf("expanded again: %s", got)
	}
	noProblems(t, tab, problems)

	// A phone: the row of views on top, the Save bar across the page,
	// whatever the remembered state.
	phone, phoneProblems := browserTab(t, root)
	onNewDocument(t, phone, `try{localStorage.setItem('musiclib.sidebar','collapsed')}catch{}`)
	if err := chromedp.Run(phone, chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile)); err != nil {
		t.Fatal(err)
	}
	browserGo(t, phone, url)
	browserWait(t, phone, `document.querySelector('#album-title') && !document.querySelector('#album-title').readOnly`)
	js(t, phone, `set($('#album-title'), 'Kind of Blue');return ''`)
	browserWait(t, phone, `document.querySelector('#changes').textContent === '1 change'`)
	if got := browserEval(t, phone, `(()=>{const b=document.querySelector('#savebar').getBoundingClientRect(),s=document.querySelector('#sidebar').getBoundingClientRect();return [Math.round(b.left),Math.round(b.right),Math.round(s.width),document.documentElement.scrollWidth<=390].join('|')})()`); got != "0|390|390|true" {
		t.Fatalf("phone: %s", got)
	}
	noProblems(t, phone, phoneProblems)
}
