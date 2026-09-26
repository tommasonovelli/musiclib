-- The durable queue (DESIGN.md §6). Tickets are values of the job_ticket
-- sequence; completions are conditioned on id, state running and the claimed
-- ticket, so an old result can never complete a different attempt (§6.3).

-- §6.3: the single enqueue of a render, an upsert on the album's one row.
-- A running job stays running and keeps its claim: the new ticket says that
-- more work follows the attempt in progress. Anything else becomes pending.
-- name: EnqueueRender :one
INSERT INTO jobs (id, kind, album_id, state, queued_at, updated_at)
VALUES (@id, 'render', @album_id, 'pending', now(), now())
ON CONFLICT (album_id) WHERE kind = 'render' DO UPDATE SET
    requested = nextval('job_ticket'),
    state = CASE WHEN jobs.state = 'running' THEN 'running' ELSE 'pending' END,
    error_code = NULL,
    error_message = NULL,
    queued_at = now(),
    updated_at = now()
RETURNING id, requested, claimed, state;

-- §6.2: the oldest pending job of one kind, skipping rows another
-- transaction holds. The caller asks render, then scan, then import (§6.1).
-- name: NextPendingJob :one
SELECT id, kind, album_id, batch_id, source_rel, overrides, requested
FROM jobs
WHERE state = 'pending' AND kind = @kind
ORDER BY queued_at, id
LIMIT 1
FOR UPDATE SKIP LOCKED;

-- name: MarkJobRunning :one
UPDATE jobs SET state = 'running', claimed = requested, updated_at = now()
WHERE id = $1 AND state = 'pending'
RETURNING requested;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = $1;

-- name: GetJobForUpdate :one
SELECT * FROM jobs WHERE id = $1 FOR UPDATE;

-- §6.4: a render whose ticket is still the one built ends by disappearing.
-- name: DeleteFinishedRender :execrows
DELETE FROM jobs
WHERE id = @id AND kind = 'render' AND state = 'running' AND claimed = @ticket::bigint AND requested = @ticket::bigint;

-- §6.3, §6.4: a render attempt goes back to pending (a newer request exists, or
-- the work was superseded before PREPARE). Leaving running clears claimed.
-- name: RequeueRenderAttempt :execrows
UPDATE jobs SET state = 'pending', claimed = NULL, updated_at = now()
WHERE id = @id AND kind = 'render' AND state = 'running' AND claimed = @ticket::bigint;

-- §6.4: a render failed before the journal is failed only while its ticket
-- is current; otherwise the newer request stays pending, without the error.
-- name: FailRenderAttempt :one
UPDATE jobs SET
    state = CASE WHEN requested = claimed THEN 'failed' ELSE 'pending' END,
    error_code = CASE WHEN requested = claimed THEN @error_code::text END,
    error_message = CASE WHEN requested = claimed THEN @error_message::text END,
    claimed = NULL,
    updated_at = now()
WHERE id = @id AND kind = 'render' AND state = 'running' AND claimed = @ticket::bigint
RETURNING state;

-- §6.4: the outcome of a scan or an import, kept for the report.
-- name: FinishAttempt :execrows
UPDATE jobs SET
    state = @state,
    claimed = NULL,
    result_album_id = @result_album_id,
    error_code = @error_code,
    error_message = @error_message,
    warnings = @warnings,
    updated_at = now()
WHERE id = @id AND kind = @kind AND state = 'running' AND claimed = @ticket::bigint;

-- §6.4, §11.1 step 5: after the journal is recovered, no attempt of the
-- previous process is still running.
-- name: RecoverRunningJobs :execrows
UPDATE jobs SET state = 'pending', claimed = NULL, updated_at = now()
WHERE state = 'running';

-- §11.1 step 6: active albums whose published renderer is not the current
-- one (never published included) and that have no render job at all: a
-- failed job stays explicit.
-- name: ListStaleRenderAlbums :many
SELECT al.id FROM albums al
WHERE al.deleted_at IS NULL
  AND al.published_renderer IS DISTINCT FROM @renderer::text
  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind = 'render' AND j.album_id = al.id)
ORDER BY al.id;

-- The render snapshot (§6.2), read in the claim's REPEATABLE READ
-- transaction.
-- name: SnapshotAlbum :one
SELECT al.id, al.title, al.year, al.genre, al.compilation, al.revision,
       (al.deleted_at IS NOT NULL)::boolean AS deleted,
       al.published_path, al.published_revision, al.published_renderer,
       al.published_build, al.published_receipt_hash,
       ar.id AS artist_id, ar.name AS artist_name, ar.revision AS artist_revision,
       al.cover_hash, cb.size AS cover_size, cb.format AS cover_format
FROM albums al
JOIN artists ar ON ar.id = al.artist_id
LEFT JOIN blobs cb ON cb.hash = al.cover_hash
WHERE al.id = $1;

-- name: SnapshotTracks :many
SELECT t.id, t.disc, t.no, t.title, t.artist, t.genre, t.source_path,
       t.blob_hash, b.size AS blob_size, b.format AS blob_format,
       t.lyrics_hash, lb.size AS lyrics_size
FROM tracks t
JOIN blobs b ON b.hash = t.blob_hash
LEFT JOIN blobs lb ON lb.hash = t.lyrics_hash
WHERE t.album_id = $1
ORDER BY t.disc, t.no;

-- name: SnapshotAttachments :many
SELECT a.id, a.rel_path, a.path_key, a.blob_hash, b.size AS blob_size, b.format AS blob_format
FROM attachments a
JOIN blobs b ON b.hash = a.blob_hash
WHERE a.album_id = $1
ORDER BY a.path_key;

-- §7.1: the import batch behind POST /api/imports. The id is the client's
-- idempotency key; an existing batch is kept and compared by the caller.
-- name: InsertImportBatch :exec
INSERT INTO import_batches (id, root_rel, created_at) VALUES (@id, @root_rel, now())
ON CONFLICT (id) DO NOTHING;

-- name: GetImportBatch :one
SELECT id, root_rel, created_at FROM import_batches WHERE id = $1;

-- §7.1: the one scan job of a batch (jobs_scan_batch_key).
-- name: InsertScanJob :exec
INSERT INTO jobs (id, kind, batch_id, state, queued_at, updated_at)
VALUES (@id, 'scan', @batch_id, 'pending', now(), now())
ON CONFLICT (batch_id) WHERE kind = 'scan' DO NOTHING;

-- name: GetScanJob :one
SELECT id, state FROM jobs WHERE kind = 'scan' AND batch_id = $1;

-- §7.2: an import job found by the scan, pending, or already failed for an
-- ambiguous branch. (batch_id, source_rel) is unique, so a scan repeated
-- after a crash inserts nothing twice.
-- name: InsertImportJob :execrows
INSERT INTO jobs (id, kind, batch_id, source_rel, state, error_code, error_message, queued_at, updated_at)
VALUES (@id, 'import', @batch_id, @source_rel, @state, @error_code, @error_message, now(), now())
ON CONFLICT (batch_id, source_rel) WHERE kind = 'import' DO NOTHING;

-- The API's reads of the queue (§10.2 GET /api/jobs, GET /api/imports/{id}).
-- They run in one REPEATABLE READ snapshot (store.InSnapshotTx). Jobs are
-- listed by id: a UUIDv7 never changes, so a page boundary never moves
-- when a job changes state (NOTES.md N-194).
-- name: ListJobs :many
SELECT * FROM jobs
WHERE state = ANY(@states::text[]) AND kind = ANY(@kinds::text[]) AND id > @after::uuid
ORDER BY id
LIMIT @lim::int;

-- name: GetBatchScanJob :one
SELECT * FROM jobs WHERE kind = 'scan' AND batch_id = $1;

-- The candidates of a batch (§7.2), by the bytes of their path.
-- name: ListBatchImportJobs :many
SELECT * FROM jobs WHERE kind = 'import' AND batch_id = $1
ORDER BY source_rel COLLATE "C", id;

-- §10.2 retry of a failed scan or import: a new ticket, back to pending,
-- the outcome of the failed attempt cleared; the overrides are the
-- caller's (the stored ones, or new ones for an import, §7.3).
-- name: RetryFailedJob :one
UPDATE jobs SET
    state = 'pending',
    claimed = NULL,
    requested = nextval('job_ticket'),
    overrides = @overrides,
    result_album_id = NULL,
    error_code = NULL,
    error_message = NULL,
    warnings = '[]',
    queued_at = now(),
    updated_at = now()
WHERE id = @id AND kind IN ('scan', 'import') AND state = 'failed'
RETURNING requested;

-- §10.2 POST /api/jobs/retry-failed, the scans and imports: every failed
-- one gets a new ticket and keeps its overrides; nothing else is touched,
-- a running job least of all.
-- name: RetryAllFailedJobs :execrows
UPDATE jobs SET
    state = 'pending',
    claimed = NULL,
    requested = nextval('job_ticket'),
    result_album_id = NULL,
    error_code = NULL,
    error_message = NULL,
    warnings = '[]',
    queued_at = now(),
    updated_at = now()
WHERE kind IN ('scan', 'import') AND state = 'failed';

-- The albums of the failed renders, for retry-failed: each is enqueued
-- again through the single enqueue (§6.3).
-- name: ListFailedRenderAlbums :many
SELECT al.id FROM jobs j JOIN albums al ON al.id = j.album_id
WHERE j.kind = 'render' AND j.state = 'failed'
ORDER BY al.id;

-- §10.2 POST /api/render-all: every active album, and every trashed album
-- whose output is still published (a deletion still to materialize).
-- name: ListRenderAllAlbums :many
SELECT id FROM albums
WHERE deleted_at IS NULL OR published_path IS NOT NULL
ORDER BY id;

-- §6.4 retention: "Esiti conservati 90 giorni; non si eliminano batch con
-- job non terminali". A batch expires when it and every one of its jobs
-- are older than the retention, and none of its jobs is pending or
-- running. Run under the catalog lock, which every transition of a
-- terminal job back to pending also holds (retry, scan commit).
-- name: ListExpiredBatches :many
SELECT b.id FROM import_batches b
WHERE b.created_at < now() - make_interval(days => @days::int)
  AND NOT EXISTS (
    SELECT 1 FROM jobs j
    WHERE j.batch_id = b.id
      AND (j.state IN ('pending', 'running') OR j.updated_at >= now() - make_interval(days => @days::int)))
ORDER BY b.id;

-- name: DeleteBatchJobs :execrows
DELETE FROM jobs WHERE batch_id = ANY(@ids::uuid[]);

-- name: DeleteImportBatches :execrows
DELETE FROM import_batches WHERE id = ANY(@ids::uuid[]);
