-- Round 22 (owner, 2026-09-28; NOTES.md N-300): the duration of an audio
-- original, shown read-only in the album's track table. A deviation from the
-- normative §4.2, by owner decision, and schema only: no data is migrated.
--
-- It is a fact of the blob's content, like its size and format: a blob is
-- content-addressed (§3.1), so the value never changes once known, and two
-- tracks sharing a blob share it. It is informational only: nothing that
-- decides the output reads it (not the render's plan, AudioDigest, the
-- fingerprint of §7.6 or the receipt of §9.2).
--
-- NULL is unknown: a blob recorded before this migration, or one whose
-- container declares no duration. The import records it from the probe it
-- already runs; a render records it for a blob still unknown (N-301), so
-- «Rebuild the library folder» fills every active album. Once known it is
-- never overwritten.

-- +goose Up

ALTER TABLE blobs ADD COLUMN duration_ms bigint;
-- Only an audio blob has one. A NULL format would make IN unknown, which a
-- CHECK accepts: hence the explicit IS NOT NULL.
ALTER TABLE blobs ADD CONSTRAINT blobs_duration_check
    CHECK (duration_ms IS NULL OR (duration_ms >= 0 AND format IS NOT NULL
                                   AND format IN ('flac', 'mp3', 'm4a-aac', 'm4a-alac')));
