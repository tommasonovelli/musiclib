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

### N-020 · `musiclibd` must set umask 022 itself — RESOLVED
Docker's default is 0022, but a different runtime or `--entrypoint` may
change it. `musiclibd` calls `unix.Umask(0o022)` at the start of the server
(`serve`, §11.1). `TestServerProcessLifecycle` starts the server as a child
process with umask 077 and reads `Umask: 0022` from its `/proc/<pid>/status`;
removing the call makes it fail. Verified in the Compose `app` container too.

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

### N-026 · `WORKERS` default — RESOLVED
The old entry said "§11.1 gives no default; default = 2, TO CONFIRM". That was
wrong: §6.1 defines it, `max(1, min(4, CPU disponibili))`, allowed 1..16.
`compose.yaml` now passes `WORKERS: ${WORKERS:-}`, and an empty value means
the §6.1 default, computed by `musiclibd` (N-066 for "available CPUs").

### N-027 · App healthcheck — RESOLVED
The slim image has no curl. `musiclibd healthcheck` reads only `HTTP_ADDR`,
turns an empty or unspecified host into loopback, GETs `/health/ready` with
a 3 s timeout, no proxy and no redirects, and exits 0 for 200 and 1 for
anything else (Docker defines only those two). It takes no lock and starts
nothing (§11.3). Compose uses it with `start_period: 120s`.

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

### N-032 · `SameFilesystem` (st_dev) is necessary, not sufficient — RESOLVED
Two bind mounts of the same filesystem share `st_dev`, but `rename(2)` between
them fails with `EXDEV`. This is exactly the Docker case if `library` and `work`
were ever separate bind mounts. `ProbeRenameExchange` does a real exchange and
catches it (`fs_cross_device`), so **boot (§11.1 step 3) must run the probe,
not only `SameFilesystem`**. The bind-mount case is not covered by a test: it
needs mount privileges. A candidate for the ext4 Docker test volume.

**Resolution (2026-09-22):** the boot compares st_dev *and* the mount id
(`fsops.SameMount`, statx `STATX_MNT_ID`) of `/data` with each media
directory, and runs the real probe in `work/` (N-033, N-063). The case is now
tested without privileges: in the Docker gate the Go build cache and the ext4
TMPDIR are two named volumes on one disk, so they share st_dev but not the
mount. `TestSameMountSeesBindMountsOfOneFilesystem` checks that
`SameFilesystem` says yes, `SameMount` says no and the probe fails with
`fs_cross_device`; `TestCheckFilesystemNestedMount` checks the boot code
`volume_nested_mount`. Both skip only where no such mount is visible.

### N-033 · Probe directories inside `library/` — DECIDED
**Decision (2026-09-21, owner rule "stick to DESIGN.md"):** §3.1 only requires checking that `renameat2(RENAME_EXCHANGE)` works; the probe runs inside `work/`, never in `library/`, which is output only (§3.3). Leftover probe directories in `work/` are removed by the boot cleanup of `work/` (§11.1 step 5).

§3.1 asks to verify `RENAME_EXCHANGE` between `library` and `work`, so the probe
briefly creates `.musiclib-probe-<random>` in the root of `library/`. That
directory is visible to players for a few milliseconds. A crash during the probe
leaves it behind. To decide: whether boot or doctor (§11.3) should remove
leftovers with this prefix automatically (the recommendation is yes, at boot
before the probe), or whether the probe should run between `work/` and a
dedicated directory on the same mount.

**Implemented (2026-09-22):** the boot runs `fsops.ProbeRenameExchange(work,
work)`: both probe directories are in `work/`, and the mount-id check proves
that `library/` is on the same mount (N-063). Step 5 of the boot removes
leftover `.musiclib-probe-*` directories at the top of `work/` with
`fsops.RemoveProbeLeftovers`, after the probe of step 3 has finished.

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
`TestMkdirAndMkdirAll` when running as root (permissions are bypassed). The
"different bind mounts, same `st_dev`" case (N-032) is now tested where such
a mount is visible: always in the Docker gate, skipped on a plain host.
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


---

## Volume, boot and `musiclibd` (2026-09-22)

### N-059 · The Windows checkout had CRLF line endings, and Git Bash rewrote container paths — RESOLVED
The system Git config has `core.autocrlf=true`, so the working tree was
checked out with CRLF. Every shell script failed in the containers
(`env: 'bash\r'`), and gofmt would have rejected every Go file. A new
`.gitattributes` (`* text=auto eol=lf`) forces LF on every checkout. The index
was already LF, so no committed file changes.

Separately, Git Bash (MSYS) rewrites arguments that look like POSIX paths
before they reach `docker.exe`. `scripts/check.sh` failed at `sqlc diff` with
`--workdir C:/Program Files/Git/src`. `scripts/lib/common.sh` now sets
`MSYS_NO_PATHCONV=1` under MSYS/Cygwin and gives Docker the native form of the
repository path (`pwd -W`). Nothing changes on Linux.

### N-060 · The volume code lives in a new package, `internal/volume` — DECIDED
§2.3 lists no package for the volume lock, the markers and the boot checks.
Two consumers need the same code:
- `cmd/musiclibd`, for the server's boot;
- the Phase 6 maintenance subcommands. Doctor, rebuild, backup and restore
  take the same lock, read the same markers and must recognize the same
  identity (§11.3, §11.4).

The alternatives do not work:
- In `cmd/musiclibd`, `internal/maintenance` would have to import a `main`
  package, which Go forbids.
- In `internal/maintenance`, the server would depend on the maintenance
  package.
- In `internal/fsops`, the filesystem primitives would know about the
  database and the domain (§13.2).

So, as with `internal/names` (N-008), it is its own small package:
`Acquire`, `CheckMaintenance`, `Identify`, `OpenLayout`, `CheckFilesystem`,
`Close`, the marker formats, and typed errors with stable `volume_*` codes.
It touches the disk only through `internal/fsops` and the database only
through `internal/store`.

### N-061 · First initialization: order and crash windows — DECIDED
§11.1: "si crea `settings.store_id` in transazione e poi il marker di volume
con scrittura durevole no-replace ... La procedura è ripetibile se il primo
avvio si interrompe." When the volume has no marker, `volume.Identify` does:

1. Check that the media storage is empty (N-062). Otherwise refuse, writing
   nothing.
2. In one transaction: `InsertStoreID(new)` (ON CONFLICT DO NOTHING), read
   the id back with `GetStoreID`, commit.
3. Remove a leftover `.musiclib-store.tmp`, create it exclusively (mode 0444),
   write `store_id=<uuid>\n`, fsync, close.
4. `renameat2(RENAME_NOREPLACE)` it to `.musiclib-store`.
5. fsync `/data`.

Then `OpenLayout` creates `originals/`, `library/` and `work/` and fsyncs
`/data`. It fsyncs every time, not only when it created something.

The database goes first. A crash can then leave only "store_id, no marker,
empty media", which step 1 lets the next boot complete. The other order could
leave "marker, no store_id", which cannot be told apart from a foreign volume
and must be refused. The rename makes the marker appear complete or not at
all, so a crash can never leave a truncated marker that blocks every later
boot.

`TestFirstInitInterruptedAtEveryStep` tests each crash window. It kills a
child process with SIGKILL at a named failpoint, checks the intermediate
state, then boots again:

| Killed at | State left | Next boot |
|---|---|---|
| `media_checked` | nothing | first init |
| `db_inserted` (inside the transaction) | nothing: PostgreSQL rolls back | first init |
| `db_committed` | store_id, no marker | completes the marker with that id |
| `marker_temp_synced` | store_id, complete temporary | removes it, rewrites, renames |
| `marker_renamed` (before the fsync of `/data`) | marker | paired |
| `marker_synced` | marker, no layout | paired, creates the layout |
| `layout_created` (before the fsync of `/data`) | marker, layout | paired, fsyncs again |

A temporary with arbitrary partial content is also tested. Killing a process
does not lose the page cache, so the fsync ordering is argued, not tested
against a power cut (the same limit as N-044).

### N-062 · What "empty media storage" means — DECIDED
§11.1 allows completing a missing marker only "su storage media vuoto". The
media storage is `originals/`, `library/` and `work/` (§3.1). It is empty when
each of the three is either absent or a directory with no entries at all.

Anything else counts as not empty:
- a file, a symlink or a special file at one of those names;
- any entry inside them, including an empty subdirectory and `work/blobs/`.

Other entries at the top of `/data` are not media and are ignored:
`lost+found` on a dedicated ext4 filesystem, `.lock`, the marker temporary.

A refusal is `volume_not_empty` when the database has no store id either,
and `volume_marker_missing` when it has one. Neither writes anything.

Empty media is necessary, not sufficient. The database must also have no
catalog content, otherwise the boot is refused with `volume_marker_missing`
(N-069).

### N-063 · Boot checks of the filesystem, new fsops primitives and test gaps — DECIDED
`Volume.CheckFilesystem` (§3.1, §11.1 step 3) runs, in this order:
1. The st_dev of `/data` equals that of `originals/`, `library/` and `work/`
   (`volume_cross_device`), and so does the mount id (`volume_nested_mount`).
   This covers "Nessun mount annidato" and N-032.
2. `faccessat2(R_OK|W_OK|X_OK, AT_EACCESS)` on `/data` and the three
   directories (`volume_permission`, also for a read-only mount).
3. `ProbeRenameExchange(work, work)`. `fs_unsupported_operation` and
   `fs_cross_device` become `volume_rename_exchange_unsupported`.

After that, `/import` must open and be listable (`import_unavailable`). It is
not checked for being read-only or a separate mount: §3.1 describes both but
does not list them among the boot checks.

New in `internal/fsops`:
- `SameMount`, using statx `STATX_MNT_ID`;
- `Root.CheckAccess`, using faccessat2 with `AT_EMPTY_PATH`;
- `RemoveProbeLeftovers`.

The first two need **Linux 5.8** (openat2 already needed 5.6). Ubuntu 24.04
ships 6.8, so this is within §2.1. On an older kernel the boot fails with a
clear message, never silently.

Tested for real:
- cross-device, with `/dev/shm`;
- a nested mount of the same filesystem, with a sibling named volume in the
  gate;
- permissions, with chmod;
- a read-only mount: the gate's read-only root, where `$HOME` belongs to the
  test user. When the permission bits also deny the write, the kernel reports
  EACCES before EROFS.

Injected: a filesystem without `RENAME_EXCHANGE`, through the package
variable `probeRenameExchange`, because producing one needs mount privileges.

Not tested: an older kernel without statx mount ids or faccessat2.

### N-064 · `DATABASE_URL` never reaches the logs — DECIDED
- The configuration is logged through `Config.LogValue`, which leaves
  `DATABASE_URL` out.
- A `DATABASE_URL` that pgx cannot parse is reported without pgx's message:
  pgx redacts passwords in parse errors only "on a best effort basis" for
  malformed input.
- Connection errors are logged while the boot waits for PostgreSQL. pgx names
  host, user and database there, never the password.

`TestServerProcessLifecycle` checks that the test database's password never
appears in the logs of a full run. The configuration tests check the error
messages.

### N-065 · The maintenance marker is checked right after the lock, before the database — DECIDED
§11.1 lists "assenza del marker di manutenzione" in step 3, after the
migrations of step 2. The boot checks it right after taking the lock instead,
before waiting for PostgreSQL and before migrating. An interrupted restore can
leave a partially restored database, and nothing (migrations, store id) may
write to either side until the maintenance operation is repeated. This order
is the more conservative one and changes nothing else.
`TestBootRefusals/maintenance_marker` proves the order: the boot is refused at
once even with an unreachable database.

### N-066 · "Available CPUs" for the `WORKERS` default is `runtime.GOMAXPROCS(0)` — DECIDED
§6.1 says `max(1, min(4, CPU disponibili))`. Since Go 1.25, GOMAXPROCS
defaults to the CPU limit of the container's cgroup as well as the affinity
mask. It is therefore the number of CPUs the process can actually use, while
`runtime.NumCPU` ignores a Compose `cpus:` limit. A `GOMAXPROCS` environment
variable would also change it; nothing sets one.

### N-067 · Formats of `/data/.musiclib-store` and `/data/.maintenance` — DECIDED
§11.3 says only that the maintenance marker holds "operazione e store_id".
Both markers use one strict text format:
- fixed `key=value` lines, in a fixed order, each ending in `\n`;
- nothing else: no spaces, CR, comments, extra or missing keys;
- at most 512 bytes;
- the store id in canonical lowercase UUID form, not nil.

```text
.musiclib-store   store_id=<uuid>\n
.maintenance      operation=<rebuild|restore>\nstore_id=<uuid>\n
```

Anything else at those names is `volume_marker_malformed` or
`volume_maintenance_malformed`: a directory, a symlink (never followed), a
FIFO (never waited on). Both block the boot. The server never rewrites or
removes either file. `volume.Maintenance.Encode` is the writer the Phase 6
subcommands must use.

### N-068 · A refused boot exits 1, and Docker restarts it in a loop — DECIDED
Every failed boot check is fatal:
1. the error is logged once, with its stable `code`;
2. everything acquired is released: HTTP, then the pool, then the lock last;
3. the process exits 1, or 2 for an invalid configuration or uid 0.

With `restart: unless-stopped`, Docker restarts it with its own backoff. The
error repeats in `docker compose logs` and the container stays unhealthy.
No attempt writes anything.

Staying alive with negative readiness was considered and rejected: it would
hold the volume lock and keep the maintenance commands out, and the refusals
do not go away by themselves. Waiting for PostgreSQL is the one condition
retried inside the process (§11.1 step 2).

### N-069 · An empty volume is never paired with a database that has catalog content — RESOLVED
**Found in review (2026-09-22).** The first version completed a missing
marker whenever the media storage was empty. A new, empty volume started
against a database already in use therefore silently adopted that
database's store id. A mistyped `/data` mount is enough to cause it: imports
then go to the wrong volume, and the originals end up split across two
volumes that both claim the same store. This violates §2.2 ("rifiutare
accoppiamenti sbagliati").

The §11.1 sentence "un marker assente si può completare soltanto su storage
media vuoto" states a necessary condition, not a sufficient one. Its purpose
is to make an *interrupted first boot* repeatable, and an interrupted first
boot never leaves catalog content behind: nothing can be imported, uploaded
or queued before the boot has completed.

**Resolution.** Completing a missing marker now needs both conditions:
- empty media storage (N-062);
- a database in its first-initialization state, checked by the sqlc query
  `store.CatalogIsEmpty` (`sql/settings.sql`): no row in `blobs`, `artists`,
  `albums`, `import_batches` or `jobs`.

Otherwise the boot is refused with `volume_marker_missing`, before the store
id transaction, and nothing is written to the volume or the database. The
check applies whether or not the database has a store id.

The other catalog tables all need one of those five: tracks, attachments and
path claims need an album or an artist, and the publication journal needs an
album. `albums` and `jobs` are also implied by `artists` and
`import_batches`: every album has an artist (`artist_id` NOT NULL), and every
job references an album or a batch (§4.2 combinations). They stay in the
query to state the intent. That is also why a mutation that drops only the
`albums` clause survives the test; dropping `blobs`, `artists` or
`import_batches` does not, nor does removing the check.

`TestEmptyVolumeRefusedWithCatalogContent` covers a blob, an artist, an
album, an import batch and a scan job. Each is tested with and without a
store id, on empty media. It checks the code, the unchanged store id, and
that the volume holds only `.lock` afterwards.

**What remains.** Two servers started on the same new database at the same
time, each with its own empty volume, can both pass the check before either
has imported anything. Both volumes then carry the same store id, and later
imports could land on either. §2.2 already excludes this ("una sola istanza
dell'applicazione per volume e database"). The flock enforces it per volume
only; nothing enforces one server per database. This is a narrow race at
first installation, not the mount mistake above.

### N-070 · Readiness follows the database, and the process does not exit when it goes away — DECIDED
`/health/ready` is 200 only after step 7, and only if a `Ping` through the
pool succeeds within 2 s. Otherwise it answers 503 with `{code, message}`:
`not_ready` or `db_unavailable` (§10.1).

§6.4 says that losing the database stops the workers and restarts the process
through Docker, "per evitare una seconda macchina a stati per riconnettere
worker parzialmente attivi". Phase 1 has no workers and no mutations, so there
is nothing to protect. The process keeps running, and readiness turns
negative and positive again with the database (`TestBootReadinessLifecycle`).
Phase 2 must add the §6.4 exit when the workers arrive.

### N-071 · Docker: the `app` service, and a Dockerfile ARG bug — RESOLVED
`build-app` used `${DEV_UID}` in its cache mount, but that ARG was declared
only in the `toolchain` stage, and a stage sees only the ARGs it declares. The
global default is now declared before the first `FROM` and redeclared in the
stages that use it.

`app` now has the healthcheck `musiclibd healthcheck` (N-027), and `WORKERS`
is empty by default (N-026). It stays behind the `app` profile, so the
development workflow never builds it by accident. Deploying is
`docker compose --profile app up -d`.

Verified end to end on Docker Desktop, with a separate project name
(`-p musiclib-e2e`, `MUSICLIB_PORT=18080`):
- the app became healthy and `/health/ready` answered 200;
- `/data` had `.lock`, the 0444 marker and the three directories, all owned
  by uid 1000;
- the process umask was 0022;
- a restart kept the store id;
- a recreated database was refused (`volume_db_uninitialized`), and so was a
  database with another store id (`volume_store_mismatch`). Both were refused
  in a restart loop, and the marker stayed unchanged: same bytes, inode and
  mtime.

### N-072 · `/data` is not checked for being ext4 — DECIDED
§3.1 says ext4 and Linux are requirements and lists what the boot verifies:
the same filesystem and a working `RENAME_EXCHANGE`. The boot checks exactly
that, plus mounts and permissions. XFS, Btrfs and tmpfs also pass. There is no
filesystem-type check to refuse them: DESIGN.md does not ask for one, and
statfs cannot tell ext4 from ext2/ext3. The docs recommend ext4 (N-028).
