# Docker: build, test and run

For production procedures, backups, restore and native-Linux release checks see [the operations guide](operations.md).

Everything in this repository is built, tested and run in Docker. The host needs
only **Docker Engine (or Docker Desktop) with the Compose v2 plugin**. No Go,
gcc, CMake, TagLib, PostgreSQL or ffmpeg on the host.

| File | Role |
|---|---|
| `Dockerfile` | multi-stage: `build-ffmpeg`, `build-lame`, `build-tags` (pinned source builds) → `toolchain` → `deps` → `test` / `build-app` → `runtime` |
| `compose.yaml` | production: `postgres` and `app` from the published image `ghcr.io/tommasonovelli/musiclib` ([operations](operations.md)) |
| `compose.dev.yaml` | development: `postgres` and `app` built from source, plus `test`, `dev` and `postgres-test` (profile `tools`) |
| `.env.example` | the settings of both files, to copy to `.env` |
| `docker/with-testdata.sh` | in-container: puts `TMPDIR` on the ext4 test volume, refuses other filesystems |
| `docker/gate.sh` | in-container: build, vet, gofmt, `go test -race` |
| `scripts/check.sh` | the full gate: `sqlc diff`, then `docker/gate.sh` with `postgres-test` |
| `scripts/sqlc.sh` | regenerates `internal/store` from `sql/` and `migrations/` |
| `scripts/fuzz.sh` | one fuzz target on the live sources |
| `scripts/dev.sh` | shell or single command in the toolchain container |
| `.gitattributes` | forces LF line endings on every checkout: the scripts run in Linux containers |
| `scripts/lint-shell.sh` | shellcheck on every script |
| `.github/workflows/release.yml` | on a tag `vX.Y.Z`: gate, image push to GHCR, GitHub Release ([Releasing](#releasing)) |

## Running the checks

```sh
scripts/check.sh                          # whole module
scripts/check.sh ./internal/names/...     # one package tree
scripts/fuzz.sh FuzzSegment 60s           # fuzz target in ./internal/names
scripts/fuzz.sh FuzzKey 10m ./internal/names
scripts/dev.sh                            # bash in the toolchain container
scripts/dev.sh go test -run TestLock -v ./internal/fsops/
scripts/lint-shell.sh
scripts/sqlc.sh                           # regenerate internal/store after editing sql/ or migrations/
```

The scripts always use `compose.dev.yaml` (`scripts/lib/common.sh`),
whatever the current directory or `COMPOSE_FILE`, and need no `.env`.

On a Windows host with Docker Desktop, run the scripts from Git Bash. They
set `MSYS_NO_PATHCONV=1` and pass Docker the native repository path, because
Git Bash would otherwise rewrite container paths such as `/src` into Windows
paths (NOTES.md N-059). The checkout must have LF line endings; `.gitattributes`
enforces that even with `core.autocrlf=true`.

`check.sh` first runs `sqlc diff` (the pinned sqlc image, no network, sources
read-only) and fails if the committed code in `internal/store` is out of date.
It then starts `postgres-test`, builds the `test` image, which contains a **snapshot of the working
tree** taken at build time, and runs `docker/gate.sh` in it:

```text
go build ./...  &&  go vet ./...  &&  test -z "$(gofmt -l .)"  &&  go test -race -count=1 ./...
```

`go test` runs with `-timeout` `GATE_TEST_TIMEOUT` per package (default `5m`; the release workflow sets `20m`).

The `test` container has no internet (its only network is the internal
`testdb` one, shared with `postgres-test`), a read-only root filesystem, no
capabilities and runs as your uid (never root: root bypasses permission checks,
so permission tests would pass for the wrong reason). Modules come from the image
layer, downloaded and verified against `go.sum` when `go.mod`/`go.sum` change.
It tests exactly the tree it was built from, even while you keep editing.

`dev` and `fuzz.sh` bind-mount the live sources instead, with network access
for `go get`. The test/dev image also contains **Chromium 154.0.8037.57-1~deb13u1**
(package SHA-256 checked in the Dockerfile), driven by the pinned Go
`chromedp v0.14.2` module for actual browser UI tests. The runtime image
contains neither Chromium nor Node. The browser tests use a local listener
inside the test container on an ephemeral localhost port, with the test
`PUBLIC_ORIGIN` adjusted to that port; they require no published database or app port.
The dev/test-only package and its dependency closure come from a fixed, signed Debian snapshot (NOTES.md N-212). UI usage is described in [docs/archive/ui.md](archive/ui.md).

`dev` is also on `testdb`, and `dev.sh` starts `postgres-test`,
so `scripts/dev.sh go test ./internal/store/...` runs the PostgreSQL tests. `fuzz.sh` needs that, so a failing input is written back to
`<package>/testdata/fuzz/<Target>/` in your tree and can be committed.

Caches persist across runs in named volumes: `musiclib_go-build-cache` (build,
test and fuzz cache, shared by `test` and `dev`) and `musiclib_go-mod-cache`
(module cache of `dev`, seeded from the image). A warm `scripts/check.sh
./internal/names/...` takes about 6 s.

## Where test data lives, and why

DESIGN.md §3.1 and §12.1 require the filesystem primitives (`openat2`,
`renameat2(RENAME_EXCHANGE)`, `fsync`, `flock`) to be tested on **real ext4**.
A container offers three kinds of storage, and only one of them qualifies:

| Storage | What it is | Used for tests? |
|---|---|---|
| container root | overlayfs | no: rename/exchange/whiteout semantics differ from ext4 |
| bind mount of the repo | host fs on native Engine; `fakeowner` file sharing on Docker Desktop | no |
| **named volume `musiclib_testdata`** | a directory on the Docker data disk | **yes: ext4** |

`docker/with-testdata.sh` creates a fresh `/testdata/run.XXXXXXXX` for each run,
exports it as `TMPDIR` (so every `t.TempDir()` lands there) and removes it
afterwards. It **fails** if `/testdata` is not a mount point or not ext4
(`statfs` magic `0xef53`). For an exploratory run elsewhere, set
`MUSICLIB_ALLOW_NON_EXT4=1`; it then only warns. It prints the filesystem at the
start of every run:

```text
with-testdata: TMPDIR=/testdata/run.lnkZ8Alq on /dev/vda1[/docker/volumes/musiclib_testdata/_data] ext4 (magic 0xef53), uid=1000 gid=1000
```

- **Native Docker Engine (production, DESIGN.md §2.1):** the volume is under
  `/var/lib/docker/volumes`, on the host's filesystem. That must be ext4.
- **Docker Desktop:** the volume is on the ext4 data disk of Docker Desktop's
  Linux VM (`/dev/vda1`). It is real ext4, but under the VM's kernel, not the
  host's.

The fsops tests also use `/dev/shm` (tmpfs, always present in containers) as
a "different filesystem" for the cross-device cases. No privileges, loop
devices or `mount` are needed. A loop-mounted ext4 image was rejected because it
needs `CAP_SYS_ADMIN` or `--privileged`.

### The really full filesystem (`/fullfs`)

The full-disk tests (DESIGN.md §12.2 "Disco pieno durante build",
NOTES.md N-143) need a filesystem that really fills up. The `test` and
`dev` services mount a **fixed-size tmpfs** at `/fullfs`
(`size=1088m`: the 1 GiB space margin of §11.2 plus room for the test
albums), set `MUSICLIB_FULLFS=/fullfs`, and the gate also sets
`MUSICLIB_REQUIRE_FULLFS=1`, so these tests fail instead of skipping there.

- The daemon mounts it; the test container gains no privilege.
- It is private to each container and disappears when the container exits:
  nothing persists, and no other run or volume shares it.
- Before it cleans or fills anything, `internal/faulttest.FullFS` requires
  `/fullfs` to be a mount point of at most 2 GiB that is not the filesystem
  of `TMPDIR`. It also serializes the test binaries with a `flock`. The
  shared ext4 `testdata` volume is never filled.
- tmpfs holds its data in RAM: while a full-disk test runs, up to about
  1.1 GiB of the Docker VM's memory is in use, then released.
- It is tmpfs, not ext4. No unprivileged container can mount an ext4
  image: Docker's `local` volume driver passes its options straight to
  mount(2), so an image file gives "block device required" and `o=loop` is
  rejected; a loop device needs a privileged container on the host. ext4's
  delayed-allocation ENOSPC at fsync is covered by injected failpoints.

Outside Docker (`MUSICLIB_FULLFS` unset) these tests skip.

If a volume was created by a different uid and is no longer writable, the
script says so. Remove the volume and it is recreated on the next run:

```sh
docker volume rm musiclib_testdata musiclib_go-build-cache musiclib_go-mod-cache
```

## Services and profiles

Two Compose files, both the project `musiclib` with the same volumes and
network (NOTES.md N-329):

- **`compose.yaml`**, production: `postgres` and `app`, the app from the
  published image `ghcr.io/tommasonovelli/musiclib:<version>`. Nothing is
  built. `docker compose up -d` starts both. `POSTGRES_PASSWORD` is
  required. Installation, upgrades and maintenance: [operations](operations.md).
- **`compose.dev.yaml`**, development: the same `postgres` and `app`, the app
  built from this repository's sources as `musiclib-app:local`
  (`MUSICLIB_VERSION=devel`), plus `test`, `dev` and `postgres-test` behind
  the profile `tools`. `POSTGRES_PASSWORD` may be unset here, so that the
  tools need no `.env`; PostgreSQL then refuses to initialize a new database.

Keep the `postgres` and `app` services of the two files in step: they differ
only in the app's `image`/`build` and in the password's default.

To run the app from source, use `compose.dev.yaml`, either with `-f` on every
command or once for all in `.env`:

```sh
cp .env.example .env                       # then set POSTGRES_PASSWORD (openssl rand -hex 32)
echo 'COMPOSE_FILE=compose.dev.yaml' >> .env
docker compose up -d --build --wait        # = docker compose -f compose.dev.yaml up -d --build --wait
```

With `COMPOSE_FILE` in `.env`, plain `docker compose` commands and the
maintenance scripts (`scripts/doctor.sh`, `backup.sh`, `rebuild.sh`,
`restore.sh`) act on the source build. Without it they act on
`compose.yaml` and its published image. The development scripts
(`check.sh`, `dev.sh`, `fuzz.sh`) always use `compose.dev.yaml`.

The database alone, during development:

```sh
docker compose -f compose.dev.yaml up -d --wait postgres
docker compose -f compose.dev.yaml exec postgres psql -U musiclib
docker compose -f compose.dev.yaml stop postgres
```

**These are the installation's volumes, not throwaway development ones.**
`compose.yaml` and `compose.dev.yaml` are the same Compose project
`musiclib`, with the same `postgres` and the same volumes `musiclib_pgdata`,
`musiclib_musiclib-data` (the originals) and `musiclib_musiclib-backup`. A
running app uses this same `postgres`, so stopping it takes the app's
database away too. Stop services with `stop`: `down` also removes the installation's `app` and
`postgres` containers, and `down -v` deletes the library's database,
originals and backups. The tests never use these volumes: they run on
`postgres-test`, below.

**postgres**: PostgreSQL 17, volume `musiclib_pgdata`, healthcheck with
`pg_isready` over TCP. TCP on purpose: the temporary init-time server listens
only on the socket and must not count as healthy. `fsync`, `full_page_writes`
and `synchronous_commit` are set to `on` explicitly (§11.1). initdb runs with
the image defaults. **No port is published** (§10.4). The app reaches it on the Compose network.
`POSTGRES_PASSWORD` has no default: set it in `.env` before the first `up`
(`openssl rand -hex 32` gives a URL-safe one, as `DATABASE_URL` needs). It
is read only when the volume is initialized.

**postgres-test** (`compose.dev.yaml`, profile `tools`): the PostgreSQL of
the tests (§12.1, NOTES.md N-024). Same image, digest and settings as
`postgres`, data on tmpfs, no published port, only on the internal `testdb`
network. `check.sh` and `dev.sh` start it and wait for it to be healthy; it
then keeps running. `docker compose -f compose.dev.yaml stop postgres-test`
discards all its data.

The tests get a database from `internal/store/pgtest`, the single helper that
knows where PostgreSQL comes from:

- `MUSICLIB_TEST_DATABASE_URL` (set in `test` and `dev`) points at the
  server; each test creates its own database and drops it `WITH (FORCE)`
  when it ends.
- Without the variable, as in a plain `go test` on a host, the database tests
  skip with a message.
- `MUSICLIB_REQUIRE_DB=1`, set only in `test`, turns that skip into a failure:
  the gate can never pass without running them.
- `pgtest.NewProxy` puts a TCP proxy between a test and that server, which
  loses a COMMIT's answer, cuts the connection before a COMMIT, or cuts every
  connection (§6.4, §12.2; NOTES.md N-108). It speaks the protocol without
  TLS, so the URL must keep `sslmode=disable`, as both services set it.

**app**: the server, `musiclibd`. From source, with `COMPOSE_FILE=compose.dev.yaml`
in `.env` as above (with the published image, the same commands without
`--build`):

```sh
mkdir -p import                                   # or set MUSICLIB_IMPORT; Compose does not create it
docker compose up -d --build --wait               # returns when app is healthy
curl -s http://127.0.0.1:8080/health/ready        # {"status":"ready"}
# Open http://127.0.0.1:8080/ in your browser (Library; Album editor,
# Import and Activity navigation). Use the exact PUBLIC_ORIGIN host.
docker compose logs -f app                        # JSON lines
docker compose stop app
# The app must already be stopped; never run maintenance alongside it.
docker compose run --rm --no-deps app doctor --deep
docker compose start app
```

It follows §11.1. The environment is `DATABASE_URL`, `PUBLIC_ORIGIN`,
`HTTP_ADDR=:8080` and `WORKERS`. Other settings:
- `/data` is the named volume `musiclib_musiclib-data`, or an ext4 host path
  through `MUSICLIB_DATA`.
- `/import` is a read-only bind of `MUSICLIB_IMPORT` (default `./import`). It
  must exist; Compose does not create it.
- `/backup` is `MUSICLIB_BACKUP` (default separate named volume); use an external disk for durable off-device backups.
- `init: true`, `restart: unless-stopped`, 45 s stop grace.
- The image runs as 1000:1000, the owner of `/data` and `/backup` in it, so
  a new named volume belongs to 1000:1000 (NOTES.md N-330). Compose runs it
  as `MUSICLIB_UID:MUSICLIB_GID` (default 1000:1000): another uid needs
  `MUSICLIB_DATA` and `MUSICLIB_BACKUP` as host directories owned by it.
  No capabilities, a read-only root filesystem and tmpfs `/tmp`. `musiclibd`
  refuses to run as root and sets umask 022 itself.
- Published on `127.0.0.1:8080`. For LAN access, set `MUSICLIB_BIND` and a
  matching `PUBLIC_ORIGIN`: the API answers only requests whose `Host`
  is the host and port of `PUBLIC_ORIGIN` (see "The API" below).
- Healthcheck: `musiclibd healthcheck` (the image has no curl). It queries
  `/health/ready` on `HTTP_ADDR` and exits 0 or 1; it takes no lock.
  `start_period` is 120 s, polled every second.

### Version

`musiclibd version` prints the application version and `render_version`
(§2.1) on two lines, `version: …` and `render_version: …`, and exits 0. It
reads no environment and needs neither the database nor the volumes. The
server also logs the version in its first event, `starting`. The version is
stamped at build time from the build argument `MUSICLIB_VERSION` (default
`devel`, a token of `[0-9A-Za-z.+-]`; the build fails on anything else, or
if the binary does not report it); it is also the backup manifest's
`app_version` (NOTES.md N-325). The `runtime` image carries the OCI labels
`org.opencontainers.image.{title,description,version,revision,source,licenses}`,
fed by `MUSICLIB_VERSION`, `MUSICLIB_REVISION` and `MUSICLIB_SOURCE`
(empty by default):

```sh
docker build --target runtime --build-arg MUSICLIB_VERSION=1.0.0 \
  --build-arg MUSICLIB_REVISION="$(git rev-parse HEAD)" \
  --build-arg MUSICLIB_SOURCE=https://github.com/OWNER/REPO -t musiclib-app:1.0.0 .
docker run --rm musiclib-app:1.0.0 version
docker image inspect musiclib-app:1.0.0 --format '{{json .Config.Labels}}'
```

The source build of `compose.dev.yaml` is `devel`; the published image carries
its release version. The installation's own:

```sh
docker compose run --rm --no-deps app version
```

### Offline inspection and rebuild (Phase 6)

Stop the app first, but leave PostgreSQL running. `doctor` is read-only
apart from taking `/data/.lock`; it never migrates the database, repairs
files or resolves a journal. Run the server once after an upgrade to apply
forward migrations before inspection. `--deep` streams hashes of originals
and receipt-listed output. Exit 0 means no errors (warnings and pending work
may be present), 1 means damage, 2 means refusal or invalid arguments.
Both maintenance commands refuse immediately when the server holds the lock.
`scripts/doctor.sh` and `scripts/rebuild.sh` perform the same stop, run and
restart steps (see [operations.md](operations.md)); the raw commands, on the
installation's Compose file (`compose.yaml`, or `COMPOSE_FILE`), are:

```sh
docker compose stop app
docker compose run --rm --no-deps app doctor --deep
# Inspect the findings before choosing whether to rebuild.
docker compose run --rm --no-deps --entrypoint cat app /data/.musiclib-store
# Copy the UUID printed after store_id=, without the prefix.
docker compose run --rm --no-deps app rebuild --store-id 'THE-UUID-PRINTED-ABOVE'
docker compose start app
```

Rebuild **deletes only `library/` and `work/`** and resets publication state,
reservations and render jobs in one transaction; it never fixes a damaged
original. It leaves the catalog and import reports intact and queues new
renders of active albums, not trashed ones. Only rebuild after confirming
the store id against the database and the marker. An interrupted rebuild
leaves `.maintenance`: do not delete it or start the server; rerun exactly
the same rebuild command until it succeeds. `backup` and `restore` use PostgreSQL 17.11 client tools in the runtime image.
See [operations.md](operations.md) for the complete offline procedure and
separate-volume restore. Do not rely on rebuild as a substitute for backup.

### What the app does at startup

`musiclibd` with no arguments runs the boot of DESIGN.md §11.1 and logs each
step (`docker compose logs app`):

1. `http listening`: `/health/live` answers 200 from here on,
   `/health/ready` answers 503 `not_ready` until the end of the boot.
2. `volume lock acquired`: exclusive `flock` on `/data/.lock`, held until the
   process exits. A second instance, or a maintenance command, is refused
   with `volume_locked`.
3. `/data/.maintenance` present: the boot stops with
   `volume_maintenance_pending` (or `_malformed`), before touching the
   database. Repeat the rebuild with the same store id if it names rebuild.
   Never remove a restore marker just to allow boot: recreate new empty
   destinations and repeat restore.
4. `database not reachable yet` (retried with backoff, 250 ms to 5 s) until
   `database reachable`; then the migrations.
5. `volume identified`: `/data/.musiclib-store` and `settings.store_id` must
   name the same store. Only a new, empty volume with a new database (no
   catalog content) is initialized; the first boot creates both.
6. `originals/`, `library/`, `work/` created if missing; boot checks: same
   filesystem and same mount for all of `/data`, read/write permissions, a
   real `RENAME_EXCHANGE` in `work/`; `media tools verified`: ffmpeg,
   ffprobe and the TagLib helper musiclib-tags present at the pinned
   versions (the line lists all four: `ffmpeg`, `ffprobe`, `musiclib_tags`,
   `taglib`); `/import` readable.
7. `journal recovered` (or `no pending publication`): a publication that a
   crash, a lost database or an error left half done is completed forward
   before anything else (§9.4). If the disk matches no legal step of it,
   publishing is **suspended** (`publishing suspended: ...`, code
   `publish_illegal_state`, with `album_id`, `build_id`, the paths and the
   `action` to take): the process stays up with the lock held, runs no
   later step and no worker, deletes nothing, and `/health/ready` answers
   503 `{"code": "publish_illegal_state", "message": ..., "details":
   {"album_id": ..., "build_id": ...}}`, so the Compose healthcheck reports
   the container unhealthy (NOTES.md N-135). See the next section.
8. `work cleaned`: leftovers of an interrupted run removed from `work/`
   (blob temporaries, probe directories, `work/import`, and every build in
   `work/render` and retired directory in `work/retired` that no journal
   references); `running jobs recovered`: jobs of the previous process back
   to pending.
9. `stale renders enqueued`: every active album whose published renderer is
   not this binary's `render_version`, and that has no job, gets a render. A
   failed render is left failed.
10. `workers started` (`WORKERS` of them: scans, imports, renders and
    publications), then `ready`.

`/health/ready` then also checks PostgreSQL on every request (2 s timeout)
and answers 503 `db_unavailable` while it is unreachable. The workers poll
the queue every 2 s: when the database is lost, or a commit's outcome is
unknown, or a publication fails after its journal was written, the workers
stop, their tool processes are killed, and the process **exits 1**
(`fatal failure, stopping the workers`, with the `code`). Docker restarts
it; the new process waits for PostgreSQL and recovers (§6.4).

SIGTERM (`docker compose stop`) stops the claims and cancels the builds
(their tools are killed), gives a publication already under way up to 30 s
to finish (otherwise its journal is completed at the next boot), then
closes the database pool and releases the volume lock last
(`http server stopped`, `workers stopped`, `database pool closed`,
`volume lock released`, `stopped`; exit 0). The 45 s stop grace covers it.

### The API

`/api` follows DESIGN.md §10 (`internal/http`; NOTES.md N-145 to N-151).
From the first moment of the boot it answers 503 `not_ready` until
`ready`. It also answers 503 while publishing is suspended
(`publish_illegal_state`) and during shutdown (`shutting_down`). The
§10.4 boundary, for any client:
- `Host` must be the host and port of `PUBLIC_ORIGIN` (421
  `host_not_allowed` otherwise). With the default
  `PUBLIC_ORIGIN=http://127.0.0.1:8080`, `curl http://127.0.0.1:8080/...`
  sends the right Host. `http://localhost:8080` does not. Neither does a
  LAN address the origin does not name.
- `Origin`, if sent, must be exactly `PUBLIC_ORIGIN` (403).
- Every request other than GET and HEAD needs `X-Musiclib-Request: 1`
  (403 `request_header_required`).
- A change of an existing album or artist needs `If-Match` with the
  `ETag` just read: 428 without it, 412 if someone changed it in
  between (reload and redo).
- JSON bodies: `Content-Type: application/json`, 16 MiB at most, no
  unknown or duplicate keys, every field present (`null` where allowed).
- No CORS header is ever sent. Every response has
  `X-Content-Type-Options: nosniff`.

`/health/live` and `/health/ready` are outside the Host check, so the
Compose healthcheck and probes by IP keep working.

Saving an album (`PUT /api/albums/<id>`, NOTES.md N-298) takes every key
of `{artist_id, new_artist, title, year, genre, compilation, tracks}`. The
album's artist is exactly one of `artist_id` (an existing artist) and
`new_artist` (the name of an artist to create with this save, in the same
transaction); the other is `null`. A save that fails (428, 412, 422, 409)
creates nothing; a `new_artist` that already exists is 409
`artist_exists` (or `artist_folder_conflict`) with the existing artist's
`artist_id` and both names in `details`. An artist left without any album,
trashed ones included, by a save that moves its last album elsewhere is
deleted by that save (N-297). `POST /api/artists` still creates an artist
without an album; it stays until an album arrives and leaves it (N-299).
Each track of `GET /api/albums/<id>` carries `duration_ms`, its duration
in milliseconds, or `null` while unknown (N-302): read-only, not a field of
the PUT body.

```sh
curl -s http://127.0.0.1:8080/api/artists
curl -si http://127.0.0.1:8080/api/albums/<id> | grep -i '^etag'     # "album:<id>:<revision>"
curl -s -X PUT http://127.0.0.1:8080/api/artists/<id>   -H 'X-Musiclib-Request: 1' -H 'Content-Type: application/json'   -H 'If-Match: "artist:<id>:<revision>"' -d '{"name":"Miles Dewey Davis"}'
curl -s -X POST http://127.0.0.1:8080/api/albums/<id>/render   -H 'X-Musiclib-Request: 1' -H 'If-Match: "album:<id>:<revision>"'
curl -s http://127.0.0.1:8080/api/albums/<id>/status
```

The album editor's files (round 14; NOTES.md N-172 to N-181). An upload's
body is the file itself, `Content-Type: application/octet-stream`; its
format is read from the content. Limits: cover 20 MiB (JPEG or PNG, 40
Mpixel, embeddable in every audio format of the album, N-091), attachment
256 MiB, LRC 2 MiB of UTF-8 (413 one byte over; 507
`insufficient_space` when `/data` has no room for it beyond the 1 GiB
margin). An attachment's `path` is a percent-encoded query parameter: a
`+` in it is read as a space, as in any query string, so a literal plus
is sent as `%2B` (`?path=Side%20A%2BB.pdf` is `Side A+B.pdf`).
Every change needs the album's `If-Match`. Downloads go by id, never by a
path:

```sh
H='-H X-Musiclib-Request:1 -H If-Match:"album:<id>:<revision>"'
curl -s -X PUT  $H -H 'Content-Type: application/octet-stream' --data-binary @front.jpg  http://127.0.0.1:8080/api/albums/<id>/cover
curl -s -X PUT  $H -H 'Content-Type: application/json' -d '{"attachment_id":"<attachment>"}' http://127.0.0.1:8080/api/albums/<id>/cover
curl -s -X DELETE $H http://127.0.0.1:8080/api/albums/<id>/cover
curl -s -X POST $H -H 'Content-Type: application/octet-stream' --data-binary @booklet.pdf 'http://127.0.0.1:8080/api/albums/<id>/attachments?path=Scans%2FBooklet.pdf'
curl -s -X DELETE $H http://127.0.0.1:8080/api/albums/<id>/attachments/<attachment>
curl -s -X PUT  $H -H 'Content-Type: application/octet-stream' --data-binary @01.lrc http://127.0.0.1:8080/api/albums/<id>/tracks/<track>/lyrics
curl -s -X DELETE $H http://127.0.0.1:8080/api/albums/<id>/tracks/<track>
curl -sOJ http://127.0.0.1:8080/api/albums/<id>/tracks/<track>/original     # also .../lyrics, .../cover, .../attachments/<attachment>/content
```

A database lost or a commit left without an answer during an API request
stops the process like one in a worker (exit 1, then Docker restarts it,
§6.4). The request got 503 `store_connection_lost` or
`store_commit_uncertain`: reload before retrying.

Verified on Docker Desktop with a separate project (`-p musiclib-e2e`,
`MUSICLIB_PORT=18080`, `PUBLIC_ORIGIN` defaulting to
`http://127.0.0.1:18080`), each answer as described above:
- `/health/ready` and `musiclibd healthcheck` answered as before (exit 0);
- `GET /api/artists` answered 200 with `nosniff` and `no-store`;
- `localhost:18080` was refused with 421;
- a POST without the header was refused with 403, and with it created
  the artist (201 and `Location`);
- `Origin: null` was refused with 403;
- a rename answered 428 without `If-Match`, 200 with it, and 412 on the
  now stale ETag.

### Importing, the queue and the library list (round 16)

Put the albums under the import mount, then drive the import through the
API (NOTES.md N-190 to N-201). The SQL path of Phase 2 (N-141) is no
longer needed.

```sh
B='http://127.0.0.1:8080/api'
M='-H X-Musiclib-Request:1 -H Content-Type:application/json'
curl -s "$B/import-source"                          # /import, sorted; symlinks listed, never followed
curl -s "$B/import-source?path=Jazz"                # a directory under /import (relative, no ..)
ID=$(cat /proc/sys/kernel/random/uuid)              # the request id: keep it to repeat the request
curl -s -X POST $M -d "{\"id\":\"$ID\",\"path\":\"Jazz\"}" "$B/imports"   # 201; the same again: 200; another path: 409
curl -s "$B/imports/$ID"                            # the report: scanning, importing, completed; each candidate
curl -s "$B/jobs?state=failed"                      # pending, running, failed jobs (state=, kind=, limit=, after=)
curl -s -X POST $M -d '{"artist":null,"title":"Kind of Blue"}' "$B/jobs/<job>/retry"   # a failed import, with §7.3 overrides
curl -s -X POST -H X-Musiclib-Request:1 "$B/jobs/<job>/retry"                          # any failed job, overrides kept
curl -s -X POST -H X-Musiclib-Request:1 "$B/jobs/<job>/dismiss"                        # a failed scan or import: no longer needs attention
curl -s -X POST -H X-Musiclib-Request:1 "$B/jobs/retry-failed"                          # every failed job that still needs attention
curl -s -X POST -H X-Musiclib-Request:1 "$B/render-all"
curl -s "$B/albums?q=miles&limit=50"                # search by title or artist; trash=true, artist=<id>, after=<next>
```

- `path` `""` imports the whole of `/import`. A batch with nothing to
  import completes with the scan failed as `no_valid_candidate`; files
  outside every album are the scan's `unassigned_file` warnings.
- A retry needs no `If-Match` (it changes no album); it is idempotent while
  the job is pending or running; a done or skipped job is 409.
- A failed scan or import stops needing attention once dismissed, or once
  a later import of its folder succeeds (NOTES.md N-285): the job shows
  `dismissed_at`, `superseded` and `needs_attention`, and retry-failed
  leaves it alone. A retry of it clears the dismissal.
- The import reports are kept 90 days, then deleted at boot or by the daily
  run of the server; batches with a job still to run are never deleted.
- At most two uploads (cover, attachment, LRC) copy at once; a third waits
  for its turn.
- The boot refuses an `/import` that is `/data` or one of its directories
  (`import_is_data`).

Verified end to end by `cmd/musiclibd.TestEndToEndImportThroughAPI` (real
server, two workers, real FLAC files).

### When the app refuses to start

Every refusal is a log line at level `ERROR` with a stable `code`, and the
process exits 1 (except `publish_illegal_state`, which keeps the process up
and unhealthy); `restart: unless-stopped` retries, so the same line repeats
until the cause is fixed. Nothing is ever repaired or rewritten automatically.

| `code` | Meaning | What to do |
|---|---|---|
| `config_invalid` (exit 2) | an environment variable is missing or invalid; the message lists all of them | fix `.env` / `compose.yaml` |
| `run_as_root` (exit 2) | uid 0 | set `MUSICLIB_UID`/`MUSICLIB_GID` |
| `volume_locked` | another process holds `/data/.lock` | stop the other instance or maintenance command |
| `volume_maintenance_pending` / `_malformed` | a rebuild or restore did not finish | repeat a rebuild using the same store id; do not manually clear a restore marker |
| `volume_store_mismatch` | the volume belongs to another database | mount the right volume, or point `DATABASE_URL` at the right database |
| `volume_db_uninitialized` | the volume is initialized, the database is new or reset | restore the database from the backup (§11.4) |
| `volume_marker_missing` / `volume_not_empty` | `/data/.musiclib-store` is missing, and either the media storage is not empty or the database already has catalog content | check the `/data` mount (`MUSICLIB_DATA`): the marker is completed automatically only on an empty volume with a database that has no catalog yet |
| `volume_marker_malformed` | `/data/.musiclib-store` is not in the expected format | inspect it; it is never rewritten |
| `volume_cross_device` / `volume_nested_mount` | a mount inside `/data` | mount one ext4 filesystem on `/data`, nothing below it |
| `volume_permission` | `/data` or a media directory is not writable by `MUSICLIB_UID`, or read-only | `chown -R` the host path, or recreate the volume |
| `volume_rename_exchange_unsupported` | the filesystem lacks `renameat2(RENAME_EXCHANGE)` | use ext4 (§3.1) |
| `import_unavailable` | `/import` is missing or not readable | check `MUSICLIB_IMPORT` |
| `import_is_data` | `/import` is the data volume or one of its directories (§7.1) | point `MUSICLIB_IMPORT` at the collection to import, never at the data volume |
| `media_tool_unavailable` / `media_tool_version` | `/usr/local/bin/ffmpeg`, `ffprobe` or `musiclib-tags` is missing, broken, or not the pinned version | pull the published image again (`docker compose pull app`), or rebuild it from source (`docker compose -f compose.dev.yaml build app`); never replace the binaries by hand |
| `store_migrate` / `store_schema_too_new` | migrations failed, or the database is newer than the binary | see the message; never downgrade |
| `publish_illegal_state` (the process **stays up**, unhealthy, no worker) | the pending publication journal does not match what is on disk (a directory moved or created by hand in `library/` or `work/`, a missing staging); nothing was deleted; `/health/ready` names the album and the build | put back what was moved and `docker compose restart app` (the recovery runs again), or stop the app and use the explicit rebuild command below |
| `publish_io` | a filesystem error (EIO, ENOSPC) while completing the pending publication | fix the disk or free space; the process exits 1, since it may be transient, and the next start retries |
| `store_connection_lost` / `store_commit_uncertain` (at run time, after `ready`) | the database was lost, or a commit's outcome is unknown (§6.4), in a worker (`fatal failure, stopping the workers`) or in an API request (`fatal failure in an API request, stopping`) | nothing: Docker restarts the app, which recovers; if it repeats, check PostgreSQL |

The files at the top of `/data`:

```text
/data/.lock              flock target; empty, never removed
/data/.musiclib-store    store_id=<uuid>\n   (mode 0444, written once)
/data/.maintenance       operation=<rebuild|restore>\nstore_id=<uuid>\n   (only during Phase 6 maintenance)
```

### Variables

Set them in `.env` next to `compose.yaml`; `.env.example` lists them.

| Variable (`.env`) | Default | Meaning |
|---|---|---|
| `POSTGRES_PASSWORD` | none: required by `compose.yaml`; empty in `compose.dev.yaml` | DB password, URL-safe (`openssl rand -hex 32`), read at initdb time only |
| `MUSICLIB_UID` / `MUSICLIB_GID` | `1000` | ids of the app process (`user:`); the image and new named volumes are 1000:1000, another uid needs host directories it owns |
| `MUSICLIB_DATA` | `musiclib-data` | named volume or absolute ext4 path for `/data` |
| `MUSICLIB_IMPORT` | `./import` | host directory mounted read-only on `/import` |
| `MUSICLIB_BACKUP` | `musiclib-backup` | external directory (prefer another ext4 disk) or named volume for `/backup` |
| `MUSICLIB_BIND` / `MUSICLIB_PORT` | `127.0.0.1` / `8080` | published address |
| `PUBLIC_ORIGIN` | `http://127.0.0.1:${MUSICLIB_PORT}` | §10.4: the only `Host` (and `Origin`) the API accepts |
| `WORKERS` | empty: `max(1, min(4, CPUs))` (§6.1) | worker pool size, 1..16 |
| `MUSICLIB_DEV_UID` / `MUSICLIB_DEV_GID` | your `id -u` / `id -g` | uid of `test`/`dev` (set by the scripts) |
| `COMPOSE_FILE` | `compose.yaml` | Compose's own: `compose.dev.yaml` for a source build (plain commands and maintenance scripts) |

## Pinned images

Every image is pinned by exact version **and** by the digest of its multi-arch
index (DESIGN.md §2.1). The digest is what is actually used; the tag documents it.
The one exception is the application's own image in `compose.yaml`, pinned by
its exact release version: its digest exists only once the release is
published (NOTES.md N-332).

| Image | Where | Pin |
|---|---|---|
| Dockerfile frontend | `Dockerfile` line 1 | `docker/dockerfile:1.26.0@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32` |
| Go 1.25.14, Debian 13 | `Dockerfile` `GO_IMAGE` | `golang:1.25.14-trixie@sha256:2c4c60ef415fbfa5e90300722293bef36c5e63fae17570ce18f580af933dbd73` |
| runtime base, Debian 13 | `Dockerfile` `RUNTIME_IMAGE` | `debian:trixie-20260918-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a` |
| PostgreSQL 17.11 | `compose.yaml` (`postgres`), `compose.dev.yaml` (`postgres`, `postgres-test`) | `postgres:17.11-trixie@sha256:f4c66b820c6f974249089d3d16d86a3698eae11e8746eb6644b2271031e91232` |
| Vibrance MusicLib (the app) | `compose.yaml` (`app`) | `ghcr.io/tommasonovelli/musiclib:1.0.0`: the release version, without a digest (NOTES.md N-332) |
| shellcheck 0.11.0 | `scripts/lint-shell.sh` | `koalaman/shellcheck:v0.11.0@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d` |
| sqlc 1.31.1 | `scripts/lib/common.sh` | `sqlc/sqlc:1.31.1@sha256:70f53171d27b2424e9358869975455a6e955a5aa8e58a998a270a6e34e525537` |
| BuildKit 0.32.2 | `.github/workflows/release.yml` (`BUILDKIT_IMAGE`) | `moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8` |
| SBOM scanner 1.12.0 | `.github/workflows/release.yml` (`SBOM_GENERATOR`) | `docker/buildkit-syft-scanner:1.12.0@sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9` |
| actionlint 1.7.12 | [Releasing](#releasing) (lint of the workflow) | `rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667` |

Toolchain and runtime share the same Debian release (13, glibc 2.41). The
native tools do not depend on it: ffmpeg, ffprobe and the TagLib helper are
fully static.

### Pinned source builds

Native tools are built from release tarballs pinned by version **and** sha256
(`Dockerfile`, ARGs of the stage). The build fails if a download does not
match its hash. See NOTES.md N-073 (FFmpeg, nasm, LAME) and N-083 (TagLib,
CMake) for how each pin was verified.

| Tool | Stage | Pin | Goes into |
|---|---|---|---|
| FFmpeg 8.1.3 (ffmpeg, ffprobe) | `build-ffmpeg` | `ffmpeg-8.1.3.tar.gz` `bd458826a039b48a9606e794554c75eb4c4984b84173f7afa3128eae89336f2b` (OpenPGP signature checked) | `toolchain` (so `test`, `dev`) and `runtime`, as `/usr/local/bin/ffmpeg` and `/usr/local/bin/ffprobe` |
| nasm 2.16.03 | `build-ffmpeg` | `nasm-2.16.03.tar.gz` `5bc940dd8a4245686976a8f7e96ba9340a0915f2d5b88356874890e207bdb581` | nowhere: assembles FFmpeg's x86 code |
| LAME 3.100 | `build-lame` | `lame-3.100.tar.gz` `ddfe36cab873794038ae2c1210557ad34857a4b6bdc515785d1da9e175b1da1e` | `toolchain` only: MP3 test fixtures |
| TagLib 2.3.2 | `build-tags` | `taglib-2.3.2.tar.gz` `3ca2d8afaa7f1cf7f6ed10e511ebc368bfacd6dcaa3dbfa690b89e502e8963dc` (no upstream signature; GitHub asset digest and Homebrew agree) | linked statically into `musiclib-tags` |
| CMake 4.4.3 (Kitware binary) | `build-tags` | `cmake-4.4.3-linux-x86_64.tar.gz` `d6c83076c575bc00b823522ac974bda66d0af05d6ddc30e739c12385cf32c6cc` (signed SHA-256 list checked) | nowhere: builds TagLib |

The TagLib helper `native/musiclib-tags` (DESIGN.md §2.1, §8.1) is built in
`build-tags` from this repository, against those two:

| Binary | Build | Goes into |
|---|---|---|
| `musiclib-tags` | release: `-O2`, fully static (libc and libstdc++ included), 3 MB | `toolchain` (so `test`, `dev`) and `runtime`, as `/usr/local/bin/musiclib-tags` |
| `musiclib-tags-asan` | ASan and UBSan, TagLib included; a finding exits 86 | `toolchain` only, as `/usr/local/bin/musiclib-tags-asan`: the hostile-input tests run every case on both |

The stage also runs `make check`, the unit tests of the helper's own parsers
under the sanitizers, and fails if they fail. Nothing of the helper is built
on the host; `native/musiclib-tags/build/` is ignored by git and Docker.

- **The ffmpeg binaries are fully static.** The images hold the same bytes,
  and the build is reproducible: a `--no-cache` rebuild gives the same
  sha256.
- **The version string** both tools report is `8.1.3-musiclib1`: the release
  plus `--extra-version`, the revision of the configure line. `musiclibd`
  refuses to boot with anything else (`media_tool_version`), and so does the
  test gate (`TestPinnedToolsInstalled`).
- **Bumping FFmpeg:**
  1. Download the new tarball and its `.asc`, check the signature against the
     FFmpeg release key, and compute the sha256.
  2. Update `FFMPEG_VERSION` and `FFMPEG_SHA256`.
  3. Reset `FFMPEG_EXTRA_VERSION` to `musiclib1`, or increase it when only
     the configure line changes.
  4. Update `media.PinnedVersion`, this table and NOTES.md.
  5. Run `scripts/check.sh`. `TestMP3EstimationWarningOfThePinnedTool` and
     the fixtures check the behaviours the adapter relies on.
- **The first build** of the `build-ffmpeg` stage takes a few minutes. It is
  cached afterwards, and shared by the `test`, `dev` and `app` images.
- **The TagLib helper is reproducible too:** the images hold the same
  `musiclib-tags`, and a `--no-cache` rebuild of `build-tags` gives the same
  sha256 for both binaries and for `libtag.a` (NOTES.md N-083). The stage
  takes about 80 s without cache; a change under `native/musiclib-tags/`
  rebuilds only the helper.
- **The helper's version** is two strings: `musiclib-tags version` prints
  `{"helper":"4","taglib":"2.3.2-musiclib1"}`. `helper` is
  `kHelperVersion` in `native/musiclib-tags/src/version.h`; `taglib` is the
  linked TagLib's own version plus `TAGLIB_BUILD_REVISION`, the revision of
  the cmake line. `musiclibd` refuses to boot with anything else
  (`media_tool_version`), and so does the gate (`TestPinnedToolsInstalled`).
- **Changing the helper** in a way that can change an inspection or a
  written file (the field table, a reading rule, the bytes written): bump
  `kHelperVersion` and `media.PinnedTagsVersion` together, then re-pin the
  binary's sha256 in `render.TestToolBinariesPinned` and the value in
  `TestVersionGolden` (every album renders again, NOTES.md N-130). The MP3
  reader and writer (`src/id3v2.cpp`, `src/ape.cpp`, `src/mp3.cpp`) and the
  M4A ones (`src/mp4.cpp`, `src/m4a.cpp`) are the helper's own; TagLib only
  cross-checks them (N-152, N-165). The re-pin, step by step:
  1. `docker build --target build-tags .` (runs the unit tests);
  2. `scripts/dev.sh sha256sum /usr/local/bin/musiclib-tags /usr/local/bin/musiclib-tags-asan`;
  3. the release sha256 into `pinnedBinaries` (`internal/render/version_test.go`),
     both into NOTES.md N-083;
  4. confirm with `docker build --no-cache --target build-tags .` that the
     bytes do not change, then `scripts/check.sh`.
- **Bumping TagLib:**
  1. Download the new release tarball, compute its sha256 and compare it
     with GitHub's asset digest and an independent pin (Homebrew's formula).
     TagLib does not sign its releases.
  2. Read the release's changes to `flac/flacfile.cpp`,
     `ogg/xiphcomment.cpp` and `flac/flacpicture.cpp`, and for MP3
     `mpeg/mpegfile.cpp`, `mpeg/id3v2/id3v2framefactory.cpp`,
     `mpeg/id3v2/id3v2frame.cpp`, `ape/apetag.cpp` and `tagutils.cpp`
     (`Utils::findID3v1`, `findAPE`). For M4A, `mp4/mp4atom.cpp`,
     `mp4/mp4tag.cpp`, `mp4/mp4itemfactory.cpp` and `mp4/mp4properties.cpp`
     (N-165). The helper's readers mirror where
     TagLib finds tags and what it drops or alters (NOTES.md N-085, N-152,
     N-154): a change there can require a change of a reader.
  3. Update `TAGLIB_VERSION` and `TAGLIB_SHA256`; reset
     `TAGLIB_BUILD_REVISION` to `musiclib1`, or increase it when only the
     cmake line changes.
  4. Update `media.PinnedTagLibVersion`, this table and NOTES.md N-083.
  5. Run `scripts/check.sh`: the hostile-input tests run on the new TagLib
     under the sanitizers too.
- **Bumping CMake:** download the tarball and `cmake-<v>-SHA-256.txt.asc`,
  check the signature against Kitware's release key, and update
  `CMAKE_VERSION` and `CMAKE_SHA256`. CMake does not reach any image, but
  it drives the TagLib build: check that the binaries are unchanged or bump
  `TAGLIB_BUILD_REVISION`.

### Bumping a pin

1. Pick the **exact** new tag (a patch version, or a dated tag for Debian).
   Never `latest`, and never a floating tag like `17` or `trixie-slim`.
2. Resolve the index digest from the registry:
   ```sh
   docker buildx imagetools inspect golang:1.25.15-trixie | awk '/^Digest:/{print $2}'
   ```
   To map a floating tag to its exact version, pull it and read the version
   variable, e.g.
   `docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' golang:1.25-trixie | grep GOLANG_VERSION`.
3. Update the tag and the digest together in the file from the table above, and
   in the table itself.
4. Run `scripts/check.sh`. For postgres, change both Compose files, then run
   `docker compose -f compose.dev.yaml up -d --wait postgres`.
5. Commit the bump on its own, with the old and new version in the message.

Rules:
- A **PostgreSQL major** bump (17 → 18) is a dump and restore, not a tag change.
- The Go, TagLib and ffmpeg versions are inputs of `render_version` (§2.1), so
  bumping them changes `render_version`.

## Releasing

`.github/workflows/release.yml` publishes a release when a tag `vX.Y.Z` is
pushed. It runs on GitHub-hosted `ubuntu-24.04` runners and builds for
linux/amd64 only. Its three jobs run in order, and each one stops the release
if it fails:

1. **guard**: the tag is `vMAJOR.MINOR.PATCH` (no leading zeros, no
   suffix), it still names the pushed commit, and that commit is on `main`.
   The `app` image of `compose.yaml` is exactly
   `ghcr.io/<owner>/musiclib:X.Y.Z`, `.env.example` exists, and
   `CHANGELOG.md` has exactly one non-empty `## [X.Y.Z]` section.
2. **publish**: first the gate, `scripts/check.sh` unchanged, with the
   Dockerfile's default uid 10001 and `GATE_TEST_TIMEOUT=20m`, in a Buildx
   builder with the pinned BuildKit. Then it logs in to GHCR, refuses if
   `ghcr.io/<owner>/musiclib:X.Y.Z` already exists, and builds the `runtime`
   target in the same builder (reusing the FFmpeg, TagLib and toolchain
   layers of the gate) with `MUSICLIB_VERSION=X.Y.Z`, `MUSICLIB_REVISION`
   (the commit) and `MUSICLIB_SOURCE` (the repository URL). It pushes
   `:X.Y.Z`, plus `:X.Y` and `:X` when this release is the newest of its
   series, and `:latest` when it is the newest of all (by `sort -V` of the
   `vX.Y.Z` tags, fetched by this job): a patch of an
   older series, or a re-run of an older release, never moves them back.
   `compose.yaml` keeps the explicit version. The image carries an SBOM and a
   `mode=max` provenance attestation.
3. **release**: pulls the image by digest, and checks that `version` prints
   `version: X.Y.Z` and that the revision label is the commit. It then creates
   the GitHub Release «Vibrance MusicLib X.Y.Z». The release body is the
   `CHANGELOG.md` section, followed by the image with its digest and an
   install snippet. Its assets are `compose.yaml` and `env.example` (the
   repository's `.env.example`: GitHub renames asset names that start with a
   dot). The release is marked **Latest** on GitHub with the same rule as
   `:latest`, checked again on the tags this job fetched: only when it is the
   newest version (`--latest=false` otherwise).

Release one version at a time. All runs share one concurrency group, so a
run waits for the one before it and the floating tags and the Latest release
only move forward. GitHub keeps only one *pending* run per group: pushing a
third tag while one run is in progress and another is waiting cancels the
waiting one. That is safe, nothing of it was published: when the others have
finished, open the cancelled run and use **Re-run all jobs**.

To release, on `main` with a clean tree and after the
[release check on a native host](operations.md#release-check-on-a-native-host):

```sh
# compose.yaml: the app's image line becomes ghcr.io/tommasonovelli/musiclib:X.Y.Z
# CHANGELOG.md: a new section "## [X.Y.Z] - YYYY-MM-DD" above the previous one
git add compose.yaml CHANGELOG.md
git commit -m "Release X.Y.Z"
git push origin main
git tag -a vX.Y.Z -m "Vibrance MusicLib X.Y.Z"
git push origin vX.Y.Z
```

Push the tag on its own: GitHub starts no workflow for tags pushed more than
three at a time. Do not create the GitHub Release by hand: the release job
creates it, and fails, after the image is published, if the tag already has
one (delete that release and re-run the job).

The `CHANGELOG.md` section of a version is its heading, `## [X.Y.Z]`,
optionally followed by ` - YYYY-MM-DD`. The section runs up to the next `## `
heading or the first link reference definition (`[X.Y.Z]: https://…`), so
keep those definitions at the end of the file. The release body is the
section's lines without the heading and without leading or trailing blank
lines.

**First release only**, once the workflow has run:

- A new GHCR package is private. On GitHub, open the account's
  **Packages → musiclib → Package settings → Change visibility**, and make it
  **Public**. Until then nobody else can pull the image, although the
  release is already visible.
- In the same settings, check that the package is connected to the
  repository (the package page shows it; otherwise use **Connect
  repository**). Also check that **Manage Actions access** gives the
  repository the **Write** role: every later release pushes with the
  repository's `GITHUB_TOKEN`. If a package named `musiclib` already existed
  without that access, the first push fails with 403: grant it and run the
  workflow again.

**When a run fails:**

- In **guard**, or in **publish** before its push (the gate included):
  nothing is published. Fix the problem on `main`, move the tag to the new
  commit (`git tag -d vX.Y.Z`, `git push origin :refs/tags/vX.Y.Z`, tag and
  push again).
- In **release**, the image is already published. Use **Re-run failed jobs**:
  the release job runs again on the same digest, and decides **Latest** again
  from the tags it fetches then. A job that pushed is never re-run to push
  again: the version check refuses it. If the image itself is wrong, do not
  publish it under the same version. Release a new patch version instead.
- In **publish** after the push succeeded: a re-run is refused by the version
  check, so check the image (below) and create the release by hand, with the
  `CHANGELOG.md` section in `notes.md` followed by the line
  ``- Digest: `sha256:…` `` (from `imagetools inspect`), and
  `cp .env.example env.example`:
  `gh release create vX.Y.Z --verify-tag --title "Vibrance MusicLib X.Y.Z" --notes-file notes.md compose.yaml env.example`
  (add `--latest=false` if it is not the newest version). If the push stopped
  after `:X.Y.Z`, move each floating tag it should have moved by hand, only
  when this is the newest version of that series (`:latest`: of all), e.g.
  `docker buildx imagetools create -t ghcr.io/tommasonovelli/musiclib:latest ghcr.io/tommasonovelli/musiclib:X.Y.Z`.
  Or release a new patch version.

Check a published release (the digest must be the one in the release notes):

```sh
docker buildx imagetools inspect ghcr.io/tommasonovelli/musiclib:X.Y.Z
docker buildx imagetools inspect ghcr.io/tommasonovelli/musiclib:X.Y.Z --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/tommasonovelli/musiclib:X.Y.Z --format '{{ json .Provenance }}'
docker run --rm ghcr.io/tommasonovelli/musiclib:X.Y.Z version
```

Changing the workflow: every third-party action is pinned by commit SHA,
with its version in a comment. To bump one, resolve the tag with
`gh api repos/OWNER/ACTION/git/ref/tags/vX.Y.Z` (for an annotated tag,
follow its object with `gh api repos/OWNER/ACTION/git/tags/SHA`). The
BuildKit and SBOM scanner images are pinned like every other image
([Pinned images](#pinned-images)). Then lint:

```sh
docker run --rm -v "$PWD:/repo:ro" -w /repo \
  rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667 -color
```

On Git Bash, prefix it with `MSYS_NO_PATHCONV=1` and use `$(pwd -W)`.
actionlint also runs shellcheck on every `run:` script.
