package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/failpoint"
	"musiclib/internal/faulttest"
	"musiclib/internal/fsops"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/render"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// The protocol tests run on the real ext4 TMPDIR of the gate and a real
// PostgreSQL 17 (DESIGN.md §12.1), with the real renames and fsyncs. Their
// builds are made by stage below: a staging directory with a few files and
// a real receipt (render.Receipt), because the publisher reads only the
// receipt and filesystem metadata (§2.2) and never the media. The executor
// and end-to-end tests build real FLAC albums with the real tools.
//
// Scenarios are set up and checked with os and absolute paths: that is the
// adversary, not the application.

// env is one data volume, one migrated database, the catalog and the
// publisher.
type env struct {
	t     *testing.T
	dbURL string
	db    *pgxpool.Pool
	data  string
	lib   *fsops.Root
	work  *fsops.Root
	cat   *catalog.Service
	b     *render.Builder
	p     *Publisher
	logs  *syncBuffer
	batch uuid.UUID
	// fp is the failpoint hook of every publisher the env makes, bfp the
	// builder's (setFailpoint, setBuildFailpoint).
	fp, bfp faulttest.Switch
}

// sharedTools are the verified tools, one Runner for the test binary.
var sharedTools = sync.OnceValues(func() (*media.Tools, error) {
	return media.NewTools(context.Background(), media.NewRunner(4), media.FFmpegPath, media.FFprobePath, media.TagsPath)
})

func newEnv(t *testing.T) *env {
	t.Helper()
	dbURL := pgtest.EmptyDB(t)
	db := pgtest.Pool(t, dbURL)
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	return newEnvOn(t, dbURL, db, filepath.Join(t.TempDir(), "data"))
}

// newEnvOn opens the volume at data (created if missing) on db.
func newEnvOn(t *testing.T, dbURL string, db *pgxpool.Pool, data string) *env {
	t.Helper()
	e := &env{t: t, dbURL: dbURL, db: db, data: data, logs: &syncBuffer{}}
	for _, d := range []string{"originals", "library", "work"} {
		if err := os.MkdirAll(filepath.Join(data, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.lib, e.work = e.root("library"), e.root("work")
	blobs, err := blobstore.New(e.root("originals"), e.work)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := sharedTools()
	if err != nil {
		t.Fatalf("NewTools: %v (run the tests in Docker, docs/docker.md)", err)
	}
	if e.b, err = render.New(render.Config{Tools: tools, Blobs: blobs, Work: e.work, Budget: jobs.NewBudget(),
		Failpoints: e.bfp.Hook()}); err != nil {
		t.Fatal(err)
	}
	e.p = e.publisher(db)
	if e.cat, err = catalog.New(db, nil, func(catalog.Blob, string) error { return nil }, importer.GenreFits); err != nil {
		t.Fatal(err)
	}
	e.batch = store.NewID()
	e.exec(`INSERT INTO import_batches (id, root_rel, created_at) VALUES ($1, 'incoming', now())
		ON CONFLICT DO NOTHING`, e.batch)
	return e
}

// publisher is a new publisher on db: a fresh process's, with no memory.
func (e *env) publisher(db *pgxpool.Pool) *Publisher {
	e.t.Helper()
	p, err := New(Config{DB: db, Library: e.lib, Work: e.work, Builder: e.b,
		Log: slog.New(slog.NewJSONHandler(io.MultiWriter(e.logs), nil)), Failpoints: e.fp.Hook()})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) root(rel string) *fsops.Root {
	e.t.Helper()
	r, err := fsops.OpenRoot(filepath.Join(e.data, rel))
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

// importAlbum commits a one-track album through the catalog (§7.6), which
// enqueues its render, and returns its id. Its blobs exist only in the
// catalog: the protocol tests never build from them.
func (e *env) importAlbum(artist, title string) uuid.UUID {
	e.t.Helper()
	id := store.NewID()
	e.exec(`INSERT INTO jobs (id, kind, batch_id, source_rel, state, queued_at, updated_at)
		VALUES ($1, 'import', $2, $3, 'pending', now(), now())`, id, e.batch, "src-"+id.String())
	var ticket int64
	if err := e.db.QueryRow(context.Background(),
		`UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1 RETURNING claimed`, id).Scan(&ticket); err != nil {
		e.t.Fatal(err)
	}
	audio := newHash()
	out, err := e.cat.CommitImport(context.Background(), catalog.ImportCandidate{
		Attempt:     jobs.Attempt{JobID: id, Ticket: ticket},
		Fingerprint: newHash(),
		Blobs:       []catalog.Blob{{Hash: audio, Size: 1000, Format: catalog.FormatFLAC}},
		Artist:      artist, Title: title,
		Tracks: []catalog.ImportTrack{{SourcePath: "01.flac", Disc: 1, No: 1, Title: "One", BlobHash: audio}},
	})
	if err != nil || out.State != jobs.StateDone {
		e.t.Fatalf("CommitImport %s/%s: %+v %v", artist, title, out, err)
	}
	return out.AlbumID
}

// claim claims the next job, which must be a render.
func (e *env) claim() *jobs.Claim {
	e.t.Helper()
	c, err := jobs.ClaimNext(context.Background(), e.db, render.Version)
	if err != nil {
		e.t.Fatal(err)
	}
	if c == nil || c.Kind != jobs.KindRender {
		e.t.Fatalf("claimed %+v, want a render", c)
	}
	return c
}

// stage makes the build of a snapshot as the builder would leave it: the
// album's files and its receipt in work/render/<build_id>/album, or nothing
// for a removal. content varies the bytes.
func (e *env) stage(snap *jobs.RenderSnapshot, content string) render.Result {
	e.t.Helper()
	return stageBuild(e.t, e.data, snap, content)
}

func stageBuild(t testing.TB, data string, snap *jobs.RenderSnapshot, content string) render.Result {
	t.Helper()
	res := render.Result{BuildID: store.NewID(), AlbumID: snap.Album.ID, AlbumRevision: snap.Album.Revision,
		RenderVersion: snap.RenderVersion, Removal: snap.Album.Deleted}
	if res.Removal {
		return res
	}
	res.Dir = catalog.AlbumPath(snap.Artist.Name, snap.Album.Title).Path
	res.Staging = render.StagingDir(res.BuildID)
	dir := filepath.Join(data, "work", filepath.FromSlash(res.Staging))
	files := map[string]string{
		"01 - One.flac":       "audio " + content,
		"Extras/booklet.pdf":  "booklet " + content,
		"cover.jpg":           "cover " + content,
		"Extras/Scans/b.jpg":  "scan " + content,
		"01 - One.lrc":        "[00:01.00]one " + content,
		"Extras/Scans/c.jpg":  "scan c " + content,
		"Extras/rip.log":      "log " + content,
		"Extras/Scans/a.jpeg": "scan a " + content,
	}
	var rf []render.ReceiptFile
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		rf = append(rf, render.ReceiptFile{RelativePath: rel, Size: int64(len(body)), SHA256: sha([]byte(body))})
	}
	sort.Slice(rf, func(i, j int) bool { return rf[i].RelativePath < rf[j].RelativePath })
	r := render.Receipt{AlbumID: res.AlbumID, BuildID: res.BuildID, AlbumRevision: res.AlbumRevision,
		RenderVersion: res.RenderVersion, Files: rf}
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, render.ReceiptName), b, 0o644); err != nil {
		t.Fatal(err)
	}
	res.ReceiptHash = render.ReceiptHash(b)
	return res
}

// render claims the next render, stages it and publishes it; a failure
// before the journal completes the job as the executor does (§6.4).
func (e *env) render(content string) (Report, render.Result, error) {
	e.t.Helper()
	c := e.claim()
	res := e.stage(c.Render, content)
	rep, err := e.p.Publish(context.Background(), c.Render, res)
	if err != nil && !jobs.Stops(err) {
		if ferr := e.p.failBeforeJournal(context.Background(), c.Attempt, err); ferr != nil {
			e.t.Fatal(ferr)
		}
	}
	return rep, res, err
}

// mustPublish renders the next job and wants it published.
func (e *env) mustPublish(content string) (Report, render.Result) {
	e.t.Helper()
	rep, res, err := e.render(content)
	if err != nil || rep.Outcome != Published {
		e.t.Fatalf("publish: %+v %v", rep, err)
	}
	return rep, res
}

func (e *env) album(id uuid.UUID) store.Album {
	e.t.Helper()
	a, err := store.New(e.db).GetAlbum(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

// renderJob returns the album's render row, if any.
func (e *env) renderJob(album uuid.UUID) (store.Job, bool) {
	e.t.Helper()
	j, err := store.New(e.db).GetJob(context.Background(), e.renderJobID(album))
	if err != nil {
		return store.Job{}, false
	}
	return j, true
}

func (e *env) renderJobID(album uuid.UUID) uuid.UUID {
	var id uuid.UUID
	err := e.db.QueryRow(context.Background(), `SELECT id FROM jobs WHERE kind = 'render' AND album_id = $1`, album).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		e.t.Fatal(err)
	}
	return id
}

// journal returns the pending journal, if any.
func (e *env) journal() (Journal, bool) {
	e.t.Helper()
	r, err := store.New(e.db).GetJournal(context.Background())
	if errors.Is(err, pgx.ErrNoRows) {
		return Journal{}, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return fromRow(r), true
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

// wantClaims checks the album's claims by path.
func (e *env) wantClaims(album uuid.UUID, paths ...string) {
	e.t.Helper()
	var got []string
	for _, p := range e.claims(album) {
		got = append(got, p)
	}
	sort.Strings(got)
	sort.Strings(paths)
	if fmt.Sprint(got) != fmt.Sprint(paths) {
		e.t.Fatalf("claims of %s: %q, want %q", album, got, paths)
	}
}

// wantPublished checks the album's published state against a build.
func (e *env) wantPublished(album uuid.UUID, res render.Result) {
	e.t.Helper()
	a := e.album(album)
	if a.PublishedRevision != res.AlbumRevision || deref(a.PublishedRenderer) != res.RenderVersion {
		e.t.Fatalf("published revision %d renderer %q, want %d %q", a.PublishedRevision, deref(a.PublishedRenderer),
			res.AlbumRevision, res.RenderVersion)
	}
	if res.Removal {
		if a.PublishedPath != nil || a.PublishedBuild != nil || a.PublishedReceiptHash != nil {
			e.t.Fatalf("a removal left published output %v %v", a.PublishedPath, a.PublishedBuild)
		}
		return
	}
	if deref(a.PublishedPath) != res.Dir || deref(a.PublishedBuild) != res.BuildID || deref(a.PublishedReceiptHash) != res.ReceiptHash {
		e.t.Fatalf("published %q %v %q, want %q %v %q", deref(a.PublishedPath), deref(a.PublishedBuild),
			deref(a.PublishedReceiptHash), res.Dir, res.BuildID, res.ReceiptHash)
	}
	e.wantInstalled(res)
}

// wantInstalled checks that library/<Dir> is exactly the build: its
// receipt, and every file the receipt lists with its size and hash.
func (e *env) wantInstalled(res render.Result) {
	e.t.Helper()
	dir := filepath.Join(e.data, "library", filepath.FromSlash(res.Dir))
	b, err := os.ReadFile(filepath.Join(dir, render.ReceiptName))
	if err != nil {
		e.t.Fatalf("no receipt at %s: %v", res.Dir, err)
	}
	r, err := render.ParseReceipt(b)
	if err != nil || r.BuildID != res.BuildID || render.ReceiptHash(b) != res.ReceiptHash {
		e.t.Fatalf("receipt at %s: %v %v, want build %s", res.Dir, r, err, res.BuildID)
	}
	for _, f := range r.Files {
		c, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.RelativePath)))
		if err != nil || int64(len(c)) != f.Size || sha(c) != f.SHA256 {
			e.t.Fatalf("%s/%s does not match the receipt: %v", res.Dir, f.RelativePath, err)
		}
	}
}

// path is an absolute path under the data volume.
func (e *env) path(rel string) string { return filepath.Join(e.data, filepath.FromSlash(rel)) }

func (e *env) exists(rel string) bool {
	e.t.Helper()
	_, err := os.Lstat(e.path(rel))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		e.t.Fatal(err)
	}
	return err == nil
}

func (e *env) write(rel, content string) {
	e.t.Helper()
	p := e.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// entries lists the names in a directory of the volume.
func (e *env) entries(rel string) []string {
	e.t.Helper()
	ents, err := os.ReadDir(e.path(rel))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, d := range ents {
		out = append(out, d.Name())
	}
	return out
}

// tree maps every entry under rel to its type, mode and content hash.
func (e *env) tree(rel string) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	base := e.path(rel)
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(base, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		v := info.Mode().String()
		switch {
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += " " + sha(b)
		case d.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			v += " -> " + l
		}
		out[filepath.ToSlash(r)] = v
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) wantTree(rel string, want map[string]string) {
	e.t.Helper()
	got := e.tree(rel)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		e.t.Fatalf("%s changed:\nbefore %v\nafter  %v", rel, want, got)
	}
}

// wantWorkClean: nothing left in work/render and work/retired.
func (e *env) wantWorkClean() {
	e.t.Helper()
	for _, d := range []string{"work/render", "work/retired"} {
		if ents := e.entries(d); len(ents) != 0 {
			e.t.Fatalf("%s holds %q", d, ents)
		}
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if Code(err) != code {
		t.Fatalf("err = %v (code %q), want %s", err, Code(err), code)
	}
}

// setFailpoint makes every publisher of the env run fn at its failpoints;
// nil removes it.
func (e *env) setFailpoint(fn func(point string) error) {
	if fn == nil {
		e.fp.Set(nil)
		return
	}
	e.fp.Set(func(p failpoint.Point) error { return fn(p.Name) })
}

// failAt makes the named failpoint return an error once.
func (e *env) failAt(at string) *atomic.Bool {
	var hit atomic.Bool
	e.setFailpoint(func(point string) error {
		if point == at && hit.CompareAndSwap(false, true) {
			return fmt.Errorf("injected at %s", point)
		}
		return nil
	})
	return &hit
}

// syncBuffer is an io.Writer safe for concurrent logs and reads.
type syncBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = append(s.b, p...)
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.b)
}

// The catalog changes the tests make, through the catalog's own
// transactions (§13.2), as the API will.

func (e *env) update(album uuid.UUID, change func(u *catalog.AlbumUpdate)) error {
	e.t.Helper()
	a := e.album(album)
	u := catalog.AlbumUpdate{ArtistID: a.ArtistID, Title: a.Title, Genre: a.Genre, Compilation: a.Compilation}
	if a.Year != nil {
		y := int(*a.Year)
		u.Year = &y
	}
	ts, err := store.New(e.db).ListAlbumTracks(context.Background(), album)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, t := range ts {
		u.Tracks = append(u.Tracks, catalog.TrackUpdate{ID: t.ID, Disc: int(t.Disc), No: int(t.No), Title: t.Title,
			Artist: t.Artist, Genre: t.Genre})
	}
	change(&u)
	_, _, err = e.cat.UpdateAlbum(context.Background(), album, a.Revision, u)
	return err
}

// bump changes the album's year: a new revision at the same path.
func (e *env) bump(album uuid.UUID) {
	e.t.Helper()
	if err := e.update(album, func(u *catalog.AlbumUpdate) {
		y := 1900
		if u.Year != nil {
			y = *u.Year + 1
		}
		u.Year = &y
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) retitle(album uuid.UUID, title string) error {
	e.t.Helper()
	return e.update(album, func(u *catalog.AlbumUpdate) { u.Title = title })
}

func (e *env) renameArtist(album uuid.UUID, name string) {
	e.t.Helper()
	a := e.album(album)
	ar, err := store.New(e.db).GetArtist(context.Background(), a.ArtistID)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := e.cat.RenameArtist(context.Background(), ar.ID, ar.Revision, name); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) trash(album uuid.UUID) {
	e.t.Helper()
	if _, _, err := e.cat.TrashAlbum(context.Background(), album, e.album(album).Revision); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) restore(album uuid.UUID) {
	e.t.Helper()
	if _, _, err := e.cat.RestoreAlbum(context.Background(), album, e.album(album).Revision); err != nil {
		e.t.Fatal(err)
	}
}
