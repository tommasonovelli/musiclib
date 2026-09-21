-- Serializes store.Migrate callers. Session-level, on a dedicated connection:
-- closing it releases the lock even when the unlock never runs. The key is the
-- ASCII "mlmigrat", distinct from the catalog lock of §5.3.
-- name: LockMigrations :exec
SELECT pg_advisory_lock(7884797347093438836);
