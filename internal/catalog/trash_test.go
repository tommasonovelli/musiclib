package catalog_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
)

// removed brings a trashed album to the end of its removal, as FINALIZE
// does: the render row is gone, nothing is published, the claims follow.
func (e *env) removed(album uuid.UUID) {
	e.t.Helper()
	e.exec(`DELETE FROM jobs WHERE kind = 'render' AND album_id = $1`, album)
	e.removalPublished(album)
}

func (e *env) trash(album uuid.UUID) {
	e.t.Helper()
	if _, _, err := e.svc.TrashAlbum(context.Background(), album, e.album(album).Revision); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) emptyTrash(wantDeleted, wantWaiting int) {
	e.t.Helper()
	deleted, waiting, err := e.svc.EmptyTrash(context.Background())
	if err != nil {
		e.t.Fatalf("EmptyTrash: %v", err)
	}
	if deleted != wantDeleted || waiting != wantWaiting {
		e.t.Fatalf("EmptyTrash = %d deleted, %d waiting; want %d, %d", deleted, waiting, wantDeleted, wantWaiting)
	}
}

// rows counts every row that refers to album, its own included.
func (e *env) rows(album uuid.UUID) int {
	e.t.Helper()
	return e.count(`SELECT (SELECT count(*) FROM albums WHERE id = $1) + (SELECT count(*) FROM tracks WHERE album_id = $1)
		+ (SELECT count(*) FROM attachments WHERE album_id = $1) + (SELECT count(*) FROM path_claims WHERE album_id = $1)
		+ (SELECT count(*) FROM jobs WHERE album_id = $1 OR result_album_id = $1)
		+ (SELECT count(*) FROM publication WHERE album_id = $1)`, album)
}

// Emptying the trash deletes the catalog rows of the trashed albums whose
// removal is complete, never a blob; an album whose removal is pending,
// failed, still published or in the journal waits; active albums are not
// touched; an artist left without albums goes, one with an album stays.
func TestEmptyTrash(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.emptyTrash(0, 0)

	kind := e.importAlbum("Miles Davis", "Kind of Blue")          // purged, Miles keeps an album
	sketches := e.importAlbum("Miles Davis", "Sketches of Spain") // active
	portrait := e.importAlbum("Bill Evans", "Portrait in Jazz")   // purged with its artist
	pending := e.importAlbum("Chet Baker", "Chet")                // removal pending
	failed := e.importAlbum("Art Pepper", "Meets the Rhythm Section")
	published := e.importAlbum("Sonny Rollins", "Saxophone Colossus")
	journal := e.importAlbum("Dave Brubeck", "Time Out")
	e.emptyTrash(0, 0) // nothing in the trash: active albums are never touched

	e.published(published, "Sonny Rollins/Saxophone Colossus")
	for _, id := range []uuid.UUID{kind, portrait, pending, failed, published, journal} {
		e.trash(id)
	}
	e.removed(kind)
	e.removed(portrait)
	e.exec(`UPDATE jobs SET state = 'failed', error_code = 'render_io', error_message = 'disk' WHERE kind = 'render' AND album_id = $1`, failed)
	// Still published, its render row gone (a removal never published).
	e.exec(`DELETE FROM jobs WHERE kind = 'render' AND album_id = $1`, published)
	// A removal between PREPARE and FINALIZE.
	e.removed(journal)
	e.setJournal(journal, nil, nil)

	miles, bill := e.album(kind).ArtistID, e.album(portrait).ArtistID
	importOf := func(album uuid.UUID) uuid.UUID {
		var id uuid.UUID
		if err := e.db.QueryRow(ctx, `SELECT id FROM jobs WHERE kind = 'import' AND result_album_id = $1`, album).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	kindJob, portraitJob := importOf(kind), importOf(portrait)
	blobs := e.count(`SELECT count(*) FROM blobs`)
	if e.rows(kind) == 0 || e.count(`SELECT count(*) FROM attachments WHERE album_id = $1`, kind) == 0 {
		t.Fatal("the fixture has no attachments")
	}
	type snapshot struct {
		revision int64
		tracks   int
	}
	before := map[uuid.UUID]snapshot{}
	for _, id := range []uuid.UUID{sketches, pending, failed, published, journal} {
		before[id] = snapshot{e.album(id).Revision, len(e.tracks(id))}
	}
	renders := e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`)

	e.emptyTrash(2, 4)

	for _, id := range []uuid.UUID{kind, portrait} {
		if n := e.rows(id); n != 0 {
			t.Errorf("album %s still has %d rows", id, n)
		}
		if _, err := e.svc.GetAlbum(ctx, id); catalog.Code(err) != catalog.CodeAlbumNotFound {
			t.Errorf("GetAlbum of a purged album: %v", err)
		}
	}
	if got := e.count(`SELECT count(*) FROM blobs`); got != blobs {
		t.Fatalf("blobs %d, want %d: emptying the trash never deletes an original", got, blobs)
	}
	// The import reports keep their outcome, without the album.
	for _, id := range []uuid.UUID{kindJob, portraitJob} {
		if j := e.job(id); j.State != string(jobs.StateDone) || j.ResultAlbumID != nil || j.SourceRel == nil {
			t.Errorf("import job %s after the purge: %+v", id, j)
		}
	}
	if !e.artistExists(miles) || e.artistExists(bill) || e.orphans() != 0 {
		t.Fatalf("artists: Miles %v, Bill %v, orphans %d", e.artistExists(miles), e.artistExists(bill), e.orphans())
	}
	for id, b := range before {
		a := e.album(id)
		if a.Revision != b.revision || len(e.tracks(id)) != b.tracks {
			t.Errorf("album %s changed: revision %d, %d tracks; want %+v", id, a.Revision, len(e.tracks(id)), b)
		}
	}
	if got := e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`); got != renders {
		t.Fatalf("render jobs %d, want %d: emptying the trash enqueues nothing", got, renders)
	}
	if e.count(`SELECT count(*) FROM publication`) != 1 {
		t.Fatal("the journal changed")
	}
	e.emptyTrash(0, 4)

	// Once their removals complete, the waiting albums go too.
	e.exec(`DELETE FROM publication`)
	e.removed(pending)
	e.removed(failed)
	e.removed(published)
	e.emptyTrash(4, 0)
	if e.count(`SELECT count(*) FROM albums`) != 1 || e.count(`SELECT count(*) FROM artists`) != 1 || e.orphans() != 0 {
		t.Fatal("only Sketches of Spain and Miles Davis should be left")
	}
	e.emptyTrash(0, 0)
}

// The album's import fingerprint goes with its row: after the purge, the
// same folder is imported again instead of being skipped. The skipped
// import that pointed at the purged album keeps its report.
func TestEmptyTrashAllowsReimport(t *testing.T) {
	e := newEnv(t)
	c := candidate(e.runningImport(), "Miles Davis", "Kind of Blue")
	first := e.commit(c)
	second := e.runningImport()
	c.Attempt = second
	skipped := e.commit(c)
	if first.State != jobs.StateDone || skipped.State != jobs.StateSkipped || skipped.AlbumID != first.AlbumID {
		t.Fatalf("import %+v, again %+v", first, skipped)
	}
	e.trash(first.AlbumID)
	e.removed(first.AlbumID)
	e.emptyTrash(1, 0)
	if j := e.job(second.JobID); j.State != string(jobs.StateSkipped) || j.ResultAlbumID != nil || j.ErrorCode == nil {
		t.Fatalf("the skipped import after the purge: %+v", j)
	}

	c.Attempt = e.runningImport()
	again := e.commit(c)
	if again.State != jobs.StateDone || again.AlbumID == first.AlbumID {
		t.Fatalf("import after the purge: %+v", again)
	}
	if got := len(e.tracks(again.AlbumID)); got != 2 {
		t.Fatalf("the new album has %d tracks", got)
	}
}

// The purge decision and every change that could make a trashed album
// unpurgeable (a render request, a restore) are serialized by the catalog
// lock: either the change comes first and the album waits, or the purge
// comes first and the change finds no album. Never an album left half
// deleted, a render row without its album, or an error of the purge.
func TestEmptyTrashRacesChanges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := range 20 {
		id := e.importAlbum("Miles Davis", "Kind of Blue")
		e.trash(id)
		e.removed(id)
		rev := e.album(id).Revision

		var (
			wg               sync.WaitGroup
			deleted, waiting int
			purgeErr, chErr  error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			deleted, waiting, purgeErr = e.svc.EmptyTrash(ctx)
		}()
		restore := i%2 == 0
		go func() {
			defer wg.Done()
			if restore {
				_, _, chErr = e.svc.RestoreAlbum(ctx, id, rev)
			} else {
				_, _, chErr = e.svc.RequestRender(ctx, id, rev)
			}
		}()
		wg.Wait()
		if purgeErr != nil {
			t.Fatalf("round %d: EmptyTrash: %v", i, purgeErr)
		}
		switch {
		case deleted == 1 && waiting == 0:
			if catalog.Code(chErr) != catalog.CodeAlbumNotFound || e.rows(id) != 0 {
				t.Fatalf("round %d: purged, then the change: %v, %d rows left", i, chErr, e.rows(id))
			}
		case deleted == 0 && chErr == nil:
			// The change came first: a restored album is not in the trash;
			// a render request makes the album wait.
			wantWaiting := 1
			if restore {
				wantWaiting = 0
			}
			if waiting != wantWaiting || e.count(`SELECT count(*) FROM albums WHERE id = $1`, id) != 1 {
				t.Fatalf("round %d: the change first, then %d waiting", i, waiting)
			}
		default:
			t.Fatalf("round %d: EmptyTrash %d deleted, %d waiting; change %v", i, deleted, waiting, chErr)
		}
		if e.count(`SELECT count(*) FROM jobs j WHERE j.kind = 'render' AND NOT EXISTS (SELECT 1 FROM albums a WHERE a.id = j.album_id)`) != 0 ||
			e.orphans() != 0 {
			t.Fatalf("round %d: a render row without its album, or an orphan artist", i)
		}
		// Leave the next round a clean trash and a free folder.
		if e.count(`SELECT count(*) FROM albums WHERE id = $1`, id) == 1 {
			e.exec(`DELETE FROM jobs WHERE kind = 'render' AND album_id = $1`, id)
			if e.album(id).DeletedAt == nil {
				e.trash(id)
				e.exec(`DELETE FROM jobs WHERE kind = 'render' AND album_id = $1`, id)
			}
			e.removalPublished(id)
			e.emptyTrash(1, 0)
		}
	}
}
