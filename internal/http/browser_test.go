package http

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Browser tests run the embedded module in a real, pinned Chromium against
// the real HTTP handler and PostgreSQL. No DOM emulation or JS mock is used.
func browserEnv(t *testing.T) (*env, context.Context) {
	t.Helper()
	e := newEnv(t)
	e.srv.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", e.api)
	mux.HandleFunc("/", e.api.Pages)
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)
	e.srv = srv
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath("/usr/bin/chromium"), chromedp.Env(append(os.Environ(), "HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir(), "XDG_CACHE_HOME="+t.TempDir())...), chromedp.NoSandbox, chromedp.Flag("disable-dev-shm-usage", true), chromedp.Flag("no-proxy-server", true), chromedp.Flag("host-resolver-rules", "MAP music.test 127.0.0.1"))
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	t.Cleanup(cancel)
	ctx, stop := chromedp.NewContext(alloc)
	t.Cleanup(stop)
	ctx, timeout := context.WithTimeout(ctx, 90*time.Second)
	t.Cleanup(timeout)
	return e, ctx
}

func browserEval(t *testing.T, ctx context.Context, expression string) string {
	t.Helper()
	var result string
	if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &result)); err != nil {
		t.Fatal(err)
	}
	return result
}
func browserWait(t *testing.T, ctx context.Context, expression string) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if err := chromedp.Run(ctx, chromedp.Poll(expression, nil, chromedp.WithPollingTimeout(2*time.Second))); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond) // A successful action navigates to its new server-rendered page.
	}
	var debug string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify({marker:window.navigationMarker, error:document.querySelector('#error')?.textContent, hidden:document.querySelector('#error')?.hidden, cover:!!document.querySelector('.cover'), etag:document.querySelector('#editor')?.dataset.etag, options:[...document.querySelectorAll('#cover-choice option')].map(o=>o.textContent)})`, &debug))
	t.Fatalf("browser condition timed out: %s; state: %s", expression, debug)
}

func TestBrowserEditorConflictAndContent(t *testing.T) {
	if _, err := os.Stat("/usr/bin/chromium"); err != nil {
		t.Fatal("the gate must provide Chromium: ", err)
	}
	e, root := browserEnv(t)
	id := e.seed("Browser Artist", "First title")
	url := testOrigin + "/albums/" + id.String()
	tab1, stop1 := chromedp.NewContext(root)
	defer stop1()
	tab2, stop2 := chromedp.NewContext(root)
	defer stop2()
	for _, ctx := range []context.Context{tab1, tab2} {
		if err := chromedp.Run(ctx, chromedp.Navigate(url), chromedp.WaitVisible("#editor:not([hidden])")); err != nil {
			t.Fatal(err)
		}
	}
	browserEval(t, tab1, `document.querySelector('#metadata [name=title]').value='Winner'; document.querySelector('#metadata button[type=submit]').click(); ''`)
	browserWait(t, tab1, `document.querySelector('#metadata [name=title]')?.value === 'Winner' && document.querySelector('#error')?.hidden`)
	browserEval(t, tab2, `document.querySelector('#metadata [name=title]').value='Loser'; document.querySelector('#metadata button[type=submit]').click(); ''`)
	browserWait(t, tab2, `!document.querySelector('#error').hidden`)
	if got := browserEval(t, tab2, `document.querySelector('#error').textContent + '|' + document.querySelector('#metadata [name=title]').value`); !strings.Contains(got, "precondition_failed") || !strings.Contains(got, "Loser") {
		t.Fatalf("conflict lost edits: %s", got)
	}
	if got := e.must(req{method: "GET", path: "/api/albums/" + id.String()}, 200).body["title"]; got != "Winner" {
		t.Fatalf("overwrote winner: %v", got)
	}
	// A cancelled confirmation must not send a deletion, even on a clean form.
	browserEval(t, tab1, `window.confirm=()=>false; document.querySelector('[data-action=track-delete]').click(); ''`)
	time.Sleep(400 * time.Millisecond)
	if got := len(e.must(req{method: "GET", path: "/api/albums/" + id.String()}, 200).body["tracks"].([]any)); got != 2 {
		t.Fatalf("cancelled deletion removed track: %d", got)
	}
	// Inheritance is an explicit command, not an empty-string override.
	browserEval(t, tab1, `const row=document.querySelectorAll('[data-track]')[1]; row.querySelector('[name=genre]').value='Other'; row.querySelector('[name=genre]').dispatchEvent(new Event('input',{bubbles:true})); row.querySelector('[data-inherit=genre]').click(); ''`)
	if got := browserEval(t, tab1, `document.querySelectorAll('[data-track]')[1].querySelector('[name=genre]').dataset.inherited`); got != "true" {
		t.Fatalf("inherit: %q", got)
	}
	browserEval(t, tab1, `window.navigationMarker='before'; document.querySelector('#metadata button[type=submit]').click(); ''`)
	browserWait(t, tab1, `typeof window.navigationMarker === 'undefined' && document.querySelectorAll('[data-track]')[1]?.querySelector('[name=genre]').dataset.inherited === 'true' && document.querySelector('#editor')?.dataset.etag.includes(':3')`)
	tracks := e.must(req{method: "GET", path: "/api/albums/" + id.String()}, 200).body["tracks"].([]any)
	if tracks[1].(map[string]any)["genre"] != nil {
		t.Fatalf("inherit did not save null: %v", tracks[1])
	}
	browserEval(t, tab1, `window.navigationMarker='before'; window.confirm=()=>true; document.querySelector('[data-action=track-delete]').click(); ''`)
	browserWait(t, tab1, `typeof window.navigationMarker === 'undefined' && document.querySelectorAll('[data-track]').length===1`)
	// The import fixture records image metadata but has no image bytes on disk.
	// Upload a real image attachment before choosing it as the cover.
	file := filepath.Join(t.TempDir(), "cover.jpg")
	if err := os.WriteFile(file, jpegImage(t, 32, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(tab1, chromedp.SetUploadFiles("#attachment-file", []string{file})); err != nil {
		t.Fatal(err)
	}
	browserEval(t, tab1, `window.navigationMarker='before'; document.querySelector('#attachment-path').value='real.jpg'; document.querySelector('[data-action=attachment-upload]').click(); ''`)
	browserWait(t, tab1, `typeof window.navigationMarker === 'undefined' && [...document.querySelector('#cover-choice').options].some(option=>option.textContent==='real.jpg')`)
	// Choose the uploaded image attachment, then remove and upload a cover; trash and restore.
	browserEval(t, tab1, `window.navigationMarker='before'; const choice=document.querySelector('#cover-choice'); choice.value=[...choice.options].find(option=>option.textContent==='real.jpg').value; document.querySelector('[data-action=cover-choice]').click(); ''`)
	browserWait(t, tab1, `typeof window.navigationMarker === 'undefined' && document.querySelector('.cover') !== null`)
	browserEval(t, tab1, `window.confirm=()=>true; document.querySelector('[data-action=cover-remove]').click(); ''`)
	browserWait(t, tab1, `document.querySelector('.cover') === null`)
	if err := chromedp.Run(tab1, chromedp.SetUploadFiles("#cover-file", []string{file})); err != nil {
		t.Fatal(err)
	}
	browserEval(t, tab1, `document.querySelector('[data-action=cover-upload]').click(); ''`)
	browserWait(t, tab1, `document.querySelector('.cover') !== null`)
	browserEval(t, tab1, `window.confirm=()=>false; document.querySelector('[data-action=trash]').click(); ''`)
	if e.must(req{method: "GET", path: "/api/albums/" + id.String()}, 200).body["trashed"] != false {
		t.Fatal("cancelled trash succeeded")
	}
	browserEval(t, tab1, `window.confirm=()=>true; document.querySelector('[data-action=trash]').click(); ''`)
	browserWait(t, tab1, `document.querySelector('#editor')?.dataset.trashed === 'true'`)
	if e.must(req{method: "GET", path: "/api/albums/" + id.String()}, 200).body["trashed"] != true {
		t.Fatal("trash failed")
	}
	browserEval(t, tab1, `document.querySelector('[data-action=restore]').click(); ''`)
	browserWait(t, tab1, `document.querySelector('#editor')?.dataset.trashed === 'false'`)
	if e.must(req{method: "GET", path: "/api/albums/" + id.String()}, 200).body["trashed"] != false {
		t.Fatal("restore failed")
	}
}

func TestBrowserPollingStopsWhenIdle(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seed("Polling artist", "Polling album")
	tab, stop := chromedp.NewContext(root)
	defer stop()
	if err := chromedp.Run(tab, chromedp.Navigate(testOrigin+"/albums/"+id.String()), chromedp.WaitVisible("#editor:not([hidden])")); err != nil {
		t.Fatal(err)
	}
	if got := browserEval(t, tab, `document.querySelector('#status').dataset.pending`); got != "true" {
		t.Fatalf("not polling pending job: %q", got)
	}
	browserEval(t, tab, `window.statusFetches=0; const originalFetch=window.fetch; window.fetch=(...args)=>{if(String(args[0]).endsWith('/status'))window.statusFetches++;return originalFetch(...args)}; ''`)
	e.exec(`DELETE FROM jobs WHERE kind='render' AND album_id=$1`, id)
	browserWait(t, tab, `document.querySelector('#status').dataset.pending === 'false'`)
	before := browserEval(t, tab, `String(window.statusFetches)`)
	time.Sleep(4300 * time.Millisecond)
	after := browserEval(t, tab, `String(window.statusFetches)`)
	if before != after {
		t.Fatalf("idle page kept polling: %s -> %s", before, after)
	}
}
