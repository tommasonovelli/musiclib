-- Catalog queries (DESIGN.md §4, §5.3, §7.6). Every statement here that
-- writes runs inside store.InCatalogTx, under the catalog lock below.

-- The catalog lock of §5.3: one transaction-scoped advisory lock with a
-- constant key, taken first by every catalog mutation. The key is the ASCII
-- "mlcatalg", distinct from the migration lock "mlmigrat" (N-053).
-- name: LockCatalog :exec
SELECT pg_advisory_xact_lock(7884786317834415207);

-- Blobs (§7.5, §7.6): the rows of blobs already pinned on disk. An existing
-- row is kept; the caller compares size and format with GetBlobs.
-- name: InsertBlobs :exec
INSERT INTO blobs (hash, size, format, created_at)
SELECT u.hash, u.size, NULLIF(u.format, ''), now()
FROM (SELECT unnest(@hashes::text[]) AS hash, unnest(@sizes::bigint[]) AS size,
             unnest(@formats::text[]) AS format) AS u
ON CONFLICT (hash) DO NOTHING;

-- name: GetBlobs :many
SELECT hash, size, format FROM blobs WHERE hash = ANY(@hashes::text[]) ORDER BY hash;

-- A content-derived format replaces "unknown", never another format.
-- name: SetBlobFormat :execrows
UPDATE blobs SET format = @format WHERE hash = @hash AND format IS NULL;

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
