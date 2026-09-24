package publish

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"musiclib/internal/faulttest"
	"musiclib/internal/jobs"
	"musiclib/internal/render"
	"musiclib/internal/store"
)

// The §12.2 crash matrix of the publication with real process crashes: a
// child process runs the publisher and is killed with SIGKILL at a named
// failpoint (no deferred function, no close, the database session dropped
// by the kernel), then the recovery runs in a fresh child process, twice.
// SIGKILL does not lose the page cache: the fsync ordering is argued, not
// tested against a power cut (the limit of N-044 and N-061).

// TestHelperProcess is not a test: it is the body of the child processes
// started by the crash tests, and returns at once in a normal run.
func TestHelperProcess(t *testing.T) {
	mode := faulttest.Mode()
	if mode == "" {
		return
	}
	faulttest.Exit(helperMain(t, mode))
}

func helperMain(t *testing.T, mode string) string {
	ctx := context.Background()
	db, err := store.NewPool(ctx, os.Getenv("PUBLISH_DB"), 2)
	if err != nil {
		return "error " + err.Error()
	}
	e := newEnvOn(t, os.Getenv("PUBLISH_DB"), db, os.Getenv("PUBLISH_DATA"))
	e.fp.Set(faulttest.Crash(os.Getenv("CRASH_AT")))
	e.bfp.Set(faulttest.Crash(os.Getenv("BUILD_CRASH_AT")))
	switch mode {
	case "publish":
		rep, _, err := e.render(os.Getenv("PUBLISH_CONTENT"))
		if err != nil {
			return "error " + Code(err)
		}
		return "published " + string(rep.Outcome)
	case "recover":
		j, err := e.p.Recover(ctx)
		switch {
		case err != nil:
			return "error " + Code(err)
		case j == nil:
			return "none"
		}
		return "recovered " + j.BuildID.String()
	case "execute":
		// The render executor on the next render job: plan, build, publish.
		if err := e.p.ExecuteRender(ctx, e.claim()); err != nil {
			return "error " + Code(err)
		}
		return "executed"
	}
	return "error: unknown mode " + mode
}

// runHelper runs the test binary as a child in the given mode on e's
// volume and database, killed at crashAt if it gets there. It returns the
// result line, or faulttest.Killed.
func (e *env) runHelper(mode, crashAt, content string, env ...string) string {
	e.t.Helper()
	return faulttest.RunChild(e.t, 2*time.Minute, mode, append([]string{"PUBLISH_DB=" + e.dbURL, "PUBLISH_DATA=" + e.data,
		"CRASH_AT=" + crashAt, "PUBLISH_CONTENT=" + content}, env...)...)
}

// resultOf is the build a journal describes, for the checks.
func resultOf(j Journal) render.Result {
	return render.Result{BuildID: j.BuildID, AlbumID: j.AlbumID, AlbumRevision: j.Revision, RenderVersion: j.Renderer,
		Removal: j.Removal(), Dir: j.NewPath, ReceiptHash: j.ReceiptHash}
}

// §12.2: "Crash prima/dopo PREPARE: vecchio output valido oppure journal
// recuperabile"; "Crash subito dopo exchange, prima degli fsync, prima/dopo
// FINALIZE: recovery non esegue uno scambio inverso"; "Crash tra
// installazione del nuovo path e ritiro del vecchio: entrambi completi,
// recovery rimuove solo il vecchio owner"; recovery run twice is
// idempotent.
func TestCrashMatrix(t *testing.T) {
	for _, sc := range scenarios {
		for _, at := range append([]string{"preflight"}, sc.points...) {
			t.Run(sc.name+"/"+at, func(t *testing.T) {
				e := newEnv(t)
				id := sc.setup(e)
				before := e.tree("library")
				if got := e.runHelper("publish", at, "new"); got != "killed" {
					t.Fatalf("child: %s, want killed at %s", got, at)
				}
				j, pending := e.journal()
				switch {
				case at == "preflight":
					// Before PREPARE: the old output is valid, no journal.
					if pending {
						t.Fatal("a journal before PREPARE")
					}
					e.wantTree("library", before)
				case at == "finalized":
					if pending {
						t.Fatal("a journal after FINALIZE")
					}
				case !pending:
					t.Fatalf("no journal after a crash at %s", at)
				}
				for i, want := range []string{"recovered " + j.BuildID.String(), "none"} {
					if !pending {
						want = "none"
					}
					if got := e.runHelper("recover", "", ""); got != want {
						t.Fatalf("recovery %d: %s, want %s", i+1, got, want)
					}
				}
				// The boot's steps 5: cleanup, running -> pending.
				if _, err := e.p.CleanWork(context.Background()); err != nil {
					t.Fatal(err)
				}
				e.wantWorkClean()
				if _, err := jobs.RecoverRunning(context.Background(), e.db); err != nil {
					t.Fatal(err)
				}
				var res render.Result
				switch {
				case pending:
					res = resultOf(j)
				case at == "finalized":
					a := e.album(id)
					res = render.Result{AlbumRevision: a.PublishedRevision, RenderVersion: deref(a.PublishedRenderer),
						Removal: a.PublishedPath == nil, Dir: deref(a.PublishedPath), BuildID: deref(a.PublishedBuild),
						ReceiptHash: deref(a.PublishedReceiptHash)}
				default:
					// Nothing was prepared: the render runs again.
					_, res = e.mustPublish("again")
				}
				e.wantPublished(id, res)
				if got := e.entries("library"); !slices.Equal(got, sc.after) {
					t.Fatalf("library holds %q, want %q: recovery removes only the old owner", got, sc.after)
				}
				if _, ok := e.renderJob(id); ok {
					t.Fatal("the job survived")
				}
			})
		}
	}
}

// A crash during the recovery itself is recovered by the next run.
func TestCrashDuringRecovery(t *testing.T) {
	for _, at := range []string{"installed", "retired"} {
		t.Run(at, func(t *testing.T) {
			e := newEnv(t)
			id := scenarios[2].setup(e) // rename
			if got := e.runHelper("publish", "prepared", "new"); got != "killed" {
				t.Fatalf("publish child: %s", got)
			}
			j := e.mustJournal()
			if got := e.runHelper("recover", at, ""); got != "killed" {
				t.Fatalf("recovery child: %s, want killed at %s", got, at)
			}
			if got := e.runHelper("recover", "", ""); got != "recovered "+j.BuildID.String() {
				t.Fatalf("second recovery: %s", got)
			}
			e.wantPublished(id, resultOf(j))
			if got := e.entries("library"); !slices.Equal(got, []string{"New"}) {
				t.Fatalf("library holds %q", got)
			}
		})
	}
}

// §12.2 "Crash prima/dopo PREPARE" before PREPARE, inside the build, with
// a real build of a real album: a child process runs the render executor
// and is killed with SIGKILL in the middle of the build (the space
// reserved, the first write of a copy, a track's tags written, the first
// file fsync, the first directory fsync). The published album and the
// originals are intact byte for byte (§9.1: "Un fallimento prima del
// journal lascia intatta la directory pubblicata"); the next boot's step 5
// removes the staging and makes the job pending; the render then
// publishes the new revision.
func TestCrashDuringBuild(t *testing.T) {
	for _, at := range []string{"reserved", "write", "tags_written", "fsync_file", "fsync_dir"} {
		t.Run(at, func(t *testing.T) {
			m := newMediaEnv(t)
			writeAlbum(t, m.src, "a", "Artist", "Album", 2)
			if err := os.WriteFile(filepath.Join(m.src, "a", "notes.txt"), []byte("liner notes"), 0o644); err != nil {
				t.Fatal(err)
			}
			m.importAll()
			m.runPool(1)
			id := m.albumID("Album")
			old := m.publishedResult(id)
			m.bump(id)
			library, originals := m.tree("library"), m.tree("originals")

			if got := m.runHelper("execute", "", "", "BUILD_CRASH_AT="+at); got != faulttest.Killed {
				t.Fatalf("child: %q, want killed at %s", got, at)
			}
			m.wantTree("library", library)
			m.wantTree("originals", originals)
			m.wantPublished(id, old)
			if _, pending := m.journal(); pending {
				t.Fatal("a journal after a crash in the build")
			}
			if n := len(m.entries("work/render")); n != map[bool]int{true: 0, false: 1}[at == "reserved"] {
				t.Fatalf("work/render holds %d builds after a crash at %s", n, at)
			}

			// The next boot: no journal to recover, step 5, the pool.
			if got := m.runHelper("recover", "", ""); got != "none" {
				t.Fatalf("recovery: %s", got)
			}
			if _, err := m.p.CleanWork(context.Background()); err != nil {
				t.Fatal(err)
			}
			m.wantWorkClean()
			if n, err := jobs.RecoverRunning(context.Background(), m.db); err != nil || n != 1 {
				t.Fatalf("RecoverRunning: %d %v", n, err)
			}
			m.runPool(1)
			res := m.publishedResult(id)
			if res.AlbumRevision != m.album(id).Revision || res.BuildID == old.BuildID {
				t.Fatalf("published revision %d build %s after the restart", res.AlbumRevision, res.BuildID)
			}
			m.wantPublished(id, res)
			m.wantTree("originals", originals)
			m.wantWorkClean()
		})
	}
}
