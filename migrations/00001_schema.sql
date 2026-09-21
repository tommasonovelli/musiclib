-- DESIGN.md §4.2: the normative schema. Forward only (§11.4), so no Down section.
-- Single-column checks keep PostgreSQL's default name <table>_<column>_check;
-- multi-column invariants are named, so that a violation says which one failed.
-- Every FK is ON DELETE RESTRICT (§4.2) and has an index led by its column.
-- Hashes are SHA-256 as 64 lowercase hex digits; FKs to blobs inherit the check.

-- +goose Up

CREATE TABLE settings (
    id       smallint PRIMARY KEY CHECK (id = 1),
    store_id uuid NOT NULL UNIQUE
);

CREATE TABLE blobs (
    hash       text PRIMARY KEY CHECK (hash ~ '^[0-9a-f]{64}$'),
    size       bigint NOT NULL CHECK (size >= 0),
    -- NULL = any other content; never derived from the source extension (§4.2).
    format     text CHECK (format IN ('flac', 'mp3', 'm4a-aac', 'm4a-alac', 'jpeg', 'png')),
    created_at timestamptz NOT NULL
);

CREATE TABLE artists (
    id         uuid PRIMARY KEY,
    name       text NOT NULL,
    folder_key text NOT NULL UNIQUE,
    revision   bigint NOT NULL CHECK (revision > 0)
);

CREATE TABLE albums (
    id                     uuid PRIMARY KEY,
    artist_id              uuid NOT NULL REFERENCES artists ON DELETE RESTRICT,
    title                  text NOT NULL,
    folder_key             text NOT NULL,
    year                   integer CHECK (year BETWEEN 1 AND 9999),
    genre                  text,
    compilation            boolean NOT NULL DEFAULT false,
    cover_hash             text REFERENCES blobs ON DELETE RESTRICT,
    revision               bigint NOT NULL CHECK (revision > 0),
    deleted_at             timestamptz,
    import_fingerprint     text UNIQUE CHECK (import_fingerprint ~ '^[0-9a-f]{64}$'),
    published_path         text,
    published_revision     bigint NOT NULL DEFAULT 0,
    published_renderer     text,
    published_build        uuid,
    published_receipt_hash text CHECK (published_receipt_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT albums_published_revision_check CHECK (published_revision BETWEEN 0 AND revision),
    CONSTRAINT albums_published_renderer_check
        CHECK ((published_renderer IS NOT NULL) = (published_revision > 0)),
    -- Output present: path, build and receipt together. Never published: no output.
    -- A published revision without output is a completed deletion (§4.2).
    CONSTRAINT albums_published_output_check CHECK (
        num_nulls(published_path, published_build, published_receipt_hash) IN (0, 3)
        AND (published_path IS NULL OR published_revision > 0))
);

-- A trashed album keeps its row but frees its folder name (§4.2, §4.3).
CREATE UNIQUE INDEX albums_active_folder_key ON albums (artist_id, folder_key)
    WHERE deleted_at IS NULL;
CREATE INDEX albums_artist_id_idx ON albums (artist_id);
CREATE INDEX albums_cover_hash_idx ON albums (cover_hash);

CREATE TABLE tracks (
    id          uuid PRIMARY KEY,
    album_id    uuid NOT NULL REFERENCES albums ON DELETE RESTRICT,
    disc        integer NOT NULL CHECK (disc BETWEEN 1 AND 99),
    no          integer NOT NULL CHECK (no BETWEEN 1 AND 999),
    title       text NOT NULL,
    -- NULL inherits from the album (§4.1).
    artist      text,
    genre       text,
    blob_hash   text NOT NULL REFERENCES blobs ON DELETE RESTRICT,
    source_path text NOT NULL,
    lyrics_hash text REFERENCES blobs ON DELETE RESTRICT,
    -- Deferred: a reorder is one transaction without temporary numbers (§4.2, §12.2).
    CONSTRAINT tracks_album_disc_no_key UNIQUE (album_id, disc, no)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX tracks_blob_hash_idx ON tracks (blob_hash);
CREATE INDEX tracks_lyrics_hash_idx ON tracks (lyrics_hash);

CREATE TABLE attachments (
    id        uuid PRIMARY KEY,
    album_id  uuid NOT NULL REFERENCES albums ON DELETE RESTRICT,
    rel_path  text NOT NULL,
    path_key  text NOT NULL,
    blob_hash text NOT NULL REFERENCES blobs ON DELETE RESTRICT,
    UNIQUE (album_id, path_key)
);

CREATE INDEX attachments_blob_hash_idx ON attachments (blob_hash);

-- One row per normalized path: a path belongs to one album only (§5.3).
CREATE TABLE path_claims (
    path_key text PRIMARY KEY,
    path     text NOT NULL,
    album_id uuid NOT NULL REFERENCES albums ON DELETE RESTRICT
);

CREATE INDEX path_claims_album_id_idx ON path_claims (album_id);

CREATE TABLE import_batches (
    -- The client's idempotency key (§7.1): any UUID version, not only v7.
    id         uuid PRIMARY KEY,
    -- Relative to /import, exactly as on disk; '' is /import itself (§5.2).
    root_rel   text NOT NULL,
    created_at timestamptz NOT NULL
);

-- Job tickets (§2.1, §6.3): requested/claimed compare values of this sequence.
CREATE SEQUENCE job_ticket AS bigint;

CREATE TABLE jobs (
    id              uuid PRIMARY KEY,
    kind            text NOT NULL CHECK (kind IN ('scan', 'import', 'render')),
    album_id        uuid REFERENCES albums ON DELETE RESTRICT,
    batch_id        uuid REFERENCES import_batches ON DELETE RESTRICT,
    source_rel      text,
    overrides       jsonb NOT NULL DEFAULT '{}',
    requested       bigint NOT NULL DEFAULT nextval('job_ticket'),
    claimed         bigint,
    state           text NOT NULL
        CHECK (state IN ('pending', 'running', 'done', 'skipped', 'failed')),
    result_album_id uuid REFERENCES albums ON DELETE RESTRICT,
    error_code      text,
    error_message   text,
    warnings        jsonb NOT NULL DEFAULT '[]',
    queued_at       timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    -- Only running admits and requires a claim (§4.2).
    CONSTRAINT jobs_claimed_check CHECK ((claimed IS NOT NULL) = (state = 'running')),
    -- Column combinations per kind (§4.2).
    CONSTRAINT jobs_render_check CHECK (kind <> 'render' OR (
        album_id IS NOT NULL AND batch_id IS NULL AND source_rel IS NULL
        AND result_album_id IS NULL AND overrides = '{}')),
    CONSTRAINT jobs_render_state_check
        CHECK (kind <> 'render' OR state IN ('pending', 'running', 'failed')),
    CONSTRAINT jobs_scan_check CHECK (kind <> 'scan' OR (
        album_id IS NULL AND batch_id IS NOT NULL AND source_rel IS NULL
        AND result_album_id IS NULL AND overrides = '{}')),
    CONSTRAINT jobs_import_check CHECK (kind <> 'import' OR (
        album_id IS NULL AND batch_id IS NOT NULL AND source_rel IS NOT NULL)),
    -- {} or a subset of {artist, title} with string values (§7.3). CASE, not AND:
    -- the jsonb operators must not run on a non-object and raise a different error.
    CONSTRAINT jobs_overrides_check CHECK (CASE WHEN jsonb_typeof(overrides) = 'object' THEN
        overrides - 'artist' - 'title' = '{}'
        AND jsonb_typeof(coalesce(overrides -> 'artist', '""')) = 'string'
        AND jsonb_typeof(coalesce(overrides -> 'title', '""')) = 'string'
        ELSE false END),
    CONSTRAINT jobs_warnings_check CHECK (jsonb_typeof(warnings) = 'array')
);

-- One render row per album whatever its state (§4.2, §6.3).
CREATE UNIQUE INDEX jobs_render_album_key ON jobs (album_id) WHERE kind = 'render';
CREATE UNIQUE INDEX jobs_scan_batch_key ON jobs (batch_id) WHERE kind = 'scan';
CREATE UNIQUE INDEX jobs_import_source_key ON jobs (batch_id, source_rel) WHERE kind = 'import';
-- The claim order (§6.1).
CREATE INDEX jobs_state_kind_queued_idx ON jobs (state, kind, queued_at, id);
-- The partial unique indexes above cannot serve lookups without their predicate.
CREATE INDEX jobs_album_id_idx ON jobs (album_id);
CREATE INDEX jobs_batch_id_idx ON jobs (batch_id);
CREATE INDEX jobs_result_album_id_idx ON jobs (result_album_id);

-- The publication journal: zero or one row (§9.3). No index on album_id: one row.
CREATE TABLE publication (
    id           smallint PRIMARY KEY CHECK (id = 1),
    album_id     uuid NOT NULL REFERENCES albums ON DELETE RESTRICT,
    ticket       bigint NOT NULL,
    revision     bigint NOT NULL CHECK (revision > 0),
    renderer     text NOT NULL,
    build_id     uuid NOT NULL,
    receipt_hash text CHECK (receipt_hash ~ '^[0-9a-f]{64}$'),
    old_path     text,
    old_build    uuid,
    new_path     text,
    -- new_path NULL = removal from the library, the only case without a receipt.
    CONSTRAINT publication_receipt_check CHECK ((receipt_hash IS NULL) = (new_path IS NULL)),
    -- The old output is the album's published path and build, present together.
    CONSTRAINT publication_old_check CHECK ((old_path IS NULL) = (old_build IS NULL))
);
