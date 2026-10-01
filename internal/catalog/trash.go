package catalog

import (
	"context"

	"github.com/google/uuid"

	"musiclib/internal/store"
)

// EmptyTrash is POST /api/trash/empty, in one catalog transaction: the
// catalog rows of every trashed album whose removal from library/ is
// complete are deleted for good. Such an album has no published output, no
// render job (pending, running or failed) and no publication journal; under
// the catalog lock that cannot change, since every enqueue, PREPARE and
// FINALIZE holds it too. The import jobs that created the album, or were
// skipped because of it, keep their report without the link to it; its
// path claims, attachments and tracks go with it; an artist left without
// any album is deleted (leaveArtist). Blobs are never touched: the originals
// stay. The album's import fingerprint goes with its row, so the same folder
// can be imported again. An album whose removal is still pending, running or
// failed, or still published, is left in the trash and counted as waiting.
// Nothing is enqueued, so the worker pool is not woken.
//
// It returns how many albums were deleted and how many are waiting.
func (s *Service) EmptyTrash(ctx context.Context) (deleted, waiting int, err error) {
	err = store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		deleted, waiting = 0, 0
		rows, err := tx.ListPurgeableAlbums(ctx)
		if err != nil {
			return dbErr("listing the albums the trash can drop", err)
		}
		trashed, err := tx.CountTrashedAlbums(ctx)
		if err != nil {
			return dbErr("counting the albums in the trash", err)
		}
		waiting = int(trashed) - len(rows)
		if len(rows) == 0 {
			return nil
		}
		ids := make([]uuid.UUID, len(rows))
		var artists []uuid.UUID
		seen := make(map[uuid.UUID]bool)
		for i, r := range rows {
			ids[i] = r.ID
			if !seen[r.ArtistID] {
				seen[r.ArtistID] = true
				artists = append(artists, r.ArtistID)
			}
		}
		if _, err := tx.ClearJobResultAlbums(ctx, ids); err != nil {
			return dbErr("unlinking the import reports of the albums in the trash", err)
		}
		// Every reference to an album is ON DELETE RESTRICT: the rows that
		// point at it go first.
		for _, step := range []struct {
			what string
			del  func(context.Context, []uuid.UUID) (int64, error)
		}{
			{"path claims", tx.DeletePurgedClaims},
			{"attachments", tx.DeletePurgedAttachments},
			{"tracks", tx.DeletePurgedTracks},
		} {
			if _, err := step.del(ctx, ids); err != nil {
				return dbErr("deleting the "+step.what+" of the albums in the trash", err)
			}
		}
		n, err := tx.DeletePurgedAlbums(ctx, ids)
		if err != nil {
			return dbErr("deleting the albums in the trash", err)
		}
		if n != int64(len(ids)) {
			return errorf(CodeDB, "%d of %d albums in the trash vanished under the catalog lock", int64(len(ids))-n, len(ids))
		}
		for _, a := range artists {
			if err := leaveArtist(ctx, tx, a); err != nil {
				return err
			}
		}
		deleted = len(ids)
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return deleted, waiting, nil
}
