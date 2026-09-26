package catalog_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

// §10.2 GET /api/albums: search by title and artist with the one
// normalization of §5.2, artist and trash filters, keyset pagination
// (NOTES.md N-190, N-191).

func (e *env) list(f catalog.AlbumFilter) catalog.AlbumPage {
	e.t.Helper()
	if f.Limit == 0 {
		f.Limit = catalog.MaxPageSize
	}
	p, err := e.svc.ListAlbums(context.Background(), f)
	if err != nil {
		e.t.Fatalf("ListAlbums(%+v): %v", f, err)
	}
	return p
}

func titles(p catalog.AlbumPage) []string {
	out := make([]string, len(p.Albums))
	for i, a := range p.Albums {
		out[i] = a.ArtistName + "/" + a.Title
	}
	return out
}

func TestListAlbumsSearch(t *testing.T) {
	e := newEnv(t)
	e.importAlbum("Miles Davis", "Kind of Blue")
	e.importAlbum("Miles Davis", "Sketches of Spain")
	e.importAlbum("Björk", "Homogenic")
	e.importAlbum("Beyoncé", "Lemonade")
	e.importAlbum("AC/DC", "Back in Black")
	e.importAlbum("Straße", "Weg")
	e.importAlbum("Vienna Phil", "Concerto con brio")
	e.importAlbum("Ꮳherokee", "ꮳ lowercase")

	cases := []struct {
		q    string
		want []string
	}{
		{"", nil}, // everything, below
		{"   ", nil},
		{"kind", []string{"Miles Davis/Kind of Blue"}},
		{"KIND OF BLUE", []string{"Miles Davis/Kind of Blue"}},
		{"  of  ", nil}, // "of" after the trim: two albums, below
		{"miles", []string{"Miles Davis/Kind of Blue", "Miles Davis/Sketches of Spain"}},
		// NFD in the query, NFC in the catalog (§5.2).
		{"Björk", []string{"Björk/Homogenic"}},
		{"BJÖRK", []string{"Björk/Homogenic"}},
		// No accent folding: §5.2 has none (N-191).
		{"beyonce", []string{}},
		{"BEYONCÉ", []string{"Beyoncé/Lemonade"}},
		// Full case folding: ß is ss.
		{"STRASSE", []string{"Straße/Weg"}},
		// The text, not the sanitized folder: "/" is searchable.
		{"ac/dc", []string{"AC/DC/Back in Black"}},
		{"ac_dc", []string{}},
		// A reserved DOS name is a folder rule, not a search rule.
		{"con", []string{"Vienna Phil/Concerto con brio"}},
		// The Cherokee fix of names.Key applies to both sides.
		{"Ꮳ", []string{"Ꮳherokee/ꮳ lowercase"}},
		{"zzz", []string{}},
	}
	all := titles(e.list(catalog.AlbumFilter{}))
	if len(all) != 8 {
		t.Fatalf("all albums: %q", all)
	}
	for _, tc := range cases {
		got := titles(e.list(catalog.AlbumFilter{Query: tc.q}))
		want := tc.want
		switch {
		case strings.TrimSpace(tc.q) == "":
			want = all
		case strings.TrimSpace(tc.q) == "of":
			want = []string{"Miles Davis/Kind of Blue", "Miles Davis/Sketches of Spain"}
		}
		if !slices.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Errorf("search %q = %q, want %q", tc.q, got, want)
		}
	}
	// The order is the artists' then the titles' folder keys, byte-wise:
	// case-insensitive whatever the locale.
	p := e.list(catalog.AlbumFilter{})
	for i := 1; i < len(all); i++ {
		a, b := p.Albums[i-1], p.Albums[i]
		if cmp := strings.Compare(names.FolderKey(a.ArtistName), names.FolderKey(b.ArtistName)); cmp > 0 ||
			(cmp == 0 && strings.Compare(names.FolderKey(a.Title), names.FolderKey(b.Title)) > 0) {
			t.Fatalf("order: %q before %q", all[i-1], all[i])
		}
	}
	for _, q := range []string{"a\x01b", strings.Repeat("x", 1025)} {
		_, err := e.svc.ListAlbums(context.Background(), catalog.AlbumFilter{Query: q, Limit: 10})
		if code := catalog.Code(err); code != names.CodeTextControlChar && code != names.CodeTextTooLong {
			t.Errorf("search %q: %v, want a names text code", q[:3], err)
		}
	}
	for _, n := range []int{0, -1, catalog.MaxPageSize + 1} {
		if _, err := e.svc.ListAlbums(context.Background(), catalog.AlbumFilter{Limit: n}); catalog.Code(err) != catalog.CodeInvalidArgument {
			t.Errorf("limit %d: %v, want %s", n, err, catalog.CodeInvalidArgument)
		}
	}
}

func TestListAlbumsFilters(t *testing.T) {
	e := newEnv(t)
	kob := e.importAlbum("Miles Davis", "Kind of Blue")
	e.importAlbum("Miles Davis", "Sketches of Spain")
	e.importAlbum("Bill Evans", "Portrait in Jazz")
	miles := e.album(kob).ArtistID
	// Two trashed albums with the same artist and title: a tie broken by id.
	var trashed []uuid.UUID
	for range 2 {
		id := e.importAlbum("Miles Davis", "Kind of Blue (Mono)")
		if _, _, err := e.svc.TrashAlbum(context.Background(), id, e.album(id).Revision); err != nil {
			t.Fatal(err)
		}
		trashed = append(trashed, id)
	}
	if got := titles(e.list(catalog.AlbumFilter{ArtistID: miles})); !slices.Equal(got,
		[]string{"Miles Davis/Kind of Blue", "Miles Davis/Sketches of Spain"}) {
		t.Fatalf("artist filter: %q", got)
	}
	if got := e.list(catalog.AlbumFilter{ArtistID: store.NewID()}); len(got.Albums) != 0 || got.Next != nil {
		t.Fatalf("an unknown artist: %+v", got)
	}
	tr := e.list(catalog.AlbumFilter{Trashed: true})
	if len(tr.Albums) != 2 || tr.Albums[0].ID != trashed[0] || tr.Albums[1].ID != trashed[1] || !tr.Albums[0].Trashed {
		t.Fatalf("trash: %+v, want the two trashed albums by id", tr.Albums)
	}
	// The same tie, one album per page: the cursor's id decides.
	p1 := e.list(catalog.AlbumFilter{Trashed: true, Limit: 1})
	p2 := e.list(catalog.AlbumFilter{Trashed: true, Limit: 1, After: p1.Next})
	if p1.Albums[0].ID != trashed[0] || p1.Next == nil || p2.Albums[0].ID != trashed[1] || p2.Next != nil {
		t.Fatalf("pages of a tie: %+v, %+v", p1, p2)
	}
	if got := titles(e.list(catalog.AlbumFilter{Trashed: true, Query: "mono", ArtistID: miles})); len(got) != 2 {
		t.Fatalf("trash, artist and search together: %q", got)
	}
	// A summary carries the album's own fields.
	s := e.list(catalog.AlbumFilter{Query: "kind of blue"}).Albums[0]
	a := e.album(kob)
	if s.ID != kob || s.Revision != a.Revision || s.ArtistID != miles || s.Year == nil || *s.Year != 1959 ||
		s.Cover == nil || s.Cover.Hash != *a.CoverHash || s.Cover.Format != catalog.FormatJPEG || s.Trashed {
		t.Fatalf("summary %+v", s)
	}
}

// Pagination over more rows than one search batch: every page full but
// the last, no album twice or missing, the same order as one big page,
// with and without a search; the limits 50 and 200.
func TestListAlbumsPagination(t *testing.T) {
	e := newEnv(t)
	// 1,100 albums by SQL: ASCII names, whose folder keys are their lower
	// case. Every seventh title holds "needle".
	e.exec(`INSERT INTO artists (id, name, folder_key, revision)
		SELECT gen_random_uuid(), 'Artist ' || lpad(i::text, 2, '0'), 'artist ' || lpad(i::text, 2, '0'), 1
		FROM generate_series(1, 30) AS i`)
	e.exec(`INSERT INTO albums (id, artist_id, title, folder_key, revision)
		SELECT gen_random_uuid(), ar.id, t.title, lower(t.title), 1
		FROM generate_series(1, 1100) AS i
		JOIN artists ar ON ar.name = 'Artist ' || lpad((i % 30 + 1)::text, 2, '0')
		CROSS JOIN LATERAL (SELECT 'Title ' || lpad(i::text, 4, '0') ||
			CASE WHEN i % 7 = 0 THEN ' Needle' ELSE '' END AS title) AS t`)
	for _, q := range []string{"", "NEEDLE"} {
		for _, limit := range []int{1, 7, catalog.DefaultPageSize, catalog.MaxPageSize} {
			if limit == 1 && q == "" {
				continue // 1,100 pages
			}
			var got []uuid.UUID
			var after *catalog.AlbumCursor
			pages := 0
			for {
				p := e.list(catalog.AlbumFilter{Query: q, Limit: limit, After: after})
				pages++
				for _, a := range p.Albums {
					got = append(got, a.ID)
				}
				if p.Next == nil {
					break
				}
				if len(p.Albums) != limit {
					t.Fatalf("q %q limit %d: a page of %d before the end", q, limit, len(p.Albums))
				}
				after = p.Next
			}
			want := 1100
			if q != "" {
				want = 1100 / 7
			}
			if len(got) != want || pages != (want+limit-1)/limit {
				t.Fatalf("q %q limit %d: %d albums in %d pages, want %d", q, limit, len(got), pages, want)
			}
			// The same order as the reference: every match, by the keys.
			var ref []uuid.UUID
			rows, err := e.db.Query(context.Background(), `SELECT al.id FROM albums al JOIN artists ar ON ar.id = al.artist_id
				WHERE $1 = '' OR al.title LIKE '%Needle%'
				ORDER BY ar.folder_key COLLATE "C", al.folder_key COLLATE "C", al.id`, q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id uuid.UUID
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				ref = append(ref, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, ref) {
				t.Fatalf("q %q limit %d: the pages differ from the reference order", q, limit)
			}
		}
	}
	// A tie across a search batch (500 rows): 600 trashed albums with one
	// artist and one title, told apart by their ids only, after 450 that do
	// not match, so that the first batch ends inside the tie.
	e.exec(`INSERT INTO albums (id, artist_id, title, folder_key, revision, deleted_at)
		SELECT gen_random_uuid(), ar.id, 'Aaa ' || i, 'aaa ' || i, 1, now()
		FROM generate_series(1, 450) AS i JOIN artists ar ON ar.name = 'Artist 01'`)
	e.exec(`INSERT INTO albums (id, artist_id, title, folder_key, revision, deleted_at)
		SELECT gen_random_uuid(), ar.id, 'Tie Needle', 'tie needle', 1, now()
		FROM generate_series(1, 600) JOIN artists ar ON ar.name = 'Artist 01'`)
	var ties []uuid.UUID
	for after := (*catalog.AlbumCursor)(nil); ; {
		p := e.list(catalog.AlbumFilter{Query: "tie", Trashed: true, Limit: catalog.MaxPageSize, After: after})
		for _, a := range p.Albums {
			ties = append(ties, a.ID)
		}
		if p.Next == nil {
			break
		}
		after = p.Next
	}
	if len(ties) != 600 || !slices.IsSortedFunc(ties, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) }) {
		t.Fatalf("a tie over a search batch: %d albums, want 600 by id", len(ties))
	}
	// The cursor of an album that left the list since keeps its place: the
	// next page starts right after it.
	eleven := e.list(catalog.AlbumFilter{Limit: 11})
	p := e.list(catalog.AlbumFilter{Limit: 10})
	e.exec(`UPDATE albums SET deleted_at = now() WHERE id = $1`, p.Albums[9].ID)
	next := e.list(catalog.AlbumFilter{Limit: 10, After: p.Next})
	if next.Albums[0].ID != eleven.Albums[10].ID {
		t.Fatalf("after a cursor whose album left the list: %s, want %s", next.Albums[0].ID, eleven.Albums[10].ID)
	}
}
