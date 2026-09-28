# Operating musiclib (Ubuntu 24.04+, Docker Engine, local ext4)

DESIGN.md §3, §10.4, §11–§12. See [Docker and tests](docker.md) and [the UI](ui.md). Run all commands from the repository root. Install Docker Engine with the Compose v2 plugin; do not install Go, PostgreSQL or media tools on the host. Use a local ext4 filesystem for `/data` and for the test volume; no nested mounts under `/data`. Keep backups on a **different physical disk** when possible.

## First start

```sh
mkdir -p import
# Create an ext4-backed data and backup directory, owned by the app UID.
sudo mkdir -p /srv/musiclib/data /mnt/backup/musiclib
sudo chown 1000:1000 /srv/musiclib/data /mnt/backup/musiclib
cat > .env <<'EOF'
POSTGRES_PASSWORD=replace-with-a-strong-password
MUSICLIB_UID=1000
MUSICLIB_GID=1000
MUSICLIB_DATA=/srv/musiclib/data
MUSICLIB_BACKUP=/mnt/backup/musiclib
MUSICLIB_IMPORT=./import
PUBLIC_ORIGIN=http://127.0.0.1:8080
EOF
docker compose --profile app up -d --build --wait
curl -f http://127.0.0.1:8080/health/ready
docker compose logs -f app
```

A bind-mounted `MUSICLIB_BACKUP` directory must be writable by `MUSICLIB_UID` (and its group by `MUSICLIB_GID`); a new named backup volume inherits the runtime image's ownership. The `.env` file contains a password: restrict its permissions (`chmod 600 .env`) and keep it out of backups/shared checkouts. `DATABASE_URL` is assembled from the Compose database and `POSTGRES_PASSWORD`; outside Compose, supply a PostgreSQL URI or pgx keyword/value connection string via `DATABASE_URL`. Offline pg_dump/pg_restore use a password-free libpq URI built from host, port, user, database and approved TLS settings; pgx-only query options are not passed to libpq. For TLS settings use the PostgreSQL URI form (keyword/value DSNs are accepted for non-TLS connections only); encrypted client keys requiring `sslpassword` are not supported by the offline tools. Passwords go to the child via `PGPASSWORD`, not command arguments. `PUBLIC_ORIGIN` is mandatory for the binary and must match the browser's exact host and port; Compose defaults it to the loopback URL. `HTTP_ADDR` defaults to `:8080` (Compose sets it explicitly), `WORKERS` to min(4, available CPUs), allowed 1–16; the pgx pool limit is WORKERS + 8. The binary sets umask 022 and refuses root. PostgreSQL 17 uses `fsync=on`, `full_page_writes=on`, `synchronous_commit=on` and has no published port. The images and clients are pinned.

The fixed paths in the container are `/data`, `/import` (read-only, must exist) and `/backup` (must not be inside `/data`). `MUSICLIB_BACKUP` must not be a host directory inside `MUSICLIB_DATA` either: backup and restore compare the filesystem device and root of both mounts from `/proc/self/mountinfo` and refuse a nested destination with `backup_destination` (exit 2). The check sees bind mounts of one host filesystem; it cannot see through a network share or a second filesystem layered over the data directory, so keep the two host paths plainly separate. `/data/.lock` is never deleted; `.musiclib-store` identifies the paired database; `originals/` is immutable content-addressed media; `library/` is disposable published output; `work/` is staging. Do not edit `library/` or change `.maintenance` manually. Only one app instance per volume and database. A missing store marker cannot be replaced by pointing an existing database at a new volume.

`GET /health/live` checks the HTTP process; `GET /health/ready` checks boot, recovery and PostgreSQL. Read JSON logs with `docker compose logs --tail=100 app`. Import by placing albums under the configured `MUSICLIB_IMPORT`, then open the Import page at `PUBLIC_ORIGIN` or use `POST /api/imports` (see [Docker API examples](docker.md#importing-the-queue-and-the-library-list)). Do not change or unmount the source before jobs complete. Job failures remain visible in Activity and require an explicit retry.

## Capacity and upgrades

Budget roughly **originals + library** (about twice the source bytes), plus concurrent staging and retired albums, tags and covers. Allow at least 1 GiB free beyond each job's conservative reservation; backups need additional space *outside* `/data`, at least the full originals plus the dump. A full disk can still cause a write error: fix capacity, then retry. Do not remove originals to make space.

Back up before an update. Stop the app, fetch the reviewed pinned source/image changes, rebuild the image and start the app. Boot applies supported forward-only migrations before workers. Do not run older binaries against a newer schema. Changing `render_version` queues a new full render of active albums that have no existing job; failed render jobs are not silently retried. Check Activity and then run doctor. Never use a floating image tag.

## Offline commands

Leave PostgreSQL running. Scripts stop the app, take the nonblocking `/data/.lock` via the offline command, and restart the app after success; doctor also restarts after exit 1 (findings) if the app was running. They leave the app stopped after any destructive command failure or doctor refusal (exit 2). The raw equivalent is `docker compose stop app; docker compose --profile app run --rm --no-deps app doctor --deep; docker compose --profile app start app` (restart only after success). If the app holds the lock, all commands refuse at once.

```sh
scripts/doctor.sh --deep                 # or scripts/doctor.sh (no content hashes)
docker compose --profile app run --rm --no-deps --entrypoint cat app /data/.musiclib-store
scripts/rebuild.sh 'STORE_UUID_FROM_MARKER'
scripts/backup.sh '2026-09-26 full'     # creates /backup/2026-09-26 full
```

Doctor is read-only apart from `.lock`: no migration, repair or journal recovery. Exit 0 means no **error** findings (warnings and pending work may remain), 1 means damage, 2 means refusal/usage. Each finding has severity, stable code, relative entity and advice. Normal mode checks existence/size, reservations, DB-anchored receipts and expected output; deep mode hashes all originals and receipt-listed files, including tags, covers and attachments. A changed file with unchanged size and mtime needs deep mode. An extra output is reported, not deleted. A journal is reported, never resolved: start the server for normal recovery or explicitly rebuild. Unreferenced originals are information; do **not** delete them. Damaged originals need a good backup copy: a render/rebuild only repairs output.

Rebuild requires the **exact** store UUID from both `.musiclib-store` and DB; it deletes only `library/` and `work/`, resets derived publication/claims/jobs atomically and schedules renders of active albums. Trashed albums stay trashed and their output disappears. It does not roll back edits or repair originals. If killed, `.maintenance` blocks boot: rerun the *same* rebuild UUID until success. Never remove the marker manually.

Backup creates a unique `.musiclib-backup-*.tmp` directory in `/backup`, verifies every original while streaming its copy, creates `catalog.dump` (PostgreSQL custom format) and `manifest.json`, fsyncs them and atomically renames the directory to its final name only on success. It never overwrites. `/import`, `library/`, `work/` are not saved. An interrupted/failed backup leaves only the named temporary; **the operator** inspects and removes obsolete temporary directories, never the app. Retain multiple complete generations and copy them to an external device; do not overwrite the only copy. Verify the backup by restoring to *separate new volumes* periodically. Backups cover completed imports; unfinished scan/import jobs become failed after restore until the source is checked or remounted and explicitly retried.

## Restore: use new empty destinations

Restore **never overwrites**. Empty database means no user tables, sequences or views in its schema, including goose metadata; empty data volume means no entries except `.lock` and an empty ext4 `lost+found` directory. A failed restore leaves `.maintenance`; do not start the server or retry into that partially restored destination. Create a **new PostgreSQL volume/database and new data volume**, retaining the old ones for investigation, then repeat from the completed backup. To replace a lost Compose installation safely, provision a separate Compose project (or move the old volumes away), point `MUSICLIB_DATA` at a new empty ext4 directory, and ensure PostgreSQL's `pgdata` is a new empty volume. Confirm `docker compose ps` and the mounts before proceeding; never run `down -v` against the only surviving backup or against a database you need. Mount the completed backup as `/backup` with `MUSICLIB_BACKUP`. Then:

```sh
docker compose up -d postgres
# Wait for PostgreSQL to be healthy before the script (it uses run --no-deps):
docker compose exec -T postgres pg_isready -U musiclib -d musiclib
scripts/restore.sh '2026-09-26 full'
# Only after exit 0 (the script does not auto-start a previously stopped app):
docker compose --profile app start app
curl -f http://127.0.0.1:8080/health/ready
scripts/doctor.sh --deep
```

If `start app` reports no existing container, use `docker compose --profile app up -d --build --wait` instead. The restored catalog and originals keep their identities; all published output is regenerated. Wait until Activity is idle before deep doctor. Corrupt dump, manifest or originals are refused. Keep the completed backup read-only and unchanged throughout restore; an external change between verification and pg_restore may leave a marker and require new destinations. Never use a temporary backup directory as restore input.

A backup made before schema 3 restores the same way: the boot adds the track durations' column, and the renders that regenerate the output record the duration of every active album's tracks (trashed albums get theirs when restored). On an installation that stays up, **Rebuild the library folder** (Activity → Advanced) does the same. Such a backup may also hold artists without albums, created before the rule that removes them (NOTES.md N-297); NOTES.md N-299 has the SQL to list them and to delete them, to run by hand with the app stopped.

## Security and troubleshooting

The default published address is **127.0.0.1 only**. LAN use needs both `MUSICLIB_BIND` and matching `PUBLIC_ORIGIN`; do not expose it directly to the Internet. For remote access put an authenticated reverse proxy in front (outside this application's scope). The API checks Host, Origin and `X-Musiclib-Request: 1`; it does not implement users or CORS. Mount `/import` read-only and never point it into `/data`.

| Code | Action |
|---|---|
| `volume_locked` | Stop the running server or another offline command; do not remove `.lock`. |
| `volume_maintenance_pending`, `volume_maintenance_malformed` | Re-run the interrupted rebuild; after a failed restore recreate **both** destinations. Do not delete the marker. |
| `volume_store_mismatch`, `volume_db_uninitialized`, `volume_marker_missing` | Pair the correct database and volume or restore on new destinations. |
| `maintenance_database` | Keep PostgreSQL running and check the credentials without printing the URL. |
| `maintenance_schema` | doctor, rebuild and backup never migrate: start the app once with this binary so its boot applies the migrations, stop it, retry. |
| `rebuild_store_id` | Pass the exact UUID from `/data/.musiclib-store`; nothing was deleted. |
| `rebuild_delete`, `rebuild_unsafe_tree` (exit 1) | Fix the cause named in the log (permissions, a mount or non-directory under `library/`/`work/`), then repeat the same rebuild. |
| `backup_exists`, `backup_destination` | Choose an unused name in a directory outside `/data` (not `library/`, `work/` or `originals/`, not through a symlink), and mount a `MUSICLIB_BACKUP` that is not under the `MUSICLIB_DATA` host directory. |
| `backup_corrupt_blob`, `backup_blob_size`, `backup_blob_missing`, `backup_unsafe_original` (exit 1) | An original is damaged, missing or unexpected: run `scripts/doctor.sh --deep`, recover the original from an older backup; remove the leftover `.musiclib-backup-*.tmp`. |
| `backup_dump_failed` (exit 1) | `pg_dump` failed; its redacted stderr is in the message. Remove the leftover temporary and retry. |
| `restore_volume_not_empty`, `restore_database_not_empty` | Nothing was written: point the restore at a new empty database and a new empty data volume. |
| `restore_manifest_invalid`, `restore_archive_invalid`, `restore_archive_extra`, `restore_dump_hash`, `restore_blob_hash`, `restore_blob_missing` | The archive failed verification before anything was written: use another complete, verified backup. |
| `restore_schema_too_new`, `restore_schema_unsupported` | Use a binary at least as new as the one that made the backup. |
| `restore_dump_failed`, `restore_migrate`, `restore_store_id`, `restore_catalog_blob_missing`, `restore_catalog_blob_size`, `restore_blob_mismatch` (exit 1, marker left) | Recreate **both** destinations and repeat from another verified backup if it fails again. |
| `doctor_receipt_hash`, `doctor_receipt_invalid`, `doctor_receipt_unreadable`, `doctor_output_hash`, `doctor_output_size`, `doctor_output_missing` | Render the affected album or rebuild after reviewing findings. |
| `doctor_output_type`, `doctor_output_extra`, `doctor_originals_extra`, `doctor_unreadable` | Inspect the named path by hand; doctor never follows symlinks and never deletes. |
| `doctor_claim_invalid`, `doctor_claim_missing`, `doctor_claim_collision`, `doctor_invalid_path`, `doctor_published_state`, `doctor_album_no_tracks`, `doctor_missing_blob`, `doctor_missing_album` | Catalog inconsistency: keep the app stopped, take a backup of the evidence and investigate; rebuild resets reservations and publication state only. |
| `doctor_blob_damaged`, `doctor_blob_hash` | Restore a good original from backup; rebuild alone cannot fix it. |
| `doctor_pending_work`, `doctor_journal_pending`, `doctor_unreferenced_blob` | Not damage: start the app (recovery/workers), or choose explicit rebuild for a stuck journal; keep unreferenced originals. |
| `doctor_failed` (exit 1) | The inspection could not complete (I/O or database error); nothing was changed. Fix the cause and rerun. |

Command exit codes: 0 = success/no doctor errors, 1 = damage or attempted operation failure, 2 = refused/invalid usage (nothing written except `.lock`, or an incomplete/foreign maintenance marker). A failure (exit 1) is logged with its code and an `advice` field. Refer to [the detailed boot/error table](docker.md#when-the-app-refuses-to-start) for all other codes.

## Release check on a native host

DESIGN.md §2.1 and §12.1 require a **native** Ubuntu 24.04+ Docker Engine with `/var/lib/docker` (testdata named volume) and `MUSICLIB_DATA` on ext4. Docker Desktop testing is not a substitute (NOTES.md N-017). Before a release, run:

```sh
findmnt -no FSTYPE /var/lib/docker      # must report ext4
findmnt -no FSTYPE /srv/musiclib/data   # must report ext4
scripts/lint-shell.sh
docker compose --profile app build app
docker compose --profile app run --rm --no-deps --entrypoint /usr/lib/postgresql/17/bin/pg_dump app --version
docker compose --profile app run --rm --no-deps --entrypoint /usr/lib/postgresql/17/bin/pg_restore app --version
# Both clients must report PostgreSQL 17.11.
scripts/check.sh
scripts/dev.sh go test -race -count=2 ./internal/maintenance ./internal/volume ./internal/catalog ./cmd/musiclibd
scripts/dev.sh go test -race -count=3 -run 'TestRebuildRealCrashWindows|TestRestoreRealCrashWindows|TestBackupRealCrashWindows' ./internal/maintenance
scripts/dev.sh go test -race -run 'TestBackupLossRestoreBootAndDoctor|TestReleaseCollectionInterruptedAndRestored' ./cmd/musiclibd
```

Record the machine/kernel, commands, output and any skipped tests; do not release until the native gate and the §12.3 acceptance run pass. This repository's Docker Desktop gate uses real ext4 inside the VM, not the native host kernel.
