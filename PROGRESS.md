# Implementation status

Tracks what has been implemented against `DESIGN.md`.
Reference order: DESIGN.md §13.1.

Doubts, bugs and uncertainties live in **`NOTES.md`**, not here.

**Legend:** `[ ]` to do · `[~]` partial · `[x]` done and tested

---

## Project conventions

- **English only.** All code, identifiers, comments, error and log messages,
  test names, test failure messages, commit messages and project docs are
  written in English.
- **`DESIGN.md` is the exception.** It is the owner's normative spec and stays
  in Italian. Code and docs cite it by section number (`DESIGN.md §5.2`).
- **Everything runs in Docker.** The whole project is containerized: build,
  tests and tooling, not only the deployment. This is a new owner requirement,
  still to be planned (see the Phase 1 checklist and **N-012** in `NOTES.md`).

---

## Phase 1 — Foundations

- [x] Go module (`musiclib`, Go 1.25.0, `golang.org/x/text v0.41.0` pinned)
- [x] Normalization: text, segments, truncation, keys, relative paths (§5.2) — `internal/names`
- [x] Containerized toolchain and gate: build/vet/gofmt/`go test -race` in Docker, TMPDIR on an ext4 volume (§3.1, §12.1) — `scripts/check.sh`, `docs/docker.md`
- [ ] Full repository layout (§2.3): `cmd/musiclibd`, `internal/volume` (N-060) and `internal/media` now exist; the other packages arrive with their phases
- [x] Docker Compose: `app` + PostgreSQL 17, digests pinned (§2.1, §10.4, §11.1) — non-root, `init`, `restart: unless-stopped`, loopback only, healthcheck via `musiclibd healthcheck`; verified end to end (N-071). ffmpeg/ffprobe (N-073) and the static TagLib helper `musiclib-tags` (N-083) are in the runtime image since Phase 2
- [x] `goose` migrations of the normative schema (§4.2), applied forward only under an advisory lock — `migrations/`, `store.Migrate`
- [x] `sqlc` setup (§2.1): pinned image, generated code committed, `sqlc diff` in the gate — `sqlc.yaml`, `sql/`, `internal/store`; Phase 1 queries only (store id, migration lock)
- [x] pgx pool with `WORKERS + 8` connections (§11.1) and UUIDv7 ids (§2.1) — `store.NewPool`, `store.NewID`
- [x] Real PostgreSQL 17 for the tests (§12.1, N-024) — `postgres-test` service, `internal/store/pgtest`
- [x] `internal/fsops`: confined primitives (`openat2 RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS`, `renameat2`, fsync) (§10.4)
- [x] Volume lock (`flock` on `/data/.lock`) and volume identity (`.musiclib-store` ↔ `settings.store_id`) (§2.2, §11.1) — `internal/volume`; maintenance marker read and enforced (§11.3)
- [x] `internal/blobstore`: 5-step verified put, dedup, `corrupt_blob` (§7.5)
- [x] Boot checks: same filesystem and mount, permissions, `RENAME_EXCHANGE` available (§3.1) — `volume.CheckFilesystem`
- [x] `cmd/musiclibd`: environment configuration, umask 022, no root, JSON logs, boot steps 1, 2, 3, 5 (blob temporaries and probe leftovers) and 7, `/health/live` and `/health/ready`, `healthcheck` subcommand, graceful shutdown with the lock released last (§2.3, §6.1, §11.1). There is no worker pool yet: step 7 only turns readiness positive

## Phase 2 — First vertical slice (one FLAC album)

- [x] `internal/media`: `ffprobe` / `ffmpeg` adapter, `AudioDigest` (§8.4) — pinned static FFmpeg 8.1.3 in every image (N-073), tool Runner (§8.5, §6.1), probe and classification (§7.2, §8.1), boot check of the tool versions (§11.1 step 3)
- [~] `native/musiclib-tags`: C++ TagLib helper (inspect / extract-images / write-managed-tags) (§8.1) — **FLAC complete**: pinned static TagLib 2.3.2 (N-083), the three operations, the Go adapter `Inspect` / `ExtractImages` / `WriteManagedTags` and the §9.1 step 6 check `VerifyTags`; MP3 and M4A answer a typed `unsupported_format` until Phase 4 (N-094)
- [~] Managed tag mapping and alias removal (§8.2, §8.3) — **FLAC complete** (table in `native/musiclib-tags/src/fields.h`, N-088); the MP3 and M4A tables are Phase 4 (N-094)
- [ ] `internal/importer`: import of a single album candidate
- [ ] `internal/catalog`: domain transactions, revisions, reservations, enqueue (§4.3, §5.3)
- [ ] `internal/render`: snapshot → pure plan → build in staging (§9.1)
- [ ] `.musiclib.json` receipt (§9.2)
- [ ] `internal/publish`: PREPARE / INSTALL / FINALIZE + journal (§9.3)
- [ ] Journal recovery at startup (§9.4)
- [ ] Boot steps 4 (journal recovery), 5 (running jobs back to pending, cleanup of `work/render` and `work/retired`), 6 (stale renders) and the worker pool of step 7, in the places marked in `cmd/musiclibd` `boot()` (§11.1); exit on database loss once workers exist (§6.4, N-070)
- [ ] `internal/jobs`: claim, pool, completion (§6.2, §6.4)

## Phase 3 — Concurrency

- [ ] Render coalescing on a single row per album (§6.3)
- [ ] `REPEATABLE READ` snapshots (§6.2)
- [ ] `path_claims` and global `pg_advisory_xact_lock` (§5.3)
- [ ] Conditional APIs: strong ETag, `If-Match`, 412/428 (§10.1)
- [ ] Artist rename and album reassignment (§4.3)
- [ ] Named failpoints and failure matrix (§12.2)

## Phase 4 — Formats and content

- [ ] MP3 (ID3v2.4, APE, ID3v1 migration), M4A AAC/ALAC (§8.1–8.3) — in `native/musiclib-tags`: independent MP3 and M4A readers, the writers, their alias and sort tables, and the ID3v1 exclusion in `VerifyTags` (N-094); probe and `AudioDigest` already handle both
- [ ] Covers: selection, limits, upload, removal (§7.4, §8.5)
- [ ] LRC files associated with tracks (§7.4)
- [ ] Attachments under `Extras/` (§5.1, §7.4)
- [~] Verification and preservation of unmanaged tags (§8.3) — FLAC done (`media.VerifyTags`, N-086); MP3 and M4A with their readers (N-094)

## Phase 5 — Full experience

- [ ] Recursive scan and multi-disc grouping (§7.2)
- [ ] Inferred initial metadata and import overrides (§7.3)
- [ ] Fingerprinting and duplicate detection (§7.6)
- [ ] Complete HTTP APIs (§10.2)
- [ ] UI: Library, Album, Import, Activity (§10.3)
- [ ] HTTP security boundary: `PUBLIC_ORIGIN`, `X-Musiclib-Request` (§10.4)
- [ ] Trash, restore, retry (§4.3, §6.4)

## Phase 6 — Operations

- [ ] `doctor`, normal and `--deep` (§11.3)
- [ ] `rebuild` with maintenance marker (§11.3)
- [ ] `backup` / `restore` (§11.4)
- [ ] Space budget and `statfs` check (§11.2)
- [ ] Operations guide with Compose examples (§11)

---

---

## Details of what is done

### `internal/names` — normalization (§5.2) ✔

Public API:

| Function | Role |
|---|---|
| `NormalizeText` / `NormalizeRequiredText` | metadata text: NFC, trim, no control characters, at most 1,024 characters |
| `Segment` | sanitized directory segment |
| `FileSegment` | like `Segment`, but preserves the extension when truncating |
| `Key` | comparison key `NFC(casefold(final_segment))` |
| `FolderKey` | `Key(Segment(name))`: value of `artists.folder_key` and `albums.folder_key` |
| `PathKey` | segment keys joined by `/`: value of `attachments.path_key` |
| `SplitRelPath` / `SplitRelPathOrRoot` | validation of a relative path, segments **not** sanitized (names to open on disk) |
| `SanitizeRelFilePath` | relative output path: sanitized segments, `Path` and `Key` |
| `Error` / `Code` | typed errors with a stable code for the API's `{code, message, details}` body |

Tests: table-driven, idempotence, fuzzing (`FuzzSegment`, `FuzzKey`, `FuzzNormalizeText`,
`FuzzSplitRelPath`) and exhaustive enumeration of every Unicode code point for the
properties of `Key`. Coverage 99.2%; `go vet` and `go test -race` clean.

```sh
go test ./...                                    # full suite (~0.4 s)
go test ./internal/names/ -run=XXX -fuzz=FuzzSegment -fuzztime=60s
```

### `internal/fsops` — confined filesystem primitives (§2.3, §10.4) ✔

The only package that touches the filesystem. Every path is relative to a
`Root` (a directory descriptor) and is validated with `names.SplitRelPath`
before any syscall. It is then resolved by a single `openat2` with
`RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS`. Single-component operations act on a
resolved directory descriptor plus a name. Every open carries
`O_CLOEXEC|O_NONBLOCK|O_NOCTTY`. An AST test forbids path-based calls
(`filepath.Join`, `os.Open`, `unix.Openat`, `AT_FDCWD`, ...) and confines
`unix.Open`, `unix.Openat2` and `unix.Close` to one function each.

Public API:

| Function | Role |
|---|---|
| `OpenRoot(path)` | the only host path accepted; checks that `openat2` is usable (`fs_openat2_unsupported`, §3.1) |
| `Root.SubRoot` / `Root.Close` / `Root.Name` | independent confined sub-root; close waits for in-flight syscalls and rejects new ones; label for errors |
| `Root.Open` / `Root.CreateExclusive` / `Root.OpenFile` | regular files only; symlinks, FIFOs, sockets and devices rejected without blocking; explicit flag set |
| `Root.Stat` / `Root.ReadDir` | describe without following; `ReadDir` sorted by name bytes; types reported, not rejected |
| `Root.Mkdir` / `Root.MkdirAll` / `Root.MkdirAllSync` | never adopt a symlink; `MkdirAll` returns the directories created |
| `Root.Rmdir` / `Root.Remove` / `Root.RemoveAll(ctx)` | `rmdir` only if empty; `Remove` unlinks symlinks without following; confined recursive removal |
| `RenameNoReplace` / `RenameExchange` | `renameat2` NOREPLACE / EXCHANGE, same or different `Root`s; no fsync |
| `SyncFile` / `SyncAndClose` / `Root.SyncDir` / `Root.SyncDirAndParents` | fsync of files and directories, bottom-up to the root; errors never dropped |
| `SameFilesystem` / `ProbeRenameExchange` | `st_dev` comparison; real `RENAME_EXCHANGE` probe with content check, no fallback |
| `Root.Lock` / `Lock.Close` | exclusive non-blocking `flock` (`fs_lock_busy`), `O_CLOEXEC` |
| `Root.StatFS` | total and unprivileged-available bytes (§11.2) |
| `SameMount` / `Root.CheckAccess` | mount id via statx `STATX_MNT_ID`; faccessat2 with `AT_EACCESS` (boot checks, N-063; Linux 5.8) |
| `RemoveProbeLeftovers` | removes `.musiclib-probe-*` directories left by an interrupted probe (N-033) |
| `FileType`, `FileInfo`, `DirEntry` | entry types (`IsSpecial`), identity (dev/ino), permissions |
| `Error` / `Code` | stable `fs_*` codes plus the relative location(s); never an absolute path |

Tests run on the real kernel, with no mocks: traversal, absolute paths and empty
segments rejected before disk; leaf and intermediate symlinks (inside and
outside the root); sequential and concurrent TOCTOU swaps; FIFO, socket and
device rejected under a timeout guard; NOREPLACE/EXCHANGE semantics, renames
across roots and filesystems; the probe and its cleanup; flock exclusivity and
`O_CLOEXEC` verified in a child process; descriptor leaks; the `Root` lifetime
protocol. The device subtest skips without `CAP_MKNOD`; see N-038 for the
other skips.

```sh
go test -race -count=1 -timeout 180s ./internal/fsops/        # host, ~1 s
scripts/check.sh ./internal/fsops/...                         # Docker gate, ext4 TMPDIR
scripts/dev.sh go test -race -count=20 ./internal/fsops/      # flakiness check
```

### `internal/blobstore` — originals (§3.1, §7.5, §11.3) ✔

The single blob-put implementation (§13.2), built on `internal/fsops`.
Layout: `originals/ab/cd/<sha256>`, with temporaries in `work/blobs/<random>.tmp`.
Nothing in the package removes or rewrites a pinned blob (§3.2).

| Function | Role |
|---|---|
| `New(originals, work)` | store over two fsops roots; creates `work/blobs` durably |
| `Put(ctx, src) (Blob, error)` | §7.5 steps 1–5; already-exists branch re-hashes and fsyncs the existing file, `corrupt_blob` on mismatch; temp removed on every path |
| `Open(sha)` | pinned blob read-only, regular file only |
| `Check(Blob)` | normal doctor: exists, regular, size (§11.3) |
| `Verify(ctx, sha) (size, error)` | deep doctor: full SHA-256 (§11.3) |
| `CleanTemps(ctx)` | boot cleanup of leftover `*.tmp` only (§11.1 step 5) |
| `ValidateSHA` | 64 lowercase hex digits, checked before any path is built |
| `Blob`, `Error`, `Code` | `{SHA256, Size}`; stable `blob_*` / `corrupt_blob` codes |

How the §7.5 steps map to the code:

1. `writeTemp`: exclusive create of the temporary (mode 0444), copy through a
   context-checking reader, SHA-256 and size.
2. `fsops.SyncAndClose`, then `readBlob` re-reads the temporary and compares
   hash and size.
3. `MkdirAll` plus `SyncDirAndParents` of the shard chain, on every put (N-041).
4. `fsops.RenameNoReplace`. On `fs_exists` the put goes to `checkExisting`.
5. `SyncDir` of `work/blobs` and of the shard directory, on both branches.

Tests on real ext4: property test over sizes around the buffer boundary;
16 concurrent puts of the same content; an existing entry that does not
match (bytes, size, symlink, directory) gives `corrupt_blob` and is left
untouched; injected failures at every protocol point (source error,
cancellation mid-copy, ENOSPC, temp altered before the re-read, failures
before and after the rename, temp removal failure); protocol order; doctor
checks; hash validation; temp cleanup. Not covered: a real full disk (N-045)
and real crashes (N-044).

```sh
scripts/check.sh ./internal/blobstore/...
scripts/dev.sh go test -race -count=20 ./internal/blobstore/
```

### `internal/store` + `migrations/` — schema, pool, migrations (§2.1, §4.2, §11.1) ✔

`migrations/00001_schema.sql` creates the whole normative schema of §4.2:
the ten tables, the `job_ticket` sequence, and every constraint and index it
lists. The interpretations of silent points are in N-052. `migrations.FS`
embeds it.

| Function | Role |
|---|---|
| `NewPool(ctx, url, workers)` | pgx pool, `MaxConns = workers + 8` (§11.1); no global state |
| `Migrate(ctx, pool)` | goose `Up` under a blocking `pg_advisory_lock` on a dedicated connection (N-053); refuses a newer schema, `store_schema_too_new` (N-054); safe at every boot |
| `NewID()` | UUIDv7 row id, strictly increasing in-process (N-051) |
| `New(db)` / `Queries` | sqlc: `GetStoreID`, `InsertStoreID` (idempotent first init, §11.1), `CatalogIsEmpty` (N-069), `LockMigrations` |
| `Error` / `Code` | `store_schema_too_new`, `store_migrate` |
| `pgtest.EmptyDB` / `New` / `Pool` | the only place that knows where the test PostgreSQL comes from (N-024) |

sqlc: edit `sql/*.sql` or `migrations/`, run `scripts/sqlc.sh`, commit the
generated `internal/store/*.go`. `scripts/check.sh` fails if they are stale.

Tests on real PostgreSQL 17 (`postgres-test`):
- concurrent and repeated `Migrate` on an empty database, with each migration
  recorded exactly once;
- refusal of a newer schema;
- idempotent `settings.store_id` first init;
- UUIDv7 version, variant and ordering;
- `TestConstraints`: table-driven, one violating statement per §4.2
  invariant, checking SQLSTATE and constraint name, plus statements that must
  pass, so that no constraint is stricter than the spec;
- the deferred track-number swap (§12.2);
- every FK being `ON DELETE RESTRICT` and indexed, checked on the catalog.

Mutation-checked: dropping `DEFERRABLE`, weakening the render unique index,
`SET NULL` on a FK, a missing FK index and a weakened `published_*` check
each make a test fail.

```sh
scripts/check.sh ./internal/store/...                     # gate, DB tests mandatory
scripts/dev.sh go test -race -count=5 ./internal/store/...
scripts/sqlc.sh                                           # regenerate after editing sql/ or migrations/
```

### `internal/volume` — lock, markers, identity, layout, boot checks (§2.2, §3.1, §11.1, §11.3) ✔

The data volume `/data` as a whole, shared by the server's boot and the
Phase 6 maintenance subcommands (N-060). Disk access goes only through
`internal/fsops` and SQL only through `internal/store`.

| Function | Role |
|---|---|
| `Acquire(path)` | `OpenRoot` + exclusive non-blocking `flock` on `.lock`; `volume_locked` if held |
| `CheckMaintenance()` | reads `.maintenance`: `volume_maintenance_pending` with operation and store id, or `volume_maintenance_malformed`; only reads |
| `Identify(ctx, pool)` | pairs `.musiclib-store` with `settings.store_id`; first init in the §11.1 order; refusals write nothing (table below) |
| `OpenLayout()` | `originals/`, `library/`, `work/` created if missing, fsync of `/data` itself every time (N-037), opened as roots |
| `CheckFilesystem()` | st_dev and mount id of the media directories equal to `/data`; `faccessat2` read/write; real `RENAME_EXCHANGE` probe in `work/` only (N-063) |
| `Close()` | closes the media roots and `/data`, then releases the lock, last |
| `Root`, `Originals`, `Library`, `Work`, `StoreID` | what the boot hands to later steps |
| `Maintenance`, `ParseMaintenance`, `Maintenance.Encode`, `ParseStoreMarker`, `EncodeStoreMarker` | the strict marker format (N-067) |
| `Error` / `Code` | stable `volume_*` codes; the fsops, pgx or parse cause is kept |

| Marker | `store_id` | Media | Catalog (`store.CatalogIsEmpty`) | Outcome |
|---|---|---|---|---|
| id A | A | any | any | paired |
| id A | B | any | any | `volume_store_mismatch` |
| id A | none | any | any | `volume_db_uninitialized` |
| none | any or none | empty (N-062) | empty | first init, or completion of an interrupted one (N-061) |
| none | any or none | empty | not empty | `volume_marker_missing` (N-069) |
| none | A | not empty | any | `volume_marker_missing` |
| none | none | not empty | any | `volume_not_empty` |
| malformed | | | | `volume_marker_malformed` |

Tests run on real ext4 and real PostgreSQL 17, with no mocks:
- first init, then repeated boots on the same pair, with the marker's bytes,
  inode, mtime and ctime unchanged;
- a child process killed with SIGKILL at each of 7 points of the first
  initialization (N-061), then a successful retry;
- a partial temporary;
- a marker without store id, and the reverse on empty and on non-empty media;
- a mismatch, refused twice and never rewritten;
- 6 kinds of non-empty media × 2 database states;
- an empty volume against a database with catalog content (a blob, an
  artist, an album, an import batch or a scan job; with and without a store
  id): refused, and nothing is written to either side (N-069);
- 17 malformed markers (a directory, a symlink to a valid marker and a FIFO
  among them);
- the maintenance marker, valid and malformed, which blocks the boot before
  anything is written;
- lock exclusion checked with a real second process;
- `Close` makes every root unusable and frees the lock;
- step order enforced, and database errors.

Boot-check tests, with the gaps listed in N-063:
- cross-device with `/dev/shm`;
- a nested mount of the same filesystem, using a sibling named volume in the
  gate;
- permissions on each directory;
- probe failures injected.

Mutation-checked: removing the emptiness check, removing the catalog check
or dropping its `blobs`, `artists` or `import_batches` clause (N-069 explains
why the `albums` clause is redundant), adopting on mismatch,
adopting a marker without store id, keeping a leftover temporary, writing the
marker before the database, a lenient UUID parser, ignoring the maintenance
marker, dropping the mount, permission or probe-location checks, and not
releasing the lock. Each of these makes a test fail.

```sh
scripts/check.sh ./internal/volume/...
scripts/dev.sh go test -race -count=20 ./internal/volume/
```

### `cmd/musiclibd` — the server's boot, health and shutdown (§2.3, §6.1, §10.4, §11.1) ✔

`musiclibd` runs the server. `musiclibd healthcheck` queries `/health/ready`
and exits 0 or 1. Any other argument is a usage error (exit 2). The Phase 6
subcommands are not there yet.

Configuration comes from the environment only, and every problem is reported
at once (`config_invalid`, exit 2):
- `DATABASE_URL`: required, parsed by pgx. It is never logged and its parse
  error is withheld (N-064).
- `PUBLIC_ORIGIN`: required. It must be exactly the canonical origin a browser
  sends: http/https, lowercase ASCII host, no default port, no path, not even
  a trailing slash.
- `HTTP_ADDR`: default `:8080`, port 1..65535.
- `WORKERS`: default `max(1, min(4, GOMAXPROCS))` (N-066), plain decimal
  1..16.
- Paths are fixed: `/data` and `/import`.

Before the boot, the server sets umask 022, refuses uid 0 (`run_as_root`) and
logs JSON on stderr.

`boot()` is the §11.1 sequence, one small function per step:

| Step | Now | Phase 2 |
|---|---|---|
| 1 | HTTP serving with negative readiness; `volume.Acquire` | |
| (N-065) | `CheckMaintenance`, before anything touches the database | |
| 2 | pool of `WORKERS + 8`, `Ping` with backoff 250 ms → 5 s until cancelled; `store.Migrate` | |
| 3 | `Identify`, `OpenLayout`, `CheckFilesystem`, `checkTools` (the Runner with `WORKERS` slots; pinned ffmpeg, ffprobe and musiclib-tags, `media_tool_unavailable` / `media_tool_version`), `/import` listable | |
| 4 | — | journal recovery |
| 5 | `blobstore.CleanTemps`, `fsops.RemoveProbeLeftovers(work)` | running → pending; `work/render`, `work/retired` |
| 6 | — | enqueue stale renders |
| 7 | readiness positive | start the worker pool |

- `/health/live` is always 200.
- `/health/ready` is 503 `not_ready` until step 7 and during shutdown. After
  that it pings PostgreSQL on each request with a 2 s timeout, and answers
  503 `db_unavailable` when the database is down (N-070).
- Both endpoints are GET only, answer JSON with `Cache-Control: no-store`,
  and every other route is 404.
- A refused boot logs its stable `code` and exits 1 (N-068).
- On SIGTERM or SIGINT the server stops accepting HTTP (graceful, 10 s),
  closes the pool, then closes the volume's roots and releases the flock,
  last. It exits 0.

Tests use real PostgreSQL 17 and ext4, with no mocks:
- **Readiness lifecycle.** The database refuses connections at first: live is
  200, ready is `not_ready`, the lock is held and nothing is written. Once the
  database is allowed, ready turns 200. When the database goes away, ready is
  `db_unavailable` and live stays 200; when it comes back, ready is 200 again.
- **Shutdown order.** The shutdown events come in order, then the lock is
  free and nothing answers any more.
- **Stop during the wait.** Stopping while waiting for PostgreSQL is a clean
  stop that writes nothing.
- **Restart.** Restarting on the same pair keeps the store id.
- **Refusals.** Maintenance (refused with an unreachable database),
  malformed maintenance, mismatch, new database, lock held, missing `/data`
  and missing `/import`: each gives its code, never turns ready, and releases
  the lock.
- **Step 5 cleanup.** Only temporaries and probe directories are removed.
- **Configuration.** 40 table cases, all problems reported at once, the
  password absent from errors and logs, and the `WORKERS` default.
- **Healthcheck.** Exit codes for 200, 503, 500, a redirect (not followed),
  nothing listening and an invalid `HTTP_ADDR`; loopback substitution.
- **Real child processes.** The server becomes healthy, sets umask 022 over
  an inherited 077, and keeps its lock against the test process and against a
  second server (which exits 1 with `volume_locked`). SIGTERM and SIGINT give
  exit 0 with the lock released last. The database password never appears in
  its logs.

Mutation-checked: the maintenance check moved after the database wait, the
pool closed after the lock, no umask, readiness positive without the
database, readiness never positive, redirects followed, root allowed, and a
`WORKERS` default of 2. Each makes a test fail.

```sh
scripts/check.sh ./cmd/...
scripts/dev.sh go test -race -count=20 ./cmd/musiclibd/
docker compose --profile app up -d --build --wait     # docs/docker.md
```

### `internal/media` — ffprobe/ffmpeg adapter, `AudioDigest` (§6.1, §7.2, §7.6, §8.1, §8.4, §8.5) ✔

The adapter of the native media tools (§2.3). The TagLib helper
(`native/musiclib-tags`) joined it in round 3, through the same Runner: see
the next section.

**Tools in the images (N-073).** FFmpeg 8.1.3 is built from its signed
release tarball, pinned by sha256, fully static, with
`--disable-autodetect` and version `8.1.3-musiclib1`. The same bytes are in
the `test`, `dev` and `runtime` images, and a no-cache rebuild gives the same
sha256. LAME 3.100 is in the toolchain images only, for the MP3 fixtures.

Public API:

| Function | Role |
|---|---|
| `NewRunner(workers)` | the process's one tool runner and the global semaphore of §6.1; built at boot, passed explicitly |
| `Runner.Run(ctx, Command)` | argv only; own process group and `Pdeathsig` SIGKILL; empty environment, cwd `/`; stdin `/dev/null` or explicit bytes; only the given descriptors (3, 4, ...); stdout streamed to a writer, optionally bounded; the first 64 KiB of stderr kept; required timeout; on every outcome the group is killed and waited for |
| `InspectTimeout` / `DecodeTimeout` | 30 s / 30 min (§8.5) |
| `NewTools(ctx, runner, ffmpeg, ffprobe, tags)` | reads the `-version` lines of ffmpeg and ffprobe and requires `PinnedVersion`; reads `musiclib-tags version` and requires `PinnedTagsVersion` and `PinnedTagLibVersion` |
| `Tools.Versions()` | the versions read (`FFmpeg`, `FFprobe`, `Tags`, `TagLib`), inputs of `render_version` (N-080, N-010) |
| `Tools.Probe(ctx, f)` | classification by content: `audio` (with `Format` = `flac`, `mp3`, `m4a-aac`, `m4a-alac`, codec, rate, channels, layout, duration, declared frames), `unsupported_audio` (reason), `no_audio`, `unreadable`; attached pictures counted |
| `Tools.AudioDigest(ctx, f)` | §8.4: probe, full decode to `pcm_f64le`, streamed SHA-256, frames = bytes / (8 × channels), declared length enforced; returns `(SampleRate, Channels, Layout, Frames, PCMSHA256)` |
| `KnownAudioExtensions()` / `HasKnownAudioExtension` | the fixed list of §7.2, for the importer's rule (N-081) |
| `Error` / `Code` | `media_tool_unavailable`, `media_tool_version`, `media_timeout`, `media_canceled`, `media_tool_failed`, `media_output_too_large`, `media_output_invalid`, `media_not_supported`, `media_decode`, `media_io`, `media_invalid_argument`; stderr kept in a field, never in `Error()` (N-082) |

How it maps to DESIGN.md:
- **§8.5, §6.1: the Runner.**
  - The kill sequence is `waitid(WNOWAIT)`, then the group SIGKILL, then the
    reap, then a wait until the group is gone (N-076).
  - A timeout is `media_timeout`.
  - A non-zero exit or a signal is always a failure, whatever the tool
    printed.
- **§7.2, §8.1, §4.2: the probe.**
  - The file reaches the tool only as descriptor 3, through FFmpeg's `fd`
    protocol, the only protocol allowed. Names and extensions never reach
    the tool, and playlists or references cannot be followed (N-075).
  - An M4A counts whatever its brand. Extra non-audio streams are refused.
  - Encryption is detected from packet side data (N-077).
- **§8.4, §7.6: `AudioDigest`.**
  - The command is §8.4's with `-err_detect crccheck+explode` (TO CONFIRM)
    and `-reinit_filter 0` (N-074).
  - A decoded length that differs from the one FLAC STREAMINFO or an MP3
    Xing/Info/VBRI header declares is refused (N-078).
  - The digest is comparable only on the same binary and machine, which §8.4
    requires anyway (N-079).
- **§11.1 step 3:** the boot builds the Runner with `WORKERS` slots and runs
  `NewTools`. A missing or different tool is a fatal boot error with its
  code.

**Cost (N-079):** `AudioDigest` of a 5-minute 44.1 kHz stereo FLAC takes 0.56
s in the dev container. ALAC, AAC and MP3 decode in 0.55 to 0.61 s.

Tests run the real pinned ffprobe, ffmpeg and LAME, on fixtures generated at
test time (lavfi sine, fixed-seed noise, silence; `image/png`). Nothing is
committed.
- **Classification of real files:**
  - FLAC 16 and 24 bit; mono, stereo and 5.1; 48 kHz silence;
  - MP3 CBR, VBR and without a Xing header;
  - AAC and ALAC in M4A;
  - covers in FLAC, MP3 and M4A;
  - rejected: real video, two audio streams, Opus in Ogg, WAV, ADTS AAC, MP2
    in a `.mp3`, CENC-encrypted AAC;
  - no audio: JPEG, PNG, a video without sound;
  - unreadable: empty, text, PDF, M3U, an HLS playlist and concat lists
    naming a real FLAC (never followed);
  - the same bytes under Unicode, NFD, `-i.flac`, `file:` and misleading
    names classify the same.
- **Exact PCM:**
  - the digest of FLAC and ALAC equals the SHA-256 of the source WAV's
    samples computed in Go (16 and 24 bit, mono, stereo, 5.1, 48 kHz),
    frames and parameters included;
  - MP3 with a LAME header decodes to exactly the source's length, CBR and
    VBR;
  - AAC and MP3 digests are identical across runs, including four
    concurrent ones.
- **Invariants:**
  - the same digest after tag changes, a FLAC re-encode at another
    compression level, an added cover, and an M4A re-mux (AAC and ALAC);
  - one changed LSB changes only the hash;
  - the same samples declared as `5.1` or `5.1(side)` differ only in
    `Layout`.
- **Refusals:**
  - FLAC with one flipped bit (at three positions), cut in half, missing its
    last bytes, damaged in the middle, or declaring more samples than it
    has;
  - MP3 CBR and VBR cut in half, damaged, or changing rate and channels
    mid-stream;
  - M4A cut in half (faststart: `media_decode`; moov at the end:
    unreadable), or damaged;
  - ALAC cut in half;
  - unsupported files refused before any decode;
  - decoder failures with a fake ffmpeg: exit 1 after plausible PCM,
    killed, empty output, a partial frame, the wrong length.
- **Runner, on real processes:**
  - a timeout on a real 10-minute decode;
  - cancellation kills and reaps a shell and two children, checked in
    `/proc`, and the group is gone;
  - a straggler is killed after a normal exit;
  - `Run` waits for a zombie member of its group;
  - `Pdeathsig`: a parent killed with SIGKILL takes its tool with it;
  - the semaphore bounds concurrency, measured by the tools themselves;
  - a caller waiting for a slot is cancelled without starting;
  - stderr is kept to 64 KiB and drained;
  - non-zero exits and signals fail;
  - the child has exactly descriptors 0 to 3, with stdin `/dev/null`, no
    inherited flock (with a control that passes the lock on purpose), an
    empty environment and cwd `/`;
  - stdin delivered, and a short read fails;
  - output limit and a failing writer;
  - no descriptor leak across every outcome.
- **Tool versions:** missing, older, unsuffixed, swapped, garbled and failing
  tools are refused, the TagLib helper included (another helper or TagLib
  version, no build suffix, an unknown field, exit 1). The boot refuses a
  missing ffprobe or helper and an ffmpeg or helper of another version, and
  logs the four verified versions.

Mutation-checked: each of the following makes a test fail:
- in the Runner: no own process group; no `Pdeathsig`; no group kill after
  exit; no wait for the group; a semaphore too large; the environment
  inherited; the cwd not `/`; stderr unbounded; a non-zero exit accepted; a
  short stdin read accepted; no output limit;
- in the decoder: no `-reinit_filter 0`; a bare `explode`; no `-err_detect`;
  no `-xerror`; no declared-length, partial-frame or empty-output check; no
  rewind before the decode or the probe; unsupported files decoded;
- in the probe: an estimated MP3 length taken as exact; no encryption,
  video, multistream or other-stream check; AAC accepted outside MP4; any
  failing exit taken as unreadable; EIO taken as unreadable; the undeclared
  layout not marked; no fd-only whitelist;
- in the tools and the boot: no version check; the boot skipping the tools.

```sh
scripts/check.sh ./internal/media/...
scripts/dev.sh go test -race -count=20 -run 'TestRun|TestNewTools|TestProbeToolFailures|TestAudioDigestDecoderFailures' ./internal/media/
scripts/dev.sh go test -run '^$' -bench AudioDigest5Min -benchtime 10x ./internal/media/
```

### `native/musiclib-tags` + the tag adapter (§2.1, §8.1–§8.3, §8.5, §9.1 step 6, §12.2) — FLAC ✔, MP3/M4A Phase 4

The TagLib helper and its Go adapter in `internal/media`. **FLAC is
complete.** Every MP3 and M4A operation answers a typed
`unsupported_format` until Phase 4 (N-094).

**The helper (C++20, `native/musiclib-tags/`).** It knows no domain (§13.2).
It gets JSON on stdin and files as descriptors only: fd 3 the audio file,
fd 4.. the extract destinations or the cover. Its protocol is N-084.

| Source | Role |
|---|---|
| `src/main.cpp` | the four operations (`version`, `inspect`, `extract-images`, `write-managed-tags`), the 256 KiB request limit, the failure protocol (exit 3, `{"code","message"}`, message ≤ 4 KiB) |
| `src/request.{h,cpp}` | typed requests; every key required, `null` = absent |
| `src/json.{h,cpp}` | strict JSON reader (integers, depth 8, unique keys, strict UTF-8) and writer |
| `src/fdio.{h,cpp}` | `checkDescriptor` (regular file, access mode, no `O_APPEND`), `FdStream`: TagLib `IOStream` on pread/pwrite with sticky errors |
| `src/flac.{h,cpp}` | the independent FLAC reader, the analysis, and `writeFlac` (TagLib) with its checks before and after the save (N-085) |
| `src/fields.h` | the constant table of managed fields, aliases, sort keys and picture keys (§8.2, N-088) |
| `src/inspection.{h,cpp}` | the inspection and its JSON; opaque reasons (N-086) |
| `src/text.{h,cpp}`, `src/sha256.{h,cpp}` | UTF-8 validation, base64, SHA-256 |
| `src/version.h` | `kHelperVersion` = `1` |
| `tests/unit_tests.cpp` | the parsers under ASan and UBSan (`make check`, run by the build) |

**Pinned TagLib (N-083).**
- TagLib 2.3.2 and CMake 4.4.3 are pinned by sha256 and built in the
  Dockerfile stage `build-tags`, static, with no zlib.
- The helper is fully static (3 MB) and has the same bytes in the `test`,
  `dev` and `runtime` images. Two `--no-cache` builds gave the same sha256.
- A second, ASan and UBSan, build of TagLib and the helper is in the
  toolchain images only.
- The version is `{"helper":"1","taglib":"2.3.2-musiclib1"}`, checked at
  boot and in the gate.

**Go adapter (`internal/media`).**

| Function | Role |
|---|---|
| `Tools.Inspect(ctx, f, format)` | managed fields by §8.1, as ordered lists; conflicts between canonical key and aliases; pictures; unmanaged fields in canonical form; opaque fields |
| `Tools.ExtractImages(ctx, f, format, targets)` | byte-exact pictures into empty destination files; nothing written unless every index exists |
| `Tools.WriteManagedTags(ctx, f, format, TagValues, *Cover)` | complete rewrite of the managed fields; zero value = removed; totals from the caller; exactly one front cover or no picture at all; aliases, sort keys and picture keys removed; unmanaged fields kept, the file not rebuilt |
| `VerifyTags(want, cover, before, after)` | §9.1 step 6: managed values and cover as asked, nothing opaque (and nothing blocking before), unmanaged fields identical: `media_tags_verification` otherwise (§12.2) |
| `Inspection.Blocking()` | the opaque fields that make a write fail (§8.3) |
| `JoinValues`, `TagNumber`, `TagBool` | "; "-join of multi-valued text (§7.3); strict parsing of numbers (never truncated) and of the compilation flag |
| `media_tags_*` codes | one per helper failure code, plus `media_tags_verification` |

**How it maps to DESIGN.md.**
- **§8.1: reading.** The canonical key wins, then the aliases in table
  order, then the `/M` of `N/M`. A disagreement is a conflict. TagLib's
  property map and implicit precedences are never used: the reader is the
  helper's own (N-085).
- **§8.2: writing.** Only canonical keys are written, through explicit
  names. The table removes `ALBUM ARTIST`, `ALBUM_ARTIST`, `TRACKNUM`,
  `TOTALTRACKS`, `TOTALDISCS`, `YEAR` and the four sort keys (N-088). A
  compilation is written as `1`, and false removes it (N-089). Pictures are
  wholly managed (N-087).
- **§8.3: unmanaged fields.** No rebuild of the file and no clearing of the
  map: the writer removes only the table's keys.
  - What TagLib would lose is refused before writing (`opaque_field`), or
    removed when it is managed anyway.
  - Two checks surround the save: TagLib's map against the expected one
    before, a re-read after.
  - The output is byte-identical for the same input.
- **§8.5:** the 30 s limit, the Runner's process group, and descriptors
  only.

**Tests** (real pinned helper and ffmpeg, fixtures made at test time by
lavfi, `image/png`, and an independent Go FLAC codec, `flacmeta_test.go`):
- **Contract:**
  - no tags;
  - every managed field, multi-valued fields, keys in any case;
  - aliases, "canonical wins" and conflicts;
  - sort keys removed, `COMPOSERSORT` kept;
  - Unicode and NFD byte-exact, astral characters;
  - unmanaged fields preserved (ReplayGain, COMPOSER, COMMENT ×2, custom
    keys with `=`, lyrics with CR/LF, APPLICATION blocks, STREAMINFO);
  - five pictures in blocks and comments, listed, extracted byte-exact,
    replaced by a JPEG or PNG front cover or removed;
  - a write that only removes;
  - idempotence, byte-identical;
  - determinism over three copies;
  - a shrinking write;
  - inspect writes nothing;
  - MP3 and M4A refused, a FLAC declared as MP3 refused;
  - the probe fixtures' JPEG cover (ffmpeg's, type 0) and the new PNG cover
    fixture, extracted byte-exact.
- **Audio integrity:** every successful write in the tests goes through
  `writeChecked`:
  - AudioDigest before and after must be equal;
  - the audio frames must be byte-identical;
  - `VerifyTags` must pass.

  `TestTagsAlteredSamplesAreCaught` shows that a changed sample passes every
  tag check and fails only the digest comparison.
- **Hostile input, every case on the release and the ASan/UBSan helper:**
  - 12 corrupt structures;
  - 4 format mismatches;
  - 23 opaque-field cases (refused, or removed and written);
  - the field-count limit at 50,000;
  - covers of 16 MiB and 16 MiB − 20 B, and a comment at the block limit;
  - 55 raw requests (size limit and exactly 256 KiB, malformed JSON,
    unknown or missing keys at every level, wrong types, NUL, surrogates,
    ranges, floats, unknown format or operation, a 100 KB format name);
  - 17 descriptor cases (FIFO under a 5 s guard, directory, `/dev/null`,
    no fd, wrong access modes, `O_APPEND`, non-empty or missing
    destinations);
  - a missing picture leaves every destination empty;
  - EFBIG in the middle of a write gives `media_tags_io`.

  Every refusal leaves the file byte-identical, mtime included.
- **Adapter:**
  - failure mapping (exit status, strict stderr object, unknown code,
    invalid UTF-8, trailing data, a signal);
  - timeout and cancellation with a fake helper;
  - `VerifyTags` with 29 mutations;
  - `TagNumber`, `TagBool`, `JoinValues`;
  - cover attributes by libFLAC's rule for 10 image kinds, written into the
    block;
  - invalid arguments refused before the helper runs.
- **Boot:** the four versions logged; a missing helper or a helper of
  another version refuses the boot.

**Mutation-checked**, each makes a test fail:
- in the helper:
  - no second save;
  - no second save and no read-back: the Go `VerifyTags` fails instead;
  - `moveRange` off by one, back to front and front to back;
  - `refuseOpaque` off;
  - `TOTALDISCS` dropped from the table;
  - no field-count limit;
  - no failure-message bound;
  - no regular-file check;
  - no UTF-8 check of values: the TagLib cross-check fails;
  - no cover block-size check: the read-back fails;
- in Go:
  - `decodeStrict` without its UTF-8 check;
  - `tagsFailure` without its exit-3 check;
  - `VerifyTags` without the unmanaged comparison, or without the check of
    blocking fields before the write;
  - the digest comparison of the §9.1 check off.

```sh
scripts/check.sh ./internal/media/...
scripts/dev.sh go test -race -count=20 -run 'TestTags|TestVerify|TestDescribe|TestNewTools' ./internal/media/
docker build --target build-tags .           # the helper and its unit tests alone
```

**Open for the owner:** N-090 (ID3 in FLAC), N-091 (covers between 16 and
20 MiB in FLAC), N-092 (invalid UTF-8 in unmanaged fields).

---

## Decisions made during implementation

1. **`internal/names` package.** §2.3 does not assign a package to normalization,
   but §13.2 requires a single implementation and its consumers are `catalog`,
   `importer` and `render`: so it lives in its own pure package, with no I/O and
   no DB.
2. **Two entry points for segments.** `Segment` for directories, `FileSegment` for
   files: only the latter preserves the extension when truncating. A single
   function would have had to guess whether `Kind of Blue (feat. J.C.)` has an
   extension.
3. **Oversized extension.** If the extension leaves no room for the stem, it is
   not preserved, rather than producing a name over 180 bytes.
4. **Pinned versions.** `golang.org/x/text` is pinned to `v0.41.0` (§2.1: never
   `latest`); it is the last version compatible with the `go 1.25.0` directive.

## Deviations from the spec

1. **Control characters in segments.** §5.2 lists `/ \ : * ? " < > |` as the
   characters to replace with `_`. Control characters were added and are
   replaced the same way: they cannot appear in an output file name or in an
   HTTP header. §5.2 already rejects them in metadata texts.
2. **NUL byte in relative paths.** `SplitRelPath` rejects it explicitly
   (`path_nul_byte`) instead of letting the syscall fail.
3. **Trimming outer dots on both sides.** "Trim outer spaces/dots" is applied
   literally, so an imported file `.hidden` materializes as `Extras/hidden`.
   The content is kept, the name is not. It is also the reason why an
   attachment cannot create hidden files in the output.
4. **Cherokee case folding fix.** `cases.Fold()` in x/text v0.41.0 is not
   idempotent on that script: `fold(U+ABB8) = U+13E8` and `fold(U+13E8) =
   U+ABB8`, whereas `CaseFolding.txt` maps `AB70..ABBF -> 13A0..13EF` and
   `13F8..13FD -> 13F0..13F5`. Without the fix, two albums differing only in
   case would get different `folder_key` values and occupy two `path_claims`
   rows. `Key` applies the correct mapping after folding; the bug was found by
   fuzzing, and the absence of other cases is verified by enumerating every
   code point.

## Open questions for the spec

See `NOTES.md`. The most urgent ones, because they change keys already written
to disk:

- **N-002** trimming the leading dot: `.hidden` becomes `hidden`. To be
  confirmed **before** the first real import.
