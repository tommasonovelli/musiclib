package importer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"musiclib/internal/catalog"
	"musiclib/internal/failpoint"
	"musiclib/internal/faulttest"
	"musiclib/internal/jobs"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// §12.2 "Due import dello stesso candidato / commit dalla risposta persa:
// un album e un esito durevole, non duplicati", with real processes: a
// child process runs the scan or the import and is killed with SIGKILL
// right before or right after the commit, or loses the commit's answer on
// the wire and stops as the server does (§6.4); two children import the
// same candidate at the same time. The parent plays the next boot: step 5
// (the work cleaned, running -> pending), then the pool's work.

const childTimeout = 2 * time.Minute

func TestHelperProcess(t *testing.T) {
	mode := faulttest.Mode()
	if mode == "" {
		return
	}
	faulttest.Exit(childMain(t, mode))
}

// childMain claims and executes one job on the parent's database and
// volume, with the failpoints the environment asks for.
func childMain(t *testing.T, mode string) string {
	url := os.Getenv("IMPORT_DB")
	var proxy *pgtest.Proxy
	if os.Getenv("IMPORT_PROXY") != "" {
		proxy = pgtest.NewProxy(t, url)
		url = proxy.URL
	}
	db, err := store.NewPool(context.Background(), url, 2)
	if err != nil {
		return "error " + err.Error()
	}
	e := newEnvAt(t, db, os.Getenv("IMPORT_DIR"))
	crashAt, cutAt, rendezvous := os.Getenv("CRASH_AT"), os.Getenv("CUT_AT"), os.Getenv("RENDEZVOUS")
	e.fp.Set(func(p failpoint.Point) error {
		switch p.Name {
		case crashAt:
			faulttest.Kill()
		case cutAt:
			if os.Getenv("IMPORT_PROXY") == "lose-ack" {
				proxy.LoseNextCommitAck()
			} else {
				proxy.CutBeforeNextCommit()
			}
		case rendezvous:
			return meet(os.Getenv("RENDEZVOUS_DIR"), 2)
		}
		return nil
	})
	if mode != "run" {
		return "unknown mode " + mode
	}
	c := e.claim()
	if c == nil {
		return "none"
	}
	if err := e.execute(c); err != nil {
		code := store.Code(err)
		if code == "" {
			code = Code(err)
		}
		return fmt.Sprintf("%s %s", c.Kind, code)
	}
	return fmt.Sprintf("%s ok", c.Kind)
}

// meet makes n processes wait for each other: each leaves its mark in dir
// and waits until n marks are there.
func meet(dir string, n int) error {
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(os.Getpid())), nil, 0o644); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(ents) >= n {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("only some of the %d processes reached the rendezvous", n)
}

// crashEnv is a volume and a database shared with the children.
type crashEnv struct {
	*env
	url string
	dir string
}

func newCrashEnv(t *testing.T) *crashEnv {
	t.Helper()
	url := pgtest.EmptyDB(t)
	db := pgtest.Pool(t, url)
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return &crashEnv{env: newEnvAt(t, db, dir), url: url, dir: dir}
}

// child runs one job in a child process with extra environment.
func (c *crashEnv) child(env ...string) *faulttest.Child {
	c.t.Helper()
	return faulttest.Start(c.t, childTimeout, "run", append([]string{"IMPORT_DB=" + c.url, "IMPORT_DIR=" + c.dir}, env...)...)
}

// boot is the next process's step 5 for the importer: work/import
// emptied, the blob temporaries removed, running jobs made pending. It
// returns the number of jobs recovered.
func (c *crashEnv) boot() int64 {
	c.t.Helper()
	ctx := context.Background()
	if err := CleanWork(ctx, c.work); err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.blobs.CleanTemps(ctx); err != nil {
		c.t.Fatal(err)
	}
	n, err := jobs.RecoverRunning(ctx, c.db)
	if err != nil {
		c.t.Fatal(err)
	}
	return n
}

// twoTrackAlbum writes the album the crash tests import.
func (c *crashEnv) twoTrackAlbum(dir string) {
	c.flac(dir+"/1.flac", track{tags: []string{"ARTIST=Crash Artist", "ALBUM=Crash Album " + dir, "TITLE=one"}})
	c.flac(dir+"/2.flac", track{freq: 700, tags: []string{"ARTIST=Crash Artist", "ALBUM=Crash Album " + dir, "TITLE=two"}})
	c.put(dir+"/notes.txt", []byte("liner notes of "+dir))
}

// wantOneAlbum checks the §12.2 property for the import of dir in b: the
// job done with the only album of that title, its tracks, one render job,
// every blob row pinned once, nothing left in work.
func (c *crashEnv) wantOneAlbum(b catalog.ImportBatch, dir string) {
	c.t.Helper()
	a := c.done(b, dir)
	if n := c.count(`SELECT count(*) FROM albums WHERE title = $1`, "Crash Album "+dir); n != 1 {
		c.t.Fatalf("%d albums titled %q, want 1", n, "Crash Album "+dir)
	}
	if len(a.Tracks) != 2 {
		c.t.Fatalf("album %s has %d tracks", a.ID, len(a.Tracks))
	}
	if n := c.count(`SELECT count(*) FROM jobs WHERE kind = 'render' AND album_id = $1`, a.ID); n != 1 {
		c.t.Fatalf("%d render jobs for the album", n)
	}
	var blobs int
	if err := filepath.WalkDir(filepath.Join(c.data, "originals"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			blobs++
		}
		return err
	}); err != nil {
		c.t.Fatal(err)
	}
	if rows := c.count(`SELECT count(*) FROM blobs`); rows != blobs {
		c.t.Fatalf("%d blob rows, %d pinned files", rows, blobs)
	}
	c.checkNoWorkLeft()
	temps, err := os.ReadDir(filepath.Join(c.data, "work", "blobs"))
	if err != nil || len(temps) != 0 {
		c.t.Fatalf("work/blobs holds %d entries (%v)", len(temps), err)
	}
}

// scanned runs the batch's scan in this process and returns the batch.
func (c *crashEnv) scanned() catalog.ImportBatch {
	c.t.Helper()
	b := c.batch("")
	if s := c.claim(); s == nil || s.Kind != jobs.KindScan || c.execute(s) != nil {
		c.t.Fatal("the scan did not run")
	}
	return b
}

func TestCrashAtTheImportCommit(t *testing.T) {
	for _, at := range []string{"import_committing", "import_committed"} {
		t.Run(at, func(t *testing.T) {
			c := newCrashEnv(t)
			c.twoTrackAlbum("A")
			b := c.scanned()
			before := sourceState(t, c.src)
			if got := c.child("CRASH_AT=" + at).Wait(t); got != faulttest.Killed {
				t.Fatalf("child: %q, want killed at %s", got, at)
			}
			committed := at == "import_committed"
			j := c.importJob(b, "A")
			if n := c.count(`SELECT count(*) FROM albums`); (n == 1) != committed || n > 1 {
				t.Fatalf("%d albums after a crash at %s", n, at)
			}
			if wantState := map[bool]string{true: "done", false: "running"}[committed]; j.State != wantState {
				t.Fatalf("the job is %s after a crash at %s, want %s", j.State, at, wantState)
			}
			// The commit and the job's outcome are one transaction: after
			// it nothing is running any more.
			if n := c.boot(); (n == 1) == committed {
				t.Fatalf("the boot recovered %d jobs", n)
			}
			c.runAll()
			c.wantOneAlbum(b, "A")
			if d := diffStates(before, sourceState(t, c.src)); len(d) > 0 {
				t.Fatalf("the source changed: %v", d)
			}
		})
	}
}

// A lost COMMIT answer (§6.4: store_commit_uncertain, fatal: the process
// stops) and a connection cut before the COMMIT: either way the restarted
// process ends with one album and one durable outcome, and nothing is
// run blindly again (§7.6: "Una conferma del commit persa non causa un
// secondo album").
func TestImportCommitAnswerLost(t *testing.T) {
	for _, tc := range []struct {
		proxy     string
		committed bool
	}{{"lose-ack", true}, {"cut-before", false}} {
		t.Run(tc.proxy, func(t *testing.T) {
			c := newCrashEnv(t)
			c.twoTrackAlbum("A")
			b := c.scanned()
			got := c.child("IMPORT_PROXY="+tc.proxy, "CUT_AT=import_committing").Wait(t)
			if want := "import " + store.CodeCommitUncertain; got != want {
				t.Fatalf("child: %q, want %q", got, want)
			}
			if n := c.count(`SELECT count(*) FROM albums`); (n == 1) != tc.committed {
				t.Fatalf("%d albums after the lost answer", n)
			}
			// The next process: step 5, then its pool.
			if n := c.boot(); (n == 1) == tc.committed {
				t.Fatalf("the boot recovered %d jobs", n)
			}
			want := "import ok"
			if tc.committed {
				want = "none"
			}
			if got := c.child().Wait(t); got != want {
				t.Fatalf("the restarted process: %q, want %q", got, want)
			}
			c.wantOneAlbum(b, "A")
		})
	}
}

// Two processes import the same candidate (two batches of the same
// directory) and meet right before their commits, so that both commit at
// once: one album, one job done and the other skipped with a reference to
// it (§7.6).
func TestTwoProcessesImportTheSameCandidate(t *testing.T) {
	c := newCrashEnv(t)
	c.twoTrackAlbum("A")
	b1 := c.scanned()
	b2 := c.scanned()
	meetDir := t.TempDir()
	env := []string{"RENDEZVOUS=import_committing", "RENDEZVOUS_DIR=" + meetDir}
	p1, p2 := c.child(env...), c.child(env...)
	for _, p := range []*faulttest.Child{p1, p2} {
		if got := p.Wait(t); got != "import ok" {
			t.Fatalf("child: %q\n%s", got, p.Output())
		}
	}
	j1, j2 := c.importJob(b1, "A"), c.importJob(b2, "A")
	if j1.State == j2.State || j1.AlbumID != j2.AlbumID ||
		!(j1.State == "done" && j2.State == "skipped" || j1.State == "skipped" && j2.State == "done") {
		t.Fatalf("outcomes %s (%s) and %s (%s), want one done and one skipped on the same album",
			j1.State, j1.AlbumID, j2.State, j2.AlbumID)
	}
	if n := c.count(`SELECT count(*) FROM albums`); n != 1 {
		t.Fatalf("%d albums", n)
	}
	done := b1
	if j2.State == "done" {
		done = b2
	}
	c.wantOneAlbum(done, "A")
}

// The scan's commit (the import jobs and the scan's outcome in one
// transaction, §7.2) interrupted before and after: repeating it is safe,
// with one import job per candidate and one album each.
func TestCrashAtTheScanCommit(t *testing.T) {
	for _, at := range []string{"scan_committing", "scan_committed"} {
		t.Run(at, func(t *testing.T) {
			c := newCrashEnv(t)
			c.twoTrackAlbum("A")
			c.twoTrackAlbum("B")
			b := c.batch("")
			if got := c.child("CRASH_AT=" + at).Wait(t); got != faulttest.Killed {
				t.Fatalf("child: %q, want killed at %s", got, at)
			}
			committed := at == "scan_committed"
			if n := c.count(`SELECT count(*) FROM jobs WHERE kind = 'import'`); (n == 2) != committed || (n != 0 && n != 2) {
				t.Fatalf("%d import jobs after a crash at %s", n, at)
			}
			if n := c.boot(); (n == 1) == committed {
				t.Fatalf("the boot recovered %d jobs", n)
			}
			c.runAll()
			if s := c.scanJob(b); s.State != "done" {
				t.Fatalf("scan %s", s.State)
			}
			if n := c.count(`SELECT count(*) FROM jobs WHERE kind = 'import' AND batch_id = $1`, b.ID); n != 2 {
				t.Fatalf("%d import jobs, want one per candidate", n)
			}
			c.wantOneAlbum(b, "A")
			c.wantOneAlbum(b, "B")
		})
	}
}
