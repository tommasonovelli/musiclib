package jobs

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/store"
)

// The retries of §6.4 and §10.2 ("Riprova" and "Riprova falliti"): a
// failed job gets a new ticket and goes back to pending. A retry changes
// no catalog row (§10.2) and is idempotent while the job is already
// pending or running: it changes nothing then (NOTES.md N-195).

// Codes of the retries.
const (
	// CodeNotRetryable: the job is done or skipped; its outcome is kept
	// for the report (§6.4) and there is nothing to retry (409).
	CodeNotRetryable = "job_not_retryable"
	// CodeInProgress: a retry with overrides other than the ones of the
	// pending or running job; the attempt in progress uses its own, and
	// the new ones can be sent once it has an outcome (409).
	CodeInProgress = "job_in_progress"
	// CodeOverridesNotAllowed: overrides for a job that is not an import
	// (§7.3: they are the import's only overrides; 422).
	CodeOverridesNotAllowed = "job_overrides_not_allowed"
	// CodeNotDismissable: only a failed scan or import can be dismissed; a
	// failed render is the album's own state (409, NOTES.md N-285).
	CodeNotDismissable = "job_not_dismissable"
)

// Dismiss is POST /api/jobs/{id}/dismiss (owner, NOTES.md N-285), in the
// caller's catalog transaction: a failed scan or import stops needing
// attention. It changes nothing else of the job: not its state, outcome,
// ticket or updated_at, so the retention of §6.4 (N-200) counts from the
// outcome as before, and a retry clears the dismissal (RetryFailedJob).
// Dismissing a dismissed job changes nothing (changed false); anything but
// a failed scan or import is CodeNotDismissable.
func Dismiss(ctx context.Context, tx *store.CatalogTx, id uuid.UUID) (changed bool, err error) {
	j, err := tx.GetJobForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errorf(CodeNotFound, "job %s does not exist", id)
	}
	if err != nil {
		return false, dbErr("locking job "+id.String(), err)
	}
	if State(j.State) != StateFailed || Kind(j.Kind) == KindRender {
		return false, errorf(CodeNotDismissable, "job %s is a %s %s: only a failed scan or import can be dismissed", id, j.State, j.Kind)
	}
	if j.DismissedAt != nil {
		return false, nil
	}
	n, err := tx.DismissFailedJob(ctx, id)
	if err != nil {
		return false, dbErr("dismissing job "+id.String(), err)
	}
	if n != 1 {
		// The row is locked and a failed scan or import: none here is a bug.
		return false, errorf(CodeInvalidResult, "job %s could not be dismissed", id)
	}
	return true, nil
}

// Retry is §10.2 POST /api/jobs/{id}/retry, in the caller's catalog
// transaction:
//
//   - a failed job gets a new ticket (requested = nextval) and is pending
//     again; a render through the single enqueue (EnqueueRender, §6.3), a
//     scan or an import with its outcome cleared. An import keeps its
//     stored overrides unless ov gives new ones, which replace them
//     (§7.3). The import's executor revalidates the current candidate
//     (§7.2);
//   - a pending or running job is left as it is: the retry is idempotent
//     (§10.2). ov, if given, must equal the job's overrides (CodeInProgress
//     otherwise);
//   - a done or skipped job is CodeNotRetryable;
//   - ov for a job that is not an import is CodeOverridesNotAllowed.
//
// changed reports whether the job was put back to pending now. ov must be
// normalized already (Overrides.Encode refuses it otherwise).
func Retry(ctx context.Context, tx *store.CatalogTx, id uuid.UUID, ov *Overrides) (changed bool, err error) {
	j, err := tx.GetJobForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errorf(CodeNotFound, "job %s does not exist", id)
	}
	if err != nil {
		return false, dbErr("locking job "+id.String(), err)
	}
	kind, state := Kind(j.Kind), State(j.State)
	if ov != nil && kind != KindImport {
		return false, errorf(CodeOverridesNotAllowed,
			"a %s job takes no overrides: only a failed import accepts an artist and a title (§7.3)", kind)
	}
	switch state {
	case StatePending, StateRunning:
		if ov == nil {
			return false, nil
		}
		stored, err := DecodeOverrides(j.Overrides)
		if err != nil {
			return false, err
		}
		if !stored.Equal(*ov) {
			return false, errorf(CodeInProgress,
				"job %s is already %s with other overrides: retry with the new ones once it has an outcome", id, state)
		}
		return false, nil
	case StateDone, StateSkipped:
		return false, errorf(CodeNotRetryable, "job %s is %s: its outcome is kept for the report, and there is nothing to retry", id, state)
	case StateFailed:
	default:
		return false, errorf(CodeInvalidResult, "job %s has the unknown state %q", id, state)
	}
	if kind == KindRender {
		if j.AlbumID == nil {
			return false, errorf(CodeInvalidResult, "render job %s has no album", id)
		}
		if _, err := EnqueueRender(ctx, tx, *j.AlbumID); err != nil {
			return false, err
		}
		return true, nil
	}
	overrides := j.Overrides
	if ov != nil {
		if overrides, err = ov.Encode(); err != nil {
			return false, err
		}
	}
	if _, err := tx.RetryFailedJob(ctx, store.RetryFailedJobParams{ID: id, Overrides: overrides}); err != nil {
		// The row is locked and failed: no row here is a bug.
		return false, dbErr("retrying job "+id.String(), err)
	}
	return true, nil
}

// RetryFailed is §10.2 POST /api/jobs/retry-failed, in the caller's catalog
// transaction: every failed job gets a new ticket and is pending again,
// the scans and imports with their stored overrides, the renders through
// the single enqueue. Pending and running jobs are not touched, so a
// running job is never duplicated (one row per job, one render row per
// album). It returns how many jobs were retried.
func RetryFailed(ctx context.Context, tx *store.CatalogTx) (int, error) {
	n, err := tx.RetryAllFailedJobs(ctx)
	if err != nil {
		return 0, dbErr("retrying the failed scans and imports", err)
	}
	albums, err := tx.ListFailedRenderAlbums(ctx)
	if err != nil {
		return 0, dbErr("listing the failed renders", err)
	}
	for _, id := range albums {
		if _, err := EnqueueRender(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	return int(n) + len(albums), nil
}

// Equal reports whether o and p are the same overrides.
func (o Overrides) Equal(p Overrides) bool {
	return sameText(o.Artist, p.Artist) && sameText(o.Title, p.Title)
}

func sameText(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
