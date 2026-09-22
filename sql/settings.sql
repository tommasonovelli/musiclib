-- name: GetStoreID :one
SELECT store_id FROM settings WHERE id = 1;

-- First initialization (§11.1): a repeated or concurrent init keeps the first
-- value; the caller reads it back with GetStoreID.
-- name: InsertStoreID :execrows
INSERT INTO settings (id, store_id) VALUES (1, $1)
ON CONFLICT (id) DO NOTHING;

-- Whether the database is still in its first-initialization state (§2.2,
-- §11.1): a missing volume marker may be completed only when nothing was ever
-- imported, uploaded, created or queued. Every other catalog table references
-- one of these (tracks, attachments and path claims need an album or an
-- artist; the publication journal needs an album).
-- name: CatalogIsEmpty :one
-- COALESCE only tells sqlc that the result is never NULL.
SELECT COALESCE(NOT EXISTS (SELECT 1 FROM blobs)
            AND NOT EXISTS (SELECT 1 FROM artists)
            AND NOT EXISTS (SELECT 1 FROM albums)
            AND NOT EXISTS (SELECT 1 FROM import_batches)
            AND NOT EXISTS (SELECT 1 FROM jobs), false)::boolean AS empty;
