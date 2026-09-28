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
- [x] Full repository layout (§2.3): `internal/maintenance` implements doctor, rebuild, backup and restore; `scripts/` supplies the offline Compose commands.
- [x] Docker Compose: `app` + PostgreSQL 17, digests pinned (§2.1, §10.4, §11.1) — non-root, `init`, `restart: unless-stopped`, loopback only, healthcheck via `musiclibd healthcheck`; verified end to end (N-071). ffmpeg/ffprobe (N-073) and the static TagLib helper `musiclib-tags` (N-083) are in the runtime image since Phase 2. Since release 1.0.0 (T3, N-329): production `compose.yaml` from the published image, development `compose.dev.yaml` built from source, same project and volumes
- [x] `goose` migrations of the normative schema (§4.2), applied forward only under an advisory lock — `migrations/`, `store.Migrate`
- [x] `sqlc` setup (§2.1): pinned image, generated code committed, `sqlc diff` in the gate — `sqlc.yaml`, `sql/`, `internal/store`; Phase 1 queries only (store id, migration lock)
- [x] pgx pool with `WORKERS + 8` connections (§11.1) and UUIDv7 ids (§2.1) — `store.NewPool`, `store.NewID`
- [x] Real PostgreSQL 17 for the tests (§12.1, N-024) — `postgres-test` service, `internal/store/pgtest`
- [x] `internal/fsops`: confined primitives (`openat2 RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS`, `renameat2`, fsync) (§10.4)
- [x] Volume lock (`flock` on `/data/.lock`) and volume identity (`.musiclib-store` ↔ `settings.store_id`) (§2.2, §11.1) — `internal/volume`; maintenance marker read and enforced (§11.3)
- [x] `internal/blobstore`: 5-step verified put, dedup, `corrupt_blob` (§7.5)
- [x] Boot checks: same filesystem and mount, permissions, `RENAME_EXCHANGE` available (§3.1) — `volume.CheckFilesystem`
- [x] `cmd/musiclibd`: environment configuration, umask 022, no root, JSON logs, boot steps 1 to 7 (steps 4–7 since Phase 2, round 9), `/health/live` and `/health/ready`, `healthcheck` subcommand, graceful shutdown with the lock released last (§2.3, §6.1, §11.1)

## Phase 2 — First vertical slice (one FLAC album)

- [x] `internal/media`: `ffprobe` / `ffmpeg` adapter, `AudioDigest` (§8.4) — pinned static FFmpeg 8.1.3 in every image (N-073), tool Runner (§8.5, §6.1), probe and classification (§7.2, §8.1), boot check of the tool versions (§11.1 step 3)
- [x] `native/musiclib-tags`: C++ TagLib helper (inspect / extract-images / write-managed-tags) (§8.1) — **FLAC complete**: pinned static TagLib 2.3.2 (N-083), the three operations, the Go adapter `Inspect` / `ExtractImages` / `WriteManagedTags` and the §9.1 step 6 check `VerifyTags`, ID3 tags in a FLAC stripped by a declared rule (N-090); **MP3 complete since round 12** (helper version 3: its own ID3v2/APE/ID3v1 reader and writer, N-152); **M4A (AAC and ALAC) complete since round 13** (helper version 4: its own box walker, ilst reader and writer, N-165)
- [x] Managed tag mapping and alias removal (§8.2, §8.3) — **FLAC, MP3 and M4A complete** (tables in `native/musiclib-tags/src/fields.h`, N-088, N-159, N-166)
- [x] `internal/importer`: import of a single album candidate (§7.1–§7.6) — batch creation (`catalog.CreateImportBatch`), the scan executor (§7.2 rules 1 to 5; the multi-disc rules 2 and 3 since round 15, N-183 to N-189), the import executor of one FLAC, MP3 or M4A candidate, formats mixed or not (N-157) (revalidation, source stability, space check, verified copies, full decode, tags, metadata, LRC, cover, fingerprint, commit); an M4A the tag reader refuses (fragmented, encrypted, several tracks) is `unsupported_audio` (N-165). The executors (`ExecuteScan`, `ExecuteImport`) run in the pool of `cmd/musiclibd` since round 9; the space check reserves in the process budget (N-139)
- [x] `internal/catalog`: domain transactions, revisions, reservations, enqueue (§4.3, §5.3) — import commit (§7.6), `PUT` album semantics, trash/restore, artist rename, `path_claims`, `CheckFresh`; the transaction runner and the catalog lock in `internal/store` (N-095)
- [x] `internal/render`: snapshot → pure plan → build in staging (§9.1) — `render_version` (§2.1, N-010 resolved by N-130), the pure planner, the verified build in `work/render/<build_id>/album` with its failure cleanup; run by `publish.ExecuteRender` since round 9
- [x] `.musiclib.json` receipt (§9.2) — canonical encoder, strict parser for recovery and doctor, `receipt_hash` (N-133)
- [x] `internal/publish`: PREPARE / INSTALL / FINALIZE + journal (§9.3) — preflight, `publishMu`, suspension after PREPARE (N-135), ownership rules (N-137), the render executor `ExecuteRender`
- [x] Journal recovery at startup (§9.4) — `Publisher.Recover`, idempotent, forward only; an illegal state suspends publishing with `publish_illegal_state`: the process stays up, not ready, no worker, deleting nothing (owner decision N-135, round 10)
- [x] Boot steps 4 (journal recovery), 5 (`work/render`, `work/retired` and the other leftovers cleaned, running → pending), 6 (stale renders) and the worker pool of step 7 with the scan, import and render executors (§11.1); exit on database loss or a pending publication (§6.4, N-070, N-135); shutdown with the 30 s grace for a prepared publication
- [x] `internal/jobs`: claim, pool, completion (§6.2, §6.4) — the single `EnqueueRender`, the REPEATABLE READ claim with its snapshot, ticket-conditioned completions, boot helpers, the worker pool, wired in `cmd/musiclibd` with two workers tested end to end (§13.1); `jobs.Stop` for a pending publication; the process space budget `jobs.Budget` (§11.2)

## Phase 3 — Concurrency

- [x] Render coalescing on a single row per album (§6.3) — `jobs.EnqueueRender`, ticket-conditioned completions
- [x] `REPEATABLE READ` snapshots (§6.2) — `jobs.ClaimNext`, `jobs.RenderSnapshot`
- [x] `path_claims` and global `pg_advisory_xact_lock` (§5.3) — `store.InCatalogTx`, `catalog.ReconcileClaims`
- [x] Conditional APIs: strong ETag, `If-Match`, 412/428 (§10.1) — `internal/http` (round 11): ETags `"album:<uuid>:<rev>"` / `"artist:<uuid>:<rev>"`, the If-Match rules of N-147, the revision compared by the catalog in the transaction of the change; two concurrent saves of one revision give exactly one 412 and lose nothing
- [x] Artist rename and album reassignment (§4.3) — `PUT /api/artists/{id}` (`RenameArtist`) and `PUT /api/albums/{id}` with another `artist_id` (`UpdateAlbum`), tested over HTTP on real PostgreSQL and end to end through the server
- [x] Named failpoints and failure matrix (§12.2) — one mechanism (`internal/failpoint`, `internal/faulttest`, N-142); real SIGKILL crashes for every row about durability or idempotency; a really full filesystem (N-143); the matrix is below. Every row now has real tests (the Phase 6 rows since the maintenance commands)

## Phase 4 — Formats and content

- [x] MP3 (ID3v2.4, APE, ID3v1 migration) (§8.1–8.3) — round 12: the helper's own ID3v2.2/2.3/2.4, APE and ID3v1 reader and ID3v2.4 writer (N-152), the alias and sort table (N-159), the ID3v1 exclusion in `VerifyTags` (N-153), the MP3 decode window (N-154), the cover limit (N-155); imported, rendered and published end to end (`publish.TestExecuteRenderMP3Album`)
- [x] M4A AAC/ALAC (§8.1–8.3) — round 13: the helper's own box walker, ilst reader and writer with chunk-offset fix-up (N-165), the atom, alias and sort table with the numeric `gnre` (N-166), the canonical form and `VerifyTags` without any extra exclusion (N-167), no decode window and cover stream ordering preserved (N-168), the cover limit (N-166); imported, rendered and published end to end (`publish.TestExecuteRenderM4AAlbum`)
- [x] Covers: selection, limits, upload, removal (§7.4, §8.5) — the import selection and its limits (`internal/importer`, the N-091 limit in `media.EmbeddedCoverFits`, MP3 since round 12, N-155, M4A since round 13, N-166); round 14: `PUT /api/albums/{id}/cover` (an upload validated by `media.ValidateCover`, or an image attachment of the album, which stays an attachment; N-091 per format, 422 `cover_not_embeddable`) and `DELETE` (no `cover.*`, no embedded picture in FLAC, MP3 and M4A; N-168's residual observed as accepted), N-174, N-181
- [x] LRC files associated with tracks (§7.4) — at import (N-117); round 14: `PUT` (an upload of at most 2 MiB of UTF-8, or an `.lrc` attachment of the album, which stays an attachment: N-177, owner decision 2026-09-25) and `DELETE /api/albums/{id}/tracks/{track}/lyrics`, the file next to its track
- [x] Attachments under `Extras/` (§5.1, §7.4) — collected at import and materialized by the build (`internal/render`, sanitized per segment, collisions refused); round 14: `POST /api/albums/{id}/attachments?path=` (at most 256 MiB streamed into the put, the path through the one normalization of §5.2, 409 for a collision) and `DELETE .../attachments/{attachment}` (the blob and the cover stay, N-176)
- [x] Verification and preservation of unmanaged tags (§8.3) — FLAC done (`media.VerifyTags`, N-086, the ID3-in-FLAC exclusion N-090); MP3 done (round 12: frames and APE items kept byte for byte, the declared ID3v1 migration, N-153); M4A done (round 13: items and boxes kept byte for byte, the samples checked through the sample table, N-167)

## Phase 5 — Full experience

- [x] Recursive scan and multi-disc grouping (§7.2) — the recursive scan, rules 1 to 5 and the unassigned-file report; round 15: `CD<N>` / `Disc <N>` directories grouped into one multi-disc candidate, `duplicate_disc` and `invalid_disc` (over 99) failing the branch, audio below a disc directory ambiguous once at least one disc directory has direct audio, rule 5 otherwise (N-183, N-184, owner decisions of 2026-09-26), attachments inside and beside the discs, limits over every disc, revalidation and stability covering the disc directories; `multidisc_not_supported_yet` removed
- [x] Inferred initial metadata and import overrides (§7.3) — the whole table and the overrides are applied by the importer; round 15: the disc of a disc directory wins over the tag with `disc_tag_ignored` (N-185), numbering per disc, the title from the candidate root, the cover from the candidate root (N-186); `tracks_renumbered` names the disc directory in a multi-disc album; round 16: the overrides `{artist, title}` through `POST /api/jobs/{id}/retry` (strict, normalized, import only, N-195), tested end to end (`mixed_album` fixed by a retry) and in the importer tests, whose retries now go through `catalog.RetryJob`
- [x] Fingerprinting and duplicate detection (§7.6) — the fingerprint in `internal/importer` (golden test), duplicates `skipped` by the import commit
- [x] Complete HTTP APIs (§10.2) — round 11: `GET`/`POST /api/artists`, `GET`/`PUT /api/artists/{id}`, `GET`/`PUT`/`DELETE /api/albums/{id}`, `GET .../status`, `POST .../restore`, `POST .../render`; round 14: cover, attachments, lyrics, track deletion and the downloads by entity id (N-172 to N-182); round 16: `GET /api/albums` (search, filters, keyset pages), `GET /api/import-source`, `POST /api/imports`, `GET /api/imports/{id}`, `GET /api/jobs`, the retries, `render-all` (N-190 to N-201), and the §6.1 limit of two upload copies (N-199)
- [x] UI: Library and Album editor (round 17); Import and Activity (round 18), SSR + embedded CSS/JS, real-browser tests for import, retry, ETag conflict, cover and trash/restore (§10.3, §12.1); round 19: presentation and form usability of the four views (flat brutalist design system, no assets, light/dark, 360px upwards), behaviour unchanged (N-236 to N-242)
- [x] HTTP security boundary: `PUBLIC_ORIGIN`, `X-Musiclib-Request` (§10.4) — `internal/http` (round 11, N-145): Host and Origin against `PUBLIC_ORIGIN`, `X-Musiclib-Request: 1` on every mutation, no CORS ever, `nosniff` everywhere; the health endpoints exempt from the Host check. The UI side (fetch setting the header, template escaping) is covered in round 17 (N-203, N-207)
- [x] Trash, restore, retry (§4.3, §6.4) — trash and restore in the catalog and over HTTP (`DELETE /api/albums/{id}`, `POST .../restore`, round 11); round 16: `POST /api/jobs/{id}/retry` and `POST /api/jobs/retry-failed` (N-195, N-196), the §6.4 retention of the import reports (N-200); the trash as a filter of `GET /api/albums` (N-190)

## Phase 6 — Operations

- [x] `doctor`, normal and `--deep` (§11.3) — read-only CLI and typed findings; real rendered-album corruption, forced-render repair, rebuild/deep convergence and structural tests (`maintenance.TestDoctor*`, `cmd/musiclibd.TestOfflineDoctorRepairAndRebuildConverge`).
- [x] `rebuild` with maintenance marker (§11.3) — confirmed store UUID, confined deletion, atomic derived-state reset/reenqueue, SIGKILL/repeat windows, real server convergence, trashed outputs excluded (`maintenance.TestRebuild*`).
- [x] `backup` / `restore` (§11.4) — streaming verified originals and PostgreSQL 17 custom dump/restore, complete manifest, durable rename, fresh-destination preconditions, crash windows and real fresh-volume end-to-end release tests (`maintenance.TestBackup*`, `TestRestore*`, `cmd/musiclibd.TestBackupLossRestoreBootAndDoctor`, `TestReleaseCollectionInterruptedAndRestored`).
- [x] Space budget and `statfs` check (§11.2) — the conservative estimates of the import and of the build, reserved atomically in the process budget `jobs.Budget` against `statfs` minus the 1 GiB margin (N-114, N-139); ENOSPC handled by every write
- [x] Operations guide and tested Compose stop/run/start scripts (§11) — `docs/operations.md`, `scripts/{doctor,rebuild,backup,restore}.sh`; lint and actual Compose backup/doctor/start with a space-containing archive name.
- [x] Maintenance scripts on the installation's Compose file (§11.3, §11.4) — `compose.yaml`, or `COMPOSE_FILE=compose.dev.yaml` for a source build; plain-Compose equivalents documented for installs without the repository (T3, N-331).

## UI redesign (rounds 19–22, `webui-principles.md`, NOTES.md N-243 to N-307)

- [x] Round 19 — design tokens (colours light/dark, type scale, 4px steps, radii with `corner-shape: squircle`, the one cover shadow, one curve and two durations, focus ring, tabular numbers, reduced motion) in `web/app.css`; the "flat structural" style removed (N-243, N-251)
- [x] Round 19 — Hanken Grotesk self-hosted (latin + latin-ext woff2, OFL.txt), served as `font/woff2` and cached as immutable (N-244)
- [x] Round 19 — shell: Italian sidebar (Libreria, Importa, Attività, Cestino), «Da sistemare» count on every page and its filter through sqlc, activity dot kept, a row of entries below 52rem, skip link and focus (N-245)
- [x] Round 19 — Library: cover grid with initials placeholders and status dots, live search with artists chosen through it, loading on scroll with a real «Mostra altri» fallback, the open album under its row with cover colours and the 4.5:1 rule, arrow keys, view transition to the editor, the three empty states, Cestino (N-246 to N-248)
- [x] Round 19 — album, import and activity restyled with the tokens only; every existing browser test unchanged and passing (N-249)
- [x] Round 20 — album editor: header with the 240 px cover (drop, menu, image-only picker), the title as the field, the artist picker with «Create artist» and «Rename artist»; tracks by disc with ⋯ menus; inherited values and «No genre»; the Save bar with in-place save, ETag from the answer and «Saved»; `beforeunload`; conflict with «Reload and reapply my changes»; bulk `.lrc`; extra files with «Save as»; trash band; errors in one sentence with «Details»; «Update in library» kept in the album menu; English copy on the album page by owner decision (N-256 to N-269)
- [x] Round 20b — the UI in English: layout, Library, panel, status words, page titles, Import and Activity copy (as far as their strings go), `lang="en"`, and a guard against Italian copy in the assets and the rendered pages (N-270, N-271, N-274)
- [x] Round 20b — sidebar: hand-drawn inline SVG icons, 248px wide, a button that collapses it to its 72px icon column (tooltips, the count as a badge, remembered per browser and applied before the first paint by a classic `sidebar.js`), icon-over-label row on phones (N-272, N-273)
- [x] Merge of rounds 20 and 20b — one sidebar, one set of status words and one voice on every page; the album page's in-place refresh keeps the collapsed sidebar; the Save bar follows the sidebar's width; the Italian guard covers every file (N-278); CSS + JS budget raised to 90 KB by the owner (N-267)
- [x] Review of rounds 20, 20b and the merge — an invalid Disc in a closed ⋯ menu no longer stops Save silently; downloads no longer trigger the unsaved-edits prompt; a trashed album's back link leads to the Trash and follows a trash/restore in place; track fields end in an ellipsis; two surviving mutants killed by new tests (N-279 to N-283)
- [x] Round 21 — Import: the music folder Finder-style with per-folder counts from names and a breadcrumb (no JS needed), one filled «Import everything in …», the fixed sentence, a progress row, results in three ARIA tabs with counts opening on the first non-empty one, one sentence and one fix per error code (Retry only where it can work), rows updated in place, «Recent imports» (N-286 to N-288, N-291)
- [x] Round 21 — Activity: In progress / Waiting / Needs attention with thumbnails, album names and relative times, one filled «Retry all» with its count, Advanced «Rebuild the library folder» with its confirmation and count, empty state (N-289, N-290)
- [x] Round 21 — stale failures (owner): Dismiss and automatic supersession through migration 00002, `POST /api/jobs/{id}/dismiss`, «Retry all» leaves them alone (N-285)
- [x] Round 21 — the Library head compacts while stuck without ever changing the page height (owner, N-284); leftovers removed, N-271 resolved; mutation checks and screenshots (N-292 to N-294)
- [x] Review of round 21 — the owner's five live failures checked against the supersede rule; two unguarded behaviours (the focus handed to a neighbour when a focused row leaves, no Dismiss on failed renders) and the Italian guard over the sentences of `queuepages.go` now tested; mutation checks (N-295)
- [x] Round 21 — owner decisions after the review: N-287 decided as implemented; the queue pages render only what has something to say (no idle «in progress», no «And 0 more», no empty notices or «Details», no empty groups), with polling across empty and back (N-296)
- [x] Round 22 — no orphan artists (owner): an artist whose last album, active or trashed, moves to another artist is deleted in the same transaction, under the catalog lock; concurrent moves and arrivals tested (N-297)
- [x] Round 22 — `PUT /api/albums/{id}` takes `new_artist` (exactly one of it and `artist_id`), created in the save's transaction: a 412 or a refused save creates nothing; «Create artist» only stages the name until Save; `POST /api/artists` kept for API clients; the SQL for the existing orphans in NOTES, to run by hand (N-298, N-299)
- [x] Round 22 — track durations: `blobs.duration_ms` (migration 00003), recorded at import from the probe and, while unknown, by the render as a permanent rule (never overwritten, a failing probe never fails a render); `duration_ms` in the album JSON; a read-only Duration column (m:ss, dash when unknown, phone layout), the album's length under the tracks, the times in the Library's panel (N-300 to N-302); tests, mutation checks, screenshots (N-303 to N-305)
- [x] Review of round 22 — orphan deletion, the `new_artist` contract, the durations and the N-299 SQL checked; four unguarded behaviours (a duration lost on a blob of unknown format, 0 taken as a known duration, the minute rounding of the album length, the album's artist restored when a staged name is dropped) now tested; mutation checks (N-307)
- [x] Round 23 — the identity Vibrance MusicLib (owner): the accent is MusicLib's Violet in both themes, contrast measured (N-310); the sidebar opens with the lockup (the symbol in Violet, the name outlined from Bricolage Grotesque in ink), the symbol stays when collapsed and the toggle moves to the foot so collapsing moves nothing (N-309, N-311); the SVG favicon with its dark twin, `/favicon.ico` not served (N-312); page titles «… — MusicLib»; the name uniform in README, docs, DESIGN.md's title and `web/` comments, identifiers unchanged (N-308). By owner decision no new tests: existing tests adjusted only where they asserted the old text or layout; review screenshots (N-314, N-315). Fix pass after review (N-316): by owner decision only the logo, the sun symbol, is outside the MIT License, in `LOGO.md` with `LICENSE` unchanged; the names are not reserved (N-313, N-317, N-318)

---

## §12.1 and §12.3 acceptance

All §12.1 levels are covered by the full Docker gate: unit and planner tests,
real PostgreSQL 17 transactions, redistributable FLAC/MP3/AAC/ALAC fixtures,
real ext4/openat2/rename/fsync and SIGKILL crash tests, real Chromium UI tests,
and `go test -race`. N-024 documents the deliberate Compose PostgreSQL
substitution for testcontainers without mounting the Docker socket. The
§12.3 seven-album real-process API/import/edit/SIGKILL/backup-loss-restore test
is `cmd/musiclibd.TestReleaseCollectionInterruptedAndRestored`, run by the gate.
**Release gap:** N-017: rerun the gate and acceptance on native Ubuntu 24.04+
Docker Engine/ext4; Docker Desktop's VM is not a native-host acceptance run.

## The §12.2 failure matrix

Status: **crash** = tested with a real process crash (SIGKILL of a child
process at a named failpoint, then the next boot), or a real process
losing the database or its helpers; **in-process** = tested with real
PostgreSQL 17, real ext4 and the real tools, failures injected at the
named points. No row remains deferred.

| §12.2 row | Required property | Status | Tests |
|---|---|---|---|
| Due worker sullo stesso job | Uno solo ottiene il claim | crash + in-process | `jobs.TestCrashAroundTheClaim` (killed inside the claim transaction, after its commit, in the executor: exactly one execution after `RecoverRunning`); `jobs.TestOneClaimPerJob`, `TestEveryJobClaimedOnce`, `TestPoolExecutesEachJobOnce` |
| Due import dello stesso candidato / commit dalla risposta persa | Un album e un esito durevole, non duplicati | crash + in-process | round 16: `http.TestRetryAnswerLost` (a retry and a `POST /api/imports` whose COMMIT answer is lost, repeated after the restart: one ticket, one batch), `http.TestCreateImportConcurrently`; `importer.TestCrashAtTheImportCommit` (before, after), `TestImportCommitAnswerLost` (answer lost, connection cut before the COMMIT; the process stops and restarts), `TestTwoProcessesImportTheSameCandidate` (two processes committing at once); `importer.TestCrashAtTheScanCommit`; `catalog.TestCommitImportLostAck`, `TestCommitImportConcurrentSameFingerprint`, `TestCommitImportIdempotent` |
| Stesso blob fissato contemporaneamente | Un file integro, nessun overwrite | crash + in-process | uploads go through the same put (round 14, §7.5); `blobstore.TestCrashDuringPut` (killed at `temp_synced`, `temp_verified`, `shards_synced`, `pinned`; `CleanTemps`; re-put idempotent, same inode); `blobstore.TestPutConcurrentSameContent` |
| Blob esistente corrotto | Errore, nessuna fiducia nel solo nome | in-process | `blobstore.TestPutDoesNotTrustExisting`; `render.TestBuildCorruptOriginal` |
| Modifica API durante un build | Build obsoleta scartata, nuova richiesta conservata | in-process | `publish.TestPublishSupersededBuild`, `TestExecuteRenderSupersededAndCancelled`; `jobs.TestCoalescing` |
| Modifica fra PREPARE e FINALIZE | Revisioni distinte e secondo render ancora necessario | in-process | `publish.TestPublishChangeBetweenPrepareAndFinalize` |
| Riuso di un vecchio nome non ancora ritirato | Conflitto di prenotazione, mai furto di ownership | in-process | `publish.TestPublishNameReuseDuringRename`; `catalog.TestRenameReservation` |
| Scambio numeri traccia | Transazione valida senza collisioni intermedie | in-process | `catalog.TestTrackSwap`; `store.TestTrackReorderIsChecked` |
| Crash prima/dopo PREPARE | Vecchio output valido oppure journal recuperabile | crash | `publish.TestCrashMatrix/*/{preflight,prepared}`; `publish.TestCrashDuringBuild` (killed at `reserved`, `write`, `tags_written`, `fsync_file`, `fsync_dir` of a real build: old album and originals intact, staging removed at step 5, the render publishes after the restart); `TestRecoverBeforePrepare` |
| Crash subito dopo exchange, prima degli fsync, prima/dopo FINALIZE | Recovery non esegue uno scambio inverso | crash | `publish.TestCrashMatrix/exchange/{installed,synced,finalized}`, `TestCrashDuringRecovery`; every case recovered twice |
| Crash tra installazione del nuovo path e ritiro del vecchio | Entrambi completi, recovery rimuove solo il vecchio owner | crash | `publish.TestCrashMatrix/rename/{installed,retired}`, `TestCrashDuringRecovery` |
| Disco pieno durante build | Vecchio album e originali intatti | really full fs + in-process | `publish.TestExecuteRenderOnAReallyFullDisk` (a real album on the full tmpfs `/fullfs`; ENOSPC at the copies, the attachment, the receipt; N-143), `TestExecuteRenderMP3OnAReallyFullDisk` (ENOSPC inside the tag helper's own write: `insufficient_space`); `media.TestTagsMP3NoSpace`; `blobstore.TestPutOnAReallyFullFilesystem`; `render.TestBuildNoSpace` (ext4's ENOSPC at fsync, injected) |
| Errore DB dopo rename | Journal completato al riavvio | in-process (real wire loss) + crash | `publish.TestRecoverDatabaseErrorAfterRename` (the FINALIZE commit cut, its answer lost, through `pgtest.Proxy`); `cmd/musiclibd.TestProcessDatabaseLossMidWork` |
| Tag writer altera i campioni o perde un tag non gestito | Album non pubblicato | in-process | `render.TestBuildTagWriterAltersSamples`, `TestBuildTagWriterLosesUnmanagedField`, `TestBuildMP3OpaqueField`; `media.TestVerifyTags`, `TestVerifyTagsID3v1Migration`, `TestTagsMP3MutationSweep` |
| Modifica dell'output con size/mtime invariati | Doctor deep la rileva e render/rebuild la ripara | in-process (real ext4 and renderer) | `maintenance.TestDoctorDetectsUnchangedSizeAndMtimeDamage`, `cmd/musiclibd.TestOfflineDoctorRepairAndRebuildConverge`: deep-only detection, forced render repairs real FLAC, rebuild converges again |
| Symlink, path assoluti, traversal, collisione file/directory | Nessuna operazione fuori root, errore prima della pubblicazione | in-process | round 16: `http.TestImportSource` (the /import listing: `..`, absolute paths, empty segments, NUL, symlinks to /data, outside and inside, through a symlink, a FIFO, never followed nor opened, no absolute path answered), `cmd/musiclibd.TestBootRefusals` (`import_is_data`); `http.TestPostAttachment` (round 14: an uploaded attachment's absolute, `..`, empty-segment, NUL and too-deep paths refused before its body is read; file/directory, case, NFC and sanitization collisions 409, nothing pinned), `catalog.TestAddAttachment`; `fsops.TestInvalidPathsRejectedBeforeDisk`, `TestLeafSymlinkRejected`, `TestIntermediateSymlinkComponentRejected`, `TestTOCTOUConcurrentSymlinkSwap`; `render.TestPlanCollisions`, `TestPlanRefusals`; `publish.TestPublishRefusesSymlinksAndSpecialFiles`, `TestRecoverIllegalStates` |
| Due finestre UI salvano revisioni diverse | Una riceve 412, nessuna modifica persa silenziosamente | real Chromium + PostgreSQL (round 17), API in-process (round 11) | `http.TestBrowserEditorConflictAndContent` (two real tabs, one winner, loser keeps edit and sees 412; track deletion, cover, trash/restore); `http.TestConcurrentCoverUploads` (round 14: two cover uploads on one revision, exactly one 412, the loser's blob absent or pinned and unreferenced), `http.TestUploadFailsAfterThePut` (a change between the put and the transaction: 412, the blob unreferenced), `catalog.TestDeleteTrackConcurrent`; `http.TestTwoClientsSaveDifferentRevisions` (20 rounds of two concurrent PUTs of one revision through a real server: exactly one 412 naming the winner's revision, the loser reloads and re-applies, both changes kept); `catalog.TestUpdateAlbumRevisions` (412/428 in the change's transaction) |
| SIGTERM/SIGKILL con più worker e helper attivi | Nessun helper del vecchio tentativo resta in attività | crash | `cmd/musiclibd.TestProcessSignalsWithHelpersActive`, `TestProcessDatabaseLossMidWork`; `media.TestRunToolDiesWithParent` (Pdeathsig), `TestRunCancelKillsAndReapsWholeGroup` |
| Crash durante rebuild o restore | Marker impedisce il boot su una manutenzione incompleta | crash | `maintenance.TestRebuildRealCrashWindows` (six SIGKILL points, marker refusal and repeat), `TestRestoreRealCrashWindows` (six SIGKILL points, marker refusal then new destinations), `volume.TestMaintenanceMarkerCrashWindows` |
| Backup, perdita DB/volume, restore su volumi nuovi | Catalogo e originali recuperati, output rigenerato verificabile | in-process + crash (real server, tools, PostgreSQL/ext4) | `cmd/musiclibd.TestBackupLossRestoreBootAndDoctor`, `TestReleaseCollectionInterruptedAndRestored` (real server SIGKILL before backup, fresh DB/volume, real render, deep doctor); `maintenance.TestBackupRealCrashWindows`, `TestRestoreRealCrashWindows` |

Beyond the rows, with real crashes: the first initialization
(`volume.TestFirstInitInterruptedAtEveryStep`, 7 points, N-061) and an
illegal journal at boot, which keeps a real server process up and not
ready (`cmd/musiclibd.TestProcessSuspendedByAnIllegalJournal`, N-135).

```sh
scripts/check.sh                                   # everything, /fullfs required
scripts/dev.sh go test -race -count=5 -timeout 50m -run 'TestCrash|ReallyFull|AnswerLost|TwoProcesses|Suspend' \
  ./internal/blobstore/ ./internal/jobs/ ./internal/importer/ ./internal/publish/ ./cmd/musiclibd/
```

---

## Details of what is done

### Phase 6: offline maintenance (`internal/maintenance`, `internal/volume`, `cmd/musiclibd`, `scripts/`) ✔

Public API of `internal/maintenance` (the caller holds the volume flock):

| Function | Role |
|---|---|
| `Doctor(ctx, db, v, deep) (Report, error)` | read-only inspection (§11.3); `Report.Findings` of `Finding{Severity, Code, Entity, Advice}`, `HasErrors()` |
| `Rebuild(ctx, db, v, confirmedStoreID, hook) error` | marker, delete only `library/` and `work/`, one-transaction derived reset, recreate, remove marker (§11.3) |
| `Backup(ctx, db, v, to, dbURL, hook) error` | new temporary, `pg_dump` custom, verified originals, manifest, fsync, no-replace rename (§11.4) |
| `Restore(ctx, db, v, from, dbURL, hook) error` | empty destinations, full archive verification, marker, `pg_restore`, forward migrations, originals via `blobstore.Put`, identity, derived reset (§11.4) |
| `RequireCurrentSchema(ctx, db) error` | doctor/rebuild/backup refuse a schema other than the latest embedded one, never migrating nor creating goose's table (N-225, N-235) |
| `Error{Code, Message, Refusal, Err}` | typed error: `Refusal` = precondition refused (exit 2), otherwise an attempted operation failed (exit 1) |
| `Manifest`, `BackupBlob` | the backup's `manifest.json`: store id, schema, app and render versions, dump SHA-256, sorted blob list |

- **Doctor.** Five sqlc inventory queries (`sql/catalog.sql`) plus structural
  checks (active album without tracks, broken blob references, render job
  without album). Normal mode: referenced blobs exist with their size,
  reservations equal the pure §5.3 union shared with `catalog.ExpectedClaims`,
  receipts match `published_receipt_hash` and list existing regular files,
  extra output is reported; pending renders and outdated renderers are
  `info`, a journal is a `warning` and its old/new paths are not judged as
  damage (N-229). Deep mode streams SHA-256 of every present original and
  every receipt-listed file (audio, tags, cover, attachments, lyrics).
  Symlinks, special and unreadable entries are per-entry findings, never
  followed. `blobstore.OpenReadOnly` avoids creating `work/blobs`.
- **Marker** (`internal/volume`). `BeginMaintenance` installs `.maintenance`
  by an fsynced temporary, `RENAME_NOREPLACE` and a directory fsync; the same
  operation and store may repeat, a foreign marker is refused;
  `EndMaintenance` checks ownership, unlinks and syncs. `IdentifyExisting` and
  `OpenExistingLayout` pair and open an existing store without initializing
  anything; `PrepareRestoreLayout`/`CompleteRestoreIdentity` create the
  restored layout and then the identity marker behind the restore marker.
- **Rebuild.** Store id confirmed against both identities; `checkDeletionTree`
  refuses another mount or a non-directory before `RemoveAll` (never follows
  symlinks, survives partial deletion); `catalog.ResetDerivedForRebuild`
  clears journal, published columns, render jobs and all claims, then
  reconciles claims and enqueues a render for each active album in one
  catalog transaction. Trashed albums lose their output and get no claim or
  render (N-218).
- **Backup.** Destination confined: absolute, no symlink component, no
  ancestor with the identity of `/data`, `originals/`, `library/` or `work/`
  (N-228), and not under the data root on the host either (nested bind
  mounts: device and root from `/proc/self/mountinfo` via `fsops.SourceOf`,
  N-228). Credentials go to `pg_dump` only through `PGPASSWORD`; its stderr
  is capped and redacted (N-226). Originals are copied with SHA-256 checked
  during the copy; any unexpected entry fails the backup. A failure leaves
  one `.musiclib-backup-*.tmp` for the operator (N-221).
- **Restore.** Refuses any database object and any volume entry besides
  `.lock` and an empty `lost+found` (N-221, N-230 D2); verifies the canonical
  manifest, the dump hash and every original **before** the marker (N-227);
  then `pg_restore --single-transaction`, `store.Migrate`, store id and
  inventory checks, `blobstore.Put` of every original, identity marker,
  `catalog.ResetDerivedForRestore` (the rebuild reset plus nonterminal
  scan/import jobs failed with `source_needs_verification`), marker removal.
- **Commands.** `musiclibd doctor [--deep]`, `rebuild --store-id UUID`,
  `backup --to DIR`, `restore --from DIR` share one preamble
  (`cmd/musiclibd/offline.go`: umask 022, non-root, config, non-blocking flock,
  10-second connect) and never start the server. Human report on stdout, JSON
  logs on stderr; exit 0 success, 1 damage or failed operation (logged with an
  `advice` field), 2 refusal or usage (N-221, N-235). SIGTERM/SIGINT cancel.
- **Scripts.** `scripts/{doctor,rebuild,backup,restore}.sh` with
  `scripts/lib/maintenance.sh`: stop the app, `compose run --rm --no-deps app
  …`, restart only after success (doctor also after exit 1) and only if it was
  running (N-230 D1); quoted names with spaces; lint-clean; run for real
  against an isolated Compose project (N-233).
- **Images.** Runtime and toolchain carry PostgreSQL 17.11 clients from a
  dated signed Debian snapshot; `/backup` is created owned by the app UID;
  the toolchain's Chromium closure is frozen on the same snapshot (N-212,
  N-222, N-226); the runtime build runs `pg_dump --version` and
  `pg_restore --version` at the paths `musiclibd` executes (N-230 D3). Owner
  decisions D1–D5 are recorded in N-230 (2026-09-27). `docs/operations.md` is the Ubuntu/ext4 operations guide and
  native release procedure (N-017, N-231).

Tests (real PostgreSQL 17, ext4, tools and processes): `maintenance.TestDoctor*`
(clean, missing/short/same-size-damaged originals, missing/extra output,
receipt damage, same-size same-mtime output damage caught only by deep,
claims, pending work, journal untouched, rename journal not damage, symlinks);
`TestRebuild*` (catalog and originals kept, trashed albums, symlinks, partial
deletion, six SIGKILL windows with convergence checks); `TestBackup*`
(manifest, refusals by identity and by host path, corrupt original, missing
zero-size referenced blob, five SIGKILL windows leaving
only the temporary); `TestRestore*` (fresh destinations, `lost+found`,
nonempty DB/volume, corrupt archive refused before the marker, pending import
failed, six SIGKILL windows then new destinations);
`volume.TestMaintenanceMarker*`, `TestReadOnlyVolume*`; `cmd/musiclibd`:
`TestDoctor*` (schema refusal creating nothing, `/data` tree unchanged),
`TestBackupRestore*` (lock, DB, stable corruption codes),
`TestRebuildCommandRefusesWrongStoreIDWithoutWriting`,
`TestOfflineDoctorRepairAndRebuildConverge`,
`TestBackupLossRestoreBootAndDoctor` and the §12.3
`TestReleaseCollectionInterruptedAndRestored`. Mutants killed: deep output
hashing, published-state reset, backup source hash, restore archive hash,
marker creation, journal transient handling, missing-blob presence check,
host-path prefix boundary (N-214, N-218, N-224, N-228, N-233).

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
| `SourceOf(r) (MountSource, error)` / `MountSource.Contains` | a root's `major:minor` and path inside its filesystem from `/proc/self/mountinfo`, independent of the mount point; nesting at a component boundary (backup destination check, N-228) |
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
checks; hash validation; temp cleanup. Since round 10: a real SIGKILL at
each of the four points (`TestCrashDuringPut`: no corrupt pinned blob, the
temporary removed by `CleanTemps`, the re-put idempotent on the same inode)
and a really full filesystem (`TestPutOnAReallyFullFilesystem`, N-143).

```sh
scripts/check.sh ./internal/blobstore/...
scripts/dev.sh go test -race -count=20 ./internal/blobstore/
```

### `internal/failpoint` + `internal/faulttest` — named failpoints and real crashes (§12.2) ✔

One mechanism for every failpoint of the protocol (N-142). There is no
package-level hook, and nothing crash-related is linked into `musiclibd`.

| Function | Role |
|---|---|
| `failpoint.Hook`, `failpoint.Point{Name, Path, File}` | the hook a component holds (nil in production) and where it is |
| `Hook.Hit(name)` / `Hook.HitFile(name, path, f)` | run the hook at a named point; a nil hook returns nil |
| `faulttest.Crash(name)` / `CrashWhen(match)` / `Kill()` | a real SIGKILL of the process itself at the point |
| `faulttest.Switch` | a hook a test changes while the component lives (`Hook`, `Set`) |
| `faulttest.Mode`, `Exit`, `Start`, `Child.Wait`, `RunChild`, `Killed` | the child-process harness: the test binary's `TestHelperProcess` in a mode, its result line or `killed` |
| `faulttest.FullFS(t)` → `Disk{Dir}`, `Fill(leave)`, `Drain`, `Available`, `BlockSize` | the really full filesystem `/fullfs` (N-143): flock-serialized, refuses anything over 2 GiB or TMPDIR's filesystem, a fallocated ballast exact to the byte |

Where the hooks are: `render.Config.Failpoints`,
`importer.Config.Failpoints` and `publish.Config.Failpoints`; unexported
fields in `blobstore.Store`, `volume.Volume` and `jobs.Pool`; `claimNext`
for the claim. The catalogue of the points is in N-142.

Tests:
- a crash kills at exactly its point, by SIGKILL;
- `Wait` fails on any other end;
- `Switch`;
- `FullFS`: real ENOSPC past the level left, exact writes succeed, the
  space comes back, and an impossible level is an error;
- the three `FullFS` guards (a mount point, at most 2 GiB, not TMPDIR's), each on its own;
- `TestOnlyTestsImportFaulttest` (AST).

Crash tests in the packages, on the matrix above:
- `blobstore.TestCrashDuringPut`;
- `jobs.TestCrashAroundTheClaim`;
- `importer.TestCrashAtTheImportCommit`, `TestImportCommitAnswerLost`,
  `TestTwoProcessesImportTheSameCandidate`, `TestCrashAtTheScanCommit`;
- `publish.TestCrashMatrix`, `TestCrashDuringRecovery`,
  `TestCrashDuringBuild`, `TestExecuteRenderOnAReallyFullDisk`;
- `volume.TestFirstInitInterruptedAtEveryStep`;
- `cmd/musiclibd.TestProcessSuspendedByAnIllegalJournal`.

Mutation-checked, each making a test fail:
- the crash hook not killing;
- `CleanTemps` doing nothing;
- `RecoverRunning` doing nothing;
- each `FullFS` guard off (mount point, size, TMPDIR);
- the import commit's fingerprint lookup off (two processes: done and
  failed, not skipped);
- the album directory or `retired/<build_id>` not fsynced;
- the created artist directory not removed, or created after the rename;
- the boot exiting on an illegal journal;
- ENOSPC typed as `render_io`;
- `Publisher.CleanWork` doing nothing;
- `faulttest` imported by production code;
- the boot suspending on any step-4 error instead of `publish_illegal_state` only;
- the pre-rename fsync of a new artist directory dropped.

Timings (Docker Desktop, 12 CPUs): the gate `scripts/check.sh` takes
120 s. The crash and full-disk tests at `-race -count=5` take 376 s wall
time: publish 365 s, importer 45 s, jobs 22 s, cmd 18 s, volume 14 s,
blobstore 13 s, faulttest 12 s.

```sh
scripts/check.sh ./internal/faulttest/... ./internal/failpoint/...
scripts/dev.sh go test -race -count=5 -run 'TestCrash|ReallyFull' ./internal/blobstore/ ./internal/jobs/ ./internal/importer/ ./internal/publish/
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
and exits 0 or 1. The Phase 6 `doctor [--deep]`, `rebuild --store-id UUID`, `backup --to /backup/NAME` and `restore --from /backup/NAME` commands run offline. Unknown arguments are usage errors (exit 2).

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

| Step | What |
|---|---|
| 1 | HTTP serving with negative readiness; `volume.Acquire` |
| (N-065) | `CheckMaintenance`, before anything touches the database |
| 2 | pool of `WORKERS + 8`, `Ping` with backoff 250 ms → 5 s until cancelled; `store.Migrate` |
| 3 | `Identify`, `OpenLayout`, `CheckFilesystem`, `checkTools` (the Runner with `WORKERS` slots; pinned ffmpeg, ffprobe and musiclib-tags, `media_tool_unavailable` / `media_tool_version`), `/import` listable and not `/data` or one of its media directories (`import_is_data`, round 16, N-197) |
| 4 | the blob store, the process space budget, the builder and the publisher; `Publisher.Recover` (§9.4): the pending journal completed forward; `publish_illegal_state` suspends publishing and keeps the process up without steps 5 to 7, `publish_io` fails the boot (N-135) |
| 5 | `blobstore.CleanTemps`, `fsops.RemoveProbeLeftovers(work)`, `importer.CleanWork`, `Publisher.CleanWork` (`work/render`, `work/retired` not referenced by a journal); `jobs.RecoverRunning`; round 16: the catalog built, then `catalog.PurgeImportReports` (§6.4, 90 days; also once a day while the server runs, N-200) |
| 6 | `jobs.EnqueueStaleRenders(render.Version)` |
| 7 | the catalog (whose commits wake the pool), the importer on `/import`, `jobs.NewPool` with `WORKERS` workers dispatching scan, import and render; readiness positive; the API enabled over the catalog (round 11) |

While the server runs, an error that stops the pool (`store.IsFatal`: a
lost connection or an uncertain commit; or `jobs.Stop`: a publication left
pending) makes `run` return it, and the process exits 1 so that Docker
restarts it (§6.4, N-070, N-135).

- `/health/live` is always 200.
- `/health/ready` is 503 `not_ready` until step 7 and during shutdown. After
  that it pings PostgreSQL on each request with a 2 s timeout, and answers
  503 `db_unavailable` when the database is down (N-070). With a journal in
  an illegal state it is 503 `publish_illegal_state` with
  `details: {album_id, build_id}`, and the process stays up (N-135).
- Both endpoints are GET only, answer JSON with `Cache-Control: no-store`,
  and are outside the API's Host and Origin checks (N-145), so the
  healthcheck subcommand is unchanged.
- **The API (round 11, `internal/http`)** is mounted on `/api` and `/api/`
  from step 1. It answers 503 `not_ready` until step 7, then serves the
  catalog. Step 7 enables it and logs `ready` before `/health/ready`
  turns positive, so a positive readiness always means a serving API
  (N-320). It is disabled with `publish_illegal_state` while publishing
  is suspended (N-135), and with `shutting_down` first thing at
  shutdown, before the HTTP server drains the running requests (§11.1).
  A fatal database error met by an API request ends the run like one met
  by the pool: exit 1, and Docker restarts (§6.4, N-149).
- `nosniff` on every response; no CORS header ever; any other route is
  404.
- A refused boot logs its stable `code` and exits 1 (N-068).
- On SIGTERM or SIGINT the server cancels the workers (no claim, builds
  and their tools killed), stops accepting HTTP (graceful, 10 s), waits for
  every worker (a prepared publication has 30 s, N-140), closes the pool,
  then closes the volume's roots and releases the flock, last. It exits 0.

Tests use real PostgreSQL 17 and ext4, with no mocks:
- **Readiness lifecycle.** The database refuses connections at first: live is
  200, ready is `not_ready`, the lock is held and nothing is written. Once the
  database is allowed, ready turns 200.
- **Database loss (§6.4).** With the workers running, the database goes
  away: the next poll fails fatally, the workers stop, `run` returns
  `store_connection_lost` with the lock released; a restart boots again.
- **Shutdown order.** The shutdown events come in order, then the lock is
  free and nothing answers any more.
- **Stop during the wait.** Stopping while waiting for PostgreSQL is a clean
  stop that writes nothing.
- **Restart.** Restarting on the same pair keeps the store id.
- **Refusals.** Maintenance (refused with an unreachable database),
  malformed maintenance, mismatch, new database, lock held, missing `/data`
  and missing `/import`: each gives its code, never turns ready, and releases
  the lock.
- **Step 5 cleanup.** Temporaries, probe directories, `work/import`
  content and unreferenced builds and retired directories are removed;
  nothing else.
- **Steps 4 to 7 in order** (`TestBootStepsInOrder`): a pending journal is
  completed first and its FINALIZE completes its running job, then the
  cleanup and `running → pending` (no job left running), then exactly one
  stale render (the album without a job; a failed job and an album with
  the current renderer are left alone), then the workers, then `ready`.
- **An illegal journal** (N-135, round 10): `TestBootSuspendsOnAnIllegalJournal`
  and, with a real process, `TestProcessSuspendedByAnIllegalJournal`: the
  process stays up with the lock held and no worker, `/health/ready`
  answers 503 `publish_illegal_state` with the album and build ids, the
  healthcheck exits 1, the logs say what to do, nothing moves, the journal
  and the running job stay; SIGTERM exits 0 and releases the lock.
- **A recovery I/O error** (`TestBootFailsOnARecoveryIOError`): a legal
  journal whose staging cannot be moved (EACCES) fails the boot with
  `publish_io` (exit 1): no suspension, no worker, the lock released, the
  journal kept, the created artist directory removed again.
- **End to end, two workers (§13.1)** (`TestEndToEndTwoWorkers`): two FLAC
  albums in `/import`, a batch, then the server scans, imports, renders
  and publishes; the output matches §1.1 with the managed tags read back
  and a receipt whose hash is the album's; an artist rename moves the album,
  retires the old directory and releases its claim; trash removes it,
  restore brings it back; `originals/` unchanged after the import and
  `/import` never changed; `work/render` and `work/retired` empty.
- **Real processes with helpers running** (`TestProcessDatabaseLossMidWork`,
  `TestProcessSignalsWithHelpersActive`): two workers with ffmpeg /
  musiclib-tags children observed in `/proc`; losing the database gives a
  non-zero exit; SIGTERM gives exit 0 with the shutdown order; SIGKILL is
  covered by `Pdeathsig`. In every case none of the observed tools
  survives, and a restart finishes the albums with no failed job. After
  SIGTERM the lock is free at once; after SIGKILL it must be free within
  5 s, since a tool child between clone and execve may still hold the
  lock's descriptor for a few scheduler ticks (round 16, N-202).
- **The API on the real server (round 11):**
  - `/api` is 503 `not_ready` during the boot, then serves;
  - the loopback Host that the health endpoints accept is 421 at `/api`;
  - while publishing is suspended, every `/api` request is 503
    `publish_illegal_state`;
  - on a database loss the API answers 503 with no database text, and
    whichever of the API and the pool meets the loss first ends the run;
  - a COMMIT answer lost under an API request
    (`TestAPIFatalErrorStopsTheProcess`) ends the run with
    `store_commit_uncertain`, the change durable; a lock on `jobs` keeps
    the workers' idle polls off the lost answer, so the API path is the
    one tested on every run (N-320);
  - `TestEndToEndTwoWorkers` makes its changes through the API: the
    artist rename (428 first), trash, restore and a forced render, each
    with the ETag just read. It checks the status after the rename, and
    a republication with a new build and the same revision.
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
database, readiness never positive, redirects followed, root allowed, a
`WORKERS` default of 2, (round 9) `run` ignoring the pool's fatal error,
and (round 11) the API never enabled and `run` ignoring the API's fatal
error. Each makes a test fail.

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
| `Tools.AudioDigest(ctx, f)` | §8.4: probe, full decode to `pcm_f64le`, streamed SHA-256, frames = bytes / (8 × channels), declared length enforced; a FLAC's trailing ID3v1 left out of the decode (N-128); returns `(SampleRate, Channels, Layout, Frames, PCMSHA256)` |
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
  - For a FLAC with a trailing ID3v1 tag (TagLib's rule, the helper's, pinned
    by a cross-check), the decoder reads only the bytes before the tag,
    through a pipe that carries exactly those bytes, with the same command.
    The render's §9.1 step 6 comparison then needs no knowledge of ID3
    (N-128, owner decision).
- **§11.1 step 3:** the boot builds the Runner with `WORKERS` slots and runs
  `NewTools`. A missing or different tool is a fatal boot error with its
  code.

**Cost (N-079):** `AudioDigest` of a 5-minute 44.1 kHz stereo FLAC takes 0.56
s in the dev container. With a trailing ID3v1, read through the pipe of N-128,
the time is the same within noise. ALAC, AAC and MP3 decode in 0.55 to 0.61 s.

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
- **Trailing ID3v1 (N-128):**
  - the Go rule agrees with the release and the ASan helper on 14 files
    (APEv2 identifiers, near misses, stacked tags, appended ID3v2, audio
    that spells "TAG", a tag inside the metadata);
  - the digest with the tag equals the digest without it, with and without
    a declared length, and with a leading ID3v2;
  - a fake ffmpeg receives exactly the bytes before the tag;
  - audio cut short by 1 to 1000 bytes before an ID3v1, a false "TAG" match,
    two ID3v1, an `APETAGEX` and an appended ID3v2 are refused;
  - the feeder: exact lengths, short and unreadable inputs, a decoder that
    does not read, a cancellation.
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
- for the trailing ID3v1: no byte limit; the extent one byte short or long;
  a divergent detector (no `APETAGEX` exclusion, "TAG" at 129 bytes); the
  feeder one byte short; a decoder that does not read accepted;
- in the probe: an estimated MP3 length taken as exact; no encryption,
  video, multistream or other-stream check; AAC accepted outside MP4; any
  failing exit taken as unreadable; EIO taken as unreadable; the undeclared
  layout not marked; no fd-only whitelist;
- in the tools and the boot: no version check; the boot skipping the tools.

```sh
scripts/check.sh ./internal/media/...
scripts/dev.sh go test -race -count=20 -run 'TestRun|TestNewTools|TestProbeToolFailures|TestAudioDigestDecoderFailures' ./internal/media/
scripts/dev.sh go test -race -count=10 -run 'ID3v1|FeedPrefix|LimitedInput|FeedsExactly' ./internal/media/
scripts/dev.sh go test -run '^$' -bench AudioDigest5Min -benchtime 10x ./internal/media/
```

### `native/musiclib-tags` + the tag adapter (§2.1, §8.1–§8.3, §8.5, §9.1 step 6, §12.2) — FLAC ✔, MP3 ✔ (round 12), M4A ✔ (round 13; the next two sections)

The TagLib helper and its Go adapter in `internal/media`. **FLAC is
complete**, **MP3 since round 12** and **M4A since round 13** (the next two
sections).

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
| `src/version.h` | `kHelperVersion` = `3` (`1` refused ID3 tags in a FLAC; `2` strips them, N-090; `3` adds MP3, N-152) |
| `tests/unit_tests.cpp` | the parsers under ASan and UBSan (`make check`, run by the build) |

**Pinned TagLib (N-083).**
- TagLib 2.3.2 and CMake 4.4.3 are pinned by sha256 and built in the
  Dockerfile stage `build-tags`, static, with no zlib.
- The helper is fully static (3 MB) and has the same bytes in the `test`,
  `dev` and `runtime` images. Two `--no-cache` builds gave the same sha256.
- A second, ASan and UBSan, build of TagLib and the helper is in the
  toolchain images only.
- The version is `{"helper":"3","taglib":"2.3.2-musiclib1"}`, checked at
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
  - M4A refused, a FLAC declared as MP3 and an MP3 declared as FLAC refused;
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
  ID3v1 is imported too since round 7: the full decode reads only the bytes
  before the tag (N-128).

### MP3 end to end: helper, media, importer, render, publish (§7.2–§7.4, §8.1–§8.5, §9.1, §12.1) ✔ (round 12)

An MP3 album is imported, catalogued, rendered and published like a FLAC
one. (M4A followed in round 13, next section.)

**The helper, version 3** (N-152 to N-159). TagLib is only a cross-check:

| Source | Role |
|---|---|
| `src/id3v2.{h,cpp}` | ID3v2.2/2.3/2.4 reader: header rules shared with TagLib and FFmpeg, whole-tag and per-frame unsynchronisation, extended headers, frame flags in their ID3v2.4 form, the ID3v2.2 identifier map, TagLib's iTunes size rules; text decoding (Latin-1, UTF-16 with byte order marks, UTF-16BE, UTF-8); the ID3v2.4 frame and tag writer |
| `src/ape.{h,cpp}` | APEv1/APEv2 footer, header and items; the writer that keeps items byte for byte |
| `src/mp3.{h,cpp}` | where the tags are (TagLib's rules), the MPEG layer III check, the §8.1 reading with conflicts, the canonical unmanaged form, pictures (APIC, PIC, APE covers), the write (managed frames, kept frames, ID3v1 migration, cover, padding, APE cleanup, ID3v1 removal) and its read-backs by the helper and by TagLib |
| `src/fields.h` | the MP3 table: §8.2 frames, alias frames, TXXX aliases, old date frames, sort frames, APE keys |
| `src/fdio.cpp`, `failure.h` | ENOSPC and EDQUOT are the failure code `no_space` (N-143) |
| `src/inspection.*` | the `audio` range `{start, end}` of every inspection; the new opaque reasons |
| `tests/unit_tests_mp3.inc` | the ID3v2 and APE parsers and writers under ASan/UBSan at build time |

**Go (`internal/media`):**
- `Inspection.Audio`; `CodeTagsNoSpace`; `MaxEmbeddedCover(mp3, ...)`
  (N-155); `PinnedTagsVersion` 3;
- `VerifyTags` with the declared ID3v1 migration and nothing else (N-153);
- `AudioDigest` of an MP3 reads only the audio before its trailing ID3v1 and
  APE tags, through FFmpeg's `subfile` protocol over descriptor 3, and takes
  the declared length from a probe of the same window (`mp3AudioEnd`,
  `windowed`, `probeWindow`, N-154).

**Importer:** MP3 candidates, and FLAC and MP3 in one candidate (N-157); the
tags → §7.3 metadata, the APIC and APE covers (§7.4), N-092 on decoded text;
the helper's `no_space` while extracting a picture is `insufficient_space`.

**Render:** MP3 tracks planned as `NN - Title.mp3` and built through the
same §9.1 step 6; the helper's `no_space` while writing tags is
`insufficient_space`; `RendererRevision` 2 (N-158).

**Tests** (real LAME, ffmpeg, both helpers, PostgreSQL 17, ext4, the full
tmpfs; fixtures by independent codecs, N-156):
- **media, contract:** no tags (managed frames and cover exactly, padding,
  idempotence, determinism over copies, the genre and total refusals);
  rich ID3v2.3 and ID3v2.4 tags with every kind of frame (every unmanaged
  frame kept byte for byte, every alias, old date and sort frame gone);
  §8.1 precedence across ID3v2, APE and ID3v1 with conflicts; APE with and
  without header (unmanaged items kept byte for byte, an all-managed tag
  removed); the ID3v1 migration (7 cases and the conflict); text in every
  encoding, astral and NFD, invalid text managed and unmanaged; three kinds
  of unsynchronisation; flagged frames (compressed, encrypted and grouped,
  read-only) and ID3v2.2; genres; pictures (four, extracted byte for byte);
  LAME's own tags; padding;
- **media, hostile** (both helpers, 29 cases, the file unchanged on every
  refusal) and the 200-seed mutation sweep on the ASan helper;
- **media, digest:** 10 tag layouts × VBR and CBR equal the bare audio's
  digest with the source's exact frame count, before and after a write; the
  window equals the helper's range; five refusals; the full disk (`no_space`
  with the kernel's message);
- **media, `VerifyTags`:** 12 migration cases; the MP3 cover limit (a sparse
  2^28-byte cover refused, a 17 MiB one embedded);
- **importer:** an MP3 album (UTF-16 and Latin-1 text, a genre reference,
  TYER, the embedded cover); precedence and its warning; FLAC and MP3 mixed;
  invalid text, a cut and a padded MP3 refused;
- **render:** an MP3 + FLAC album (planned tags, unmanaged frames, APE items,
  the migrated comment, no ID3v1, same audio, deterministic); a blocking
  field fails the build; the MP3 plan;
- **publish:** `TestExecuteRenderMP3Album` (LAME's tags plus an APE tag,
  imported, built and published by two workers: managed tags, unmanaged
  fields, same samples as `/import`); `TestExecuteRenderMP3OnAReallyFullDisk`.

**Owner decisions of 2026-09-25** (after the review):
- N-161: TORY (original release year) is kept, not removed as a date alias;
- N-162: a genre an MP3 cannot hold ("(Rock)", "13") is refused by
  `PUT /api/albums/{id}` with 422 `genre_not_writable` when the album has an
  MP3 track (`catalog.GenreFits`, `media.MP3GenreWritable`, pinned to the
  helper), and warned about at import (`genre_not_writable`);
- N-163: the APE tag is bounded at 256 MiB (`corrupt`, and refused by the
  decode window);
- N-164: an ID3v1 title, artist or album that is the 30-character Latin-1
  truncation of the winning value is not a conflict.

**Mutation-checked** (N-160): precedence, the removal tables, the migration
(absent and duplicated), the APE items, the refusal of blocking fields,
`VerifyTags` (three ways), the decode window, its probe, the APE header in
the window, the renderer's `no_space` mapping, the importer's MP3 acceptance.

```sh
scripts/check.sh
scripts/dev.sh go test -race -count=3 -run 'MP3|Mp3|TestVerifyTags|TestMaxEmbeddedCover|TestWindowed' \
  ./internal/media/ ./internal/importer/ ./internal/render/ ./internal/publish/
docker build --target build-tags .           # the helper and its unit tests alone
```

### M4A end to end: helper, media, importer, render, publish (§7.2–§7.4, §8.1–§8.5, §9.1, §12.1) ✔ (round 13)

An M4A album, AAC or ALAC, is imported, catalogued, rendered and published
like a FLAC or MP3 one, and one candidate may mix the three formats (N-157).
No audio format of §8.1 is refused any more: `audio_format_not_supported_yet`
and `render_format_not_supported_yet` are gone.

**The helper, version 4** (N-165 to N-168). TagLib is only a cross-check:

| Source | Role |
|---|---|
| `src/mp4.{h,cpp}` | ISO BMFF primitives without TagLib: box headers (32/64-bit, size 0 at the top level only), children that must tile their container (at most 50,000), data atoms, freeform mean/name, UTF-16BE, the sample table (`chunks` from stsc/stsz/stco/co64, strictly checked), the chunk-offset fix-up (`patchOffsets`, 32-bit overflow refused) |
| `src/m4a.{h,cpp}` | the bounded reader (top-level boxes, moov ≤ 256 MiB, the one `soun` track, the refusals of §8.1: fragments, DRM, other tracks, external data, `stz2`), the ilst analysis with §8.1 precedence and conflicts, the canonical unmanaged form, the samples hash, the write (managed atoms, kept items byte for byte, the cover in place, padding, offsets) and its read-backs by the helper and by TagLib |
| `src/fields.h` | the M4A table: §8.2 atoms, freeform aliases, `gnre`, sort atoms and freeform sort names, `covr` |
| `src/inspection.h` | the new opaque reason `unsupported_data` |
| `tests/unit_tests_mp4.inc` | the primitives under ASan/UBSan at build time |

**Go (`internal/media`):** `PinnedTagsVersion` 4; `MaxEmbeddedCover(m4a-*, ...)`
268,435,432 bytes (N-166); `VerifyTags` unchanged, documented with no extra
exclusion for M4A (N-167); `AudioDigest` reads an M4A whole (no window,
N-168).

**Importer:** M4A candidates, alone or mixed with FLAC and MP3; the iTunes
atoms → §7.3 metadata; `covr` images are front covers for §7.4; an M4A the
tag reader refuses (fragmented, encrypted, several tracks) is
`unsupported_audio` with the helper's reason; a metadata structure the writer
cannot keep is `unrenderable_tag` (N-092). The genre warning of N-162 stays
MP3 only: every genre fits an M4A.

**Render:** M4A tracks planned as `NN - Title.m4a` and built through the same
§9.1 step 6; the helper's `no_space` is `insufficient_space` as for MP3;
`RendererRevision` 3 (N-169).

**Tests** (real FFmpeg AAC and ALAC, both helpers, PostgreSQL 17, ext4, the
full tmpfs; metadata by the independent codec `mp4meta_test.go` and by
FFmpeg's muxer, N-170):
- **media, contract:** no metadata (AAC and ALAC, moov before and after the
  media: exact items written, cover, idempotence, determinism over copies,
  removal of everything); rich tags (managed, conflicting and agreeing
  aliases, `gnre`, sort atoms, 20 unmanaged items of every kind — freeform
  of several means, integers, a locale, UTF-16, invalid UTF-8, unparsable
  children, a 64-bit item — kept byte for byte and in order, the cover where
  the stream-creating covr was relative to iTunSMPB); §8.1 precedence (6 cases); unreadable managed atoms
  (9 reasons, removed, the write succeeds); Unicode byte-exact and genres
  like "(Rock)"; layouts (room kept, exact room, growth before the media in
  stco and co64, no udta, growth after the media, shrink past the padding
  bound, a QuickTime meta, extra and free boxes), each checked by an
  independent reading of every box, chunk offset and sample, and written
  twice; pictures (JPEG, PNG, untyped, BMP; extracted byte for byte);
  invalid requests (numbers above 32,767, a GIF cover); the cover limit;
- **media, hostile** (both helpers, 38 cases, the file unchanged on every
  refusal: damaged structures, other formats and codecs, fragments, two
  tracks, video, CENC, FairPlay, `sinf`, `pssh`, external data, `stz2`, the
  blocking metadata structures), the moov bound on a sparse 300 MiB file,
  and the 200-seed mutation sweep on the ASan helper;
- **media, digest:** gapless AAC with an edit list, `iTunSMPB`, both and
  neither, and ALAC, the same digest and frame count after a growth and a
  shrink; the FFmpeg cover/`iTunSMPB` behaviour pinned including GIF and
  implicit covers before iTunSMPB and separated covr items (N-168); the full
  disk (`no_space` with the kernel's message);
- **media, `VerifyTags`:** 14 M4A differences refused, the MP3 migration not
  applied;
- **importer:** an AAC + ALAC album (FFmpeg's tags, the covr cover, "(Rock)"
  without a warning); FLAC + MP3 + M4A; a `gnre` conflict warned; a foreign
  meta refused; a cut file corrupt; a fragmented M4A unsupported;
- **render:** an AAC + ALAC + MP3 + FLAC album (planned tags, unmanaged items,
  the cover, same audio, deterministic); a blocking structure fails the
  build; the M4A plan;
- **http:** an album of M4A tracks saves "(Rock)", one with an MP3 track
  refuses it (N-162 unchanged);
- **publish:** `TestExecuteRenderM4AAlbum` (FFmpeg-tagged AAC and ALAC, one
  with moov first so that the media moves, imported, built and published by
  two workers: managed tags, every unmanaged item and box of the original,
  same samples); `TestExecuteRenderM4AOnAReallyFullDisk` (ENOSPC reaches the
  helper's own write).

**Mutation-checked** (N-171): canonical over aliases; sort atoms, `gnre` and
freeform aliases removed; kept items; the cover's place; the chunk-offset
fix-up (with and without the read-back); the refusal of blocking fields;
two tracks, `enca`, `sinf`; `VerifyTags` for M4A; the importer's
`unsupported_audio`; the renderer's `no_space`.

```sh
scripts/check.sh
scripts/dev.sh go test -race -count=3 -run 'M4A|TestVerifyTags|TestMaxEmbeddedCover|TestUpdateAlbumGenre' \
  ./internal/media/ ./internal/importer/ ./internal/render/ ./internal/publish/ ./internal/http/
docker build --target build-tags .           # the helper and its unit tests alone
```

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
  a lost database stopping every worker, an error marked with `Stop`
  stopping it and the same error unmarked not (`TestPoolStop`,
  mutation-checked);
- the closed types, strict both ways;
- the space budget (§11.2, N-139): `NewBudget`, `Reserve(free, estimate)`
  against free minus `SpaceMargin` minus what is reserved, `Release`
  idempotent; 64 goroutines racing for 100 MiB never overspend, every
  fitting reservation granted, 50 rounds (mutation-checked).

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
| `PathCollision(paths)` | §5.2's collision rule on final paths: one file key twice, a file where another needs a directory, one directory spelled two ways (owner decision, N-131); used by the import commit and the render planner |
| `CheckFresh(ctx, *CatalogTx, snapshot, renderer)` | the four conditions of §6.3 for PREPARE (N-097, N-112) |
| `CreateArtist(ctx, name)` | round 11, `POST /api/artists`: revision 1, or `artist_exists` / `artist_folder_conflict` returned **with** the existing artist, read in the same transaction |
| `RequestRender(ctx, id, ifMatch)` | round 11, the forced render of §10.2: If-Match compared in the transaction, `jobs.EnqueueRender`, no bump |
| `ListArtists`, `GetArtist`, `GetAlbum`, `GetAlbumStatus` | round 11: the API's reads, each in one REPEATABLE READ snapshot (`store.InSnapshotTx`); every artist, those without albums included (owner decision N-146) |
| `JobMessage(err, message)` | round 11: the message stored with a failed job, `DatabaseJobMessage` when err holds a database error anywhere (§10.1, N-150); used by the importer and the render executor |
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
- `CheckFresh`, each condition alone; fatal codes winning over domain codes;
- (round 11) `CreateArtist`: NFC and trim, casefold identity (`STRÁUSS`
  against `Stráuß`), a sanitization conflict, invalid names, nothing
  enqueued; eight concurrent creations give one artist; `ListArtists`
  (all artists, the one without albums and the trash-only one included, byte order; N-146); `GetAlbum` and `GetAlbumStatus` field
  by field; `RequestRender` (428, 412, 404, no bump, coalescing with a
  running attempt, a trashed album); `JobMessage` on a real PgError,
  wrapped, nested and joined.

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

### `internal/importer` — scan and import of one candidate (§7.1–§7.6, §8.5, §11.2) ✔ (FLAC; MP3 since round 12; M4A since round 13; multi-disc since round 15)

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
  - Rules 1 to 5 apply; the multi-disc rules 2 and 3 are detailed in the
    round-15 part below.
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
  - FLAC, MP3 (round 12) and M4A (round 13) are tracks; the code
    `audio_format_not_supported_yet` of the first rounds is gone.
  - Other audio is `unsupported_audio`.
  - No audio with a known audio extension is `corrupt_audio`; without one,
    it is an attachment.
  - Audio in a subdirectory is `ambiguous_candidate`; in a multi-disc
    candidate, audio anywhere but directly in a disc directory is.
- **§7.6, §8.4:** `AudioDigest` decodes every track completely. A failed
  decode is `corrupt_audio`.
- **N-092, N-090:** a field from `Inspection.Blocking()` is
  `unrenderable_tag`, naming the file and the field. An ID3 tag in a FLAC,
  which the helper reports as removed, is accepted with the warning
  `flac_id3_tag`; a render strips it. The full decode of a FLAC with a
  trailing ID3v1 reads only the bytes before the tag (N-128). A file still
  refused as `corrupt_audio` gets a message that names the failed check.
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
| `ambiguous_candidate` | rule 4; audio below a disc directory of a rule-2 shaped directory (N-184) |
| `duplicate_disc` | rule 3: two disc directories with the same number |
| `not_a_candidate` | no direct audio any more |
| `no_valid_candidate` | a scan with nothing to import |
| `insufficient_space` | §11.2 |
| `corrupt_audio`, `unsupported_audio` | §7.2, §8.1 |
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
- **Owner decisions:** N-092 refused; N-090 accepted, a trailing ID3v1
  included (N-128); an ID3v2 appended at the end refused by the decode, with
  a message that says so.
- **Formats:** a corrupt FLAC, text named `.mp3`, M4A, WAV, and a FLAC
  without an extension.
- **Layouts:** an ambiguous branch next to a good one, with the
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

**Round 15: multi-disc candidates (§7.2 rules 2 and 3, §7.3, §7.4, §7.6).**
The placeholder `multidisc_not_supported_yet` is gone.

| Piece | Role |
|---|---|
| `group` / `discShaped` / `multiDisc` | rule 2: a directory without direct audio whose audio-holding children are all `CD<N>` or `Disc <N>`, at least one of them with direct audio, is one candidate (N-184 (b′)); rule 3's checks (N-183) |
| `discNumber` (`isDiscName`) | the name rule of N-120, kept: ASCII-only folding, leading zeros, N > 0; a huge N stays over 99, never wraps |
| `branch.Discs` | the disc directories and their numbers, given to the import by `revalidate` |
| `splitTracks` | on the copies: a track must be directly in a disc directory; its number is the track's disc |
| `trackTags.Disc`, `discTagWarnings` | §7.3: the directory wins over the disc tag, with `disc_tag_ignored` (N-185, kept by the owner) |
| `numberTracks`, `renumberedPath` | per-disc numbering; `tracks_renumbered` carries the disc directory as its path in a multi-disc album, none otherwise |
| `duplicate_disc`, `jobs.WarnDiscTagIgnored` | the new code and warning |

**How it maps to DESIGN.md:**
- **§7.2 rule 2, the shape (N-184, owner decision (b′), 2026-09-26).** A
  directory without direct audio is rule-2 shaped when every child holding
  audio has a disc name and at least one disc-named child has audio
  directly in it. Otherwise rule 5 applies as in Phase 2: a data-CD backup
  `Rips/CD1/Artist - Album/*.mp3` + `Rips/CD2/Other/*.mp3` gives two
  independent albums, and `Box/CD1` + `Box/CD2` + `Box/Bonus` (a non-disc
  sibling with audio) gives three.
- **Disc directories.** In a rule-2 shaped directory every direct child
  with a disc name is a disc directory, audio or not (N-183, confirmed by
  the owner). One without audio contributes only attachments, but counts
  for the checks: `CD1` next to `CD01/scan.jpg` is `duplicate_disc`,
  `CD100/scans` is `invalid_disc`.
- **§7.2 rule 3, in this order, each failing the branch as a whole with one
  pre-failed import job naming the paths:**
  - audio below a disc directory is `ambiguous_candidate`, naming the
    misplaced file (`Box/CD1/1.flac` + `Box/CD2/Bonus/x.flac`); no partial
    import;
  - two disc directories with one number (`CD1`/`CD01`, `CD1`/`Disc 1`)
    are `duplicate_disc`;
  - a disc over 99 is `invalid_disc`;
  - then the rejected entries and the limits, counted over every disc.
- **§7.2 attachments.** Root files, siblings without audio, and non-audio
  files inside the discs keep their paths (`Extras/CD1/Scans/x.jpg`).
- **§7.2 rule 4 at the import.** Direct audio next to disc directories, or
  audio found by content outside them, is `ambiguous_candidate`.
- **§7.1 and the retry.** The revalidation and the stability snapshot walk
  the whole subtree, disc directories included.
- **§7.3.** The disc comes from the directory. Numbering is per disc: the
  tags, or that disc alone renumbered by the natural order with
  `tracks_renumbered`. The title falls back to the candidate's root.
  `mixed_album` and the album artist apply across every disc.
- **§7.4.** The LRC association stays per directory. The external cover
  comes only from the candidate's root (N-186), so a disc-level
  `cover.jpg` is an attachment only.
- **§7.6.** The fingerprint is unchanged: its paths already carry the disc
  directories (N-187).
- **§5.1.** The planner's `Disc <D>/` layout, the LRC next to its track,
  `cover.jpg` at the root and `Extras/CD1/...` are verified end to end by
  `publish.TestExecuteRenderMultiDiscAlbum`.

**Tests:**
- **Pure layout tables** (`TestGroupMultiDisc`, 34 layouts):
  - `CD1`/`CD2`, `Disc 1`/`Disc 2`, mixed case, leading zeros,
    non-contiguous discs, a single `CD1`, disc 99, the batch root as the
    album;
  - the duplicates `CD1`/`CD01`, `CD1`/`Disc 1`, case-only, and one
    without audio;
  - `CD100` with audio and `CD100/scans` without, and a number too large
    for an int;
  - `CD0`, `CD 1` and `Disc1` as ordinary directories;
  - `Box/CD1` next to `Box/Bonus`, and `CD1` + `CD2` + `Bonus` as three
    albums;
  - siblings and disc directories without audio;
  - audio below a disc directory (example A), in and below one, and one
    disc with direct audio next to one with only nested audio: ambiguous;
  - no disc directory with direct audio (a lone `CD1/sub`, the data-CD
    backup of example B, `CD1/A` + `CD01/B`, `CD100/A`): rule 5, no
    duplicate or over-99 check;
  - direct audio plus discs;
  - nested albums next to single-disc ones;
  - rejected entries inside and outside, unassigned files.
- **Other pure tests:**
  - `TestGroupMultiDiscLimits`: the exact limits over several discs;
  - `TestDiscNumber`: separators and `İ`;
  - `TestInferMetadata`: six disc-directory rows;
  - `TestRenumberedWarningPath`: `tracks_renumbered` names the disc
    directory of the renumbered disc, and has no path without one.
- **End to end:**
  - `TestImportMultiDiscAlbum`: disc tags overridden with the warning,
    per-disc numbering with and without usable tags, the LRC per
    directory, the root cover over `CD1/cover.jpg`, attachments in the
    discs, the ignored files, the fingerprint;
  - `TestImportMultiDiscMixedFormats`: FLAC and MP3;
  - `TestImportMultiDiscCoverOnlyFromTheRoot`;
  - `TestScanMultiDiscLayouts` on disk, including example B's data-CD
    backup and `CD1` + `CD2` + `Bonus`;
  - `TestImportMultiDiscRevalidates`: a duplicate added, audio added
    below a disc, all the audio gone, a FLAC named `.jpg` at the root,
    and a single-disc album that became multi-disc;
  - `TestImportMultiDiscRefusesAChangedSource`: four changes inside the
    discs;
  - `TestMultiDiscFingerprint`: two discs and one disc of the same files,
    then a skipped re-import;
  - `TestScanMultiDiscFileLimit`: 10,001 real files over three
    directories;
  - `TestCrashAtTheScanCommitMultiDisc`: SIGKILL at both scan commit
    points;
  - `publish.TestExecuteRenderMultiDiscAlbum`: the published tree and the
    disc tags.

**Mutation-checked (N-189):**
- the duplicate check, the over-99 check, audio below a disc falling to
  rule 5 (the deep-audio check off), `discShaped` ignoring whether any disc
  has direct audio, and the limits skipped for multi-disc branches;
- `tracks_renumbered` without the disc directory's path;
- `Disc` without its space, leading zeros counted, Unicode lowercasing;
- the tag over the directory, the warning off, numbering over the whole
  album;
- the title and the cover from a disc directory;
- root audio accepted, and the directory's disc not given to the track.


```sh
scripts/check.sh ./internal/importer/...
scripts/dev.sh go test -race -count=10 ./internal/importer/
```

### `internal/render` — `render_version`, pure plan, build in staging, receipt (§2.1, §5, §6.2, §8.2, §9.1, §9.2, §11.2) ✔ (FLAC; MP3 since round 12; M4A since round 13)

From the claim's snapshot to a complete, verified album directory in
`work/render/<build_id>/album`, ready for the publisher (§9.3, next round).
The planner is pure; the builder reads originals through the blob store and
writes only under `work/` through fsops, and has no root for `library/`.

| Function | Role |
|---|---|
| `Version`, `RendererRevision`, `GoVersion` | `render_version` (§2.1), a compile-time constant of six tokens: renderer revision, `names.AlgorithmVersion`, Go, ffmpeg/ffprobe, helper, TagLib (N-130) |
| `CheckTools(media.Versions)` | the tools verified at boot and the toolchain are the ones `Version` names; called by `cmd/musiclibd` at step 3 |
| `NewPlan(snapshot, renderVersion)` | the pure plan (§6.2, §9.1 steps 2–3): `Dir` (`catalog.AlbumPath`), cover, tracks with their expected `media.TagValues`, LRC and attachment copies; every collision and limit checked first; a trashed album is a removal plan |
| `Plan.Files()` | the files the receipt lists, sorted by bytes |
| `New(Config)` / `Builder.Build(ctx, Plan)` | the build of §9.1 steps 4–8; returns `Result{BuildID, AlbumID, AlbumRevision, RenderVersion, Removal, Staging, ReceiptHash}` |
| `Builder.Discard(ctx, buildID)` | removes a build's directory: on every failure, and for the publisher |
| `StagingDir(buildID)` | `render/<build_id>/album`, relative to `/data/work` |
| `Receipt`, `Receipt.Encode`, `ParseReceipt`, `ReceiptHash` | `.musiclib.json` (§9.2): one canonical form, a strict parser for §9.4 and §11.3 (N-133) |
| `Error` / `Code` | `render_*` codes (below); media, blob store, fsops and names codes pass through |

Also: `names.AlgorithmVersion` (the frozen algorithm's identifier, pinned by
`TestAlgorithmVersionPinned`), `media.CoverMIME`, and the boot's
`render_version` log field and `codeOf` case.

**How it maps to DESIGN.md:**
- **§2.1 `render_version`:** derived, never written by hand; each input
  either changes it by construction or has a pinned-digest test that fails
  until its revision is bumped; the tool binaries' sha256 are pinned in the
  gate (N-130).
- **§5.1 layout:** `<NN> - <title>.flac` (`%02d`), `Disc <D>/` when more
  than one disc or a disc other than 1, `cover.jpg`/`cover.png` from the
  blob format, LRC with the track's basename, attachments under `Extras/`.
- **§5.2:** every name through `names` (`Segment`, `FileSegment`,
  `SanitizeRelFilePath`, `PathKey`); collisions after normalization (file
  against file, file against directory, one directory spelled two ways, by
  `catalog.PathCollision`, the rule the import commit uses too) are
  `render_path_collision` naming both entries; no suffix, no deduplication
  (N-131). The path limits are one rule, `checkOutputPath`, shared by the
  planner and the receipt: below `Extras/` for an attachment.
- **§8.2 tags:** title; the track's artist override or the album artist;
  album artist; album; track and track total (highest on the disc); disc and
  disc total (highest in the album); year as four digits; the track's genre,
  `""` for none, or the album's; compilation; the album cover or none.
- **§9.1 steps 5–8:** every copy hashed while read and compared with the
  blob name (`corrupt_blob`), then read back; per track digest, inspect,
  write, inspect, `VerifyTags`, digest, digests equal; final sizes and
  hashes; the receipt; fsync of every file, then every new directory
  bottom-up up to `work/` (N-132).
- **§11.2:** estimate and `statfs` with 1 GiB before anything is created;
  ENOSPC anywhere is `insufficient_space`. The process budget is N-114's.

**Codes:** `render_invalid_snapshot` (`render_format_not_supported_yet`, for
MP3 and M4A until Phase 4, is gone since round 13), `render_path_collision`, `render_path_invalid`,
`render_version_mismatch`, `render_copy_mismatch`, `render_audio_changed`,
`insufficient_space`, `render_io`, `render_canceled`,
`render_receipt_invalid`, `render_invalid_argument`.

**Tests** (real pinned ffmpeg, ffprobe and helper; real ext4; real
PostgreSQL 17 for the catalog test; FLAC fixtures generated at test time by
ffmpeg with metadata written by the tests' own codec):
- **Planner (pure, table-driven):** the §1.1 example; single disc, multi-disc,
  a single disc numbered 2, disc 10; every number 1..999 (`09`, `100`); JPEG
  and PNG cover; LRC next to its track and in its disc; sanitized and NFC
  titles; unordered snapshots; nested, Unicode, NFD, hidden, DOS-named
  attachments and attachments named like tracks, the cover or the receipt;
  nine collisions (case, sanitization, casefold, file/directory both ways,
  directories differing in case at two depths, track/track after
  sanitization and by case); components over 180 bytes
  truncated with the LRC following; paths over 1,024 bytes or 16 levels,
  absolute and `..` refused, exactly at the limits accepted; inherited and
  overridden artist and genre, the empty genre, totals with gaps, years 1,
  999, 1959, 9999, compilation; the removal plan; 15 invalid snapshots;
  purity (same input, same plan; no aliasing; plan.go's imports).
- **Receipt:** golden bytes (HTML characters and U+2028 raw), escaping, 200
  random round trips, 44 hostile inputs, `Encode` refusing 12 invalid
  receipts, the UTF-8 byte order against UTF-16 order, and the path limits
  at the boundary (the maximum accepted, one level or byte more refused,
  under `Extras/` and elsewhere).
- **`render_version`:** its tokens and the source expression, the golden
  value, the Go toolchain, `CheckTools` with each version changed, the three
  binaries' sha256, the renderer's pinned output digest.
- **Builder:**
  - a normal album with cover, LRC, nested Unicode attachments, old tags,
    aliases, sort keys, unmanaged fields and pictures: exactly the planned
    files, modes, the receipt against the files, copies byte for byte,
    `VerifyTags` of every output against its original, equal audio digests,
    kept COMMENT values and COMPOSERSORT, overrides;
  - no cover and multi-disc: no picture left, the disc directories;
  - trailing ID3v1, leading ID3v2, both: no ID3 left, same audio;
  - determinism: two builds, every music file byte-identical, receipts equal
    but for `build_id`;
  - a corrupt track, cover, LRC or attachment original, and a missing one;
  - a tag writer that changes the samples (another sine's frames behind the
    written metadata): only the digest comparison catches it;
  - a tag writer that loses COMMENT: `media_tags_verification`;
  - a faulty copy (a stray byte) caught by the read-back, for the cover, a
    track and an attachment;
  - attachments of 16 levels and of 1,024 bytes below `Extras/`: built, and
    the receipt round-trips through `ParseReceipt`;
  - ENOSPC at five points; the space check; the fsync order; cancellation
    while a tool runs (the process gone, the staging removed); the removal
    build; refusals;
  - after every build: `library/` and `originals/` unchanged, and after a
    failure nothing left in `work/render`.
- **From the catalog:** an album imported by the real importer, its render
  claimed with its snapshot, planned and built twice: the same bytes, the
  expected tags and audio.

**Mutation-checked**, each makes a test fail: the digest comparison off;
`VerifyTags` off; the blob hash check off, with and without the read-back;
the read-back off; the cover not embedded; the file and the file/directory
collision checks off; the receipt not sorted; the receipt's order check off;
its canonical comparison off; an input of `render_version` dropped or
written as a literal; `CheckTools` skipping TagLib; the year unpadded; the
genre inheritance changed; track totals from the album; multi-disc only for
several discs; no discard on failure; the discard with the cancelled
context; the staging's parent not fsynced; directories fsynced top-down;
ENOSPC untyped; the names truncation suffix one digit shorter; the receipt
checking paths with its own rule (the planner and the receipt disagreeing
again); the `Extras/` exception removed, or its byte limit loosened; the
directory-spelling comparison off; the planner or the catalog not calling
`catalog.PathCollision`.

```sh
scripts/check.sh ./internal/render/...
scripts/dev.sh go test -race -count=10 ./internal/render/
```

### `internal/publish` — publication, journal, recovery, the render executor (§3.2, §5.3, §6.3, §6.4, §9.3, §9.4, §9.5, §11.1) ✔

The protocol that makes a finished build visible in `library/`, and the
forward completion of an interrupted publication at boot. Disk access goes
only through `internal/fsops` (library/ and work/ as two roots on the same
filesystem, cross-root `renameat2`), SQL only through `sql/publish.sql`
(sqlc) and the catalog's and queue's functions, always under
`store.InCatalogTx`.

| Function | Role |
|---|---|
| `New(Config{DB, Library, Work, Builder, Log})` | the process's one publisher; creates `work/retired` durably |
| `Publisher.Publish(ctx, snap, res)` | preflight, PREPARE, INSTALL, FINALIZE under `publishMu`, then the cleanup outside it; `Report{Outcome, Job, Journal}` |
| `Publisher.Recover(ctx)` | §9.4 at boot step 4: the pending journal completed forward, idempotent; `publish_illegal_state` for a state matching no legal step |
| `Publisher.CleanWork(ctx)` | boot step 5: builds and retired directories no journal references |
| `Publisher.ExecuteRender(ctx, claim)` | the render executor: `render.NewPlan` → `Builder.Build` → `Publish`; every non-fatal path ends in a completion (N-107) |
| `Journal`, `Outcome` (`Published`, `Superseded`, `Refused`) | the publication row and what a publication did |
| `Error` / `Code` | `publish_destination_occupied`, `publish_foreign_output`, `publish_unsafe_entry`, `publish_staging_invalid`, `publish_state_changed`, `publish_illegal_state`, `publish_io`, `publish_suspended`, `publish_journal_pending`, `publish_canceled` |

How §9.3 maps to the code:

- **`publishMu`** is a one-slot channel (so waiting respects the context),
  always taken before any catalog transaction; the catalog never takes it.
- **Preflight** (`preflight`): the staging's receipt parses and names the
  album, build, revision and renderer with the build's hash; the new path
  is absent, or exactly the album's published path without another
  album's receipt; the old path, when different, absent or a directory
  without another album's receipt; no symlink, special file or file on the
  way (N-137). A conflict returns a typed error: the executor fails the
  job (`FailRender`, §6.4) and the build is discarded; no journal.
- **PREPARE** (`prepare`): `catalog.CheckFresh`; ticket, revision or
  renderer stale → `RequeueRender`, discard, no journal; stale claims →
  `ReconcileClaims` in the same transaction, and if another album owns a
  path, `FailRender` with `path_reserved` naming it, never requeued (N-112);
  otherwise the album's current published path and build are checked
  against the preflight's, the single row is inserted (the built revision,
  the renderer, the build, the receipt hash or NULL for a removal, old and
  new paths) and the claims are reconciled to include them. An uncertain
  commit is fatal.
- **INSTALL** (`install`, shared with the recovery): the whole observed
  state is checked first (`checkTransition`); then the new path already
  holding the build is left alone (never a second exchange); otherwise
  parents created and `RENAME_NOREPLACE` of the staging, or
  `RENAME_EXCHANGE` when the new path is exactly the old one; the old path,
  when different, retired to `work/retired/<build_id>` with NOREPLACE
  (absent is already retired; an existing retired name is never
  overwritten); the old artist directory `rmdir`ed if empty (a failure is
  a warning); every directory involved fsynced, deepest first.
- **FINALIZE** (`finalize`): the row re-read `FOR UPDATE` and compared with
  the journal; `published_*` written from the journal's values (NULL path,
  build and receipt for a removal); the render row completed with
  `FinishRender` by the journal's ticket (deleted, or pending for a newer
  request); the journal deleted; the claims reconciled.
- **After the release:** the build directory (which holds the old album
  after an exchange) and the retired directory removed; a failure is a
  warning, and boot step 5 resumes it.
- **After PREPARE, any failure** suspends publishing in the process and is
  returned marked with `jobs.Stop`: the pool stops, the process exits 1,
  and the boot recovers the journal (N-135). A publication already
  prepared gets 30 s after a shutdown to finish (`graceful`), otherwise it
  is left to the recovery.

Tests on real ext4 and PostgreSQL 17 (the protocol tests stage their builds
with a real receipt; the executor and the boot tests build real FLAC albums
with the real tools):
- **Transitions:** a new album; the same path (the exchange, with the old
  album seen in the staging right after it); an artist rename (both
  directories between install and retirement, the old artist directory
  removed, the old claim released); a title change; a case-only rename
  (`Abba/X` → `ABBA/X`, exact paths); a removal, a removal never
  published, a restore.
- **Ownership:** a foreign directory at the new path; another album's
  receipt at the published path and at the new path; damage at the own
  path replaced (receipt removed or garbage, a file altered, a file added);
  symlinks at the new path, as the artist directory and at the old path, a
  FIFO and a file at the new path. Every refusal leaves `library/`
  unchanged and writes no journal.
- **Concurrency (§9.5):** a change during the build (superseded, requeued,
  discarded); a new renderer; a change between PREPARE and FINALIZE
  (published revision is the built one, the job pending again, a second
  render publishes the new one); N-112 repaired and refused (failed,
  never requeued); a name reused during a rename (`path_reserved` naming
  the owner, also between INSTALL and FINALIZE, free after the
  retirement); trash then restore before and after PREPARE; the lock
  order (a catalog mutation commits while a publisher waits for
  `publishMu`); two workers and an artist rename over several albums with
  real builds.
- **Failure and recovery:** a failure at every point after PREPARE for four
  kinds of publication, then a new publisher's recovery, run twice, and the
  cleanup; before PREPARE (old output valid, the render runs again); seven
  illegal states (a missing staging, a foreign directory, another album's
  receipt, the exchanged output damaged so that only a reverse exchange
  would "fix" it, an existing retired name, a symlink at the old path, a
  removal over another album), each refused twice with nothing moved;
  the database lost at FINALIZE through `pgtest.Proxy` (the commit cut, the
  answer lost); the suspension (a second publication refused untouched);
  the shutdown grace, kept and exceeded.
- **Real crashes** (`crash_test.go`, N-136): the publisher in a child
  process killed with SIGKILL at each of the six failpoints for the four
  kinds (21 cases), then two recoveries in fresh children; a crash during
  the recovery itself; since round 10 a crash at five points of a real
  build (`TestCrashDuringBuild`) and a really full disk
  (`TestExecuteRenderOnAReallyFullDisk`, N-143).
- **INSTALL durability** (round 10, N-144): the new artist directory synced
  before the rename and removed again if the installation fails; the moved
  album and `work/retired/<build_id>` fsynced; the order traced
  (`TestPublishFsyncOrder`, `TestPublishRemovesTheArtistDirectoryOfAFailedInstall`).

Mutation-checked, each making a test fail: `old_path == new_path` compared
case-insensitively; the "already installed" check removed (a second
exchange); `published_revision` from the album instead of the journal;
the ticket condition of the completion removed; N-112 requeueing instead
of failing; `publishMu` taken under a catalog transaction; the whole-state
check before INSTALL removed.

```sh
scripts/check.sh ./internal/publish/...
scripts/dev.sh go test -race -count=10 -timeout 60m ./internal/publish/
```

---

### `web/` + `internal/http` HTML pages — UI parts 1 and 2 (§2.1, §2.3, §6.4, §7, §10.3, §10.4, §12.1) ✔

- `web/` embeds layout, Library, Album, Import, Activity, CSS and small vanilla JS modules; no frontend build. Pages `/`, `/albums/{id}`, `/import`, `/activity`; exact assets under `/static/`. SSR read-only views and search/filter/pagination work without JS. The editor declares JS required.
- Library searches titles/artists through the catalog, filters artist/trash, uses the round-16 cursor. Published fields and one render-job JOIN in `ListAlbumSummaries` give page badges in one snapshot with no per-album queries; no JSON list representation change. Album SSR includes metadata, cover, tracks, inherited overrides, attachments, lyrics, downloads, artist choices and status.
- A single metadata Save PUT carries `If-Match`; other commands use the same ETag and required mutation header. Typed errors preserve edits, 412 never reloads; destructive actions confirm. Status polls every 2 s only while pending/running. Templates escape all catalog text; CSP disallows inline scripts, pages use the API boundary and `nosniff`.
- Tests: server pages/escaping/Host/CSP/404/badges on PostgreSQL, and real Chromium driven by chromedp in the dev/test Docker image for save, two tabs/412, cover upload/choose/remove, inherit, track deletion, trash/restore and idle polling. Mutation probes N-207 and N-213.
- Round 18: Import browses only `/import` and lists unsafe entries as text, creates a batch with one retained UUID, shows scan/unassigned files/candidate reports with safe typed errors and result links; failed import retries support only artist/title replacement or clearing. Activity lists all paged jobs and links albums/reports; failed-only retry, retry-failed and confirmed render-all use the JSON API. Both poll every two seconds only while active, with an activity indicator on those pages. Browser tests execute a real scan and FLAC imports on ext4, a mixed-album override retry and stored-override clearing, an ambiguous branch, an unassigned file and a no-candidate scan; test lost response/idempotent resubmit/409, safe listing/report escaping, job retries, render-all confirmation, stopped polling and nonfailed retry guard. A mixed failed/pending report browser test verifies override drafts, focus and caret through multiple polls and a failed poll, then persists the retry; the Activity test claims a running render and verifies its ticket, uniqueness and lack of a retry action after Retry failed, alongside the pending guard. The HTTP browser listener uses an ephemeral port (N-211). The accepted-retry follow-up invalidates the previous attempt's override draft and ignores pre-retry report responses; a gated Chromium test forces a second failure before the retry refresh, then delivers the old report last (N-210, N-213). No extra catalog API or SQL was required.

- Round 19 (presentation and usability only; no handler, template field, endpoint, polling rule, confirmation or error semantic changed): `web/app.css` rewritten as a flat, structural design system — square corners, one hairline grid over near-black panel and control outlines, no shadows or blur, monospaced labels/badges/paths/timestamps, one accent, tokens on `:root` with a full `prefers-color-scheme: dark` override, `prefers-reduced-motion` honoured, and **no `url()` at all** because the page CSP blocks `data:` images (N-236, N-237). Library: a real filter bar (wide query, medium artist, trash toggle), scannable artist/title rows with a semantic status badge, a framed empty state. Album editor: aligned metadata grid with the artist select, new-artist field and Create button as one cluster; a sticky Save bar that stays reachable while working down the page; a restructured track table (disc/number narrow and fixed-width, title wide, dedicated lyrics, files and remove columns, inherited fields dashed and muted with the inherit command dimmed when already inheriting, `data-inherited` kept); labelled upload rows for cover and attachments with the constraints as help text; only the applicable trash/restore action offered (N-238, N-239, N-241). Import: a file-browser list with CSS-drawn directory/file glyphs, path display, one action row, and a report of scan state, framed warnings and candidate cards. Activity: a toolbar with the heavier Render all as the primary action, one-line job rows (state, subject, aligned action) and an empty state. Status lines and the `role="alert"` error box are one component across the four views. Accessibility: every control keeps a programmatic label (`aria-label` in the compact table), the table keeps its real header cells, the focus ring stays 3px, contrast was checked per token in both themes, and a skip link was added. The full gate and the chromedp suite passed with **no test file touched** (browser tests also run four times in a row); `docs/ui.md` gained the two user-visible facts (N-240, N-242).

### UI redesign round 19: foundations and Library (`web/`, `internal/http/pages.go`, `internal/catalog`, `sql/catalog.sql`) ✔

- **Spec:** `webui-principles.md` (owner, normative for the redesign) and DESIGN.md §2.1, §8.5, §10.3, §10.4; owner decisions N-243.
- **Foundations:** `web/app.css` rewritten by numbered sections (font, tokens, base, shell, status, controls, Library, Album, Import/Activity, motion, narrow, squircle). Tokens on `:root` with a dark override; one shadow (`--shadow-cover`), one curve, 200/400 ms; 3px `--accent` focus ring offset 2px; reduced motion stops every animation and transition. Hanken Grotesk v12 (latin, latin-ext) with `OFL.txt` embedded and served from an explicit `/static/` allowlist (N-244).
- **Shell:** `web/layout.html` in Italian (`lang="it"`): a sidebar with Libreria, Importa, Attività (with the activity dot), Cestino (`/?trash=true`) and «Da sistemare» (`/?fix=true`) with the count of albums whose render failed, read on every page by `catalog.CountFailedAlbums` (sqlc `CountFailedAlbums`); below 52rem a row at the top. The page head holds the title and, on the Library, the search (sticky, solid).
- **Library:** `web/library.html` + `pageData.Library` (`libraryData`, `pageAlbum` with cover presence, year, initials, status and its Italian word). Grid of original covers (`loading="lazy"`, `decoding="async"`, 200×200), graphite initials without a cover, a red dot for Error, a still graphite dot for Queued, an orange pulsing dot for Processing (owner, N-248), nothing for Aligned; artist hits for a search (`catalog.MatchArtists`), the artist filter, the fix filter (`AlbumFilter.Failed`, sqlc `@failed`), the empty states of the principles. `web/library.js` (one new module): live search and loading on scroll by adopting the server-rendered page (N-246), the open album under its row with colours from a 32×32 canvas histogram and the 4.5:1 rule, ARIA, focus and arrow keys, and the cover's view transition into the editor (N-247). No `innerHTML` anywhere; the CSP is unchanged.
- **Other views:** the album, import and activity pages keep their markup, ids and behaviour; they use the tokens (hairlines instead of outlines, dot + word statuses, quiet buttons) until rounds 20–21.
- **Budget:** CSS + JS 59,702 bytes; fonts 54,292 bytes (N-251).
- **Tests:** `internal/http/library_test.go` (server pages on PostgreSQL: tiles, words, initials, empty states, trash, artist hits, fix filter and count on every page, 422s, static allowlist with types and cache headers); `internal/catalog/failed_test.go` (failed filter with trash, artist, search and cursor; count; `MatchArtists`); `internal/http/library_browser_test.go` in real Chromium: covers and initials, dots, the font actually loaded, escaping in grid and panel, open/Esc/Enter/Space/second click/move to another cover with focus return, `--cover-bg`/`--cover-ink` on real PNG covers meeting 4.5:1 including both fallbacks, arrow keys, the view-transition name, live search, artist choice, loading on scroll with the cursor, the no-JS grid, next page and form, the fix and trash views, no console error and no CSP violation. Existing page tests updated deliberately (N-249). Mutation checks N-250; review fixes (stuck-head hairline, track artist line, honest status fixtures and years) and re-run mutants N-253–N-255. Review screenshots: `MUSICLIB_UI_SHOTS=/src/tmp/ui-shots` (N-252).

### UI redesign round 20: the album editor (`web/album.html`, `web/app.js`, `web/app.css`, `web/layout.html`, `internal/http/pages.go`) ✔

- **Spec:** `webui-principles.md` «Album», «Linguaggio», «Errori», «Vincoli tecnici»; DESIGN.md §1.2, §4.1, §4.3, §8.5, §10; owner decisions N-243 and N-256 (the album page is in English).
- **Server:** `pageData.Editor` (`editorData`): the album, its ETag, the §10.3 status and its English word (`statusWord`, shared with the Library since the merge, N-278), tracks by disc (`pageDisc`, `pageTrack`: own artist/genre, «No genre», lyrics), `MultiDisc`, image-only cover choices (`coverCandidate`: JPEG/PNG by format, or by name without a format hint; N-257), `.lrc` choices, the cover limit in whole MB (`coverMB`, 16 with FLAC, 20 otherwise), the album count of the artist for the rename (`artistAlbums`, active + trashed), every artist with its ETag. The layout draws no page head for the editor and loads `app.js` on the album page only.
- **Markup:** one page for JS and no-JS: readonly fields and hidden commands marked `data-js`; declarative popover ⋯ menus (downloads work without JS); `<dialog>`s for confirmations, the cover picker, the lyrics picker and the bulk-lyrics proposals; notices per area with a closed «Details»; changes rendered as `data-method`/`data-url`/`data-ask` controls.
- **Module:** `web/app.js` rewritten (13,678 bytes): edits are named fields against a baseline (N-258); the Save bar; save and every change without a reload by adopting the page's own `#editor` and the sidebar's current view and count (`.nav`, `.fix-filter`; the whole `.sidebar` before the merge, N-278), the ETag from the change's answer, edits re-applied (N-260); errors table and duplicate-number sentence (N-261); conflict reapply (N-262); artist datalist, create, rename with the album ETag's own bump (N-266); bulk `.lrc` matching (N-264); drop on the cover; `beforeunload`; status polled from the page every 2 s only while pending (N-265).
- **Style:** section 8 of `web/app.css` rewritten, the old album CSS deleted; fields as text until used, hairline menus and sheets without shadow or scrim, the Save bar sliding from the bottom, the phone layout below 40rem (N-268).
- **Budget:** CSS + JS 59,702 → 65,941 bytes at the end of the round (N-267; the owner has since set the target at 90 KB).
- **Tests:** `internal/http/album_page_test.go` (server, real PostgreSQL: `coverCandidate`, `coverMB`, the rendered page: image-only picker with thumbnails, readonly title, English status word, limit in words, disc headings only with more than one disc, placeholders and «No genre», render in the menu, trash band, datalist with ETags; status words); `internal/http/album_editor_browser_test.go` in real Chromium: save in place with the Save bar count, «Saved», the answer's ETag and a second save, Enter in the title, the `beforeunload` prompt; inherited values and «No genre» saved as null/""; the artist picker, «Create artist», «Rename artist» on active and trashed albums then a save without 412; conflict in two tabs and reapply; cover menu, image-only picker thumbnails, choose, remove, drop, upload; bulk `.lrc` proposals, unmatched and doubly claimed files, assignment, the lyrics menu, no false edits; trash and restore from the keyboard, track menu from the keyboard, cancelled deletion; errors with «Details» closed and the duplicate number naming both tracks; escaping in every field, file name and dialog, and the no-JS page; no console error and no CSP violation in each; `internal/http/album_browser_test.go` (fixture and review screenshots). Updated: `TestPagesCatalogEscapingAndBoundary`, `TestBrowserPollingStopsWhenIdle`, `browserWait`; `TestBrowserEditorConflictAndContent` replaced (N-260). Mutation checks N-269.

### UI redesign round 20b: English UI and the sidebar (`web/`, `internal/http/pages.go`) ✔

- **Spec:** `webui-principles.md` (glossary, voice, empty states, the shell), the owner's decisions of 2026-09-27 (English; icons, a wider sidebar, a collapse button) and DESIGN.md §2.3, §10.3, §10.4. Built in parallel with round 20, without touching `web/album.html` or `web/app.js` (N-275).
- **English:** `<html lang="en">`; the layout, the Library and its panel, the page titles and the Library status words (Waiting / Updating / Needs attention) in `pages.go`; the Import and Activity copy as far as their strings go, with one word table in `queue.js` for states, kinds and source entries while `data-state` / `data-type` keep the API values (N-270, N-271). Error codes, ISO times and job kinds on Activity stayed for round 21 (done there).
- **Sidebar:** six hand-drawn inline SVG symbols in the layout (20px, 1.5 strokes, `currentColor`, `aria-hidden`); 248px expanded, 72px collapsed, every row on a 3rem icon column so that no icon moves; a real toggle button (`aria-expanded`, `aria-controls`, «Collapse sidebar» / «Expand sidebar»); collapsed labels as tooltips on hover and focus that stay the accessible names, the Needs-attention count as a badge on its icon, the activity dot on its icon; the width animates 400 ms on the system curve only when pressed, never with reduced motion. The state is `data-sidebar` on `<html>`, set from `localStorage` (try/catch, expanded by default) by `web/sidebar.js`, a 1.2 KB classic script at the top of `<head>`, so it is there before the first paint; served from the `/static/` allowlist (N-272, N-273). Phones: a row of icon-over-label views, Needs attention as icon and number, no toggle.
- **Budget:** CSS + JS 64,226 bytes at the end of the round (N-267, N-275; the owner has since set the target at 90 KB).
- **Tests:** `copy_test.go` (the Italian guard over the embedded templates and modules, its own check, nine rendered pages), `sidebar_browser_test.go` in real Chromium (toggle by keyboard and click, `aria-expanded`, geometry of both states, accessible names through CDP, badge, current page, tooltip, persistence across a navigation and a reload with the state present at parse time and no transition on load, reduced motion, throwing storage, no JavaScript, the phone row, no CSP violation or console error); every Italian assertion updated and the queue tests moved to `data-state` / `data-type` (N-274). Mutation checks N-276; review screenshots, now with the sidebar expanded, collapsed and with a tooltip (N-277).

### UI redesign: merge of rounds 20 and 20b (`web/`, `internal/http/`) ✔

- **Spec:** the owner's decisions of 2026-09-27 (English, N-256; the sidebar) and 2026-09-28 (the 90 KB budget, N-267); `webui-principles.md`; DESIGN.md §2.3, §10.3, §10.4. The two rounds were built in parallel on `0076b94` and merged file by file (three-way where both changed a file).
- **One shell:** `web/layout.html` has round 20b's English sidebar (icons, toggle, `sidebar.js` first in `<head>`, `lang="en"`) and round 20's `{{if .Editor}}` (`app.js` on the album page only) and `{{if not .Editor}}` (no page head for the editor). The album page's back link is «Library»; its `#editor` and `#savebar` lost the `lang="en"` they carried inside the Italian document.
- **One set of status words:** `statusWord` in `pages.go` serves the Library and the album page (Waiting / Updating / Needs attention; «Up to date» and «In the trash» are never shown, N-248); round 20's `albumStatusWord` («Needs fixing» for Error) is gone (N-278).
- **The album page and the sidebar:** the in-place refresh adopts the sidebar's `.nav` and `.fix-filter` only, so the toggle keeps its `aria-expanded` and label and `<html data-sidebar>` is never touched; the collapsed state sets `--sidebar` itself, so the fixed Save bar starts at the sidebar's edge in both states (and follows its width animation); the Library panel's disc caption keeps the panel's ink (round 20's graphite `.disc` had reached it) (N-278).
- **Budget:** CSS + JS 70,691 bytes (`app.css` 30,204, `app.js` 13,832, `library.js` 13,412, `queue.js` 12,022, `sidebar.js` 1,221), under the owner's 90 KB target; per page 43,447 to 45,257 bytes; fonts 54,292 (N-267).
- **Tests:** the Italian guard covers every embedded template and script and thirteen rendered pages, among them the album page active with a status word, failing and trashed (`copy_test.go`); `TestBrowserSidebarAlbumRefresh` (collapsed sidebar across a save and a trash on the album page, toggle state, current view, no width transition, the Save bar's edge in both states and on a phone); the Library panel's disc caption in the panel's ink; album screenshots with the sidebar collapsed. Mutation checks and screenshots in N-278.
- **Review (N-279 to N-283):** `app.js` opens the closed ⋯ menu of an invalid field on Save (it used to abort silently); `download` on the album page's download links (no «Leave site?» with unsaved edits); the back link of a trashed album is «Trash» → `/?trash=true` and is adopted by the in-place refresh; `text-overflow: ellipsis` on track fields. New or extended tests: `TestBrowserAlbumErrors`, `TestBrowserAlbumSaveInPlace`, `TestBrowserAlbumTrashAndMenus`, `TestBrowserAlbumArtistPickerAndRename`, `TestBrowserAlbumLyricsStopAtFailure`, `TestAlbumPageMarkup`. Budget 70,973 bytes (`app.css` 30,229, `app.js` 14,089).

### UI redesign round 21: Import and Activity (`web/`, `internal/http/`, `internal/catalog`, `internal/jobs`, `internal/importer`, `sql/`, `migrations/00002_job_attention.sql`) ✔

- **Spec:** `webui-principles.md` «Importa», «Attività», «Linguaggio», «Errori», «Stati vuoti», «Vincoli tecnici»; DESIGN.md §6.4, §7.1–§7.3, §7.6, §10.2–§10.4, §11.3; the owner's decisions of 2026-09-28 (the compacting Library head, N-284; stale failures, N-285).
- **Import** (`/import`, rendered by the server, `queuepages.go`, `web/import.html`): the music folder Finder-style with a breadcrumb, folder rows with what each holds (`importer.Tally`, names and types only through the confined `importer.Browse`; `SortForDisplay`), the entries never followed as inert rows, the open folder's files on one line; one filled «Import everything in <folder>» over `POST /api/imports` with the id retained per page; the fixed sentence; «Recent imports» (`catalog.ListRecentImports`). The results (`?batch=`): the progress row, three tabs with counts (Needs attention, Imported, Already there) that open on the first non-empty one and follow the ARIA tabs pattern (links without JS); rows with the album's cover and name, or the folder with one plain sentence from one code table, its one fix (a title or artist override, or Retry only where it can work) and Dismiss, the code in a closed «Details»; empty, missing and unreadable folders; an import past its 90 days (N-286 to N-288).
- **Activity** (`/activity`, `catalog.ListActivity` in one snapshot): In progress, Waiting, Needs attention with dots and totals, at most 100 rows each; each row the album's thumbnail (lazy, 40 px, initials otherwise) and name linking to the editor, or the folder of an import linking to its results; relative times in `<time datetime>` with the full date on hover and focus; one filled «Retry all» confirming with the answer's count; Advanced «Rebuild the library folder» (`POST /api/render-all`, a confirmation sheet with the server's count, then the answer's); the empty state (N-289, N-290).
- **In place** (`web/queue.js`, rewritten, 11.0 KB): the page polls its own URL every 2 s only while work runs and adopts rows by version, small parts by the server's HTML; drafts, focus, caret, open «Details» and scroll survive; answers applied in request order keep N-210's guarantee; errors in one sentence where they happen (N-291).
- **Stale failures** (owner, N-285): migration `00002` adds `jobs.dismissed_at` and the SQL functions `job_superseded` and `job_needs_attention`; `POST /api/jobs/{id}/dismiss` (`jobs.Dismiss`, `catalog.DismissJob`); a failed import is superseded by a later successful or already-there import of its folder (of a subfolder too for the structural errors, and for a scan); settled failures leave Needs attention, its counts, Activity and «Retry all»; the job JSON shows `dismissed_at`, `superseded`, `needs_attention`; N-200's retention unchanged.
- **The Library head** compacts while stuck (scroll-state query, IntersectionObserver fallback in `library.js`), giving back its height as a margin so the page never gets shorter and the threshold cannot oscillate (N-284).
- **Removed:** the old queue module, the `.badge` and queue styles, the unused `.panel`, `.help`, `.statusline`, `.field`, `.btn-sm`, `.notice-title`, `.notice-info` rules; the «until round 21» remarks; N-271 resolved.
- **Budget:** CSS + JS 74,119 bytes (N-294); 76,612 after the review (N-296).
- **Tests:** `jobs.TestDismiss`, `TestSuperseded`, `TestNoValidCandidateAttention`, `TestRetryFailedSkipsSettled`; `catalog.TestListActivity`, `TestRecentImportsCardsAndRenderAllCount`, `TestDismissJob`, `TestPurgeImportReports` (dismissed cases); `importer.TestTallyOnDisk`, `TestSortForDisplay`; `http.TestDismissAPI`, `TestJobsList` (17 keys), `TestRelativeTime`, `TestJobProblems`, `TestImportPageTabsAndStates`, `TestQueuePagesEscaping`, `TestPagesSpeakEnglish` (the new pages); in real Chromium `TestBrowserImportBrowse` (with and without JS, keyboard, escaping, empty and missing folders), `TestBrowserImportAndOverride` (the real importer: progress, tabs, sentences, the title fix), `TestBrowserImportLostAnswerAndConflict`, `TestBrowserImportReportAndRetry`, `TestBrowserImportDraftSurvivesPolling` (drafts, caret, focus, Details, position, node identity, a failed poll, Dismiss mid-poll), `TestBrowserImportRetryInvalidatesPreviousAttempt`, `TestBrowserImportRetryOnlyWhenItCanWork`, `TestBrowserImportTabs`, `TestBrowserStaleFailures` (the owner's rows), `TestBrowserActivityActionsAndIdle`, `TestBrowserLibraryHeadCompacts`; review screenshots `TestBrowserQueueScreenshots`. Mutation checks N-293.
- **Review (N-295):** the owner's live failures checked against N-285's rule; `TestBrowserImportDraftSurvivesPolling` (the focus goes to the next row when its row leaves), `TestBrowserActivityActionsAndIdle` (Dismiss on import rows only), `TestPagesSpeakEnglish` (the results, folder and missing views, and every sentence of the code table) extended; six review mutants, two survivors now killed.
- **After the review (N-296, owner):** the dot's label only while work runs, «And N more» only when some are not listed, notices made by `queue.js` when an error happens, groups and empty-state sentences as `data-part` elements added and removed by the poll, «Retry all» delegated; `TestQueuePagesRenderOnlyWhatIsThere`, `TestBrowserActivityActionsAndIdle`, `TestBrowserImportAndOverride` extended; six mutants killed.

### Round 22: no orphan artists, the track duration (`internal/catalog`, `internal/http`, `internal/importer`, `internal/render`, `internal/publish`, `internal/jobs`, `internal/media`, `sql/`, `migrations/00003_blob_duration.sql`, `web/`) ✔

- **Spec:** DESIGN.md §4.1–§4.3, §5.3, §7.6, §9.1, §10.1–§10.3; `webui-principles.md` «Album», «Tipografia», «Cura nei dettagli invisibili»; the owner's decisions of 2026-09-28 (no orphan artists; no temporary data-migration code; a read-only Duration column).
- **No orphan artists** (N-297): `catalog.leaveArtist` (sqlc `DeleteOrphanArtist`) runs in `applyUpdate` right after `UpdateAlbumMetadata`, the one writer of `albums.artist_id`, whenever the artist changed: the artist left without any album, active or trashed, is deleted in the same catalog transaction. The FK RESTRICT is the second guard. The folder on disk follows the renders, removed by the publisher only when empty (§9.3).
- **A new artist with the save** (N-298): `catalog.AlbumUpdate.NewArtist`; `UpdateAlbum` normalizes it, then creates it inside the transaction after the If-Match check with `CreateArtist`'s rule (`insertArtist`, shared): a failed save creates nothing, a name that exists is 409 with the existing artist's id and both names. The PUT body gains the required key `new_artist` (exactly one of it and `artist_id` non-null). The editor's «Create artist» stages the name in a hidden field («New artist», one change), dropped when the field names another artist; after a 409 or `artist_not_found` the page takes the server's current list. `POST /api/artists` stays for API clients (N-299); the exact SQL to list and delete the existing orphans is in N-299.
- **Durations** (N-300, N-301): migration `00003` adds `blobs.duration_ms` (NULL = unknown, `blobs_duration_check`: ≥ 0 and audio blobs only). The importer passes the duration of its existing probe (`catalog.Blob.DurationMS`, `media.DurationMS`); `InsertBlobs` stores it for new rows, `catalog.RecordDurations` (`SetBlobDuration`) fills only unknown ones, never overwriting. The render snapshot says which blobs are unknown (`SnapshotTrack.DurationKnown`), the plan marks those tracks (`Track.ProbeDuration`), the builder probes their verified copies once more (failpoint `duration`; a failure is dropped, never the build's) and returns `Result.Durations`, which `publish.ExecuteRender` records in its own short transaction (a failure is logged). No revision, no render, no effect on `AudioDigest`, the fingerprint, the receipt or the output. «Rebuild the library folder» fills an existing library once.
- **API and UI** (N-302): each track of the album JSON has `duration_ms` (number or null, read-only). The album page's track table has a read-only Duration cell (server-rendered, so also without JavaScript): m:ss or h:mm:ss to the nearest second in `<time datetime>`, an en dash with «Duration unknown» for screen readers, graphite Note, tabular, right-aligned, beside the title on a phone; never a form control, so never an edit. Under the table «N tracks» and, when every duration is known, the album's length in words. The Library's open album shows the same times.
- **Budget:** CSS + JS 78,606 bytes (N-305).
- **Tests:** `catalog.TestLastAlbumLeavesArtist`, `TestUpdateAlbumNewArtist`, `TestOrphanArtistConcurrency`, `TestBlobDurations`, `TestBlobDurationValidation`; `importer.TestImportRecordsDurations` (FLAC, MP3, AAC and ALAC M4A, against the pinned ffprobe); `publish.TestRenderRecordsUnknownDurations` (filled, kept, a failed probe, no probe once known, the same files); `http.TestAlbumNewArtist` (header, 428, 412, refusals inside the transaction, 409, concurrent saves naming one new artist), `TestAlbumReassignment`, `TestUpdateAlbum` (the new key's refusals), `TestGetAlbumAndStatus` (the track keys), `TestAlbumPageMarkup`, `TestDurationWords`; in Chromium `TestBrowserNoOrphanArtist` (the owner's scenario, failed saves, a stale list), `TestBrowserTrackDurations` (column, dash, not dirty after save and reload, phone, panel), `TestBrowserAlbumArtistPickerAndRename` (staging); `cmd/musiclibd.TestReleaseCollectionInterruptedAndRestored` (every imported track has a duration); screenshots `tracks` and `staged`. Mutation checks N-304. A rare race in `cmd/musiclibd.TestEndToEndImportThroughAPI` (a retry answered after a worker had already claimed the job) fixed in the test (N-306).
- **Review (N-307):** `catalog.TestBlobDurationAfterUnknownFormat`, `media.TestDurationMS`, `TestDurationWords` (nearest minute), `TestBrowserNoOrphanArtist` (the album's artist back, nothing to save, when a staged name is dropped) added or extended; four review mutants, all four survivors now killed.

### Round 23: the identity Vibrance MusicLib (`web/`, `internal/http/pages.go`, README, `docs/`, DESIGN.md's title, `LOGO.md`) ✔

- **Spec:** the owner's brand document of 2026-09-28 (outside the repository): the name, the symbol, MusicLib's Violet, the lockup, the favicon, the titles; its player sections are out of scope (owner, N-308). No migration, no API change.
- **Name** (N-308): **Vibrance MusicLib** in titles and at first mention, **MusicLib** after, `musiclib` only for identifiers, all of which are unchanged (module, binaries, `MUSICLIB_*`, Compose names, DB, `.musiclib.json`, `.musiclib-store`, `.musiclib-backup-*`, `X-Musiclib-Request`, `musiclib.sidebar`). README, CONTRIBUTING, `opensource.md`, `docs/operations.md`, `docs/ui.md`, DESIGN.md's title and `web/` comments; the README's stale «the interface is in Italian» and «Importa» corrected; «outside MusicLib» in Activity's Advanced. Page titles «… — MusicLib».
- **Brand assets** (N-309): `web/brand/logo.svg`, the owner's symbol (path data byte-identical, LF endings); `web/favicon.svg`, the same nine paths in Violet with the dark twin inside; in the layout's sprite `brand-sun` (the nine paths, `currentColor`) and `brand-word`, «MusicLib» outlined from Bricolage Grotesque (google/fonts at a pinned commit, instance wght 700, wdth 100, opsz 20, tracking −0.02 em, shaped with HarfBuzz; no font file in the repository).
- **Accent** (N-310): `--accent` `#643fd1` light, `#a79bfe` dark; `--on-accent` unchanged. Every use measured in both themes: 6.6/6.1/5.9/5.0/5.6:1 (light, on paper, canvas, fill, the current row, a notice), 7.1/8.8/6.1/7.4/6.3:1 (dark); text on the accent 6.6:1 and 7.0:1.
- **Sidebar** (N-311): the lockup heads it, the symbol (28 px) in the icons' column and the word on the labels' edge, its capitals centred on the symbol; collapsed, the word goes and the symbol stays in place; the toggle moves to the sidebar's foot, in the icons' column, so collapsing moves no icon and leaves the toggle under the pointer; tab order skip link, brand, views, toggle. Hidden on phones, as the name was.
- **Favicon** (N-312): `/static/favicon.svg` in the allowlist as `image/svg+xml`, `Cache-Control: public, max-age=86400`; `<link rel="icon">` on every page; `/favicon.ico` stays 404.
- **Budget:** CSS + JS 79,209 bytes (N-315); each page carries about 12 KB more HTML (the inline symbol and word).
- **Tests** (owner: no new tests, N-314): `TestBrowserSidebarCollapse` (the toggle is now the eighth tab stop, it comes last in the accessible names, the tooltip check starts from the brand), `TestBrowserSidebarPhone` (the brand, not `.sidebar-top`, is hidden), `TestBrowserAlbumEscapingAndNoScript` (the title), the tooltip screenshot's focus. Evidence: review screenshots in `tmp/ui-shots-r23/` from a throwaway test, not committed (N-315).
- **Logo licence** (owner decisions N-313 and N-318; fix pass after the review N-316, N-317, N-318): `LICENSE` byte-identical; `LOGO.md` states that the sun symbol (`web/brand/logo.svg`, `web/favicon.svg`, `brand-sun`), the owner's artwork, is not under the MIT License, all rights reserved by tommasonovelli; keeping it in unmodified copies is allowed, a modified version distributed to others replaces it with its own. Nothing else is reserved: the names are unrestricted, and the outlined name (`brand-word`) is MIT like the rest, its Bricolage Grotesque letterforms under the OFL. Pointers in the README's «License», CONTRIBUTING, `web/brand/README.md`, the favicon's comment and `opensource.md`'s MIT item; the `opensource.md` P0 name item is only the check that the name does not collide with someone else's. The sidebar toggle at the foot (N-311) confirmed by the owner.

### Release 1.0.0, T1: readiness published last (`cmd/musiclibd`, §11.1) ✔

- **Flaky test removed at its cause** (N-320): `TestAPIFatalErrorStopsTheProcess` sometimes got 503 `not_ready` after `/health/ready` had answered 200. Step 7 published readiness before enabling the API, a production ordering bug that every test calling `/api` after readiness was exposed to. `boot` now enables the API, logs `ready`, then publishes readiness, last. The test also had a second nondeterminism: a worker's idle poll could meet the armed lost COMMIT answer instead of the API. It now holds `jobs` locked while the API request runs, and asserts only the API path. `TestBootStepsInOrder` fails cleanly instead of panicking when an event is missing.
- **Evidence:** reproduced with a 300 ms probe in the window (3/3 `not_ready`); the fixed order passes with the same probe (whole package). Mutations: readiness before `Enable` fails 9 tests; readiness before the `ready` event fails 2; no `jobs` lock under a 200 µs poll interval fails 15/20. With the fix: the target test `-race -count=100`, four siblings `-race -count=30`, the whole package `-race -count=5`, and `scripts/check.sh` all pass.

### Release 1.0.0, T2: version stamping (`internal/buildinfo`, `internal/maintenance`, `cmd/musiclibd`, `Dockerfile`, §11.4) ✔

- **Scope** (N-322, N-327): the image carries its version. The database password file first built in T2 was removed by the owner's decision (N-327): the password stays in `.env` → `DATABASE_URL`, as before, and DESIGN.md is unchanged.
- **Version** (N-325): `internal/buildinfo.Version` (`"devel"`, set by `-ldflags -X` from the build argument `MUSICLIB_VERSION`) is the only source. It appears in the `starting` event, `musiclibd version` (`cmd/musiclibd/version.go`: `version: …` / `render_version: …`, no environment, database or volume) and the backup manifest's `app_version`, replacing `debug.ReadBuildInfo`. Older manifests (`musiclib-devel`) still restore. `build-app` refuses an empty or non-`[0-9A-Za-z.+-]` version and checks that the binary reports it. `runtime` carries the OCI labels title, description, version, revision (`MUSICLIB_REVISION`), source (`MUSICLIB_SOURCE`) and licenses (MIT), declared last, so a new version rebuilds only the go build, the final binary layer (the `COPY` of `musiclibd`) and the labels, and a new revision only the labels (wording corrected in T3, N-328).
- **Tests** (focused, owner rule): `TestBackupManifestAndRefusals` checks `app_version == buildinfo.Version`; `TestServerProcessSIGINT` checks that the real server's first event is `starting` with `version`.
- **Evidence:** `scripts/check.sh` (whole module) passed after the removal (N-327). `TestServerProcessSIGINT` and `TestBackupManifestAndRefusals` ran `-race -count=10`. The `runtime` image built with `MUSICLIB_VERSION=1.0.0-test`: `docker run --rm IMAGE version` prints `version: 1.0.0-test` and the render_version, the labels carry the version, `/secrets` is gone and `init-db-secret` is a usage error (exit 2).

### Release 1.0.0, T3: production and development Compose files (`compose.yaml`, `compose.dev.yaml`, `.env.example`, `scripts/`, docs; §2.1, §10.4, §11.1, §11.3, §11.4) ✔

- **Scope** (owner decisions N-329, N-330): installation from the published image `ghcr.io/tommasonovelli/musiclib:1.0.0` with only `compose.yaml` and `.env.example`, a password and `docker compose up -d`. Keep it simple: production is `postgres` + `app`, nothing else. No Go change except two comments; no new tests (owner rule).
- **`compose.yaml`** (N-329): production, `name: musiclib`, `postgres` and `app` without profile, the app from the published image (the single line to change on an upgrade; version-pinned, digest TO CONFIRM with T4, N-332). Every previous setting kept: hardening, capabilities, PostgreSQL durability flags, healthchecks, stop grace periods, loopback binding, `PUBLIC_ORIGIN`, read-only `/import` with `create_host_path: false`, volumes `pgdata`, `musiclib-data`, `musiclib-backup`. `POSTGRES_PASSWORD` required (`${…:?…}`, a clear message; empty refused too); no `musiclib` fallback.
- **`compose.dev.yaml`** (N-329): standalone (Compose interpolates the whole model, so `include:`/`extends:` would make the tools need a password). The same `postgres` and `app`, the app built from source (`musiclib-app:local`, `MUSICLIB_VERSION: devel`), plus `postgres-test`, `test`, `dev` (profile `tools`), `testdb`, the tool volumes. Password default empty: the tools need no `.env`, and PostgreSQL refuses to initialize without it. Same project and volume names; its config hashes equal those of the owner's running containers, so switching recreates nothing.
- **uid** (N-330): the image is 1000:1000, no Compose file passes `APP_UID`/`APP_GID`; `MUSICLIB_UID`/`MUSICLIB_GID` are only the `user:` override, with host directories owned by that uid.
- **Scripts** (N-331): `common.sh` has `compose_dev` (always `compose.dev.yaml`: `check.sh`, `dev.sh`, `fuzz.sh`, `start_test_db`) and `compose_app` (from the repository root, no `-f`: `COMPOSE_FILE` from the environment or `.env`, else `compose.yaml`: `maintenance.sh`, hence `doctor.sh`, `backup.sh`, `rebuild.sh`, `restore.sh`). No `--profile app` left anywhere.
- **`.env.example`**: `POSTGRES_PASSWORD=` with the `openssl rand -hex 32` recipe and the initialization-only caveat, `PUBLIC_ORIGIN`, and commented `MUSICLIB_BIND`, `MUSICLIB_PORT`, `MUSICLIB_IMPORT`, `MUSICLIB_DATA`, `MUSICLIB_BACKUP`, `WORKERS`, `MUSICLIB_UID`/`GID`, `COMPOSE_FILE`, one line each.
- **Docs**: `docs/operations.md` (first start from the image, with the image-not-yet-published caveat; running from source; upgrades; «Upgrading from a source build», N-333; plain-Compose maintenance equivalents; restore; release check through `compose.dev.yaml`); `docs/docker.md` (the two files, services, variables incl. `COMPOSE_FILE`, pinned images incl. the app image, raw commands); CONTRIBUTING; README command blocks and the sentences they made false; comments in `docker/with-testdata.sh`, `internal/faulttest/fullfs.go`, `internal/media/runner.go`, `Dockerfile`. T2's layer wording corrected (N-325).
- **Commands**: production `docker compose up -d --wait`; the owner's source build `COMPOSE_FILE=compose.dev.yaml` in `.env`, then `docker compose up -d --build --wait` (or `docker compose -f compose.dev.yaml up -d --build --wait`); maintenance `scripts/doctor.sh --deep` etc., or `docker compose stop app`, `docker compose run --rm --no-deps app doctor --deep`, `docker compose start app`.
- **Evidence** (N-333): `config` of both files with and without the password; old vs new resolved configs differ only in the app's image/build; production smoke in the isolated project `musiclib-smoke` from a scratch directory holding only `compose.yaml` and `.env` (image built locally under the release tag, `version 1.0.0-smoke`): healthy, `/health/ready` ready, `version`, plain doctor `--deep` and backup, `scripts/doctor.sh --deep` and `scripts/backup.sh` (plus the `backup_exists` refusal leaving the app stopped), then `down -v` after checking the `musiclib-smoke_` volume names, and the smoke tag removed; `COMPOSE_FILE` from `.env` honoured by the wrappers; the dev file's password-less PostgreSQL refusal. `scripts/lint-shell.sh` clean; `scripts/check.sh` (whole module) passed.

### `internal/http` — the API's conventions, security boundary and first endpoints (§2.3, §10.1, §10.2, §10.4) ✔ (round 11)

The API is plain `net/http` (Go 1.22 method and wildcard patterns). There
is no framework and no new module. Handlers validate and translate; every
catalog write goes through `internal/catalog` (§13.2), and every read is
one catalog snapshot. The package holds no SQL and no absolute path
(N-151).

| Piece | Role |
|---|---|
| `New(Config{PublicOrigin, RenderVersion, Fatal, Log})` | the API, answering 503 `not_ready` until `Enable` |
| `Enable(*catalog.Service)` / `Disable(code, message)` | serve the catalog; or answer 503 with that code (shutdown, suspended publishing, a fatal database error) |
| `ServeHTTP` | `nosniff`, `Cache-Control: no-store`, the §10.4 boundary, clean `/api/` paths only, availability, then the router |
| `SecurityHeaders(next)` | `nosniff` on everything else the server answers (health) |
| `ETag(kind, id, revision)` | `"album:<uuid>:<rev>"`, `"artist:<uuid>:<rev>"` (§10.1) |
| `ifMatch` | the rules of N-147: 428 without or with `*`, 400 malformed, 412 for tags that name no revision of this resource, the revision otherwise; the resource is the caller's choice (the album's for its sub-resources, §10.2) |
| `readObject` / `object` | the strict JSON of N-148: media type, 16 MiB (413), UTF-8 and lone surrogates, one value, duplicate keys at any depth; then exact keys, every key required, typed decoders, canonical ids |
| `translate`, `statusOf` | typed errors to `{code, message, details}` and the status of N-149; messages from the typed errors, never `err.Error()`; a fatal store error disables the API and calls `Fatal` once (§6.4) |
| `RequestHeader` | `X-Musiclib-Request`, required with value `1` on every mutation |

Endpoints (§10.2); representations in N-150:

| Endpoint | Answer |
|---|---|
| `GET /api/artists` | every artist of the catalog, those without albums included (owner decision N-146: the artist selector of the album editor) |
| `POST /api/artists` `{name}` | 201 + `Location`; 409 `artist_exists` / `artist_folder_conflict` with `details.artist` (§10.2) |
| `GET /api/artists/{id}` | `{id, name, revision, etag}` + `ETag` |
| `PUT /api/artists/{id}` `{name}` + If-Match | the atomic rename (§4.3), every album bumped and enqueued; 200 with the artist |
| `GET /api/albums/{id}` | the desired aggregate + `ETag` |
| `GET /api/albums/{id}/status` | revisions, renderer, published path (relative), render job; `no-store`, no ETag |
| `PUT /api/albums/{id}` + If-Match | `{artist_id, title, year, genre, compilation, tracks: [{id, disc, no, title, artist, genre}]}`, one transaction; exactly the current tracks; another `artist_id` is the reassignment of §4.3; 200 with the album |
| `DELETE /api/albums/{id}` + If-Match | trash; 200 with the album |
| `POST /api/albums/{id}/restore` + If-Match | restore; 200 with the album |
| `POST /api/albums/{id}/render` + If-Match | forced enqueue with no new revision (`catalog.RequestRender`, `jobs.EnqueueRender`); 202 with the status |
| cover, attachments, lyrics, track deletion, downloads | round 14: the table of the next section |
| the library list, imports, jobs, retries, render-all | round 16: the table of the section after it |

Tests: a real `httptest` server over the real handler, over the real
catalog on real PostgreSQL 17. No mock.
- **Artists:**
  - creation, normalization, `Location`;
  - the list: every artist, the no-album and the trash-only ones included, the
    order;
  - 409 with the existing artist, by casefold and by sanitization
    (`AC/DC` against `AC_DC`); 422 texts; nothing written on a refusal;
  - eight concurrent creations: one 201, seven 409 naming it;
  - the rename: 428, 412 with the current revision, then the albums
    bumped and enqueued and the album's old ETag stale; a rename
    conflict.
- **The album:**
  - every field of the aggregate; the declared field order;
    byte-identical bodies across GETs; HEAD;
  - the status: `no-store` and no ETag; a failed job whose stored
    message holds database text is shown with the safe message (N-150).
- **PUT:**
  - a no-op without bump or render; a change with a swap of track
    numbers; the answer equal to the next GET;
  - 428 (absent, `*`), 412 (stale, another album's ETag, the artist's,
    weak, foreign), 400 (two tags of the album, garbage), 404 before
    412;
  - 17 content refusals, each leaving the album unchanged: track list,
    duplicate id, numbers, ranges, texts, an unknown artist (422), blob
    and id not being fields, missing, null and mistyped fields,
    non-canonical ids.
- **Reassignment:** to another artist, with the `album_folder_conflict`
  first.
- **§12.2, "Due finestre UI" at the API:** 20 rounds of two concurrent
  PUTs of one revision. Exactly one 412, naming the winner's revision;
  the loser reloads and re-applies; both changes kept, and the revision
  arithmetic exact.
- **Trash, restore and the forced render:** 428, `body_not_allowed`,
  no-ops, a restore conflict, 412; the render with no bump, coalescing
  on the same row with a newer ticket, and 404.
- **The boundary** (24 cases): Host (case, port, rebinding IP), Origin
  (another, `null`, a trailing slash, https, two fields), the header
  (missing, `0`, `true`, twice) on POST, PUT and DELETE, CORS preflights,
  a non-browser POST. No CORS header and always `nosniff`; a refused
  request changes nothing.
- **Routing:** JSON 404 and 405 with `Allow`; no redirect for unclean
  paths.
- **Bodies over HTTP:** exactly 16 MiB accepted, one byte more is 413;
  duplicate keys; trailing values; unknown and case-variant keys; invalid
  UTF-8 and surrogates; 415.
- **Availability:** 503 `not_ready` before `Enable`, behind the
  boundary; 503 `shutting_down` after `Disable`.
- **§6.4:** the database refusing connections gives 503
  `store_connection_lost` with no database text, the API stays disabled,
  and `Fatal` is called once. A COMMIT answer lost through `pgtest.Proxy`
  gives 503 `store_commit_uncertain`, the row is durable, and `Fatal` is
  called once.
- **Unit tests:** 37 If-Match cases; 38 JSON cases; 26 schema cases; the
  media types and the limit; `FuzzCheckJSON` (about 3.4M executions).

Mutation-checked (each makes a test fail):
- an absent If-Match accepted; `If-Match: *` accepted;
- the Host check removed; the Origin check removed; `Origin: null`
  accepted;
- the header check removed; any header value accepted;
- the duplicate-key check removed; trailing values accepted; the
  surrogate check removed; a duplicate track id accepted;
- `Disable` ignored; `Fatal` not called;
- the job-message sanitization removed, at the catalog source and at the
  API.

```sh
scripts/check.sh ./internal/http/...
scripts/dev.sh go test -race -count=3 ./internal/http/ ./internal/catalog/ ./cmd/musiclibd/
scripts/fuzz.sh FuzzCheckJSON 90s ./internal/http
```

### The album editor's content: cover, attachments, lyrics, track deletion, downloads (§4.3, §5.1, §5.2, §7.4, §7.5, §8.5, §10.2, §11.2) ✔ (round 14)

The Phase 4 content operations over the API. Handlers validate and
translate; every change is a transaction of `internal/catalog`; every blob
goes through the single put of `internal/blobstore`; no SQL and no absolute
path in `internal/http` (N-180).

| Endpoint | Answer |
|---|---|
| `PUT /api/albums/{id}/cover` + If-Match, a JPEG or PNG (`application/octet-stream`) | the cover (§8.5: 20 MiB, 40 Mpixel, a valid decode; N-091: 422 `cover_not_embeddable`); only the cover, not an attachment (N-174); 200 with the album |
| `PUT /api/albums/{id}/cover` + If-Match, `{"attachment_id"}` | an image attachment of the album as the cover; it stays an attachment (§7.4) |
| `DELETE /api/albums/{id}/cover` + If-Match | no cover: no `cover.*`, no embedded picture; a no-op without one |
| `POST /api/albums/{id}/attachments?path=<relative path>` + If-Match, the file | 201 with the album and `Location` of the content; at most 256 MiB, streamed; 409 `attachment_path_collision` (N-172, confirmed by the owner) |
| `DELETE /api/albums/{id}/attachments/{attachment}` + If-Match | the row goes, the blob stays; the cover and a track's lyrics stay even if they are the same blob (N-176) |
| `PUT /api/albums/{id}/tracks/{track}/lyrics` + If-Match, the file or `{"attachment_id"}` | an LRC of at most 2 MiB of UTF-8, or an `.lrc` attachment, which stays (N-177, owner decision 2026-09-25) |
| `DELETE /api/albums/{id}/tracks/{track}/lyrics` + If-Match | `lyrics_hash = NULL`; a no-op without lyrics |
| `DELETE /api/albums/{id}/tracks/{track}` + If-Match | the track goes, the blob stays; the last one is 422 `no_tracks` (§4.3) |
| `GET /api/albums/{id}/tracks/{track}/original` | the original audio, `attachment`, named as imported |
| `GET /api/albums/{id}/tracks/{track}/lyrics` | the LRC, `text/plain; charset=utf-8`, `attachment` |
| `GET /api/albums/{id}/cover` | the cover, `inline` |
| `GET /api/albums/{id}/attachments/{attachment}/content` | the file: `inline` only for a validated JPEG or PNG, `application/pdf` for a PDF, otherwise `application/octet-stream`; always `attachment` but for images (N-175) |

**Pieces:**
- `internal/catalog/content.go`: `SetCover` (`CoverChoice`), `RemoveCover`,
  `AddAttachment`, `DeleteAttachment`, `SetLyrics` (`LyricsChoice`),
  `RemoveLyrics`, `DeleteTrack`, each one `changeAlbum` (the album's
  revision compared in the transaction, the single bump and enqueue of
  §4.3, no-ops without either, N-179); the blob recorded with the import's
  rules (N-102); the pure `AttachmentPath`, `AttachmentConflict`,
  `CheckLyricsAttachment`, `CheckRevision` and `Service.CheckCoverFits`; the codes
  `attachment_not_found`, `track_not_found`, `cover_not_embeddable`,
  `invalid_lyrics`. Nine queries in `sql/catalog.sql`, no schema change.
- `internal/media/cover.go`: `ValidateCover`, the §8.5 rule moved from the
  importer and shared with the API; a read error is `media_io`, never an
  invalid image.
- `internal/http`: `upload.go` (the protocol of N-172 and N-173: the media
  types, the limits, the budget reservation and the put, the snapshot
  precheck, the failpoint `upload_pinned`), `content.go`, `download.go`
  (`http.ServeContent`, the RFC 6266 disposition); `API.Enable(Backend)`;
  `Config.Failpoints`.
- `cmd/musiclibd`: the API is enabled with the boot's blob store, budget
  and work root.
- `render_version` unchanged (N-180).

**How it maps to DESIGN.md:**
- **§10.2 conditional changes:** the album's ETag in If-Match (428, 400,
  412 as N-147), compared in the transaction; §10.4's boundary on every
  mutation.
- **§10.2 uploads, §7.5:** the blob is pinned before the transaction; a
  failed save leaves an unreferenced blob, never a reference (tested with
  the failpoint). A snapshot precheck (404, 412, 409, N-091) refuses what
  cannot be saved before anything is read or pinned (N-173); an
  attachment chosen as lyrics is refused for an unknown track or a name
  without `.lrc` before its blob is read.
- **§11.2:** every put reserves its size in `jobs.Budget` against statfs
  minus 1 GiB; 507 `insufficient_space` (confirmed by the owner, N-172).
- **§5.2:** the attachment's path is validated by `names` before any byte
  is read, kept as sent in `rel_path`, sanitized by the plan; collisions
  after normalization (file, file/directory, directory spelling) are 409
  naming both.
- **§4.3:** removals without undo, blobs kept, at least one track, the
  trash included (N-178).
- **§10.2 downloads:** by entity id within the album's snapshot, never by a
  client path; `nosniff`; only validated images inline (N-175).

**Tests** (real PostgreSQL 17, the real blob store on ext4, the real
tools; no mock):
- **media** (`cover_test.go`): `ValidateCover`'s codes, a read error
  (`media_io`) told from an invalid image.
- **catalog** (`content_test.go`): every operation's 428, 412, 404
  (another album's track and attachment included), bump and enqueue, no-op
  and refusal without bump, render or wake-up; N-091 with a refusing
  `CoverFits`; a blob known with another size or format; the attachment's
  §5.2 refusals with their codes and seven collision kinds, NFC against
  NFD; the cover kept when its attachment goes, and a track's lyrics kept
  when their `.lrc` attachment goes; the last track, in the
  trash too; ten rounds of two concurrent deletions of an album's two
  tracks (one succeeds, one 412, one track left).
- **http** (`content_test.go`, `download_test.go`): each endpoint's 428,
  412, 404, 415, 422 and success; §8.5 refusals (text, a truncated JPEG,
  a 40,005,000-pixel PNG header, a GIF, an empty body); 413 at exactly the
  limit plus one for the cover (20 MiB), the LRC (2 MiB) and the attachment
  (256 MiB), both declared (a request head whose body is never sent) and
  streamed, and exactly the limit accepted (a 20 MiB PNG on MP3 and M4A
  albums; 2 MiB of LRC; 256 MiB of attachment, streamed from a generator);
  N-091 per format at its boundary (16,777,174 bytes on FLAC); invalid
  UTF-8 LRC (Latin-1, a lone continuation byte, an encoded surrogate, a cut
  rune); paths refused before the body is read; 409 collisions naming
  both; 507 for both upload kinds with the budget held (deterministic:
  a reservation against an inflated free space, N-172), then 201 and 200
  once released; the declared Content-Length reserved while a 5 MiB body
  is received through a pipe, and released after
  (`TestUploadReservesItsLength`); a lyrics attachment refused on the
  snapshot without reading its blob (`TestLyricsAttachmentPrecheck`);
  `+` in `?path=` a space and `%2B` a plus (`TestAttachmentPathPlus`); a malformed chunked body 400
  `upload_incomplete`; the failpoint between the put and the
  transaction for the three uploads; 15 rounds of two concurrent cover
  uploads; the boundary (header, Origin) on every mutation; 405 with
  `Allow`; downloads: bytes, types, `inline` versus `attachment`,
  `nosniff`, `no-store`, HEAD, a range, file names with quotes, a
  backslash, CR/LF, `;`, `%` and non-ASCII decoded back by
  `mime.ParseMediaType`, 404 for every id of another album, 500 for a
  missing blob; `TestContentDisposition` (nine names); `TestStatusTable`.
- **end to end** (`cmd/musiclibd/editor_test.go`, the real server with two
  workers): `TestEndToEndEditorContent` imports a FLAC, an MP3 and an M4A
  track, then through the API: the originals downloaded byte for byte; a
  cover uploaded, published as `cover.jpg` and embedded in the three files
  (read back by the helper); removed, gone from the files and from the
  tags; an attachment published under `Extras/`, downloaded as a PDF,
  removed; lyrics uploaded and an `.lrc` attachment assigned, each next to
  its track (the attachment kept); lyrics removed; the M4A and MP3 tracks
  removed, the last refused; after every change the receipt and the
  directory agree exactly, and `work/` is empty.
  `TestM4ACoverRemovalResidual`: N-168's residual as accepted (N-181).

**Mutation-checked** (N-182): the If-Match comparison of the transaction;
the snapshot precheck; each size limit by one byte; the one-track minimum;
the ownership of the downloads' ids; the album condition in three
queries; N-091 in the transaction and in the precheck; the collision check
in both places; the cover validation; the LRC UTF-8 check; the budget
release; an upload reserving 0 instead of its size; the lyrics
attachment's track and `.lrc` prechecks; inline for every format; the cover no-op; Content-Type sniffing;
the file-name escaping, twice; a read error taken for an invalid image.

```sh
scripts/check.sh
scripts/dev.sh go test -race -count=2 -timeout=15m ./internal/http/ ./internal/catalog/ ./internal/media/ ./internal/importer/ ./cmd/musiclibd/
scripts/dev.sh go test -race -count=2 -run 'TestEndToEndEditorContent|TestM4ACoverRemovalResidual' ./cmd/musiclibd/
```

### The rest of the §10.2 API: library list, imports, queue, retries (§6.1, §6.4, §7.1–§7.3, §10.1, §10.2, §10.4) ✔ (round 16)

The endpoints of §10.2 still missing after round 14, over the existing
catalog, queue and importer. Handlers validate and translate; every write
is one transaction of `internal/catalog` (§13.2); no SQL and no absolute
path in `internal/http`. Decisions N-190 to N-201, mutation checks N-202.

| Endpoint | Answer |
|---|---|
| `GET /api/albums?q=&artist=&trash=&limit=&after=` | album summaries in the library's order (artist key, title key, id), a page of 1..200 (default 50; 201 is 422), `next` an opaque keyset cursor (N-190); `q` searches titles and artist names with `names.Key` as a substring, no accent folding (N-191, owner decision 2026-09-26); a cursor key with NUL or invalid UTF-8 is 422 |
| `GET /api/import-source?path=` | the entries of a directory under `/import`, sorted by name bytes, typed (`directory`, `file`, `symlink`, `special`, `invalid_name`); symlinks never followed; 422 for an invalid path or a path through a symlink, 404 missing (N-197) |
| `POST /api/imports` `{id, path}` | the batch and its scan job in one transaction: 201 + `Location`, 200 for the same request again, 409 `import_batch_conflict` for the same id with another path (N-201) |
| `GET /api/imports/{id}` | the report: `scanning`/`importing`/`completed`, the scan (its warnings: unassigned files, rejected entries), each candidate's job with its state, typed error, warnings, overrides and result album (N-193) |
| `GET /api/jobs?state=&kind=&limit=&after=` | pending, running and failed jobs by id (N-194), messages through N-150 |
| `POST /api/jobs/{id}/retry` (body empty or `{artist, title}`) | a failed job gets a new ticket; pending or running: unchanged; done or skipped: 409; overrides for an import only, strict (N-195); no If-Match (N-196); 202 with the job |
| `POST /api/jobs/retry-failed` | every failed job, running ones untouched; 202 `{retried}` |
| `POST /api/render-all` | every active album and every trashed one still published, through `jobs.EnqueueRender`; 202 `{enqueued}` (N-198) |

**Pieces:**
- `internal/jobs/retry.go`: `Retry`, `RetryFailed`, `Overrides.Equal`, the
  codes `job_not_retryable`, `job_in_progress`, `job_overrides_not_allowed`.
- `internal/catalog/queue.go`: `JobView`, `ListJobs`, `GetJob`,
  `ImportReport` (`State()`), `GetImportReport`, `RetryJob` (overrides
  normalized, §5.2), `RetryFailed`, `RenderAll`, `PurgeImportReports`,
  `DefaultPageSize`, `MaxPageSize`, `ReportRetentionDays`, `CodeJobNotFound`.
- `internal/catalog/search.go`: `ListAlbums`, `AlbumFilter`, `AlbumCursor`,
  `AlbumSummary`, `AlbumPage`.
- `internal/importer/browse.go`: `Browse`, `SourceEntry`, the entry types,
  `DisplayName`, `CodeSourceNotReadable`.
- `internal/http`: `albumlist.go`, `queue.go`, `query.go` (strict query
  strings, `limit`), the routes, the new codes in `statusOf`, `translate`
  for the queue's typed errors, `Backend.Source`; `upload.go`:
  `MaxConcurrentUploads`, `uploadSlot` and the failpoints `upload_waiting`,
  `upload_copying` (N-199).
- `cmd/musiclibd`: `/import` compared with `/data` and its media
  directories at step 3 (`import_is_data`, N-197); the catalog built at
  step 5 and the report retention run there and daily (N-200); the API
  enabled with `/import`.
- SQL: ten queries in `sql/jobs.sql`, one in `sql/catalog.sql`; no schema
  change, no new index (N-190).

**How it maps to DESIGN.md:**
- **§10.2 GET /api/albums:** search by title and artist, artist and trash
  filters, pages of 50 up to 200; deterministic order; one snapshot.
- **§5.2:** the search compares with the one normalization (`names.Key`),
  never SQL `lower()`; the import root is validated and kept as on disk.
- **§7.1:** `/import` only, relative paths, no symlink followed, the root
  never `/data`; the request UUID makes creation idempotent, 409 for other
  parameters, batch and scan job in one transaction.
- **§7.2:** the report per candidate, the unassigned files, "no valid
  candidate" completed with the scan's explanation; a retry revalidates
  the current candidate.
- **§7.3:** the overrides `{artist, title}` only, through the retry.
- **§6.3, §10.2:** retries and render-all through the single enqueue, one
  row per album, no catalog change, idempotent while pending or running;
  retry-failed never duplicates a running job.
- **§6.1:** two upload copies at once.
- **§6.4:** the report outcomes kept 90 days, batches with a job to run
  never deleted.
- **§10.1, §10.4:** strict JSON and queries, typed errors, `nosniff`,
  `no-store`, the boundary on every mutation, no absolute path.

**Tests** (real PostgreSQL 17, real ext4, the real tools; no mock):
- **jobs** (`retry_test.go`): `TestRetryImport` (new ticket, outcome
  cleared, overrides kept, replaced, cleared; pending and running
  unchanged; other overrides `job_in_progress`; non-normalized refused),
  `TestRetryRefusals` (done, skipped, overrides on a scan and a render,
  unknown id, a scan retried), `TestRetryRender` (the album's one row),
  `TestRetryFailed` (exactly the three failed jobs; running, pending and
  done rows byte-equal, `updated_at` included; a second call retries
  nothing).
- **catalog** (`search_test.go`, `queue_test.go`): the search cases (case,
  NFD, `ß`, no accent folding, `AC/DC` and a DOS name searched as text,
  the Cherokee fold, control characters and 1,025 characters refused, the
  limits); filters and a tie of two trashed albums paged one by one; 1,100
  albums paged at 1, 7, 50 and 200 with and without a search across the
  batch boundary, equal to the reference order; a cursor whose album left
  the list; the report's states, order and no-valid-candidate case;
  `ListJobs` pages and filters; `RetryJob` normalization and wake-ups;
  `RenderAll` (six albums, the running render keeps its claim with a newer
  ticket, the failed one pending, no revision, one row each after two
  runs); `PurgeImportReports` with fabricated timestamps, each of its
  three conditions (batch age, outcome age, no job to run) tested alone.
- **http** (`imports_test.go`, `jobs_test.go`, `slots_test.go`): every
  endpoint's answers and refusals (22 import-source cases on a real tree
  with symlinks to `/data`, outside and inside, a FIFO, an invalid name,
  an unreadable directory; the list query refusals, among them four cursors
  holding NUL or invalid UTF-8 and a search text with NUL; 9 import body
  refusals; 12 retry body refusals), no absolute path in any answer;
  eight concurrent `POST /api/imports` with one id, five rounds;
  `TestRetryAnswerLost` (the COMMIT answer of a retry and of an import
  lost through `pgtest.Proxy`, the request repeated after the restart);
  the upload slots (N-199).
- **end to end** (`cmd/musiclibd/imports_test.go`,
  `TestEndToEndImportThroughAPI`, the real server with two workers and
  real FLAC files): the listing, the batch (201, then 200), the real scan
  and imports, the report (a good album, `mixed_album`, an ambiguous
  branch, an unassigned file), `mixed_album` fixed by a retry with a title,
  the ambiguous branch retried unchanged (fails again) then fixed on disk
  (imports), a done job 409, a batch without a valid candidate completed
  with its explanation, the library list, render-all publishing every
  album again. `TestEndToEndTwoWorkers` creates its batch through the API.
  `TestBootRefusals` (`import_is_data`, three cases),
  `TestImportIsNotDataStatFailure` (a data directory that cannot be
  described is `import_unavailable`), `TestBootStepsInOrder` (the purge at
  step 5).

**Mutation-checked (N-202):** batch idempotency and 409; the retry's
states, new ticket and overrides whitelist; retry-failed on running jobs;
render-all's scope both ways; the three retention conditions; the upload
slot's capacity, each upload path, the context; the page limit in both
layers; the search key and texts; the cursor id across a search batch; a
symlink shown as a directory; the boot's `/import` identity check; the
cursor's NUL check; the boot code of a data directory's `Stat` failure.

```sh
scripts/check.sh
scripts/dev.sh go test -race -count=2 ./internal/http/ ./internal/catalog/ ./internal/jobs/ ./internal/importer/ ./cmd/musiclibd/
scripts/dev.sh go test -race -count=5 -run 'TestUploadSlot|TestCreateImportConcurrently|TestRetry' ./internal/http/ ./internal/jobs/
```

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
5. **Dismissed and superseded failures (owner, round 21).** §4.2's normative
   schema gains `jobs.dismissed_at` (migration 00002, with the SQL functions
   `job_superseded` and `job_needs_attention`), §10.2 gains `POST
   /api/jobs/{id}/dismiss`, and §6.4's «Riprova falliti» leaves dismissed and
   superseded failures alone (NOTES.md N-285).
6. **Orphan artists and the new artist of a save (owner, round 22).** An
   artist left without albums by a save is deleted in that save's
   transaction, beyond §4.3's «non vengono mostrati» (N-297); §10.2's PUT
   body gains the required key `new_artist`, the name of an artist created
   by the save (N-298).
7. **Track durations (owner, round 22).** §4.2's `blobs` gains
   `duration_ms` (migration 00003), informational, recorded at import and
   by renders (N-300, N-301); the album JSON's tracks carry it (N-302).

## Open questions for the spec

See `NOTES.md`. The most urgent ones, because they change keys already written
to disk:

- **N-002** trimming the leading dot: `.hidden` becomes `hidden`. To be
  confirmed **before** the first real import.
