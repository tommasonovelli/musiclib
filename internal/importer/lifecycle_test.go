package importer

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// setHook installs testHook for one test.
func setHook(t *testing.T, fn func(point string)) {
	t.Helper()
	testHook = fn
	t.Cleanup(func() { testHook = nil })
}

// A source changed during the import is refused (§7.1): a file rewritten,
// touched, added or removed, at the named points after the copies and
// before the final check. Nothing is committed and the job fails.
func TestImportRefusesAChangedSource(t *testing.T) {
	for _, tc := range []struct {
		name, point string
		change      func(e *env)
	}{
		{"content", "after-copy", func(e *env) { e.put("A/1.flac", track{tags: []string{"TITLE=changed"}}.flac(e.t)) }},
		{"mtime only", "after-copy", func(e *env) {
			if err := os.Chtimes(filepath.Join(e.src, "A/notes.txt"), time.Now(), time.Unix(1, 0)); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"added", "before-recheck", func(e *env) { e.put("A/Scans/new.txt", []byte("new")) }},
		{"removed", "before-recheck", func(e *env) {
			if err := os.Remove(filepath.Join(e.src, "A/notes.txt")); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"replaced before its copy", "before-copy", func(e *env) {
			p := filepath.Join(e.src, "A/notes.txt")
			if err := os.Rename(p, p+".old"); err != nil {
				e.t.Fatal(err)
			}
			e.put("A/notes.txt", []byte("same size!"))
		}},
		{"replaced", "after-copy", func(e *env) {
			p := filepath.Join(e.src, "A/notes.txt")
			if err := os.Rename(p, p+".old"); err != nil {
				e.t.Fatal(err)
			}
			e.put("A/notes.txt", []byte("same size!"))
			if err := os.Remove(p + ".old"); err != nil {
				e.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.flac("A/1.flac", track{tags: []string{"TITLE=x"}})
			e.put("A/notes.txt", []byte("same size."))
			e.batch("")
			if c := e.claim(); c == nil || e.execute(c) != nil {
				t.Fatal("scan")
			}
			fired := false
			setHook(t, func(p string) {
				if p == tc.point && !fired {
					fired = true
					tc.change(e)
				}
			})
			e.allowChange = true
			c := e.claim()
			if err := e.execute(c); err != nil {
				t.Fatal(err)
			}
			if !fired {
				t.Fatal("the hook did not run")
			}
			j := e.importJob(catalog.ImportBatch{ID: c.BatchID}, "A")
			if j.State != "failed" || j.Code != CodeSourceChanged {
				t.Fatalf("job %+v, want failed %s", j, CodeSourceChanged)
			}
			if n := e.count(`SELECT count(*) FROM albums`); n != 0 {
				t.Errorf("%d albums", n)
			}
		})
	}
}

// §7.1: the same request id with the same root is the same batch; with
// another root it is refused (409); the root is validated as a path (§5.2).
func TestCreateImportBatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := uuid.New()
	a, err := e.cat.CreateImportBatch(ctx, id, "Incoming/Ünï")
	if err != nil || !a.Created || a.RootRel != "Incoming/Ünï" {
		t.Fatalf("first: %+v %v", a, err)
	}
	again, err := e.cat.CreateImportBatch(ctx, id, "Incoming/Ünï")
	if err != nil || again.Created || again.ID != id || again.ScanJobID != a.ScanJobID {
		t.Fatalf("again: %+v %v", again, err)
	}
	_, err = e.cat.CreateImportBatch(ctx, id, "Other")
	if catalog.Code(err) != catalog.CodeImportBatchConflict {
		t.Fatalf("another root: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'scan' AND batch_id = $1`, id); n != 1 {
		t.Errorf("%d scan jobs", n)
	}
	for _, bad := range []string{"/abs", "a/../b", "a//b", "\xff"} {
		if _, err := e.cat.CreateImportBatch(ctx, uuid.New(), bad); catalog.Code(err) == "" || err == nil {
			t.Errorf("root %q: %v", bad, err)
		}
	}
	root, err := e.cat.CreateImportBatch(ctx, uuid.New(), "")
	if err != nil || root.RootRel != "" {
		t.Errorf("the /import root: %+v %v", root, err)
	}
	// Eight concurrent requests with one id: one batch, one scan job.
	id2 := uuid.New()
	var wg sync.WaitGroup
	created := make(chan bool, 8)
	for range 8 {
		wg.Go(func() {
			b, err := e.cat.CreateImportBatch(ctx, id2, "X")
			if err != nil {
				t.Error(err)
				return
			}
			created <- b.Created
		})
	}
	wg.Wait()
	close(created)
	n := 0
	for c := range created {
		if c {
			n++
		}
	}
	if n != 1 || e.count(`SELECT count(*) FROM jobs WHERE kind = 'scan' AND batch_id = $1`, id2) != 1 {
		t.Errorf("%d created", n)
	}
}

// A scan repeated after a crash (its job put back to pending by the boot
// recovery, §11.1 step 5) inserts no import job twice: (batch_id,
// source_rel) is unique (§7.2), and a job already done stays done.
func TestScanRepeatedAfterCrash(t *testing.T) {
	e := newEnv(t)
	e.flac("A/1.flac", track{})
	e.flac("B/1.flac", track{freq: 500})
	b := e.importDir("")
	a := e.done(b, "A")
	// A crash left the scan to be redone.
	e.exec(`UPDATE jobs SET state = 'pending', claimed = NULL, requested = nextval('job_ticket') WHERE kind = 'scan' AND batch_id = $1`, b.ID)
	c := e.claim()
	if c == nil || c.Kind != jobs.KindScan {
		t.Fatalf("claim %+v", c)
	}
	if err := e.execute(c); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'import' AND batch_id = $1`, b.ID); n != 2 {
		t.Errorf("%d import jobs", n)
	}
	if again := e.done(b, "A"); again.ID != a.ID {
		t.Error("the done job changed")
	}
	if s := e.scanJob(b); s.State != "done" {
		t.Errorf("the repeated scan %+v, want done", s)
	}
	if e.claim() != nil {
		t.Error("a job is pending after the repeated scan")
	}
	// The scan's commit is conditioned on its attempt: a stale one writes
	// nothing, not even the imports.
	stale := catalog.ScanOutcome{Attempt: c.Attempt, BatchID: b.ID,
		Imports: []jobs.ImportJob{{SourceRel: "C"}}, Result: jobs.Result{State: jobs.StateDone}}
	if _, err := e.cat.CommitScan(context.Background(), stale); jobs.Code(err) != jobs.CodeAttemptStale {
		t.Errorf("stale commit: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'import' AND source_rel = 'C'`); n != 0 {
		t.Error("a stale scan inserted an import")
	}
}

// Running an import again after it committed (a lost acknowledgement, §7.6)
// makes no second album; importing the same files again in another batch
// is skipped with a reference to the album (§7.6).
func TestImportIdempotentAndIdentical(t *testing.T) {
	e := newEnv(t)
	e.flac("A/1.flac", track{tags: []string{"TITLE=one"}})
	e.put("A/cover.jpg", jpegImage(t, 8, 8, 1))
	b := e.batch("")
	scan := e.claim()
	if err := e.execute(scan); err != nil {
		t.Fatal(err)
	}
	c := e.claim()
	if err := e.execute(c); err != nil {
		t.Fatal(err)
	}
	a := e.done(b, "A")
	if err := e.execute(c); err != nil { // the same attempt again
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM albums`); n != 1 {
		t.Fatalf("%d albums", n)
	}
	if again := e.done(b, "A"); again.ID != a.ID {
		t.Error("another album")
	}

	b2 := e.importDir("")
	j := e.importJob(b2, "A")
	if j.State != "skipped" || j.AlbumID != a.ID || j.Code != catalog.CodeDuplicateImport {
		t.Errorf("identical re-import %+v", j)
	}
	if n := e.count(`SELECT count(*) FROM albums`); n != 1 {
		t.Errorf("%d albums", n)
	}
}

// The executors in the form jobs.Pool expects, with two workers: a batch of
// three albums, the same candidate twice in two batches concurrently; one
// album per candidate, the duplicate skipped (§6.1, §7.6, §12.2).
func TestPoolRunsScansAndImports(t *testing.T) {
	e := newEnv(t)
	for i, name := range []string{"A", "B", "C"} {
		e.flac(name+"/1.flac", track{freq: 400 + 100*i, tags: []string{"ALBUM=" + name}})
	}
	exec := func(ctx context.Context, c *jobs.Claim) error {
		switch c.Kind {
		case jobs.KindScan:
			return e.im.ExecuteScan(ctx, c)
		case jobs.KindImport:
			return e.im.ExecuteImport(ctx, c)
		}
		return store.InCatalogTx(ctx, e.db, func(tx *store.CatalogTx) error { // no renderer yet
			_, err := jobs.FailRender(ctx, tx, c.Attempt, "test_parked", "no renderer in this test")
			return err
		})
	}
	pool, err := jobs.NewPool(e.db, 2, "rv-test", exec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	b1, b2 := e.batch(""), e.batch("")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Minute)
	for e.count(`SELECT count(*) FROM jobs WHERE state IN ('pending', 'running') AND kind <> 'render'`) > 0 ||
		e.count(`SELECT count(*) FROM jobs WHERE kind = 'import'`) < 6 {
		if time.Now().After(deadline) {
			t.Fatal("the pool did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"A", "B", "C"} {
		j1, j2 := e.importJob(b1, name), e.importJob(b2, name)
		states := j1.State + "+" + j2.State
		if (states != "done+skipped" && states != "skipped+done") || j1.AlbumID != j2.AlbumID {
			t.Errorf("%s: %s %s", name, fmt.Sprint(j1), fmt.Sprint(j2))
		}
	}
	if n := e.count(`SELECT count(*) FROM albums`); n != 3 {
		t.Errorf("%d albums", n)
	}
}

// The limits of §7.2 with the production constants: 10,000 files per
// candidate, checked before the import; the others still import.
func TestScanFileLimit(t *testing.T) {
	e := newEnv(t)
	e.flac("Big/1.flac", track{})
	dir := filepath.Join(e.src, "Big", "Scans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range MaxFiles { // plus the track: MaxFiles + 1
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%05d.txt", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.flac("Small/1.flac", track{freq: 500})
	b := e.importDir("")
	e.failed(b, "Big", catalog.CodeTooManyFiles)
	e.done(b, "Small")
}
