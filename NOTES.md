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

### N-004 · "Lexicographic tie-break" for the album genre — DECIDED (round 5)
§7.3. It does not say what the ordering is based on: raw UTF-8 bytes, NFC form,
or casefold key. To be decided when §7.3 is implemented, and pinned in a test:
it is an input to the determinism of the import.

**Decision (round 5, `internal/importer`):** the genres are compared as the
bytes of their normalized text (NFC, trimmed: `names.NormalizeText`), and the
smallest wins a tie. So `Rock` beats `pop` (0x52 < 0x70), and `Zouk` beats
`Émo` (NFD input is normalized first). Genres that differ only in case are
different genres. It is the simplest total order, it depends on nothing but
the values, and it needs no casefold choice between two spellings. Pinned by
`TestInferMetadata` ("genre tie by bytes") and `TestImportMultiValuedYearGenre`;
mutation-checked.

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

The ffmpeg/ffprobe part is available since 2026-09-22: `media.Tools.Versions()`
returns the versions read from the tools at boot, never assumed (N-080).
Since 2026-09-23 it also returns the TagLib helper's two versions:
`Versions.Tags` (the helper, `2` since N-090) and `Versions.TagLib` (`2.3.2-musiclib1`,
N-083). The helper's version also covers the managed-field table and the
bytes it writes, so it is the "mapping dei tag" input of §2.1.

### N-011 · No CI check on ext4 — RESOLVED (local), OPEN (CI)
The Docker gate (`scripts/check.sh`) refuses to run unless `TMPDIR` is on
ext4 (magic 0xef53, named volume `musiclib_testdata`). A CI job running the
same scripts on a native Engine is still to be set up: see N-017.

### N-012 · The whole project must be containerized — RESOLVED
Build, vet, gofmt, race tests, fuzzing and shell lint run in Docker
(`Dockerfile`, `compose.yaml`, `scripts/`, `docs/docker.md`). ffmpeg is in
the images since 2026-09-22 (N-073), the TagLib helper since 2026-09-23
(N-083).

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

### N-025 · How to pin ffmpeg and TagLib — RESOLVED
`apt-get install ffmpeg=<ver>` is reproducible only with a pinned
`snapshot.debian.org`; alternatives are a static build or a source build
pinned by sha256. TagLib 2.x: source tarball pinned by sha256, built on the
runtime's Debian release. Both feed `render_version`.

**ffmpeg (2026-09-22):** a static source build of FFmpeg 8.1.3 pinned by
sha256, the same bytes in the test, dev and runtime images (N-073).

**TagLib (2026-09-23):** TagLib 2.3.2 from its release tarball pinned by
sha256, built static, linked into a fully static `musiclib-tags`: the same
bytes in the test, dev and runtime images, reproducible with `--no-cache`
(N-083).

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

The same holds for `testHook` in `internal/jobs/claim.go` (round 4): a
package-level named failpoint, nil in production, that must join the §12.2
failpoint mechanism in phase 3.

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

---

## `internal/media`: ffprobe/ffmpeg adapter (2026-09-22)

### N-073 · ffmpeg and ffprobe: a static source build of FFmpeg 8.1.3 — DECIDED
Resolves N-025 for ffmpeg. Candidates:
- **Debian packages from a pinned `snapshot.debian.org` date.** Rejected:
  - `ffmpeg` pulls in some hundred libraries (SDL, X11, VA-API, Vulkan...) the
    adapter never uses;
  - snapshot.debian.org is slow and rate-limited at build time;
  - the golang image and the slim runtime image would still link different
    point releases of the same shared libraries.
- **A static source build, pinned by sha256.** Chosen.

What is built (Dockerfile, stage `build-ffmpeg`):
- **FFmpeg 8.1.3** (2026-09-21), the latest point release of the newest branch
  that has had several point releases. 9.0 is seven weeks old. Pinned as
  `ffmpeg-8.1.3.tar.gz`, sha256
  `bd458826a039b48a9606e794554c75eb4c4984b84173f7afa3128eae89336f2b`. Its
  OpenPGP signature was verified against the FFmpeg release key
  `FCF9 86EA 15E6 E293 A564 4F10 B432 2F04 D676 58D8` in a throwaway
  container.
- **nasm 2.16.03** (x86 SIMD code), sha256
  `5bc940dd8a4245686976a8f7e96ba9340a0915f2d5b88356874890e207bdb581`. nasm
  publishes no checksums or signatures. Instead, the `.tar.xz` matches the
  sha256 in Debian's signed `nasm_2.16.03-1.dsc`, and the `.tar.gz` used has
  the same uncompressed content.
- **Configure:**
  - `--disable-autodetect`: nothing is picked up from the build image;
  - `--disable-shared --enable-static --extra-ldflags=-static`;
  - `--disable-debug --disable-doc --disable-network --disable-ffplay`;
  - `--disable-devices --enable-indev=lavfi`: lavfi is only for the test
    fixtures;
  - `--extra-version=musiclib1`.

  Every native demuxer, decoder and parser stays enabled, because
  classification by content (§7.2) must recognize any audio, supported or
  not.
- **The binaries are fully static** (`ldd`: "not a dynamic executable"), 29 MB
  each. The same files are copied into `toolchain` (so into `deps`, `test`
  and `dev`) and into `runtime`. Their sha256 is identical in the test and
  runtime images:
  - ffmpeg `3d67d1c2fc18f34ca7bb5cbb08becef864994047a263d97f35117f42da50be10`;
  - ffprobe `3f315e9f2ae071d5eceb147b201c9ae817dcc22cd974334e4f7ec0c230155c16`.

  A rebuild of the stage with `docker build --no-cache` produced the same
  two sha256: the build is bit-for-bit reproducible, on the same machine and
  architecture (amd64).
- **The version is `8.1.3-musiclib1`** (`media.PinnedVersion`). The suffix is
  the revision of the configure line, so a change of the build without a
  change of release still changes the version the boot checks (N-080). To
  bump: change the ARGs and the suffix, then `media.PinnedVersion`; the gate
  fails until both agree (`TestPinnedToolsInstalled`).

Consequences of the minimal configuration:
- **No zlib** (the Go image has no `zlib1g-dev`, and autodetect is off):
  - no PNG encoder or decoder. A PNG still probes as `png_pipe` or as an
    attached picture; the tests build PNG fixtures with Go's `image/png`.
    The "decodifica valida" of covers in §8.5 must therefore be done in Go
    (`image/png`, `image/jpeg`), never by ffmpeg. For the same reason the
    pinned ffmpeg cannot mux a PNG cover into a FLAC (`-c:v copy` fails: no
    decoder, so no dimensions); the fixture `flac-cover-png` is written by
    the tests' own FLAC codec (2026-09-23);
  - QuickTime compressed `moov` atoms (`cmov`) are unreadable, so such an
    M4A is ClassUnreadable, an album error. None of FLAC, MP3, AAC, ALAC or
    plain MP4 needs zlib.
- **No MP3 encoder:** FFmpeg has no native one. The MP3 fixtures come from
  **LAME 3.100**, built in stage `build-lame` from
  `lame-3.100.tar.gz` (sha256
  `ddfe36cab873794038ae2c1210557ad34857a4b6bdc515785d1da9e175b1da1e`, the
  sha256 of Debian's `lame_3.100.orig.tar.gz` in the `.dsc`). It goes into
  the toolchain images only, never into `runtime`, and ffmpeg is not linked
  against it.
- **Licensing:** the build uses no `--enable-gpl` or nonfree component. The
  distributed binaries are LGPL 2.1+, and the pinned tarball URL is their
  corresponding source.
- **Cost:** the build stage takes about 2.5 minutes on 12 CPUs, once per cache
  invalidation. The runtime image grows by 58 MB.

### N-074 · Decoder options beyond the letter of §8.4 — DECIDED (`crccheck` confirmed by the owner, 2026-09-23)
`decodeArgs` is §8.4's command plus three things.

**1. `-err_detect crccheck+explode` instead of `explode` — DECIDED (owner, 2026-09-23).**
- In FFmpeg's flag syntax a bare `explode` *replaces* the defaults, and the
  FLAC decoder checks the frame CRC-16 only with `crccheck` (or `compliant`).
- Measured on the pinned build: single-bit flips at 61 positions of a 3 s FLAC
  (xor 0x10). With `explode`, **57 of 61 decoded with exit status 0 to
  different samples**. With `crccheck+explode`, 0 of 61.
- Taken literally, §8.4 would therefore let corrupt FLAC files in as
  supported tracks, against §7.6 ("un file corrotto non entra come traccia
  supportata") and §8.1.
- The superset only adds the checksum verification the formats themselves
  carry. What it can additionally refuse: an MP3 written with CRC protection
  whose CRCs are wrong (some old encoders). An MP3 with correct CRCs
  (`lame -p`) passes, as tested.
- The owner confirmed it on 2026-09-23. Reverting would be one word, and the
  tests "flac with one flipped bit" would then fail.

**2. `-reinit_filter 0` — DECIDED.**
- Without it, when the decoded parameters change mid-stream, ffmpeg rebuilds
  the filter graph and inserts a resampler that converts to the first
  segment's rate and layout.
- Measured: a 44.1 kHz stereo MP3 followed by a 22.05 kHz mono one decoded
  with exit 0, the second part resampled and upmixed. This is exactly the
  "no -ar, -ac or normalization" that §8.4 forbids.
- With the option, the change is a fatal error (exit 234), and the file is
  refused (`media_decode`).

**3. `-protocol_whitelist fd -fd 3 -i fd:` — DECIDED.** N-075.

Mutation-checked: removing any of the three makes a test fail. So does
removing `-xerror`, or `-err_detect` altogether.

### N-075 · The tools read only the descriptor they are given — DECIDED
The adapter never puts a path on a tool's command line:
- the caller opens the file through `internal/fsops` (confined, regular files
  only) and passes the `*os.File`;
- the tool gets it as descriptor 3 and reads it with FFmpeg's `fd` protocol
  (`-fd 3 fd:`), the only protocol allowed (`-protocol_whitelist fd`);
- file names therefore never reach ffmpeg, so a name like `-i.flac`,
  `file:evil.flac` or a Unicode name cannot be misparsed (tested);
- no extension reaches ffmpeg either, so the format is decided by content
  alone (§4.2, §7.2);
- a demuxer that wants to open another file (an HLS playlist, a concat list,
  a mov external reference) cannot. HLS is not even detected without a
  standard extension, and concat lists are refused (EPERM for absolute
  names, EINVAL for relative ones). All of them are ClassUnreadable (tested).

The descriptor shares its file offset with the caller. `Probe` and
`AudioDigest` seek it to 0 before each tool run, and the caller must not use
it concurrently. The offset is unspecified afterwards.

For a FLAC with a trailing ID3v1 tag, the decoder's descriptor 3 is instead
the read end of a pipe, which the adapter fills with exactly the bytes
before the tag, read with pread (N-128). Still a descriptor, never a path,
and the same command line.

Every tool also gets an empty environment (no `FFREPORT` or `AV_LOG_*` from
the server's environment) and `/` as working directory.

### N-076 · How the Runner kills and waits — DECIDED, with ACCEPTED limits
- **Sequence:**
  - the tool is its own group leader (`Setpgid`), with `Pdeathsig: SIGKILL`;
  - the Runner waits for its exit with `waitid(WEXITED|WNOWAIT)`, which does
    not reap it;
  - it then sends SIGKILL to the group, on every outcome, success included;
  - only then does it reap the leader.

  While the leader is an unreaped zombie its pid, which is the group id,
  cannot be reused, so the group kill can never hit an unrelated process.
  After reaping, the Runner polls `kill(-pgid, 0)` until ESRCH, zombies
  included, bounded by 10 s. The slot is released only after that.
- **Tested for real:**
  - a shell with two background children, cancelled: none survives;
  - a straggler left by a tool that exited 0: killed at once;
  - a process that joined the group and stays a zombie until the test reaps
    it: `Run` is still waiting;
  - the parent killed by SIGKILL: the tool dies.
- **Limits:**
  - `Pdeathsig` fires when the *thread* that forked the child exits. Go only
    ends a thread when a goroutine exits while locked to it with
    `runtime.LockOSThread`. Nothing in the application does that. A future
    use must not start tools from such a goroutine.
  - A process that leaves the group (`setsid`, `setpgid`) escapes the group
    kill. ffmpeg, ffprobe and the TagLib helper never do.
  - Orphans are reaped by PID 1. Compose runs the app with `init: true`
    (tini). Without an init, a group member orphaned by the kill would stay
    a zombie, and `Run` would fail with `media_io` after 10 s instead of
    returning.
  - Stderr keeps the *first* 64 KiB (§8.5) and drains the rest; the last lines
    of a very verbose tool are lost.

### N-077 · Probe classification: the readings of §7.2/§8.1 — DECIDED (the `other_stream` refusal confirmed by the owner, 2026-09-23)
- **Classes:**
  - `audio`: supported;
  - `unsupported_audio`: it has audio, but is not supported;
  - `no_audio`: probed fine, no audio stream;
  - `unreadable`: ffprobe could not read it.

  The last one covers empty files, text, PDF and M3U alike (ffprobe answers
  "Invalid data found"), so they are not `no_audio`. §7.2 sends them to the
  importer's rule: an attachment, or an album error with a known audio
  extension.
- **Tool failure versus unreadable content:** only exit status 1 *with* the
  JSON error object of `-show_error` is a classification. A bad option, a
  crash, another exit status, or exit 1 without the object (even with
  plausible output) is an error of the tool. EIO and ENOMEM in the error
  object are errors of the machine, not of the file.
- **M4A:** any file of the mov demuxer (`mov,mp4,m4a,3gp,3g2,mj2`) with one
  AAC or ALAC stream, whatever its brand (`M4A `, `isom`, `mp42`...). Many
  real `.m4a` files are not branded `M4A `, and §8.1 speaks of the container,
  not the brand.
- **Streams other than audio and attached pictures (subtitle, data,
  QuickTime chapter text tracks) — DECIDED (owner, 2026-09-23).** They make the file
  `unsupported_audio` (`other_stream`). §8.1 lists only "no real video, no
  multistream audio". Refusing is the conservative reading: the file is
  visibly refused, rather than accepted with content that the tag writer and
  the verification were never tested on. Audiobook-style M4A with chapter
  tracks would be refused. Video chapter-thumbnail tracks, on the other
  hand, carry the attached-picture flag and are therefore accepted; only
  text chapter tracks are refused (`other_stream`).
- **Encryption (DRM):**
  - ffprobe reports a CENC-encrypted AAC like a clear one: codec `aac`, tag
    `mp4a`, no stream flag. The probe therefore reads the first 16 packets
    (`-read_intervals %+#16`) and refuses the file if a packet carries
    `Encryption info` side data (`encrypted`, tested with FFmpeg's own CENC
    output).
  - A stream with a clear lead longer than 16 packets passes the probe, but
    encrypted packets do not decode: the full decode fails. Tested: the CENC
    file fails with exit 183 even without the probe check.
  - FairPlay (`drms`) is unknown to FFmpeg: no codec, so
    `unsupported_format`.
- **MP2 or MP1 in a `.mp3`** is `unsupported_format`: the mp3 demuxer reads
  it, but the codec is not MP3.
- **Layout:** FFmpeg's own description (`mono`, `stereo`, `5.1`,
  `5.1(side)`...), or `unknown:<channels>` when the file declares none
  (ffprobe omits the field or prints `unknown`). The description depends on
  the FFmpeg version, which is part of `render_version`.

### N-078 · Declared lengths: truncation that the decoder does not see — DECIDED (the stale-header refusal confirmed by the owner, 2026-09-23), two ACCEPTED
- **The finding:** an MP3 cut in the middle decodes with exit status 0. The
  demuxer drops the incomplete last frame silently, and nothing in ffmpeg
  turns that into an error.
- **The rule:** when the container declares an exact length, `AudioDigest`
  requires the decoded frame count to equal it (`media_decode`, "decoded N
  frames, the container declares M"):
  - **FLAC:** the STREAMINFO total. It is absent when a streaming encoder
    wrote 0; then nothing is checked, as tested.
  - **MP3:** the Xing, Info or VBRI frame count, net of the LAME encoder delay
    and padding.
  - **M4A:** no check. An AAC decode keeps the encoder's end padding (441,344
    frames for a 441,000-frame source), by design of FFmpeg's mov/AAC path. A
    truncated M4A already fails in the demuxer, because its sample table
    points past the end (tested).
- **How the MP3 case is known:** ffprobe prints an estimated duration
  exactly like an exact one. The only difference is libavformat's warning
  "Estimating duration from bitrate, this may be inaccurate". The probe runs
  at `-loglevel warning` and recognizes that exact line
  (`mp3EstimatedLine`). A declared count must also be a whole number of
  frames.
  - This reads stderr, but never to declare success. If the wording changed,
    an estimate would be taken as exact and complete files would be refused,
    never the reverse.
  - `TestMP3EstimationWarningOfThePinnedTool` pins the line on the real tool,
    so a bump that changes it fails the gate.
  - A crafted file could forge the line only through a warning that quotes
    its own metadata, and would only disable its own truncation check.
- **ACCEPTED:** an MP3 without a Xing/Info/VBRI header that is cut in the
  middle cannot be detected (no declared length). LAME writes the header by
  default, for CBR too.
- **DECIDED (owner, 2026-09-23):** an MP3 whose Xing header is stale is
  refused as "truncated". This happens with a file cut or joined by a tool that did not
  update the header. It is the conservative outcome, and the file is not
  lost: it stays in `/import`.
- **ACCEPTED:** FFmpeg's gapless trimming drops everything past the declared
  count when the file is *longer* than its LAME header says (within 1/16 of
  the size; beyond that FFmpeg ignores the header). So the digest does not
  cover such a tail.

### N-079 · What the digest guarantees, and what it costs — DECIDED
- **Comparable only on the same binary and the same machine.** FFmpeg selects
  its SIMD code at run time from the CPU flags, and the float decoders (AAC,
  MP3) may round differently on different CPUs. The integer decoders (FLAC,
  ALAC) are bit-exact everywhere: the test checks their PCM against an
  independent computation in Go.
- That matches §8.4: the digest is transient, computed twice in the same
  render (before and after the tags), and never stored.
- **One thread each** for the decoder (`-threads 1` before `-i`), the encoder
  (after it) and the filter graph (`-filter_threads 1`, §6.1). FFmpeg 8's CLI
  still runs demuxer, decoder and muxer in separate internal threads, which
  cannot be configured.
- **Measured cost** in the dev container (Docker Desktop, WSL2, Intel
  i5-10400F; `BenchmarkAudioDigest5MinFLAC`, 3 × 10 runs), for 5 minutes of
  44.1 kHz stereo:

  | Input | Time |
  |---|---|
  | FLAC 16-bit, 36 MB (probe + decode + SHA-256, 211.7 MB of f64 PCM) | **0.56 s** |
  | ALAC | 0.55 s |
  | AAC 256k | 0.55 s |
  | MP3 V0 | 0.61 s |

  The last three were timed on the same decode command outside Go. A render
  that digests each track twice spends about 1.1 s per 5 minutes of audio,
  per worker.

  A FLAC with a trailing ID3v1, which the decoder reads through a pipe
  (N-128), takes the same time within noise (0.57 to 0.60 s, round 7).

### N-080 · Tool versions for `render_version` — DECIDED
`media.NewTools` runs `ffmpeg -version` and `ffprobe -version` through the
Runner at boot and parses the token after "version" on the first line. It
refuses the tools if:
- a tool is missing, or its line does not name the expected tool (for
  example ffprobe installed at the ffmpeg path): `media_tool_unavailable`;
- the token is not `media.PinnedVersion`: `media_tool_version`.

The boot fails in both cases (§11.1 step 3, `cmd/musiclibd.checkTools`).
`Tools.Versions()` returns what was read. `render_version` itself is still
N-010's.

### N-081 · Where the list of known audio extensions lives — DECIDED
The fixed list of §7.2 is `media.KnownAudioExtensions()`, in §7.2's order,
pinned by `TestKnownAudioExtensions`. It lives here because it defines when
a failed probe means "corrupt audio". The rule that applies it (album error
versus attachment) is the importer's. `HasKnownAudioExtension` compares the
last extension ASCII case-insensitively: `.FLAC` is known, fullwidth or
Cyrillic look-alikes are not.

### N-082 · Tool stderr is kept out of error messages — DECIDED
`media.Error.Stderr` holds up to 64 KiB of the tool's standard error, but
`Error()` never includes it. A decoder's warnings can quote tag contents,
which §11.1 keeps out of the logs by default. A later round that wants
stderr in a job's `error_message` reads the field explicitly.

---

## `native/musiclib-tags` and the tag adapter (2026-09-23)

### N-083 · TagLib 2.3.2: pin, build and reproducibility — DECIDED
Resolves N-025 for TagLib. Dockerfile stage `build-tags`.
- **Release:** TagLib 2.3.2 (2026-09-05), the latest point release; the 2.3
  branch has had two. Pinned as `taglib-2.3.2.tar.gz` (GitHub release
  asset), sha256
  `3ca2d8afaa7f1cf7f6ed10e511ebc368bfacd6dcaa3dbfa690b89e502e8963dc`.
  TagLib publishes **no signature**. The sha256 matches three sources:
  - GitHub's digest of the release asset;
  - taglib.org/releases, which serves the same bytes;
  - Homebrew's `taglib.rb`, which pins it independently.
- **utfcpp**, TagLib 2's one dependency, is bundled in the tarball
  (`3rdparty/utfcpp`): there is no separate download.
- **CMake 4.4.3**, Kitware's binary release, sha256
  `d6c83076c575bc00b823522ac974bda66d0af05d6ddc30e739c12385cf32c6cc`. The
  signed list `cmake-4.4.3-SHA-256.txt.asc` was checked in a throwaway
  container:
  - "Good signature from Brad King";
  - primary key `CBA2 3971 357C 2E65 90D9 EFD3 EC8F EF3A 7BFB 4EDA`;
  - signing subkey `C6C2 6532 4BBE BDC3 50B5 13D0 2D2C EF10 3492 1684`.

  The keyserver copy says that the key has expired; gpg still reports the
  signature as good. CMake runs only in the build stage.
- **Modules:**
  - on: `WITH_VORBIS` (FLAC), `WITH_MP4` and `WITH_APE` (MPEG and ID3 are
    always built);
  - off: ASF, DSF, Matroska, MOD, RIFF, Shorten, TrueAudio, bindings, tests
    and examples.
- **No zlib.** Only compressed ID3v2 frames need it. Without it they are
  `UnknownFrame`s, which the Phase 4 MP3 reader must report as opaque, so
  that a render refuses them rather than losing them. FLAC and MP4 never
  need zlib.
- **Two builds of the same sources:**
  - release: static `libtag.a` and a fully static helper, libc and
    libstdc++ included (`ldd`: "not a dynamic executable", 3.0 MB);
  - asan: TagLib and the helper under ASan and UBSan,
    `-fno-sanitize-recover=all`, for the hostile-input tests only (25 MB,
    toolchain images only, never in `runtime`).
- **Optimization:** `-O2` goes in `CMAKE_CXX_FLAGS`, with
  `CMAKE_CXX_FLAGS_RELEASE=-DNDEBUG`, because the Release build type would
  otherwise add `-O3`. `NDEBUG` also keeps TagLib's `debug()` messages off
  standard error, where they would break the failure protocol (N-084).
- **Reproducibility:** `-ffile-prefix-map` in TagLib's and the helper's
  flags keeps `/build` out of the objects. Two `docker build --no-cache
  --target build-tags` gave the same sha256. Helper version `2` (round 6,
  N-090), checked again with a `--no-cache` build against the images:
  - `musiclib-tags`
    `a80552042e2ae4f158a403f9887ace816c46a026592921ef3bee51c711d14a32`;
  - `musiclib-tags-asan`
    `5f29bcd2932709095b962502c6daa42d7177e9dcfa94c9dec66fa9fb5dc69f6a`;
  - `libtag.a`
    `5b1358dfd51c8654d030b85666788309e65f159f2862192d5b5f048465fb976f`
    (unchanged: TagLib's build did not change).

  Version `1` was `1f390a3271bd38b0425ae5cc4c68f6f50f68384cab28c879a3eb422eec0cf4b2`
  (release) and `630c65c612a643e16042c8b4fad4705cce37e37f576d18acfccd0b9b1369f9cb`
  (ASan). The `runtime` image and the `test` and `dev` images hold the same
  `/usr/local/bin/musiclib-tags`. As for ffmpeg (N-073), this holds on the
  same machine and architecture (amd64).
- **The unit tests of the helper's own parsers** (`make check`: UTF-8,
  base64, JSON, SHA-256) run under ASan and UBSan in the build stage, which
  fails if they fail.
- **Version string:** `2.3.2-musiclib1`. The first part is
  `TagLib::runtimeVersion()`, read at run time from the linked library; the
  suffix is `TAGLIB_BUILD_REVISION`, the revision of the cmake line (like
  FFmpeg's `--extra-version`). The helper's own version is `1`
  (`src/version.h`), `2` since the ID3 strip of N-090 (round 6). `musiclib-tags version` prints both, and
  `media.NewTools` requires `media.PinnedTagsVersion` and
  `media.PinnedTagLibVersion`: the boot refuses anything else
  (`media_tool_version`), and so does the gate (`TestPinnedToolsInstalled`).
- **Cost:** a `--no-cache` build of the stage (CMake download, TagLib twice,
  the helper twice) takes 70 to 80 s on 12 CPUs, once per cache
  invalidation. The runtime image grows by 3 MB.
- **Checked end to end:** `docker compose --profile app up --wait` on the
  runtime image logs `media tools verified` with `musiclib_tags` `1` and
  `taglib` `2.3.2-musiclib1`, in the hardened container (read-only root,
  no capabilities), and turns ready.

### N-084 · The helper's protocol — DECIDED
- `musiclib-tags <op>`, with `op` one of `version`, `inspect`,
  `extract-images` and `write-managed-tags`. Any other argument count or
  operation is `invalid_request`. The request is JSON on standard input, at
  most 256 KiB, read before anything else.
- **Success:** exit 0 and one JSON value on standard output.
- **Typed failure:** exit 3 and `{"code","message"}` on standard error. The
  codes are `invalid_request`, `bad_descriptor`, `unsupported_format`,
  `format_mismatch`, `corrupt`, `opaque_field`, `too_large`,
  `picture_not_found`, `io` and `internal`, mapped one to one to
  `media_tags_*`.
  - The message is cut to 4 KiB, on a character boundary. It can quote a
    request string, and the JSON must fit in the 64 KiB of stderr the Runner
    keeps (§8.5). Found by the raw-request tests: a 100 KB format name gave
    `media_tool_failed` instead of `unsupported_format`.
  - Any other exit status, a signal, or stderr that is not exactly one such
    object is `media_tool_failed`. A sanitizer finding exits 86.
- **Files are descriptors, never paths** (N-075):
  - fd 3 is the audio file: readable for `inspect` and `extract-images`,
    read-write and not `O_APPEND` for a write;
  - `extract-images` writes picture *i* of the request to fd 4+*i*. These
    are empty regular files that the caller creates through fsops. That is
    the reading of §8.1's "file estratti in una directory assegnata"
    without giving the helper a directory: the caller assigns every output;
  - a write reads the cover from fd 4.

  Every descriptor must be a regular file (`fstat`), so a FIFO, a device or
  a directory is `bad_descriptor` without a read, and the helper cannot
  block. Every destination is checked, and every index resolved, before the
  first byte is written.
- **Every key of a write request is required;** `null` means absent. A field
  the caller forgot therefore cannot be removed silently. Text must be
  non-empty and without NUL; numbers are integers in 1..2³¹-1; the cover's
  MIME type is 1..255 printable ASCII characters.
- **The helper's JSON parser is its own and strict:**
  - integers only, depth 8, no duplicate keys, strict UTF-8, no lone
    surrogates;
  - its writer refuses to print invalid UTF-8.

  On the Go side, `decodeStrict` checks that the output is valid UTF-8
  before decoding: `encoding/json` would replace invalid bytes silently. It
  also rejects unknown fields and trailing data.
- **I/O goes through `FdStream`**, an `IOStream` on pread and pwrite. TagLib's
  `FileStream` could not be used, because it ignores errors of `fwrite` and
  `ftruncate`. In `FdStream` the first error sticks: every later operation
  is a no-op, and the save is reported as `io`.
  - A write that fails half way can leave the staging copy partially
    rewritten. The caller discards it on any error (§9.1). Tested with
    `ulimit -f`, which gives EFBIG in the middle of a write.
  - Moving the audio frames forward (the metadata grows) copies back to
    front; moving them back (it shrinks by more than TagLib's padding
    threshold) copies front to back.
- **Timeout:** the 30 s of §8.5 (`InspectTimeout`), for all three
  operations.

### N-085 · The FLAC reader is independent of TagLib; TagLib is the writer — DECIDED
TagLib's reading silently drops or alters what it cannot represent, and
saving writes back only what it read. The helper therefore reads the metadata
with its own strict parser, written from RFC 9639, and uses TagLib only to
write. Everything TagLib 2.3.2 would drop or alter was found in its source,
and the reader reports it as opaque or corrupt:
- an entry without `=`, or with an empty key;
- a key outside 0x20..0x7D. TagLib's `String` also truncates it at a NUL;
- invalid UTF-8: TagLib replaces the value with an empty string, which is
  then dropped;
- a NUL in a value: TagLib truncates it;
- more than 50,000 fields: TagLib keeps none (`MAX_XIPH_COMMENT_FIELD_COUNT`,
  checked with `>`). The writer also refuses to *produce* such a comment
  (`too_large`): a file at the limit plus new managed fields could not be
  read back;
- a field count or entry length beyond the block: TagLib drops the rest
  (`corrupt`);
- a second Vorbis comment block: TagLib discards it;
- iXML and bext APPLICATION blocks (`riff`+`iXML`/`bext`, or `iXML`/`bext`
  directly): TagLib 2.3 extracts them on read and re-renders them in
  another form and position on save;
- a new block of 16 MiB or more: dropped silently on save. The helper checks
  the rendered comment and the cover block before saving;
- `Tag::duplicate` in `save()` when the file had no comment block: it
  deletes a DATE that is not a number ("Spring 1999"). `writeFlac` therefore
  saves a second time;
- ID3 tags in a FLAC file: TagLib reads them and would rewrite them on save.
  They are reported, and a write strips them (N-090).

Checks around the write:
- **Before saving:** TagLib must read the same file as the reader: the same
  ID3 tags (an ID3v2 tag of the same extent, N-090), the same iXML or bext, and the same field map and vendor. The
  field map and vendor are compared only when the comment has no opaque
  entry. Then TagLib's field map, after the managed fields were applied,
  must equal the expected one.
- **After saving:** the file is read back by the reader, and compared with
  what was asked: fields, vendor, kept blocks, and cover.

Either failing is `internal`. The mutation tests show that each layer
catches faults on its own:
- the second save removed: the read-back fails;
- the read-back also removed: the Go `VerifyTags` fails;
- the cover block check removed: TagLib drops the block, and the read-back
  fails;
- the UTF-8 classification of values removed: the TagLib cross-check fails.

TagLib pads with 4 KiB, and resets the padding to 4 KiB when a write leaves
more than max(1% of the file, 4 KiB), capped at 1 MiB. A write whose padding
stays within that bound rewrites the metadata in place, and the audio does
not move. The output is a function of the input bytes and the request only:
byte-identical for the same input (tested three times on the same file),
and a second identical write gives the same bytes.

### N-086 · Canonical unmanaged form, opaque reasons, and what VerifyTags compares — DECIDED
- **Unmanaged keys:**
  - `vorbis:KEY`: the key in upper case, values in file order;
  - `vorbis.vendor`: only when not empty;
  - `flac.blocks`: `type:sha256` of every block a write keeps byte for byte
    (STREAMINFO, SEEKTABLE, APPLICATION, CUESHEET, reserved types), in file
    order.

  Sorted by key. The written comment is ordered by key, as TagLib's field
  map is. The order between different keys is not preserved, but the order
  of the values of one key is: §8.3's "preservazione semantica".
- An empty value counts as absent, as in TagLib. Bytes after the last Vorbis
  entry are ignored: they are not a field.
- **Opaque reasons** (`inspection.h`): `malformed_entry`, `invalid_key`,
  `invalid_utf8`, `nul_byte`, `duplicate_block`, `foreign_metadata`,
  `foreign_tag`, `invalid_picture`. `removed=true` means that a write
  removes the field anyway (a managed key, alias, sort key or picture, and
  since helper version `2` an ID3 tag in a FLAC, N-090).
  Only fields with `removed=false` block a write (`opaque_field`, §8.3:
  "provocano un errore di render"). `Inspection.Blocking()` lists them.
- **`VerifyTags` (§9.1 step 6)** requires:
  - the managed fields exactly as asked, and no conflict left;
  - exactly the expected cover, or no picture;
  - no opaque field after the write, and **no blocking opaque field
    before** it. An opaque field is absent from `Unmanaged` on both sides,
    so the comparison could not see it lost. Mutation-found: with the
    helper's refusal disabled, several hostile writes succeeded, and the
    first `VerifyTags` accepted them;
  - the unmanaged fields equal, key by key and value by value.

  Only managed fields, their aliases and sort keys, pictures, and the ID3
  tags of a FLAC (N-090) can differ; they are outside `Unmanaged` by
  construction. The ID3v1 migration of §8.3 is MP3 only (N-094). Because `flac.blocks` covers STREAMINFO, whose
  MD5 is that of the samples, a writer that changed the audio of a FLAC
  would also fail this comparison, besides the digest.

### N-087 · Pictures are wholly managed; cover attributes — DECIDED
- §8.2: "una cover assente nel DB significa nessuna immagine incorporata";
  §8.5: "unica cover incorporata". A write therefore removes every picture
  of every type: PICTURE blocks, METADATA_BLOCK_PICTURE, COVERART and
  COVERARTMIME. It then adds at most one front cover (type 3), with an
  empty description. The originals keep every picture (§7.4).
- **Inspect lists every picture**, with location, type, MIME type,
  dimensions and SHA-256. `extract-images` writes the embedded bytes
  unchanged. A legacy `COVERART` has type 0 and no MIME type.
- **The picture's width, height, depth and colors are computed as libFLAC's
  metaflac computes them** (`share/grabbag/picture.c`), from the image
  header, in Go:
  - JPEG: 8-bit precision × components (gray 8, YCbCr 24, CMYK 32);
  - PNG: IHDR bit depth × samples per pixel, and 24 for any palette image,
    with the palette size as the number of colors.

  The first draft derived the depth from Go's color model, which gets RGB
  (32 instead of 24), gray+alpha, low-bit gray and 48-bit RGB wrong. It was
  fixed before any file was written.
- **The adapter checks only the header** (`DecodeConfig`), and that it
  matches the declared format. A full decode of the cover (§8.5 "decodifica
  valida", 40 Mpixel) is the caller's check when it accepts a cover.
- **Observation for the importer (§7.4):** ffmpeg writes its attached
  picture into a FLAC as type 0, "Other", unless told otherwise. Real files
  muxed that way have no front cover, and the selection falls to "la prima
  immagine incorporata valida".

### N-088 · The FLAC alias and sort table — DECIDED
The table is `native/musiclib-tags/src/fields.h` (§8.2: "tabella costante
dell'adapter, coperta da fixture"). Canonical key, then its aliases in
reading order:

| Field | Canonical | Aliases (read as a fallback, always removed) |
|---|---|---|
| album artist | `ALBUMARTIST` | `ALBUM ARTIST`, `ALBUM_ARTIST` |
| track | `TRACKNUMBER` | `TRACKNUM` (TagLib's own alias) |
| track total | `TRACKTOTAL` | `TOTALTRACKS`; then the `/M` of `TRACKNUMBER=N/M` |
| disc | `DISCNUMBER` | — |
| disc total | `DISCTOTAL` | `TOTALDISCS`; then the `/M` of `DISCNUMBER=N/M` |
| date | `DATE` | `YEAR` (TagLib's own alias) |

- `TITLE`, `ARTIST`, `ALBUM`, `GENRE` and `COMPILATION` have no alias.
- The sort keys removed are `TITLESORT`, `ARTISTSORT`, `ALBUMARTISTSORT` and
  `ALBUMSORT`. `COMPOSERSORT` and every other sort key are unmanaged, and
  stay.
- Sources that disagree with the chosen one are reported as a conflict.
- A write sets only canonical keys, through explicit field names: no
  property map and no `Tag::set*`, whose aliases and conversions are
  TagLib's.
- Totals come from the caller (§8.2: "massimo numero presente").
- `TagNumber` parses one value of ASCII digits. A number that is too large
  becomes `MaxInt32`, so that the caller refuses it rather than truncating
  (§7.3).

### N-089 · Compilation false removes the field — DECIDED
`true` is written as `COMPILATION=1`, and `false` removes the field. §8.2
says only "valore assente significa rimozione". Since the DB column is a
non-null boolean, false is the absence of the flag. `TagBool` reads "1",
"0", "true" and "false", case-insensitively.

### N-090 · ID3 tags inside a FLAC — RESOLVED (owner decision 2026-09-23; importer round 5, helper round 6)
**The rule.** ID3 is not part of the FLAC format, and stale TIT2/TPE1 values
next to the Vorbis comments would contradict §8.2. A write of a FLAC removes
the one ID3v2 tag before `fLaC` and the ID3v1 tag at the end, by a declared
rule of the adapter: an exception to §8.3, like the MP3 ID3v1 migration.
(Stacked ID3v2 tags before `fLaC` are refused, see the ACCEPTED limits.)
Everything else is kept exactly as before (§8.2, §8.3): Vorbis fields,
vendor, kept blocks, audio.

**The helper (`native/musiclib-tags`, version `2`).**
- **Inspect** still reports each tag, as `{"id3v2" or "id3v1", foreign_tag,
  removed=true}`. `Inspection.Blocking()` no longer includes them.
- **The reader** (`readRawFlac`) locates the ID3v2 tag without parsing it.
  Its extent must be exactly the one TagLib's `ID3v2::Header` computes,
  because that is what TagLib removes:
  - a size byte ≥ 0x80, or a version or revision of 0xFF (TagLib reads
    those as a 10-byte tag of size 0), is `corrupt`;
  - a declared size past the end of the file is `corrupt`;
  - the footer flag adds 10 bytes;
  - `checkTagLibAgrees` also compares TagLib's `completeTagSize()` with the
    reader's (`internal` otherwise).
- **An ID3v1 tag** that would start inside the metadata blocks is `corrupt`:
  TagLib's save truncates the file where it starts.
- **The write** calls `FLAC::File::strip(ID3v1 | ID3v2)` before the first
  save:
  - The save then removes the ID3v2 bytes and truncates the ID3v1.
  - Stripping before the save also keeps `Tag::duplicate` (a file with no
    comment block) from copying an ID3 title, comment, genre or date into
    the new comment (`TestTagsFLACStripsID3WithoutComment`).
- **A second save follows whenever a tag was stripped.** TagLib chooses the
  padding against `length()` before it removes the ID3 bytes. With a large
  ID3v2 (more than 1% of the file) the first save keeps a padding that a
  later write of the output would reset. The second save chooses it against
  the final length. So the output is byte for byte the output of the same
  file without its ID3 tags, and a second write gives the same bytes.
- **The read-back** fails (`internal`) if an ID3v2 or ID3v1 tag is still
  there, before any other comparison.

**Go.**
- `media.VerifyTags` excludes exactly this removal. ID3 tags are never in
  `Unmanaged`: they are removed opaque fields, not blocking before the
  write, and forbidden after it (no opaque field at all). Its doc lists the
  exclusions: managed fields, aliases, sort keys, pictures, and the ID3 tags
  of a FLAC.
- **The importer** gives the `flac_id3_tag` warning for each opaque field
  with reason `foreign_tag` and `removed=true`. The message says the library
  copy will not carry the tag, and that ID3 frames without a Vorbis
  equivalent are dropped. Every field of `Blocking()` still refuses the file,
  so an ID3 tag that a helper did not declare removed would be refused
  (`TestTagWarnings`).
- **The version bump** (`kHelperVersion` and `media.PinnedTagsVersion` `2`)
  feeds `render_version` (N-010) and the boot check.

**What is lost, by the owner's decision.** No ID3 frame is migrated into a
Vorbis comment, and none is kept in the library copy:
- frames with a Vorbis equivalent (TIT2, TPE1, TALB, TRCK, TDRC, TCON,
  COMM...): the Vorbis comment is authoritative, and the managed fields come
  from the DB;
- frames without one: TXXX user fields, PRIV, UFID, APIC pictures, USLT
  lyrics, POPM ratings, GEOB objects, and every other frame;
- the whole ID3v1 tag (its comment and its genre byte included). This
  differs from the MP3 rule of §8.3, which keeps an ID3v1 comment in COMM:
  for FLAC the owner decided removal, and there is no ID3v2 in the output
  to hold it.

The original file stays unchanged in the blob store (§7.5), so nothing is
lost from the library itself; only the rendered copy lacks the tags.
`TestTagsFLACStripsID3` pins it with TXXX, PRIV and APIC frames: after the
write, the Vorbis comment has no new field.

**Tests** (`internal/media/tags_id3_test.go`, release and ASan/UBSan
helper). Cases:
- a leading ID3v2, a trailing ID3v1, and both;
- an ID3v2 with a footer, of size 0, with a garbage body (unsynchronisation
  and extended-header flags), and with garbage frames;
- an 800 KB ID3v2.

For each one:
- the inspection equals the file's without ID3, apart from the removed
  opaque fields;
- the output decodes to the samples of the file without ID3;
- the output is byte for byte the reference's (the same file without ID3,
  written the same way);
- no ID3 is left;
- another copy gives the same bytes, and a second write too.

The hostile table adds:
- an ID3v2 larger than the file, with and without a footer;
- an ID3v2 version or revision of 0xFF;
- an ID3v1 inside the metadata blocks.

The earlier non-synchsafe case stays `corrupt`.

**Mutation-checked:**
- no strip: the read-back fails;
- no strip and no read-back checks: `VerifyTags` fails;
- no second save: the 800 KB case differs from the reference;
- no 0xFF check: the extent cross-check fails the inspection;
- no ID3v1 overlap check;
- in the importer: warning without `removed`, and no refusal.

**ACCEPTED limits:**
- **The ID3v1 detection mirrors TagLib's heuristic:** "TAG" 128 bytes before
  the end, unless those are the "TAG" of an "APETAGEX" that starts 131 bytes
  before the end. A FLAC whose audio happens to have those bytes there
  (about 1 in 16 million) is taken for a FLAC with an ID3v1. Since N-128 the
  full decode at import leaves those 128 bytes out, cuts into the last
  frame, and refuses the file (`corrupt_audio`), as tested with such a
  file. Before N-128 it was imported with a misleading warning, and its
  render failed at §9.1 step 6. Nothing wrong is published either way.
- **Two stacked ID3v1 tags:** only the last is detected. Since N-128 the
  full decode reads the other one as bytes after the audio and refuses the
  file at import (`corrupt_audio`). A render would strip the last one, and
  its read-back would find the next one and fail with `media_tags_internal`.
  Nothing is lost silently.
- **Stacked ID3v2 tags before `fLaC`:** the reader locates one ID3v2 tag and
  then requires `fLaC`, so a second tag is `format_mismatch`: the import
  refuses the file (`corrupt_audio`, "not a FLAC stream the tag reader
  accepts"). The helper strips one tag, as TagLib does.
- **An ID3v2 appended at the end of the file** (footer `3DI`) is detected
  neither by TagLib's FLAC code nor by the reader, so a render would not
  strip it. It is bytes after the audio, and the full decode refuses the
  file at import (`corrupt_audio`, N-128).

### N-091 · A cover larger than about 16 MiB cannot go into a FLAC — DECIDED (owner, 2026-09-23)
**Round 5 (done):** `media.MaxEmbeddedCover(audioFormat, coverFormat)` and
`media.EmbeddedCoverFits` hold the limit: for FLAC `0xFFFFFF − 32 − len(MIME)`
bytes (the description is always empty, N-087), that is 16,777,173 for JPEG and
16,777,174 for PNG, exactly the helper's check in `writeFlac`
(`TestMaxEmbeddedCoverIsTheHelpersLimit` writes a cover of exactly the limit
through the real helper and has one more byte refused). MP3 and M4A have no
limit yet and are refused (`media_tags_unsupported_format`): a cover is never
accepted for a format whose writer does not exist. `importer.CoverFits` adapts
it to `catalog.CoverFits`, and the wiring passes it to `catalog.New`.

**How it interacts with §7.4:** the importer asks the same question during the
selection, for every audio format of the album. A valid JPEG/PNG that does not
fit is **not selected**: the job gets a `cover_not_embeddable` warning, the
selection moves on to the next candidate of §7.4's order, and the image, if
external, stays an attachment like every other file. An embedded picture fits
by construction in FLAC (it came out of a block of the same limit). The
catalog's check at the commit remains the second line. Tested with a 16.85 MB
uncompressed PNG `cover.png` (`TestCoverSkipped`); mutation-checked.

**Owner decision (2026-09-23):** a per-format limit of the embeddable cover,
enforced when a cover is chosen (import) or uploaded or selected (the cover
endpoints of §10.2), never discovered by a render. An image that does not
fit stays an attachment (§7.4); it is only not selectable as the cover.

**Done in round 4:** the catalog asks the question through `catalog.CoverFits`,
a function given to `catalog.New`, once per audio format of the album
(`CommitImport` today, the cover endpoints later). It invents no limit
beyond "embeddable in every format of the album" (N-106). **Pending:** the
function itself belongs to the tag adapter, which knows the limits (FLAC:
the metadata block below, 16 MiB − 1 minus the picture header; MP3 and M4A
in Phase 4); it is written and wired with the importer round. The helper's
refusals below stay as the last line of defence.

§8.5 accepts covers up to 20 MiB, but a FLAC metadata block holds at most
16 MiB - 1: the picture structure and a MIME type of about 40 bytes, plus
the image. Between the two, the render of a FLAC album fails with
`media_tags_too_large`. The helper refuses covers of 16 MiB or more before
reading them; the FLAC writer refuses the rest before writing. That is
conservative: nothing is embedded wrongly or silently dropped (TagLib would
drop the block). The owner decides whether:
- the cover limit for FLAC albums should be lower;
- such a cover should stay external only;
- or the error should stay.

### N-092 · Invalid UTF-8 in unmanaged Vorbis fields — DECIDED (owner, 2026-09-23); implementation in the importer round
**Round 5 (done):** every track is inspected on its verified copy, and any
field of `Inspection.Blocking()` other than the N-090 ID3 tags fails the import
with `unrenderable_tag`, whose message names the file, the field key (for
example `vorbis:COMMENT`) and the reason (`invalid_utf8`, `nul_byte`,
`malformed_entry`, `invalid_key`, `duplicate_block`, `foreign_metadata`).
Tested with a Latin-1 COMMENT; mutation-checked.

**Owner decision (2026-09-23):** the importer refuses such a file up front,
with a typed error that names the file and the field, using
`Inspection.Blocking()`. The render keeps refusing (`opaque_field`) as a
second line; nothing is dropped silently. The import commit is compatible:
it receives only files the importer accepted (N-106).

A FLAC whose unmanaged field is not valid UTF-8 (Latin-1 tags from old
tools) imports: the file and its audio are fine. It cannot be rendered,
though: the field is opaque (N-085), and TagLib would empty it. The same
holds for a NUL in a value, a malformed entry and an invalid key. The
choice is the owner's:
- as now: the render fails with `media_tags_opaque_field`, and the album
  stays unpublished until its source is fixed;
- the importer refuses such files up front: `Inspection.Blocking()` exists
  for that;
- the render drops the field. That is a silent loss, which §8.3 forbids
  unless the owner decides otherwise.

The same field in a *managed* key (a Latin-1 `TITLE`) is no problem: the
write replaces it.

### N-093 · The pinned ffmpeg refuses FLAC files with an invalid PICTURE block — ACCEPTED
FFmpeg's FLAC demuxer fails to open a file whose PICTURE block does not
parse ("Error parsing attached picture"), whatever the audio. Probe and
`AudioDigest` therefore refuse such a file. With its `.flac` extension, it is
an album error (§7.2), and it never reaches a render. The helper still
handles it: `invalid_picture`, removed by a write, which succeeds. That is
tested with the audio frames compared byte for byte, since no digest is
possible. A test pins the ffmpeg behaviour, so a bump that changes it
fails the gate.

### N-094 · MP3 and M4A: designed now, implemented in Phase 4 — DECIDED
This round implements FLAC completely. For the other formats:
- **Now:**
  - the request and response types, and the `Format` enum;
  - `fields.h` holds the per-format table layout;
  - every MP3 and M4A operation fails with a typed `unsupported_format`
    (`media_tags_unsupported_format`, tested);
  - the TagLib build already has the MPEG, ID3v2, APE and MP4 modules.
- **Phase 4, MP3:**
  - the reader: ID3v2, then APE, then ID3v1, first non-empty value, with
    conflicts (§8.1). It must be independent of TagLib, like the FLAC one,
    with a raw ID3v2 scan: TagLib drops or upgrades frames on the v2.3→v2.4
    conversion, and compressed frames need zlib (N-083);
  - the writer: ID3v2.4, APE cleanup of managed keys, covers and sort keys
    only, ID3v1 removal with the comment moved to COMM `legacy-id3v1`
    (§8.3);
  - its alias table: old ID3 year and date frames (TYER, TDAT, TIME, TRDA,
    TORY), and the sort frames TSOT, TSOP, TSO2 and TSOA;
  - the ID3v1-migration exclusion in `VerifyTags`.
- **Phase 4, M4A:** the reader and the writer of the `©nam`, `©ART`,
  `aART`, `©alb`, `trkn`, `disk`, `©day`, `©gen`, `cpil` and `covr` atoms,
  the sort atoms `sonm`, `soar`, `soaa` and `soal`, and the numeric `gnre`
  removed when the textual `©gen` is written (§8.2).

---

## `internal/store` transactions, `internal/jobs`, `internal/catalog` (2026-09-23, round 4)

### N-095 · The transaction runner and the catalog lock live in `internal/store` — DECIDED
`store.InCatalogTx` and `store.InSnapshotTx` are the only ways the queue and
the catalog open a transaction (§5.3, §6.2, §6.4).
- **The lock.** `InCatalogTx` is READ COMMITTED, and its first statement is
  `pg_advisory_xact_lock(7884786317834415207)`, the ASCII `mlcatalg`. It is
  distinct from the migration lock `mlmigrat` (N-053) and released by the
  commit or the rollback. Every later statement of the transaction takes a
  new snapshot, so it sees every commit made before the lock was granted.
- **`store.CatalogTx`** embeds `*Queries` through an unexported alias
  (`type queries = Queries`), so the query methods are promoted but no other
  package can set or read the field: only `InCatalogTx` builds a usable one
  (checked: `&store.CatalogTx{Queries: ...}` does not compile outside
  `store`). The zero value is the only one another package can make, and it
  has no connection: a query on it panics before any SQL. Every function that
  writes the catalog or the queue takes a `*store.CatalogTx`, so the
  compiler, not a convention, enforces "all mutations under the same lock"
  (§5.3). The external tests reach the `*Queries` inside through
  `store.CatalogQueries`, defined in `export_test.go` only. *Fixed after the
  round 4 review: the first version embedded the exported `*Queries`, which
  any package could fill with a bare pool.*
- **`InSnapshotTx`** is REPEATABLE READ without the lock: the claim and its
  snapshot (§6.2, N-096).
- **Retries.** 40001 and 40P01 run the whole short transaction again, at most
  3 more times ("fino a tre volte" read as three retries: four runs), with a
  jitter in `[0, 5 ms << retry)`. Then `store_retries_exhausted`, which is not
  fatal. `fn` must have no effect outside the transaction and must reset
  what it reports; every caller does.
- **Classification (§6.4):**

  | Where | Failure | Result |
  |---|---|---|
  | begin | context ended | `store_canceled` |
  | begin | anything else | `store_connection_lost` (fatal) |
  | fn | context ended | `store_canceled` |
  | fn | the connection closed, or the rollback failed | `store_connection_lost` (fatal) |
  | fn | anything else | returned unchanged (retried if 40001/40P01) |
  | commit | a `*pgconn.PgError` with severity FATAL or PANIC (the server ended the session) | `store_commit_uncertain` (fatal, N-113) |
  | commit | any other `*pgconn.PgError` (the server answered and rolled back) | returned (retried if 40001) |
  | commit | `pgx.ErrTxCommitRollback` | returned |
  | commit | anything else | `store_commit_uncertain` (fatal, N-109) |

  The COMMIT runs with `context.WithoutCancel`: a cancellation after `fn`
  returned does not interrupt it (tested). A cancellation while waiting for
  the lock is `store_canceled`; pgx then sends a CancelRequest and closes the
  session, so the server does not keep waiting (tested).
- **`store.IsFatal`** is true for the two fatal codes. A fatal error wraps
  `fn`'s error, which may be a domain error, so `catalog.Code` and
  `jobs.Code` return the fatal code first (mutation-checked).
- **Not wired yet:** the §6.4 reaction (stop the mutations and the workers,
  exit, Docker restarts). `jobs.Pool.Run` returns the first fatal error;
  `cmd/musiclibd` must exit on it when the pool is started (N-070, N-107).

### N-096 · The claim does not take the catalog lock; every other queue write does — DECIDED
A REPEATABLE READ snapshot taken before waiting for a lock would already be
stale when the lock is granted, and SKIP LOCKED would be pointless under a
global lock. The claim is still correct without it, because every catalog
change of an album touches its render row in the same transaction
(`EnqueueRender`):
- a change in progress holds the row, so the claim skips it (SKIP LOCKED);
- a change committed after the claim's snapshot makes `FOR UPDATE` fail with
  40001, and the claim runs again (tested with a real concurrent commit);
- a change that reaches the row after the claim waits for the claim's commit,
  then keeps the job running with a newer `requested` (§6.3).

The completions and the boot helpers take the catalog lock, which is
conservative: FINALIZE and PREPARE hold it anyway.

### N-097 · `CheckFresh` lives in `internal/catalog`, not in `internal/jobs` — DECIDED
The four-condition recheck of §6.3 needs the claims union, which is the
catalog's (`ReconcileClaims`), and the catalog already depends on the queue,
not the reverse. It checks, in this order: the job running with the
snapshot's ticket and `requested == claimed` (a missing job counts as a
stale ticket), the album's revision, the renderer, and that the album owns
every key of its union (`StaleClaims`; extra rows are tolerated here, since
`ReconcileClaims` removes them at the next change). The reviewer may prefer
it elsewhere; moving it is mechanical. The behaviour on `StaleClaims` is
N-112.

### N-098 · `EnqueueRender` lives in `internal/jobs` — DECIDED
§2.3 lists "enqueue" under `catalog`, the assignment under `jobs`. It is
still the single implementation (§13.2): `jobs.EnqueueRender(ctx,
*store.CatalogTx, albumID)`, called by the catalog inside its transactions
and by the boot helper of step 6. Flagged for the reviewer.

### N-099 · `tracks.source_path` and `attachments.rel_path` are relative to the candidate's root — DECIDED
Not to `/import`: `jobs.source_rel` locates the candidate. This is also the
"percorso relativo originale" of the fingerprint (§7.6), and it keeps the
paths stable if the same candidate is imported from another place.

**The fingerprint's serializer** (`importer.fingerprintJSON`, round 5) is
Go's `encoding/json` encoder with `SetEscapeHTML(false)`, and no trailing
newline. That is the "serializzatore comune senza escape HTML" of §7.6,
with one caveat: the encoder still escapes U+2028 and U+2029 as ` `
and ` `, even with HTML escaping off, and it replaces invalid UTF-8
with U+FFFD (the paths are valid UTF-8 by §5.2, so that never happens).
Other serializers would write U+2028 and U+2029 raw. This does not affect
determinism: the same input always gives the same bytes. It does matter for
compatibility, because the fingerprint is frozen once albums exist
(`albums.import_fingerprint`, §7.6). Recomputing it with another serializer
(another language, a later Go that changed this behaviour) must reproduce
these two escapes, or a candidate with such a character in a path would no
longer be recognized as a duplicate. `TestFingerprintGolden` pins the
bytes for HTML characters and non-ASCII text, not for U+2028; a Go bump
that changed the escaping would show up only on such a path.

### N-100 · An empty album genre is stored as NULL — DECIDED
§4.1 distinguishes NULL and `""` for a *track* genre (inherit or explicitly
none). An album inherits from nothing, so both mean "no genre" and are
stored as NULL; saving `""` over NULL is a no-op (tested).

### N-101 · Artists are looked up by `folder_key` — DECIDED, one ACCEPTED
- `artists.folder_key` is unique, so the folder is the identity. An existing
  artist is reused only if `names.Key(name)` equals `names.Key` of its name
  (§7.6 "solo se"), keeping the existing spelling. A different name with the
  same folder is `artist_folder_conflict` with both names; nothing is merged.
- **ACCEPTED:** two names equal after casefold whose folders differ only
  through the §5.2 truncation hash (the hash covers the case-sensitive
  segment; names of about 180 bytes and more) become two artists with two
  folders. Nothing is merged and no path collides.

### N-102 · Blob rows at import — DECIDED
- An existing row with a NULL format takes the format the importer read
  from the content; a known format is never changed.
- Two different known formats are `invalid_blob_format`; the same hash with
  another size is `blob_mismatch` (corruption or a bug; §7.5 already
  verifies the bytes).
- The roles (track: audio; cover: JPEG/PNG; LRC: no format) are checked
  against the merged format.
- The candidate lists every blob exactly once, and every listed blob is used.

### N-103 · A skipped duplicate import — DECIDED
`state = skipped`, `result_album_id` = the existing album, `error_code =
duplicate_import`, and a message that says "in the trash: restore it
instead" when the album is trashed (§7.6: "se nel cestino, proporre il
ripristino"). `skipped` needs a code and a message like `failed`: §4.2 has
no other column for the reason.

### N-104 · Every effective change bumps and enqueues, trashed albums included — DECIDED
Renaming a trashed album (§4.3 allows it) or renaming its artist bumps its
revision and enqueues its render: the ETag must move (§10.1), and the render
of a trashed album is its removal, which is idempotent. A save with no
effective change (texts compared after normalization) does neither (§4.3).

### N-105 · Closed warning codes and overrides — DECIDED
- `jobs.Warning{Code, Message, Path}` with four codes for now
  (`unassigned_file`, `tracks_renumbered`, `year_discordant`,
  `tag_conflict`); the importer adds its own as constants, never as free
  strings. Encode and decode are strict (unknown fields, codes, trailing
  data).
- `jobs.Overrides{Artist, Title}`: `Encode`, the only writer, requires
  normalized texts. `DecodeOverrides` checks the closed shape only. The
  first version also rejected non-normalized values; since the claim decodes
  the overrides inside its transaction, one bad row would have aborted every
  claim and starved every import queued after it. The import commit
  normalizes the album's artist and title anyway (tested,
  mutation-checked).
- **Remaining risk:** a row whose `overrides` has a malformed *shape* still
  aborts every import claim, because it stays at the head of the queue. Only
  a bug or a manual edit of the database can write one (`Encode` is the only
  writer).

### N-106 · What the import commit validates, and what the importer round must do — DECIDED
- The commit validates the whole closed input (§7.6): the fingerprint, the
  texts (§5.2), years, discs and numbers, duplicate numbers, the blob list,
  the roles, the cover (JPEG/PNG, at most 20 MiB, §8.5), the LRC association
  (§7.4: same directory, `.lrc`, same stem by NFC and casefold, exactly one
  track), unique source paths, attachment collisions after normalization
  (file/directory included, §5.2), and the limits of §7.2 (1,000 tracks,
  10,000 files).
- **N-091:** the cover must fit every audio format of the album, asked
  through `catalog.CoverFits`, once per format, in a fixed order. The
  catalog knows no per-format limit.
- It never inspects media. **Requirements for the importer round:** decode
  and verify everything before calling it (§7.6 `AudioDigest`), refuse the
  N-092 files, set `Blob.Format` from the content, check the 40 Mpixel limit
  and the full decode of the cover (§8.5, N-087), and supply `CoverFits`.
- A domain rejection makes the job `failed` with its code in the same
  transaction, and writes nothing else; any other error aborts the
  transaction.

### N-107 · Nothing is wired into `cmd/musiclibd` yet — DECIDED
- §11.1 step 5 (`jobs.RecoverRunning`) must follow the journal recovery of
  step 4, which does not exist yet: FINALIZE of a recovered publication
  completes its job by ticket, which needs the job still running.
- Step 6 (`jobs.EnqueueStaleRenders`) needs `render_version` (N-010).
- The pool (`jobs.NewPool`) has no executors yet. **Requirement for the
  executor round:** every non-fatal path of an executor must end in a
  completion (`Finish`, `Fail` or `Requeue`). Otherwise the job stays
  `running` until the next boot (§11.1 step 5), and since an enqueue keeps a
  running job running (§6.3), later edits to that album would not render.
- FINALIZE of a recovered publication will find its job through the
  album's single render row: the journal holds `album_id` and `ticket`, not
  the job id.
- `POST /api/albums/{id}/render` (a forced enqueue that checks the revision
  seen without bumping it, §10.2) is a small catalog function for the HTTP
  round.

### N-108 · `pgtest.Proxy` loses the database on the wire, against the real server — DECIDED
There is no mock of the commit. The proxy understands the v3 protocol without
TLS and can let the server commit and drop the answer
(`LoseNextCommitAck`), drop the COMMIT itself (`CutBeforeNextCommit`), or cut
every connection (`CutAll`). It is used by the runner's tests and by the
import commit's lost-acknowledgement test (§7.6, §12.2).

### N-109 · pgx calls a failure after a sent COMMIT "safe to retry" — RESOLVED
*Found by `TestLostConnection` on 2026-09-23.* In pgx v5.11.0, when the answer
to a COMMIT is lost, `MultiResultReader.NextResult` ignores the read error of
`peekMessage` (which has already closed the connection), and the next
`receiveMessage` returns `connLockError{"conn closed"}`, whose
`SafeToRetry()` is always true ("a lock failure by definition happens before
the connection is used"). So a COMMIT that the server did commit was
classified as "nothing sent, nothing committed". Both codes are fatal, so the
process would have restarted anyway, but the code claimed a certainty it did
not have. The runner no longer consults `SafeToRetry` at commit: any failure
without an answer from the server is `store_commit_uncertain`.
`TestPgxSafeToRetryIsWrongAfterASentCommit` pins the pgx behaviour and logs
when a pgx upgrade changes it; mutation-checked.

### N-113 · A COMMIT answered with FATAL or PANIC is uncertain, not a rollback — RESOLVED
*Found by the round 4 review.* The runner treated every `*pgconn.PgError` at
COMMIT as "the server answered and rolled back". A PgError with severity
FATAL or PANIC means that the server ended the session: 57P01 from
`pg_terminate_backend` or a fast shutdown, 57P02, 57P03. pgconn closes the
connection when it reads one. For example, a COMMIT blocked on the deferred
unique check of tracks `(album_id, disc, no)` whose backend is terminated
became an ordinary error of `CommitImport` or `UpdateAlbum`; the pool
reconnected and the process never restarted, against §6.4 (the loss of the
connection is never a normal error, no partial reconnection). With
synchronous replication, a termination while COMMIT waits for the standby
follows the local commit, so the outcome is not even certain.

Such an error is now `store_commit_uncertain`, fatal and never retried. It
is decided by the severity (`SeverityUnlocalized`, else `Severity`), never by
`tx.Conn().IsClosed()`: the connection is already back in the pool when
`Commit` returns (N-111). `TestSessionEndedAtCommitIsUncertain` blocks a real
COMMIT on a deferred unique check that waits for another session's
uncommitted row, terminates its backend, and wants the fatal code wrapping
57P01 and one run. Mutation-checked: with the severity test disabled, the
test fails with the raw 57P01 (code "", not fatal).

### N-110 · REPEATABLE READ claims conflict with each other; the pool claims one at a time — DECIDED
`FOR UPDATE SKIP LOCKED` skips only rows locked *now*. In REPEATABLE READ, a
job that another claimer claimed and committed after this snapshot is a
serialization failure (40001), not a skipped row. Concurrent claimers all
race for the head of the queue: with 8 raw claimers, some exhausted their
three retries; with 16 workers, 35 serialization failures in one run of
`TestPoolExecutesEachJobOnce`.

§6.2 requires the claim and the snapshot in one REPEATABLE READ transaction,
so that stays. Inside the process (one instance per database, §2.2) the pool
serializes its workers' claims with a mutex: a claim takes milliseconds, the
work stays parallel, and the §6.4 retries are left for the rare conflict with
a catalog change. The database guarantee of one claim per job does not rely
on the mutex (tested with raw concurrent claimers). A test counts the 40001s
of the pool's statements with a pgx tracer and wants none (mutation-checked).

### N-111 · The runner read a connection it had already released — RESOLVED
*Found by the race detector on 2026-09-23.* `abort` called
`tx.Conn().IsClosed()` after `tx.Rollback`, but a pgxpool transaction
releases its connection inside `Rollback`, and another goroutine may have
acquired it at once. It now reads the state before the rollback
(mutation-checked with `-race`).

### N-112 · When PREPARE finds `StaleClaims`: fail the render, never requeue — TO CONFIRM (proposal for the publish round)
§6.3 says that superseded work goes back to pending. For a stale ticket,
revision or renderer that is right: a newer request or renderer exists, and
the next build answers it. `StaleClaims` is different: nothing in the queue
changes the claims, so a requeued job would be claimed, rebuilt in full and
refused again, forever. Every catalog change re-derives the claims in its
own transaction, so `StaleClaims` means that something outside the rules
happened (a manual edit, a bug, an interrupted rebuild).

Proposal, all in PREPARE's transaction:
1. Try `catalog.ReconcileClaims` for the album. If it succeeds, the missing
   claims were free and are now held: PREPARE continues (the build does not
   depend on the claims).
2. If it fails with `path_reserved`, call `jobs.FailRender` with that code
   and a message naming the owning album, and discard the build. The job is
   `failed` and visible (§6.4); a later change of either album, or a retry,
   enqueues it again. No journal is written.
3. Doctor (§11.3) reports such a conflict as structural damage of the
   reservations.

The more conservative alternative is (2) alone, without the repair of (1).

---

## `internal/importer`: scan and import of one candidate (2026-09-23, round 5)

### N-114 · No process-wide space budget yet: a statfs check per import — DECIDED (budget: executor/pool round)
§11.2 asks for three things: a conservative estimate, a `statfs` check with a
1 GiB margin, and a process budget that reserves the estimates of the jobs in
progress. This round does the first two only.

Before any copy, `Importer.checkSpace` requires `free − 1 GiB ≥ estimate`:
- the free space is `f_bavail` of `/data/work`, which is on the filesystem of
  `originals/` (§3.1);
- the estimate is the sum of the candidate's file sizes (every file becomes
  a blob, and deduplication only lowers it), plus 2 × 20 MiB for one embedded
  picture extracted into `work/import` and pinned as the cover.

A refusal is `insufficient_space`: failed, with no automatic retry (§6.4).

**Risk until the budget exists:** two workers can both see the same free
space.

**The seam:** `checkSpace` is the one place to reserve and release the
estimate. The executor/pool round adds the in-memory reservation there, and
the render's own.

A put still handles ENOSPC anyway (§11.2), and nothing published is removed
to make room.

### N-115 · What the scan probes, and why — DECIDED
§7.2 recognizes audio by its content. But the scan only needs to know what is
audio to *group* the candidates, and the import probes every file again on
the verified copies.

At the scan (`scanAudio`), a file counts as audio as follows:
- **Known audio extension** (`media.HasKnownAudioExtension`): audio, whatever
  its content. A corrupt one is found by the import, as an album error.
- **Not probed:** an empty file, or a file whose extension (ASCII
  case-insensitive) is one of `jpg jpeg png gif bmp tif tiff webp pdf cue log
  lrc`. These are the kinds that §7.2 says never need an audio probe (images,
  PDF, CUE, LOG), plus the LRC files of §7.4.
- **Probed:** every other file, with ffprobe on the source. The source is
  read only, through the confined root, with the Runner's 30 s limit. Audio,
  supported or not, counts as audio.

**Cost:** one probe per unusual file (NFO, TXT, M3U, a file without an
extension). The Runner's semaphore bounds it, and nearly every file of a rip
is decided by its name.

**What this can miss:** an audio file with one of the listed extensions, such
as `.jpg`. Nothing is imported wrongly because of it:
- outside every candidate, it is reported as an unassigned file, not dropped
  silently;
- in a candidate's subdirectory, the import finds it by content and fails the
  album as ambiguous.

### N-116 · The year of a DATE tag — DECIDED
§7.3 says "valore valido più frequente" but does not define a valid value.

A track has a year when its DATE field has exactly one value, and that value,
trimmed:
- starts with four ASCII digits not followed by a fifth digit (`1959`,
  `1959-08-17`, `2001/05`, `1987T...`);
- gives a year in 1..9999 (`albums.year`, §4.2).

Anything else is no year: `0000`, `19999`, `abcd`, or two values. It is
ignored, not an error.

The warning `year_discordant` is given when the valid values differ. Pinned
by `TestInferMetadata`.

### N-117 · An imported LRC file that is not UTF-8 stays an attachment — DECIDED
§10.2 requires UTF-8 text for LRC uploads. §7.4 only says when an imported
LRC is associated.

An LRC file may match exactly one track while its content is not valid UTF-8
(a Latin-1 export, for example). It is then not associated:
- it stays an attachment under `Extras/`, with the warning
  `lyrics_not_utf8`;
- nothing is lost, and nothing is converted.

The 2 MiB limit of uploads is not applied at import: §10.2 is about the
browser. The check streams the blob, in constant memory.

Ambiguity is an explicit error (§7.4), `lyrics_association`: several tracks
with the stem, or several LRC files for one track.

### N-118 · Content-derived blob formats at import — DECIDED
`blobs.format` comes from the content only (§4.2):
- `flac` for the tracks, from the probe;
- `jpeg` or `png` for the chosen cover, from the Go decode of §8.5;
- NULL for everything else, other images included.

Validating every image of a rip only to label its blob is not required. It
would mean a full decode of scans of up to 20 MiB and 40 Mpixel, most of
which never become covers.

NULL therefore means "not known to be one of the formats of the list".
N-102 already lets a later content check fill it, and the cover endpoints of
§10.2 validate an image when it is chosen.

### N-119 · The natural order of §7.3 — DECIDED
The rule:
- digit runs (ASCII 0–9) compare as integers of any length, leading zeros
  ignored;
- every other run compares by its UTF-8 bytes;
- a digit run against a non-digit run compares by bytes;
- names equal by value (`01` and `1`) are then ordered by the bytes of the
  full path, so the order is total.

§7.3 is silent on case, so the bytes decide:
- `Track 9` comes before `track 1`;
- `Track10` comes before `Track 2`, because `Track` is a prefix of `Track `.

It serves two purposes only: numbering a disc whose tags are unusable, and
ordering the tracks given to the metadata rules. Every other list
(directories, files, the fingerprint, the warnings) is ordered by the bytes
of the path.

Pinned by `TestNaturalOrder`; mutation-checked.

### N-120 · Multi-disc layouts in Phase 2 — DECIDED (Phase 5 groups them)
Rules 2 and 3 of §7.2 are Phase 5. Until then, `group` recognizes the shape
of rule 2:
- a directory without direct audio;
- whose audio is only in direct children named `CD<N>` or `Disc <N>`, with
  N > 0, leading zeros allowed, ASCII case-insensitive;
- with no audio below a disc directory.

That branch fails as a whole with `multidisc_not_supported_yet`, including
rule 3's duplicate disc numbers. It is never grouped any other way, and never
imported disc by disc.

Anything without that shape follows rule 5, as §7.2 says. For example,
`Box/CD1` next to `Box/Bonus` (both with audio) are two independent
candidates, `CD1` and `Bonus`, in Phase 2 as in Phase 5.

`CD0`, `CD 1` and `Disc1` are not disc names.

A track's disc number comes from its tag, or is 1: §7.3's rule for albums
without disc directories.

### N-121 · How names are compared in §7.3 — DECIDED (track artists: owner decision, 2026-09-23)
- **Track artists: §7.6's identity (owner decision, 2026-09-23).** Round 5
  compared them exactly, so an album with no `album artist` tag and tracks
  by `Abba` and `ABBA` became Various Artists, compilation true. The owner
  decided that, for "unico artista non vuoto delle tracce" of §7.3, artists
  equal after NFC, trim and casefold (the comparison §7.6 uses for artist
  identity) count as one artist. Implemented in round 6:
  - **One implementation:** `catalog.SameArtistName` (formerly the
    unexported `sameName` of `resolveArtist`), used by the importer's
    `trackArtist`. It is `names.Key` on the normalized texts: Unicode full
    case folding, so `Strauß` and `STRAUSS` are one artist too.
  - **The spelling, deterministic:** the one the most tracks use; a tie goes
    to the smallest in the bytes of the normalized text, as the genre
    (N-004). `abba`, `ABBA`, `Abba` once each give `ABBA`. Tracks without an
    artist do not count.
  - **Genuinely different artists** still give Various Artists and
    compilation true. For example `Abba`, `ABBA` and `Queen`: every track
    keeps its own spelling as its override, since none equals `Various
    Artists`.
  - **The track-artist override** of §7.3 ("NULL se ... uguale all'artista
    album normalizzato") uses the same comparison. That applies whatever
    chose the album artist: the `album artist` tag, the tracks, or the
    explicit override.
- **What that loses, decided explicitly:** a track whose artist differs from
  the album artist only in case gets NULL and inherits. Its own spelling is
  not kept in the catalog, and the render writes the album artist's
  spelling into its ARTIST tag. This follows from the owner's rule: the
  override exists for a *different* artist, and §7.6 says the two spellings
  are the same artist. The alternative (keeping the differing spelling as an
  override) would leave `ABBA` next to `Abba` in the output, the inconsistency
  the rule removes. Nothing is lost from the library itself: the original
  file keeps its tag in the blob store, and the editor can set a per-track
  artist. Pinned by `TestInferMetadata` ("track artist differing in case
  inherits", "... from the artist override inherits").
- **Album tags and album artist tags** are still compared exactly after the
  §5.2 normalization (NFC, trim). Two album artist tags that differ only in
  case are "discordanti": the import is refused
  (`ambiguous_album_artist`), and an explicit artist resolves it. The
  reviewer judged this correct and the owner did not change it (pinned: "album
  artists differing in case stay ambiguous").
- **Mutation-checked:** the identity by bytes, the tie to the largest, and
  the override compared by bytes each make `TestInferMetadata` fail.
- **Genre differences are kept** (§7.3: "differenze conservate come override
  traccia"). A track whose genre differs from the album genre keeps its own:
  - a track with no genre then gets `""`, explicitly none (§4.1);
  - with no album genre, every track inherits (NULL).
- **Invalid tag text.** A tag value that is not a valid text (a control
  character, more than 1,024 characters) is `invalid_tag`, naming the file
  and the field. It is never truncated or cleaned silently.
- **Missing title.** A track without a usable title gets its file name
  without the extension, or the whole name when that leaves nothing
  (`.flac`).

### N-122 · The scan's outcome and report — DECIDED
- **Paths:** the paths of the import jobs and of the scan's warnings are
  relative to `/import` (the batch root joined), exactly as on disk.
- **Unassigned files:** each file outside every branch is an
  `unassigned_file` warning, one per file.
- **Symlinks, special files and invalid names:** they are never followed nor
  opened.
  - Outside every branch, each one is a `rejected_entry` warning.
  - Inside a candidate, they fail it with `source_rejected_entry` (§5.2).
    Only regular files are ever ignored: a symlink named `.DS_Store`,
    `Thumbs.db` or `desktop.ini` inside a candidate fails the album with
    `source_rejected_entry`, like any other symlink (importer review).
  - An invalid name (not UTF-8, or deeper than 16 levels under `/import`)
    cannot be a `Warning.Path`, so it appears only quoted in the message.
- **No valid candidate:** the scan job is **failed** with
  `no_valid_candidate` (§7.2: "batch completato con spiegazione, non successo
  vuoto"). The message says whether there was no audio at all or only failed
  branches. `failed` is what makes the batch visible and retryable.
- **Root failures** fail the scan:
  - a missing root: `source_not_found`;
  - a root that is a file: `source_not_directory`;
  - a root that goes through a symlink: `source_rejected_entry`.
- **A scan retried later** keeps every import job already there, even a
  failed one: `(batch_id, source_rel)` is unique. A branch fixed after its
  failure is retried through its own import job, which revalidates the disk.

### N-123 · /import itself as a candidate — DECIDED
A batch rooted at `/import`, with audio directly in it, gives a candidate
with `source_rel = ""`. §5.2 allows it.

There is no directory name to fall back on for the title: the mount's name is
not the user's. Without an album tag, the import therefore fails with
`album_title_missing`, and an explicit title resolves it (tested).

### N-124 · Details of the cover selection (§7.4) — DECIDED
- **External candidates:** files **at the candidate's root**, whose name
  without its last extension has the key (`names.Key`) `cover`, `folder` or
  `front`. A name without an extension is not one. In each group they are
  ordered by the key of the name, then by its bytes.
- **Embedded front covers** (type 3): by descending frequency, a tie going to
  the smallest SHA-256.
- **Then "la prima immagine incorporata valida":** every picture of every
  type, tracks in their final order (disc, number), pictures in file order.
  This fallback also applies when front covers exist but none is valid.
- **Tried once:** each image is tried once, by hash.
- **Warnings:** every refused candidate gives one:
  - `cover_skipped`: not a JPEG or PNG by content, over 20 MiB, over
    40 Mpixel, or not decoding completely;
  - `cover_not_embeddable`: N-091.
- **Validation** uses Go's `image/jpeg` and `image/png` (N-073), in memory. A
  40 Mpixel 16-bit PNG decodes to about 320 MB. This is accepted: the limits
  of §8.5 bound it, and a worker handles one album at a time.
- **Embedded pictures** are extracted into `work/import/<random>.img`, then:
  - checked against the hash the inspection reported;
  - validated;
  - pinned through the blob store only if chosen.

  The temporary is removed on every path.

### N-125 · Where the new pieces live — DECIDED
- **Catalog transactions:** `catalog.CreateImportBatch`, `GetImportBatch`,
  `CommitScan` and `FailJob` hold the batches and the job outcomes, under the
  catalog lock (§13.2: no SQL in the workers).
- **Queue inserts:** `jobs.EnqueueScan` and `jobs.EnqueueImport`, next to
  `EnqueueRender` (N-098).
- **`catalog.StemKey`** is exported, so that the importer associates the LRC
  files with the same function the commit checks them with (one
  implementation, §13.2).
- **`fsops.Describe(f)`** is an fstat of an open descriptor. It compares what
  was opened with what the walk described (§7.1).
- **`importer.CleanWork`** runs at boot step 5 (`cmd/musiclibd.cleanWork`),
  before any worker.
- **The executors are not wired.** `ExecuteScan` and `ExecuteImport` wait for
  the publish round, which starts the pool (N-107). There, one executor will
  dispatch render, scan and import.
- **New warning codes** in `jobs`: `rejected_entry`, `cover_skipped`,
  `cover_not_embeddable`, `lyrics_not_utf8`, `flac_id3_tag`.

### N-126 · What the stability check sees, and what it cannot — ACCEPTED
The checks, in order:
1. When the import walks the candidate, it records the identity (st_dev,
   st_ino), size and mtime (ns) of every directory (the candidate's root
   included) and every non-ignored file.
2. Every file is opened and `fstat`ed, and must match; its copied size must
   match too.
3. After everything is read, just before the commit, the whole candidate is
   walked again and compared: an addition, a removal or any difference
   refuses the import.

What they cannot see:
- a change after that last walk and before the commit: the window is one
  catalog transaction;
- a rewrite that keeps the size and the mtime to the nanosecond. §7.1
  promises identity, size and mtime, nothing more; the fingerprint covers the
  bytes actually copied;
- an in-place rewrite of an ignored file (`.DS_Store`, `Thumbs.db`,
  `desktop.ini`), which is not compared.

**Correction (importer review):** adding or removing an ignored file during
the import *does* refuse it as `source_changed`. The file is not compared,
but its directory's mtime is, and creating or deleting an entry changes it.
Only an in-place rewrite of an existing ignored file goes unseen. This is
conservative: a spurious refusal is retried, while an unseen change is not.
The cost is that Finder or Explorer browsing the share during an import can
create a `.DS_Store` or `Thumbs.db` and make the import fail as
`source_changed`; retrying the job imports it.

The source is never written:
- `/import` is reachable only through the `source` type: stat, list, open for
  reading;
- every end-to-end test compares every source entry's bytes, inode, mode and
  mtime before and after each job.

The open-time identity check is defence in depth. Its mutation alone
survives, because the final walk also sees a replacement; with both checks
removed, the test fails.

### N-127 · The device case of the scan is not tested without CAP_MKNOD — ACCEPTED
`TestScanRejectsSpecialFiles` covers a symlink, a FIFO and a socket inside a
candidate. They are rejected without being opened: a FIFO opened in blocking
mode would hang the test.

A device node needs CAP_MKNOD, which the dev and test containers do not have.
The subtest logs it, and the fsops suite has the same gap (N-038).

The walk opens nothing but regular files, by lstat, so a device is handled
like the FIFO.

---

## Round 6: carry-over before the renderer (2026-09-23)

N-090 and N-121 were implemented, and N-099, N-122 and N-126 corrected
after the importer review; see those entries.

### N-128 · The full decode of a FLAC with a trailing ID3v1 reads only the bytes before the tag — RESOLVED (owner decision 2026-09-23)
**The finding (round 6).** The pinned ffmpeg (8.1.3, with `-err_detect
crccheck+explode -xerror`, N-074) refuses to decode a FLAC that has 128
bytes of ID3v1 after its last frame, and one with an ID3v2 tag appended at
the end ("3DI" footer): "invalid sync code", exit 183, `media_decode`. So
`AudioDigest` refused the file, and the importer failed the album with
`corrupt_audio` before the tags were read, with the message "not a valid
audio file", although the file plays and a render strips the ID3v1 anyway
(N-090). A **leading** ID3v2 always decoded to the same samples.

**The owner's rule (2026-09-23).** For a FLAC with a detected trailing
ID3v1 tag, `AudioDigest` decodes only the bytes before the tag, with the
same pinned ffmpeg and exactly the same command line (§8.4, N-074): no
relaxation of `crccheck+explode`, no other transform. The 128 bytes left
out are the bytes a render strips, so the input's digest and the output's
describe the same stream.

**One detection rule (§13.2).** The rule is TagLib 2.3.2's
`Utils::findID3v1`, which is also the helper's reader (`readRawFlac`) and
the rule by which TagLib strips the tag at render: "TAG" 128 bytes before
the end, unless those are the "TAG" of an "APETAGEX" that starts 131 bytes
before the end; for a file of 128 to 130 bytes, "TAG" at the start (no
FLAC can reach that branch: "fLaC" or "ID3" holds the first bytes).
- **Where it lives:** `media.hasTrailingID3v1`, used by `flacAudioEnd`,
  which `AudioDigest` calls for a file the probe classified as FLAC. It
  reads the last 131 bytes with pread.
- **Why in Go and not reported by the helper's `inspect`.** `AudioDigest`
  keeps its signature `(ctx, f)` and its callers (the importer, the render's
  §9.1 step 6) need no extra knowledge. Reporting the extent from `inspect`
  would have meant either a new argument that every caller must fetch and
  pass (the render could get it wrong), or `AudioDigest` running the helper
  itself: a second tool run per digest, and a digest that fails with a tag
  code when the metadata is damaged. It would also have bumped the helper
  version, and so `render_version`, for 11 bytes of comparison. The rule is
  three comparisons on fixed bytes. Written once in Go, it is pinned to the
  helper by a test instead.
- **The cross-check** (`TestTrailingID3v1IsTheHelpersRule`), with the
  release and the ASan/UBSan helper, file by file: the Go rule and the
  helper's inspection (`id3v1` reported, or `corrupt` "an ID3v1 tag
  overlaps the metadata blocks") agree, and the Go extent is exactly the
  last 128 bytes. The helper itself checks its reading against TagLib's
  `hasID3v1Tag()` (`checkTagLibAgrees`), so all three agree. Cases: no tag;
  an ID3v1; a leading ID3v2 and an ID3v1; two ID3v1; `APETAGEX`,
  `APETAGEY` and `XPETAGEX` 131 bytes from the end; "TAG" 129 and 127 bytes
  from the end; lower-case "tag"; an appended ID3v2, alone and followed by
  an ID3v1; audio bytes that spell "TAG"; an ID3v1 inside the metadata.
  `TestHasTrailingID3v1` covers the rule on its own, the short-file branch
  included. That the render strips exactly these 128 bytes is
  `TestTagsFLACStripsID3`: the output is byte for byte the output of the
  file without the tag.

**The byte limit.** The decoder never gets a path (N-075). For a limited
file, descriptor 3 is the read end of a pipe, and a goroutine writes
exactly the first `size − 128` bytes of the file into it with pread
(`feedPrefix`), then closes it. Any other file is passed as today, as
descriptor 3 itself. After the run the parent closes the read end, so a
feeder still writing gets EPIPE and returns: nothing blocks or leaks, on any
outcome. Its failures:
- a failed read of the input is `media_io` (the machine's fault), whatever
  the decoder made of the short stream;
- a decoder that exits 0 without reading the whole stream is
  `media_decode`;
- otherwise the decoder's own failure, as before.

The pipe costs nothing measurable: `BenchmarkAudioDigest5MinFLAC`, 3 × 10
runs of 5 minutes of FLAC, 0.56–0.57 s whole, 0.57–0.60 s with an ID3v1.

**Test evidence** (`internal/media/digest_id3v1_test.go`, real ffmpeg):
- **Same samples:** the digest of a FLAC with a trailing ID3v1 equals the
  digest of the same FLAC without it, with and without a declared length
  (STREAMINFO total 0), and with a leading ID3v2 too. In
  `TestTagsFLACStripsID3`, the input's digest now equals that of the file
  without ID3 for every case, trailing ID3v1 included, and so does the
  output's: the §9.1 step 6 comparison holds.
- **Exactly the bytes:** a fake ffmpeg that copies its descriptor 3 to a
  file receives exactly the file without its last 128 bytes (a single
  ID3v1, two ID3v1, a leading ID3v2 too), and the whole file otherwise
  (`APETAGEX`, no tag). The samples alone could not show an extent that
  is a few bytes too long: see N-129.
- **A real truncation is still refused:** audio cut short by 1, 2, 3, 10,
  100 and 1000 bytes and followed by an ID3v1 is `media_decode`, also
  without a declared length, so the refusal is the decoder's: "CRC error"
  (the frame CRC-16 of N-074) for the shortest cuts, "invalid residual"
  for the others.
- **A false match is refused:** a valid FLAC of uniform noise, whose last
  frame the encoder stores VERBATIM, with two samples set so that its audio
  bytes spell "TAG" 128 bytes before the end. Read whole it decodes; under
  the rule it is `media_decode`, with and without a declared length. Why
  this holds in general: a FLAC frame starts with its sync code (0xFF), so
  a "TAG" can never be where a frame starts, and the cut always falls
  inside a frame. The file is then accepted only if that incomplete frame
  parsed to its end and its CRC-16 matched by chance, **and** STREAMINFO
  declares no total (otherwise the frame count of N-078 refuses it). Before
  this rule such a file was imported and its render failed (N-090).
- **What a render keeps stays refused:** an `APETAGEX` 131 bytes from the
  end, two ID3v1 tags, an ID3v2 appended at the end (alone or followed by
  an ID3v1).
- **The feeder:** exactly n bytes for n at and around the read chunk; a
  short input and an unreadable one are read failures; a decoder that
  exits 0 or 1 without reading, and a cancellation during the decode, end
  with `media_decode`, `media_decode` and `media_canceled`, with no leak
  under `-race -count=10`.

**The appended ID3v2** stays refused. The helper does not strip it:
TagLib's FLAC code and the helper's reader only look for an ID3v2 before
`fLaC` (N-090's ACCEPTED limits), and extending the helper is outside
N-090. Its bytes would stay in the library copy, so they must not be left
out of the digest either.

**The importer** gives the `flac_id3_tag` warning for a FLAC with a trailing
ID3v1 and imports it (`TestImportUnrenderableTagsAndID3`). The message of a
file still refused now says which check failed (`corruptMessages`):
- `media_decode`: "does not decode completely: an audio frame is damaged or
  missing, or the file has bytes after its last frame that are not audio
  (an ID3v2 tag appended at the end is one)";
- `media_not_supported`: "is not supported audio";
- `media_tags_corrupt`: "has a damaged metadata structure";
- `media_tags_format_mismatch`: "is not a FLAC stream the tag reader
  accepts".

The code stays `corrupt_audio`, and the media error follows the message.

**Mutation-checked:**
- no byte limit (the whole file decoded): the same-samples tests, and
  `TestTagsFLACStripsID3`, fail;
- the extent one byte short: the same-samples tests fail;
- the extent one byte long: the exact-bytes test fails; the samples do not
  change (N-129);
- a divergent detector (no `APETAGEX` exclusion; "TAG" looked for at 129
  bytes from the end): the cross-check fails;
- the feeder writing one byte less: the same-samples and feeder tests fail;
- a decoder that exits 0 without reading accepted: the failure test fails.

---

## Round 7: the trailing ID3v1 in the full decode (2026-09-23)

N-128 was resolved (owner decision), and N-075 and N-090 corrected; see
those entries.

### N-129 · The pinned ffmpeg ignores up to 9 bytes after the last FLAC frame — ACCEPTED
Found while mutation-checking N-128. Measured on a 3-second FLAC, with the
command line of §8.4 and N-074: after the last frame, 1 to 9 bytes of junk
(zeros, "TAG…", random) decode with exit 0 to the same samples; 10 bytes or
more fail with "invalid sync code". The parser does not treat a remainder
too short for a frame header as a frame. This was true before N-128 too.
- **Consequence:** a FLAC with up to 9 junk bytes at its end is imported,
  and the digest does not cover those bytes. They are not audio: the
  samples are complete and verified by the frame CRCs and the declared
  length. A render keeps them, since they are not a tag, and its digest
  comparison is unaffected.
- **Why accepted:** refusing them would need a check outside ffmpeg of where
  the last frame ends, which is a FLAC parser §8.4 does not ask for. The
  bytes carry no audio and nothing is lost.
- For N-128 it means that the samples alone cannot prove the extent to the
  byte. `TestAudioDigestFeedsExactlyTheAudio` checks the bytes the decoder
  receives instead.
