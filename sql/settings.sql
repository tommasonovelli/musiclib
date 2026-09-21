-- name: GetStoreID :one
SELECT store_id FROM settings WHERE id = 1;

-- First initialization (§11.1): a repeated or concurrent init keeps the first
-- value; the caller reads it back with GetStoreID.
-- name: InsertStoreID :execrows
INSERT INTO settings (id, store_id) VALUES (1, $1)
ON CONFLICT (id) DO NOTHING;
