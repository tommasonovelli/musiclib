package catalog_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// duration reads a blob's duration_ms: -1 for NULL.
func (e *env) duration(hash string) int64 {
	e.t.Helper()
	var ms *int64
	if err := e.db.QueryRow(context.Background(), `SELECT duration_ms FROM blobs WHERE hash = $1`, hash).Scan(&ms); err != nil {
		e.t.Fatal(err)
	}
	if ms == nil {
		return -1
	}
	return *ms
}

func (e *env) recordDurations(d map[string]int64) error {
	return store.InCatalogTx(context.Background(), e.db, func(tx *store.CatalogTx) error {
		return catalog.RecordDurations(context.Background(), tx, d)
	})
}

// N-300: the import records an audio blob's duration; a known duration is
// never overwritten, by a later import of the same blob or by a render's
// report; an unknown one is filled by either; nothing but an audio blob
// has one.
func TestBlobDurations(t *testing.T) {
	e := newEnv(t)
	c := candidate(e.runningImport(), "Miles Davis", "Kind of Blue")
	audio1, audio2 := c.Blobs[0].Hash, c.Blobs[1].Hash
	c.Blobs[0].DurationMS = ptr(int64(545_000))
	if out := e.commit(c); out.State != jobs.StateDone {
		t.Fatalf("import %+v", out)
	}
	if e.duration(audio1) != 545_000 || e.duration(audio2) != -1 {
		t.Fatalf("durations %d, %d; want 545000 and unknown", e.duration(audio1), e.duration(audio2))
	}
	v, err := e.svc.GetAlbum(context.Background(), e.albumByTitle("Kind of Blue"))
	if err != nil || v.Tracks[0].DurationMS == nil || *v.Tracks[0].DurationMS != 545_000 || v.Tracks[1].DurationMS != nil {
		t.Fatalf("the album view: %+v %v", v.Tracks, err)
	}

	// The same audio in another album, with other durations (another
	// probe): the known one stays, the unknown one is filled.
	c2 := candidate(e.runningImport(), "Miles Davis", "Kind of Blue (Legacy)")
	c2.Blobs[0].Hash, c2.Blobs[1].Hash = audio1, audio2
	c2.Tracks[0].BlobHash, c2.Tracks[1].BlobHash = audio1, audio2
	c2.Blobs[0].DurationMS, c2.Blobs[1].DurationMS = ptr(int64(1)), ptr(int64(0))
	c2.Blobs[0].Size, c2.Blobs[1].Size = c.Blobs[0].Size, c.Blobs[1].Size
	if out := e.commit(c2); out.State != jobs.StateDone {
		t.Fatalf("second import %+v", out)
	}
	if e.duration(audio1) != 545_000 || e.duration(audio2) != 0 {
		t.Fatalf("durations %d, %d; want 545000 kept and 0 filled", e.duration(audio1), e.duration(audio2))
	}

	// A render's report: an unknown duration is filled, a known one kept,
	// a blob that is not audio or not there left alone.
	c3 := candidate(e.runningImport(), "Bill Evans", "Portrait in Jazz")
	if out := e.commit(c3); out.State != jobs.StateDone {
		t.Fatalf("third import %+v", out)
	}
	lrc, missing := c3.Blobs[2].Hash, newHash()
	err = e.recordDurations(map[string]int64{c3.Blobs[0].Hash: 200_000, audio1: 7, lrc: 5, missing: 9, c3.Blobs[1].Hash: -1})
	if err != nil {
		t.Fatal(err)
	}
	if e.duration(c3.Blobs[0].Hash) != 200_000 || e.duration(audio1) != 545_000 || e.duration(lrc) != -1 ||
		e.duration(c3.Blobs[1].Hash) != -1 || e.count(`SELECT count(*) FROM blobs WHERE hash = $1`, missing) != 0 {
		t.Fatal("RecordDurations wrote what it must not")
	}
	// No new revision, no render: the duration is not the album's output.
	if e.album(e.albumByTitle("Portrait in Jazz")).Revision != 1 {
		t.Fatal("a recorded duration bumped the album")
	}

	// The schema refuses what the service would never write.
	for _, sql := range []string{
		`UPDATE blobs SET duration_ms = -1 WHERE hash = '` + audio1 + `'`,
		`UPDATE blobs SET duration_ms = 5 WHERE hash = '` + lrc + `'`,
	} {
		if _, err := e.db.Exec(context.Background(), sql); err == nil {
			t.Errorf("%s: accepted", sql)
		}
	}
}

// N-300, N-307: a blob already recorded with an unknown format (N-118: the
// same bytes kept earlier as an extra file) gets its format and then its
// duration from the import that uses it as a track: blobs_duration_check
// and SetBlobDuration want the audio format first.
func TestBlobDurationAfterUnknownFormat(t *testing.T) {
	e := newEnv(t)
	c := candidate(e.runningImport(), "Miles Davis", "Kind of Blue")
	e.exec(`INSERT INTO blobs (hash, size, format, created_at) VALUES ($1, $2, NULL, now())`, c.Blobs[0].Hash, c.Blobs[0].Size)
	c.Blobs[0].DurationMS = ptr(int64(545_000))
	if out := e.commit(c); out.State != jobs.StateDone {
		t.Fatalf("import %+v", out)
	}
	if n := e.count(`SELECT count(*) FROM blobs WHERE hash = $1 AND format = 'flac'`, c.Blobs[0].Hash); n != 1 || e.duration(c.Blobs[0].Hash) != 545_000 {
		t.Fatalf("format recorded %d, duration %d; want flac and 545000", n, e.duration(c.Blobs[0].Hash))
	}
}

// N-300: the candidate's durations are validated with its blobs.
func TestBlobDurationValidation(t *testing.T) {
	e := newEnv(t)
	for name, mutate := range map[string]func(c *catalog.ImportCandidate){
		"negative":  func(c *catalog.ImportCandidate) { c.Blobs[0].DurationMS = ptr(int64(-5)) },
		"not audio": func(c *catalog.ImportCandidate) { c.Blobs[3].DurationMS = ptr(int64(5)) },
	} {
		t.Run(name, func(t *testing.T) {
			c := candidate(e.runningImport(), "Artist", name)
			mutate(&c)
			out := e.commit(c)
			if out.State != jobs.StateFailed || out.ErrorCode != catalog.CodeInvalidBlob {
				t.Fatalf("outcome %+v, want failed %s", out, catalog.CodeInvalidBlob)
			}
		})
	}
}

func (e *env) albumByTitle(title string) (id uuid.UUID) {
	e.t.Helper()
	if err := e.db.QueryRow(context.Background(), `SELECT id FROM albums WHERE title = $1`, title).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}
