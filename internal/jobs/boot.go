package jobs

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/store"
)

// RecoverRunning is §11.1 step 5 (§6.4: "running -> pending"): no attempt of
// a previous process survives it, so every running job becomes pending and
// loses its claim. The one render row per album means that a recovered
// render and a newer request are the same pending row (§6.4).
//
// It must run after the journal recovery of step 4 and before any worker
// starts: FINALIZE of a recovered publication completes its job by the
// ticket in the journal, which needs the job still running. It returns the
// number of jobs recovered.
func RecoverRunning(ctx context.Context, db *pgxpool.Pool) (int64, error) {
	var n int64
	err := store.InCatalogTx(ctx, db, func(tx *store.CatalogTx) error {
		var err error
		n, err = tx.RecoverRunningJobs(ctx)
		if err != nil {
			return dbErr("recovering running jobs", err)
		}
		return nil
	})
	return n, err
}

// EnqueueStaleRenders is §11.1 step 6: it enqueues, through EnqueueRender,
// the render of every active album whose published renderer is not
// renderVersion (an album never published included) and that has no render
// job at all. An existing job, pending, running or failed, is left alone: a
// failed render stays explicit instead of being retried at every boot. It
// returns the number of renders enqueued.
func EnqueueStaleRenders(ctx context.Context, db *pgxpool.Pool, renderVersion string) (int, error) {
	if renderVersion == "" {
		return 0, errorf(CodeInvalidArgument, "the render version is empty")
	}
	var n int
	err := store.InCatalogTx(ctx, db, func(tx *store.CatalogTx) error {
		n = 0
		ids, err := tx.ListStaleRenderAlbums(ctx, renderVersion)
		if err != nil {
			return dbErr("listing albums with a stale renderer", err)
		}
		for _, id := range ids {
			if _, err := EnqueueRender(ctx, tx, id); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
