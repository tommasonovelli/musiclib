package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// The renderer the tests claim for.
const testRenderer = "rv-test-1"

// fixture is a migrated database with helpers that write catalog rows by
// SQL: package jobs cannot import the catalog, and the queue must work on
// whatever rows the catalog wrote.
type fixture struct {
	t  *testing.T
	db *pgxpool.Pool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, db: pgtest.New(t)}
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

var hashSeq atomic.Int64

// newHash returns a fresh SHA-256 in blobs' format.
func newHash() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "blob-%d-%d", time.Now().UnixNano(), hashSeq.Add(1)))
	return hex.EncodeToString(sum[:])
}

func (f *fixture) blob(format string) string {
	f.t.Helper()
	h := newHash()
	var fp *string
	if format != "" {
		fp = &format
	}
	f.exec(`INSERT INTO blobs (hash, size, format, created_at) VALUES ($1, 1000, $2, now())`, h, fp)
	return h
}

// album inserts an artist, an album with one track and returns the album.
func (f *fixture) album(title string) uuid.UUID {
	f.t.Helper()
	artist, album := store.NewID(), store.NewID()
	f.exec(`INSERT INTO artists (id, name, folder_key, revision) VALUES ($1, $2, $2, 1)`, artist, "artist "+album.String())
	f.exec(`INSERT INTO albums (id, artist_id, title, folder_key, revision) VALUES ($1, $2, $3, $3, 1)`, album, artist, title)
	f.exec(`INSERT INTO tracks (id, album_id, disc, no, title, blob_hash, source_path) VALUES ($1, $2, 1, 1, 'one', $3, '01.flac')`,
		store.NewID(), album, f.blob("flac"))
	return album
}

func (f *fixture) inTx(fn func(tx *store.CatalogTx) error) {
	f.t.Helper()
	if err := store.InCatalogTx(context.Background(), f.db, fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) enqueue(albumID uuid.UUID) Enqueued {
	f.t.Helper()
	var e Enqueued
	f.inTx(func(tx *store.CatalogTx) error {
		var err error
		e, err = EnqueueRender(context.Background(), tx, albumID)
		return err
	})
	return e
}

func (f *fixture) batch() uuid.UUID {
	f.t.Helper()
	id := store.NewID()
	f.exec(`INSERT INTO import_batches (id, root_rel, created_at) VALUES ($1, 'incoming', now())`, id)
	return id
}

// importJob inserts a pending import job queued at queuedAt.
func (f *fixture) importJob(batch uuid.UUID, source string, queuedAt time.Time) uuid.UUID {
	f.t.Helper()
	id := store.NewID()
	f.exec(`INSERT INTO jobs (id, kind, batch_id, source_rel, state, queued_at, updated_at)
		VALUES ($1, 'import', $2, $3, 'pending', $4, $4)`, id, batch, source, queuedAt)
	return id
}

func (f *fixture) scanJob(batch uuid.UUID, queuedAt time.Time) uuid.UUID {
	f.t.Helper()
	id := store.NewID()
	f.exec(`INSERT INTO jobs (id, kind, batch_id, state, queued_at, updated_at) VALUES ($1, 'scan', $2, 'pending', $3, $3)`,
		id, batch, queuedAt)
	return id
}

func (f *fixture) job(id uuid.UUID) store.Job {
	f.t.Helper()
	j, err := store.New(f.db).GetJob(context.Background(), id)
	if err != nil {
		f.t.Fatalf("job %s: %v", id, err)
	}
	return j
}

func (f *fixture) jobExists(id uuid.UUID) bool {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE id = $1`, id).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n == 1
}

func (f *fixture) claim() *Claim {
	f.t.Helper()
	c, err := ClaimNext(context.Background(), f.db, testRenderer)
	if err != nil {
		f.t.Fatalf("ClaimNext: %v", err)
	}
	return c
}

// setHook installs testHook for the test. Tests that use it do not run in
// parallel.
func setHook(t *testing.T, fn func(point string)) {
	t.Helper()
	testHook = fn
	t.Cleanup(func() { testHook = nil })
}

func ptr[T any](v T) *T { return &v }
