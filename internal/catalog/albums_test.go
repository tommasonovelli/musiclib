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

// §4.3 and §10.1: no bump without an effective change; a change bumps and
// enqueues atomically; the If-Match revision is checked in the transaction.
func TestUpdateAlbumRevisions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Artist", "Album")
	e.clearRenders()
	wakes := e.wakes.Load()

	u := e.update(id)
	if _, _, err := e.svc.UpdateAlbum(ctx, id, 0, u); catalog.Code(err) != catalog.CodePreconditionRequired {
		t.Errorf("no If-Match: %v, want %s", err, catalog.CodePreconditionRequired)
	}
	ce := wantCode(t, func() error { _, _, err := e.svc.UpdateAlbum(ctx, id, 7, u); return err }(), catalog.CodePreconditionFailed)
	if ce.Details.Revision != 1 {
		t.Errorf("412 details %+v, want the current revision 1", ce.Details)
	}

	// Saving the album as it is, with texts that normalize to the stored
	// ones, changes nothing.
	u.Title = " Album "
	u.Tracks[0].Title = "So What "
	rev, changed, err := e.svc.UpdateAlbum(ctx, id, 1, u)
	if err != nil || changed || rev != 1 {
		t.Fatalf("no-op save = %d, %v, %v; want 1, unchanged", rev, changed, err)
	}
	if _, ok := e.renderJob(id); ok || e.wakes.Load() != wakes {
		t.Error("a no-op save enqueued a render")
	}

	u = e.update(id)
	u.Title = "Album (Remastered)"
	rev, changed, err = e.svc.UpdateAlbum(ctx, id, 1, u)
	if err != nil || !changed || rev != 2 {
		t.Fatalf("change = %d, %v, %v; want 2, changed", rev, changed, err)
	}
	al := e.album(id)
	r, ok := e.renderJob(id)
	if al.Revision != 2 || al.Title != "Album (Remastered)" || al.FolderKey != "album (remastered)" || !ok || r.State != "pending" {
		t.Errorf("after the change: album %+v, render %+v (%v)", al, r, ok)
	}
	if got := e.claims(id); !reflect.DeepEqual(got, map[string]string{"artist/album (remastered)": "Artist/Album (Remastered)"}) {
		t.Errorf("claims %v: the old unpublished path must be released", got)
	}
	if e.wakes.Load() != wakes+1 {
		t.Error("no wake-up after the change")
	}

	// A change of one track only.
	u = e.update(id)
	u.Tracks[1].Genre = nil // inherit instead of explicitly none
	if rev, changed, err = e.svc.UpdateAlbum(ctx, id, 2, u); err != nil || !changed || rev != 3 {
		t.Fatalf("track change = %d, %v, %v", rev, changed, err)
	}
	if ts := e.tracks(id); ts[1].Genre != nil {
		t.Errorf("track genre %v, want NULL", ts[1].Genre)
	}
	// The stale revision after it.
	if _, _, err := e.svc.UpdateAlbum(ctx, id, 2, u); catalog.Code(err) != catalog.CodePreconditionFailed {
		t.Errorf("old revision: %v", err)
	}

	// An album genre saved as "" equals no genre.
	u = e.update(id)
	u.Genre = nil
	if _, _, err = e.svc.UpdateAlbum(ctx, id, 3, u); err != nil {
		t.Fatal(err)
	}
	u = e.update(id)
	u.Genre = ptr("")
	if rev, changed, err := e.svc.UpdateAlbum(ctx, id, 4, u); err != nil || changed || rev != 4 {
		t.Errorf("empty genre over NULL = %d, %v, %v; want a no-op", rev, changed, err)
	}

	if _, _, err := e.svc.UpdateAlbum(ctx, store.NewID(), 1, u); catalog.Code(err) != catalog.CodeAlbumNotFound {
		t.Errorf("unknown album: %v", err)
	}
}

// §10.2: the list must be exactly the current tracks; the values are
// validated like at import; the artist must exist.
func TestUpdateAlbumValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Artist", "Album")
	for _, tc := range []struct {
		name   string
		mutate func(u *catalog.AlbumUpdate)
		code   string
	}{
		{"missing track", func(u *catalog.AlbumUpdate) { u.Tracks = u.Tracks[:1] }, catalog.CodeTrackListMismatch},
		{"unknown track", func(u *catalog.AlbumUpdate) { u.Tracks[1].ID = store.NewID() }, catalog.CodeTrackListMismatch},
		{"track twice", func(u *catalog.AlbumUpdate) {
			u.Tracks[1].ID = u.Tracks[0].ID
			u.Tracks[1].No = 5
		}, catalog.CodeTrackListMismatch},
		{"extra track", func(u *catalog.AlbumUpdate) {
			u.Tracks = append(u.Tracks, catalog.TrackUpdate{ID: store.NewID(), Disc: 1, No: 9, Title: "x"})
		}, catalog.CodeTrackListMismatch},
		{"same number", func(u *catalog.AlbumUpdate) { u.Tracks[1].No = u.Tracks[0].No }, catalog.CodeDuplicateTrackNumber},
		{"disc", func(u *catalog.AlbumUpdate) { u.Tracks[0].Disc = 100 }, catalog.CodeInvalidDisc},
		{"number", func(u *catalog.AlbumUpdate) { u.Tracks[0].No = 0 }, catalog.CodeInvalidTrackNumber},
		{"empty title", func(u *catalog.AlbumUpdate) { u.Title = "" }, names.CodeTextEmpty},
		{"empty track artist", func(u *catalog.AlbumUpdate) { u.Tracks[0].Artist = ptr(" ") }, names.CodeTextEmpty},
		{"year", func(u *catalog.AlbumUpdate) { u.Year = ptr(-1) }, catalog.CodeInvalidYear},
		{"unknown artist", func(u *catalog.AlbumUpdate) { u.ArtistID = store.NewID() }, catalog.CodeArtistNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := e.update(id)
			tc.mutate(&u)
			before, beforeTracks := e.album(id), e.tracks(id)
			_, _, err := e.svc.UpdateAlbum(ctx, id, before.Revision, u)
			wantCode(t, err, tc.code)
			if !reflect.DeepEqual(e.album(id), before) || !reflect.DeepEqual(e.tracks(id), beforeTracks) {
				t.Error("a refused update changed the album")
			}
		})
	}
}

// §4.2, §12.2: numbers are swapped inside one transaction, thanks to the
// deferred unique constraint, with no temporary numbers.
func TestTrackSwap(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := candidate(e.runningImport(), "Artist", "Swap")
	h := newHash()
	c.Blobs = append(c.Blobs, catalog.Blob{Hash: h, Size: 1, Format: catalog.FormatFLAC})
	c.Tracks = append(c.Tracks, catalog.ImportTrack{SourcePath: "CD1/03.flac", Disc: 1, No: 3, Title: "Three", BlobHash: h})
	id := e.commit(c).AlbumID

	u := e.update(id)
	u.Tracks[0].No, u.Tracks[1].No = 2, 1
	if _, changed, err := e.svc.UpdateAlbum(ctx, id, 1, u); err != nil || !changed {
		t.Fatalf("swap: %v", err)
	}
	u = e.update(id) // ordered by number again
	byID := map[uuid.UUID]int{}
	for i := range u.Tracks {
		byID[u.Tracks[i].ID] = u.Tracks[i].No
	}
	// A three-way rotation, and one track moved to a second disc.
	for i := range u.Tracks {
		u.Tracks[i].No = u.Tracks[i].No%3 + 1
	}
	u.Tracks[2].Disc = 2
	if _, _, err := e.svc.UpdateAlbum(ctx, id, 2, u); err != nil {
		t.Fatalf("rotation: %v", err)
	}
	for _, tr := range e.tracks(id) {
		want := byID[tr.ID]%3 + 1
		if tr.No != int32(want) {
			t.Errorf("track %s number %d, want %d", tr.ID, tr.No, want)
		}
	}
}

// Moving an album to another artist (§1.2, §10.2 PUT artist_id) moves its
// path; the folder must be free under the new artist.
func TestUpdateAlbumArtist(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("First", "Album")
	other := e.importAlbum("Second", "Album")
	second := e.album(other).ArtistID

	u := e.update(id)
	u.ArtistID = second
	ce := wantCode(t, func() error { _, _, err := e.svc.UpdateAlbum(ctx, id, 1, u); return err }(), catalog.CodeAlbumFolderConflict)
	if ce.Details.AlbumID != other {
		t.Errorf("conflict names %s, want %s", ce.Details.AlbumID, other)
	}
	u.Title = "Album (Deluxe)"
	if _, _, err := e.svc.UpdateAlbum(ctx, id, 1, u); err != nil {
		t.Fatal(err)
	}
	if got := e.claims(id); !reflect.DeepEqual(got, map[string]string{"second/album (deluxe)": "Second/Album (Deluxe)"}) {
		t.Errorf("claims %v", got)
	}
}

// §4.3: trash and restore; the trashed album keeps only what is published;
// a restore checks the name like any change.
func TestTrashRestore(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Artist", "Album")
	e.clearRenders()

	rev, changed, err := e.svc.TrashAlbum(ctx, id, 1)
	if err != nil || !changed || rev != 2 {
		t.Fatalf("trash = %d, %v, %v", rev, changed, err)
	}
	if al := e.album(id); al.DeletedAt == nil {
		t.Error("deleted_at not set")
	}
	if got := e.claims(id); len(got) != 0 {
		t.Errorf("a trashed, unpublished album keeps claims %v", got)
	}
	if _, ok := e.renderJob(id); !ok {
		t.Error("no render enqueued for the trash")
	}
	if rev, changed, err := e.svc.TrashAlbum(ctx, id, 2); err != nil || changed || rev != 2 {
		t.Errorf("second trash = %d, %v, %v; want a no-op", rev, changed, err)
	}
	if _, _, err := e.svc.TrashAlbum(ctx, id, 1); catalog.Code(err) != catalog.CodePreconditionFailed {
		t.Errorf("trash with an old revision: %v", err)
	}

	// Its folder is free: another album takes it.
	taker := e.importAlbum("Artist", "Album")
	before := e.album(id)
	ce := wantCode(t, func() error { _, _, err := e.svc.RestoreAlbum(ctx, id, 2); return err }(), catalog.CodeAlbumFolderConflict)
	if ce.Details.AlbumID != taker {
		t.Errorf("conflict names %s, want %s", ce.Details.AlbumID, taker)
	}
	if !reflect.DeepEqual(e.album(id), before) {
		t.Error("a refused restore changed the album")
	}

	// A trashed album can be renamed before its restore (§4.3), even to a
	// name that is taken.
	u := e.update(id)
	u.Title = "Album"
	if _, changed, err := e.svc.UpdateAlbum(ctx, id, 2, u); err != nil || changed {
		t.Fatalf("no-op rename in the trash: %v %v", changed, err)
	}
	u.Title = "Album (Old Edition)"
	if rev, _, err := e.svc.UpdateAlbum(ctx, id, 2, u); err != nil || rev != 3 {
		t.Fatalf("rename in the trash = %d, %v", rev, err)
	}
	if got := e.claims(id); len(got) != 0 {
		t.Errorf("renaming a trashed album reserved %v", got)
	}
	rev, changed, err = e.svc.RestoreAlbum(ctx, id, 3)
	if err != nil || !changed || rev != 4 {
		t.Fatalf("restore = %d, %v, %v", rev, changed, err)
	}
	if got := e.claims(id); !reflect.DeepEqual(got, map[string]string{"artist/album (old edition)": "Artist/Album (Old Edition)"}) {
		t.Errorf("claims after the restore %v", got)
	}
	if rev, changed, err := e.svc.RestoreAlbum(ctx, id, 4); err != nil || changed || rev != 4 {
		t.Errorf("restore of an active album = %d, %v, %v; want a no-op", rev, changed, err)
	}
}

// A restore whose path is still reserved by another album fails with
// path_reserved naming it (§5.3).
func TestRestorePathReserved(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.importAlbum("X", "T")
	e.published(a, "X/T")
	if _, _, err := e.svc.TrashAlbum(ctx, a, 1); err != nil {
		t.Fatal(err)
	}
	// A was trashed but its removal is not published yet: X/T stays A's.
	if got := e.claims(a); !reflect.DeepEqual(got, map[string]string{"x/t": "X/T"}) {
		t.Fatalf("claims of the trashed album %v", got)
	}
	b := e.importAlbum("Y", "T")
	e.published(b, "Y/T")
	u := e.update(b)
	u.ArtistID = e.album(a).ArtistID
	ce := wantCode(t, func() error { _, _, err := e.svc.UpdateAlbum(ctx, b, 1, u); return err }(), catalog.CodePathReserved)
	if ce.Details.AlbumID != a || ce.Details.Path != "X/T" {
		t.Errorf("details %+v, want album %s and X/T", ce.Details, a)
	}
	// Once the removal is published, the path is free.
	e.removalPublished(a)
	if _, _, err := e.svc.UpdateAlbum(ctx, b, 1, u); err != nil {
		t.Fatalf("after the removal: %v", err)
	}
	// Now A's restore finds its folder taken by B.
	if _, _, err := e.svc.RestoreAlbum(ctx, a, 2); catalog.Code(err) != catalog.CodeAlbumFolderConflict {
		t.Errorf("restore: %v", err)
	}
}
