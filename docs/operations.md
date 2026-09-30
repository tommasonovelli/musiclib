# Operating Vibrance MusicLib (Ubuntu 24.04+, Docker Engine, local ext4)

See also [Docker and tests](docker.md). Run all commands from the directory that holds `compose.yaml` and `.env` (the repository root for a source build). Install Docker Engine with the Compose v2 plugin, from Docker's own packages or from Ubuntu's `docker.io` and `docker-compose-v2` (verified on Ubuntu 26.04 with Engine 29.1.3 and Compose 2.40.3), not Docker Desktop; do not install Go, PostgreSQL or media tools on the host. Use a local ext4 filesystem for `/data` and for the test volume; no nested mounts under `/data`. Keep backups on a **different physical disk** when possible.

## First start

A production installation needs only two files of the release, `compose.yaml` and `env.example` (the repository's `.env.example`, attached without its leading dot), and runs the published image `ghcr.io/tommasonovelli/musiclib:1.0.0` (Linux amd64); nothing is built. **The image exists only once the `v1.0.0` release is published**: until then `docker compose up` fails to pull it, and you run from source instead (below).

```sh
mkdir -p ~/musiclib/import && cd ~/musiclib
[ -e compose.yaml ] || curl -fsSLO https://github.com/tommasonovelli/vibrance-musiclib/releases/latest/download/compose.yaml
[ -e .env ] || { curl -fsSL -o .env https://github.com/tommasonovelli/vibrance-musiclib/releases/latest/download/env.example && chmod 600 .env && sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 32)/" .env; }
docker compose up -d --wait      # PostgreSQL and the app; returns when the app is healthy
curl -f http://127.0.0.1:8080/health/ready
docker compose logs -f app
```

The block is idempotent: it never replaces an existing `compose.yaml` or `.env`. `import` is the read-only `/import` (or set `MUSICLIB_IMPORT` in `.env`); Compose does not create it.

**The database password has a default, `musiclib`: change it before the first start.** It is defined once, at the top of `compose.yaml` (`x-db-password`), for both services; `POSTGRES_PASSWORD` in `.env` overrides it, and an empty value means the default. The block above writes a random one into `.env` without printing it. Without the block, set it in `.env` (preferred: a new release's `compose.yaml` comes with the default again) or in place of `musiclib` in `compose.yaml`. The database publishes no port, so other machines cannot reach it; a password of your own is still better. PostgreSQL reads it only when its volume is first initialized: changing it later does not change the database's password, and the app can no longer connect. To change it, run `docker compose exec postgres psql -U musiclib -d musiclib -c '\password musiclib'`, put the same value in `.env`, then `docker compose up -d --wait`.

The defaults keep the database, `/data` and `/backup` in the named volumes `musiclib_db`, `musiclib_data` and `musiclib_backup`. For host directories on ext4 (a data disk, a backup disk), set them in `.env` before the first start:

```sh
# Create an ext4-backed data and backup directory, owned by the app UID.
sudo mkdir -p /srv/musiclib/data /mnt/backup/musiclib
sudo chown 1000:1000 /srv/musiclib/data /mnt/backup/musiclib
cat >> .env <<'EOF'
MUSICLIB_DATA=/srv/musiclib/data
MUSICLIB_BACKUP=/mnt/backup/musiclib
MUSICLIB_IMPORT=/srv/music
EOF
```

Use a **new, empty directory dedicated to MusicLib** for `MUSICLIB_DATA`, never your music collection or a directory shared with anything else. MusicLib creates only `originals/`, `library/`, `work/` and its marker files (`.lock`, `.musiclib-store`, and `.maintenance` while a rebuild or restore runs) there, and never changes or deletes any other entry; but it does not refuse a directory that already holds other files, so this check is yours. The music to import goes in `MUSICLIB_IMPORT`, mounted read-only.

The image runs as `1000:1000` and owns `/data` and `/backup`, so a new named volume belongs to `1000:1000`. `MUSICLIB_UID`/`MUSICLIB_GID` in `.env` run the process as another uid (Compose `user:`); the named volumes do not follow, so another uid needs `MUSICLIB_DATA` and `MUSICLIB_BACKUP` as host directories owned by it (and `/import` readable by it). A bind-mounted `MUSICLIB_BACKUP` directory must be writable by `MUSICLIB_UID` (and its group by `MUSICLIB_GID`). The `.env` file contains a password: restrict its permissions (`chmod 600 .env`) and keep it out of backups/shared checkouts.

Compose sets `DATABASE_URL` without a password and passes the password as `PGPASSWORD`, which the PostgreSQL client (pgx) uses for a connection string that has none; so the password need not be URL-safe. Outside Compose, supply a PostgreSQL URI or pgx keyword/value connection string via `DATABASE_URL`, with its password or with `PGPASSWORD`. Offline pg_dump/pg_restore use a password-free libpq URI built from host, port, user, database and approved TLS settings; pgx-only query options are not passed to libpq. For TLS settings use the PostgreSQL URI form (keyword/value DSNs are accepted for non-TLS connections only); encrypted client keys requiring `sslpassword` are not supported by the offline tools. Passwords go to the child via `PGPASSWORD`, not command arguments. `PUBLIC_ORIGIN` is mandatory for the binary and must match the browser's exact host and port; Compose defaults it to the loopback URL. `HTTP_ADDR` defaults to `:8080` (Compose sets it explicitly), `WORKERS` to min(4, available CPUs), allowed 1–16; the pgx pool limit is WORKERS + 8. The binary sets umask 022 and refuses root. PostgreSQL 17 uses `fsync=on`, `full_page_writes=on`, `synchronous_commit=on` and has no published port. The images and clients are pinned.

The fixed paths in the container are `/data`, `/import` (read-only, must exist) and `/backup` (must not be inside `/data`). `MUSICLIB_BACKUP` must not be a host directory inside `MUSICLIB_DATA` either: backup and restore compare the filesystem device and root of both mounts from `/proc/self/mountinfo` and refuse a nested destination with `backup_destination` (exit 2). The check sees bind mounts of one host filesystem; it cannot see through a network share or a second filesystem layered over the data directory, so keep the two host paths plainly separate. `/data/.lock` is never deleted; `.musiclib-store` identifies the paired database; `originals/` is immutable content-addressed media; `library/` is disposable published output; `work/` is staging. Do not edit `library/` or change `.maintenance` manually. Only one app instance per volume and database. A missing store marker cannot be replaced by pointing an existing database at a new volume.

`GET /health/live` checks the HTTP process; `GET /health/ready` checks boot, recovery and PostgreSQL. Read JSON logs with `docker compose logs --tail=100 app`. Import by placing albums under the configured `MUSICLIB_IMPORT`, then open the Import page at `PUBLIC_ORIGIN` or use `POST /api/imports` (see [Docker API examples](docker.md#importing-the-queue-and-the-library-list-round-16)). Do not change or unmount the source before jobs complete. Job failures remain visible in Activity and require an explicit retry.

### Running from source

`compose.dev.yaml` builds the app from a clone of the repository (`musiclib-app:local`, version `devel`) with the same PostgreSQL, settings and volumes. `COMPOSE_FILE` in `.env` makes it the default of plain `docker compose` commands and of the `scripts/` maintenance wrappers:

```sh
git clone https://github.com/tommasonovelli/vibrance-musiclib.git && cd vibrance-musiclib
cp .env.example .env && chmod 600 .env
sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 32)/" .env
echo 'COMPOSE_FILE=compose.dev.yaml' >> .env
mkdir -p import
docker compose up -d --build --wait    # = docker compose -f compose.dev.yaml up -d --build --wait
```

The first build compiles pinned dependencies and takes several minutes. `compose.dev.yaml` has the same password default as `compose.yaml`. Run every command of this guide from the clone's folder.

## Capacity and upgrades

Budget roughly **originals + library** (about twice the source bytes), plus concurrent staging and retired albums, tags and covers. Allow at least 1 GiB free beyond each job's conservative reservation; backups need additional space *outside* `/data`, at least the full originals plus the dump. A full disk can still cause a write error: fix capacity, then retry. Do not remove originals to make space.

Back up before an update (`scripts/backup.sh NAME`, or the plain commands under "Offline commands"). Then, with the published image, change the version of the `app` image in `compose.yaml` (the only place that names it), or take the `compose.yaml` of the new release and keep your `.env` (a database password set in `compose.yaml` instead of `.env` must be moved to `.env` first), and:

```sh
docker compose pull app
docker compose up -d --wait        # recreates the app on the new image; the volumes stay
docker compose run --rm --no-deps app version
```

From source, fetch the reviewed changes and run `docker compose up -d --build --wait` (with `COMPOSE_FILE=compose.dev.yaml`). Boot applies supported forward-only migrations before workers. Do not run older binaries against a newer schema: the server refuses with `store_schema_too_new`, and going back needs a backup made by the older version. Changing `render_version` queues a new full render of active albums that have no existing job; failed render jobs are not silently retried. Check Activity and then run doctor. Never use a floating image tag.

## Offline commands

Leave PostgreSQL running. From a clone of the repository, the scripts stop the app, take the nonblocking `/data/.lock` via the offline command, and restart the app after success if it was running; doctor also restarts after exit 1 (findings). They leave the app stopped after any destructive command failure or doctor refusal (exit 2). They act on `compose.yaml` and its published image, or on the file named by `COMPOSE_FILE` (environment or `.env`): a source build sets `COMPOSE_FILE=compose.dev.yaml`, so that the offline command runs the same image as the server. If the app holds the lock, all commands refuse at once.

```sh
scripts/doctor.sh --deep                 # or scripts/doctor.sh (no content hashes)
docker compose run --rm --no-deps --entrypoint cat app /data/.musiclib-store
scripts/rebuild.sh 'STORE_UUID_FROM_MARKER'
scripts/backup.sh '2026-09-26 full'     # creates /backup/2026-09-26 full
```

Without the repository (only `compose.yaml` and `.env`), run the same three steps by hand:

```sh
docker compose stop app
docker compose run --rm --no-deps app doctor --deep                    # or: doctor
docker compose start app
```

For the others, the middle line is `docker compose run --rm --no-deps app backup --to '/backup/2026-09-26 full'`, `... app rebuild --store-id 'STORE_UUID_FROM_MARKER'` or `... app restore --from '/backup/2026-09-26 full'` (restore: see below). Start the app again only after exit 0; after doctor's exit 1, read the findings first. After a failed rebuild, backup or restore leave it stopped, as the scripts do.

Doctor is read-only apart from `.lock`: no migration, repair or journal recovery. Exit 0 means no **error** findings (warnings and pending work may remain), 1 means damage, 2 means refusal/usage. Each finding has severity, stable code, relative entity and advice. Normal mode checks existence/size, reservations, DB-anchored receipts and expected output; deep mode hashes all originals and receipt-listed files, including tags, covers and attachments. A changed file with unchanged size and mtime needs deep mode. An extra output is reported, not deleted. A journal is reported, never resolved: start the server for normal recovery or explicitly rebuild. Unreferenced originals are information; do **not** delete them. Damaged originals need a good backup copy: a render/rebuild only repairs output.

Rebuild requires the **exact** store UUID from both `.musiclib-store` and DB; it deletes only `library/` and `work/`, resets derived publication/claims/jobs atomically and schedules renders of active albums. Trashed albums stay trashed and their output disappears. It does not roll back edits or repair originals. If killed, `.maintenance` blocks boot: rerun the *same* rebuild UUID until success. Never remove the marker manually.

Backup creates a unique `.musiclib-backup-*.tmp` directory in `/backup`, verifies every original while streaming its copy, creates `catalog.dump` (PostgreSQL custom format) and `manifest.json`, fsyncs them and atomically renames the directory to its final name only on success. It never overwrites. `/import`, `library/`, `work/` are not saved. A destination the app cannot use, such as a `/backup` that `MUSICLIB_UID` cannot write, is refused with its own code (`fs_permission`, exit 2) before anything is written. An interrupted/failed backup leaves only the named temporary; **the operator** inspects and removes obsolete temporary directories, never the app. Retain multiple complete generations and copy them to an external device; do not overwrite the only copy. Verify the backup by restoring to *separate new volumes* periodically. Backups cover completed imports; unfinished scan/import jobs become failed after restore until the source is checked or remounted and explicitly retried.

## Restore: use new empty destinations

Restore **never overwrites**. Empty database means no user tables, sequences or views in its schema, including goose metadata; empty data volume means no entries except `.lock` and an empty ext4 `lost+found` directory. A failed restore leaves `.maintenance`; do not start the server or retry into that partially restored destination. Create a **new PostgreSQL volume/database and new data volume**, retaining the old ones for investigation, then repeat from the completed backup. To replace a lost Compose installation safely, provision a separate Compose project ([next to an existing installation](#restoring-next-to-an-existing-installation)) or move the old volumes away, point `MUSICLIB_DATA` at a new empty ext4 directory, and ensure the PostgreSQL volume `db` is new and empty. Confirm `docker compose ps` and the mounts before proceeding; never run `down -v` against the only surviving backup or against a database you need. Mount the completed backup as `/backup` with `MUSICLIB_BACKUP`. Then:

```sh
# PostgreSQL only, never the app: its first start would initialize the new destinations.
docker compose up -d --wait postgres
scripts/restore.sh '2026-09-26 full'
# (by hand: docker compose run --rm --no-deps app restore --from '/backup/2026-09-26 full')
# Only after exit 0 (the script does not auto-start a previously stopped app):
docker compose up -d --wait
curl -f http://127.0.0.1:8080/health/ready
scripts/doctor.sh --deep
```

`docker compose up -d --wait` creates the app's container if it does not exist yet (from source: with `COMPOSE_FILE=compose.dev.yaml` and `--build`). The restored catalog and originals keep their identities; all published output is regenerated. Wait until Activity is idle before deep doctor. Corrupt dump, manifest or originals are refused. Keep the completed backup read-only and unchanged throughout restore; an external change between verification and pg_restore may leave a marker and require new destinations. Never use a temporary backup directory as restore input.

A backup made before schema 3 restores the same way: the boot adds the track durations' column, and the renders that regenerate the output record the duration of every active album's tracks (trashed albums get theirs when restored). On an installation that stays up, **Rebuild the library folder** (Activity → Advanced) does the same. Such a backup may also hold artists without any album, created before MusicLib began deleting an artist together with its last album; they are kept as they are, and nothing removes them automatically.

### Restoring next to an existing installation

To restore on a machine that still runs an installation (to test a backup, for example), use a second directory with its own `compose.yaml` and `.env`, such as `~/musiclib-restore` created with the install block without its last line. `compose.yaml` fixes the Compose project name, `name: musiclib`: in a second directory the same name would select the **existing** installation's containers and volumes, so every command there would act on it. Three settings in the new directory's `.env` keep the two apart:

1. **Its own project name.** `COMPOSE_PROJECT_NAME` overrides `name:`; the new project gets its own containers (`musiclib-restore`, `musiclib-restore-db`) and volumes (`musiclib-restore_db`, `musiclib-restore_data`, `musiclib-restore_backup`). Check it before any other command:

   ```sh
   echo 'COMPOSE_PROJECT_NAME=musiclib-restore' >> .env
   docker compose config | head -1          # must print: name: musiclib-restore
   ```

2. **Another port**, with `MUSICLIB_PORT` and `PUBLIC_ORIGIN` changed together (the app refuses requests for any other origin):

   ```sh
   echo 'MUSICLIB_PORT=8081' >> .env
   sed -i 's|^PUBLIC_ORIGIN=.*|PUBLIC_ORIGIN=http://127.0.0.1:8081|' .env
   ```

3. **The backup in a host directory.** The new directory's `compose.yaml` cannot mount the old project's named volume, so copy the completed backup out of it (with `cp -a`, which keeps its owner, uid 1000, and modes), into a directory writable by the app's uid for the new installation's own later backups:

   ```sh
   sudo mkdir -p /srv/musiclib-restore/backup
   sudo cp -a '/var/lib/docker/volumes/musiclib_backup/_data/2026-09-26 full' /srv/musiclib-restore/backup/
   sudo chown 1000:1000 /srv/musiclib-restore/backup
   echo 'MUSICLIB_BACKUP=/srv/musiclib-restore/backup' >> .env
   ```

   If the existing installation keeps its backups in a host directory (`MUSICLIB_BACKUP`), copy from there. Then run the restore above in the new directory and open `http://127.0.0.1:8081`. The two installations share nothing and can run side by side.

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

A release requires a passing run on a **native** Ubuntu 24.04+ Docker Engine with `/var/lib/docker` (testdata named volume) and `MUSICLIB_DATA` on ext4. Docker Desktop testing is not a substitute. Before a release, run:

```sh
findmnt -T /var/lib/docker -no FSTYPE      # must report ext4
findmnt -T /srv/musiclib/data -no FSTYPE   # must report ext4
scripts/lint-shell.sh
docker compose -f compose.dev.yaml build app
docker compose -f compose.dev.yaml run --rm --no-deps --entrypoint /usr/lib/postgresql/17/bin/pg_dump app --version
docker compose -f compose.dev.yaml run --rm --no-deps --entrypoint /usr/lib/postgresql/17/bin/pg_restore app --version
# Both clients must report PostgreSQL 17.11.
scripts/check.sh
scripts/dev.sh go test -race -count=2 ./internal/maintenance ./internal/volume ./internal/catalog ./cmd/musiclibd
scripts/dev.sh go test -race -count=3 -run 'TestRebuildRealCrashWindows|TestRestoreRealCrashWindows|TestBackupRealCrashWindows' ./internal/maintenance
scripts/dev.sh go test -race -run 'TestBackupLossRestoreBootAndDoctor|TestReleaseCollectionInterruptedAndRestored' ./cmd/musiclibd
```

Record the machine/kernel, commands, output and any skipped tests; do not release until the native gate and the acceptance tests above (a test collection imported, edited, interrupted and restored without manual fixes) pass. This repository's Docker Desktop gate uses real ext4 inside the VM, not the native host kernel.
