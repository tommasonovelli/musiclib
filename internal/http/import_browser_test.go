package http

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// The browser creates the batch; the real importer executes claimed jobs on
// real source files and commits the report on PostgreSQL.
func TestBrowserImportAndOverride(t *testing.T) {
	e, root := browserEnv(t)
	dir := filepath.Join(e.imports, "Source")
	for _, name := range []string{"Good", "Mixed", "Ambiguous/Bonus"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []struct{ path, album string }{
		{"Good/01.flac", "Good"}, {"Mixed/01.flac", "First"}, {"Mixed/02.flac", "Second"},
		{"Ambiguous/01.flac", "Ambiguous"}, {"Ambiguous/Bonus/02.flac", "Ambiguous"},
	} {
		args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=0.15", "-c:a", "flac", "-metadata", "album=" + f.album, "-metadata", "artist=Browser artist", "-y", filepath.Join(dir, filepath.FromSlash(f.path))}
		if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "unassigned <script> &.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	tools, err := media.NewTools(t.Context(), media.NewRunner(2), media.FFmpegPath, media.FFprobePath, media.TagsPath)
	if err != nil {
		t.Fatal(err)
	}
	im, err := importer.New(importer.Config{Catalog: e.svc, Tools: tools, Blobs: e.blobs, Source: e.source, Work: e.work, Budget: e.budget, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	tab, stop := chromedp.NewContext(root)
	defer stop()
	if err := chromedp.Run(tab, chromedp.Navigate(e.srv.URL+"/import")); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('#source-entries')?.textContent.includes('Source/')`)
	browserEval(t, tab, `document.querySelector('#source-entries a').click();''`)
	browserWait(t, tab, `document.querySelector('#source-path')?.textContent.endsWith('/Source')`)
	browserEval(t, tab, `document.querySelector('#import-form button').click();''`)
	browserWait(t, tab, `location.search.startsWith('?batch=')`)
	id := uuid.MustParse(browserEval(t, tab, `new URLSearchParams(location.search).get('batch')`))
	// Resend the same UUID, as after a lost answer: one durable batch.
	e.must(req{method: "POST", path: "/api/imports", body: importBody(id, "Source")}, 200)
	if n := e.count(`SELECT count(*) FROM import_batches WHERE id=$1`, id); n != 1 {
		t.Fatalf("%d batches", n)
	}
	run := func(kind jobs.Kind) {
		t.Helper()
		e.exec(`UPDATE jobs SET state='failed', error_code='test_parked', error_message='parked by test' WHERE kind='render' AND state='pending'`)
		c, err := jobs.ClaimNext(t.Context(), e.db, testRender)
		if err != nil || c == nil || c.Kind != kind {
			t.Fatalf("claim %v: %+v", err, c)
		}
		switch kind {
		case jobs.KindScan:
			err = im.ExecuteScan(context.Background(), c)
		case jobs.KindImport:
			err = im.ExecuteImport(context.Background(), c)
		}
		if err != nil {
			t.Fatalf("execute %s: %v", kind, err)
		}
	}
	run(jobs.KindScan)
	browserWait(t, tab, `document.querySelector('#candidates')?.textContent.includes('Mixed') && document.querySelector('#candidates')?.textContent.includes('ambiguous_candidate')`)
	if got := browserEval(t, tab, `String(document.querySelector('#scan-warnings').textContent.includes('<script>') && document.querySelector('#report script')===null)`); got != "true" {
		t.Fatal("report warning escaped incorrectly")
	}
	run(jobs.KindImport)
	run(jobs.KindImport)
	browserWait(t, tab, `document.querySelector('#batch-state')?.textContent.includes('completed') && document.querySelector('#candidates')?.textContent.includes('mixed_album')`)
	// Title override resolves discordant album tags; the retry's body has only
	// the two specified fields. The real executor revalidates the source.
	browserEval(t, tab, `window.retryBodies=[];const realFetch=window.fetch;window.fetch=(url,opts)=>{if(String(url).endsWith('/retry'))window.retryBodies.push(JSON.parse(opts.body));return realFetch(url,opts)};const f=[...document.querySelectorAll('#candidates > li')].find(li=>li.firstChild.textContent.includes('Mixed')).querySelector('form');f.elements.namedItem('title').value='Fixed title';f.querySelector('button').click();''`)
	if got := browserEval(t, tab, `String(window.retryBodies.length===1 && Object.keys(window.retryBodies[0]).sort().join(',')==='artist,title')`); got != "true" {
		t.Fatalf("unexpected override fields: %s", got)
	}
	browserWait(t, tab, `document.querySelector('#batch-state')?.textContent.includes('importing')`)
	run(jobs.KindImport)
	browserWait(t, tab, `document.querySelector('#batch-state')?.textContent.includes('completed') && document.querySelector('#candidates')?.textContent.includes('Album')`)
	if n := e.count(`SELECT count(*) FROM albums WHERE title='Fixed title'`); n != 1 {
		t.Fatalf("override album count %d", n)
	}
	if err := chromedp.Run(tab, chromedp.Navigate(e.srv.URL+"/")); err != nil {
		t.Fatal(err)
	}
	browserWait(t, tab, `document.querySelector('main')?.textContent.includes('Fixed title')`)
	if got := browserEval(t, tab, `document.querySelector('main').textContent`); !strings.Contains(got, "Good") {
		t.Fatalf("library missing imported album: %s", got)
	}
}
