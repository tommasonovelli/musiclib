-- Catalog queries (DESIGN.md §4, §5.3, §7.6). Every statement here that
-- writes runs inside store.InCatalogTx, under the catalog lock below.

-- The catalog lock of §5.3: one transaction-scoped advisory lock with a
-- constant key, taken first by every catalog mutation. The key is the ASCII
-- "mlcatalg", distinct from the migration lock "mlmigrat" (N-053).
-- name: LockCatalog :exec
SELECT pg_advisory_xact_lock(7884786317834415207);

-- Offline integrity inventory (§11.3). References include trashed albums: their
-- originals still belong to the catalog. The list includes unreferenced rows
-- so doctor can distinguish them from missing files and orphaned files.
-- name: DoctorBlobs :many
SELECT b.hash, b.size, EXISTS (
    SELECT 1 FROM tracks t WHERE t.blob_hash = b.hash OR t.lyrics_hash = b.hash
) OR EXISTS (SELECT 1 FROM attachments a WHERE a.blob_hash = b.hash)
  OR EXISTS (SELECT 1 FROM albums al WHERE al.cover_hash = b.hash) AS referenced
FROM blobs b ORDER BY b.hash;

-- name: DoctorAlbums :many
SELECT al.id, ar.name AS artist_name, al.title,
       (al.deleted_at IS NULL)::boolean AS active,
       al.revision, al.published_path, al.published_revision,
       al.published_renderer, al.published_build, al.published_receipt_hash,
       j.state AS job_state
FROM albums al JOIN artists ar ON ar.id = al.artist_id
LEFT JOIN jobs j ON j.kind = 'render' AND j.album_id = al.id
ORDER BY al.id;

-- name: DoctorClaims :many
SELECT path_key, path, album_id FROM path_claims ORDER BY path_key;

-- name: DoctorPublication :many
SELECT album_id, build_id, old_path, new_path FROM publication;

-- name: DoctorStructuralIssues :many
SELECT 'doctor_album_no_tracks'::text AS code, id::text AS entity
FROM albums a WHERE deleted_at IS NULL AND NOT EXISTS (SELECT 1 FROM tracks t WHERE t.album_id=a.id)
UNION ALL
SELECT 'doctor_missing_blob', t.id::text FROM tracks t LEFT JOIN blobs b ON b.hash=t.blob_hash WHERE b.hash IS NULL
UNION ALL
SELECT 'doctor_missing_blob', a.id::text FROM attachments a LEFT JOIN blobs b ON b.hash=a.blob_hash WHERE b.hash IS NULL
UNION ALL
SELECT 'doctor_missing_album', j.id::text FROM jobs j LEFT JOIN albums a ON a.id=j.album_id WHERE j.kind='render' AND a.id IS NULL
ORDER BY code, entity;

-- Blobs (§7.5, §7.6): the rows of blobs already pinned on disk. An existing
-- row is kept; the caller compares size and format with GetBlobs. A
-- duration of -1 is unknown (NULL, NOTES.md N-300).
-- name: InsertBlobs :exec
INSERT INTO blobs (hash, size, format, duration_ms, created_at)
SELECT u.hash, u.size, NULLIF(u.format, ''), NULLIF(u.duration_ms, -1), now()
FROM (SELECT unnest(@hashes::text[]) AS hash, unnest(@sizes::bigint[]) AS size,
             unnest(@formats::text[]) AS format, unnest(@durations::bigint[]) AS duration_ms) AS u
ON CONFLICT (hash) DO NOTHING;

-- name: GetBlobs :many
SELECT hash, size, format FROM blobs WHERE hash = ANY(@hashes::text[]) ORDER BY hash;

-- A content-derived format replaces "unknown", never another format.
-- name: SetBlobFormat :execrows
UPDATE blobs SET format = @format WHERE hash = @hash AND format IS NULL;

-- A probed duration replaces "unknown", never a known one (N-300, N-301):
-- the first value recorded stays, whatever a later probe says. Only an audio
-- blob has one (blobs_duration_check); any other row is left alone.
-- name: SetBlobDuration :execrows
UPDATE blobs SET duration_ms = @duration_ms
WHERE hash = @hash AND duration_ms IS NULL AND format IN ('flac', 'mp3', 'm4a-aac', 'm4a-alac');

-- Artists (§4.3, §7.6).
-- name: GetArtist :one
SELECT id, name, folder_key, revision FROM artists WHERE id = $1;

-- name: GetArtistByFolderKey :one
SELECT id, name, folder_key, revision FROM artists WHERE folder_key = $1;

-- name: InsertArtist :exec
INSERT INTO artists (id, name, folder_key, revision) VALUES ($1, $2, $3, 1);

-- name: RenameArtist :one
UPDATE artists SET name = $2, folder_key = $3, revision = revision + 1
WHERE id = $1
RETURNING revision;

-- An artist left without any album, active or trashed, is deleted in the
-- transaction that took its last album away (owner decision, NOTES.md
-- N-297). One row or none: an artist that still has an album is kept.
-- name: DeleteOrphanArtist :execrows
DELETE FROM artists ar
WHERE ar.id = @id AND NOT EXISTS (SELECT 1 FROM albums al WHERE al.artist_id = ar.id);

-- Every album of an artist, trashed ones included: a rename changes the
-- tags and the folder of all of them (§4.3).
-- name: ListArtistAlbumIDs :many
SELECT id FROM albums WHERE artist_id = $1 ORDER BY id;

-- Albums (§4.2, §4.3).
-- name: GetAlbum :one
SELECT * FROM albums WHERE id = $1;

-- name: GetAlbumByFingerprint :one
SELECT id, deleted_at FROM albums WHERE import_fingerprint = $1;

-- The active album holding a folder under an artist, other than exclude
-- (albums_active_folder_key, §4.2).
-- name: FindActiveAlbumByFolder :one
SELECT id, title FROM albums
WHERE artist_id = @artist_id AND folder_key = @folder_key AND deleted_at IS NULL AND id <> @exclude;

-- name: InsertAlbum :exec
INSERT INTO albums (id, artist_id, title, folder_key, year, genre, compilation, cover_hash, revision, import_fingerprint)
VALUES (@id, @artist_id, @title, @folder_key, @year, @genre, @compilation, @cover_hash, 1, @import_fingerprint);

-- name: UpdateAlbumMetadata :exec
UPDATE albums
SET artist_id = @artist_id, title = @title, folder_key = @folder_key, year = @year,
    genre = @genre, compilation = @compilation
WHERE id = @id;

-- Trash (deleted_at = now) or restore (deleted_at = NULL), §4.3.
-- name: SetAlbumTrashed :exec
UPDATE albums SET deleted_at = CASE WHEN @trashed::boolean THEN now() END WHERE id = @id;

-- The revision half of §4.3's "bump and enqueue".
-- name: BumpAlbumRevision :one
UPDATE albums SET revision = revision + 1 WHERE id = $1 RETURNING revision;

-- What the path claims of an album are derived from (§5.3).
-- name: GetAlbumPathState :one
SELECT (al.deleted_at IS NULL)::boolean AS active, al.title, ar.name AS artist_name, al.published_path
FROM albums al JOIN artists ar ON ar.id = al.artist_id
WHERE al.id = $1;

-- The publication journal, zero or one row (§9.3).
-- name: GetPublication :one
SELECT album_id, old_path, new_path FROM publication WHERE id = 1;

-- Tracks (§4.2). COPY: an import writes up to 1,000 tracks (§7.2) in one
-- short transaction.
-- name: InsertTracks :copyfrom
INSERT INTO tracks (id, album_id, disc, no, title, artist, genre, blob_hash, source_path, lyrics_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ListAlbumTracks :many
SELECT id, disc, no, title, artist, genre FROM tracks WHERE album_id = $1 ORDER BY disc, no, id;

-- The audio formats of an album's tracks, for the per-format rules of a
-- change (N-162).
-- name: ListAlbumAudioFormats :many
SELECT DISTINCT b.format FROM tracks t JOIN blobs b ON b.hash = t.blob_hash
WHERE t.album_id = $1 AND b.format IS NOT NULL ORDER BY b.format;

-- The (album_id, disc, no) constraint is deferred to the commit, so a swap
-- of numbers needs no temporary values (§4.2, §12.2).
-- name: UpdateTrack :execrows
UPDATE tracks SET disc = @disc, no = @no, title = @title, artist = @artist, genre = @genre
WHERE id = @id AND album_id = @album_id;

-- Attachments (§4.2, §7.4). COPY for the same reason as tracks.
-- name: InsertAttachments :copyfrom
INSERT INTO attachments (id, album_id, rel_path, path_key, blob_hash) VALUES ($1, $2, $3, $4, $5);

-- Path claims (§5.3).
-- name: ListAlbumClaims :many
SELECT path_key, path FROM path_claims WHERE album_id = $1 ORDER BY path_key;

-- name: GetClaims :many
SELECT path_key, path, album_id FROM path_claims WHERE path_key = ANY(@keys::text[]) ORDER BY path_key;

-- Inserts a claim, or updates the display path of a claim the album already
-- owns. Zero rows means another album owns the key.
-- name: UpsertClaim :execrows
INSERT INTO path_claims (path_key, path, album_id) VALUES (@path_key, @path, @album_id)
ON CONFLICT (path_key) DO UPDATE SET path = EXCLUDED.path
WHERE path_claims.album_id = EXCLUDED.album_id;

-- name: DeleteClaim :execrows
DELETE FROM path_claims WHERE path_key = @path_key AND album_id = @album_id;

-- The API's reads (§10.1, §10.2). They run in one REPEATABLE READ snapshot
-- (store.InSnapshotTx), so that a representation and its revision agree.
-- Orders are deterministic and byte-wise (COLLATE "C"), independent of the
-- database's locale.

-- Every artist of the catalog, those without albums included (owner
-- decision N-146: the list feeds the album editor's artist selector).
-- name: ListArtists :many
SELECT id, name, folder_key, revision FROM artists
ORDER BY folder_key COLLATE "C", id;

-- The desired album (§10.2 GET /api/albums/{id}): no published column.
-- name: GetAlbumView :one
SELECT al.id, al.artist_id, ar.name AS artist_name, al.title, al.year, al.genre, al.compilation,
       (al.deleted_at IS NOT NULL)::boolean AS trashed, al.revision,
       al.cover_hash, cb.size AS cover_size, cb.format AS cover_format
FROM albums al
JOIN artists ar ON ar.id = al.artist_id
LEFT JOIN blobs cb ON cb.hash = al.cover_hash
WHERE al.id = $1;

-- Albums as the queue views show them (NOTES.md N-286): the title, the
-- artist, whether there is a cover and whether the album is in the trash.
-- name: ListAlbumCards :many
SELECT al.id, al.title, ar.name AS artist_name,
       (al.cover_hash IS NOT NULL)::boolean AS has_cover, (al.deleted_at IS NOT NULL)::boolean AS trashed
FROM albums al
JOIN artists ar ON ar.id = al.artist_id
WHERE al.id = ANY(@ids::uuid[]);

-- name: ListAlbumTrackViews :many
SELECT t.id, t.disc, t.no, t.title, t.artist, t.genre, t.source_path,
       t.blob_hash, b.size AS blob_size, b.format AS blob_format, b.duration_ms AS blob_duration_ms, t.lyrics_hash
FROM tracks t
JOIN blobs b ON b.hash = t.blob_hash
WHERE t.album_id = $1
ORDER BY t.disc, t.no, t.id;

-- name: ListAlbumAttachmentViews :many
SELECT a.id, a.rel_path, a.blob_hash, b.size AS blob_size, b.format AS blob_format
FROM attachments a
JOIN blobs b ON b.hash = a.blob_hash
WHERE a.album_id = $1
ORDER BY a.path_key COLLATE "C", a.id;

-- The processing state (§10.2 GET /api/albums/{id}/status): revisions,
-- published renderer and path (relative to library/), the render job.
-- name: GetAlbumStatus :one
SELECT al.id, al.revision, (al.deleted_at IS NOT NULL)::boolean AS trashed,
       al.published_path, al.published_revision, al.published_renderer,
       j.id AS job_id, j.state AS job_state, j.error_code AS job_error_code,
       j.error_message AS job_error_message, j.queued_at AS job_queued_at, j.updated_at AS job_updated_at
FROM albums al
LEFT JOIN jobs j ON j.kind = 'render' AND j.album_id = al.id
WHERE al.id = $1;

-- The editor's content operations (§4.3, §10.2): cover, attachments,
-- lyrics and track deletion. Each runs in store.InCatalogTx with the
-- album's revision compared first.

-- name: SetAlbumCover :exec
UPDATE albums SET cover_hash = @cover_hash WHERE id = @id;

-- An attachment of an album: the album is part of the key, so an
-- attachment of another album is not found (§10.2 downloads by id).
-- name: GetAlbumAttachment :one
SELECT a.id, a.rel_path, a.path_key, a.blob_hash FROM attachments a
WHERE a.id = @id AND a.album_id = @album_id;

-- name: ListAlbumAttachmentPaths :many
SELECT id, rel_path FROM attachments WHERE album_id = $1 ORDER BY path_key COLLATE "C", id;

-- name: InsertAttachment :exec
INSERT INTO attachments (id, album_id, rel_path, path_key, blob_hash) VALUES (@id, @album_id, @rel_path, @path_key, @blob_hash);

-- name: DeleteAttachment :execrows
DELETE FROM attachments WHERE id = @id AND album_id = @album_id;

-- A track of an album, with the same rule as GetAlbumAttachment.
-- name: GetAlbumTrack :one
SELECT id, lyrics_hash FROM tracks WHERE id = @id AND album_id = @album_id;

-- name: SetTrackLyrics :execrows
UPDATE tracks SET lyrics_hash = @lyrics_hash WHERE id = @id AND album_id = @album_id;

-- name: CountAlbumTracks :one
SELECT count(*) FROM tracks WHERE album_id = $1;

-- name: DeleteTrack :execrows
DELETE FROM tracks WHERE id = @id AND album_id = @album_id;

-- §10.2 GET /api/albums: album summaries in the library's order, the
-- artist's folder key, then the album's, then the id, byte-wise (COLLATE
-- "C"). Keyset pagination: the page starts after the cursor's triple
-- (NOTES.md N-190). The text search is applied by the caller with the
-- normalization of internal/names, never with SQL lower() (§5.2). With
-- @failed the page keeps only the albums whose render job failed (§10.3
-- "Errore"), active or trashed, and @trashed is ignored (NOTES.md N-245).
-- name: ListAlbumSummaries :many
SELECT al.id, al.revision, al.artist_id, ar.name AS artist_name, ar.folder_key AS artist_key,
       al.title, al.folder_key AS title_key, al.year, al.genre, al.compilation,
       (al.deleted_at IS NOT NULL)::boolean AS trashed,
       al.cover_hash, cb.size AS cover_size, cb.format AS cover_format,
       al.published_path, al.published_revision, al.published_renderer,
       j.state AS job_state, j.error_code AS job_error_code, j.error_message AS job_error_message
FROM albums al
JOIN artists ar ON ar.id = al.artist_id
LEFT JOIN blobs cb ON cb.hash = al.cover_hash
LEFT JOIN jobs j ON j.kind = 'render' AND j.album_id = al.id
WHERE (CASE WHEN @failed::boolean THEN j.state IS NOT DISTINCT FROM 'failed'
            ELSE (al.deleted_at IS NOT NULL) = @trashed::boolean END)
  AND (NOT @by_artist::boolean OR al.artist_id = @artist_id::uuid)
  AND (@first::boolean
       OR (ar.folder_key COLLATE "C", al.folder_key COLLATE "C", al.id)
          > (@after_artist::text, @after_title::text, @after_id::uuid))
ORDER BY ar.folder_key COLLATE "C", al.folder_key COLLATE "C", al.id
LIMIT @lim::int;

-- The number of albums the Library's "Da sistemare" filter lists: one
-- render job per album (jobs_render_album_key), so the failed render jobs
-- count the albums in §10.3 "Errore", active or trashed (NOTES.md N-245).
-- name: CountFailedAlbums :one
SELECT count(*)::bigint AS failed FROM jobs WHERE kind = 'render' AND state = 'failed';
