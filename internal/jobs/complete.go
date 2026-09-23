package jobs

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/store"
)

// The completions of §6.4. Every one takes a *store.CatalogTx, so it runs
// under the catalog lock: in FINALIZE's or PREPARE's transaction for a
// render, in the import commit's for an import. Every one is conditioned on
// the job id, state running and the claimed ticket (§6.3), and every
// transition out of running clears claimed.

// RenderOutcome is what a render completion did to the job row.
type RenderOutcome string

const (
	// RenderDeleted: the ticket built was still the requested one; the job
	// is gone (§6.4).
	RenderDeleted RenderOutcome = "deleted"
	// RenderRequeued: a newer request exists (or the work was superseded);
	// the job is pending for it, without claim and without error.
	RenderRequeued RenderOutcome = "requeued"
	// RenderFailed: the failure concerns the current request; the job is
	// failed with its error until a retry or a change of the album (§6.4).
	RenderFailed RenderOutcome = "failed"
)

// FinishRender completes a render whose publication succeeded, in FINALIZE's
// transaction (§9.3 C): the job is deleted if its ticket is unchanged,
// otherwise it goes back to pending for the newer request (§6.4). An attempt
// that is no longer the running one is CodeAttemptStale and changes nothing.
func FinishRender(ctx context.Context, tx *store.CatalogTx, a Attempt) (RenderOutcome, error) {
	n, err := tx.DeleteFinishedRender(ctx, store.DeleteFinishedRenderParams{ID: a.JobID, Ticket: a.Ticket})
	if err != nil {
		return "", dbErr("deleting the finished render "+a.String(), err)
	}
	if n == 1 {
		return RenderDeleted, nil
	}
	if err := RequeueRender(ctx, tx, a); err != nil {
		return "", err
	}
	return RenderRequeued, nil
}

// RequeueRender puts a render attempt back to pending: its work was
// superseded before PREPARE (§6.3: the staging is discarded and the job
// becomes pending), or a newer request exists. CodeAttemptStale if the
// attempt is no longer the running one.
func RequeueRender(ctx context.Context, tx *store.CatalogTx, a Attempt) error {
	n, err := tx.RequeueRenderAttempt(ctx, store.RequeueRenderAttemptParams{ID: a.JobID, Ticket: a.Ticket})
	if err != nil {
		return dbErr("requeueing "+a.String(), err)
	}
	if n != 1 {
		return errorf(CodeAttemptStale, "%s is not a running render with that ticket", a)
	}
	return nil
}

// FailRender records the failure of a render before the journal (§6.4):
// failed, with code and message, only if the ticket is still the requested
// one; otherwise pending for the newer request, without the error.
// CodeAttemptStale if the attempt is no longer the running one.
func FailRender(ctx context.Context, tx *store.CatalogTx, a Attempt, code, message string) (RenderOutcome, error) {
	if !validErrorCode(code) || message == "" {
		return "", errorf(CodeInvalidResult, "a failure needs an error code and a message, got %q", code)
	}
	state, err := tx.FailRenderAttempt(ctx, store.FailRenderAttemptParams{
		ID: a.JobID, Ticket: a.Ticket, ErrorCode: code, ErrorMessage: clipMessage(message),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errorf(CodeAttemptStale, "%s is not a running render with that ticket", a)
	}
	if err != nil {
		return "", dbErr("recording the failure of "+a.String(), err)
	}
	if State(state) == StateFailed {
		return RenderFailed, nil
	}
	return RenderRequeued, nil
}

// Result is the outcome of a scan or an import attempt, kept for the report
// (§6.4, §7.2, §7.6).
type Result struct {
	// State is done, skipped or failed.
	State State
	// AlbumID is jobs.result_album_id: required for an import that is done
	// (the album created) or skipped (the identical album, §7.6); uuid.Nil
	// otherwise.
	AlbumID uuid.UUID
	// ErrorCode and ErrorMessage explain a failed or skipped attempt; a
	// done attempt has none.
	ErrorCode    string
	ErrorMessage string
	Warnings     []Warning
}

func (r Result) validate(kind Kind) error {
	if kind != KindScan && kind != KindImport {
		return errorf(CodeInvalidResult, "a %s job has no result of this form", kind)
	}
	switch r.State {
	case StateDone:
		if r.ErrorCode != "" || r.ErrorMessage != "" {
			return errorf(CodeInvalidResult, "a done job has no error")
		}
	case StateSkipped, StateFailed:
		if !validErrorCode(r.ErrorCode) || r.ErrorMessage == "" {
			return errorf(CodeInvalidResult, "a %s job needs an error code and a message, got %q", r.State, r.ErrorCode)
		}
	default:
		return errorf(CodeInvalidResult, "%q is not an outcome", r.State)
	}
	wantAlbum := kind == KindImport && r.State != StateFailed
	if wantAlbum && r.AlbumID == uuid.Nil {
		return errorf(CodeInvalidResult, "a %s import needs its result album", r.State)
	}
	if !wantAlbum && r.AlbumID != uuid.Nil {
		return errorf(CodeInvalidResult, "a %s %s job has no result album", r.State, kind)
	}
	return nil
}

// Finish records the outcome of a scan or import attempt (§6.4): done,
// skipped or failed, with its error and warnings; claimed is cleared.
// CodeAttemptStale if the attempt is no longer the running one.
func Finish(ctx context.Context, tx *store.CatalogTx, kind Kind, a Attempt, r Result) error {
	if err := r.validate(kind); err != nil {
		return err
	}
	warnings, err := EncodeWarnings(r.Warnings)
	if err != nil {
		return err
	}
	p := store.FinishAttemptParams{
		ID: a.JobID, Kind: string(kind), Ticket: a.Ticket, State: string(r.State), Warnings: warnings,
	}
	if r.AlbumID != uuid.Nil {
		p.ResultAlbumID = &r.AlbumID
	}
	if r.ErrorCode != "" {
		msg := clipMessage(r.ErrorMessage)
		p.ErrorCode, p.ErrorMessage = &r.ErrorCode, &msg
	}
	n, err := tx.FinishAttempt(ctx, p)
	if err != nil {
		return dbErr("finishing "+a.String(), err)
	}
	if n != 1 {
		return errorf(CodeAttemptStale, "%s is not a running %s job with that ticket", a, kind)
	}
	return nil
}

// Status is a job row as the completions see it.
type Status struct {
	ID        uuid.UUID
	Kind      Kind
	State     State
	Requested int64
	// Claimed is 0 when the job is not running.
	Claimed int64
	// AlbumID is album_id for a render, result_album_id otherwise.
	AlbumID      uuid.UUID
	ErrorCode    string
	ErrorMessage string
}

// Runs reports whether a is the attempt in progress: running, with a's
// ticket.
func (s Status) Runs(a Attempt) bool {
	return s.ID == a.JobID && s.State == StateRunning && s.Claimed == a.Ticket
}

// LockStatus locks a job row until the end of the transaction and returns
// it. The import commit uses it to recheck, first, that its job is not
// already completed (§7.6 step 1). CodeNotFound for an unknown id.
func LockStatus(ctx context.Context, tx *store.CatalogTx, jobID uuid.UUID) (Status, error) {
	j, err := tx.GetJobForUpdate(ctx, jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Status{}, errorf(CodeNotFound, "job %s does not exist", jobID)
	}
	if err != nil {
		return Status{}, dbErr("locking job "+jobID.String(), err)
	}
	s := Status{
		ID: j.ID, Kind: Kind(j.Kind), State: State(j.State), Requested: j.Requested,
		Claimed: deref(j.Claimed), ErrorCode: deref(j.ErrorCode), ErrorMessage: deref(j.ErrorMessage),
	}
	if s.Kind == KindRender {
		s.AlbumID = deref(j.AlbumID)
	} else {
		s.AlbumID = deref(j.ResultAlbumID)
	}
	return s, nil
}
