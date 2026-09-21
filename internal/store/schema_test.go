package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"musiclib/internal/store/pgtest"
)

// SQLSTATEs of the integrity violations the schema must raise.
const (
	fk     = "23503"
	unique = "23505"
	check  = "23514"
)

// TestConstraints runs one representative violation per invariant of §4.2
// against the seeded rows, each in a rolled-back transaction with deferred
// constraints made immediate. An empty code marks a statement that must
// succeed: it proves that a constraint is not stricter than the spec.
func TestConstraints(t *testing.T) {
	pool := pgtest.New(t)
	r := seed(t, pool)
	insertAlbum := `INSERT INTO albums (id, artist_id, title, folder_key, revision) VALUES (gen_random_uuid(), '{artist}', 'B', `
	insertJob := `INSERT INTO jobs (id, queued_at, updated_at, kind, album_id, batch_id, source_rel, state)
		VALUES (gen_random_uuid(), now(), now(), `
	for _, tc := range []struct{ name, sql, code, constraint string }{
		{"settings is a single row", `INSERT INTO settings VALUES (2, gen_random_uuid())`, check, "settings_id_check"},

		{"hash is lowercase hex", `INSERT INTO blobs VALUES (upper('{other}'), 1, NULL, now())`, check, "blobs_hash_check"},
		{"blob size", `INSERT INTO blobs VALUES ('{other}', -1, NULL, now())`, check, "blobs_size_check"},
		{"blob format", `INSERT INTO blobs VALUES ('{other}', 1, 'ogg', now())`, check, "blobs_format_check"},
		{"referenced blob is never deleted", `DELETE FROM blobs WHERE hash = '{audio}'`, fk, "tracks_blob_hash_fkey"},

		{"artist revision", `UPDATE artists SET revision = 0`, check, "artists_revision_check"},
		{"artist folder key", `INSERT INTO artists VALUES (gen_random_uuid(), 'ARTIST', 'artist', 1)`, unique, "artists_folder_key_key"},

		{"album artist exists", `UPDATE albums SET artist_id = gen_random_uuid()`, fk, "albums_artist_id_fkey"},
		{"album year", `UPDATE albums SET year = 10000`, check, "albums_year_check"},
		{"album revision", `UPDATE albums SET revision = 0`, check, "albums_revision_check"},
		{"album cover is a blob", `UPDATE albums SET cover_hash = '{other}'`, fk, "albums_cover_hash_fkey"},
		{"active folder key per artist", insertAlbum + `'album', 1)`, unique, "albums_active_folder_key"},
		{"trashed album frees its folder key", `UPDATE albums SET deleted_at = now(); ` + insertAlbum + `'album', 1)`, "", ""},
		{"fingerprint format", `UPDATE albums SET import_fingerprint = 'x'`, check, "albums_import_fingerprint_check"},
		{"fingerprint unique", `INSERT INTO albums (id, artist_id, title, folder_key, revision, import_fingerprint)
			VALUES (gen_random_uuid(), '{artist}', 'B', 'b', 1, '{audio}')`, unique, "albums_import_fingerprint_key"},
		{"published revision at most revision", `UPDATE albums SET published_revision = 2, published_renderer = 'r'`, check, "albums_published_revision_check"},
		{"renderer iff published", `UPDATE albums SET published_revision = 1`, check, "albums_published_renderer_check"},
		{"published output is whole", `UPDATE albums SET published_revision = 1, published_renderer = 'r', published_path = 'A/B'`, check, "albums_published_output_check"},
		{"no output while unpublished", `UPDATE albums SET published_path = 'A/B', published_build = gen_random_uuid(),
			published_receipt_hash = '{audio}'`, check, "albums_published_output_check"},
		{"completed deletion has no output", `UPDATE albums SET deleted_at = now(), published_revision = 1, published_renderer = 'r'`, "", ""},
		{"receipt hash format", `UPDATE albums SET published_revision = 1, published_renderer = 'r', published_path = 'A/B',
			published_build = gen_random_uuid(), published_receipt_hash = 'x'`, check, "albums_published_receipt_hash_check"},

		{"disc range", `UPDATE tracks SET disc = 100 WHERE id = '{track}'`, check, "tracks_disc_check"},
		{"track number range", `UPDATE tracks SET no = 0 WHERE id = '{track}'`, check, "tracks_no_check"},
		{"track position unique", `UPDATE tracks SET no = 1`, unique, "tracks_album_disc_no_key"},
		{"lyrics are a blob", `UPDATE tracks SET lyrics_hash = '{other}'`, fk, "tracks_lyrics_hash_fkey"},

		{"attachment path key per album", `INSERT INTO attachments VALUES (gen_random_uuid(), '{album}', 'SCAN.pdf', 'scan.pdf', '{image}')`,
			unique, "attachments_album_id_path_key_key"},
		{"path claimed by one album", `INSERT INTO path_claims VALUES ('artist/album', 'ARTIST/Album', '{album}')`, unique, "path_claims_pkey"},
		{"claim album exists", `UPDATE path_claims SET album_id = gen_random_uuid()`, fk, "path_claims_album_id_fkey"},

		{"job kind", `UPDATE jobs SET kind = 'rebuild' WHERE id = '{scan}'`, check, "jobs_kind_check"},
		{"job state", `UPDATE jobs SET state = 'queued' WHERE id = '{scan}'`, check, "jobs_state_check"},
		{"running requires a claim", `UPDATE jobs SET state = 'running' WHERE id = '{render}'`, check, "jobs_claimed_check"},
		{"only running has a claim", `UPDATE jobs SET claimed = requested WHERE id = '{render}'`, check, "jobs_claimed_check"},
		{"render columns", `UPDATE jobs SET batch_id = '{batch}' WHERE id = '{render}'`, check, "jobs_render_check"},
		{"render states", `UPDATE jobs SET state = 'done' WHERE id = '{render}'`, check, "jobs_render_state_check"},
		{"one render row per album whatever the state", insertJob + `'render', '{album}', NULL, NULL, 'failed')`,
			unique, "jobs_render_album_key"},
		{"scan columns", `UPDATE jobs SET source_rel = 'a' WHERE id = '{scan}'`, check, "jobs_scan_check"},
		{"one scan per batch", insertJob + `'scan', NULL, '{batch}', NULL, 'pending')`, unique, "jobs_scan_batch_key"},
		{"import columns", `UPDATE jobs SET source_rel = NULL WHERE id = '{import}'`, check, "jobs_import_check"},
		{"one import per candidate", insertJob + `'import', NULL, '{batch}', 'a', 'pending')`, unique, "jobs_import_source_key"},
		{"another candidate", insertJob + `'import', NULL, '{batch}', '', 'pending')`, "", ""},
		{"override keys", `UPDATE jobs SET overrides = '{"year": "1999"}' WHERE id = '{import}'`, check, "jobs_overrides_check"},
		{"overrides are an object", `UPDATE jobs SET overrides = '"x"' WHERE id = '{import}'`, check, "jobs_overrides_check"},
		{"warnings are an array", `UPDATE jobs SET warnings = '{}' WHERE id = '{scan}'`, check, "jobs_warnings_check"},
		{"job batch exists", `UPDATE jobs SET batch_id = gen_random_uuid() WHERE id = '{scan}'`, fk, "jobs_batch_id_fkey"},

		{"journal is a single row", `INSERT INTO publication (id, album_id, ticket, revision, renderer, build_id)
			VALUES (2, '{album}', 1, 1, 'r', gen_random_uuid())`, check, "publication_id_check"},
		{"journal revision", `UPDATE publication SET revision = 0`, check, "publication_revision_check"},
		{"receipt iff new path", `UPDATE publication SET receipt_hash = NULL`, check, "publication_receipt_check"},
		{"removal has no receipt", `UPDATE publication SET receipt_hash = NULL, new_path = NULL`, "", ""},
		{"old path and build together", `UPDATE publication SET old_path = 'A/B'`, check, "publication_old_check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err := tx.Exec(t.Context(), "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(t.Context(), r.Replace(tc.sql))
			var pgErr *pgconn.PgError
			switch {
			case tc.code == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tc.code == "":
			case !errors.As(err, &pgErr):
				t.Fatalf("got %v, want SQLSTATE %s on %s", err, tc.code, tc.constraint)
			case pgErr.Code != tc.code || pgErr.ConstraintName != tc.constraint:
				t.Fatalf("got SQLSTATE %s on %q (%s), want %s on %s",
					pgErr.Code, pgErr.ConstraintName, pgErr.Message, tc.code, tc.constraint)
			}
		})
	}
}

// Swapping two track numbers is one transaction without temporary numbers
// (§4.2, §12.2); a duplicate left at commit is still refused.
func TestTrackReorderIsChecked(t *testing.T) {
	pool := pgtest.New(t)
	r := seed(t, pool)
	for _, tc := range []struct {
		name, sql string
		ok        bool
	}{
		{"swap", `UPDATE tracks SET no = 2 WHERE id = '{track}'; UPDATE tracks SET no = 1 WHERE id = '{track2}'`, true},
		{"duplicate at commit", `UPDATE tracks SET no = 1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			// A no-op after Commit; otherwise it returns the connection that
			// pool.Close would wait for forever.
			defer tx.Rollback(t.Context())
			if _, err := tx.Exec(t.Context(), r.Replace(tc.sql)); err != nil {
				t.Fatalf("checked before commit: %v", err)
			}
			err = tx.Commit(t.Context())
			var pgErr *pgconn.PgError
			if tc.ok && err != nil ||
				!tc.ok && (!errors.As(err, &pgErr) || pgErr.ConstraintName != "tracks_album_disc_no_key") {
				t.Fatalf("commit = %v, want ok=%t", err, tc.ok)
			}
		})
	}
}

// Every FK restricts deletion (§4.2: no implicit destructive cascade) and is
// the leading column of a full index (§4.2: indexes on the FKs). The
// single-row journal is exempt from the index.
func TestForeignKeysRestrictAndAreIndexed(t *testing.T) {
	pool := pgtest.New(t)
	rows, err := pool.Query(t.Context(), `
SELECT c.conname, c.confdeltype = 'r',
       c.conrelid = 'publication'::regclass OR EXISTS (
           SELECT 1 FROM pg_index i
           WHERE i.indrelid = c.conrelid AND i.indkey[0] = c.conkey[1] AND i.indpred IS NULL)
FROM pg_constraint c
WHERE c.contype = 'f' AND c.connamespace = 'public'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for rows.Next() {
		var name string
		var restrict, indexed bool
		if err := rows.Scan(&name, &restrict, &indexed); err != nil {
			t.Fatal(err)
		}
		n++
		if !restrict || !indexed {
			t.Errorf("%s: ON DELETE RESTRICT %t, indexed %t", name, restrict, indexed)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no foreign keys found")
	}
}
