package http

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", e.api)
	mux.HandleFunc("/", e.api.Pages)
	srv := httptest.NewUnstartedServer(e.signedIn(mux))
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)
	e.srv = srv
	// Browser requests use the listener's real port; the boundary must agree.
	e.api.origin = srv.URL
	e.api.host = listener.Addr().String()
	e.host = e.api.host
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath("/usr/bin/chromium"), chromedp.Env(append(os.Environ(), "HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir(), "XDG_CACHE_HOME="+t.TempDir())...), chromedp.NoSandbox, chromedp.Flag("disable-dev-shm-usage", true), chromedp.Flag("no-proxy-server", true))
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
		time.Sleep(100 * time.Millisecond)
	}
	var debug string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify({path:location.pathname, error:document.querySelector('#error')?.textContent, notices:[...document.querySelectorAll('.notice:not([hidden])')].map(n=>n.textContent), changes:document.querySelector('#changes')?.textContent, etag:document.querySelector('#editor')?.dataset.etag})`, &debug))
	t.Fatalf("browser condition timed out: %s; state: %s", expression, debug)
}

func TestBrowserPollingStopsWhenIdle(t *testing.T) {
	e, root := browserEnv(t)
	id := e.seed("Polling artist", "Polling album")
	tab, stop := chromedp.NewContext(root)
	defer stop()
	if err := chromedp.Run(tab, chromedp.Navigate(e.srv.URL+"/albums/"+id.String()), chromedp.WaitReady("#status")); err != nil {
		t.Fatal(err)
	}
	if got := browserEval(t, tab, `document.querySelector('#status').dataset.pending + '|' + document.querySelector('#status').textContent`); got != "true|Waiting" {
		t.Fatalf("not polling pending job: %q", got)
	}
	// The status is the page's own (N-265): count the page's requests.
	browserEval(t, tab, `window.statusFetches=0; const originalFetch=window.fetch; window.fetch=(...args)=>{if(String(args[0])===location.pathname)window.statusFetches++;return originalFetch(...args)}; ''`)
	e.align(id)
	browserWait(t, tab, `document.querySelector('#status').dataset.pending === 'false' && document.querySelector('#status').textContent === ''`)
	before := browserEval(t, tab, `String(window.statusFetches)`)
	if before == "0" {
		t.Fatal("the status was not polled")
	}
	time.Sleep(4300 * time.Millisecond)
	after := browserEval(t, tab, `String(window.statusFetches)`)
	if before != after {
		t.Fatalf("idle page kept polling: %s -> %s", before, after)
	}
}
