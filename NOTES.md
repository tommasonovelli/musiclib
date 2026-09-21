# Log of doubts, bugs and uncertainties

Everything that comes up during implementation and is **not** an ordinary
coding choice goes here: bugs in external dependencies, ambiguous points of
`DESIGN.md`, known risks, decisions taken in the absence of guidance.

Every entry has a status: `OPEN` · `RESOLVED` · `TO CONFIRM` (needs a product
decision) · `ACCEPTED` (known and deliberate risk) · `DECIDED` (resolved by an
implementation decision, recorded for review).

The labels were translated from the original Italian log: `APERTO` → `OPEN`,
`RISOLTO` → `RESOLVED`, `DA CONFERMARE` → `TO CONFIRM`, `ACCETTATO` →
`ACCEPTED`, `DECISO` → `DECIDED`.

Cross-references: `PROGRESS.md` for the implementation status.

---

## Bugs found in external dependencies

### N-001 · x/text `cases.Fold()` is not idempotent on Cherokee letters — RESOLVED
*Found on 2026-09-20 by fuzzing `FuzzKey`, in `internal/names`.*

`golang.org/x/text/cases.Fold()` v0.41.0 performs `fold(U+ABB8) = U+13E8` **and**
`fold(U+13E8) = U+ABB8`: the folding oscillates with period 2. Unicode's
`CaseFolding.txt` instead maps `AB70..ABBF -> 13A0..13EF` and `13F8..13FD ->
13F0..13F5` (Cherokee uppercase letters are the canonical target, because they
were encoded first).

**Domain impact:** two albums whose names differ only in case would have got
different `folder_key` values, and therefore two distinct rows in
`path_claims`, contrary to §5.3 (case-only variants of the same album occupy a
single row).

**Resolution:** `names.Key` applies the correct mapping after folding
(`foldCherokee`). The offending input stays in the fuzzing corpus as a
regression test; `TestKeyCoversAllOfUnicode` enumerates every code point and
checks that there are no other cases of non-idempotence or broken case orbits.

**To be re-evaluated** on every `golang.org/x/text` upgrade: if the bug is fixed
upstream, `foldCherokee` becomes a no-op but **must not be removed without a
key migration**, because the algorithm is frozen (§5.2).

---

## Ambiguities in `DESIGN.md`

### N-002 · "Trim outer spaces/dots": both ends or only the trailing one? — DECIDED
**Decision (2026-09-21, owner rule "stick to DESIGN.md"):** follow the letter of §5.2: trim outer spaces and dots at both ends. `.hidden` becomes `hidden`.

§5.2. The implementation trims **both ends**, as the letter of the spec says.
Visible consequence: an imported file `.hidden` materializes as
`Extras/hidden`.

- For: no attachment can create hidden files in the output; no attachment can
  collide with `.musiclib.json`.
- Against: §7.2 says that hidden files other than these are kept, which may
  refer to keeping the *content* (which happens) or also the *name* (which
  does not).

The Windows rule the constraint derives from concerns only **trailing**
spaces/dots. If keeping the leading dot is preferred, the change is one line in
`names.sanitize`, but it changes the `folder_key` values: it must be made
**now**, not after the first import.

### N-003 · Extension longer than the truncation budget — DECIDED
§5.2 requires 180 bytes per component and says that the extension is
preserved, without defining the case where the extension alone leaves no room
for the stem. Implemented: the extension is not preserved rather than exceeding
180 bytes. The length limit is an invariant, preserving the extension is not.

### N-004 · "Lexicographic tie-break" for the album genre — OPEN
§7.3. It does not say what the ordering is based on: raw UTF-8 bytes, NFC form,
or casefold key. To be decided when §7.3 is implemented, and pinned in a test:
it is an input to the determinism of the import.

### N-005 · Order of the checks on path depth and length — DECIDED
§5.2 says "at most 16 levels and 1,024 bytes **after the transformation**". The
depth is checked before sanitization because sanitization does not change the
number of segments (it is per segment and neither introduces nor removes `/`);
the 1,024 bytes are checked afterwards. Equivalent, but with better error
messages.

---

## Deliberate deviations from the spec

### N-006 · Control characters added to the set of forbidden characters — ACCEPTED
§5.2 lists `/ \ : * ? " < > |`. The implementation also replaces control
characters (C0, C1, DEL) with `_`: they cannot appear in an output file name or
in an HTTP `Content-Disposition` header. §5.2 already rejects them in metadata
texts, so this is consistent with the intent.

### N-007 · NUL byte explicitly rejected in relative paths — ACCEPTED
`names.SplitRelPath` returns `path_nul_byte` instead of letting the syscall fail
with an opaque error.

### N-008 · `internal/names` package not listed in §2.3 — ACCEPTED
§2.3 does not assign a package to normalization, but §13.2 requires a single
implementation and its consumers are `catalog`, `importer` and `render`.

---

## Open risks and uncertainties

### N-009 · Go 1.25 toolchain and dependency versions — ACCEPTED
`golang.org/x/text` is pinned to `v0.41.0`, the last one compatible with the
`go 1.25.0` directive; `v0.42.0` requires Go 1.26. §2.1 requires pinned
versions and never `latest`. When the `Dockerfile` is written, the versions of
Go, TagLib and ffmpeg must be pinned there by the same criterion, and they
contribute to `render_version`.

### N-010 · `render_version` is not defined yet — OPEN
§2.1 describes it as a build constant covering the renderer code, the naming
rules, the tag mapping and the tool versions. It must be introduced before the
first render, and it must include an identifier of the version of the
`internal/names` algorithm.

### N-011 · No CI check on ext4 — RESOLVED (local), OPEN (CI)
The Docker gate (`scripts/check.sh`) refuses to run unless `TMPDIR` is on
ext4 (magic 0xef53, named volume `musiclib_testdata`). A CI job running the
same scripts on a native Engine is still to be set up: see N-017.

### N-012 · The whole project must be containerized — RESOLVED
Build, vet, gofmt, race tests, fuzzing and shell lint run in Docker
(`Dockerfile`, `compose.yaml`, `scripts/`, `docs/docker.md`). The TagLib
helper and ffmpeg are still TODOs in the `Dockerfile` (N-025).

---

## Docker and deployment (2026-09-21)

### N-017 · The development host uses Docker Desktop, which §3.1 does not support — ACCEPTED (dev), OPEN (CI)
The tests run on real ext4, but inside Docker Desktop's VM with its kernel
(6.12 linuxkit), not the host's. Production must be a native Engine on Ubuntu
24.04+. A native-Engine run (CI or the host's own Engine) is needed before
any release.

### N-018 · No overlayfs or bind mounts for test data — DECIDED
The container root is overlayfs, whose rename/exchange semantics differ from
ext4; on Docker Desktop bind mounts are `fakeowner`. Only a named volume is
used. A loop-mounted ext4 image was rejected: it needs CAP_SYS_ADMIN.

### N-019 · Go 1.25 is outside upstream support — DECIDED
**Decision (2026-09-21, owner rule "stick to DESIGN.md"):** DESIGN.md only requires pinned versions; stay on Go 1.25.x and x/text v0.41.0, so no key migration is needed.

Go 1.27 and 1.26 exist, so 1.25.14 gets no more security fixes. Bumping the
`go` directive also moves `golang.org/x/text` (v0.42+) and `x/sys` (v0.48+).
x/text's Unicode tables feed `names.Key` (§5.2, frozen algorithm): the bump
needs the exhaustive code-point test and possibly a key migration. Decide
before the first real import.

### N-020 · `musiclibd` must set umask 022 itself — OPEN
Docker's default is 0022, but a different runtime or `--entrypoint` may
change it. `musiclibd` calls `unix.Umask(0o022)` at startup (§11.1).

### N-021 · initdb with data checksums and the builtin C.UTF-8 locale — RESOLVED
Reverted to image defaults: not in DESIGN.md. `POSTGRES_INITDB_ARGS` was
removed from `compose.yaml` (owner's instruction, 2026-09-21). initdb now uses
the image defaults (no data checksums, libc `en_US.utf8` collation). An
existing `musiclib_pgdata` volume keeps whatever it was initialized with.

### N-022 · Postgres credentials and exposure — TO CONFIRM
`POSTGRES_PASSWORD` defaults to `musiclib`, read only at initdb. No published
port; `sslmode=disable` on the Compose network. Acceptable for single-user
use (§10.4); users should set `.env` before the first `up`.

### N-023 · Digest bump policy — DECIDED
Exact tag plus index digest, resolved with `docker buildx imagetools
inspect`, committed on their own and followed by `scripts/check.sh`. A
PostgreSQL major bump means dump/restore. Bumping Go, TagLib or ffmpeg
changes `render_version` (N-010).

### N-024 · testcontainers (§12.1) inside the containerized gate — DECIDED (owner)
Deviation from the letter of §12.1 (testcontainers): same real PostgreSQL 17
image and digest, started by Compose, because testcontainers inside the gate
would need the root-equivalent Docker socket. No container gets the socket.

Built: a `postgres-test` service (profile `tools`, tmpfs data, no published
port) on an internal network `testdb` (`internal: true`) shared with `test`,
which lost `network_mode: none` but still has no internet. `dev` joins
`testdb` too. `scripts/check.sh` and `scripts/dev.sh` start it and wait for
health. The only code that knows where the database comes from is
`internal/store/pgtest` (`EmptyDB` returns the URL of a fresh database,
dropped `WITH (FORCE)` at the end of the test). It reads
`MUSICLIB_TEST_DATABASE_URL`, skips without it, and fails instead of skipping
when `MUSICLIB_REQUIRE_DB=1`, which only the `test` service sets. Swapping the
launcher later touches only that helper.

### N-025 · How to pin ffmpeg and TagLib — OPEN
`apt-get install ffmpeg=<ver>` is reproducible only with a pinned
`snapshot.debian.org`; alternatives are a static build or a source build
pinned by sha256. TagLib 2.x: source tarball pinned by sha256, built on the
runtime's Debian release. Both feed `render_version`.

### N-026 · `WORKERS` default = 2 — TO CONFIRM
§11.1 gives no default.

### N-027 · App healthcheck — OPEN
The slim image has no curl: the Compose healthcheck on `/health/ready` needs
a `musiclibd healthcheck` subcommand.

### N-028 · Named `/data` volume on a native Engine — ACCEPTED
It is ext4 only if Docker's root directory is. The §3.1 boot check (same
filesystem plus a real `RENAME_EXCHANGE` probe, N-032) is the enforcement;
the docs recommend `MUSICLIB_DATA=/path/on/ext4` for real use.

---

## `internal/fsops`

*N-013..N-015 found on 2026-09-21 while translating the package to English;
resolved the same day when the package was completed.*

### N-013 · Opening a FIFO blocks forever; `Root.Close` then deadlocks — RESOLVED
Every open goes through `sysOpenat2`, which always adds
`O_NONBLOCK|O_NOCTTY|O_CLOEXEC`; `OpenFile` checks the type with `fstat` on the
returned descriptor (race-free: it is the inode actually opened) and clears
`O_NONBLOCK` only for a regular file. The `RWMutex` was replaced by a user count
that is never held across a syscall (N-031). Regression test:
`TestSpecialFilesRejectedWithoutBlocking` (every open under a 5 s guard that
unblocks the FIFO, so a regression fails in seconds instead of hanging;
verified by removing `O_NONBLOCK`).

### N-014 · Directory descriptor closed twice in `Remove` and `RemoveAll` — RESOLVED
Every descriptor now has one owner and one close: `defer closeInto(&err, ...)`
right after it is obtained, or explicit hand-over (`MkdirAll`). `unix.Close`
appears only in `closeFD` and no close error is discarded, so a double close
would surface as `EBADF`; `TestNoOperationOnStringBuiltPaths` enforces this on
the AST and `TestNoDescriptorLeaks` checks that no path leaks a descriptor.

### N-015 · Minor `internal/fsops` observations — RESOLVED
The probe registers the cleanup of each directory right after its `Mkdir`,
before writing markers (`TestProbeRenameExchangeCleansUpOnFailure`). A
`renameat2` error now carries both locations (`Root/Path -> DstRoot/DstPath`);
errors found before the syscall name the side they concern
(`TestRenameErrorLocations`).

## Translation to English

### N-016 · Italian fixture names left in test inputs — RESOLVED
All `internal/fsops` fixture names and contents are now English. The one
remaining Italian input, `"  spazi  "` in `internal/names/relpath_test.go`, is
in a frozen package and was left alone: the lead engineer decides whether to
change it.

---

## `internal/fsops` — review findings and decisions (2026-09-21)

### N-030 · A device node reaches its driver's `open` before rejection — ACCEPTED
`OpenFile` opens first and checks the type on the descriptor, because only
that check is race-free. Opening a character or block device therefore calls
the driver's `open` routine (with `O_NONBLOCK|O_NOCTTY`) before the rejection.
Linux cannot "open only if regular". A pre-check with `O_PATH` + `fstat` would
avoid this in the common case but not in a race, and reopening an `O_PATH`
descriptor needs `/proc/self/fd` magic links, which `RESOLVE_NO_SYMLINKS`
forbids by design. Exposure: device nodes can only appear under `/import`
(the user's files); the scan must `Stat`/`ReadDir` and reject special types
before opening, as §5.2 already requires.

### N-031 · `Root` lifetime: user count instead of an `RWMutex` — DECIDED
The old `RWMutex` was held across `openat2`, so a blocking open also blocked
`Close`, and every operation started after it queued behind the writer. Now
the mutex is held only to register or deregister a single syscall on the root
descriptor. `Close` rejects new users at once (`fs_root_closed`), waits for the
in-flight ones, which are single syscalls that cannot block indefinitely by
construction, and closes the descriptor. Consequence to know: an operation
that has already resolved its own descriptors (a long `RemoveAll`) keeps
running after `Root.Close`; it is stopped by its context. Shutdown (§11.1)
must cancel contexts, not rely on `Close`.

### N-032 · `SameFilesystem` (st_dev) is necessary, not sufficient — OPEN (boot sequence)
Two bind mounts of the same filesystem share `st_dev`, but `rename(2)` between
them fails with `EXDEV`. This is exactly the Docker case if `library` and `work`
were ever separate bind mounts. `ProbeRenameExchange` does a real exchange and
catches it (`fs_cross_device`), so **boot (§11.1 step 3) must run the probe,
not only `SameFilesystem`**. The bind-mount case is not covered by a test: it
needs mount privileges. A candidate for the ext4 Docker test volume.

### N-033 · Probe directories inside `library/` — DECIDED
**Decision (2026-09-21, owner rule "stick to DESIGN.md"):** §3.1 only requires checking that `renameat2(RENAME_EXCHANGE)` works; the probe runs inside `work/`, never in `library/`, which is output only (§3.3). Leftover probe directories in `work/` are removed by the boot cleanup of `work/` (§11.1 step 5).

§3.1 asks to verify `RENAME_EXCHANGE` between `library` and `work`, so the probe
briefly creates `.musiclib-probe-<random>` in the root of `library/`. That
directory is visible to players for a few milliseconds. A crash during the probe
leaves it behind. To decide: whether boot or doctor (§11.3) should remove
leftovers with this prefix automatically (the recommendation is yes, at boot
before the probe), or whether the probe should run between `work/` and a
dedicated directory on the same mount.

### N-034 · The flock inheritance test proved nothing — RESOLVED
The old `TestLockNotInheritedByChildren` released the lock in the parent and
re-acquired it. `LOCK_UN` releases the lock of the open file description for
every holder, inherited descriptors included, so the test passed even without
`O_CLOEXEC`. Now a child process (the test binary itself) checks each descriptor
it inherited against the lock file's dev/ino. A control run that passes the
descriptor on purpose must report it, and a second child checks cross-process
exclusion (held → busy, released → acquired). Verified by removing `O_CLOEXEC`:
the test fails.

### N-035 · Other bugs found in review — RESOLVED
- Close errors discarded: `Stat` (`_ = unix.Close`); `Mkdir`, `Rmdir`,
  `SyncDir` and `ReadDir` dropped the close error when the main syscall also
  failed; `MkdirAll` ignored the close of `next` on an error path.
- `OpenFile` ignored the error of `fcntl(F_GETFL)`.
- `FileInfo.Perm` stored the raw setuid/setgid/sticky bits (`0o4000`...) in an
  `os.FileMode`, where they mean nothing. They are now translated to
  `os.ModeSetuid`/`os.ModeSetgid`/`os.ModeSticky`.
- `renameat2` `EINVAL` (moving a directory into its own subtree) was reported as
  `fs_unsupported_operation`. It is now `fs_invalid_argument`. Only the probe,
  where no ancestry is possible, maps it to "unsupported".
- `RemoveAll` returned `fs_not_found` when the *parent* was missing, contrary
  to its contract ("a missing path is not an error").
- `Remove` did `fstatat` then `unlinkat`: a stat/use window. `unlinkat` without
  `AT_REMOVEDIR` already fails with `EISDIR` on a directory, so it is now one
  syscall. `RemoveAll` trusts the kernel over the listed type (file↔directory
  replaced mid-removal).
- The traversal in `MkdirAll`/`RemoveAll` used plain `openat(O_NOFOLLOW)`. It now
  uses `openat2` with the confinement flags like every other open, and
  `unix.Openat` is banned by the AST guard.
- The `ReadDir` test's NFC/NFD `"é"` fixtures had collapsed to the same bytes. They
  are now written as escapes.

### N-036 · `OpenFile` accepts an explicit set of flags — DECIDED
Access mode plus `O_CREATE`, `O_EXCL`, `O_TRUNC`, `O_APPEND`, `O_SYNC`, `O_DSYNC`.
Anything else (`O_PATH`, `O_TMPFILE`, `O_NOATIME`, ...) and nonsensical
combinations (`O_EXCL` without `O_CREATE`, `O_TRUNC` read-only) yield the new
stable code `fs_invalid_argument`; `O_DIRECTORY` keeps `fs_is_directory`.

### N-037 · Scope of `SyncDirAndParents` and of `OpenRoot` — DECIDED
- The fsync chain stops at the `Root`. When the volume layout is created
  (`library/`, `work/`, `originals/` under `/data`, §11.1 first
  initialization), the caller must sync the `/data` Root, not the sub-root.
- `OpenRoot` is the only function that takes a host path. It is trusted
  configuration: it follows symlinks in that path and does not require it to be
  absolute. Its label (the last element) appears in errors, the full path never
  does.

### N-038 · Test environment coverage — OPEN
Host run: `/tmp` is on `/` = ext4 (`findmnt`); `statfs` reports the shared
ext2/3/4 magic `0xEF53`. Skipped where the privilege is missing:
the device subtest (needs `CAP_MKNOD`); `TestRenameAcrossFilesystems` if
no second filesystem (`/dev/shm`, `/run/user`) is writable;
`TestProbeRenameExchangeCleansUpOnFailure` and the partial-failure branch of
`TestMkdirAndMkdirAll` when running as root (permissions are bypassed). A test
for "different bind mounts, same `st_dev`" (N-032) is missing.
Docker gate (`scripts/check.sh`, uid 1000, no capabilities, TMPDIR on the
ext4 `testdata` volume): passes; only the device subtest skips.
`scripts/dev.sh go test -race -count=20`: 20/20.

---

## `internal/blobstore` (2026-09-21)

### N-040 · Temporary names use `crypto/rand.Text`, not UUIDv7 — DECIDED
§3.1 writes `work/blobs/<uuid>.tmp`; §2.1 asks for UUIDv7 identifiers. A temp
name is never stored or referenced: it only has to be unique for the
exclusive create. `rand.Text()` (26 base32 characters, 130 random bits,
stdlib since Go 1.24) does that with no new dependency. `CleanTemps` relies
only on the `.tmp` suffix. `github.com/google/uuid` will be pinned when the
catalog needs real UUIDv7 row identifiers.

### N-041 · The shard chain is fsynced on every put, not only when created — DECIDED
§7.5 step 3 says to sync the *new* shard directories and their parents. If
put A creates `ab/cd` and put B, running concurrently, finds it already
there, B would skip the sync and could report success before A made `ab`
durable. `Put` therefore always runs `SyncDirAndParents("ab/cd")` (a superset
of the spec's set). On ext4 an fsync of a clean directory is cheap.

### N-042 · Pinned blobs are mode 0444 from creation — DECIDED
The temporary is created with `O_CREAT|O_EXCL|O_WRONLY` and mode 0444: Linux
allows writing through the descriptor that created the file, and the rename
keeps the mode, so there is no chmod step and no window in which a pinned
blob is writable. Shard directories are 0755. This stops accidental writes by
the application's own uid. It is not a security boundary: the owner can chmod
and root bypasses it. Doctor does not check the mode yet.

### N-043 · `Put` has no expected hash/size parameter — DECIDED
Callers compare the returned `Blob` with what they expected. A mismatch found
after pinning leaves an unreferenced blob, which §7.5 already makes harmless.
The upload size limits (§10.2) are the caller's job. `io.LimitReader`
truncates silently, so the caller must read `limit+1` bytes and reject the
upload, not pin a truncated prefix.

### N-044 · Failpoints are in-process only — OPEN (phase 3)
`Store.failpoint` (unexported, nil in production) runs at `temp_synced`,
`temp_verified`, `shards_synced` and `pinned`. Tests use it to inject errors
and to check the disk state at each point. §12.2 asks for real process
crashes at named failpoints. That needs a child-process harness, which is
phase 3. These names are the ones to use there.

### N-045 · ENOSPC is not tested on a really full filesystem — OPEN
The `ENOSPC`/`EDQUOT` → `blob_no_space` mapping and its cleanup are tested
by injecting `ENOSPC` at the `temp_synced` point. That point is realistic:
with delayed allocation, ext4 can report ENOSPC at fsync. A real full-disk
test needs a small dedicated ext4 volume. A loop mount needs
`CAP_SYS_ADMIN` (N-018), so it belongs with the "Disco pieno durante build"
row of §12.2. Pinned blobs are never opened for writing, so ENOSPC cannot
damage them in any case.

### N-046 · What counts as a corrupt blob — DECIDED
Anything at a blob's name that is not a regular file with the matching size
and SHA-256 is `corrupt_blob`: a symlink (never followed, even to a good
copy), a directory, a special file, a wrong size or wrong content. The put
discards its good temporary and never replaces the entry (§7.5). Repair comes
from the backup (§11.3). A broken shard chain (for example `ab` being a file)
is reported as `blob_io` with the fsops cause, not as `corrupt_blob`.

### N-047 · `Put` can fail with a valid blob pinned — DECIDED
A failure after the rename (a step 5 fsync, the `pinned` failpoint), or a
failed temp removal in the already-exists branch, returns an error even
though the blob is intact. The caller must treat it as a failure and must
not reference the blob. A retry is idempotent: it goes through the
already-exists branch, which verifies and fsyncs.

### N-048 · `CleanTemps` preconditions — DECIDED
It must run at boot before any `Put` (§11.1 step 5), because an in-flight
temporary looks exactly like a leftover. It removes only regular `*.tmp`
entries of `work/blobs` and leaves everything else there. It does not fsync
`work/blobs` afterwards: a temporary that comes back after a crash is removed
at the next boot. It holds only the `work` root, so it cannot reach
`originals/`.

### N-049 · Enumerating every blob for `doctor --deep` — OPEN (phase 6)
§11.3 hashes "all blobs present" and reports unreferenced ones. `Verify`
covers one blob. A walk over `originals/ab/cd/` that also reports entries not
shaped like blobs will be added together with doctor.

---

## `internal/store` and migrations (2026-09-21)

### N-050 · Pinned versions of the store dependencies — DECIDED
Every module below declares `go` ≤ 1.25.0 (checked on proxy.golang.org):

| Module | Version | Why this one |
|---|---|---|
| `github.com/pressly/goose/v3` | v3.27.0 (`go 1.25.0`) | v3.27.1..v3.27.3 declare `go 1.25.7`, v3.28.0 `go 1.26.0` |
| `github.com/jackc/pgx/v5` | v5.11.0 (`go 1.25.0`) | latest |
| `github.com/google/uuid` | v1.6.0 | latest; has `NewV7` |
| `golang.org/x/sync` | v0.22.0 (`go 1.25.0`) | selected by MVS, indirect |

`golang.org/x/text` stays v0.41.0 and `golang.org/x/sys` v0.47.0, so the
frozen `names.Key` tables (§5.2, N-001) are untouched. Only 12 modules are
compiled. The other `go.sum` entries (sqlite, testify, ...) come from goose's
own tests and are never built. sqlc is the image `sqlc/sqlc:1.31.1`, pinned
by index digest (docs/docker.md).

### N-051 · UUIDv7 from `github.com/google/uuid` — DECIDED
§2.1 wants application-generated UUIDv7. `store.NewID` wraps `uuid.NewV7`,
which is strictly increasing within the process (a 12-bit sub-millisecond
counter) and fails only if `crypto/rand` does, which crashes the program since
Go 1.24, so `uuid.Must` never panics in practice. Supersedes the plan of
N-040. pgx encodes and decodes `uuid.UUID` natively. sqlc maps `uuid` to
`uuid.UUID` and nullable `uuid` to `*uuid.UUID`.

### N-052 · Interpretations of §4.2 in the schema — DECIDED
Where §4.2 is silent, the simplest reading was chosen and nothing was added
that it does not ask for:
- **FK actions:** `ON DELETE RESTRICT` is explicit. `ON UPDATE` keeps the
  default (NO ACTION): keys are never updated.
- **Defaults:** only the ones §4.2 lists (`compilation false`,
  `published_revision 0`, `overrides '{}'`, `warnings '[]'`), plus
  `requested DEFAULT nextval('job_ticket')` from its comment. Timestamps and
  revisions have no default: the services write them.
- **Hash checks** (`^[0-9a-f]{64}$`) on `blobs.hash`,
  `albums.import_fingerprint` (a SHA-256, §7.6), `albums.published_receipt_hash`
  and `publication.receipt_hash`. The other hash columns are FKs to `blobs`.
- **Job column combinations**, from the comments of §4.2. `render`: `album_id`
  required; `batch_id`, `source_rel` and `result_album_id` NULL; `overrides = {}`;
  state pending/running/failed. `scan`: `batch_id` required; `album_id`,
  `source_rel` and `result_album_id` NULL; `overrides = {}`. `import`:
  `batch_id` and `source_rel` required, `album_id` NULL. `result_album_id` is
  not tied to a state, because §7.6 also uses it on `skipped`.
- **`overrides`:** an object whose keys are a subset of `{artist, title}`,
  with string values. §7.3 treats the two keys independently. The closed Go
  type validates the rest.
- **Not enforced in SQL:** non-empty names and titles, `tracks.artist <> ''`
  (§4.1/§5.2: domain validation), `claimed <= requested`, and any coupling of
  `error_code`/`error_message` to the state. §4.2 does not list them.
- **`published_*` coherence:** path, build and receipt all present or all NULL.
  A non-NULL path requires `published_revision > 0`. `published_revision > 0`
  with no path is a completed deletion.
- **`publication`:** `receipt_hash` NULL iff `new_path` NULL (removal);
  `old_path` and `old_build` present together. A row with both paths NULL is
  not forbidden.
- **Indexes:** "every FK used for lookups" is read as every FK, blob FKs
  included. The partial unique indexes on `jobs` cannot serve lookups without
  their predicate, so `jobs(album_id)` and `jobs(batch_id)` also get plain
  indexes. The single-row `publication` has none. `TestForeignKeysRestrictAndAreIndexed`
  enforces the rule.
- **Names:** default PostgreSQL names for single-column constraints;
  explicit names for multi-column ones and for the partial unique indexes.
  The column `no` is kept: `NO` is a non-reserved keyword.
- **Forward only:** the migration has no `-- +goose Down` section (§11.4).

### N-053 · Migration locking: a blocking advisory lock, not goose's locker — DECIDED
goose's `PostgresSessionLocker` polls `pg_try_advisory_lock` every 5 s, so a
waiting boot sleeps up to 5 s. `store.Migrate` instead takes
`pg_advisory_lock` (blocking, cancellable through the context) on a dedicated
connection outside the pool, then runs goose on the pool. Closing that
connection always releases the lock, even when the process dies. The key is
ASCII `mlmigrat`, distinct from the future catalog lock (§5.3).

### N-054 · A newer schema is refused — DECIDED
If the database's goose version is above the last embedded migration,
`Migrate` returns `store_schema_too_new` instead of running old code on a
newer schema (§11.4: "solo migrazioni forward supportate").

### N-055 · No boot check of the PostgreSQL version and durability settings — DECIDED
**Decision (2026-09-21, owner rule "stick to DESIGN.md"):** DESIGN.md asks PostgreSQL to keep the settings on, which compose.yaml enforces with `-c`; no additional boot check (simplest option).

§11.1 requires `fsync`, `full_page_writes` and `synchronous_commit` on. Compose
sets them, but an `ALTER ROLE ... SET synchronous_commit = off` or an
`options=-c synchronous_commit=off` in `DATABASE_URL` would silently weaken
§3.2. A `VerifyServer` check (settings as seen by a pool session, plus
PostgreSQL 17) was written and tested, then removed under the owner's
"nothing DESIGN.md does not ask for" rule. The owner decides whether to add
it back to the boot sequence.

### N-056 · `sqlc generate` does not delete stale output — ACCEPTED
When a file of `sql/` is renamed or removed, its `internal/store/*.sql.go`
must be deleted by hand. `sqlc diff` in the gate compares only the files that
sqlc generates, so it does not catch a leftover file. A leftover usually
fails the build anyway, through duplicate or dangling identifiers.

### N-057 · The dev image could not run `go get` — RESOLVED
`/home/dev/go/pkg` was created root-owned, as an implicit parent of
`pkg/mod`, so the Go command could not create its checksum-database cache
(`pkg/sumdb`) and every `go get` failed while verifying modules. The
`Dockerfile` now creates it owned by `dev`.

### N-058 · Leftover test databases — ACCEPTED
A test run killed before its cleanups leaves `musiclib_test_*` databases on
`postgres-test`. Nothing reuses them. The server's data is tmpfs, so
`docker compose --profile tools stop postgres-test` removes them all.

