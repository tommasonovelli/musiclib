-- The publication journal (DESIGN.md §9.3, §9.4): zero or one row, written
-- by PREPARE and deleted by FINALIZE, each in a short transaction under the
-- catalog lock (store.InCatalogTx). The paths are relative to library/ and
-- exact (§5.3): never keys.

-- name: GetJournal :one
SELECT * FROM publication WHERE id = 1;

-- name: GetJournalForUpdate :one
SELECT * FROM publication WHERE id = 1 FOR UPDATE;

-- PREPARE (§9.3 A). The primary key (id = 1) refuses a second journal.
-- name: InsertJournal :exec
INSERT INTO publication (id, album_id, ticket, revision, renderer, build_id, receipt_hash, old_path, old_build, new_path)
VALUES (1, @album_id, @ticket, @revision, @renderer, @build_id, @receipt_hash, @old_path, @old_build, @new_path);

-- FINALIZE (§9.3 C).
-- name: DeleteJournal :execrows
DELETE FROM publication WHERE id = 1 AND album_id = @album_id AND build_id = @build_id;

-- FINALIZE (§9.3 C): the published state is the journal's, never the
-- album's current revision (§6.3). A removal has no path, build or receipt.
-- name: SetAlbumPublished :execrows
UPDATE albums SET
    published_path = @path,
    published_revision = @revision,
    published_renderer = @renderer,
    published_build = @build,
    published_receipt_hash = @receipt_hash
WHERE id = @id;

-- The album's single render row (§4.2), locked: FINALIZE completes the
-- attempt the journal names by its ticket (§6.4, N-107).
-- name: GetRenderJobForUpdate :one
SELECT * FROM jobs WHERE kind = 'render' AND album_id = $1 FOR UPDATE;
