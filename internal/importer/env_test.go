package importer

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/faulttest"
	"musiclib/internal/fsops"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/store/pgtest"
)

// env is a complete import environment on real ext4 (TMPDIR in the gate)
// and a real PostgreSQL 17: an /import-like source directory, /data's
// originals and work, the real pinned tools, the catalog and the importer.
type env struct {
	t     *testing.T
	db    *pgxpool.Pool
	cat   *catalog.Service
	im    *Importer
	blobs *blobstore.Store
	src   string // the /import-like directory, a host path for the test only
	data  string
	work  *fsops.Root
	// allowChange: the test changes the source on purpose during a job.
	allowChange bool
	// fp is the importer's failpoint hook (setHook).
	fp faulttest.Switch
}

func tools(t testing.TB) *media.Tools {
	t.Helper()
	tl, err := media.NewTools(context.Background(), media.NewRunner(4), media.FFmpegPath, media.FFprobePath, media.TagsPath)
	if err != nil {
		t.Fatalf("NewTools: %v (run the tests in Docker, docs/docker.md)", err)
	}
	return tl
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvOn(t, pgtest.New(t))
}

func newEnvOn(t *testing.T, db *pgxpool.Pool) *env {
	t.Helper()
	return newEnvAt(t, db, t.TempDir())
}

// newEnvAt is an env whose source and data are dir/import and dir/data,
// created if missing: a fresh process on an existing volume.
func newEnvAt(t *testing.T, db *pgxpool.Pool, dir string) *env {
	t.Helper()
	e := &env{t: t, db: db, src: filepath.Join(dir, "import"), data: filepath.Join(dir, "data")}
	for _, d := range []string{e.src, filepath.Join(e.data, "originals"), filepath.Join(e.data, "work")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := e.root(e.src)
	originals := e.root(filepath.Join(e.data, "originals"))
	e.work = e.root(filepath.Join(e.data, "work"))
	var err error
	if e.blobs, err = blobstore.New(originals, e.work); err != nil {
		t.Fatal(err)
	}
	if e.cat, err = catalog.New(db, nil, CoverFits); err != nil {
		t.Fatal(err)
	}
	e.im, err = New(Config{Catalog: e.cat, Tools: tools(t), Blobs: e.blobs, Source: src, Work: e.work,
		Budget: jobs.NewBudget(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Failpoints: e.fp.Hook()})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) root(p string) *fsops.Root {
	e.t.Helper()
	r, err := fsops.OpenRoot(p)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		if err := r.Close(); err != nil {
			e.t.Error(err)
		}
	})
	return r
}

// put writes a source file, creating its directories.
func (e *env) put(rel string, b []byte) {
	e.t.Helper()
	p := filepath.Join(e.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) flac(rel string, tr track) { e.t.Helper(); e.put(rel, tr.flac(e.t)) }

// batch creates an import batch of rootRel.
func (e *env) batch(rootRel string) catalog.ImportBatch {
	e.t.Helper()
	b, err := e.cat.CreateImportBatch(context.Background(), uuid.New(), rootRel)
	if err != nil {
		e.t.Fatalf("CreateImportBatch(%q): %v", rootRel, err)
	}
	return b
}

// claim claims the next scan or import job, parking render jobs as failed
// first: the renderer does not exist yet, and a render would be claimed
// before them (§6.1).
func (e *env) claim() *jobs.Claim {
	e.t.Helper()
	e.exec(`UPDATE jobs SET state = 'failed', error_code = 'test_parked', error_message = 'parked by the test'
		WHERE kind = 'render' AND state = 'pending'`)
	c, err := jobs.ClaimNext(context.Background(), e.db, "rv-test")
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// execute runs one claim as the pool would and checks that the source did
// not change (§3.2 guarantee 1), unless the test changes it on purpose.
func (e *env) execute(c *jobs.Claim) error {
	e.t.Helper()
	before := sourceState(e.t, e.src)
	var err error
	switch c.Kind {
	case jobs.KindScan:
		err = e.im.ExecuteScan(context.Background(), c)
	case jobs.KindImport:
		err = e.im.ExecuteImport(context.Background(), c)
	default:
		e.t.Fatalf("unexpected %s job", c.Kind)
	}
	if !e.allowChange {
		if d := diffStates(before, sourceState(e.t, e.src)); len(d) > 0 {
			e.t.Fatalf("the source changed during %s: %v", c.Kind, d)
		}
	}
	e.checkNoWorkLeft()
	return err
}

// runAll executes jobs until none is pending.
func (e *env) runAll() {
	e.t.Helper()
	for {
		c := e.claim()
		if c == nil {
			return
		}
		if err := e.execute(c); err != nil {
			e.t.Fatalf("executing %s %s: %v", c.Kind, c.SourceRel, err)
		}
	}
}

// importDir runs a whole batch of rootRel and returns it.
func (e *env) importDir(rootRel string) catalog.ImportBatch {
	e.t.Helper()
	b := e.batch(rootRel)
	e.runAll()
	return b
}

// checkNoWorkLeft: nothing is left in work/import after a job.
func (e *env) checkNoWorkLeft() {
	e.t.Helper()
	ents, err := os.ReadDir(filepath.Join(e.data, "work", workDir))
	if err != nil {
		e.t.Fatal(err)
	}
	if len(ents) > 0 {
		e.t.Fatalf("work/import holds %d entries after a job", len(ents))
	}
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

// importJob is an import job row as the tests read it.
type importJob struct {
	ID        uuid.UUID
	State     string
	Code      string
	Message   string
	AlbumID   uuid.UUID
	Warnings  []jobs.Warning
	SourceRel string
}

// importJob returns the import job of sourceRel in batch b.
func (e *env) importJob(b catalog.ImportBatch, sourceRel string) importJob {
	e.t.Helper()
	var j importJob
	var code, msg *string
	var album *uuid.UUID
	var ws []byte
	err := e.db.QueryRow(context.Background(), `SELECT id, state, error_code, error_message, result_album_id, warnings, source_rel
		FROM jobs WHERE kind = 'import' AND batch_id = $1 AND source_rel = $2`, b.ID, sourceRel).
		Scan(&j.ID, &j.State, &code, &msg, &album, &ws, &j.SourceRel)
	if err != nil {
		e.t.Fatalf("import job %q: %v", sourceRel, err)
	}
	if code != nil {
		j.Code, j.Message = *code, *msg
	}
	if album != nil {
		j.AlbumID = *album
	}
	if j.Warnings, err = jobs.DecodeWarnings(ws); err != nil {
		e.t.Fatal(err)
	}
	return j
}

// scanJob returns the scan job of b.
func (e *env) scanJob(b catalog.ImportBatch) importJob {
	e.t.Helper()
	var j importJob
	var code, msg *string
	var ws []byte
	err := e.db.QueryRow(context.Background(), `SELECT id, state, error_code, error_message, warnings
		FROM jobs WHERE kind = 'scan' AND batch_id = $1`, b.ID).Scan(&j.ID, &j.State, &code, &msg, &ws)
	if err != nil {
		e.t.Fatal(err)
	}
	if code != nil {
		j.Code, j.Message = *code, *msg
	}
	if j.Warnings, err = jobs.DecodeWarnings(ws); err != nil {
		e.t.Fatal(err)
	}
	return j
}

// done returns the album of a done import job, failing otherwise.
func (e *env) done(b catalog.ImportBatch, sourceRel string) album {
	e.t.Helper()
	j := e.importJob(b, sourceRel)
	if j.State != "done" {
		e.t.Fatalf("import %q: %s %s: %s", sourceRel, j.State, j.Code, j.Message)
	}
	return e.album(j.AlbumID)
}

// failed checks that the import job of sourceRel failed with code.
func (e *env) failed(b catalog.ImportBatch, sourceRel, code string) importJob {
	e.t.Helper()
	j := e.importJob(b, sourceRel)
	if j.State != "failed" || j.Code != code {
		e.t.Fatalf("import %q: %s %s (%s), want failed %s", sourceRel, j.State, j.Code, j.Message, code)
	}
	return j
}

// album is the catalog's view of an imported album.
type album struct {
	ID          uuid.UUID
	Artist      string
	Title       string
	Year        *int32
	Genre       *string
	Compilation bool
	Cover       *string
	Fingerprint string
	Tracks      []trackRow
	Attachments map[string]string // rel_path -> blob hash
}

type trackRow struct {
	Disc, No   int32
	Title      string
	Artist     *string
	Genre      *string
	Source     string
	Blob       string
	Lyrics     *string
	BlobFormat *string
}

func (e *env) album(id uuid.UUID) album {
	e.t.Helper()
	ctx := context.Background()
	a := album{ID: id, Attachments: map[string]string{}}
	var fp *string
	err := e.db.QueryRow(ctx, `SELECT ar.name, al.title, al.year, al.genre, al.compilation, al.cover_hash, al.import_fingerprint
		FROM albums al JOIN artists ar ON ar.id = al.artist_id WHERE al.id = $1`, id).
		Scan(&a.Artist, &a.Title, &a.Year, &a.Genre, &a.Compilation, &a.Cover, &fp)
	if err != nil {
		e.t.Fatal(err)
	}
	if fp != nil {
		a.Fingerprint = *fp
	}
	rows, err := e.db.Query(ctx, `SELECT t.disc, t.no, t.title, t.artist, t.genre, t.source_path, t.blob_hash, t.lyrics_hash, b.format
		FROM tracks t JOIN blobs b ON b.hash = t.blob_hash WHERE t.album_id = $1 ORDER BY t.disc, t.no`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	for rows.Next() {
		var r trackRow
		if err := rows.Scan(&r.Disc, &r.No, &r.Title, &r.Artist, &r.Genre, &r.Source, &r.Blob, &r.Lyrics, &r.BlobFormat); err != nil {
			e.t.Fatal(err)
		}
		a.Tracks = append(a.Tracks, r)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	rows, err = e.db.Query(ctx, `SELECT rel_path, blob_hash FROM attachments WHERE album_id = $1`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	for rows.Next() {
		var p, h string
		if err := rows.Scan(&p, &h); err != nil {
			e.t.Fatal(err)
		}
		a.Attachments[p] = h
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return a
}

// blobFormat returns blobs.format of a hash ("" for NULL).
func (e *env) blobFormat(hash string) string {
	e.t.Helper()
	var f *string
	if err := e.db.QueryRow(context.Background(), `SELECT format FROM blobs WHERE hash = $1`, hash).Scan(&f); err != nil {
		e.t.Fatal(err)
	}
	if f == nil {
		return ""
	}
	return *f
}

// retry puts a finished import job back to pending with overrides, as
// POST /api/jobs/{id}/retry will (§10.2): a new ticket, no error.
func (e *env) retry(id uuid.UUID, ov jobs.Overrides) {
	e.t.Helper()
	b, err := ov.Encode()
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`UPDATE jobs SET state = 'pending', claimed = NULL, requested = nextval('job_ticket'), error_code = NULL,
		error_message = NULL, result_album_id = NULL, warnings = '[]', overrides = $2, queued_at = now() WHERE id = $1`, id, b)
}

func hasWarning(ws []jobs.Warning, code jobs.WarningCode, path string) bool {
	for _, w := range ws {
		if w.Code == code && (path == "" || w.Path == path) {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }
