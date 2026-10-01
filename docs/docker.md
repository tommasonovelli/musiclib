# Developing Vibrance MusicLib: build, test and release

This guide is for developers: building MusicLib from source, the test gate, the storage the tests need, the Compose services, the pinned versions and the release procedure. To install, run, back up or upgrade MusicLib, read the [operations guide](operations.md); the project's rules are in [AGENTS.md](../AGENTS.md) and [CONTRIBUTING.md](../CONTRIBUTING.md).

## Contents

- [What you need](#what-you-need)
- [The files](#the-files)
- [The developer scripts](#the-developer-scripts)
- [Running the app from source](#running-the-app-from-source)
- [Services and profiles](#services-and-profiles)
- [Where test data lives, and why](#where-test-data-lives-and-why)
- [Version and image labels](#version-and-image-labels)
- [Pinned images](#pinned-images)
- [Releasing](#releasing)

## What you need

Everything is built, tested and run in Docker. The host needs only **Docker Engine (or Docker Desktop) with the Compose v2 plugin**: no Go, gcc, CMake, TagLib, PostgreSQL or FFmpeg.

- **On Windows**, with Docker Desktop, run the scripts from **Git Bash**. They set `MSYS_NO_PATHCONV=1` and pass Docker the native repository path, because Git Bash would otherwise rewrite container paths such as `/src` into Windows paths.
- **Line endings.** The checkout must have LF line endings: the scripts run in Linux containers, which cannot run a script with CRLF endings, and the gate's `gofmt` check fails on a Go source that has them. `.gitattributes` enforces LF on every checkout, even with `core.autocrlf=true`.
- **Docker Desktop is fine for development**, but a release also needs the [release check on a native host](#release-check-on-a-native-host): Docker Desktop's ext4 is real, but under its VM's kernel, not the host's.

## The files

| File | Role |
|---|---|
| `Dockerfile` | multi-stage: `build-ffmpeg`, `build-lame`, `build-tags` (pinned source builds) → `toolchain` → `deps` → `test` / `build-app` → `runtime` |
| `compose.yaml` | production: `postgres` and `app` from the published image `ghcr.io/tommasonovelli/musiclib` ([operations guide](operations.md)) |
| `compose.dev.yaml` | development: `postgres` and `app` built from source, plus `test`, `dev` and `postgres-test` (profile `tools`) |
| `.env.example` | the settings of both files, to copy to `.env` |
| `.gitattributes` | forces LF line endings on every checkout |
| `scripts/check.sh` | the full gate: `sqlc diff`, then `docker/gate.sh` in the `test` container with `postgres-test` |
| `scripts/dev.sh` | a shell, or one command, in the toolchain container on the live sources |
| `scripts/sqlc.sh` | regenerates `internal/store` from `sql/` and `migrations/` |
| `scripts/fuzz.sh` | one fuzz target on the live sources |
| `scripts/lint-shell.sh` | shellcheck on every shell script |
| `scripts/lib/common.sh` | shared helpers of the scripts (sourced, never run) |
| `scripts/doctor.sh`, `rebuild.sh`, `backup.sh`, `restore.sh` | the maintenance wrappers for an installation: see [The maintenance scripts](operations.md#the-maintenance-scripts) (`scripts/lib/maintenance.sh` is their shared part) |
| `docker/gate.sh` | in-container: build, vet, gofmt, `go test -race` |
| `docker/with-testdata.sh` | in-container: puts `TMPDIR` on the ext4 test volume, refuses other filesystems |
| `.github/workflows/release.yml` | on a tag `vX.Y.Z`: gate, image push to GHCR, GitHub Release ([Releasing](#releasing)) |

## The developer scripts

Run every script from anywhere in the checkout: they find the repository root themselves, and need no `.env`. `check.sh`, `dev.sh` and `fuzz.sh` always use `compose.dev.yaml`, whatever the current folder or `COMPOSE_FILE`; `sqlc.sh` and `lint-shell.sh` run their pinned image directly with `docker run`.

Before doing anything, every script checks that `docker` is on the `PATH`, that `docker compose` works and that the Docker daemon answers; otherwise it stops with a message such as `check.sh: error: cannot reach the Docker daemon (is it running? is your user allowed to use it?)` and exits 1. Their own messages start with the script's name (`check.sh: …`); errors with `…: error: …`.

The toolchain containers run as your uid and gid, so that files they write into the sources belong to you. Root is never used as the test user: root bypasses permission checks, so permission tests would pass for the wrong reason. If you run a script as root, the containers use uid 10001 instead.

### scripts/check.sh: the gate

```text
scripts/check.sh [package-pattern...]
```

The full quality gate. Run it before every commit of code; it is what the release workflow runs too.

```sh
scripts/check.sh                          # the whole module
scripts/check.sh ./internal/names/...     # one package tree
scripts/check.sh ./internal/names ./internal/fsops
```

- **Arguments**: package patterns, default `./...`. Each one must start with `./` and name a folder (`./internal/names` or `./internal/names/...`); `gofmt` checks the same folders.
- **What it does**, in order:
  1. `sqlc diff`, with the pinned sqlc image, without network, on read-only sources: it fails if the committed code in `internal/store` is out of date (`internal/store is out of date with sql/ or migrations/: run scripts/sqlc.sh`). This step always checks the whole store.
  2. Builds the `test` image, which contains a **snapshot of the working tree** taken now: it tests exactly that tree, even while you keep editing (`building the test image (uid 1000:1000)`).
  3. Starts `postgres-test` and waits until it is healthy (`starting postgres-test`); it keeps running afterwards.
  4. Runs `docker/gate.sh` in the `test` container, wrapped by `docker/with-testdata.sh`:

     ```text
     go build ./...  &&  go vet ./...  &&  test -z "$(gofmt -l .)"  &&  go test -race -count=1 ./...
     ```

- **Success**: the last line is `==> gate passed`, and the exit code is 0. A warm `scripts/check.sh ./internal/names/...` takes about 6 s; the whole module takes several minutes.
- **Failure**: the exit code is not 0, and the output shows the failing step (`==> go vet …`, a test's `FAIL` lines, or the list of unformatted files followed by `the files above are not gofmt-formatted (run: gofmt -w <file>)`).
- **Settings**: `GATE_TEST_TIMEOUT`, the `go test -timeout` per package (default `15m`, enough for the slowest package on a 4-CPU host; the release workflow sets `20m`), and `MUSICLIB_DEV_UID`/`MUSICLIB_DEV_GID` (below). Example: `GATE_TEST_TIMEOUT=30m scripts/check.sh`.

The `test` container has no internet (its only network is the internal `testdb`, shared with `postgres-test`), a read-only root filesystem, no capabilities, and runs as your uid. Go modules come from the image layer, downloaded and verified against `go.sum` when `go.mod` or `go.sum` change. It sets `MUSICLIB_REQUIRE_DB=1` and `MUSICLIB_REQUIRE_FULLFS=1`, so the PostgreSQL tests and the full-disk tests fail instead of skipping: the gate can never pass without running them. A host `go test`, which skips them, proves nothing.

### scripts/dev.sh: the toolchain on the live sources

```text
scripts/dev.sh                  # an interactive bash
scripts/dev.sh COMMAND [ARG...] # one command
```

A shell, or one command, in the `dev` container: the Go toolchain with the pinned FFmpeg and tag helper, the repository **bind-mounted** at `/src` (your live sources, not a snapshot), network access for `go get`, `TMPDIR` on the ext4 test volume and `postgres-test` reachable.

```sh
scripts/dev.sh go test -race -count=20 -run TestX ./internal/publish       # repeat a concurrency test
scripts/dev.sh go test ./internal/store/...                                # PostgreSQL tests run here too
scripts/dev.sh go test -run TestLock -v ./internal/fsops/
scripts/dev.sh go mod tidy
```

- **What it does**: builds the `dev` image (target `deps`) if needed, starts `postgres-test` and waits for it, then runs the command through `docker/with-testdata.sh`, which first prints the `with-testdata: TMPDIR=…` line.
- **Result**: the exit code is the command's; the container is removed afterwards. Files the command writes under `/src` appear in your checkout, owned by you.
- Unlike the gate, `dev` does not set `MUSICLIB_REQUIRE_DB` or `MUSICLIB_REQUIRE_FULLFS`; it does set the database URL and `/fullfs`, so those tests run.

**Browser tests.** The toolchain image also contains **Chromium 154.0.8037.57-1~deb13u1** (package SHA-256 checked in the Dockerfile, from a fixed, signed Debian snapshot), driven by the pinned Go module `chromedp v0.14.2` for real browser tests of the UI. The tests use a local listener on an ephemeral localhost port inside the container, with the test `PUBLIC_ORIGIN` set to that port; they need no published database or app port. The runtime image contains neither Chromium nor Node. To write review screenshots of the pages (both themes, laptop and phone widths) into the git-ignored `tmp/ui-shots/`:

```sh
scripts/dev.sh env MUSICLIB_UI_SHOTS=/src/tmp/ui-shots go test -count=1 -run 'TestBrowser(Library|Album|Queue)Screenshots' ./internal/http/
```

### scripts/sqlc.sh: regenerate the store

```text
scripts/sqlc.sh          # generate
scripts/sqlc.sh diff     # only report differences
```

Regenerates `internal/store` from `sql/*.sql` and `migrations/` with the pinned sqlc image (`sqlc.yaml`), as your uid, without network. Run it after every change in `sql/` or `migrations/`, review the generated diff, and commit the generated files together with their sources: `scripts/check.sh` fails while they are out of date.

- `generate` (the default) writes `internal/store`; `diff` mounts the sources read-only and prints what `generate` would change.
- Success: exit 0. With `diff`, differences are printed and the exit code is not 0. Any other argument prints `usage: scripts/sqlc.sh [generate|diff]` and exits 1.

### scripts/fuzz.sh: one fuzz target

```text
scripts/fuzz.sh <FuzzTarget> <duration> [package]
```

```sh
scripts/fuzz.sh FuzzSegment 60s                  # in ./internal/names, the default package
scripts/fuzz.sh FuzzKey 10m ./internal/names
scripts/fuzz.sh FuzzCheckJSON 1000x ./internal/http
```

- **Arguments**: the target, a name that starts with `Fuzz`; the duration, a Go duration (`30s`, `10m`) or a count (`1000x`); the package, one `./relative/folder` without `...` (default `./internal/names`), which must exist. The targets today are `FuzzSegment`, `FuzzKey`, `FuzzNormalizeText` and `FuzzSplitRelPath` in `./internal/names`, and `FuzzCheckJSON` in `./internal/http`.
- **What it does**: builds the `dev` image, then runs `go test <package> -run='^$' -fuzz='^<target>$' -fuzztime=<duration>` in the `dev` container, on the live sources.
- **Success**: `go test` ends with `ok` and exit 0.
- **Failure**: a failing input is written to `<package>/testdata/fuzz/<FuzzTarget>/` in your checkout, ready to be committed as a regression test, and the exit code is not 0. A wrong argument prints the usage and exits 1.
- The fuzzing cache lives in the `musiclib_go-build-cache` volume and survives across runs. `fuzz.sh` does not start `postgres-test`.

### scripts/lint-shell.sh: shellcheck

```text
scripts/lint-shell.sh
```

Runs the pinned shellcheck image, without network, on every `*.sh` file under `scripts/` and `docker/`, `scripts/lib/` included. Run it after changing any shell script. It prints `shellcheck N files`, then either shellcheck's findings (exit not 0) or `clean` (exit 0). It takes no argument.

### docker/gate.sh and docker/with-testdata.sh

These two run **inside** the containers; you do not run them on the host.

- `docker/gate.sh [package-pattern...]` is the gate itself, as described under `check.sh`. Run from the module root; it refuses a pattern that does not start with `./` or does not name a folder. It prints each step as `==> …` and ends with `==> gate passed`. It is the default command of the `test` image, so `scripts/check.sh` calls it for you.
- `docker/with-testdata.sh <command> [args...]` runs the command with `TMPDIR` on the ext4 test volume: see [Where test data lives](#where-test-data-lives-and-why). It is the entrypoint of the `test` and `dev` containers.

### Settings of the scripts

| Variable | Default | Used by | What it does |
|---|---|---|---|
| `MUSICLIB_DEV_UID`, `MUSICLIB_DEV_GID` | your `id -u` / `id -g` (10001 if you are root) | all scripts | the uid and gid of the `test` and `dev` containers; `0` is refused |
| `GATE_TEST_TIMEOUT` | `15m` | `check.sh` | `go test -timeout` per package |
| `MUSICLIB_ALLOW_NON_EXT4` | unset | `with-testdata.sh` | `1`: continue with a warning when `/testdata` is not ext4, for an exploratory run only |
| `MUSICLIB_UI_SHOTS` | unset | browser tests | a folder where the screenshot tests write their images |

## Running the app from source

To run MusicLib built from your checkout, follow [Running from source](operations.md#running-from-source) in the operations guide: copy `.env.example` to `.env`, set `POSTGRES_PASSWORD` and `MUSICLIB_PASSWORD`, then either add `COMPOSE_FILE=compose.dev.yaml` to `.env` or pass `-f compose.dev.yaml` to every command:

```sh
docker compose -f compose.dev.yaml up -d --build --wait     # returns when the app is healthy
```

- With `COMPOSE_FILE` in `.env`, plain `docker compose` commands and the maintenance scripts (`scripts/doctor.sh`, `backup.sh`, `rebuild.sh`, `restore.sh`) act on the source build; without it they act on `compose.yaml` and its published image. The development scripts ignore `COMPOSE_FILE`: `check.sh`, `dev.sh` and `fuzz.sh` always use `compose.dev.yaml`.
- The maintenance scripts act only on containers created in the clone's folder. Before stopping anything they compare the folder that Docker records in the project's `app` and `postgres` containers (label `com.docker.compose.project.working_dir`) with the checkout, as the same directory (`-ef`, so `C:\…` and `/c/…` forms in Git Bash, symlinks and trailing slashes compare correctly). A container created elsewhere (a second checkout or worktree that ran `docker compose up` under the same project name, or an installation in `~/musiclib`) makes them refuse with the commands to run by hand. Create the app and database of a source build from the checkout's root, as above.
- The app is built as the image `musiclib-app:local`, version `devel`. The tag is fixed: a second clone that builds from source retags the first clone's image.
- `import` must exist (`mkdir -p import`, or set `MUSICLIB_IMPORT`): Compose does not create it.

The database alone, during development:

```sh
docker compose -f compose.dev.yaml up -d --wait postgres
docker compose -f compose.dev.yaml exec postgres psql -U musiclib
docker compose -f compose.dev.yaml stop postgres
```

## Services and profiles

Two Compose files, both the project `musiclib` with the same volumes and network:

- **`compose.yaml`**, production: `postgres` and `app`, the app from the published image `ghcr.io/tommasonovelli/musiclib:<version>`. Nothing is built.
- **`compose.dev.yaml`**, development: the same `postgres` and `app`, the app built from this repository's sources as `musiclib-app:local` (`MUSICLIB_VERSION=devel`), plus `test`, `dev` and `postgres-test` behind the profile `tools`.

Both files load without a `.env`: the database password defaults to `musiclib`. Only the app needs one setting to start, `MUSICLIB_PASSWORD` (empty by default, so `docker compose` itself never fails without it). **Keep the `postgres` and `app` services of the two files in step**: they differ only in the app's `image`/`build`.

> **These are the installation's volumes, not throwaway development ones.** `compose.yaml` and `compose.dev.yaml` are the same Compose project `musiclib`, with the same `postgres` and the same volumes `musiclib_db`, `musiclib_data` (the originals) and `musiclib_backup`. A running app uses this same `postgres`, so stopping it takes the app's database away too. Stop services with `stop`: `down` also removes the installation's `app` and `postgres` containers, and **`down -v` deletes the library's database, originals and backups**. The tests never use these volumes: they run on `postgres-test`.

**postgres**: PostgreSQL 17, container `musiclib-db`, volume `musiclib_db`, healthcheck with `pg_isready` over TCP (on purpose: the temporary server of the first initialization listens only on the socket and must not count as healthy). `fsync`, `full_page_writes` and `synchronous_commit` are set to `on` explicitly; initdb runs with the image defaults. **No port is published**: the app reaches it on the Compose network. The password is defined once per file, in `x-db-password`: `POSTGRES_PASSWORD` from `.env`, or `musiclib` when it is unset or empty.

**postgres-test** (profile `tools`): the PostgreSQL of the tests. Same image, digest and settings as `postgres`, data on tmpfs, no published port, only on the internal `testdb` network. `check.sh` and `dev.sh` start it and wait for it to be healthy; it then keeps running. `docker compose -f compose.dev.yaml stop postgres-test` discards all its data.

The tests get a database from `internal/store/pgtest`, the single helper that knows where PostgreSQL comes from:

- `MUSICLIB_TEST_DATABASE_URL` (set in `test` and `dev`) points at `postgres-test`; each test creates its own database and drops it `WITH (FORCE)` when it ends.
- Without the variable, as in a plain `go test` on a host, the database tests skip with a message.
- `MUSICLIB_REQUIRE_DB=1`, set only in `test`, turns that skip into a failure.
- `pgtest.NewProxy` puts a TCP proxy between a test and that server, which loses a COMMIT's answer, cuts the connection before a COMMIT, or cuts every connection. It speaks the protocol without TLS, so the URL must keep `sslmode=disable`, as both services set it.

**test** (profile `tools`): the hermetic gate, described under [`scripts/check.sh`](#scriptschecksh-the-gate).

**dev** (profile `tools`): the interactive toolchain of [`scripts/dev.sh`](#scriptsdevsh-the-toolchain-on-the-live-sources) and [`scripts/fuzz.sh`](#scriptsfuzzsh-one-fuzz-target), on the default network (internet for `go get`) and on `testdb`.

**app**: `musiclibd`, container `musiclib`; its settings are described in the [operations guide](operations.md#configuration). The image runs as 1000:1000, the owner of `/data` and `/backup` in it; Compose runs it as `MUSICLIB_UID:MUSICLIB_GID`, with no capabilities, a read-only root filesystem and tmpfs `/tmp`; `init: true`, `restart: unless-stopped`, 45 s stop grace. Its healthcheck is `musiclibd healthcheck` (the image has no curl): it queries `/health/ready` on `HTTP_ADDR` and exits 0 or 1, takes no lock, `start_period` 120 s, polled every second.

Caches persist across runs in named volumes: `musiclib_go-build-cache` (build, test and fuzz cache, shared by `test` and `dev`) and `musiclib_go-mod-cache` (the module cache of `dev`, seeded from the image).

## Where test data lives, and why

The filesystem primitives (`openat2`, `renameat2(RENAME_EXCHANGE)`, `fsync`, `flock`) must be tested on **real ext4**, the only supported filesystem. A container offers three kinds of storage, and only one of them qualifies:

| Storage | What it is | Used for tests? |
|---|---|---|
| container root | overlayfs | no: rename/exchange/whiteout semantics differ from ext4 |
| bind mount of the repository | the host filesystem on native Engine; `fakeowner` file sharing on Docker Desktop | no |
| **named volume `musiclib_testdata`** | a folder on the Docker data disk | **yes: ext4** |

`docker/with-testdata.sh` creates a fresh `/testdata/run.XXXXXXXX` for each run, exports it as `TMPDIR` (so every `t.TempDir()` lands there) and removes it afterwards; it also removes leftovers of killed runs older than a day. It **fails** if `/testdata` is not a mount point, is not writable, or is not ext4 (`statfs` magic `0xef53`); with `MUSICLIB_ALLOW_NON_EXT4=1` it only warns about the filesystem. It prints the filesystem at the start of every run:

```text
with-testdata: TMPDIR=/testdata/run.lnkZ8Alq on /dev/vda1[/docker/volumes/musiclib_testdata/_data] ext4 (magic 0xef53), uid=1000 gid=1000
```

- **Native Docker Engine:** the volume is under `/var/lib/docker/volumes`, on the host's filesystem. That must be ext4.
- **Docker Desktop:** the volume is on the ext4 data disk of Docker Desktop's Linux VM (`/dev/vda1`). It is real ext4, but under the VM's kernel, not the host's.

The fsops tests also use `/dev/shm` (tmpfs, always present in containers) as a "different filesystem" for the cross-device cases. No privileges, loop devices or `mount` are needed. A loop-mounted ext4 image was rejected because it needs `CAP_SYS_ADMIN` or `--privileged`.

If a volume was created by a different uid and is no longer writable, `with-testdata.sh` says so. Remove the development volumes (never the installation's `musiclib_db`, `musiclib_data` or `musiclib_backup`); they are recreated on the next run:

```sh
docker volume rm musiclib_testdata musiclib_go-build-cache musiclib_go-mod-cache
```

### The really full filesystem (`/fullfs`)

The full-disk tests (a disk that fills up during a build must leave the published album and the originals intact) need a filesystem that really fills up. The `test` and `dev` services mount a **fixed-size tmpfs** at `/fullfs` (`size=1088m`: the app's 1 GiB free-space margin plus room for the test albums) and set `MUSICLIB_FULLFS=/fullfs`; the gate also sets `MUSICLIB_REQUIRE_FULLFS=1`, so these tests fail instead of skipping there.

- The daemon mounts it; the test container gains no privilege.
- It is private to each container and disappears when the container exits: nothing persists, and no other run or volume shares it.
- Before it cleans or fills anything, `internal/faulttest.FullFS` requires `/fullfs` to be a mount point of at most 2 GiB that is not the filesystem of `TMPDIR`. It also serializes the test binaries with a `flock`. The shared ext4 `testdata` volume is never filled.
- tmpfs holds its data in RAM: while a full-disk test runs, up to about 1.1 GiB of the Docker VM's memory is in use, then released.
- It is tmpfs, not ext4. No unprivileged container can mount an ext4 image: Docker's `local` volume driver passes its options straight to mount(2), so an image file gives "block device required" and `o=loop` is rejected; a loop device needs a privileged container on the host. ext4's delayed-allocation ENOSPC at fsync is covered by injected failpoints.

Outside Docker (`MUSICLIB_FULLFS` unset) these tests skip.

## Version and image labels

`musiclibd version` prints the application version and `render_version` (the identity of the renderer, naming rules and pinned tools) on two lines, `version: …` and `render_version: …`, and exits 0. It reads no environment and needs neither the database nor the volumes. The server also logs the version in its first event, `starting`.

The version is stamped at build time from the build argument `MUSICLIB_VERSION` (default `devel`, a token of `[0-9A-Za-z.+-]`; the build fails on anything else, or if the binary does not report it); it is also the backup manifest's `app_version`. The source build of `compose.dev.yaml` is `devel`; the published image carries its release version. The `runtime` image carries the OCI labels `org.opencontainers.image.{title,description,version,revision,source,licenses}`, fed by `MUSICLIB_VERSION`, `MUSICLIB_REVISION` and `MUSICLIB_SOURCE` (empty by default):

```sh
docker build --target runtime --build-arg MUSICLIB_VERSION=1.0.0 \
  --build-arg MUSICLIB_REVISION="$(git rev-parse HEAD)" \
  --build-arg MUSICLIB_SOURCE=https://github.com/OWNER/REPO -t musiclib-app:1.0.0 .
docker run --rm musiclib-app:1.0.0 version
docker image inspect musiclib-app:1.0.0 --format '{{json .Config.Labels}}'
```

`backup` and `restore` use the PostgreSQL 17.11 client tools (`pg_dump`, `pg_restore`) of the runtime image.

## Pinned images

Every image is pinned by exact version **and** by the digest of its multi-arch index. The digest is what is actually used; the tag documents it. The one exception is the application's own image in `compose.yaml`, pinned by its exact release version: its digest exists only once the release is published, and the release notes name it.

| Image | Where | Pin |
|---|---|---|
| Dockerfile frontend | `Dockerfile` line 1 | `docker/dockerfile:1.26.0@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32` |
| Go 1.25.14, Debian 13 | `Dockerfile` `GO_IMAGE` | `golang:1.25.14-trixie@sha256:2c4c60ef415fbfa5e90300722293bef36c5e63fae17570ce18f580af933dbd73` |
| runtime base, Debian 13 | `Dockerfile` `RUNTIME_IMAGE` | `debian:trixie-20260918-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a` |
| PostgreSQL 17.11 | `compose.yaml` (`postgres`), `compose.dev.yaml` (`postgres`, `postgres-test`) | `postgres:17.11-trixie@sha256:f4c66b820c6f974249089d3d16d86a3698eae11e8746eb6644b2271031e91232` |
| Vibrance MusicLib (the app) | `compose.yaml` (`app`) | `ghcr.io/tommasonovelli/musiclib:1.2.0`: the release version, without a digest |
| Caddy 2.11.4 (documentation example only) | [operations guide, "Caddy in Docker"](operations.md#caddy-in-docker) | `caddy:2.11.4-alpine@sha256:6aeddd44c3078b0f9a35206472a11420648a79c184603ef95957d0a20044cb2b` |
| shellcheck 0.11.0 | `scripts/lint-shell.sh` | `koalaman/shellcheck:v0.11.0@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d` |
| sqlc 1.31.1 | `scripts/lib/common.sh` | `sqlc/sqlc:1.31.1@sha256:70f53171d27b2424e9358869975455a6e955a5aa8e58a998a270a6e34e525537` |
| BuildKit 0.32.2 | `.github/workflows/release.yml` (`BUILDKIT_IMAGE`) | `moby/buildkit:v0.32.2@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8` |
| SBOM scanner 1.12.0 | `.github/workflows/release.yml` (`SBOM_GENERATOR`) | `docker/buildkit-syft-scanner:1.12.0@sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9` |
| actionlint 1.7.12 | [Releasing](#releasing) (lint of the workflow) | `rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667` |

Toolchain and runtime share the same Debian release (13, glibc 2.41). The native tools do not depend on it: ffmpeg, ffprobe and the TagLib helper are fully static.

### Pinned source builds

Native tools are built from release tarballs pinned by version **and** sha256 (`Dockerfile`, ARGs of the stage). The build fails if a download does not match its hash.

| Tool | Stage | Pin | Goes into |
|---|---|---|---|
| FFmpeg 8.1.3 (ffmpeg, ffprobe) | `build-ffmpeg` | `ffmpeg-8.1.3.tar.gz` `bd458826a039b48a9606e794554c75eb4c4984b84173f7afa3128eae89336f2b` (OpenPGP signature checked) | `toolchain` (so `test`, `dev`) and `runtime`, as `/usr/local/bin/ffmpeg` and `/usr/local/bin/ffprobe` |
| nasm 2.16.03 | `build-ffmpeg` | `nasm-2.16.03.tar.gz` `5bc940dd8a4245686976a8f7e96ba9340a0915f2d5b88356874890e207bdb581` | nowhere: assembles FFmpeg's x86 code |
| LAME 3.100 | `build-lame` | `lame-3.100.tar.gz` `ddfe36cab873794038ae2c1210557ad34857a4b6bdc515785d1da9e175b1da1e` | `toolchain` only: MP3 test fixtures |
| TagLib 2.3.2 | `build-tags` | `taglib-2.3.2.tar.gz` `3ca2d8afaa7f1cf7f6ed10e511ebc368bfacd6dcaa3dbfa690b89e502e8963dc` (no upstream signature; GitHub asset digest and Homebrew agree) | linked statically into `musiclib-tags` |
| CMake 4.4.3 (Kitware binary) | `build-tags` | `cmake-4.4.3-linux-x86_64.tar.gz` `d6c83076c575bc00b823522ac974bda66d0af05d6ddc30e739c12385cf32c6cc` (signed SHA-256 list checked) | nowhere: builds TagLib |

The TagLib helper `native/musiclib-tags` is built in `build-tags` from this repository, against those two:

| Binary | Build | Goes into |
|---|---|---|
| `musiclib-tags` | release: `-O2`, fully static (libc and libstdc++ included), 3 MB | `toolchain` (so `test`, `dev`) and `runtime`, as `/usr/local/bin/musiclib-tags` |
| `musiclib-tags-asan` | ASan and UBSan, TagLib included; a finding exits 86 | `toolchain` only, as `/usr/local/bin/musiclib-tags-asan`: the hostile-input tests run every case on both |

The stage also runs `make check`, the unit tests of the helper's own parsers under the sanitizers, and fails if they fail. Nothing of the helper is built on the host; `native/musiclib-tags/build/` is ignored by git and Docker.

- **The ffmpeg binaries are fully static.** The images hold the same bytes, and the build is reproducible: a `--no-cache` rebuild gives the same sha256.
- **The version string** both tools report is `8.1.3-musiclib1`: the release plus `--extra-version`, the revision of the configure line. `musiclibd` refuses to boot with anything else (`media_tool_version`), and so does the test gate (`TestPinnedToolsInstalled`).
- **Bumping FFmpeg:**
  1. Download the new tarball and its `.asc`, check the signature against the FFmpeg release key, and compute the sha256.
  2. Update `FFMPEG_VERSION` and `FFMPEG_SHA256`.
  3. Reset `FFMPEG_EXTRA_VERSION` to `musiclib1`, or increase it when only the configure line changes.
  4. Update `media.PinnedVersion` and this table; record how the pin was verified in the commit message.
  5. Run `scripts/check.sh`. `TestMP3EstimationWarningOfThePinnedTool` and the fixtures check the behaviours the adapter relies on.
- **The first build** of the `build-ffmpeg` stage takes a few minutes. It is cached afterwards, and shared by the `test`, `dev` and `app` images.
- **The TagLib helper is reproducible too:** the images hold the same `musiclib-tags`, and a `--no-cache` rebuild of `build-tags` gives the same sha256 for both binaries and for `libtag.a`. The stage takes about 80 s without cache; a change under `native/musiclib-tags/` rebuilds only the helper.
- **The helper's version** is two strings: `musiclib-tags version` prints `{"helper":"4","taglib":"2.3.2-musiclib1"}`. `helper` is `kHelperVersion` in `native/musiclib-tags/src/version.h`; `taglib` is the linked TagLib's own version plus `TAGLIB_BUILD_REVISION`, the revision of the cmake line. `musiclibd` refuses to boot with anything else (`media_tool_version`), and so does the gate (`TestPinnedToolsInstalled`).
- **Changing the helper** in a way that can change an inspection or a written file (the field table, a reading rule, the bytes written): bump `kHelperVersion` and `media.PinnedTagsVersion` together, then re-pin the binary's sha256 in `render.TestToolBinariesPinned` and the value in `TestVersionGolden` (every album renders again). The MP3 reader and writer (`src/id3v2.cpp`, `src/ape.cpp`, `src/mp3.cpp`) and the M4A ones (`src/mp4.cpp`, `src/m4a.cpp`) are the helper's own; TagLib only cross-checks them. The re-pin, step by step:
  1. `docker build --target build-tags .` (runs the unit tests);
  2. `scripts/dev.sh sha256sum /usr/local/bin/musiclib-tags /usr/local/bin/musiclib-tags-asan`;
  3. the release sha256 into `pinnedBinaries` (`internal/render/version_test.go`), both into the commit message;
  4. confirm with `docker build --no-cache --target build-tags .` that the bytes do not change, then `scripts/check.sh`.
- **Bumping TagLib:**
  1. Download the new release tarball, compute its sha256 and compare it with GitHub's asset digest and an independent pin (Homebrew's formula). TagLib does not sign its releases.
  2. Read the release's changes to `flac/flacfile.cpp`, `ogg/xiphcomment.cpp` and `flac/flacpicture.cpp`, and for MP3 `mpeg/mpegfile.cpp`, `mpeg/id3v2/id3v2framefactory.cpp`, `mpeg/id3v2/id3v2frame.cpp`, `ape/apetag.cpp` and `tagutils.cpp` (`Utils::findID3v1`, `findAPE`). For M4A, `mp4/mp4atom.cpp`, `mp4/mp4tag.cpp`, `mp4/mp4itemfactory.cpp` and `mp4/mp4properties.cpp`. The helper's readers mirror where TagLib finds tags and what it drops or alters: a change there can require a change of a reader.
  3. Update `TAGLIB_VERSION` and `TAGLIB_SHA256`; reset `TAGLIB_BUILD_REVISION` to `musiclib1`, or increase it when only the cmake line changes.
  4. Update `media.PinnedTagLibVersion` and this table; record how the pin was verified in the commit message.
  5. Run `scripts/check.sh`: the hostile-input tests run on the new TagLib under the sanitizers too.
- **Bumping CMake:** download the tarball and `cmake-<v>-SHA-256.txt.asc`, check the signature against Kitware's release key, and update `CMAKE_VERSION` and `CMAKE_SHA256`. CMake does not reach any image, but it drives the TagLib build: check that the binaries are unchanged or bump `TAGLIB_BUILD_REVISION`.

### Bumping a pin

1. Pick the **exact** new tag (a patch version, or a dated tag for Debian). Never `latest`, and never a floating tag like `17` or `trixie-slim`.
2. Resolve the index digest from the registry:

   ```sh
   docker buildx imagetools inspect golang:1.25.15-trixie | awk '/^Digest:/{print $2}'
   ```

   To map a floating tag to its exact version, pull it and read the version variable, e.g. `docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' golang:1.25-trixie | grep GOLANG_VERSION`.
3. Update the tag and the digest together in the file from the table above, and in the table itself.
4. Run `scripts/check.sh`. For postgres, change both Compose files, then run `docker compose -f compose.dev.yaml up -d --wait postgres`.
5. Update [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md) and `licenses/` when a component of the runtime image or a Go module compiled into `musiclibd` changes: its version, and its license texts. That covers FFmpeg (`licenses/ffmpeg/NOTICES.txt` says how it was made), TagLib, Go, `go.mod` (`go version -m` on the built `musiclibd` lists the modules), the runtime base and its snapshot date, and `GO_IMAGE`, whose C and C++ runtime libraries go into the static binaries (`dpkg-query -W libc6-dev gcc-14` in it gives their versions).
6. Commit the bump on its own, with the old and new version in the message.

Rules:

- A **PostgreSQL major** bump (17 → 18) is a dump and restore, not a tag change.
- The Go, TagLib and ffmpeg versions are inputs of `render_version`, so bumping them changes `render_version`.

## Releasing

`.github/workflows/release.yml` publishes a release when a tag `vX.Y.Z` is pushed. It runs on GitHub-hosted `ubuntu-24.04` runners and builds for linux/amd64 only. Its three jobs run in order, and each one stops the release if it fails:

1. **guard**: the tag is `vMAJOR.MINOR.PATCH` (no leading zeros, no suffix), it still names the pushed commit, and that commit is on `main`. The `app` image of `compose.yaml` is exactly `ghcr.io/<owner>/musiclib:X.Y.Z`, `.env.example` exists, and `CHANGELOG.md` has exactly one non-empty `## [X.Y.Z]` section.
2. **publish**: first the gate, `scripts/check.sh` unchanged, with the Dockerfile's default uid 10001 and `GATE_TEST_TIMEOUT=20m`, in a Buildx builder with the pinned BuildKit. Then it logs in to GHCR, refuses if `ghcr.io/<owner>/musiclib:X.Y.Z` already exists, and builds the `runtime` target in the same builder (reusing the FFmpeg, TagLib and toolchain layers of the gate) with `MUSICLIB_VERSION=X.Y.Z`, `MUSICLIB_REVISION` (the commit) and `MUSICLIB_SOURCE` (the repository URL). It pushes `:X.Y.Z`, plus `:X.Y` and `:X` when this release is the newest of its series, and `:latest` when it is the newest of all (by `sort -V` of the `vX.Y.Z` tags, fetched by this job): a patch of an older series, or a re-run of an older release, never moves them back. `compose.yaml` keeps the explicit version. The image carries an SBOM and a `mode=max` provenance attestation.
3. **release**: pulls the image by digest, and checks that `version` prints `version: X.Y.Z` and that the revision label is the commit. It then creates the GitHub Release «Vibrance MusicLib X.Y.Z». The release body is the `CHANGELOG.md` section, followed by the image with its digest and an install snippet. Its assets are `compose.yaml` and `env.example` (the repository's `.env.example`: GitHub renames asset names that start with a dot), `THIRD_PARTY_NOTICES.md`, and the FFmpeg and TagLib source tarballs built into the image, downloaded again and checked against the versions and sha256 of the `Dockerfile`'s ARGs. The release is marked **Latest** on GitHub with the same rule as `:latest`, checked again on the tags this job fetched: only when it is the newest version (`--latest=false` otherwise).

Release one version at a time. All runs share one concurrency group, so a run waits for the one before it and the floating tags and the Latest release only move forward. GitHub keeps only one *pending* run per group: pushing a third tag while one run is in progress and another is waiting cancels the waiting one. That is safe, nothing of it was published: when the others have finished, open the cancelled run and use **Re-run all jobs**.

### Release check on a native host

A release requires a passing run on a **native** Ubuntu 24.04+ Docker Engine with `/var/lib/docker` (the `testdata` named volume) and `MUSICLIB_DATA` on ext4. Docker Desktop testing is not a substitute: its gate uses real ext4 inside the VM, not the native host's kernel. Before a release, run:

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

Record the machine and kernel, the commands, their output and any skipped tests. Do not release until the native gate and the acceptance tests above (a test collection imported, edited, interrupted and restored without manual fixes) pass.

### Publishing a release

On `main`, with a clean tree and after the [release check on a native host](#release-check-on-a-native-host):

```sh
# compose.yaml: the app's image line becomes ghcr.io/tommasonovelli/musiclib:X.Y.Z
# CHANGELOG.md: a new section "## [X.Y.Z] - YYYY-MM-DD" above the previous one
git add compose.yaml CHANGELOG.md
git commit -m "Release X.Y.Z"
git push origin main
git tag -a vX.Y.Z -m "Vibrance MusicLib X.Y.Z"
git push origin vX.Y.Z
```

Push the tag on its own: GitHub starts no workflow for tags pushed more than three at a time. Do not create the GitHub Release by hand: the release job creates it, and fails, after the image is published, if the tag already has one (delete that release and re-run the job).

The `CHANGELOG.md` section of a version is its heading, `## [X.Y.Z]`, optionally followed by ` - YYYY-MM-DD`. The section runs up to the next `## ` heading or the first link reference definition (`[X.Y.Z]: https://…`), so keep those definitions at the end of the file. The release body is the section's lines without the heading and without leading or trailing blank lines.

**First release only**, once the workflow has run:

- A new GHCR package is private. On GitHub, open the account's **Packages → musiclib → Package settings → Change visibility**, and make it **Public**. Until then nobody else can pull the image, although the release is already visible.
- In the same settings, check that the package is connected to the repository (the package page shows it; otherwise use **Connect repository**). Also check that **Manage Actions access** gives the repository the **Write** role: every later release pushes with the repository's `GITHUB_TOKEN`. If a package named `musiclib` already existed without that access, the first push fails with 403: grant it and run the workflow again.

### When a run fails

- In **guard**, or in **publish** before its push (the gate included): nothing is published. Fix the problem on `main`, move the tag to the new commit (`git tag -d vX.Y.Z`, `git push origin :refs/tags/vX.Y.Z`, tag and push again).
- In **release**, the image is already published. Use **Re-run failed jobs**: the release job runs again on the same digest, and decides **Latest** again from the tags it fetches then. A job that pushed is never re-run to push again: the version check refuses it. If the image itself is wrong, do not publish it under the same version. Release a new patch version instead.
- In **publish** after the push succeeded: a re-run is refused by the version check, so check the image (below) and create the release by hand, with the `CHANGELOG.md` section in `notes.md` followed by the line ``- Digest: `sha256:…` `` (from `imagetools inspect`), `cp .env.example env.example`, and the FFmpeg and TagLib tarballs downloaded and checked as the workflow's step "Add the third-party notices and sources" does: `gh release create vX.Y.Z --verify-tag --title "Vibrance MusicLib X.Y.Z" --notes-file notes.md compose.yaml env.example THIRD_PARTY_NOTICES.md ffmpeg-*.tar.gz taglib-*.tar.gz` (add `--latest=false` if it is not the newest version). If the push stopped after `:X.Y.Z`, move each floating tag it should have moved by hand, only when this is the newest version of that series (`:latest`: of all), e.g. `docker buildx imagetools create -t ghcr.io/tommasonovelli/musiclib:latest ghcr.io/tommasonovelli/musiclib:X.Y.Z`. Or release a new patch version.

### Checking a published release

The digest must be the one in the release notes:

```sh
docker buildx imagetools inspect ghcr.io/tommasonovelli/musiclib:X.Y.Z
docker buildx imagetools inspect ghcr.io/tommasonovelli/musiclib:X.Y.Z --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/tommasonovelli/musiclib:X.Y.Z --format '{{ json .Provenance }}'
docker run --rm ghcr.io/tommasonovelli/musiclib:X.Y.Z version
```

### Changing the workflow

Every third-party action is pinned by commit SHA, with its version in a comment. To bump one, resolve the tag with `gh api repos/OWNER/ACTION/git/ref/tags/vX.Y.Z` (for an annotated tag, follow its object with `gh api repos/OWNER/ACTION/git/tags/SHA`). The BuildKit and SBOM scanner images are pinned like every other image ([Pinned images](#pinned-images)). Then lint:

```sh
docker run --rm -v "$PWD:/repo:ro" -w /repo \
  rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667 -color
```

On Git Bash, prefix it with `MSYS_NO_PATHCONV=1` and use `$(pwd -W)`. actionlint also runs shellcheck on every `run:` script.
