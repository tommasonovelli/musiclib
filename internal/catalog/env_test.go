package catalog_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// env is a migrated database with a catalog service on it. The helpers
// that write by SQL stand for the parts of the system that do not exist
// yet (the importer's scan, the publisher's FINALIZE).
type env struct {
	t     *testing.T
	db    *pgxpool.Pool
	svc   *catalog.Service
	wakes atomic.Int32
	batch uuid.UUID

	mu sync.Mutex
	// fits is the cover check the service uses (N-091); nil accepts.
	fits func(cover catalog.Blob, format string) error
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvOn(t, pgtest.New(t))
}

func newEnvOn(t *testing.T, db *pgxpool.Pool) *env {
	t.Helper()
	e := &env{t: t, db: db}
	e.svc = e.service(db)
	e.batch = store.NewID()
	e.exec(`INSERT INTO import_batches (id, root_rel, created_at) VALUES ($1, 'incoming', now())`, e.batch)
	return e
}

// service returns a catalog service on db sharing e's wake counter and
// cover check.
func (e *env) service(db *pgxpool.Pool) *catalog.Service {
	e.t.Helper()
	svc, err := catalog.New(db, func() { e.wakes.Add(1) }, func(cover catalog.Blob, format string) error {
		e.mu.Lock()
		fits := e.fits
		e.mu.Unlock()
		if fits == nil {
			return nil
		}
		return fits(cover, format)
	}, func(genre, format string) error {
		// The real rule of N-162, as importer.GenreFits applies it.
		if format == catalog.FormatMP3 && !media.MP3GenreWritable(genre) {
			return catalog.GenreNotWritable(genre, format)
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return svc
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

var hashSeq atomic.Int64

func newHash() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d-%d", time.Now().UnixNano(), hashSeq.Add(1)))
	return hex.EncodeToString(sum[:])
}

func ptr[T any](v T) *T { return &v }

// runningImport inserts an import job and claims it by SQL, as the pool
// would: the attempt the importer hands to the commit.
func (e *env) runningImport() jobs.Attempt {
	e.t.Helper()
	id := store.NewID()
	e.exec(`INSERT INTO jobs (id, kind, batch_id, source_rel, state, queued_at, updated_at)
		VALUES ($1, 'import', $2, $3, 'pending', now(), now())`, id, e.batch, "src-"+id.String())
	return e.claimJob(id)
}

func (e *env) claimJob(id uuid.UUID) jobs.Attempt {
	e.t.Helper()
	var ticket int64
	if err := e.db.QueryRow(context.Background(),
		`UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1 AND state = 'pending' RETURNING claimed`,
		id).Scan(&ticket); err != nil {
		e.t.Fatalf("claiming %s: %v", id, err)
	}
	return jobs.Attempt{JobID: id, Ticket: ticket}
}

// candidate is a complete two-track album: an LRC for the first track, a
// cover that is also kept as an attachment (§7.4), a booklet.
func candidate(a jobs.Attempt, artist, title string) catalog.ImportCandidate {
	audio1, audio2, lrc, cover, booklet := newHash(), newHash(), newHash(), newHash(), newHash()
	return catalog.ImportCandidate{
		Attempt:     a,
		Fingerprint: newHash(),
		Blobs: []catalog.Blob{
			{Hash: audio1, Size: 30_000_000, Format: catalog.FormatFLAC},
			{Hash: audio2, Size: 25_000_000, Format: catalog.FormatFLAC},
			{Hash: lrc, Size: 2_000},
			{Hash: cover, Size: 500_000, Format: catalog.FormatJPEG},
			{Hash: booklet, Size: 1_000_000},
		},
		Artist:    artist,
		Title:     title,
		Year:      ptr(1959),
		Genre:     ptr("Jazz"),
		CoverHash: &cover,
		Tracks: []catalog.ImportTrack{
			{SourcePath: "CD1/01 So What.flac", Disc: 1, No: 1, Title: "So What", BlobHash: audio1,
				Lyrics: &catalog.ImportLyrics{SourcePath: "CD1/01 so what.LRC", BlobHash: lrc}},
			{SourcePath: "CD1/02 Freddie.flac", Disc: 1, No: 2, Title: "Freddie Freeloader",
				Artist: ptr("Miles & Cannonball"), Genre: ptr(""), BlobHash: audio2},
		},
		Attachments: []catalog.ImportAttachment{
			{RelPath: "Scans/Booklet.pdf", BlobHash: booklet},
			{RelPath: "cover.jpg", BlobHash: cover},
		},
		Warnings: []jobs.Warning{{Code: jobs.WarnYearDiscordant, Message: "1959 and 1960"}},
	}
}

func (e *env) commit(c catalog.ImportCandidate) catalog.ImportOutcome {
	e.t.Helper()
	out, err := e.svc.CommitImport(context.Background(), c)
	if err != nil {
		e.t.Fatalf("CommitImport: %v", err)
	}
	return out
}

// importAlbum imports a new album and returns its id.
func (e *env) importAlbum(artist, title string) uuid.UUID {
	e.t.Helper()
	out := e.commit(candidate(e.runningImport(), artist, title))
	if out.State != jobs.StateDone {
		e.t.Fatalf("import of %s/%s: %+v", artist, title, out)
	}
	return out.AlbumID
}

func (e *env) album(id uuid.UUID) store.Album {
	e.t.Helper()
	a, err := store.New(e.db).GetAlbum(context.Background(), id)
	if err != nil {
		e.t.Fatalf("album %s: %v", id, err)
	}
	return a
}

func (e *env) artist(id uuid.UUID) store.Artist {
	e.t.Helper()
	a, err := store.New(e.db).GetArtist(context.Background(), id)
	if err != nil {
		e.t.Fatalf("artist %s: %v", id, err)
	}
	return a
}

func (e *env) job(id uuid.UUID) store.Job {
	e.t.Helper()
	j, err := store.New(e.db).GetJob(context.Background(), id)
	if err != nil {
		e.t.Fatalf("job %s: %v", id, err)
	}
	return j
}

// claims returns the album's claims as key -> path.
func (e *env) claims(album uuid.UUID) map[string]string {
	e.t.Helper()
	rows, err := store.New(e.db).ListAlbumClaims(context.Background(), album)
	if err != nil {
		e.t.Fatal(err)
	}
	m := map[string]string{}
	for _, r := range rows {
		m[r.PathKey] = r.Path
	}
	return m
}

// renderJob returns the album's render row, if any.
func (e *env) renderJob(album uuid.UUID) (store.Job, bool) {
	e.t.Helper()
	var id uuid.UUID
	err := e.db.QueryRow(context.Background(), `SELECT id FROM jobs WHERE kind = 'render' AND album_id = $1`, album).Scan(&id)
	if err != nil {
		return store.Job{}, false
	}
	return e.job(id), true
}

func (e *env) clearRenders() { e.exec(`DELETE FROM jobs WHERE kind = 'render'`) }

func (e *env) reconcile(album uuid.UUID) error {
	e.t.Helper()
	return store.InCatalogTx(context.Background(), e.db, func(tx *store.CatalogTx) error {
		return catalog.ReconcileClaims(context.Background(), tx, album)
	})
}

// published records a completed publication at path, as FINALIZE will
// (§9.3 C): the published columns, then the claims realigned.
func (e *env) published(album uuid.UUID, path string) {
	e.t.Helper()
	e.exec(`UPDATE albums SET published_path = $2, published_revision = revision, published_renderer = 'rv-test',
		published_build = $3, published_receipt_hash = $4 WHERE id = $1`, album, path, store.NewID(), newHash())
	if err := e.reconcile(album); err != nil {
		e.t.Fatalf("reconciling after the publication: %v", err)
	}
}

// removalPublished records a completed removal: no output, a revision
// published (§4.2).
func (e *env) removalPublished(album uuid.UUID) {
	e.t.Helper()
	e.exec(`UPDATE albums SET published_path = NULL, published_build = NULL, published_receipt_hash = NULL,
		published_revision = revision, published_renderer = 'rv-test' WHERE id = $1`, album)
	if err := e.reconcile(album); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) tracks(album uuid.UUID) []store.ListAlbumTracksRow {
	e.t.Helper()
	ts, err := store.New(e.db).ListAlbumTracks(context.Background(), album)
	if err != nil {
		e.t.Fatal(err)
	}
	return ts
}

// update is the PUT body that saves the album exactly as it is.
func (e *env) update(album uuid.UUID) catalog.AlbumUpdate {
	e.t.Helper()
	a := e.album(album)
	u := catalog.AlbumUpdate{ArtistID: a.ArtistID, Title: a.Title, Genre: a.Genre, Compilation: a.Compilation}
	if a.Year != nil {
		u.Year = ptr(int(*a.Year))
	}
	for _, t := range e.tracks(album) {
		u.Tracks = append(u.Tracks, catalog.TrackUpdate{ID: t.ID, Disc: int(t.Disc), No: int(t.No), Title: t.Title,
			Artist: t.Artist, Genre: t.Genre})
	}
	return u
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func wantCode(t *testing.T, err error, code string) *catalog.Error {
	t.Helper()
	if catalog.Code(err) != code {
		t.Fatalf("err = %v (code %q), want %s", err, catalog.Code(err), code)
	}
	e, _ := catalog.AsError(err)
	return e
}
