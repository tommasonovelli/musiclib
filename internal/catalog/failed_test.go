package catalog_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
)

// The Library's "Da sistemare" filter and count (NOTES.md N-245): the
// albums whose render job failed, active or trashed; the trash flag is
// ignored, the other filters and the cursor still apply.
func TestListAlbumsFailedAndCount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ok := e.importAlbum("Miles Davis", "Kind of Blue")
	broken := e.importAlbum("Miles Davis", "Sketches of Spain")
	other := e.importAlbum("Bill Evans", "Portrait in Jazz")
	gone := e.importAlbum("Chet Baker", "Chet")
	if _, _, err := e.svc.TrashAlbum(ctx, gone, e.album(gone).Revision); err != nil {
		t.Fatal(err)
	}
	fail := func(id uuid.UUID) {
		e.exec(`UPDATE jobs SET state = 'failed', error_code = 'render_io', error_message = 'disk' WHERE kind = 'render' AND album_id = $1`, id)
	}
	count := func() int64 {
		t.Helper()
		n, err := e.svc.CountFailedAlbums(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := titles(e.list(catalog.AlbumFilter{Failed: true})); len(got) != 0 || count() != 0 {
		t.Fatalf("nothing failed: %q, %d", got, count())
	}
	for _, id := range []uuid.UUID{broken, other, gone} {
		fail(id)
	}
	// A failed import job is not an album to fix.
	e.exec(`UPDATE jobs SET state = 'failed', error_code = 'mixed_album', error_message = 'x' WHERE kind = 'import'`)
	want := []string{"Bill Evans/Portrait in Jazz", "Chet Baker/Chet", "Miles Davis/Sketches of Spain"}
	for _, trashed := range []bool{false, true} {
		if got := titles(e.list(catalog.AlbumFilter{Failed: true, Trashed: trashed})); !slices.Equal(got, want) {
			t.Fatalf("failed (trashed=%v): %q", trashed, got)
		}
	}
	if n := count(); n != 3 {
		t.Fatalf("count %d", n)
	}
	if got := titles(e.list(catalog.AlbumFilter{Failed: true, ArtistID: e.album(ok).ArtistID})); !slices.Equal(got, want[2:]) {
		t.Fatalf("failed by artist: %q", got)
	}
	if got := titles(e.list(catalog.AlbumFilter{Failed: true, Query: "PORTRAIT"})); !slices.Equal(got, want[:1]) {
		t.Fatalf("failed search: %q", got)
	}
	p1 := e.list(catalog.AlbumFilter{Failed: true, Limit: 2})
	p2 := e.list(catalog.AlbumFilter{Failed: true, Limit: 2, After: p1.Next})
	if !slices.Equal(append(titles(p1), titles(p2)...), want) || p2.Next != nil {
		t.Fatalf("failed pages: %q %q", titles(p1), titles(p2))
	}
	// The ordinary views are unchanged by a failure.
	if got := titles(e.list(catalog.AlbumFilter{})); len(got) != 3 {
		t.Fatalf("library: %q", got)
	}
}

func TestMatchArtists(t *testing.T) {
	artists := []catalog.Artist{{Name: "Miles Davis"}, {Name: "Björk"}, {Name: "Milestone"}, {Name: "Straße"}, {Name: "Bill Evans"}}
	for _, c := range []struct {
		q     string
		limit int
		want  []string
	}{
		{"", 5, nil},
		{"   ", 5, nil},
		{"MILES", 5, []string{"Miles Davis", "Milestone"}},
		{"miles", 1, []string{"Miles Davis"}},
		{"BJÖRK", 5, []string{"Björk"}},
		{"strasse", 5, []string{"Straße"}},
		{"bjork", 5, nil},
		{"zzz", 5, nil},
	} {
		got, err := catalog.MatchArtists(artists, c.q, c.limit)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, a := range got {
			names = append(names, a.Name)
		}
		if !slices.Equal(names, c.want) {
			t.Errorf("MatchArtists(%q, %d) = %q, want %q", c.q, c.limit, names, c.want)
		}
	}
	if _, err := catalog.MatchArtists(artists, "bad\x00text", 5); err == nil {
		t.Error("a NUL in the search is accepted")
	}
}
