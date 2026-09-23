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
