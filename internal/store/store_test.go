package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/migrations"
)

func TestNewPoolSize(t *testing.T) {
	for _, tc := range []struct{ workers, maxConns int }{{1, 9}, {16, 24}} {
		pool, err := store.NewPool(t.Context(), "postgres://u@localhost/db", tc.workers)
		if err != nil {
			t.Fatalf("workers=%d: %v", tc.workers, err)
		}
		if got := pool.Config().MaxConns; int(got) != tc.maxConns {
			t.Errorf("workers=%d: MaxConns = %d, want %d", tc.workers, got, tc.maxConns)
		}
		pool.Close()
	}
}

func TestNewID(t *testing.T) {
	prev := store.NewID()
	for range 10000 {
		id := store.NewID()
		if id.Version() != 7 || id.Variant().String() != "RFC4122" {
			t.Fatalf("%s: version %d, variant %s; want a v7 RFC 4122 UUID", id, id.Version(), id.Variant())
		}
		if bytes.Compare(prev[:], id[:]) >= 0 {
			t.Fatalf("%s after %s: not strictly increasing", id, prev)
		}
		prev = id
	}
}

// Concurrent boots on an empty database: every call succeeds and each
// migration is applied exactly once; a later call is a no-op.
func TestMigrateConcurrentAndIdempotent(t *testing.T) {
	pool := pgtest.Pool(t, pgtest.EmptyDB(t))
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range cap(errs) {
		wg.Go(func() { errs <- store.Migrate(t.Context(), pool) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Migrate: %v", err)
		}
	}
	if err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	var applied int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM goose_db_version WHERE version_id > 0`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != len(files) {
		t.Errorf("%d migrations recorded, want %d (one per file)", applied, len(files))
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	pool := pgtest.New(t)
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (99999999, true)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(t.Context(), pool); store.Code(err) != store.CodeSchemaTooNew {
		t.Fatalf("Migrate = %v, want %s", err, store.CodeSchemaTooNew)
	}
}

func TestStoreIDFirstInit(t *testing.T) {
	q := store.New(pgtest.New(t))
	if _, err := q.GetStoreID(t.Context()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetStoreID before init: %v, want no rows", err)
	}
	// The second init, as after a crash between the commit and the volume
	// marker (§11.1), must keep the first identity.
	first := store.NewID()
	for i, candidate := range []uuid.UUID{first, store.NewID()} {
		n, err := q.InsertStoreID(t.Context(), candidate)
		if err != nil || n != int64(1-i) {
			t.Fatalf("InsertStoreID #%d = %d, %v; want %d rows", i+1, n, err, 1-i)
		}
		if got, err := q.GetStoreID(t.Context()); err != nil || got != first {
			t.Fatalf("GetStoreID after init #%d = %s, %v; want %s", i+1, got, err, first)
		}
	}
}

// hash is a valid blob name for the fixtures.
func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// seed inserts one valid row of every table, which already proves the
// happy path of every constraint, and returns the placeholder replacer
// shared by the statements of the tests.
func seed(t *testing.T, pool *pgxpool.Pool) *strings.Replacer {
	t.Helper()
	ids := map[string]string{"{audio}": hash("audio"), "{image}": hash("image"), "{other}": hash("other")}
	for _, k := range []string{"{artist}", "{album}", "{track}", "{track2}", "{batch}", "{render}", "{scan}", "{import}", "{build}"} {
		ids[k] = store.NewID().String()
	}
	var pairs []string
	for k, v := range ids {
		pairs = append(pairs, k, v)
	}
	r := strings.NewReplacer(pairs...)
	exec(t, pool, r.Replace(`
INSERT INTO settings VALUES (1, '{build}');
INSERT INTO blobs (hash, size, format, created_at) VALUES ('{audio}', 10, 'flac', now()), ('{image}', 5, NULL, now());
INSERT INTO artists VALUES ('{artist}', 'Artist', 'artist', 1);
INSERT INTO albums (id, artist_id, title, folder_key, revision, import_fingerprint)
    VALUES ('{album}', '{artist}', 'Album', 'album', 1, '{audio}');
INSERT INTO tracks (id, album_id, disc, no, title, blob_hash, source_path) VALUES
    ('{track}', '{album}', 1, 1, 'One', '{audio}', 'a/01.flac'),
    ('{track2}', '{album}', 1, 2, 'Two', '{audio}', 'a/02.flac');
INSERT INTO attachments VALUES (gen_random_uuid(), '{album}', 'Scan.pdf', 'scan.pdf', '{image}');
INSERT INTO path_claims VALUES ('artist/album', 'Artist/Album', '{album}');
INSERT INTO import_batches VALUES ('{batch}', '', now());
INSERT INTO jobs (id, kind, album_id, batch_id, source_rel, overrides, state, result_album_id, queued_at, updated_at) VALUES
    ('{render}', 'render', '{album}', NULL, NULL, '{}', 'pending', NULL, now(), now()),
    ('{scan}', 'scan', NULL, '{batch}', NULL, '{}', 'done', NULL, now(), now()),
    ('{import}', 'import', NULL, '{batch}', 'a', '{"artist": "A"}', 'done', '{album}', now(), now());
INSERT INTO publication (id, album_id, ticket, revision, renderer, build_id, receipt_hash, new_path)
    VALUES (1, '{album}', 1, 1, 'r1', '{build}', '{audio}', 'Artist/Album');
`))
	return r
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}
