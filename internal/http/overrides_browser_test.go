package http

import (
	"github.com/chromedp/chromedp"
	"testing"
)

func TestBrowserImportOverridesReplaceAndClear(t *testing.T) {
	e, root := browserEnv(t)
	batch := e.newBatch()
	// A completed scan with a candidate failure, as a durable report.
	e.exec(`UPDATE jobs SET state='done' WHERE kind='scan' AND batch_id=$1`, batch)
	job := e.importJob(batch, "in/mixed", "failed")
	tab, stop := chromedp.NewContext(root)
	defer stop()
	if err := chromedp.Run(tab, chromedp.Navigate(e.srv.URL+"/import?batch="+batch.String())); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#candidates form input[name=title]') !== null`)
	browserEval(t, tab, `const f=document.querySelector('#candidates form');f.elements.namedItem('artist').value='Override artist';f.elements.namedItem('title').value='Override title';f.querySelector('button').click();''`)
	browserWait(t, tab, `document.querySelector('#candidates')?.textContent.includes('Waiting')`)
	e.exec(`UPDATE jobs SET state='failed', error_code='mixed_album', error_message='different titles' WHERE id=$1`, job)
	if err := chromedp.Run(tab, chromedp.Navigate(e.srv.URL+"/import?batch="+batch.String())); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#candidates form input[name=title]')?.value === 'Override title'`)
	browserEval(t, tab, `const f=document.querySelector('#candidates form');f.elements.namedItem('artist').value='';f.elements.namedItem('title').value='';f.querySelector('button').click();''`)
	browserWait(t, tab, `document.querySelector('#candidates')?.textContent.includes('Waiting') && document.querySelector('#candidates form') === null`)
	if got := e.must(req{method: "GET", path: "/api/imports/" + batch.String()}, 200).body["candidates"].([]any)[0].(map[string]any)["overrides"].(map[string]any); got["artist"] != nil || got["title"] != nil {
		t.Fatalf("overrides not cleared: %v", got)
	}
}
