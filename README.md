![Vibrance MusicLib](docs/assets/banner.png)

# Vibrance MusicLib

Vibrance MusicLib turns a local music collection into an organized library of folders and tagged files. Import albums, correct their metadata in the browser, and let MusicLib regenerate the library while keeping the imported originals unchanged.

It is a personal library manager: the result is an ordinary directory tree that you can use with your own player.

## What it does

- Imports album directories recursively, including multidisc albums, with a report of accepted and rejected candidates.
- Reads and writes tags for **FLAC, MP3 and M4A containing AAC or ALAC**. Recognizing another audio extension during a scan does not mean that format can be imported.
- Lets you edit artists, albums and tracks; manage JPEG/PNG covers, attachments and LRC lyrics; search the library; and trash or restore albums.
- Keeps a durable work queue, reports failed jobs, and supports explicit retries and rendering of individual albums or the whole library.
- Provides offline integrity checks, rebuild, backup and restore commands.

A generated album looks like this:

```text
library/
  Miles Davis/
    Kind of Blue/
      .musiclib.json
      cover.jpg
      01 - So What.flac
      01 - So What.lrc
      02 - Freddie Freeloader.flac
      Extras/
        booklet.pdf
```

`.musiclib.json` is a generated receipt. Edit metadata through MusicLib: files in `library/` are generated output, and manual changes can be replaced by a later render.

## Current status and boundaries

The supported production target is **Ubuntu 24.04 or later, Docker Engine with Compose v2, and local ext4 storage**. Start with **Linux amd64**: the current build downloads an x86_64 CMake binary, and ARM support has not been established. Docker Desktop is used for development; NAS filesystems and Docker Desktop are not declared production targets.

The implementation and existing test evidence are recorded in [PROGRESS.md](PROGRESS.md). The native Ubuntu/ext4 release acceptance gate remains open (N-017); this README does not declare a completed public release. There is currently a source build workflow, rather than a documented published application image. See the [public release checklist](opensource.md) for the remaining work, including the audit of bundled third-party software.

MusicLib runs as one application instance paired with one database and data volume. It has no player, streaming, transcoding of published audio, online music recognition, automatic filesystem watcher, or user accounts and roles. The interface is in English.

## Start from source

Use a native Linux host with Docker Engine and the Compose v2 plugin. The container build supplies Go, PostgreSQL clients, TagLib and FFmpeg; you do not need to install them on the host. The first build downloads and compiles pinned dependencies and can take several minutes.

The following example uses the default named volumes for the database, data and backups. Docker's volume storage must be on local ext4 for the data volume. Keep enough free space for originals, generated files and staging.

```sh
git clone https://github.com/tommasonovelli/musiclib.git
cd musiclib
mkdir -p import
umask 077
cat > .env <<'EOF'
POSTGRES_PASSWORD=REPLACE_WITH_YOUR_OWN_LONG_RANDOM_PASSWORD
PUBLIC_ORIGIN=http://127.0.0.1:8080
EOF
chmod 600 .env
```

**Before starting, replace the password placeholder** with your own strong random password. For this Compose example, use at least 32 random characters from `A-Z`, `a-z`, `0-9`, `_` and `-`. Compose currently inserts the same value directly into a PostgreSQL URI; reserved URI characters and Compose interpolation characters need additional handling. The built-in `musiclib` password fallback is unsuitable for deployment. Changing `.env` later does not change the password of an already initialized database.

```sh
docker compose --profile app up -d --build --wait
curl -f http://127.0.0.1:8080/health/ready
docker compose logs --tail=100 app
```

Open **http://127.0.0.1:8080/**. Plain `docker compose up -d` starts only PostgreSQL; the `app` profile builds and starts `musiclib-app:local`. Readiness becomes positive after boot, migrations and recovery complete.

Place albums in `import/`, then choose **Import** in the browser and start an import from a directory. The import directory must exist before startup, is mounted read-only, and must remain available and unchanged until its jobs finish. MusicLib copies accepted files into its own store; it does not move your source collection.

For existing source directories or host bind mounts, configure `MUSICLIB_IMPORT`, `MUSICLIB_DATA` and `MUSICLIB_BACKUP` as described in the [operations guide](docs/operations.md#first-start). The app defaults to UID/GID `1000:1000`; source files must be readable and source directories traversable by that identity. Bind-mounted data and backup directories must be writable by it. `MUSICLIB_UID` and `MUSICLIB_GID` also set image build arguments: changing them requires a rebuild and correct ownership of existing storage. A fresh named volume inherits ownership from the image; an existing volume is not automatically re-owned.

## Storage, safety and maintenance

| Container path | Purpose |
|---|---|
| `/import` | Your external source collection, mounted read-only |
| `/data/originals` | Immutable, content-addressed copies of imported files |
| `/data/library` | Generated album folders and tagged files |
| `/data/work` | Temporary staging for imports and renders |
| `/backup` | Backup destination outside `/data` |

PostgreSQL holds the catalog, queue and publication journal. A render prepares a complete album in staging, writes its managed tags, verifies the audio and publishes it through a recoverable directory replacement protocol. Originals remain available for future renders. The storage contract and recovery rules are detailed in [DESIGN.md](DESIGN.md).

Budget roughly **twice the imported media bytes** for originals plus library, with additional room for staging, covers, attachments and retired output. The external source collection and separate backups add to this total. MusicLib reserves space conservatively, but external writes and a full disk can still interrupt a job.

The default backup volume is separate from the data volume, but it is not an off-device backup. Prefer a backup directory on another physical disk, retain several complete generations, and periodically restore into fresh destinations. Backup includes the catalog and originals; generated output is rebuilt after restore, and external import sources are not included.

From the repository root, with PostgreSQL running, the maintenance wrappers stop the app before taking its exclusive lock:

```sh
scripts/doctor.sh --deep
scripts/backup.sh 'before-update'
```

Read [operations](docs/operations.md) before running rebuild or restore. Restore requires a new empty database and data volume; it never overwrites an installation. Back up before updates. Migrations are forward-only: an older binary cannot safely use a newer schema, and a downgrade requires a compatible backup restore. Do not delete lock or maintenance markers to bypass a refusal.

## Access and API

The default published address is `127.0.0.1:8080`. `PUBLIC_ORIGIN` must match the exact host and port used by the browser: `localhost` and `127.0.0.1` are different hosts. LAN access needs an explicit binding and matching origin.

**There is no authentication.** Host/Origin validation and the `X-Musiclib-Request: 1` mutation header protect the browser boundary; that header is not a credential. Do not expose MusicLib directly to the Internet. Remote access requires an authenticated reverse proxy; consult the [security and troubleshooting guidance](docs/operations.md#security-and-troubleshooting).

The HTTP API supports catalog editing, import reports, queue management and downloads by entity ID. Changes to existing albums and artists use `ETag`/`If-Match` to detect conflicting edits. File uploads use raw request bodies. See the [API examples and request rules](docs/docker.md#the-api) and [import/queue examples](docs/docker.md#importing-the-queue-and-the-library-list-round-16). A complete OpenAPI document is future work tracked in [opensource.md](opensource.md).

## Architecture and further reading

`musiclibd` is a Go HTTP server with embedded HTML, CSS, JavaScript and fonts, backed by PostgreSQL 17. A small C++ helper uses TagLib for media tags, and FFmpeg/ffprobe run as external processes. The runtime container runs without root, has a read-only root filesystem and exposes readiness/liveness checks. The UI needs no Node runtime or frontend build.

| Guide | Contents |
|---|---|
| [Operations](docs/operations.md) | Configuration, storage, upgrades, maintenance, restore, troubleshooting and native release checks |
| [Docker and development](docs/docker.md) | Services, variables, pinned dependencies, test workflow and API examples |
| [Browser interface](docs/ui.md) | Import, editing, activity, limits and current UI behavior |
| [Design](DESIGN.md) | Domain model, storage invariants and recovery protocol (Italian) |
| [Progress](PROGRESS.md) / [decisions](NOTES.md) | Existing implementation evidence and unresolved questions |
| [Contributing](CONTRIBUTING.md) | Contributor setup and change validation |
| [Public release checklist](opensource.md) | License, packaging, documentation and acceptance work (Italian) |

## License

MusicLib's original code and documentation are licensed under the [MIT License](LICENSE), copyright 2026 tommasonovelli. Third-party dependencies and assets retain their own licenses, including the [SIL Open Font License](web/OFL.txt) for Hanken Grotesk. The audit and distribution requirements for bundled third-party software remain tracked in [opensource.md](opensource.md#p0--licenza-provenienza-e-distribuzione). The logo, the sun symbol, is the author's artwork and is not covered by the MIT License: see [LOGO.md](LOGO.md).
