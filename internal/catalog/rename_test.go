package catalog_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

// §4.3, §5.3: an artist rename bumps the artist and every album, trashed
// ones included, re-derives every claim and enqueues every render, all or
// nothing.
func TestRenameArtist(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a1 := e.importAlbum("X", "T1")
	a2 := e.importAlbum("X", "T2")
	a3 := e.importAlbum("X", "T3")
	artist := e.album(a1).ArtistID
	if _, _, err := e.svc.TrashAlbum(ctx, a3, 1); err != nil {
		t.Fatal(err)
	}
	// Another album's published path is where A2 would go.
	c := e.importAlbum("Z", "T2")
	e.published(c, "Y/T2")
	e.clearRenders()

	type state struct {
		artist store.Artist
		albums []store.Album
		claims []map[string]string
		jobs   int
	}
	snapshot := func() state {
		s := state{artist: e.artist(artist), jobs: e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`)}
		for _, id := range []uuid.UUID{a1, a2, a3} {
			s.albums = append(s.albums, e.album(id))
			s.claims = append(s.claims, e.claims(id))
		}
		return s
	}
	before := snapshot()
	ce := wantCode(t, func() error { _, _, err := e.svc.RenameArtist(ctx, artist, 1, "Y"); return err }(), catalog.CodePathReserved)
	if ce.Details.AlbumID != c {
		t.Errorf("details %+v, want album %s", ce.Details, c)
	}
	if after := snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused rename changed something:\n%+v\n%+v", before, after)
	}

	wakes := e.wakes.Load()
	rev, changed, err := e.svc.RenameArtist(ctx, artist, 1, "W")
	if err != nil || !changed || rev != 2 {
		t.Fatalf("rename = %d, %v, %v", rev, changed, err)
	}
	if ar := e.artist(artist); ar.Name != "W" || ar.FolderKey != "w" || ar.Revision != 2 {
		t.Errorf("artist %+v", ar)
	}
	for i, id := range []uuid.UUID{a1, a2, a3} {
		if al := e.album(id); al.Revision != before.albums[i].Revision+1 {
			t.Errorf("album %d revision %d, want %d", i, al.Revision, before.albums[i].Revision+1)
		}
		if _, ok := e.renderJob(id); !ok {
			t.Errorf("album %d: no render enqueued", i)
		}
	}
	for i, want := range []map[string]string{{"w/t1": "W/T1"}, {"w/t2": "W/T2"}, {}} {
		if got := e.claims([]uuid.UUID{a1, a2, a3}[i]); !reflect.DeepEqual(got, want) {
			t.Errorf("album %d claims %v, want %v", i, got, want)
		}
	}
	if e.wakes.Load() != wakes+1 {
		t.Error("no wake-up after the rename")
	}

	// The same name is a no-op.
	if rev, changed, err := e.svc.RenameArtist(ctx, artist, 2, " W "); err != nil || changed || rev != 2 {
		t.Errorf("same name = %d, %v, %v; want a no-op", rev, changed, err)
	}
	if _, _, err := e.svc.RenameArtist(ctx, artist, 0, "V"); catalog.Code(err) != catalog.CodePreconditionRequired {
		t.Errorf("no If-Match: %v", err)
	}
	ce = wantCode(t, func() error { _, _, err := e.svc.RenameArtist(ctx, artist, 1, "V"); return err }(), catalog.CodePreconditionFailed)
	if ce.Details.Revision != 2 {
		t.Errorf("412 details %+v", ce.Details)
	}
	if _, _, err := e.svc.RenameArtist(ctx, store.NewID(), 1, "V"); catalog.Code(err) != catalog.CodeArtistNotFound {
		t.Errorf("unknown artist: %v", err)
	}
	if _, _, err := e.svc.RenameArtist(ctx, artist, 2, "\t"); catalog.Code(err) != names.CodeTextControlChar && catalog.Code(err) != names.CodeTextEmpty {
		t.Errorf("invalid name: %v", err)
	}
}

// Merges are never implicit (§5.3, §7.6): a rename onto another artist's
// folder, by casefold or by sanitization, is a conflict naming both.
func TestRenameArtistFolderConflict(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	x := e.album(e.importAlbum("X", "One")).ArtistID
	e.importAlbum("AC_DC", "Two")
	e.importAlbum("Zappa", "Three")
	for _, name := range []string{"ZAPPA", "AC/DC"} {
		ce := wantCode(t, func() error { _, _, err := e.svc.RenameArtist(ctx, x, 1, name); return err }(), catalog.CodeArtistFolderConflict)
		if len(ce.Details.Names) != 2 || ce.Details.Names[0] != name {
			t.Errorf("%s: names %v", name, ce.Details.Names)
		}
	}
	// A case-only rename of oneself is not a conflict.
	if _, changed, err := e.svc.RenameArtist(ctx, x, 1, "x"); err != nil || !changed {
		t.Errorf("case-only rename: %v %v", changed, err)
	}
}
