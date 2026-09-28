package catalog_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/names"
)

// orphans counts the artists without any album, active or trashed: never
// more than zero after a change the owner's rule N-297 covers.
func (e *env) orphans() int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM artists ar WHERE NOT EXISTS (SELECT 1 FROM albums al WHERE al.artist_id = ar.id)`)
}

func (e *env) artistExists(id uuid.UUID) bool {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM artists WHERE id = $1`, id) == 1
}

// moveTo saves album with artist as its artist, at its current revision.
func (e *env) moveTo(album, artist uuid.UUID) error {
	e.t.Helper()
	u := e.update(album)
	u.ArtistID = artist
	_, _, err := e.svc.UpdateAlbum(context.Background(), album, e.album(album).Revision, u)
	return err
}

// N-297: an artist whose last album, active or trashed, leaves it is
// deleted by the change that moved the album; one that keeps an album, even
// a trashed one, stays.
func TestLastAlbumLeavesArtist(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	kind := e.importAlbum("Miles Davis", "Kind of Blue")
	sketches := e.importAlbum("Miles Davis", "Sketches of Spain")
	portrait := e.importAlbum("Bill Evans", "Portrait in Jazz")
	miles, bill := e.album(kind).ArtistID, e.album(portrait).ArtistID

	// Miles keeps Sketches of Spain, in the trash: he stays.
	if _, _, err := e.svc.TrashAlbum(ctx, sketches, e.album(sketches).Revision); err != nil {
		t.Fatal(err)
	}
	if err := e.moveTo(kind, bill); err != nil {
		t.Fatal(err)
	}
	if !e.artistExists(miles) || e.artist(miles).Name != "Miles Davis" {
		t.Fatal("an artist with a trashed album was deleted")
	}
	// A save that changes something else keeps the artist too.
	u := e.update(sketches)
	u.Title = "Sketches of Spain (Mono)"
	if _, _, err := e.svc.UpdateAlbum(ctx, sketches, e.album(sketches).Revision, u); err != nil || !e.artistExists(miles) {
		t.Fatalf("a rename of the artist's last album: %v, artist kept %v", err, e.artistExists(miles))
	}
	// The trashed album, his last, leaves him (§4.3: a trashed album can be
	// edited): he is deleted in the same transaction.
	if err := e.moveTo(sketches, bill); err != nil {
		t.Fatal(err)
	}
	if e.artistExists(miles) || e.orphans() != 0 {
		t.Fatalf("Miles Davis is still there with no album (orphans %d)", e.orphans())
	}
	if _, err := e.svc.GetArtist(ctx, miles); catalog.Code(err) != catalog.CodeArtistNotFound {
		t.Fatalf("GetArtist of the deleted artist: %v", err)
	}
	// His folder is the render's business: the moved albums have their
	// renders and new claims under Bill Evans.
	for _, id := range []uuid.UUID{kind, sketches} {
		if _, ok := e.renderJob(id); !ok {
			t.Errorf("album %s has no render after its move", id)
		}
	}
	if got := e.claims(kind); len(got) != 1 || got["bill evans/kind of blue"] != "Bill Evans/Kind of Blue" {
		t.Errorf("claims %v", got)
	}

	// Moving to an artist that no longer exists is refused and leaves
	// everything as it was.
	if err := e.moveTo(portrait, miles); catalog.Code(err) != catalog.CodeArtistNotFound {
		t.Fatalf("a move to the deleted artist: %v", err)
	}
	if e.album(portrait).ArtistID != bill || !e.artistExists(bill) {
		t.Fatal("a refused move changed the album or its artist")
	}
}

// N-298: an album update can name a new artist, created in its own
// transaction; a failed update leaves no artist behind, a name that exists
// is refused with the existing artist, and moving back deletes it.
func TestUpdateAlbumNewArtist(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	kind := e.importAlbum("Miles Davis", "Kind of Blue")
	e.importAlbum("Miles Davis", "Sketches of Spain")
	e.importAlbum("AC_DC", "Back in Black")
	miles := e.album(kind).ArtistID
	artists := e.count(`SELECT count(*) FROM artists`)

	withNew := func(name string) catalog.AlbumUpdate {
		u := e.update(kind)
		u.ArtistID, u.NewArtist = uuid.Nil, &name
		return u
	}
	// Refused before, or inside, the transaction: no artist is created.
	for _, tc := range []struct {
		name   string
		mutate func(u *catalog.AlbumUpdate)
		rev    int64
		code   string
	}{
		{"stale revision", nil, 7, catalog.CodePreconditionFailed},
		{"no revision", nil, 0, catalog.CodePreconditionRequired},
		{"invalid year", func(u *catalog.AlbumUpdate) { u.Year = ptr(0) }, 1, catalog.CodeInvalidYear},
		// Found inside the transaction, after the artist was inserted.
		{"track list", func(u *catalog.AlbumUpdate) { u.Tracks = u.Tracks[:1] }, 1, catalog.CodeTrackListMismatch},
		{"empty name", func(u *catalog.AlbumUpdate) { u.NewArtist = ptr("  ") }, 1, names.CodeTextEmpty},
		{"both", func(u *catalog.AlbumUpdate) { u.ArtistID = miles }, 1, catalog.CodeInvalidArgument},
		{"neither", func(u *catalog.AlbumUpdate) { u.NewArtist = nil }, 1, catalog.CodeInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := withNew("Cannonball Adderley")
			if tc.mutate != nil {
				tc.mutate(&u)
			}
			_, _, err := e.svc.UpdateAlbum(ctx, kind, tc.rev, u)
			wantCode(t, err, tc.code)
			if e.count(`SELECT count(*) FROM artists`) != artists || e.album(kind).Revision != 1 {
				t.Fatal("a refused update left an artist behind or changed the album")
			}
		})
	}
	// A name that exists (after NFC, trim, casefold), or that shares an
	// existing artist's folder: 409 with the existing artist, no merge.
	for name, want := range map[string]string{" miles davis ": catalog.CodeArtistExists, "AC/DC": catalog.CodeArtistFolderConflict} {
		_, _, err := e.svc.UpdateAlbum(ctx, kind, 1, withNew(name))
		ce := wantCode(t, err, want)
		if ce.Details.ArtistID == uuid.Nil || len(ce.Details.Names) != 2 {
			t.Errorf("%q: details %+v", name, ce.Details)
		}
		if e.count(`SELECT count(*) FROM artists`) != artists || e.album(kind).ArtistID != miles {
			t.Fatalf("%q: a conflict changed something", name)
		}
	}

	// The new artist and the album's move, one transaction.
	rev, changed, err := e.svc.UpdateAlbum(ctx, kind, 1, withNew(" Cannonball Adderley "))
	if err != nil || !changed || rev != 2 {
		t.Fatalf("with a new artist: %d %v %v", rev, changed, err)
	}
	cannonball := e.album(kind).ArtistID
	if a := e.artist(cannonball); a.Name != "Cannonball Adderley" || a.FolderKey != "cannonball adderley" || a.Revision != 1 {
		t.Fatalf("the new artist %+v", a)
	}
	if !e.artistExists(miles) {
		t.Fatal("Miles Davis still has Sketches of Spain")
	}
	// Back to Miles Davis: Cannonball Adderley is left without albums and
	// goes.
	if err := e.moveTo(kind, miles); err != nil {
		t.Fatal(err)
	}
	if e.artistExists(cannonball) || e.orphans() != 0 {
		t.Fatal("the artist created with the album stayed without it")
	}
}

// N-297 under concurrency: every change runs under the one catalog lock
// (§5.3), so two albums leaving the same artist at once delete it exactly
// once, and an album arriving while the last one leaves either keeps the
// artist or finds it gone (CodeArtistNotFound); never an orphan, never a
// database error.
func TestOrphanArtistConcurrency(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	home := e.album(e.importAlbum("Home", "Base")).ArtistID
	for round := range 8 {
		// Two albums leave the same artist at once.
		a := e.importAlbum("Leaving", "A"+string(rune('a'+round)))
		b := e.importAlbum("Leaving", "B"+string(rune('a'+round)))
		leaving := e.album(a).ArtistID
		ua, ub := e.update(a), e.update(b)
		ua.ArtistID, ub.ArtistID = home, home
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, job := range []struct {
			id uuid.UUID
			u  catalog.AlbumUpdate
		}{{a, ua}, {b, ub}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, errs[i] = e.svc.UpdateAlbum(ctx, job.id, 1, job.u)
			}()
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("round %d: two albums leaving: %v, %v", round, errs[0], errs[1])
		}
		if e.artistExists(leaving) || e.orphans() != 0 {
			t.Fatalf("round %d: the artist both albums left is still there", round)
		}

		// One album leaves an artist while another arrives at it.
		c := e.importAlbum("Pivot", "C"+string(rune('a'+round)))
		d := e.importAlbum("Elsewhere", "D"+string(rune('a'+round)))
		pivot, elsewhere := e.album(c).ArtistID, e.album(d).ArtistID
		uc, ud := e.update(c), e.update(d)
		uc.ArtistID, ud.ArtistID = home, pivot
		// And an import arrives at the leaving artist by name.
		cand := candidate(e.runningImport(), "Pivot", "E"+string(rune('a'+round)))
		var (
			out       catalog.ImportOutcome
			importErr error
		)
		wg.Add(3)
		go func() { defer wg.Done(); _, _, errs[0] = e.svc.UpdateAlbum(ctx, c, 1, uc) }()
		go func() { defer wg.Done(); _, _, errs[1] = e.svc.UpdateAlbum(ctx, d, 1, ud) }()
		go func() { defer wg.Done(); out, importErr = e.svc.CommitImport(ctx, cand) }()
		wg.Wait()
		if errs[0] != nil || importErr != nil || out.State != "done" {
			t.Fatalf("round %d: the leaving album: %v; the import: %+v %v", round, errs[0], out, importErr)
		}
		e1 := out.AlbumID
		switch catalog.Code(errs[1]) {
		case "":
			// It arrived first, or after the import: the artist stays with it.
			if e.album(d).ArtistID != pivot || !e.artistExists(pivot) || e.artistExists(elsewhere) {
				t.Fatalf("round %d: the arriving album was saved but the artists are wrong", round)
			}
		case catalog.CodeArtistNotFound:
			// It came after the leaving one deleted the artist and before
			// the import made a new one of the same name.
			if e.album(d).ArtistID != elsewhere || e.artistExists(pivot) {
				t.Fatalf("round %d: refused, but something changed", round)
			}
		default:
			t.Fatalf("round %d: the arriving album: %v", round, errs[1])
		}
		if e.artist(e.album(e1).ArtistID).Name != "Pivot" {
			t.Fatalf("round %d: the import's artist", round)
		}
		if e.orphans() != 0 {
			t.Fatalf("round %d: %d artists without albums", round, e.orphans())
		}
		// Clear the round: its albums go to Home, and their artists with them.
		for _, id := range []uuid.UUID{d, e1} {
			if e.album(id).ArtistID != home {
				if err := e.moveTo(id, home); err != nil {
					t.Fatal(err)
				}
			}
		}
		if n := e.count(`SELECT count(*) FROM artists`); n != 1 || e.orphans() != 0 {
			t.Fatalf("round %d: %d artists left, want Home only", round, n)
		}
	}
}
