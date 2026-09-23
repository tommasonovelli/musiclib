package jobs

import (
	"context"

	"github.com/google/uuid"

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
