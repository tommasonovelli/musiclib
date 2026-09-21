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
- [ ] Containerized toolchain: build, tests and tooling run in Docker, not only the deployment (owner requirement, N-012)
- [ ] Full repository layout (§2.3): the other packages are still missing
- [ ] Docker Compose: `app` + PostgreSQL 17, pinned digests (§2.1, §11.1)
- [ ] `goose` migrations of the normative schema (§4.2)
- [ ] `sqlc` queries (§2.1)
- [ ] `internal/fsops`: confined primitives (`openat2 RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS`, `renameat2`, fsync) (§10.4)
- [ ] Volume lock (`flock` on `/data/.lock`) and volume identity (`.musiclib-store` ↔ `settings.store_id`) (§2.2, §11.1)
- [ ] `internal/blobstore`: 5-step verified put, dedup, `corrupt_blob` (§7.5)
- [ ] Boot checks: same filesystem, `RENAME_EXCHANGE` available (§3.1)

## Phase 2 — First vertical slice (one FLAC album)

- [ ] `internal/media`: `ffprobe` / `ffmpeg` adapter, `AudioDigest` (§8.4)
- [ ] `native/musiclib-tags`: C++ TagLib helper (inspect / extract-images / write-managed-tags) (§8.1)
- [ ] Managed tag mapping and alias removal (§8.2, §8.3)
- [ ] `internal/importer`: import of a single album candidate
- [ ] `internal/catalog`: domain transactions, revisions, reservations, enqueue (§4.3, §5.3)
- [ ] `internal/render`: snapshot → pure plan → build in staging (§9.1)
- [ ] `.musiclib.json` receipt (§9.2)
- [ ] `internal/publish`: PREPARE / INSTALL / FINALIZE + journal (§9.3)
- [ ] Journal recovery at startup (§9.4)
- [ ] `internal/jobs`: claim, pool, completion (§6.2, §6.4)

## Phase 3 — Concurrency

- [ ] Render coalescing on a single row per album (§6.3)
- [ ] `REPEATABLE READ` snapshots (§6.2)
- [ ] `path_claims` and global `pg_advisory_xact_lock` (§5.3)
- [ ] Conditional APIs: strong ETag, `If-Match`, 412/428 (§10.1)
- [ ] Artist rename and album reassignment (§4.3)
- [ ] Named failpoints and failure matrix (§12.2)

## Phase 4 — Formats and content

- [ ] MP3 (ID3v2.4, APE, ID3v1 migration), M4A AAC/ALAC (§8.1–8.3)
- [ ] Covers: selection, limits, upload, removal (§7.4, §8.5)
- [ ] LRC files associated with tracks (§7.4)
- [ ] Attachments under `Extras/` (§5.1, §7.4)
- [ ] Verification and preservation of unmanaged tags (§8.3)

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
