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
- [~] `native/musiclib-tags`: C++ TagLib helper (inspect / extract-images / write-managed-tags) (§8.1) — **FLAC complete**: pinned static TagLib 2.3.2 (N-083), the three operations, the Go adapter `Inspect` / `ExtractImages` / `WriteManagedTags` and the §9.1 step 6 check `VerifyTags`, ID3 tags in a FLAC stripped by a declared rule (N-090); MP3 and M4A answer a typed `unsupported_format` until Phase 4 (N-094)
- [~] Managed tag mapping and alias removal (§8.2, §8.3) — **FLAC complete** (table in `native/musiclib-tags/src/fields.h`, N-088); the MP3 and M4A tables are Phase 4 (N-094)
- [x] `internal/importer`: import of a single album candidate (§7.1–§7.6) — batch creation (`catalog.CreateImportBatch`), the scan executor (§7.2 rules 1, 4 and 5; rules 2 and 3 fail as `multidisc_not_supported_yet` until Phase 5, N-120), the import executor of one FLAC candidate (revalidation, source stability, space check, verified copies, full decode, tags, metadata, LRC, cover, fingerprint, commit); MP3 and M4A fail as `audio_format_not_supported_yet` until Phase 4. The executors (`ExecuteScan`, `ExecuteImport`) have the form `jobs.Pool` expects and are **not wired** into `cmd/musiclibd`: the pool starts in the publish round with the journal recovery (N-107, N-125); only `importer.CleanWork` joined boot step 5
- [x] `internal/catalog`: domain transactions, revisions, reservations, enqueue (§4.3, §5.3) — import commit (§7.6), `PUT` album semantics, trash/restore, artist rename, `path_claims`, `CheckFresh`; the transaction runner and the catalog lock in `internal/store` (N-095)
- [ ] `internal/render`: snapshot → pure plan → build in staging (§9.1)
- [ ] `.musiclib.json` receipt (§9.2)
- [ ] `internal/publish`: PREPARE / INSTALL / FINALIZE + journal (§9.3)
- [ ] Journal recovery at startup (§9.4)
- [ ] Boot steps 4 (journal recovery), 5 (running jobs back to pending, cleanup of `work/render` and `work/retired`), 6 (stale renders) and the worker pool of step 7, in the places marked in `cmd/musiclibd` `boot()` (§11.1); exit on database loss once workers exist (§6.4, N-070). The helpers of steps 5 and 6 and the pool exist and are tested (`jobs.RecoverRunning`, `jobs.EnqueueStaleRenders`, `jobs.Pool`); the wiring waits for step 4 and `render_version` (N-107)
- [x] `internal/jobs`: claim, pool, completion (§6.2, §6.4) — the single `EnqueueRender`, the REPEATABLE READ claim with its snapshot, ticket-conditioned completions, boot helpers, the worker pool

## Phase 3 — Concurrency

- [x] Render coalescing on a single row per album (§6.3) — `jobs.EnqueueRender`, ticket-conditioned completions
- [x] `REPEATABLE READ` snapshots (§6.2) — `jobs.ClaimNext`, `jobs.RenderSnapshot`
- [x] `path_claims` and global `pg_advisory_xact_lock` (§5.3) — `store.InCatalogTx`, `catalog.ReconcileClaims`
- [~] Conditional APIs: strong ETag, `If-Match`, 412/428 (§10.1) — the revision check in the transaction of the change, with typed `precondition_required` / `precondition_failed` (catalog); the HTTP ETag and headers come with the API
- [~] Artist rename and album reassignment (§4.3) — both done and tested in the catalog (`RenameArtist`; `UpdateAlbum` with another `artist_id`); the HTTP endpoints come with the API
- [ ] Named failpoints and failure matrix (§12.2)

## Phase 4 — Formats and content

- [ ] MP3 (ID3v2.4, APE, ID3v1 migration), M4A AAC/ALAC (§8.1–8.3) — in `native/musiclib-tags`: independent MP3 and M4A readers, the writers, their alias and sort tables, and the ID3v1 exclusion in `VerifyTags` (N-094); probe and `AudioDigest` already handle both
- [~] Covers: selection, limits, upload, removal (§7.4, §8.5) — the import selection and its limits are done (`internal/importer`, the N-091 limit in `media.EmbeddedCoverFits`); upload and removal come with the API
- [~] LRC files associated with tracks (§7.4) — at import, done (N-117); assignment and upload with the API
- [~] Attachments under `Extras/` (§5.1, §7.4) — collected at import; their materialization is the renderer's
- [~] Verification and preservation of unmanaged tags (§8.3) — FLAC done (`media.VerifyTags`, N-086, the ID3-in-FLAC exclusion N-090); MP3 and M4A with their readers (N-094)

## Phase 5 — Full experience

- [~] Recursive scan and multi-disc grouping (§7.2) — the recursive scan with rules 1, 4 and 5 and the unassigned-file report are done; rules 2 and 3 (multi-disc) fail explicitly until this phase (N-120)
- [~] Inferred initial metadata and import overrides (§7.3) — the whole table and the overrides are applied by the importer (tested through a retry done by SQL); disc directories come with rules 2 and 3, the retry endpoint with the API
- [x] Fingerprinting and duplicate detection (§7.6) — the fingerprint in `internal/importer` (golden test), duplicates `skipped` by the import commit
- [ ] Complete HTTP APIs (§10.2)
- [ ] UI: Library, Album, Import, Activity (§10.3)
- [ ] HTTP security boundary: `PUBLIC_ORIGIN`, `X-Musiclib-Request` (§10.4)
- [~] Trash, restore, retry (§4.3, §6.4) — trash and restore in the catalog; retry with the HTTP API

## Phase 6 — Operations

- [ ] `doctor`, normal and `--deep` (§11.3)
- [ ] `rebuild` with maintenance marker (§11.3)
- [ ] `backup` / `restore` (§11.4)
- [~] Space budget and `statfs` check (§11.2) — the import's estimate and `statfs` check with the 1 GiB margin; the process-wide budget is the executor/pool round's (N-114)
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
  - The command is §8.4's with `-err_detect crccheck+explode` (confirmed by
    the owner) and `-reinit_filter 0` (N-074).
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
| `src/version.h` | `kHelperVersion` = `2` (`1` refused ID3 tags in a FLAC; `2` strips them, N-090) |
| `tests/unit_tests.cpp` | the parsers under ASan and UBSan (`make check`, run by the build) |

**Pinned TagLib (N-083).**
- TagLib 2.3.2 and CMake 4.4.3 are pinned by sha256 and built in the
  Dockerfile stage `build-tags`, static, with no zlib.
- The helper is fully static (3 MB) and has the same bytes in the `test`,
  `dev` and `runtime` images. Two `--no-cache` builds gave the same sha256.
- A second, ASan and UBSan, build of TagLib and the helper is in the
  toolchain images only.
- The version is `{"helper":"2","taglib":"2.3.2-musiclib1"}`, checked at
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
  - **Declared exception (N-090):** ID3v2 and ID3v1 tags inside a FLAC are
    stripped. The inspection reports them as removed opaque fields, never
    blocking; the read-back and `VerifyTags` require that none is left. The
    output is the one the same file without its ID3 tags would give, byte
    for byte.
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
  - ID3 in FLAC (N-090): a leading ID3v2 (with frames that have no Vorbis
    equivalent), a trailing ID3v1, both, a footer, a size of 0, a garbage
    body, garbage frames, and an 800 KB tag that moves TagLib's padding
    choice; on both helpers each is stripped, the samples are those of the
    file without ID3, the output is byte for byte the reference's, and it is
    deterministic and idempotent. A file with no comment block gets no ID3
    value copied into the new one.
- **Audio integrity:** every successful write in the tests goes through
  `writeChecked`:
  - AudioDigest before and after must be equal;
  - the audio frames must be byte-identical;
  - `VerifyTags` must pass.

  `TestTagsAlteredSamplesAreCaught` shows that a changed sample passes every
  tag check and fails only the digest comparison.
- **Hostile input, every case on the release and the ASan/UBSan helper:**
  - 17 corrupt structures, among them an ID3v2 larger than the file (with
    and without a footer), an ID3v2 version or revision of 0xFF, and an
    ID3v1 inside the metadata blocks;
  - 4 format mismatches;
  - 21 opaque-field cases (refused, or removed and written);
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
  - `VerifyTags` with 32 mutations (three for the ID3 tags);
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
  - no ID3 strip: the read-back fails; no strip and no read-back: the Go
    `VerifyTags` fails;
  - no second save after a strip: the output of the 800 KB ID3v2 differs
    from the reference;
  - no 0xFF version check: the TagLib extent cross-check fails the
    inspection (internal, not corrupt); no ID3v1 overlap check;
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

**Owner decisions of 2026-09-23:**
- N-091 (a per-format embeddable cover limit, checked when a cover is chosen)
  and N-092 (the importer refuses invalid UTF-8 and other blocking unmanaged
  fields) are done since round 5: `media.MaxEmbeddedCover` /
  `EmbeddedCoverFits`, and the importer.
- N-090 (ID3 in FLAC stripped by a declared rule) is done: the importer
  accepts such files with a warning (round 5), and the helper strips the
  tags at render since helper version `2` (round 6). A FLAC with a trailing
  ID3v1 is still refused at import by the full decode (N-128).

### `internal/store` — transaction runner and catalog lock (§5.3, §6.2, §6.4) ✔

The only way the queue and the catalog open a transaction (N-095).

| Function | Role |
|---|---|
| `InCatalogTx(ctx, pool, fn(*CatalogTx) error)` | READ COMMITTED; first statement `pg_advisory_xact_lock` with the constant key `mlcatalg` (§5.3), distinct from `mlmigrat` |
| `CatalogTx` | the query methods of a `*Queries` held in an unexported field, so only `InCatalogTx` can build a usable one: every catalog or queue write takes one, so the single lock is enforced by the compiler (N-095) |
| `InSnapshotTx(ctx, pool, fn(*Queries) error)` | REPEATABLE READ without the lock: the claim and its snapshot (§6.2, N-096) |
| `IsFatal(err)` | `store_commit_uncertain` or `store_connection_lost`: the process must stop and restart (§6.4) |
| `CodeCommitUncertain`, `CodeConnectionLost`, `CodeRetriesExhausted`, `CodeCanceled` | the runner's stable codes |
| `pgtest.NewProxy` | a TCP proxy that loses a COMMIT's answer, cuts before a COMMIT, or cuts every connection, against the real server (N-108) |

- 40001 and 40P01 rerun the whole short transaction, at most three more times,
  with jitter; then `store_retries_exhausted` (not fatal).
- No answer to a COMMIT is always `store_commit_uncertain`, never retried:
  pgx's `SafeToRetry` is wrong there (N-109). The COMMIT runs to its end even
  if the caller is cancelled. An answer that ends the session (severity
  FATAL or PANIC, such as 57P01) is uncertain too (N-113).
- A lost connection before the COMMIT is `store_connection_lost`; the
  connection state is read before the rollback releases it (N-111).
- sqlc: `sql/catalog.sql` and `sql/jobs.sql`; `COPY` for tracks and
  attachments. No schema change: `00001` is untouched.

Tests on real PostgreSQL 17, the wire failures through the proxy:
- the key is held from the first statement, another session cannot take it,
  a second catalog transaction waits for the first (seen in `pg_locks`), and
  the migration lock is independent;
- a real 40001 (a commit after the snapshot, then `FOR UPDATE`) runs the
  transaction twice, and the second run sees the commit;
- a real deadlock between two transactions runs the victim again (3 runs);
- four runs, then `store_retries_exhausted` keeping the 40001;
- other errors (a Go error, 23505) are returned after one run;
- a deferred 23505 at COMMIT is an ordinary error, not fatal;
- a COMMIT blocked on a deferred unique check whose backend is terminated
  (`pg_terminate_backend`, FATAL 57P01): uncertain, fatal, run once
  (mutation-checked);
- a lost COMMIT answer: uncertain, the row durable; a cut before the COMMIT:
  uncertain, nothing durable; a cut in the middle: connection lost, nothing
  durable; all fatal and run once;
- an unreachable database: connection lost, `fn` never called;
- cancellation before the begin and while waiting for the lock:
  `store_canceled`, not fatal; a cancellation after `fn` returned: committed;
- pgx's wrong `SafeToRetry` after a sent COMMIT, pinned.

```sh
scripts/check.sh ./internal/store/...
scripts/dev.sh go test -race -count=20 ./internal/store/...
```

### `internal/jobs` — the durable queue (§6, §11.1 steps 5–6) ✔

No job framework (§2.3): a job is a row, an attempt is `{JobID, Ticket}`, and
the work is a function supplied by the caller.

| Function | Role |
|---|---|
| `EnqueueRender(ctx, *CatalogTx, albumID)` | the single §6.3 upsert: `requested = nextval`, running stays running with its claim, anything else pending, error cleared, `queued_at = now` (N-098) |
| `ClaimNext(ctx, pool, renderVersion)` | one REPEATABLE READ transaction: render, then scan, then import (§6.1), each by `queued_at, id`, `FOR UPDATE SKIP LOCKED`, `claimed = requested`; a render also loads its `RenderSnapshot` in the same transaction (§6.2) |
| `RenderSnapshot` | the immutable value the planner will consume: artist, album (published state included), cover, tracks, attachments; no absolute path |
| `FinishRender` / `RequeueRender` / `FailRender` | §6.4 completions of a render, conditioned on id, `running` and the claimed ticket; leaving running clears `claimed` |
| `Finish(ctx, tx, kind, attempt, Result)` | the outcome of a scan or an import (`done`, `skipped`, `failed`), validated for the kind |
| `LockStatus` / `Status.Runs` | the row locked for the import commit and for PREPARE |
| `RecoverRunning`, `EnqueueStaleRenders(renderVersion)` | §11.1 steps 5 and 6 (not wired yet, N-107) |
| `NewPool(pool, workers, renderVersion, Executor, log)`, `Pool.Run`, `Pool.Wake` | `WORKERS` goroutines (1..16), a 2 s poll plus the wake-up signal, claims one at a time (N-110), context shutdown, the first fatal error stops every worker and is returned |
| `Warning`, `Overrides`, `EncodeWarnings`, `DecodeWarnings`, `DecodeOverrides` | closed types of `warnings` and `overrides` (§4.2, N-105) |
| `Error` / `Code` | `job_attempt_stale`, `job_not_found`, `job_invalid_result`, `job_invalid_overrides`, `job_invalid_argument`, `job_db`; a fatal store code wins |

Tests on real PostgreSQL 17 (albums as SQL fixtures):
- 8 concurrent claimers on one job, 25 rounds: exactly one claim each round;
  60 jobs and 8 claimers: each claimed exactly once;
- the priority render, scan, import, then `queued_at`, then id;
- coalescing: running keeps its claim while `requested` moves; the old
  attempt requeues and can complete nothing more; the new one deletes;
- every render completion with the current ticket and with a newer request,
  and every completion with a wrong ticket or after the attempt ended
  (`job_attempt_stale`, row unchanged); a failed render reactivated by an
  enqueue; the error message clipped on a rune boundary;
- `Finish` for 17 results, valid and invalid, with a wrong ticket and twice;
- the snapshot of a full album equals the expected value although a change
  of album, tracks and attachments was committed in the middle of the claim;
- a real 40001 inside the claim reruns only the claim transaction;
- the boot helpers: running to pending, the others untouched; stale and
  never-published albums enqueued; current, failed, queued and trashed left
  alone;
- the pool: Wake with an hour's poll, the 2 s poll, 120 jobs on 16 workers
  each run once with no 40001 between the workers (pgx tracer), shutdown
  reaching the running job and no goroutine left, a fatal executor error and
  a lost database stopping every worker;
- the closed types, strict both ways.

```sh
scripts/check.sh ./internal/jobs/...
scripts/dev.sh go test -race -count=20 ./internal/jobs/
```

### `internal/catalog` — domain transactions (§4, §5, §7.6, §10.1) ✔

Every mutation runs in `store.InCatalogTx`; every output change bumps the
revision, re-derives the claims and enqueues the render in the same
transaction (§3.2 guarantee 6).

| Function | Role |
|---|---|
| `New(pool, wake, CoverFits)` | the service; `wake` is `Pool.Wake`; `CoverFits` is the N-091 question, required |
| `CommitImport(ctx, ImportCandidate)` | §7.6: the job not already completed (else its outcome, `AlreadyCompleted`); a known fingerprint makes it `skipped`, trashed or not; blobs, artist, album at revision 1, tracks, attachments, claims, render, job `done`; a domain rejection makes it `failed` and writes nothing else (N-102, N-103, N-106) |
| `UpdateAlbum(ctx, id, ifMatch, AlbumUpdate)` | `PUT /api/albums/{id}`: exactly the current tracks, numbers permuted freely (deferred unique), no bump on a no-op, folder and reservation checks, another artist allowed |
| `TrashAlbum` / `RestoreAlbum` | §4.3; the restore checks the folder and the reservations |
| `RenameArtist(ctx, id, ifMatch, name)` | §4.3: the artist and every album bumped, every path reserved, all or nothing; no implicit merge |
| `ReconcileClaims(ctx, *CatalogTx, albumID)` | the one claims function (§5.3), for PREPARE, FINALIZE and rebuild too: the union of desired (if active), journal and published paths on normalized keys; `path_reserved` names the owner |
| `AlbumPath(artist, title)` | the desired path `<artist>/<album>` (§5.1) and its key, the two `folder_key`s joined |
| `CheckFresh(ctx, *CatalogTx, snapshot, renderer)` | the four conditions of §6.3 for PREPARE (N-097, N-112) |
| `Error{Code, Message, Details}`, `Code`, `AsError` | stable codes for `{code, message, details}` (§10.1); `Details` carries the owning album, the path, both names, the current revision |

Tests on real PostgreSQL 17:
- **import commit:** every row written, as expected; the same commit twice
  and 8 times concurrently (one album); two jobs with one fingerprint
  concurrently (one done, one skipped); a lost COMMIT answer through the
  proxy, then the retry (`AlreadyCompleted`, one album, one render); a cut
  before the COMMIT, then the boot's recovery and a new attempt; duplicates
  in and out of the trash; the same bytes under another fingerprint
  (blobs deduplicated); a folder conflict, casefold included, then a retry
  with another title; the artist casefold and NFC rule; a sanitization-only
  collision naming both artists; 42 validation cases and the exact limits;
  recorded blobs (size, format, role, a format learned); stale attempts;
  the cover asked for each audio format;
- **8 concurrent imports of one new artist and title:** one done, seven
  `album_folder_conflict`, never a database error (the lock's mutation test);
- **revisions:** 428 and 412 with the current revision; a normalized no-op
  without bump, render or wake-up; a change bumping and enqueueing; the old
  unpublished path released; a refused change leaving no bump, render or
  claim;
- a swap and a three-way rotation of track numbers, one moved to disc 2;
- the album moved to another artist, with a folder conflict first;
- trash and restore, a conflict on restore, a rename in the trash;
- the claims union through nine states (published, case-only rename, rename,
  journal, trashed during the publication, FINALIZE, removal, restore); the
  §5.3 rename example (B gets `path_reserved` naming A until A's old path is
  retired); stray claims released, another album's claims never touched;
- the artist rename, all or nothing, trashed albums included; conflicts by
  casefold and by sanitization;
- `CheckFresh`, each condition alone; fatal codes winning over domain codes.

Mutation-checked, across the three packages: without the catalog lock, the
ticket or `requested` conditions of each completion, the running case of the
enqueue, the published path in the union or its preference order, the
release rule (never, or also inside the union), the no-op detection,
REPEATABLE READ, the pool's claim mutex, the uncertain commit, the retry
count, the shape-only overrides decode, the fatal-code precedence, the cover
check, the restore's folder check, the fingerprint lookup, the terminal-job
check, the sanitization conflict, the claim order, two `CheckFresh`
conditions, the claimed ticket, and the connection read before the rollback
(race detector): each makes a test fail.

```sh
scripts/check.sh ./internal/catalog/...
scripts/dev.sh go test -race -count=20 ./internal/store/... ./internal/jobs/ ./internal/catalog/
```

### `internal/importer` — scan and import of one candidate (§7.1–§7.6, §8.5, §11.2) ✔ (FLAC)

This is Phase 2's FLAC slice. The scan groups a batch into candidates; the
import turns one candidate into the closed input of `catalog.CommitImport`.
`/import` is reached only through a `source` type that can stat, list and
open for reading, and nothing else. Every order is explicit, never the
filesystem's.

| Function | Role |
|---|---|
| `New(Config)` | the importer, over the catalog, the tools, the blob store, `/import` and `/data/work`; creates `work/import` |
| `ExecuteScan(ctx, *jobs.Claim)` | a scan job (§7.2): walk, group, then every import job and the scan's outcome in one transaction (`catalog.CommitScan`) |
| `ExecuteImport(ctx, *jobs.Claim)` | an import job (§7.1–§7.6), ending in `catalog.CommitImport` |
| `CoverFits` | N-091 in the form `catalog.New` expects (`media.EmbeddedCoverFits`) |
| `CleanWork(ctx, work)` | boot step 5: empties `work/import` |
| `MaxTracks`, `MaxFiles`, `MaxCoverBytes`, `MaxCoverPixels` | §7.2 and §8.5, pinned |
| `VariousArtists`, `UnknownArtist` | the automatic artists of §7.3 |
| `Error` / `Code` | stable codes (table below); never an absolute path |

Both executors have the form `jobs.Pool` expects. Every non-fatal outcome
ends in a completion (N-107). A shutdown or a fatal store error returns
without one, and the job stays running until the boot recovers it.

**Next to it:**
- `catalog.CreateImportBatch` (§7.1: the same UUID and root give the same
  batch; another root is `import_batch_conflict`; the root is validated as
  in §5.2 and kept as on disk), `GetImportBatch`, `CommitScan`, `FailJob`,
  and the exported `StemKey`;
- `jobs.EnqueueScan`, `jobs.EnqueueImport`, and five warning codes;
- `media.MaxEmbeddedCover` and `EmbeddedCoverFits`;
- `fsops.Describe`;
- four sqlc queries in `sql/jobs.sql`.

**How it maps to DESIGN.md:**
- **§7.2 scan.**
  - The walk is recursive, uses lstat and getdents only, and is sorted by
    bytes.
  - It ignores exactly `.DS_Store`, `Thumbs.db` and `desktop.ini`, ASCII
    case-insensitively. Hidden files are kept.
  - Symlinks, special files and invalid names are rejected and never
    opened.
  - Audio is decided by the rule of N-115.
  - Rules 1, 4 and 5 apply; the rule 2 and 3 shape fails as not supported
    yet (N-120).
  - The limits are checked on the tree.
  - Unassigned files and rejected entries become warnings.
  - With no valid candidate, the scan is failed with an explanation (N-122).
- **§7.1, §7.2 import.**
  - The candidate is revalidated with the scan's own functions.
  - Its identity snapshot is taken, then the statfs check (N-114) runs.
  - Every file is copied through `blobstore.Put`, after an fstat identity
    check.
  - Everything is read again from the copies.
  - The candidate is walked again before the commit (N-126).
- **§7.2 classification, on the copies.**
  - FLAC is a track. MP3 and M4A are `audio_format_not_supported_yet`.
  - Other audio is `unsupported_audio`.
  - No audio with a known audio extension is `corrupt_audio`; without one,
    it is an attachment.
  - Audio in a subdirectory is `ambiguous_candidate`.
- **§7.6, §8.4:** `AudioDigest` decodes every track completely. A failed
  decode is `corrupt_audio`.
- **N-092, N-090:** a field from `Inspection.Blocking()` is
  `unrenderable_tag`, naming the file and the field. An ID3 tag in a FLAC,
  which the helper reports as removed, is accepted with the warning
  `flac_id3_tag`; a render strips it. A trailing ID3v1 makes the full decode
  fail first (N-128).
- **§7.3:** the whole table, as a pure function (`inferMetadata`): album,
  album artist, track artist, title, disc, numbers, year, genre (N-004),
  compilation, `mixed_album`, multi-valued text, overrides. The details are
  N-116, N-119 and N-121. Track artists equal under §7.6 (NFC, trim,
  casefold, `catalog.SameArtistName`) are one artist, and a track artist
  equal to the album artist that way inherits it (N-121, owner decision).
- **§7.4:** the LRC association (N-117); the cover selection and its limits
  (§8.5, N-091, N-124). An external cover also stays an attachment.
- **§7.6:** the fingerprint: compact JSON with no HTML escaping and no
  newline, sorted by path bytes, SHA-256.

**Codes:**

| Code | When |
|---|---|
| `source_not_found`, `source_not_directory`, `source_rejected_entry` | the root or the candidate is missing, is not a directory, or holds or goes through a symlink or special file |
| `source_changed` | §7.1 |
| `ambiguous_candidate` | rule 4 |
| `multidisc_not_supported_yet` | rules 2 and 3, until Phase 5 |
| `not_a_candidate` | no direct audio any more |
| `no_valid_candidate` | a scan with nothing to import |
| `insufficient_space` | §11.2 |
| `corrupt_audio`, `unsupported_audio`, `audio_format_not_supported_yet` | §7.2, §8.1 |
| `unrenderable_tag` | N-092 |
| `mixed_album`, `ambiguous_album_artist`, `album_title_missing` | §7.3 (an override resolves each) |
| `invalid_tag` | a tag that is not a valid text |

The catalog's own codes also apply: `too_many_files`, `invalid_disc`,
`invalid_track_number`, `lyrics_association`.

**Tests** use real PostgreSQL 17, real ext4 and the real pinned ffmpeg,
ffprobe and musiclib-tags. The FLAC fixtures are generated by ffmpeg, with
their metadata written by an independent Go codec in the tests. After every
job, every source entry's bytes, inode, mode and mtime are compared, and
`work/import` must be empty.
- **Albums:**
  - a normal album: tags, cover, LRC, nested and Unicode attachments, a
    hidden file kept, the three ignored names ignored, and the fingerprint
    recomputed independently;
  - untagged files;
  - `mixed_album` and `ambiguous_album_artist`, each resolved by an override
    on retry;
  - Various Artists and compilation;
  - multi-valued tags, the year tie, the genre tie (N-004) and the genre
    overrides;
  - out-of-range numbers.
- **LRC:** matched, not UTF-8, ambiguous, unmatched.
- **Owner decisions:** N-092 refused; N-090 accepted (and a trailing ID3v1
  refused by the decode, N-128).
- **Formats:** a corrupt FLAC, text named `.mp3`, M4A, WAV, and a FLAC
  without an extension.
- **Layouts:** multi-disc; an ambiguous branch next to a good one, with the
  unassigned files and a symlink reported; a symlink, a FIFO and a socket
  inside a candidate (the device case needs CAP_MKNOD, N-127); root failures;
  revalidation after the scan; `/import` itself as a candidate.
- **Covers:**
  - the external order and its tie by key;
  - the embedded most frequent front cover, its tie by hash, type 0, and an
    invalid front cover;
  - a real ffmpeg-muxed attached picture;
  - `cover.jpg` that is not a JPEG, a PNG that does not fit a FLAC block
    (N-091), a file over 20 MiB, over 40 Mpixel, and no valid cover.
- **Stability:** content, mtime only, a file added, removed, or replaced
  before or after its copy, each injected at a named point.
- **Idempotence:**
  - the batch UUID repeated, including eight concurrent requests;
  - the scan repeated after a crash, and a stale scan commit;
  - the same import attempt run twice;
  - an identical re-import, which is skipped.
- **Pool:** two workers on two batches of the same three albums give three
  albums, each duplicate skipped.
- **Limits:** 10,001 real files; the exact limits on synthetic trees.
- **Shutdown and staleness:** a cancellation leaves the job running; a stale
  attempt changes nothing; space; `CleanWork`.
- **Pure tests:** natural order, grouping, disc names, ignored names, the
  §7.3 table (29 cases, seven of them for N-121), `tagWarnings`, the golden
  fingerprint, LRC, UTF-8, image
  validation, the candidate orders, snapshots.

**Mutation-checked:** each of the following makes a test fail:
- in the natural order: digits compared as bytes, no path tie-break, leading
  zeros counted;
- in the fingerprint: HTML escaping, no sort, a trailing newline;
- in stability: no final recheck; no final recheck and no open-time identity
  check; mtime not compared;
- in the metadata: the genre or the year tie going to the largest, the track
  artist kept when equal to the album artist;
- N-121: the track artists compared by bytes instead of §7.6's identity,
  the spelling tie going to the largest, the track-artist override compared
  by bytes;
- in the covers: `folder.*` before `cover.*`, the tie by bytes only, the
  front covers by ascending count, no N-091 check, no pixel limit, no full
  decode;
- N-090: an ID3 tag warned about without being declared removed; the
  blocking fields not refused;
- elsewhere: Unicode folding of the ignored names, N-090 not excepted, N-092
  not refused, no probe at the scan, no disc layout detection, the LRC UTF-8
  check off, rejected entries ignored in candidates, the import and scan
  inserts without ON CONFLICT, and the batch conflict unchecked.

The open-time identity check alone survives, by design (N-126).

```sh
scripts/check.sh ./internal/importer/...
scripts/dev.sh go test -race -count=10 ./internal/importer/
```

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
