package http

import (
	"testing"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// The Duration column (round 22, NOTES.md N-302) in a real Chromium: read
// only, right-aligned tabular figures in the Note style, an en dash with
// words for an unknown one, the total under the table; never an edit, so
// never dirty, before or after a save; on a phone it sits beside the title
// without widening the page; the Library's open album shows the same times.
func TestBrowserTrackDurations(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seedAlbum("Miles Davis", "Kind of Blue", kindOfBlue(), nil)
	tab, problems := albumTab(t, root, e.srv.URL+"/albums/"+id.String())
	if err := chromedp.Run(tab, emulation.SetDeviceMetricsOverride(1280, 900, 1, false)); err != nil {
		t.Fatal(err)
	}
	cells := `$$('.t-time').map(c=>c.textContent).join(',')`
	want := "Duration 9:22,Duration 9:46,Duration 5:37,Duration 11:33,Duration 9:26,–Duration unknown"
	if got := js(t, tab, `return `+cells); got != want {
		t.Fatalf("cells: %s", got)
	}
	style := `(()=>{const c=$$('.t-time'),s=getComputedStyle(c[0]),r=c.map(x=>Math.round(x.getBoundingClientRect().right));` +
		`return [s.textAlign,s.fontVariantNumeric,s.fontSize,s.color===getComputedStyle($('#status')).color||s.color,new Set(r).size,` +
		`c[5].querySelector('[aria-hidden=true]').textContent,c[5].querySelector('.sr-only').textContent,c.some(x=>x.querySelector('input,textarea,[name]')),$('#length').textContent].join('|')})()`
	if got := js(t, tab, `return `+style); got != "right|tabular-nums|13px|true|1|–|Duration unknown|false|6 tracks" {
		t.Fatalf("laptop column: %s", got)
	}
	// Loaded, nothing to save; the column is no field.
	if got := js(t, tab, `return [$('#changes').textContent, $('#save').disabled, [...$('#metadata').elements].some(f=>f.closest('.t-time'))].join('|')`); got != "|true|false" {
		t.Fatalf("at load: %s", got)
	}
	// A save, and the page adopting its server HTML: the times stay, and
	// nothing is counted as an edit afterwards.
	js(t, tab, `set(row(0).querySelector('[name=title]'),'So What (Take 3)');$('#save').click();return ''`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === 'Saved'`)
	browserWait(t, tab, `document.querySelector('#changes').textContent === '' && !document.querySelector('#savebar').classList.contains('is-on')`)
	if got := js(t, tab, `return `+cells+`+'|'+$('#save').disabled`); got != want+"|true" {
		t.Fatalf("after the save: %s", got)
	}
	if trackOf(t, e, id, 0)["duration_ms"] != 562_000.0 {
		t.Fatal("the save changed the duration")
	}
	// A reload: the same.
	browserGo(t, tab, e.srv.URL+"/albums/"+id.String())
	browserWait(t, tab, `!document.querySelector('#album-title').readOnly`)
	if got := js(t, tab, `return `+cells+`+'|'+$('#changes').textContent`); got != want+"|" {
		t.Fatalf("after a reload: %s", got)
	}

	// The phone: the time on the title's line, at the right, the lyrics
	// under it, no horizontal scroll.
	if err := chromedp.Run(tab, emulation.SetDeviceMetricsOverride(390, 844, 2, true)); err != nil {
		t.Fatal(err)
	}
	phone := `(()=>{const r=row(0),b=x=>r.querySelector(x).getBoundingClientRect(),tm=b('.t-time'),ti=b('[name=title]'),ly=b('.t-lyrics'),d=b('.dots'),rr=r.getBoundingClientRect();` +
		`return [Math.abs((tm.top+tm.bottom)/2-(ti.top+ti.bottom)/2)<4, ti.right<=tm.left, tm.right<=d.left+1, tm.right<=rr.right, ly.top>=tm.bottom-1, document.documentElement.scrollWidth<=innerWidth, $$('.t-time').every(c=>c.checkVisibility())].join('|')})()`
	if got := js(t, tab, `return `+phone); got != "true|true|true|true|true|true|true" {
		t.Fatalf("phone layout: %s", got)
	}
	noProblems(t, tab, problems)

	// The Library's open album: the same times, the dash with its words.
	lib, libProblems := browserTab(t, root)
	browserGo(t, lib, e.srv.URL+"/")
	browserEval(t, lib, `document.querySelector('.tile[data-album="`+id.String()+`"] .tile-link').click();''`)
	browserWait(t, lib, `document.querySelectorAll('#album-panel .panel-tracks .track-time').length === 6`)
	if got := browserEval(t, lib, `[...document.querySelectorAll('#album-panel .track-time')].map(c=>c.textContent).join(',')+'|'+getComputedStyle(document.querySelector('#album-panel .track-time')).fontVariantNumeric`); got != "Duration 9:22,Duration 9:46,Duration 5:37,Duration 11:33,Duration 9:26,–Duration unknown|tabular-nums" {
		t.Fatalf("panel: %s", got)
	}
	noProblems(t, lib, libProblems)
}
