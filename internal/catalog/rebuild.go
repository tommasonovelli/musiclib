package catalog

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// ResetDerivedForRebuild resets only publication state and the render queue.
// The caller has installed the durable maintenance marker and removed the
// derived directories. Repeating it after a crash is safe (DESIGN.md §11.3).
// Catalog metadata, import reports and blob rows are never changed.
func ResetDerivedForRebuild(ctx context.Context, db *pgxpool.Pool) error {
	return resetDerived(ctx, db, false)
}

// ResetDerivedForRestore marks source jobs failed in the same catalog
// transaction as the shared derived-state reset (DESIGN.md §11.4).
func ResetDerivedForRestore(ctx context.Context, db *pgxpool.Pool) error {
	return resetDerived(ctx, db, true)
}

func resetDerived(ctx context.Context, db *pgxpool.Pool, restore bool) error {
	return store.InCatalogTx(ctx, db, func(tx *store.CatalogTx) error {
		if restore {
			if _, err := tx.RestoreFailSourceJobs(ctx); err != nil {
				return dbErr("failing unfinished source jobs", err)
			}
		}
		if err := tx.RebuildClearJournal(ctx); err != nil {
			return dbErr("clearing the publication journal", err)
		}
		if err := tx.RebuildClearPublished(ctx); err != nil {
			return dbErr("clearing publication state", err)
		}
		if err := tx.RebuildClearRenders(ctx); err != nil {
			return dbErr("clearing the render queue", err)
		}
		if err := tx.RebuildClearClaims(ctx); err != nil {
			return dbErr("clearing path reservations", err)
		}
		ids, err := tx.RebuildActiveAlbums(ctx)
		if err != nil {
			return dbErr("reading active albums", err)
		}
		for _, id := range ids {
			if err := ReconcileClaims(ctx, tx, id); err != nil {
				return err
			}
			if _, err := jobs.EnqueueRender(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
}
