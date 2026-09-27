-- Offline rebuild (§11.3), always inside one InCatalogTx transaction.
-- Clearing the journal before resetting the published state permits a
-- repeat after a crash at any deletion or transaction boundary.
-- name: RebuildClearJournal :exec
DELETE FROM publication;

-- name: RebuildClearPublished :exec
UPDATE albums SET published_path = NULL, published_revision = 0,
    published_renderer = NULL, published_build = NULL,
    published_receipt_hash = NULL;

-- name: RebuildClearRenders :exec
DELETE FROM jobs WHERE kind = 'render';

-- name: RebuildClearClaims :exec
DELETE FROM path_claims;

-- name: RebuildActiveAlbums :many
SELECT id FROM albums WHERE deleted_at IS NULL ORDER BY id;

-- A restore is allowed only on a brand-new database (§11.4). Check before
-- creating even goose's metadata table. Include sequences and views too.
-- name: RestoreDatabaseObjects :one
SELECT count(*)::bigint FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p', 'v', 'm', 'S');

-- name: RestoreFailSourceJobs :execrows
UPDATE jobs SET state='failed', claimed=NULL, error_code='source_needs_verification',
    error_message='Verify or remount the import source and retry this job after restore.',
    updated_at=now()
WHERE kind IN ('scan','import') AND state IN ('pending','running');

-- Offline commands read the schema version without creating goose's table
-- (§11.3: doctor is read-only; goose's GetDBVersion would create it).
-- name: SchemaVersionTableExists :one
SELECT (to_regclass('goose_db_version') IS NOT NULL)::boolean AS present;
