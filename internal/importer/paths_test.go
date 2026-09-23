package importer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/jobs"
)

// §7.2: an import revalidates its candidate against the current disk, never
// trusting the scan: audio added below it after the scan makes it
// ambiguous; a candidate that lost its audio is not a candidate; one that
// disappeared is not found.
func TestImportRevalidates(t *testing.T) {
	e := newEnv(t)
	e.flac("A/1.flac", track{})
	e.flac("B/1.flac", track{freq: 500})
	e.flac("C/1.flac", track{freq: 600})
	b := e.batch("")
	if err := e.execute(e.claim()); err != nil {
		t.Fatal(err)
	}
	e.flac("A/Bonus/1.flac", track{freq: 700})
	if err := os.Remove(filepath.Join(e.src, "B/1.flac")); err != nil {
		t.Fatal(err)
	}
	e.put("B/1.txt", []byte("the audio is gone"))
	if err := os.RemoveAll(filepath.Join(e.src, "C")); err != nil {
		t.Fatal(err)
	}
	e.runAll()
	e.failed(b, "A", CodeAmbiguousCandidate)
	e.failed(b, "B", CodeNotACandidate)
	e.failed(b, "C", CodeSourceNotFound)
}

// /import itself can be the candidate (source_rel ""): the album title
// comes from the tags, or an explicit title; there is no directory name
// to fall back on.
func TestImportTheImportRoot(t *testing.T) {
	e := newEnv(t)
	e.flac("1.flac", track{})
	b := e.importDir("")
	j := e.failed(b, "", CodeAlbumTitleMissing)
	e.retry(j.ID, jobs.Overrides{Title: ptr("Loose")})
	e.runAll()
	if a := e.done(b, ""); a.Title != "Loose" || a.Tracks[0].Source != "1.flac" {
		t.Errorf("album %+v", a)
	}
}

// §11.2: the estimate plus the 1 GiB margin must fit in the free space.
func TestCheckSpace(t *testing.T) {
	e := newEnv(t)
	fs, err := e.work.StatFS()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.im.checkSpace(1); err != nil && fs.FreeBytes > spaceMargin+1<<20 {
		t.Errorf("a small estimate: %v", err)
	}
	if err := e.im.checkSpace(fs.FreeBytes - spaceMargin + 1<<30); Code(err) != CodeInsufficientSpace {
		t.Errorf("an estimate beyond the free space minus the margin: %v", err)
	}
	if got := estimate([]*srcFile{{ID: identity{Size: 10}}, {ID: identity{Size: 5}}}); got != 15+2*MaxCoverBytes {
		t.Errorf("estimate %d", got)
	}
}

// A shutdown (the context cancelled) completes nothing: the job stays
// running for the boot's recovery (§6.4, §11.1 step 5).
func TestImportCancelledLeavesTheJobRunning(t *testing.T) {
	e := newEnv(t)
	e.flac("A/1.flac", track{})
	e.batch("")
	if err := e.execute(e.claim()); err != nil {
		t.Fatal(err)
	}
	c := e.claim()
	ctx, cancel := context.WithCancel(context.Background())
	setHook(t, func(p string) {
		if p == "after-copy" {
			cancel()
		}
	})
	if err := e.im.ExecuteImport(ctx, c); err == nil {
		t.Fatal("no error after the cancellation")
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE id = $1 AND state = 'running'`, c.Attempt.JobID); n != 1 {
		t.Error("the cancelled job is not running")
	}
}

// A completion for an attempt that is no longer the running one changes
// nothing and is reported.
func TestFailStaleAttempt(t *testing.T) {
	e := newEnv(t)
	e.flac("A/1.flac", track{tags: []string{"ALBUM=X"}})
	e.flac("A/2.flac", track{freq: 500, tags: []string{"ALBUM=Y"}})
	e.batch("")
	if err := e.execute(e.claim()); err != nil {
		t.Fatal(err)
	}
	c := e.claim()
	e.exec(`UPDATE jobs SET claimed = requested + 1000 WHERE id = $1`, c.Attempt.JobID) // another attempt
	err := e.im.ExecuteImport(context.Background(), c)
	if jobs.Code(err) != jobs.CodeAttemptStale && catalog.Code(err) != jobs.CodeAttemptStale {
		t.Fatalf("err %v, want %s", err, jobs.CodeAttemptStale)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE id = $1 AND state = 'running'`, c.Attempt.JobID); n != 1 {
		t.Error("a stale attempt completed the job")
	}
}

// CleanWork removes what an interrupted import left in work/import.
func TestCleanWork(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.data, "work", workDir)
	if err := os.MkdirAll(filepath.Join(dir, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "a.img"), []byte("left"))
	if err := CleanWork(context.Background(), e.work); err != nil {
		t.Fatal(err)
	}
	if ents, err := os.ReadDir(dir); err != nil || len(ents) != 0 {
		t.Errorf("work/import: %v %v", ents, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := CleanWork(context.Background(), e.work); err != nil {
		t.Errorf("without work/import: %v", err)
	}
	if fi, err := e.work.Stat(workDir); err != nil || fi.Type != fsops.TypeDir {
		t.Errorf("work/import not recreated: %v", err)
	}
}
