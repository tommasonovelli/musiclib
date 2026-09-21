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

### N-002 · "Trim outer spaces/dots": both ends or only the trailing one? — TO CONFIRM
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

### N-011 · No CI check on ext4 — OPEN
§12.1 requires tests of the real primitives on a test ext4 volume. Right now
the tests run on the developer's filesystem, with no guarantee that it is ext4.
To be addressed with `internal/fsops` and with the CI scripts.

### N-012 · The whole project must be containerized — OPEN
*New owner requirement, 2026-09-21.*

Build, tests and tooling (fuzzing, `go vet`, `gofmt`, `sqlc`, `goose`, the C++
TagLib helper, ffmpeg) must run in Docker, not only the deployment described in
§11.1. To be planned: a pinned build/test image (Go, TagLib, ffmpeg versions as
in N-009), how tests get a real ext4 volume inside the container (N-011), and
which privileges the `internal/fsops` tests need (for example `CAP_MKNOD` for
the device subtest of `TestSpecialFilesRejected`, and a seccomp profile that
allows `openat2`).

---

## `internal/fsops` (work in progress, not yet committed)

*Found on 2026-09-21 while translating the package to English. Not fixed: the
package is being completed separately.*

### N-013 · Opening a FIFO blocks forever; `Root.Close` then deadlocks — OPEN
The doc comment of `Root.openat2` says that O_NONBLOCK keeps the opening of a
FIFO from blocking before the type check, and `OpenFile` clears O_NONBLOCK after
the check. But no code ever **sets** O_NONBLOCK: `openat2` only adds
O_CLOEXEC. As a result `Root.Open` on a FIFO with no writer blocks in the
syscall forever, against §5.2/§9.3 (special files must be rejected, with
`fs_special_file`).

The blocked goroutine holds `Root.mu.RLock`, so any later `Root.Close` blocks
on `mu.Lock` forever as well. `TestSpecialFilesRejected` reproduces it: its
`fifo` subtest fails after 5 s ("Open of a special file blocked"), then the
test's cleanup hangs in `Root.Close` until the `go test` timeout. With that test
skipped, every other `internal/fsops` test passes.

### N-014 · Directory descriptor closed twice in `Remove` and `RemoveAll` — OPEN
`Root.Remove` and `Root.RemoveAll` register `defer unix.Close(dirfd)` and, on the
success path, also `return closeFD(..., dirfd)`. The descriptor is therefore
closed twice. Between the two closes another goroutine may have received the
same fd number, and the deferred close would then close an unrelated file,
which is the hazard the `Root` RWMutex is meant to prevent. `MkdirAll` has the
same pattern on an error path: if `closeFD` of the current directory fails, the
deferred close closes it again. Tests do not catch it because the second close
normally just returns `EBADF`, which is ignored.

### N-015 · Minor `internal/fsops` observations — OPEN
- `ProbeRenameExchange`: the cleanup `defer` for a probe directory is
  registered only after `makeProbeDir` succeeds. If `Mkdir` succeeds but the
  marker creation or write fails, the `.musiclib-probe-*` directory is left
  behind (doctor would report it, §11.3, but the probe could clean it up).
- `rename`: every `renameat2` error is reported with the **destination** root
  and path, including errors about the source (for example `ENOENT` on a
  missing source). The code is correct, the location in the message may be
  misleading.

---

## Translation to English

### N-016 · Italian fixture names left in test inputs — TO CONFIRM
*2026-09-21.* The code, comments, messages and test names were translated to
English. The test **inputs** were left unchanged on purpose, because inputs and
expected values are frozen during a translation. Some of them are Italian words
that have no meaning for the test: file and directory names and file contents
in the `internal/fsops` tests (`"segreto"`, `"fuori"`, `"dentro"`, `"manca"`,
`"esterna"`, `"Artista"`, `"non toccare"`, ...) and one path segment in
`internal/names/relpath_test.go` (`"  spazi  "`). Renaming them consistently
would not change what the tests check. To decide whether to translate them in a
separate change.
