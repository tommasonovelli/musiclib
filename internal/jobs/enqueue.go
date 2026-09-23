package jobs

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"musiclib/internal/names"
	"musiclib/internal/store"
)

// Enqueued is the album's render row right after EnqueueRender.
type Enqueued struct {
	JobID     uuid.UUID
	Requested int64
	// Claimed is the ticket of the attempt in progress, 0 if none: a
	// running job keeps it, and Requested > Claimed says that more work
	// follows that attempt (§6.3).
	Claimed int64
	State   State
}

// EnqueueRender is the single enqueue of a render (§6.3, §13.2), an upsert
// on the album's one render row (§4.2):
//
//	requested = nextval(job_ticket)
//	state     = running if it was running, pending otherwise
//	error     = NULL
//	queued_at = now
//
// It takes a *store.CatalogTx: it runs in the transaction of the catalog
// mutation that needs it, so that metadata and render request are saved
// together (§3.2 guarantee 6). A failed render becomes pending again.
func EnqueueRender(ctx context.Context, tx *store.CatalogTx, albumID uuid.UUID) (Enqueued, error) {
	row, err := tx.EnqueueRender(ctx, store.EnqueueRenderParams{ID: store.NewID(), AlbumID: &albumID})
	if err != nil {
		return Enqueued{}, dbErr("enqueueing the render of album "+albumID.String(), err)
	}
	e := Enqueued{JobID: row.ID, Requested: row.Requested, State: State(row.State)}
	if row.Claimed != nil {
		e.Claimed = *row.Claimed
	}
	return e, nil
}

// EnqueueScan inserts the one scan job of an import batch (§7.1), pending,
// unless the batch already has one (jobs_scan_batch_key). It returns the id
// of the batch's scan job and whether it was created now.
func EnqueueScan(ctx context.Context, tx *store.CatalogTx, batchID uuid.UUID) (uuid.UUID, bool, error) {
	id := store.NewID()
	if err := tx.InsertScanJob(ctx, store.InsertScanJobParams{ID: id, BatchID: &batchID}); err != nil {
		return uuid.Nil, false, dbErr("enqueueing the scan of batch "+batchID.String(), err)
	}
	row, err := tx.GetScanJob(ctx, &batchID)
	if err != nil {
		return uuid.Nil, false, dbErr("reading the scan job of batch "+batchID.String(), err)
	}
	return row.ID, row.ID == id, nil
}

// ImportJob is one import job the scan found (§7.2): a candidate, pending,
// or, when ErrorCode is set, a branch already failed with its reason.
type ImportJob struct {
	// SourceRel is the candidate's directory relative to /import, exactly
	// as on disk; "" is /import itself (§5.2).
	SourceRel    string
	ErrorCode    string
	ErrorMessage string
}

// EnqueueImport inserts an import job of a batch. A job with the same
// (batch_id, source_rel) is kept as it is (jobs_import_source_key), so a
// scan repeated after a crash inserts nothing twice (§7.2); inserted says
// whether this call created the row.
func EnqueueImport(ctx context.Context, tx *store.CatalogTx, batchID uuid.UUID, j ImportJob) (bool, error) {
	if _, err := names.SplitRelPathOrRoot(j.SourceRel); err != nil {
		return false, &Error{Code: CodeInvalidArgument, Msg: fmt.Sprintf("import source %q", j.SourceRel), Err: err}
	}
	p := store.InsertImportJobParams{
		ID: store.NewID(), BatchID: &batchID, SourceRel: &j.SourceRel, State: string(StatePending),
	}
	if j.ErrorCode != "" || j.ErrorMessage != "" {
		if !validErrorCode(j.ErrorCode) || j.ErrorMessage == "" {
			return false, errorf(CodeInvalidResult, "a failed import needs an error code and a message, got %q", j.ErrorCode)
		}
		msg := clipMessage(j.ErrorMessage)
		p.State, p.ErrorCode, p.ErrorMessage = string(StateFailed), &j.ErrorCode, &msg
	}
	n, err := tx.InsertImportJob(ctx, p)
	if err != nil {
		return false, dbErr(fmt.Sprintf("enqueueing the import of %q", j.SourceRel), err)
	}
	return n == 1, nil
}
