package publish

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/fsops"
	"musiclib/internal/store"
)

// Recover is §9.4, the boot's step 4 (§11.1): before any worker, it
// completes forward the publication that a crash, a lost database or a
// failure after PREPARE left in the journal. It returns the journal it
// completed, or nil when there was none.
//
//   - A removal (new_path NULL) looks for no staging: it retires the old
//     path.
//   - A new path already holding the receipt with the journal's build and
//     hash is installed: the exchange is never repeated, so a crash right
//     after it never becomes a reverse exchange.
//   - Otherwise the build must be in its staging, and the planned
//     installation is performed with the ownership and destination checks.
//   - The old path, if it differs from the new one: absent is already
//     retired; present is retired once; an existing retired directory is
//     never overwritten.
//   - The fsyncs are repeated, then FINALIZE, then the cleanup of work/.
//
// It is idempotent: it may run any number of times, and a crash inside it
// is recovered by the next run. A state that matches no legal transition
// is CodeIllegalState: nothing is deleted, the boot stops with that code,
// and the operator corrects the disk or runs rebuild (§9.4, §11.3).
// A fatal store error is returned as it is.
func (p *Publisher) Recover(ctx context.Context) (*Journal, error) {
	if err := p.lock(ctx); err != nil {
		return nil, err
	}
	j, err := p.recoverLocked(ctx)
	p.unlock()
	if err != nil || j == nil {
		return nil, err
	}
	p.cleanup(ctx, *j)
	return j, nil
}

func (p *Publisher) recoverLocked(ctx context.Context) (*Journal, error) {
	row, found, err := p.readJournal(ctx)
	if err != nil || !found {
		return nil, err
	}
	j := fromRow(row)
	if err := j.validate(); err != nil {
		return nil, wrap(CodeIllegalState, err, "the publication journal is not valid")
	}
	p.log.Info("recovering a pending publication", "album_id", j.AlbumID, "build_id", j.BuildID,
		"revision", j.Revision, "old_path", j.OldPath, "new_path", j.NewPath)
	if err := p.install(ctx, j); err != nil {
		return nil, err
	}
	jo, err := p.finalize(ctx, j)
	if err != nil {
		return nil, err
	}
	p.log.Info("pending publication completed", "album_id", j.AlbumID, "build_id", j.BuildID, "job", jo)
	return &j, nil
}

// CleanWork is the part of §11.1 step 5 that belongs to the publisher: it
// removes every build in work/render and every directory in work/retired
// that no journal references, and returns their paths relative to work/.
// It runs after Recover and before any worker, so no build is in progress;
// after a successful Recover no journal is left, and everything goes.
func (p *Publisher) CleanWork(ctx context.Context) ([]string, error) {
	keep := uuid.Nil
	row, found, err := p.readJournal(ctx)
	if err != nil {
		return nil, err
	}
	if found {
		keep = row.BuildID
	}
	var removed []string
	for _, dir := range []string{renderDir(uuid.Nil), retiredDir} {
		ents, err := p.work.ReadDir(dir)
		if fsops.Code(err) == fsops.CodeNotFound {
			continue
		}
		if err != nil {
			return removed, err
		}
		for _, e := range ents {
			if keep != uuid.Nil && e.Name == keep.String() {
				continue
			}
			rel := dir + "/" + e.Name
			if err := p.work.RemoveAll(ctx, rel); err != nil {
				return removed, err
			}
			removed = append(removed, rel)
		}
		if len(removed) > 0 {
			if err := p.work.SyncDir(dir); err != nil {
				return removed, err
			}
		}
	}
	return removed, nil
}

// readJournal reads the journal row, if any, through the transaction
// runner, so that a lost database is classified as §6.4 wants.
func (p *Publisher) readJournal(ctx context.Context) (store.Publication, bool, error) {
	var (
		row   store.Publication
		found bool
	)
	err := store.InCatalogTx(ctx, p.db, func(tx *store.CatalogTx) error {
		var err error
		row, err = tx.GetJournal(ctx)
		found = err == nil
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return dbErr("reading the publication journal", err)
		}
		return nil
	})
	return row, found, err
}
