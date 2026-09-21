# Docker: build, test and run

Everything in this repository is built, tested and run in Docker. The host needs
only **Docker Engine (or Docker Desktop) with the Compose v2 plugin**. No Go,
gcc, PostgreSQL or ffmpeg on the host.

| File | Role |
|---|---|
| `Dockerfile` | multi-stage: `toolchain` → `deps` → `test` / `build-app` → `runtime` |
| `compose.yaml` | `postgres` (always), `app` (profile `app`), `test` and `dev` (profile `tools`) |
| `docker/with-testdata.sh` | in-container: puts `TMPDIR` on the ext4 test volume, refuses other filesystems |
| `docker/gate.sh` | in-container: build, vet, gofmt, `go test -race` |
| `scripts/check.sh` | the full gate |
| `scripts/fuzz.sh` | one fuzz target on the live sources |
| `scripts/dev.sh` | shell or single command in the toolchain container |
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
```

`check.sh` builds the `test` image, which contains a **snapshot of the working
tree** taken at build time, and runs `docker/gate.sh` in it:

```text
go build ./...  &&  go vet ./...  &&  test -z "$(gofmt -l .)"  &&  go test -race -count=1 ./...
```

The `test` container has no network, a read-only root filesystem, no
capabilities and runs as your uid (never root: root bypasses permission checks,
so permission tests would pass for the wrong reason). Modules come from the image
layer, downloaded and verified against `go.sum` when `go.mod`/`go.sum` change.
It tests exactly the tree it was built from, even while you keep editing.

`dev` and `fuzz.sh` bind-mount the live sources instead, with network access
for `go get`. `fuzz.sh` needs that, so a failing input is written back to
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
and `synchronous_commit` are set to `on` explicitly (§11.1). The cluster is
initialized with data checksums and the PostgreSQL 17 `builtin` `C.UTF-8`
locale, so collation does not depend on the image's glibc. **No port is
published** (§10.4). The app reaches it on the Compose network.
`POSTGRES_PASSWORD` defaults to `musiclib`: set your own in `.env` before the
first `up`. It is read only when the volume is initialized.

**app** (profile `app`): not built or started by plain `docker compose up` or
`build`, because `./cmd/musiclibd` does not exist yet. Once it does:

```sh
docker compose --profile app up -d --build
docker compose stop app && docker compose run --rm app doctor --deep && docker compose start app   # §11.3
```

It follows §11.1. The environment is `DATABASE_URL`, `PUBLIC_ORIGIN`,
`HTTP_ADDR=:8080` and `WORKERS`. Other settings:
- `/data` is the named volume `musiclib_data`, or an ext4 host path through
  `MUSICLIB_DATA`.
- `/import` is a read-only bind of `MUSICLIB_IMPORT` (default `./import`). It
  must exist; Compose does not create it.
- `init: true`, `restart: unless-stopped`, 45 s stop grace.
- Runs as `MUSICLIB_UID:MUSICLIB_GID` (default 1000:1000), with no
  capabilities, a read-only root filesystem and tmpfs `/tmp`.
- Published on `127.0.0.1:8080`. For LAN access, set `MUSICLIB_BIND` and a
  matching `PUBLIC_ORIGIN`.

| Variable (`.env`) | Default | Meaning |
|---|---|---|
| `POSTGRES_PASSWORD` | `musiclib` | DB password (initdb time only) |
| `MUSICLIB_UID` / `MUSICLIB_GID` | `1000` | ids of the app process and owner of `/data` |
| `MUSICLIB_DATA` | `musiclib-data` | named volume or absolute ext4 path for `/data` |
| `MUSICLIB_IMPORT` | `./import` | host directory mounted read-only on `/import` |
| `MUSICLIB_BIND` / `MUSICLIB_PORT` | `127.0.0.1` / `8080` | published address |
| `PUBLIC_ORIGIN` | `http://127.0.0.1:${MUSICLIB_PORT}` | §10.4 |
| `WORKERS` | `2` | worker pool size |
| `MUSICLIB_DEV_UID` / `MUSICLIB_DEV_GID` | your `id -u` / `id -g` | uid of `test`/`dev` (set by the scripts) |

## Pinned images

Every image is pinned by exact version **and** by the digest of its multi-arch
index (DESIGN.md §2.1). The digest is what is actually used; the tag documents it.

| Image | Where | Pin |
|---|---|---|
| Dockerfile frontend | `Dockerfile` line 1 | `docker/dockerfile:1.26.0@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32` |
| Go 1.25.14, Debian 13 | `Dockerfile` `GO_IMAGE` | `golang:1.25.14-trixie@sha256:2c4c60ef415fbfa5e90300722293bef36c5e63fae17570ce18f580af933dbd73` |
| runtime base, Debian 13 | `Dockerfile` `RUNTIME_IMAGE` | `debian:trixie-20260918-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a` |
| PostgreSQL 17.11 | `compose.yaml` | `postgres:17.11-trixie@sha256:f4c66b820c6f974249089d3d16d86a3698eae11e8746eb6644b2271031e91232` |
| shellcheck 0.11.0 | `scripts/lint-shell.sh` | `koalaman/shellcheck:v0.11.0@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d` |

Toolchain and runtime share the same Debian release (13, glibc 2.41), so the
future TagLib helper is built and run against the same libraries.

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
