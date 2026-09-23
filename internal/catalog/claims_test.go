package catalog_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

func TestAlbumPath(t *testing.T) {
	for _, tc := range []struct{ artist, title, path, key string }{
		{"Miles Davis", "Kind of Blue", "Miles Davis/Kind of Blue", "miles davis/kind of blue"},
		{"AC/DC", "Back in Black", "AC_DC/Back in Black", "ac_dc/back in black"},
		{"ABBA", "Arrival", "ABBA/Arrival", "abba/arrival"},
		{"CON", "What?", "_CON/What_", "_con/what_"},
	} {
		p := catalog.AlbumPath(tc.artist, tc.title)
		if p.Path != tc.path || p.Key != tc.key {
			t.Errorf("AlbumPath(%q, %q) = %+v, want %s %s", tc.artist, tc.title, p, tc.path, tc.key)
		}
		// The claim key is the two folder_key columns joined (§5.2).
		if p.Key != names.FolderKey(tc.artist)+"/"+names.FolderKey(tc.title) {
			t.Errorf("key %s is not the folder keys joined", p.Key)
		}
	}
}

// setJournal writes the publication row for album, as PREPARE will (§9.3 A).
func (e *env) setJournal(album uuid.UUID, oldPath, newPath *string) {
	e.t.Helper()
	e.exec(`DELETE FROM publication`)
	var oldBuild *uuid.UUID
	if oldPath != nil {
		oldBuild = ptr(store.NewID())
	}
	var receipt *string
	if newPath != nil {
		receipt = ptr(newHash())
	}
	e.exec(`INSERT INTO publication (id, album_id, ticket, revision, renderer, build_id, receipt_hash, old_path, old_build, new_path)
		SELECT 1, id, 1, revision, 'rv-test', $2, $3, $4, $5, $6 FROM albums WHERE id = $1`,
		album, store.NewID(), receipt, oldPath, oldBuild, newPath)
}

// §5.3: the claims of an album are exactly the union of its desired path
// (if active), its published path and the paths of its publication in
// progress, on normalized keys, with the path preference desired, journal,
// published. Every state, one after the other.
func TestClaimsUnion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.importAlbum("Abba", "X")
	artist := e.album(a).ArtistID
	rev := func() int64 { return e.album(a).Revision }
	check := func(step string, want map[string]string) {
		t.Helper()
		if err := e.reconcile(a); err != nil {
			t.Fatalf("%s: reconcile: %v", step, err)
		}
		if got := e.claims(a); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: claims %v, want %v", step, got, want)
		}
	}
	rename := func(name string) {
		t.Helper()
		if _, _, err := e.svc.RenameArtist(ctx, artist, e.artist(artist).Revision, name); err != nil {
			t.Fatalf("rename to %s: %v", name, err)
		}
	}

	check("active, never published", map[string]string{"abba/x": "Abba/X"})
	e.published(a, "Abba/X")
	check("published at the desired path", map[string]string{"abba/x": "Abba/X"})

	// A case-only rename: one row, the desired spelling (§5.3).
	rename("ABBA")
	check("case variant of the published path", map[string]string{"abba/x": "ABBA/X"})

	// A real rename: the old name stays reserved until retired.
	rename("Björk")
	check("renamed, old output still published", map[string]string{"björk/x": "Björk/X", "abba/x": "Abba/X"})

	// PREPARE of the rename: the journal's paths join the union; the
	// journal spelling wins over the published one.
	e.setJournal(a, ptr("ABBA/X"), ptr("BJÖRK/X"))
	check("publication in progress", map[string]string{"björk/x": "Björk/X", "abba/x": "ABBA/X"})

	// Trashed during the publication: the desired path leaves the union,
	// the journal's new path stays.
	if _, _, err := e.svc.TrashAlbum(ctx, a, rev()); err != nil {
		t.Fatal(err)
	}
	check("trashed, publication in progress", map[string]string{"björk/x": "BJÖRK/X", "abba/x": "ABBA/X"})

	// FINALIZE of the rename: published = the journal's new path.
	e.exec(`DELETE FROM publication`)
	e.published(a, "BJÖRK/X")
	check("trashed, published", map[string]string{"björk/x": "BJÖRK/X"})

	// The removal published: nothing left.
	e.removalPublished(a)
	check("trashed, removal published", map[string]string{})

	if _, _, err := e.svc.RestoreAlbum(ctx, a, rev()); err != nil {
		t.Fatal(err)
	}
	check("restored", map[string]string{"björk/x": "Björk/X"})
}

// The §5.3 example: A is renamed from X to Y; X stays reserved by A until
// the old directory is retired, and B asking for X gets path_reserved
// naming A; after the publication completes, B can take X.
func TestRenameReservation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.importAlbum("Artist", "X")
	e.published(a, "Artist/X")
	u := e.update(a)
	u.Title = "Y"
	if _, _, err := e.svc.UpdateAlbum(ctx, a, 1, u); err != nil {
		t.Fatal(err)
	}
	if got := e.claims(a); !reflect.DeepEqual(got, map[string]string{"artist/x": "Artist/X", "artist/y": "Artist/Y"}) {
		t.Fatalf("claims of A %v", got)
	}

	// B asks for X, through an import and through a rename, and gets
	// path_reserved naming A. The folder itself is free (A is now Y).
	out := e.commit(candidate(e.runningImport(), "Artist", "x"))
	if out.ErrorCode != catalog.CodePathReserved || !strings.Contains(out.ErrorMessage, a.String()) {
		t.Errorf("import of X: %+v, want %s naming %s", out, catalog.CodePathReserved, a)
	}
	b := e.importAlbum("Artist", "B")
	e.clearRenders()
	before, beforeClaims := e.album(b), e.claims(b)
	ub := e.update(b)
	ub.Title = "X"
	ce := wantCode(t, func() error { _, _, err := e.svc.UpdateAlbum(ctx, b, 1, ub); return err }(), catalog.CodePathReserved)
	if ce.Details.AlbumID != a || ce.Details.Path != "Artist/X" {
		t.Errorf("details %+v", ce.Details)
	}
	// All or nothing: no bump, no render, no claim change for B.
	if _, ok := e.renderJob(b); ok || !reflect.DeepEqual(e.album(b), before) || !reflect.DeepEqual(e.claims(b), beforeClaims) {
		t.Error("the refused change left traces")
	}
	// Nobody can steal the reservation, not even a reconcile of B.
	if err := e.reconcile(b); err != nil {
		t.Fatal(err)
	}
	if got := e.claims(a); len(got) != 2 {
		t.Errorf("A lost a claim: %v", got)
	}

	// FINALIZE of A's rename releases X.
	e.published(a, "Artist/Y")
	if got := e.claims(a); !reflect.DeepEqual(got, map[string]string{"artist/y": "Artist/Y"}) {
		t.Errorf("claims of A after FINALIZE %v", got)
	}
	if _, _, err := e.svc.UpdateAlbum(ctx, b, 1, ub); err != nil {
		t.Errorf("B after the retirement: %v", err)
	}
}

// ReconcileClaims releases only what is outside the union, never another
// album's claim, and fails without writing when a key has another owner.
func TestReconcileClaims(t *testing.T) {
	e := newEnv(t)
	a := e.importAlbum("A", "One")
	b := e.importAlbum("B", "Two")
	e.exec(`INSERT INTO path_claims (path_key, path, album_id) VALUES ('stray/key', 'Stray/Key', $1)`, a)
	e.exec(`UPDATE path_claims SET path = 'A/ONE' WHERE album_id = $1 AND path_key = 'a/one'`, a)
	if err := e.reconcile(a); err != nil {
		t.Fatal(err)
	}
	if got := e.claims(a); !reflect.DeepEqual(got, map[string]string{"a/one": "A/One"}) {
		t.Errorf("claims of A %v: the stray claim must go and the path be the desired one", got)
	}
	if got := e.claims(b); !reflect.DeepEqual(got, map[string]string{"b/two": "B/Two"}) {
		t.Errorf("claims of B %v", got)
	}

	// B's desired key held by A: path_reserved, and B's claims untouched.
	e.exec(`UPDATE path_claims SET album_id = $1 WHERE path_key = 'b/two'`, a)
	e.exec(`INSERT INTO path_claims (path_key, path, album_id) VALUES ('other/b', 'Other/B', $1)`, b)
	err := e.reconcile(b)
	ce := wantCode(t, err, catalog.CodePathReserved)
	if ce.Details.AlbumID != a {
		t.Errorf("details %+v", ce.Details)
	}
	if got := e.claims(b); !reflect.DeepEqual(got, map[string]string{"other/b": "Other/B"}) {
		t.Errorf("claims of B after the refusal %v", got)
	}
	if err := e.reconcile(store.NewID()); catalog.Code(err) != catalog.CodeAlbumNotFound {
		t.Errorf("unknown album: %v", err)
	}
}
