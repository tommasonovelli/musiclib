package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

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

// helperEnv selects the child-process mode of TestHelperProcess.
const helperEnv = "MUSICLIB_PUBLISH_HELPER"

// TestHelperProcess is not a test: it is the body of the child processes
// started by the crash tests, and returns at once in a normal run.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	fmt.Println("RESULT:", helperMain(t, mode))
	os.Exit(0)
}

func helperMain(t *testing.T, mode string) string {
	ctx := context.Background()
	db, err := store.NewPool(ctx, os.Getenv("PUBLISH_DB"), 2)
	if err != nil {
		return "error " + err.Error()
	}
	e := newEnvOn(t, os.Getenv("PUBLISH_DB"), db, os.Getenv("PUBLISH_DATA"))
	at := os.Getenv("CRASH_AT")
	failpointHook = func(point string) error {
		if point == at {
			if err := unix.Kill(os.Getpid(), unix.SIGKILL); err != nil {
				return err
			}
			select {} // never returns
		}
		return nil
	}
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
	}
	return "error: unknown mode " + mode
}

// runHelper runs the test binary as a child in the given mode on e's
// volume and database. It returns the result line, or "killed".
func (e *env) runHelper(mode, crashAt, content string) string {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(e.t.Context(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, "PUBLISH_DB="+e.dbURL, "PUBLISH_DATA="+e.data,
		"CRASH_AT="+crashAt, "PUBLISH_CONTENT="+content)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		e.t.Fatalf("child %s timed out\n%s", mode, out)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
			return "killed"
		}
	}
	if err != nil {
		e.t.Fatalf("child %s: %v\n%s", mode, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if res, ok := strings.CutPrefix(line, "RESULT: "); ok {
			return res
		}
	}
	e.t.Fatalf("child %s: no result\n%s", mode, out)
	return ""
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
