package catalog_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
)

// A scan is committed in one transaction: its import jobs and its outcome,
// conditioned on the attempt. An invalid import job or outcome rolls back
// everything (§7.2).
func TestCommitScan(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b, err := e.svc.CreateImportBatch(ctx, uuid.New(), "in")
	if err != nil {
		t.Fatal(err)
	}
	if n := e.wakes.Load(); n != 1 {
		t.Errorf("%d wake-ups after a new batch, want 1", n)
	}
	a := e.claimJob(b.ScanJobID)
	out := catalog.ScanOutcome{Attempt: a, BatchID: b.ID, Imports: []jobs.ImportJob{
		{SourceRel: "in/A"}, {SourceRel: "in/B", ErrorCode: "ambiguous_candidate", ErrorMessage: "why"},
	}, Result: jobs.Result{State: jobs.StateDone, Warnings: []jobs.Warning{{Code: jobs.WarnUnassignedFile, Message: "m", Path: "in/x"}}}}

	bad := out
	bad.Imports = append(append([]jobs.ImportJob(nil), out.Imports...), jobs.ImportJob{SourceRel: "/abs"})
	if _, err := e.svc.CommitScan(ctx, bad); jobs.Code(err) != jobs.CodeInvalidArgument {
		t.Errorf("invalid import: %v", err)
	}
	badResult := out
	badResult.Result = jobs.Result{State: jobs.StateFailed}
	if _, err := e.svc.CommitScan(ctx, badResult); jobs.Code(err) != jobs.CodeInvalidResult {
		t.Errorf("invalid outcome: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'import' AND batch_id = $1`, b.ID); n != 0 {
		t.Fatalf("%d import jobs after refused commits", n)
	}
	n, err := e.svc.CommitScan(ctx, out)
	if err != nil || n != 2 {
		t.Fatalf("CommitScan: %d %v", n, err)
	}
	if j := e.job(b.ScanJobID); j.State != "done" || j.Claimed != nil {
		t.Errorf("scan job %+v", j)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'import' AND batch_id = $1 AND state = 'pending'`, b.ID); n != 1 {
		t.Errorf("%d pending imports", n)
	}
	if _, err := e.svc.CommitScan(ctx, out); jobs.Code(err) != jobs.CodeAttemptStale {
		t.Errorf("a second commit of the same attempt: %v", err)
	}
}

// FailJob completes a running attempt as failed, and nothing else.
func TestFailJob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.runningImport()
	if err := e.svc.FailJob(ctx, jobs.KindImport, a, "corrupt_audio", "bad file", nil); err != nil {
		t.Fatal(err)
	}
	if j := e.job(a.JobID); j.State != "failed" || *j.ErrorCode != "corrupt_audio" || j.Claimed != nil {
		t.Errorf("job %+v", j)
	}
	if err := e.svc.FailJob(ctx, jobs.KindImport, a, "corrupt_audio", "again", nil); jobs.Code(err) != jobs.CodeAttemptStale {
		t.Errorf("a finished attempt: %v", err)
	}
	if _, err := e.svc.GetImportBatch(ctx, uuid.New()); catalog.Code(err) != catalog.CodeImportBatchNotFound {
		t.Errorf("unknown batch: %v", err)
	}
	if _, err := e.svc.CreateImportBatch(ctx, uuid.Nil, ""); catalog.Code(err) != catalog.CodeInvalidArgument {
		t.Errorf("nil id: %v", err)
	}
}
