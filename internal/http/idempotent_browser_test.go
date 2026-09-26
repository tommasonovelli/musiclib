package http

import (
	"github.com/chromedp/chromedp"
	"testing"
)

func TestBrowserImportLostAnswerAndConflict(t *testing.T) {
	e, root := browserEnv(t)
	tab, stop := chromedp.NewContext(root)
	defer stop()
	if err := chromedp.Run(tab, chromedp.Navigate(e.srv.URL+"/import")); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#source-entries') !== null && !document.querySelector('#error').hidden === false`)
	// Deliver the POST to the server and discard its response once.
	browserEval(t, tab, `window.posts=[];const send=window.fetch;window.fetch=async (url,opts)=>{if(url==='/api/imports'){window.posts.push(JSON.parse(opts.body));const result=await send(url,opts);if(window.posts.length===1)throw Error('lost answer');return result}return send(url,opts)};document.querySelector('#import-form button').click();''`)
	browserWait(t, tab, `document.querySelector('#error').textContent.includes('network_error')`)
	browserEval(t, tab, `document.querySelector('#import-path').value='changed';document.querySelector('#import-form button').click();''`)
	browserWait(t, tab, `document.querySelector('#error').textContent.includes('409') || document.querySelector('#error').textContent.includes('conflict')`)
	browserEval(t, tab, `document.querySelector('#import-path').value='';document.querySelector('#import-form button').click();''`)
	browserWait(t, tab, `location.search.startsWith('?batch=')`)
	if got := browserEval(t, tab, `String(window.posts.length===3 && window.posts.every(p=>p.id===window.posts[0].id))`); got != "true" {
		t.Fatalf("request UUID not retained: %s", got)
	}
	if n := e.count(`SELECT count(*) FROM import_batches`); n != 1 {
		t.Fatalf("%d batches", n)
	}
}
