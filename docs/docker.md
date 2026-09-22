# Docker: build, test and run

Everything in this repository is built, tested and run in Docker. The host needs
only **Docker Engine (or Docker Desktop) with the Compose v2 plugin**. No Go,
gcc, PostgreSQL or ffmpeg on the host.

| File | Role |
|---|---|
| `Dockerfile` | multi-stage: `build-ffmpeg`, `build-lame` (pinned source builds) → `toolchain` → `deps` → `test` / `build-app` → `runtime` |
| `compose.yaml` | `postgres` (always), `app` (profile `app`), `test`, `dev` and `postgres-test` (profile `tools`) |
| `docker/with-testdata.sh` | in-container: puts `TMPDIR` on the ext4 test volume, refuses other filesystems |
| `docker/gate.sh` | in-container: build, vet, gofmt, `go test -race` |
| `scripts/check.sh` | the full gate: `sqlc diff`, then `docker/gate.sh` with `postgres-test` |
| `scripts/sqlc.sh` | regenerates `internal/store` from `sql/` and `migrations/` |
| `scripts/fuzz.sh` | one fuzz target on the live sources |
| `scripts/dev.sh` | shell or single command in the toolchain container |
| `.gitattributes` | forces LF line endings on every checkout: the scripts run in Linux containers |
| `scripts/lint-shell.sh` | shellcheck on every script |

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

The `test` container has no internet (its only network is the internal
`testdb` one, shared with `postgres-test`), a read-only root filesystem, no
capabilities and runs as your uid (never root: root bypasses permission checks,
so permission tests would pass for the wrong reason). Modules come from the image
layer, downloaded and verified against `go.sum` when `go.mod`/`go.sum` change.
It tests exactly the tree it was built from, even while you keep editing.

`dev` and `fuzz.sh` bind-mount the live sources instead, with network access
for `go get`. `dev` is also on `testdb`, and `dev.sh` starts `postgres-test`,
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

If a volume was created by a different uid and is no longer writable, the
script says so. Remove the volume and it is recreated on the next run:

```sh
docker volume rm musiclib_testdata musiclib_go-build-cache musiclib_go-mod-cache
```

## Services and profiles

```sh
docker compose up -d                # postgres only
docker compose ps                   # waits for "(healthy)"
docker compose exec postgres psql -U musiclib
docker compose down                 # keeps the volumes; add -v to delete them
```

**postgres**: PostgreSQL 17, volume `musiclib_pgdata`, healthcheck with
`pg_isready` over TCP. TCP on purpose: the temporary init-time server listens
only on the socket and must not count as healthy. `fsync`, `full_page_writes`
and `synchronous_commit` are set to `on` explicitly (§11.1). initdb runs with
the image defaults. **No port is published** (§10.4). The app reaches it on the Compose network.
`POSTGRES_PASSWORD` defaults to `musiclib`: set your own in `.env` before the
first `up`. It is read only when the volume is initialized.

**postgres-test** (profile `tools`): the PostgreSQL of the tests (§12.1,
NOTES.md N-024). Same image, digest and settings as `postgres`, data on tmpfs,
no published port, only on the internal `testdb` network. `check.sh` and
`dev.sh` start it and wait for it to be healthy; it then keeps running.
`docker compose --profile tools stop postgres-test` discards all its data.

The tests get a database from `internal/store/pgtest`, the single helper that
knows where PostgreSQL comes from:

- `MUSICLIB_TEST_DATABASE_URL` (set in `test` and `dev`) points at the
  server; each test creates its own database and drops it `WITH (FORCE)`
  when it ends.
- Without the variable, as in a plain `go test` on a host, the database tests
  skip with a message.
- `MUSICLIB_REQUIRE_DB=1`, set only in `test`, turns that skip into a failure:
  the gate can never pass without running them.

**app** (profile `app`): the server, `musiclibd`. The profile keeps plain
`docker compose up` / `build` (the database during development) and the
`tools` services from building or starting it by accident. Deploy:

```sh
mkdir -p import                                   # or set MUSICLIB_IMPORT; Compose does not create it
docker compose --profile app up -d --build --wait # returns when app is healthy
curl -s http://127.0.0.1:8080/health/ready        # {"status":"ready"}
docker compose logs -f app                        # JSON lines
docker compose stop app && docker compose run --rm app doctor --deep && docker compose start app   # §11.3, Phase 6
```

It follows §11.1. The environment is `DATABASE_URL`, `PUBLIC_ORIGIN`,
`HTTP_ADDR=:8080` and `WORKERS`. Other settings:
- `/data` is the named volume `musiclib_musiclib-data`, or an ext4 host path
  through `MUSICLIB_DATA`.
- `/import` is a read-only bind of `MUSICLIB_IMPORT` (default `./import`). It
  must exist; Compose does not create it.
- `init: true`, `restart: unless-stopped`, 45 s stop grace.
- Runs as `MUSICLIB_UID:MUSICLIB_GID` (default 1000:1000), with no
  capabilities, a read-only root filesystem and tmpfs `/tmp`. `musiclibd`
  refuses to run as root and sets umask 022 itself.
- Published on `127.0.0.1:8080`. For LAN access, set `MUSICLIB_BIND` and a
  matching `PUBLIC_ORIGIN`.
- Healthcheck: `musiclibd healthcheck` (the image has no curl). It queries
  `/health/ready` on `HTTP_ADDR` and exits 0 or 1; it takes no lock.
  `start_period` is 120 s, polled every second.

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
   database. Repeat the rebuild or restore it names (Phase 6).
4. `database not reachable yet` (retried with backoff, 250 ms to 5 s) until
   `database reachable`; then the migrations.
5. `volume identified`: `/data/.musiclib-store` and `settings.store_id` must
   name the same store. Only a new, empty volume with a new database (no
   catalog content) is initialized; the first boot creates both.
6. `originals/`, `library/`, `work/` created if missing; boot checks: same
   filesystem and same mount for all of `/data`, read/write permissions, a
   real `RENAME_EXCHANGE` in `work/`; `media tools verified`: ffmpeg and
   ffprobe present at the pinned version; `/import` readable.
7. Leftovers of an interrupted run removed from `work/`; `ready`.

`/health/ready` then also checks PostgreSQL on every request (2 s timeout):
if the database goes away it answers 503 `db_unavailable` and the container
turns unhealthy, while the process keeps running.

SIGTERM (`docker compose stop`) stops the HTTP server, closes the database
pool and releases the volume lock last (`http server stopped`,
`database pool closed`, `volume lock released`, `stopped`; exit 0).

### When the app refuses to start

Every refusal is a log line at level `ERROR` with a stable `code`, and the
process exits 1; `restart: unless-stopped` retries, so the same line repeats
until the cause is fixed. Nothing is ever repaired or rewritten automatically.

| `code` | Meaning | What to do |
|---|---|---|
| `config_invalid` (exit 2) | an environment variable is missing or invalid; the message lists all of them | fix `.env` / `compose.yaml` |
| `run_as_root` (exit 2) | uid 0 | set `MUSICLIB_UID`/`MUSICLIB_GID` |
| `volume_locked` | another process holds `/data/.lock` | stop the other instance or maintenance command |
| `volume_maintenance_pending` / `_malformed` | a rebuild or restore did not finish | repeat it until it completes |
| `volume_store_mismatch` | the volume belongs to another database | mount the right volume, or point `DATABASE_URL` at the right database |
| `volume_db_uninitialized` | the volume is initialized, the database is new or reset | restore the database from the backup (§11.4) |
| `volume_marker_missing` / `volume_not_empty` | `/data/.musiclib-store` is missing, and either the media storage is not empty or the database already has catalog content | check the `/data` mount (`MUSICLIB_DATA`): the marker is completed automatically only on an empty volume with a database that has no catalog yet |
| `volume_marker_malformed` | `/data/.musiclib-store` is not in the expected format | inspect it; it is never rewritten |
| `volume_cross_device` / `volume_nested_mount` | a mount inside `/data` | mount one ext4 filesystem on `/data`, nothing below it |
| `volume_permission` | `/data` or a media directory is not writable by `MUSICLIB_UID`, or read-only | `chown -R` the host path, or recreate the volume |
| `volume_rename_exchange_unsupported` | the filesystem lacks `renameat2(RENAME_EXCHANGE)` | use ext4 (§3.1) |
| `import_unavailable` | `/import` is missing or not readable | check `MUSICLIB_IMPORT` |
| `media_tool_unavailable` / `media_tool_version` | `/usr/local/bin/ffmpeg` or `ffprobe` is missing, broken, or not the pinned version | rebuild the image from this repository (`docker compose --profile app build app`); never replace the binaries by hand |
| `store_migrate` / `store_schema_too_new` | migrations failed, or the database is newer than the binary | see the message; never downgrade |

The files at the top of `/data`:

```text
/data/.lock              flock target; empty, never removed
/data/.musiclib-store    store_id=<uuid>\n   (mode 0444, written once)
/data/.maintenance       operation=<rebuild|restore>\nstore_id=<uuid>\n   (only during Phase 6 maintenance)
```

### Variables

Set them in `.env` next to `compose.yaml`.

| Variable (`.env`) | Default | Meaning |
|---|---|---|
| `POSTGRES_PASSWORD` | `musiclib` | DB password (initdb time only) |
| `MUSICLIB_UID` / `MUSICLIB_GID` | `1000` | ids of the app process and owner of `/data` |
| `MUSICLIB_DATA` | `musiclib-data` | named volume or absolute ext4 path for `/data` |
| `MUSICLIB_IMPORT` | `./import` | host directory mounted read-only on `/import` |
| `MUSICLIB_BIND` / `MUSICLIB_PORT` | `127.0.0.1` / `8080` | published address |
| `PUBLIC_ORIGIN` | `http://127.0.0.1:${MUSICLIB_PORT}` | §10.4 |
| `WORKERS` | empty: `max(1, min(4, CPUs))` (§6.1) | worker pool size, 1..16 |
| `MUSICLIB_DEV_UID` / `MUSICLIB_DEV_GID` | your `id -u` / `id -g` | uid of `test`/`dev` (set by the scripts) |

## Pinned images

Every image is pinned by exact version **and** by the digest of its multi-arch
index (DESIGN.md §2.1). The digest is what is actually used; the tag documents it.

| Image | Where | Pin |
|---|---|---|
| Dockerfile frontend | `Dockerfile` line 1 | `docker/dockerfile:1.26.0@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32` |
| Go 1.25.14, Debian 13 | `Dockerfile` `GO_IMAGE` | `golang:1.25.14-trixie@sha256:2c4c60ef415fbfa5e90300722293bef36c5e63fae17570ce18f580af933dbd73` |
| runtime base, Debian 13 | `Dockerfile` `RUNTIME_IMAGE` | `debian:trixie-20260918-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a` |
| PostgreSQL 17.11 | `compose.yaml` (`postgres`, `postgres-test`) | `postgres:17.11-trixie@sha256:f4c66b820c6f974249089d3d16d86a3698eae11e8746eb6644b2271031e91232` |
| shellcheck 0.11.0 | `scripts/lint-shell.sh` | `koalaman/shellcheck:v0.11.0@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d` |
| sqlc 1.31.1 | `scripts/lib/common.sh` | `sqlc/sqlc:1.31.1@sha256:70f53171d27b2424e9358869975455a6e955a5aa8e58a998a270a6e34e525537` |

Toolchain and runtime share the same Debian release (13, glibc 2.41), so the
future TagLib helper is built and run against the same libraries.

### Pinned source builds

Native tools are built from release tarballs pinned by version **and** sha256
(`Dockerfile`, ARGs of the stage). The build fails if a download does not
match its hash. See NOTES.md N-073 for how each pin was verified.

| Tool | Stage | Pin | Goes into |
|---|---|---|---|
| FFmpeg 8.1.3 (ffmpeg, ffprobe) | `build-ffmpeg` | `ffmpeg-8.1.3.tar.gz` `bd458826a039b48a9606e794554c75eb4c4984b84173f7afa3128eae89336f2b` (OpenPGP signature checked) | `toolchain` (so `test`, `dev`) and `runtime`, as `/usr/local/bin/ffmpeg` and `/usr/local/bin/ffprobe` |
| nasm 2.16.03 | `build-ffmpeg` | `nasm-2.16.03.tar.gz` `5bc940dd8a4245686976a8f7e96ba9340a0915f2d5b88356874890e207bdb581` | nowhere: assembles FFmpeg's x86 code |
| LAME 3.100 | `build-lame` | `lame-3.100.tar.gz` `ddfe36cab873794038ae2c1210557ad34857a4b6bdc515785d1da9e175b1da1e` | `toolchain` only: MP3 test fixtures |

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
4. Run `scripts/check.sh`. For postgres, run `docker compose up -d postgres`
   and wait for healthy.
5. Commit the bump on its own, with the old and new version in the message.

Rules:
- A **PostgreSQL major** bump (17 → 18) is a dump and restore, not a tag change.
- The Go, TagLib and ffmpeg versions are inputs of `render_version` (§2.1), so
  bumping them changes `render_version`.
