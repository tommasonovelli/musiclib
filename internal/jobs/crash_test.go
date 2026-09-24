package jobs

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/faulttest"
	"musiclib/internal/store"
)

// §12.2 "Due worker sullo stesso job" with real crashes: a child process
// claims (or runs a pool) and is killed with SIGKILL inside the claim
// transaction, right after its commit, or in the executor; the parent is
// the next boot. A crash inside the transaction claims nothing (PostgreSQL
// rolls it back); a crash after it leaves the job running, which
// RecoverRunning (§11.1 step 5) makes pending. Either way the next process
// executes the job exactly once, and no job is lost.

func TestHelperProcess(t *testing.T) {
	mode := faulttest.Mode()
	if mode == "" {
		return
	}
	ctx := context.Background()
	db, err := store.NewPool(ctx, os.Getenv("JOBS_DB"), 2)
	if err != nil {
		faulttest.Exit("error " + err.Error())
	}
	switch mode {
	case "claim":
		c, err := claimNext(ctx, db, testRenderer, faulttest.Crash(os.Getenv("CRASH_AT")))
		switch {
		case err != nil:
			faulttest.Exit("error " + Code(err))
		case c == nil:
			faulttest.Exit("none")
		}
		faulttest.Exit("claimed " + c.Attempt.JobID.String())
	case "pool":
		// The executor is where the build or the import runs: the process
		// dies in the middle of it.
		p, err := NewPool(db, 2, testRenderer, func(context.Context, *Claim) error {
			faulttest.Kill()
			return nil
		}, discard)
		if err != nil {
			faulttest.Exit("error " + err.Error())
		}
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		faulttest.Exit("pool returned " + Code(p.Run(rctx)))
	}
	faulttest.Exit("unknown mode " + mode)
}

func TestCrashAroundTheClaim(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		at   string // a claim failpoint, or "executor"
	}{
		{KindRender, "claim_selected_render"},
		{KindRender, "claim_snapshot_album"},
		{KindRender, "claimed"},
		{KindRender, "executor"},
		{KindScan, "claim_selected_scan"},
		{KindScan, "claimed"},
		{KindScan, "executor"},
	} {
		t.Run(string(tc.kind)+"/"+tc.at, func(t *testing.T) {
			f := newFixture(t)
			var id uuid.UUID
			if tc.kind == KindRender {
				id = f.enqueue(f.album("crash")).JobID
			} else {
				id = f.scanJob(f.batch(), time.Now())
			}
			requested := f.job(id).Requested
			mode := "claim"
			if tc.at == "executor" {
				mode = "pool"
			}
			if got := faulttest.RunChild(t, time.Minute, mode, "JOBS_DB="+f.url, "CRASH_AT="+tc.at); got != faulttest.Killed {
				t.Fatalf("child: %q, want killed at %s", got, tc.at)
			}

			// Inside the transaction nothing was claimed; after the commit
			// the job is running with its claim.
			committed := tc.at == "claimed" || tc.at == "executor"
			j := f.job(id)
			if wantState := map[bool]string{true: "running", false: "pending"}[committed]; j.State != wantState ||
				(j.Claimed != nil) != committed || j.Requested != requested {
				t.Fatalf("after the crash: %s claimed %v requested %d, want %s", j.State, j.Claimed, j.Requested, wantState)
			}

			// The next boot: step 5, then the pool.
			n, err := RecoverRunning(context.Background(), f.db)
			if err != nil || (n == 1) != committed {
				t.Fatalf("RecoverRunning: %d %v", n, err)
			}
			if j := f.job(id); j.State != "pending" || j.Claimed != nil {
				t.Fatalf("after the recovery: %s claimed %v", j.State, j.Claimed)
			}
			var runs atomic.Int32
			fin := finishExec(f)
			p, err := NewPool(f.db, 2, testRenderer, func(ctx context.Context, c *Claim) error {
				if c.Attempt.JobID != id {
					t.Errorf("claimed %s, want %s", c.Attempt.JobID, id)
				}
				runs.Add(1)
				if c.Kind == KindScan {
					return store.InCatalogTx(ctx, f.db, func(tx *store.CatalogTx) error {
						return Finish(ctx, tx, KindScan, c.Attempt, Result{State: StateDone})
					})
				}
				return fin(ctx, c)
			}, discard)
			if err != nil {
				t.Fatal(err)
			}
			p.poll = 50 * time.Millisecond
			stop := startPool(t, p)
			waitUntil(t, "the job to complete", func() bool {
				if tc.kind == KindRender {
					return !f.jobExists(id)
				}
				return f.job(id).State == "done"
			})
			// A few more polls: nothing runs it again.
			time.Sleep(200 * time.Millisecond)
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			if got := runs.Load(); got != 1 {
				t.Fatalf("the job ran %d times, want exactly once", got)
			}
		})
	}
}
