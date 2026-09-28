package http

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// The Library head compacts while it is stuck (owner, NOTES.md N-284):
// stuck is compact, back at the top is full; the page never gets shorter,
// so a slow scroll across the threshold cannot make it oscillate; the
// search does not move sideways; nothing moves with reduced motion; the
// same on a phone; and the IntersectionObserver fallback of library.js
// does the same where scroll-state queries are missing.

type headState struct {
	Font    float64 `json:"font"`
	Flow    float64 `json:"flow"`
	Doc     float64 `json:"doc"`
	Y       float64 `json:"y"`
	SearchX float64 `json:"searchX"`
	Line    string  `json:"line"`
	Stuck   bool    `json:"stuck"`
	Moves   string  `json:"moves"`
}

const headStateJS = `JSON.stringify((()=>{const h=document.querySelector('.page-head'),b=h.querySelector('.head-bar'),t=h.querySelector('.page-title');
return {font:parseFloat(getComputedStyle(t).fontSize), flow:b.getBoundingClientRect().height+parseFloat(getComputedStyle(b).marginBottom), doc:document.documentElement.scrollHeight,
y:scrollY, searchX:h.querySelector('.search').getBoundingClientRect().left, line:getComputedStyle(t,'::after').content, stuck:h.classList.contains('is-stuck'), moves:getComputedStyle(b).transitionDuration}})())`

func readHead(t *testing.T, tab context.Context) headState {
	t.Helper()
	var s headState
	if err := json.Unmarshal([]byte(browserEval(t, tab, headStateJS)), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBrowserLibraryHeadCompacts(t *testing.T) {
	e, root := browserEnv(t)
	for i := range 40 {
		e.seed("Artist", fmt.Sprintf("Album %02d", i))
	}
	e.exec(`UPDATE albums SET cover_hash = NULL`)
	for _, c := range []struct {
		width            int64
		fullFont, flow   float64
		reduced, noState bool
	}{
		{1280, 34, 97, false, false},
		{390, 34, 127, false, false},
		{1280, 34, 97, true, false},
		{1280, 34, 97, false, true},
		{390, 34, 127, false, true},
	} {
		name := fmt.Sprintf("%dpx reduced=%v fallback=%v", c.width, c.reduced, c.noState)
		tab, problems := browserTab(t, root)
		motion := "no-preference"
		if c.reduced {
			motion = "reduce"
		}
		err := chromedp.Run(tab,
			emulation.SetDeviceMetricsOverride(c.width, 700, 1, c.width < 600),
			emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-reduced-motion", Value: motion}}),
			chromedp.ActionFunc(func(ctx context.Context) error {
				if !c.noState {
					return nil
				}
				// A browser without scroll-state queries, as library.js sees it.
				_, err := page.AddScriptToEvaluateOnNewDocument(`{const s=CSS.supports.bind(CSS);Object.defineProperty(CSS,'supports',{value:(...a)=>String(a.join(':')).includes('scroll-state')?false:s(...a)})}`).Do(ctx)
				return err
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		browserGo(t, tab, e.srv.URL+"/")
		browserWait(t, tab, `document.querySelectorAll('#grid > .tile').length === 40 && document.fonts.status === 'loaded'`)
		top := browserEval(t, tab, `String(document.querySelector('.page-head').getBoundingClientRect().top + scrollY)`)
		var threshold float64
		fmt.Sscan(top, &threshold)
		full := readHead(t, tab)
		wantMoves := "0.2s, 0.2s, 0.2s" // padding, margin, row-gap: the one short duration
		if c.reduced {
			wantMoves = "0s"
		}
		if full.Font != c.fullFont || full.Flow != c.flow || full.Line != "none" || full.Moves != wantMoves {
			t.Fatalf("%s: at the top %+v", name, full)
		}
		// A slow scroll across the threshold, one pixel at a time, down then
		// up: compact exactly when stuck, the page as long, the scroll where
		// it was put, the search in its place.
		settle := 260 * time.Millisecond
		if c.reduced {
			settle = 60 * time.Millisecond
		}
		check := func(y float64) {
			t.Helper()
			browserEval(t, tab, fmt.Sprintf(`scrollTo(0, %v);''`, y))
			time.Sleep(settle)
			s := readHead(t, tab)
			stuck := y > threshold
			wantFont := c.fullFont
			if stuck {
				wantFont = 17
			}
			if s.Font != wantFont || s.Flow != full.Flow || s.Doc != full.Doc || s.Y != y || s.SearchX != full.SearchX ||
				(stuck != (s.Line != "none")) || (c.noState && s.Stuck != stuck) {
				t.Fatalf("%s: at %v (threshold %v): %+v, at the top %+v", name, y, threshold, s, full)
			}
		}
		for y := max(0, threshold-3); y <= threshold+6; y++ {
			check(y)
		}
		for y := threshold + 5; y >= max(0, threshold-3); y-- {
			check(y)
		}
		// Just past the threshold, watched for a second: the title only ever
		// shrinks, nothing else moves.
		browserEval(t, tab, fmt.Sprintf(`scrollTo(0, 0);''`))
		time.Sleep(settle)
		browserEval(t, tab, fmt.Sprintf(`window.samples=[];const title=document.querySelector('.page-title'),t0=performance.now();scrollTo(0, %v);(function f(){samples.push([scrollY, parseFloat(getComputedStyle(title).fontSize), document.documentElement.scrollHeight]);if(performance.now()-t0<1000)setTimeout(f,8)})();''`, threshold+1))
		time.Sleep(1200 * time.Millisecond)
		var samples [][3]float64
		if err := json.Unmarshal([]byte(browserEval(t, tab, `JSON.stringify(samples)`)), &samples); err != nil {
			t.Fatal(err)
		}
		if len(samples) < 20 {
			t.Fatalf("%s: %d samples", name, len(samples))
		}
		for i, s := range samples {
			if s[0] != threshold+1 || s[2] != full.Doc || (i > 0 && s[1] > samples[i-1][1]) {
				t.Fatalf("%s: sample %d of %d oscillates: %v (previous %v)", name, i, len(samples), s, samples[max(0, i-1)])
			}
		}
		if last := samples[len(samples)-1]; last[1] != 17 {
			t.Fatalf("%s: not compact after a second: %v", name, last)
		}
		// Clicks under the head's given-back margin reach the grid.
		if c.width > 600 && !c.noState {
			browserEval(t, tab, `scrollTo(0, 400);''`)
			time.Sleep(settle)
			if got := browserEval(t, tab, `(()=>{const b=document.querySelector('.head-bar').getBoundingClientRect();const el=document.elementFromPoint(b.left+b.width/2, b.bottom+10);return String(!document.querySelector('.page-head').contains(el))})()`); got != "true" {
				t.Fatalf("%s: the head's margin takes clicks", name)
			}
		}
		if p := problems(); len(p) != 0 {
			t.Fatalf("%s: console: %q", name, p)
		}
	}
}
