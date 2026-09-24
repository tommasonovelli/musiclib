package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/unix"

	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/publish"
	"musiclib/internal/render"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// The boot steps 4 to 7 and the workers (§11.1), on real albums: FLAC
// files made by the pinned ffmpeg in the test's /import, imported, built
// and published by the server itself.

// writeFLAC writes a FLAC file of the given length with Vorbis comments.
func writeFLAC(t *testing.T, path string, freq int, seconds float64, tags map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", fmt.Sprintf("sine=frequency=%d:duration=%g:sample_rate=44100", freq, seconds), "-ac", "2",
		"-c:a", "flac", "-fflags", "+bitexact", "-flags:a", "+bitexact"}
	for k, v := range tags {
		args = append(args, "-metadata", k+"="+v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, media.FFmpegPath, append(args, path)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
}

// writeAlbum writes n tracks of an album under imports/dir.
func writeAlbum(t *testing.T, imports, dir, artist, album string, n int, seconds float64) {
	t.Helper()
	for i := 1; i <= n; i++ {
		writeFLAC(t, filepath.Join(imports, dir, fmt.Sprintf("%02d.flac", i)), 200+50*i, seconds, map[string]string{
			"ARTIST": artist, "ALBUM": album, "TITLE": "Track " + strconv.Itoa(i), "TRACKNUMBER": strconv.Itoa(i),
			"DATE": "1959"})
	}
}

// dbPool is a pool on the test database, for the test's own catalog calls:
// the API does not exist yet (Phase 5), so the test creates batches and
// edits albums through the catalog service, as the API will.
func dbPool(t *testing.T, dbURL string) *pgxpool.Pool {
	t.Helper()
	return pgtest.Pool(t, dbURL)
}

func catalogOn(t *testing.T, db *pgxpool.Pool) *catalog.Service {
	t.Helper()
	c, err := catalog.New(db, nil, importer.CoverFits)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// waitFor polls cond for up to 2 minutes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func queryInt(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// idle: no job pending or running.
func idle(t *testing.T, db *pgxpool.Pool) bool {
	return queryInt(t, db, `SELECT count(*) FROM jobs WHERE state IN ('pending', 'running')`) == 0
}

// hashTree maps every file under dir to its SHA-256 and mode, every
// directory to its mode.
func hashTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(dir, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		v := info.Mode().String() + " " + info.ModTime().String()
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			s := sha256.Sum256(b)
			v += " " + hex.EncodeToString(s[:])
		}
		out[r] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(a, b map[string]string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

// published reads an album's published columns.
func published(t *testing.T, db *pgxpool.Pool, id uuid.UUID) store.Album {
	t.Helper()
	a, err := store.New(db).GetAlbum(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// checkOutput checks library/<path> against the album's published state:
// the receipt with its hash in the album, every listed file as listed, and
// the tracks' managed tags.
func checkOutput(t *testing.T, data string, a store.Album, artist, album string, tracks int) {
	t.Helper()
	dir := filepath.Join(data, "library", filepath.FromSlash(*a.PublishedPath))
	b, err := os.ReadFile(filepath.Join(dir, render.ReceiptName))
	if err != nil {
		t.Fatal(err)
	}
	r, err := render.ParseReceipt(b)
	if err != nil || render.ReceiptHash(b) != *a.PublishedReceiptHash || r.BuildID != *a.PublishedBuild ||
		r.AlbumRevision != a.PublishedRevision || r.AlbumID != a.ID {
		t.Fatalf("receipt %v (%v) does not match the album's published state", r, err)
	}
	var names []string
	for _, f := range r.Files {
		names = append(names, f.RelativePath)
		c, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.RelativePath)))
		s := sha256.Sum256(c)
		if err != nil || int64(len(c)) != f.Size || hex.EncodeToString(s[:]) != f.SHA256 {
			t.Fatalf("%s does not match the receipt", f.RelativePath)
		}
	}
	var want []string
	for i := 1; i <= tracks; i++ {
		want = append(want, fmt.Sprintf("%02d - Track %d.flac", i, i))
	}
	if !slices.Equal(names, want) {
		t.Fatalf("output files %q, want %q (§1.1)", names, want)
	}
	tools, err := media.NewTools(context.Background(), media.NewRunner(1), media.FFmpegPath, media.FFprobePath, media.TagsPath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsops.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	for i, name := range want {
		f, err := root.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		in, err := tools.Inspect(context.Background(), f, catalog.FormatFLAC)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		m := in.Managed
		if !slices.Equal(m.Title, []string{"Track " + strconv.Itoa(i+1)}) || !slices.Equal(m.AlbumArtist, []string{artist}) ||
			!slices.Equal(m.Album, []string{album}) || !slices.Equal(m.Track, []string{strconv.Itoa(i + 1)}) ||
			!slices.Equal(m.Date, []string{"1959"}) {
			t.Fatalf("%s: managed tags %+v", name, m)
		}
	}
}

// §13.1, the Phase 2 slice end to end, with two workers: a FLAC album in
// /import, a batch, the scan, the import, the render and the publication
// by the server; then an artist rename (the album moves, the old path is
// retired and its claim released), a trash (the album leaves library/) and
// a restore (it comes back). originals/ is never touched after the import
// and /import never at all.
func TestEndToEndTwoWorkers(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeAlbum(t, p.imports, "Kind of Blue", "Miles Davis", "Kind of Blue", 3, 1)
	writeAlbum(t, p.imports, "Portrait", "Bill Evans", "Portrait in Jazz", 2, 1)
	importBefore := hashTree(t, p.imports)
	cfg := testConfig(dbURL)
	cfg.Workers = 2
	d := startDaemon(t, cfg, p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	db := dbPool(t, dbURL)
	cat := catalogOn(t, db)
	ctx := context.Background()
	if _, err := cat.CreateImportBatch(ctx, uuid.New(), ""); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	waitFor(t, "the albums to be published", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision > 0`) == 2
	})
	if err := db.QueryRow(ctx, `SELECT id FROM albums WHERE title = 'Kind of Blue'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM jobs WHERE state = 'failed'`); n != 0 {
		t.Fatalf("%d failed jobs; logs:\n%s", n, d.logs)
	}
	a := published(t, db, id)
	if *a.PublishedPath != "Miles Davis/Kind of Blue" || a.PublishedRevision != a.Revision || *a.PublishedRenderer != render.Version {
		t.Fatalf("published %+v", a)
	}
	checkOutput(t, p.data, a, "Miles Davis", "Kind of Blue", 3)
	originals := hashTree(t, filepath.Join(p.data, "originals"))

	// The artist rename moves the album; the old path is retired.
	ar, err := store.New(db).GetArtist(ctx, a.ArtistID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cat.RenameArtist(ctx, ar.ID, ar.Revision, "Miles Dewey Davis"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the rename to be published", func() bool {
		return idle(t, db) && published(t, db, id).PublishedRevision == published(t, db, id).Revision
	})
	a = published(t, db, id)
	checkOutput(t, p.data, a, "Miles Dewey Davis", "Kind of Blue", 3)
	if exists(t, filepath.Join(p.data, "library", "Miles Davis")) {
		t.Fatal("the old artist directory is still in the library")
	}
	claims, err := store.New(db).ListAlbumClaims(ctx, id)
	if err != nil || len(claims) != 1 || claims[0].Path != "Miles Dewey Davis/Kind of Blue" {
		t.Fatalf("claims %+v %v, want only the new path", claims, err)
	}

	// Trash: removed from library/. Restore: back.
	if _, _, err := cat.TrashAlbum(ctx, id, a.Revision); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the removal", func() bool {
		return idle(t, db) && published(t, db, id).PublishedRevision == published(t, db, id).Revision
	})
	if a = published(t, db, id); a.PublishedPath != nil || exists(t, filepath.Join(p.data, "library", "Miles Dewey Davis")) {
		t.Fatalf("the trashed album is still published: %v", a.PublishedPath)
	}
	if _, _, err := cat.RestoreAlbum(ctx, id, a.Revision); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the restore", func() bool {
		a := published(t, db, id)
		return idle(t, db) && a.PublishedRevision == a.Revision && a.PublishedPath != nil
	})
	checkOutput(t, p.data, published(t, db, id), "Miles Dewey Davis", "Kind of Blue", 3)

	if !sameTree(originals, hashTree(t, filepath.Join(p.data, "originals"))) {
		t.Fatal("originals/ changed after the import")
	}
	if !sameTree(importBefore, hashTree(t, p.imports)) {
		t.Fatal("/import changed")
	}
	for _, dir := range []string{"render", "retired"} {
		if ents, _ := os.ReadDir(filepath.Join(p.data, "work", dir)); len(ents) != 0 {
			t.Fatalf("work/%s holds %d entries", dir, len(ents))
		}
	}
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}
	assertShutdownOrder(t, d.logs, true)
}

// seedAlbum commits an album with the catalog, blobs in the catalog only,
// and returns its id: the boot tests need catalog rows, not media.
func seedAlbum(t *testing.T, db *pgxpool.Pool, artist, title string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	batch, job := store.NewID(), store.NewID()
	if _, err := db.Exec(ctx, `INSERT INTO import_batches (id, root_rel, created_at) VALUES ($1, '', now())`, batch); err != nil {
		t.Fatal(err)
	}
	var ticket int64
	execSQL(t, db, `INSERT INTO jobs (id, kind, batch_id, source_rel, state, queued_at, updated_at)
		VALUES ($1, 'import', $2, 'x', 'pending', now(), now())`, job, batch)
	if err := db.QueryRow(ctx, `UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1 RETURNING claimed`,
		job).Scan(&ticket); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(artist + "/" + title))
	audio := hex.EncodeToString(h[:])
	fp := sha256.Sum256([]byte("fp " + artist + "/" + title))
	out, err := catalogOn(t, db).CommitImport(ctx, catalog.ImportCandidate{
		Attempt: jobs.Attempt{JobID: job, Ticket: ticket}, Fingerprint: hex.EncodeToString(fp[:]),
		Blobs:  []catalog.Blob{{Hash: audio, Size: 10, Format: catalog.FormatFLAC}},
		Artist: artist, Title: title,
		Tracks: []catalog.ImportTrack{{SourcePath: "1.flac", Disc: 1, No: 1, Title: "One", BlobHash: audio}},
	})
	if err != nil || out.State != jobs.StateDone {
		t.Fatalf("CommitImport: %+v %v", out, err)
	}
	return out.AlbumID
}

func execSQL(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// §11.1 steps 4 to 7 in order: the journal recovered first (its FINALIZE
// completes its running job by ticket), then work/ cleaned and running
// jobs made pending, then the stale renders enqueued (only for active
// albums without a job; a failed job stays), then the workers, then
// readiness.
func TestBootStepsInOrder(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	bootOnce(t, dbURL, p)
	db := dbPool(t, dbURL)
	journaled := seedAlbum(t, db, "A", "Journaled")
	failed := seedAlbum(t, db, "B", "Failed")
	current := seedAlbum(t, db, "C", "Current")
	// The journaled album's render is running with ticket T and its removal
	// journal prepared (an album never published, nothing on disk).
	execSQL(t, db, `UPDATE jobs SET state = 'running', claimed = requested WHERE album_id = $1`, journaled)
	var ticket int64
	if err := db.QueryRow(context.Background(), `SELECT claimed FROM jobs WHERE album_id = $1`, journaled).Scan(&ticket); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `INSERT INTO publication (id, album_id, ticket, revision, renderer, build_id) VALUES (1, $1, $2, 1, 'old-renderer', $3)`,
		journaled, ticket, store.NewID())
	execSQL(t, db, `UPDATE jobs SET state = 'failed', error_code = 'x', error_message = 'x' WHERE album_id = $1`, failed)
	execSQL(t, db, `DELETE FROM jobs WHERE album_id = $1`, current)
	execSQL(t, db, `UPDATE albums SET published_revision = 1, published_renderer = $2 WHERE id = $1`, current, render.Version)

	d := startDaemon(t, testConfig(dbURL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	var order []string
	for _, m := range d.logs.messages(t) {
		switch m {
		case "journal recovered", "work cleaned", "running jobs recovered", "stale renders enqueued", "workers started", "ready":
			order = append(order, m)
		}
	}
	want := []string{"journal recovered", "work cleaned", "running jobs recovered", "stale renders enqueued", "workers started", "ready"}
	if !slices.Equal(order[len(order)-len(want):], want) {
		t.Fatalf("boot events %q, want %q", order, want)
	}
	for _, ev := range d.logs.events(t) {
		switch ev["msg"] {
		case "running jobs recovered":
			if ev["jobs"] != float64(0) {
				t.Fatalf("%v running jobs recovered: FINALIZE must complete the journal's job first", ev["jobs"])
			}
		case "stale renders enqueued":
			if ev["albums"] != float64(1) {
				t.Fatalf("%v stale renders enqueued, want 1 (the journaled album)", ev["albums"])
			}
		}
	}
	a := published(t, db, journaled)
	if a.PublishedRevision != 1 || deref(a.PublishedRenderer) != "old-renderer" {
		// The pool may already be rendering it again: the renderer is still
		// the journal's until that render is published, and it never is
		// (its blobs are not on disk).
		t.Fatalf("the journal was not finalized: %+v", a)
	}
	var state string
	if err := db.QueryRow(context.Background(), `SELECT state FROM jobs WHERE album_id = $1`, failed).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("the failed render is %q (%v), want left failed", state, err)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM jobs WHERE album_id = $1`, current); n != 0 {
		t.Fatal("an album with the current renderer was enqueued")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM publication`); n != 0 {
		t.Fatal("the journal survived the boot")
	}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// plantIllegalJournal makes the §9.4 illegal state on a booted volume: a
// journal whose new path holds a foreign directory and whose staging does
// not exist. It returns the album, the journal's build and the library's
// hashes.
func plantIllegalJournal(t *testing.T, dbURL string, p paths) (album, build uuid.UUID, library map[string]string) {
	t.Helper()
	bootOnce(t, dbURL, p)
	db := dbPool(t, dbURL)
	id := seedAlbum(t, db, "Artist", "Album")
	execSQL(t, db, `UPDATE jobs SET state = 'running', claimed = requested WHERE album_id = $1`, id)
	h := sha256.Sum256([]byte("receipt"))
	build = store.NewID()
	execSQL(t, db, `INSERT INTO publication (id, album_id, ticket, revision, renderer, build_id, receipt_hash, new_path)
		VALUES (1, $1, 1, 1, $2, $3, $4, 'Artist/Album')`, id, render.Version, build, hex.EncodeToString(h[:]))
	writeFile(t, filepath.Join(p.data, "library", "Artist", "Album", "mine.txt"), "the user's")
	return id, build, hashTree(t, filepath.Join(p.data, "library"))
}

// wantSuspended checks the state of a process that found an illegal
// journal: nothing moved, the journal kept, the job neither run nor
// recovered (no step after 4 ran).
func wantSuspended(t *testing.T, dbURL string, p paths, album uuid.UUID, library map[string]string) {
	t.Helper()
	db := dbPool(t, dbURL)
	if !sameTree(library, hashTree(t, filepath.Join(p.data, "library"))) {
		t.Fatal("library/ changed")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM publication`); n != 1 {
		t.Fatal("the journal was resolved by guessing")
	}
	if n := queryInt(t, db, `SELECT count(*) FROM jobs WHERE album_id = $1 AND state = 'running'`, album); n != 1 {
		t.Fatal("a step after the recovery ran: the job is no longer running")
	}
}

// §9.4 "la pubblicazione viene sospesa e l'errore esposto" (owner
// decision N-135): a journal whose state matches no legal transition does
// not stop the process. It stays alive with the lock held, starts no
// worker, and /health/ready answers 503 publish_illegal_state with the
// journal's album and build and no path; /health/live is 200. Nothing is
// deleted. A stop is a normal shutdown that releases the lock.
func TestBootSuspendsOnAnIllegalJournal(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	album, build, library := plantIllegalJournal(t, dbURL, p)

	d := startDaemon(t, testConfig(dbURL), p)
	d.logs.waitLog(t, "publishing suspended: the pending publication is in an illegal state")
	status, body := getJSON(t, d.base+"/health/ready")
	if status != http.StatusServiceUnavailable || body.Code != publish.CodeIllegalState ||
		body.Details["album_id"] != album.String() || body.Details["build_id"] != build.String() || len(body.Details) != 2 {
		t.Fatalf("ready: %d %+v", status, body)
	}
	if strings.Contains(body.Message, p.data) || body.Message == "" {
		t.Fatalf("message %q", body.Message)
	}
	d.waitStatus(t, "/health/live", http.StatusOK)
	assertLockHeld(t, p.data)
	if d.logs.has(t, "ready") || d.logs.has(t, "workers started") || d.logs.has(t, "work cleaned") {
		t.Fatal("the boot went on past an illegal journal")
	}
	wantSuspended(t, dbURL, p, album, library)
	if err := d.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertLockFree(t, p.data)
	wantSuspended(t, dbURL, p, album, library)
}

// getJSON GETs url and decodes the error body.
func getJSON(t *testing.T, url string) (int, errorBody) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var b errorBody
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

// toolChildren returns the pids of the native tools that are children of
// pid (§8.5: the Runner starts them directly).
func toolChildren(pid int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || close < 0 {
			continue
		}
		comm, fields := s[open+1:close], strings.Fields(s[close+1:])
		if len(fields) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		if ppid == pid && (comm == "ffmpeg" || comm == "ffprobe" || comm == "musiclib-tags") {
			out = append(out, n)
		}
	}
	return out
}

// alive reports whether pid runs (a zombie waiting to be reaped does not).
func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && len(s) > i+2 && s[i+2] != 'Z'
}

// watchTools collects the tool children of the server until stop is
// closed, and signals first when one has been seen.
func watchTools(pid int, first chan<- struct{}, stop <-chan struct{}) <-chan []int {
	out := make(chan []int, 1)
	go func() {
		seen := map[int]bool{}
		signaled := false
		for {
			for _, c := range toolChildren(pid) {
				seen[c] = true
			}
			if len(seen) > 0 && !signaled {
				close(first)
				signaled = true
			}
			select {
			case <-stop:
				var pids []int
				for p := range seen {
					pids = append(pids, p)
				}
				out <- pids
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	return out
}

// assertNoSurvivors: none of pids is still running shortly after the
// server exited (§8.5, §12.2).
func assertNoSurvivors(t *testing.T, pids []int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, p := range pids {
		for alive(p) {
			if time.Now().After(deadline) {
				t.Fatalf("helper %d survived the server", p)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// A real server process with two workers busy on an album whose tools are
// running: the database is taken away. §6.4: the process exits non-zero
// (Docker restarts it), no helper survives it, and a restart recovers and
// finishes the work.
func TestProcessDatabaseLossMidWork(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeAlbum(t, p.imports, "Long", "Artist", "Long Album", 4, 20)
	s := startServerProcess(t, dbURL, p, 0o022, envWorkers+"=2")
	s.waitHealthy(t)
	db := dbPool(t, dbURL)
	if _, err := catalogOn(t, db).CreateImportBatch(context.Background(), uuid.New(), ""); err != nil {
		t.Fatal(err)
	}
	first, stop := make(chan struct{}), make(chan struct{})
	pids := watchTools(s.cmd.Process.Pid, first, stop)
	select {
	case <-first:
	case <-time.After(2 * time.Minute):
		t.Fatalf("no tool started; stderr:\n%s", s.stderr)
	}
	allowConnections(t, dbURL, false)
	code := s.wait(t)
	close(stop)
	if code == exitOK {
		t.Fatalf("exit %d after losing the database; stderr:\n%s", code, s.stderr)
	}
	if !strings.Contains(s.stderr.String(), `"code":"store_connection_lost"`) {
		t.Fatalf("no store_connection_lost in the logs:\n%s", s.stderr)
	}
	assertNoSurvivors(t, <-pids)
	assertLockFree(t, p.data)

	allowConnections(t, dbURL, true)
	again := startServerProcess(t, dbURL, p, 0o022, envWorkers+"=2")
	again.waitHealthy(t)
	waitFor(t, "the album to be published after the restart", func() bool {
		return queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision > 0 AND published_revision = revision`) == 1 &&
			idle(t, db)
	})
	if err := again.cmd.Process.Signal(unix.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := again.wait(t); code != exitOK {
		t.Fatalf("exit %d; stderr:\n%s", code, again.stderr)
	}
}

// §12.2 "SIGTERM/SIGKILL con più worker e helper attivi": SIGTERM while two
// workers run tools; the builds are cancelled, no helper survives, the
// process exits 0 with the lock released last. A SIGKILL leaves no helper
// either (Pdeathsig, §8.5), and the next boot recovers.
func TestProcessSignalsWithHelpersActive(t *testing.T) {
	for _, sig := range []unix.Signal{unix.SIGTERM, unix.SIGKILL} {
		t.Run(sig.String(), func(t *testing.T) {
			dbURL := pgtest.EmptyDB(t)
			p := testPaths(t)
			writeAlbum(t, p.imports, "One", "Artist", "One", 3, 20)
			writeAlbum(t, p.imports, "Two", "Other", "Two", 3, 20)
			s := startServerProcess(t, dbURL, p, 0o022, envWorkers+"=2")
			s.waitHealthy(t)
			db := dbPool(t, dbURL)
			if _, err := catalogOn(t, db).CreateImportBatch(context.Background(), uuid.New(), ""); err != nil {
				t.Fatal(err)
			}
			first, stop := make(chan struct{}), make(chan struct{})
			pids := watchTools(s.cmd.Process.Pid, first, stop)
			select {
			case <-first:
			case <-time.After(2 * time.Minute):
				t.Fatalf("no tool started; stderr:\n%s", s.stderr)
			}
			if err := s.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			<-s.done
			close(stop)
			if sig == unix.SIGTERM {
				if code := s.wait(t); code != exitOK {
					t.Fatalf("exit %d after SIGTERM; stderr:\n%s", code, s.stderr)
				}
				assertShutdownOrder(t, s.stderr, true)
			}
			assertNoSurvivors(t, <-pids)
			assertLockFree(t, p.data)

			again := startServerProcess(t, dbURL, p, 0o022, envWorkers+"=2")
			again.waitHealthy(t)
			waitFor(t, "both albums after the restart", func() bool {
				return queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision > 0 AND published_revision = revision`) == 2 &&
					idle(t, db)
			})
			if n := queryInt(t, db, `SELECT count(*) FROM jobs WHERE state = 'failed'`); n != 0 {
				t.Fatalf("%d failed jobs after the restart; stderr:\n%s", n, again.stderr)
			}
		})
	}
}

// N-135's other half: a step-4 failure that is not an illegal state fails
// the boot as before (exit 1), because it may be transient and the restart
// retries it. A legal journal whose staging cannot be moved (its build
// directory is read-only: EACCES on the rename) makes the recovery fail
// with publish_io: run returns that code, nothing is suspended, no worker
// starts, the lock is released, the journal stays.
func TestBootFailsOnARecoveryIOError(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	bootOnce(t, dbURL, p)
	db := dbPool(t, dbURL)
	id := seedAlbum(t, db, "Artist", "Album")
	var ticket int64
	if err := db.QueryRow(context.Background(),
		`UPDATE jobs SET state = 'running', claimed = requested WHERE album_id = $1 RETURNING claimed`, id).Scan(&ticket); err != nil {
		t.Fatal(err)
	}
	// A complete staging with its receipt: the journal is legal.
	build := store.NewID()
	staging := filepath.Join(p.data, "work", filepath.FromSlash(render.StagingDir(build)))
	body := []byte("audio")
	writeFile(t, filepath.Join(staging, "01 - One.flac"), string(body))
	sum := sha256.Sum256(body)
	receipt, err := render.Receipt{AlbumID: id, BuildID: build, AlbumRevision: 1, RenderVersion: render.Version,
		Files: []render.ReceiptFile{{RelativePath: "01 - One.flac", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(staging, render.ReceiptName), string(receipt))
	execSQL(t, db, `INSERT INTO publication (id, album_id, ticket, revision, renderer, build_id, receipt_hash, new_path)
		VALUES (1, $1, $2, 1, $3, $4, $5, 'Artist/Album')`, id, ticket, render.Version, build, render.ReceiptHash(receipt))
	buildDir := filepath.Dir(staging)
	if err := os.Chmod(buildDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(buildDir, 0o755); err != nil {
			t.Error(err)
		}
	})
	library := libraryEntries(t, p)

	d := startDaemon(t, testConfig(dbURL), p)
	err = d.wait(t)
	if codeOf(err) != publish.CodeIO {
		t.Fatalf("run: %v (code %q), want %s", err, codeOf(err), publish.CodeIO)
	}
	for _, msg := range []string{"publishing suspended: the pending publication is in an illegal state", "workers started",
		"work cleaned", "ready"} {
		if d.logs.has(t, msg) {
			t.Fatalf("%q logged after a recovery I/O error", msg)
		}
	}
	assertLockFree(t, p.data)
	if n := queryInt(t, db, `SELECT count(*) FROM publication`); n != 1 {
		t.Fatal("the journal is gone")
	}
	// The artist directory created for the failed rename was removed again
	// (N-144): the same entries (library/'s own mtime did change).
	if got := libraryEntries(t, p); !slices.Equal(got, library) {
		t.Fatalf("library/ holds %q, it held %q", got, library)
	}
}

// libraryEntries lists every path under library/, sorted.
func libraryEntries(t *testing.T, p paths) []string {
	t.Helper()
	var out []string
	for r := range hashTree(t, filepath.Join(p.data, "library")) {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}
