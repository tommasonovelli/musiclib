-- Round 21 (owner, 2026-09-28; NOTES.md N-285): a failed scan or import stops
-- needing attention once the user dismisses it, or once a later import of its
-- source succeeds. A refinement of DESIGN.md §6.4, whose failures otherwise
-- stay visible with «Riprova» until the 90-day purge (N-200). Neither changes
-- the job's state, outcome, ticket or updated_at: the retention of §6.4 and
-- the catalog lock of every transition back to pending are unchanged.

-- +goose Up

-- When the user dismissed a failed scan or import (POST /api/jobs/{id}/dismiss).
-- A render is the album's own state and cannot be dismissed; a retry clears it.
ALTER TABLE jobs ADD COLUMN dismissed_at timestamptz;
ALTER TABLE jobs ADD CONSTRAINT jobs_dismissed_check
    CHECK (dismissed_at IS NULL OR (state = 'failed' AND kind IN ('scan', 'import')));

-- job_superseded: the failed scan or import is moot, because an import
-- requested after its last attempt (a larger ticket, whatever the clock) is
-- done or skipped (the album is in the library, or already was):
--   - of the same source: the import's candidate, the scan's batch root;
--   - or of a path under it, for a scan (it covers the whole folder) and for
--     the structural failures whose remedy is to import the subfolders
--     (not_a_candidate, ambiguous_candidate, no_valid_candidate).
-- Paths are compared as on disk, byte for byte (§5.2); '' is /import itself,
-- under which every path is. The table of codes is NOTES.md N-285.
-- +goose StatementBegin
CREATE FUNCTION job_superseded(failed_id uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1
        FROM jobs f
        LEFT JOIN import_batches b ON b.id = f.batch_id
        CROSS JOIN LATERAL (SELECT coalesce(f.source_rel, b.root_rel) AS path) src
        JOIN jobs later ON later.kind = 'import' AND later.state IN ('done', 'skipped')
            AND later.requested > f.requested
            AND (later.source_rel = src.path
                 OR ((f.kind = 'scan'
                      OR f.error_code IN ('not_a_candidate', 'ambiguous_candidate', 'no_valid_candidate'))
                     AND (src.path = '' OR starts_with(later.source_rel, src.path || '/'))))
        WHERE f.id = failed_id AND f.state = 'failed' AND f.kind IN ('scan', 'import'))
$$;
-- +goose StatementEnd

-- job_needs_attention: a failed job the UI lists under «Needs attention» and
-- «Retry all» retries. A failed render always does (it is the album's
-- state). A failed scan or import does unless it is dismissed or superseded;
-- and a scan that found no valid candidate is left out while its batch has
-- import jobs, whose own rows say what went wrong (one problem, one row).
-- +goose StatementBegin
CREATE FUNCTION job_needs_attention(job_id uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM jobs j
        WHERE j.id = job_id AND j.state = 'failed'
          AND (j.kind = 'render' OR (
              j.dismissed_at IS NULL
              AND NOT job_superseded(j.id)
              AND NOT (j.kind = 'scan' AND j.error_code = 'no_valid_candidate'
                       AND EXISTS (SELECT 1 FROM jobs i WHERE i.batch_id = j.batch_id AND i.kind = 'import')))))
$$;
-- +goose StatementEnd
