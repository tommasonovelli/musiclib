package importer

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// §7.3: digit runs compare as integers, the rest by bytes, ties by the full
// path. The expected order is written out, so any change of the rule fails.
func TestNaturalOrder(t *testing.T) {
	// Sorted by value, "1.flac" and "01.flac" tie; the path breaks the tie:
	// "a/01.flac" < "a/1.flac" < "b/1.flac".
	want := []string{
		"a/01.flac", "a/1.flac", "b/1.flac",
		"a/2.flac", "a/9.flac", "a/10.flac", "a/99999999999999999999999.flac", "a/100000000000000000000000.flac",
		"a/A.flac", "a/Track10.flac", "a/Track 2.flac", "a/Track 10.flac", "a/a.flac", "a/track 1.flac", "a/é.flac",
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 50 {
		got := slices.Clone(want)
		r.Shuffle(len(got), func(i, j int) { got[i], got[j] = got[j], got[i] })
		slices.SortFunc(got, pathNaturalCompare)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got  %q\nwant %q", got, want)
		}
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1", "01", 0}, {"x2", "x10", -1}, {"x10", "x2", 1}, {"a", "a1", -1}, {"1a", "a", -1},
		{"", "", 0}, {"", "a", -1}, {"007", "7", 0}, {"0", "", 1},
	} {
		if got := naturalCompare(tc.a, tc.b); got != tc.want {
			t.Errorf("naturalCompare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// dirOf builds a synthetic tree: paths ending in "/" are directories, "!"
// marks audio, "@" a rejected entry.
func dirOf(paths ...string) *srcDir {
	root := &srcDir{}
	dirs := map[string]*srcDir{"": root}
	var mk func(rel string) *srcDir
	mk = func(rel string) *srcDir {
		if d, ok := dirs[rel]; ok {
			return d
		}
		parent := mk(rel[:max(0, strings.LastIndexByte(rel, '/'))])
		d := &srcDir{Rel: rel}
		parent.Dirs = append(parent.Dirs, d)
		dirs[rel] = d
		return d
	}
	for _, p := range paths {
		switch {
		case strings.HasSuffix(p, "/"):
			mk(strings.TrimSuffix(p, "/"))
		case strings.HasSuffix(p, "@"):
			p = strings.TrimSuffix(p, "@")
			d := mk(p[:max(0, strings.LastIndexByte(p, '/'))])
			d.Rejected = append(d.Rejected, rejectedEntry{Rel: p, Valid: true, Why: fsops.TypeSymlink.String()})
		default:
			audio := strings.HasSuffix(p, "!")
			p = strings.TrimSuffix(p, "!")
			d := mk(p[:max(0, strings.LastIndexByte(p, '/'))])
			d.Files = append(d.Files, &srcFile{Rel: p, Audio: audio})
		}
	}
	return root
}

func outcome(g grouping) (branches map[string]string, unassigned, rejected []string) {
	branches = map[string]string{}
	for _, b := range g.Branches {
		code := "ok"
		if b.Err != nil {
			code = b.Err.Code
		}
		branches[b.Dir.Rel] = code
	}
	for _, f := range g.Unassigned {
		unassigned = append(unassigned, f.Rel)
	}
	for _, r := range g.Rejected {
		rejected = append(rejected, r.Rel)
	}
	return branches, unassigned, rejected
}

// §7.2's rules on synthetic trees.
func TestGroup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tree       []string
		branches   map[string]string
		unassigned []string
		rejected   []string
	}{
		{"rule 1 at the root", []string{"1.flac!", "cover.jpg", "Scans/a.jpg"}, map[string]string{"": "ok"}, nil, nil},
		{"rule 1 below, rule 5", []string{"readme.txt", "A/1.flac!", "B/C/1.flac!", "B/x.txt", "E/"},
			map[string]string{"A": "ok", "B/C": "ok"}, []string{"B/x.txt", "readme.txt"}, nil},
		{"rule 4: audio below audio", []string{"A/1.flac!", "A/Bonus/1.flac!", "B/1.flac!"},
			map[string]string{"A": CodeAmbiguousCandidate, "B": "ok"}, nil, nil},
		{"rule 4 deep", []string{"A/1.flac!", "A/x/y/z/1.flac!"}, map[string]string{"A": CodeAmbiguousCandidate}, nil, nil},
		{"rule 2", []string{"Box/CD1/1.flac!", "Box/CD2/1.flac!", "Box/Art/f.jpg", "Box/notes.txt"},
			map[string]string{"Box": CodeMultiDiscNotSupported}, nil, nil},
		{"rule 2, one disc", []string{"Box/Disc 01/1.flac!"}, map[string]string{"Box": CodeMultiDiscNotSupported}, nil, nil},
		{"rule 3: duplicate numbers", []string{"Box/CD1/1.flac!", "Box/cd01/1.flac!"}, map[string]string{"Box": CodeMultiDiscNotSupported}, nil, nil},
		{"not rule 2: another audio child", []string{"Box/CD1/1.flac!", "Box/Bonus/1.flac!"},
			map[string]string{"Box/CD1": "ok", "Box/Bonus": "ok"}, nil, nil},
		{"not rule 2: audio below a disc", []string{"Box/CD1/1.flac!", "Box/CD2/x/1.flac!"},
			map[string]string{"Box/CD1": "ok", "Box/CD2/x": "ok"}, nil, nil},
		{"not rule 2: CD0", []string{"Box/CD0/1.flac!"}, map[string]string{"Box/CD0": "ok"}, nil, nil},
		{"rejected inside a candidate", []string{"A/1.flac!", "A/Scans/link@"}, map[string]string{"A": CodeSourceRejected}, nil, nil},
		{"rejected outside", []string{"link@", "A/1.flac!"}, map[string]string{"A": "ok"}, nil, []string{"link"}},
		{"nothing", []string{"a.txt", "D/b.txt"}, map[string]string{}, []string{"D/b.txt", "a.txt"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, u, r := outcome(group(dirOf(tc.tree...), "base"))
			if !reflect.DeepEqual(b, tc.branches) || !reflect.DeepEqual(u, tc.unassigned) || !reflect.DeepEqual(r, tc.rejected) {
				t.Errorf("branches %v unassigned %v rejected %v\nwant     %v unassigned %v rejected %v", b, u, r, tc.branches, tc.unassigned, tc.rejected)
			}
		})
	}
}

func TestIsDiscName(t *testing.T) {
	for name, want := range map[string]bool{
		"CD1": true, "cd01": true, "Cd12": true, "Disc 1": true, "DISC 007": true, "disc 3": true,
		"CD 1": false, "Disc1": false, "CD0": false, "Disc 00": false, "CD": false, "Disc ": false,
		"CD1a": false, "CD-1": false, "diſc 1": false, "Disc  1": false, "CD１": false,
	} {
		if got := isDiscName(name); got != want {
			t.Errorf("isDiscName(%q) = %v, want %v", name, got, want)
		}
	}
}

// §7.2 limits with the production constants, on synthetic trees.
func TestGroupLimits(t *testing.T) {
	files := []string{"A/1.flac!"}
	for i := range MaxFiles - 1 {
		files = append(files, fmt.Sprintf("A/s/%d.txt", i))
	}
	if b, _, _ := outcome(group(dirOf(files...), "")); b["A"] != "ok" {
		t.Errorf("exactly %d files: %v", MaxFiles, b)
	}
	if b, _, _ := outcome(group(dirOf(append(files, "A/one-more.txt")...), "")); b["A"] != catalog.CodeTooManyFiles {
		t.Errorf("%d files: %v", MaxFiles+1, b)
	}
	var tracks []string
	for i := range MaxTracks {
		tracks = append(tracks, fmt.Sprintf("A/%d.flac!", i))
	}
	if b, _, _ := outcome(group(dirOf(tracks...), "")); b["A"] != "ok" {
		t.Errorf("exactly %d tracks: %v", MaxTracks, b)
	}
	if b, _, _ := outcome(group(dirOf(append(tracks, "A/x.flac!")...), "")); b["A"] != catalog.CodeTooManyFiles {
		t.Errorf("%d tracks: %v", MaxTracks+1, b)
	}
	if MaxFiles != 10000 || MaxTracks != 1000 || MaxCoverBytes != 20<<20 || MaxCoverPixels != 40_000_000 ||
		media.MaxCoverBytes != catalog.MaxCoverBytes {
		t.Error("the limits of §7.2 and §8.5 changed")
	}
}

func TestIgnored(t *testing.T) {
	for name, want := range map[string]bool{
		".DS_Store": true, ".ds_store": true, "Thumbs.db": true, "THUMBS.DB": true, "desktop.ini": true, "Desktop.INI": true,
		".DS_Store.bak": false, "thumbs.db ": false, ".dſ_store": false, ".hidden": false, "desktop.ini/": false,
	} {
		if got := ignored(name); got != want {
			t.Errorf("ignored(%q) = %v, want %v", name, got, want)
		}
	}
}

// tt builds the tags of a track for inferMetadata.
func tt(path string, kv ...string) trackTags {
	var m media.ManagedTags
	fields := map[string]*[]string{
		"TITLE": &m.Title, "ARTIST": &m.Artist, "ALBUMARTIST": &m.AlbumArtist, "ALBUM": &m.Album,
		"TRACKNUMBER": &m.Track, "DISCNUMBER": &m.Disc, "DATE": &m.Date, "GENRE": &m.Genre, "COMPILATION": &m.Compilation,
	}
	for _, s := range kv {
		k, v, _ := strings.Cut(s, "=")
		*fields[k] = append(*fields[k], v)
	}
	return trackTags{Path: path, Tags: m}
}

// The table of §7.3, case by case.
func TestInferMetadata(t *testing.T) {
	none := jobs.Overrides{}
	type want struct {
		artist, title string
		year          int
		genre         string
		compilation   bool
		nos           []int
		titles        []string
		artists       []string // "" = inherit
		genres        []string // "-" = inherit
		warnings      []jobs.WarningCode
		code          string
	}
	for _, tc := range []struct {
		name   string
		dir    string
		tracks []trackTags
		ov     jobs.Overrides
		want   want
	}{
		{"untagged", "Dir", []trackTags{tt("b 10.flac"), tt("b 9.flac")}, none, want{
			artist: UnknownArtist, title: "Dir", nos: []int{2, 1}, titles: []string{"b 10", "b 9"},
			artists: []string{"", ""}, genres: []string{"-", "-"}, warnings: []jobs.WarningCode{jobs.WarnTracksRenumbered}}},
		{"one album artist", "D", []trackTags{tt("1", "ALBUMARTIST=AA", "ARTIST=AA", "TRACKNUMBER=1"), tt("2", "ARTIST=B", "TRACKNUMBER=2")}, none, want{
			artist: "AA", title: "D", nos: []int{1, 2}, titles: []string{"1", "2"}, artists: []string{"", "B"}, genres: []string{"-", "-"}}},
		{"discordant album artists", "D", []trackTags{tt("1", "ALBUMARTIST=A"), tt("2", "ALBUMARTIST=B")}, none, want{code: CodeAmbiguousAlbumArtist}},
		{"artist override", "D", []trackTags{tt("1", "ALBUMARTIST=A", "ARTIST=A", "TRACKNUMBER=1"), tt("2", "ALBUMARTIST=B", "TRACKNUMBER=2")},
			jobs.Overrides{Artist: ptr("A")}, want{artist: "A", title: "D", nos: []int{1, 2}, titles: []string{"1", "2"},
				artists: []string{"", ""}, genres: []string{"-", "-"}}},
		{"various artists", "D", []trackTags{tt("1", "ARTIST=A", "TRACKNUMBER=1"), tt("2", "ARTIST=B", "TRACKNUMBER=2")}, none, want{
			artist: VariousArtists, title: "D", compilation: true, nos: []int{1, 2}, titles: []string{"1", "2"},
			artists: []string{"A", "B"}, genres: []string{"-", "-"}}},
		{"compilation tag", "D", []trackTags{tt("1", "ARTIST=A", "COMPILATION=true", "TRACKNUMBER=1")}, none, want{
			artist: "A", title: "D", compilation: true, nos: []int{1}, titles: []string{"1"}, artists: []string{""}, genres: []string{"-"}}},
		{"track artist equal after normalization", "D", []trackTags{tt("1", "ALBUMARTIST=Zé", "ARTIST= Zé ", "TRACKNUMBER=1")}, none, want{
			artist: "Zé", title: "D", nos: []int{1}, titles: []string{"1"}, artists: []string{""}, genres: []string{"-"}}},
		// N-121 (owner, 2026-09-23): artists equal under §7.6 (NFC, trim,
		// casefold) are one artist. A track whose spelling differs only in
		// case inherits the album artist: its own spelling is not kept.
		{"track artist differing in case inherits", "D", []trackTags{tt("1", "ALBUMARTIST=abba", "ARTIST=ABBA", "TRACKNUMBER=1")}, none, want{
			artist: "abba", title: "D", nos: []int{1}, titles: []string{"1"}, artists: []string{""}, genres: []string{"-"}}},
		{"track artists differing in case: the most frequent spelling", "D", []trackTags{tt("1", "ARTIST=Abba", "TRACKNUMBER=1"),
			tt("2", "ARTIST=ABBA", "TRACKNUMBER=2"), tt("3", "ARTIST=Abba", "TRACKNUMBER=3"), tt("4", "TRACKNUMBER=4")}, none, want{
			artist: "Abba", title: "D", nos: []int{1, 2, 3, 4}, titles: []string{"1", "2", "3", "4"}, artists: make([]string, 4), genres: slices.Repeat([]string{"-"}, 4)}},
		{"track artists differing in case: a tie goes to the smallest bytes", "D", []trackTags{tt("1", "ARTIST=abba", "TRACKNUMBER=1"),
			tt("2", "ARTIST=ABBA", "TRACKNUMBER=2"), tt("3", "ARTIST=Abba", "TRACKNUMBER=3")}, none, want{
			artist: "ABBA", title: "D", nos: []int{1, 2, 3}, titles: []string{"1", "2", "3"}, artists: make([]string, 3), genres: slices.Repeat([]string{"-"}, 3)}},
		// Full case folding, not lower(): "ß" folds to "ss".
		{"track artists equal by case folding", "D", []trackTags{tt("1", "ARTIST=Strauß", "TRACKNUMBER=1"), tt("2", "ARTIST=STRAUSS", "TRACKNUMBER=2")}, none, want{
			artist: "STRAUSS", title: "D", nos: []int{1, 2}, titles: []string{"1", "2"}, artists: []string{"", ""}, genres: []string{"-", "-"}}},
		{"case variants and another artist", "D", []trackTags{tt("1", "ARTIST=Abba", "TRACKNUMBER=1"), tt("2", "ARTIST=ABBA", "TRACKNUMBER=2"),
			tt("3", "ARTIST=Queen", "TRACKNUMBER=3")}, none, want{
			artist: VariousArtists, title: "D", compilation: true, nos: []int{1, 2, 3}, titles: []string{"1", "2", "3"},
			artists: []string{"Abba", "ABBA", "Queen"}, genres: slices.Repeat([]string{"-"}, 3)}},
		{"album artists differing in case stay ambiguous", "D", []trackTags{tt("1", "ALBUMARTIST=Abba"), tt("2", "ALBUMARTIST=ABBA")}, none, want{code: CodeAmbiguousAlbumArtist}},
		{"track artist differing in case from the artist override inherits", "D", []trackTags{tt("1", "ARTIST=queen", "TRACKNUMBER=1"), tt("2", "ARTIST=Abba", "TRACKNUMBER=2")},
			jobs.Overrides{Artist: ptr("Queen")}, want{artist: "Queen", title: "D", nos: []int{1, 2}, titles: []string{"1", "2"},
				artists: []string{"", "Abba"}, genres: []string{"-", "-"}}},
		{"mixed album", "D", []trackTags{tt("1", "ALBUM=X"), tt("2", "ALBUM=Y"), tt("3")}, none, want{code: CodeMixedAlbum}},
		{"title override resolves mixed", "D", []trackTags{tt("1", "ALBUM=X", "TRACKNUMBER=1"), tt("2", "ALBUM=Y", "TRACKNUMBER=2")},
			jobs.Overrides{Title: ptr("T")}, want{artist: UnknownArtist, title: "T", nos: []int{1, 2}, titles: []string{"1", "2"},
				artists: []string{"", ""}, genres: []string{"-", "-"}}},
		{"one album among empty ones", "D", []trackTags{tt("1", "ALBUM=X", "TRACKNUMBER=1"), tt("2", "TRACKNUMBER=2")}, none, want{
			artist: UnknownArtist, title: "X", nos: []int{1, 2}, titles: []string{"1", "2"}, artists: []string{"", ""}, genres: []string{"-", "-"}}},
		{"no title at /import", "", []trackTags{tt("1")}, none, want{code: CodeAlbumTitleMissing}},
		{"multi-valued", "D", []trackTags{tt("1", "TITLE=a", "TITLE=b", "ARTIST=Y", "ARTIST=X", "GENRE=g2", "GENRE=g1", "TRACKNUMBER=1")}, none, want{
			artist: "Y; X", title: "D", genre: "g2; g1", nos: []int{1}, titles: []string{"a; b"}, artists: []string{""}, genres: []string{"-"}}},
		{"year: most frequent, tie smallest, invalid ignored", "D", []trackTags{
			tt("1", "DATE=2003-01-01", "TRACKNUMBER=1"), tt("2", "DATE=2001", "TRACKNUMBER=2"), tt("3", "DATE=2003", "TRACKNUMBER=3"),
			tt("4", "DATE=2001/05", "TRACKNUMBER=4"), tt("5", "DATE=0000", "TRACKNUMBER=5"), tt("6", "DATE=19999", "TRACKNUMBER=6"),
			tt("7", "DATE=abcd", "TRACKNUMBER=7"), tt("8", "DATE=1990", "DATE=1991", "TRACKNUMBER=8")}, none, want{
			artist: UnknownArtist, title: "D", year: 2001, nos: []int{1, 2, 3, 4, 5, 6, 7, 8},
			titles:  []string{"1", "2", "3", "4", "5", "6", "7", "8"},
			artists: make([]string, 8), genres: slices.Repeat([]string{"-"}, 8), warnings: []jobs.WarningCode{jobs.WarnYearDiscordant}}},
		{"genre: most frequent, overrides kept", "D", []trackTags{
			tt("1", "GENRE=Rock", "TRACKNUMBER=1"), tt("2", "GENRE=Pop", "TRACKNUMBER=2"), tt("3", "GENRE=Rock", "TRACKNUMBER=3"), tt("4", "TRACKNUMBER=4")}, none, want{
			artist: UnknownArtist, title: "D", genre: "Rock", nos: []int{1, 2, 3, 4}, titles: []string{"1", "2", "3", "4"},
			artists: make([]string, 4), genres: []string{"-", "Pop", "-", ""}}},
		// N-004: the tie goes to the smallest in the bytes of the NFC text:
		// "Rock" (0x52) before "pop" (0x70), and "Zouk" before "Émo".
		{"genre tie by bytes (N-004)", "D", []trackTags{tt("1", "GENRE=pop", "TRACKNUMBER=1"), tt("2", "GENRE=Rock", "TRACKNUMBER=2")}, none, want{
			artist: UnknownArtist, title: "D", genre: "Rock", nos: []int{1, 2}, titles: []string{"1", "2"},
			artists: make([]string, 2), genres: []string{"pop", "-"}}},
		{"genre tie by bytes, accents (N-004)", "D", []trackTags{tt("1", "GENRE=Émo", "TRACKNUMBER=1"), tt("2", "GENRE=Zouk", "TRACKNUMBER=2")}, none, want{
			artist: UnknownArtist, title: "D", genre: "Zouk", nos: []int{1, 2}, titles: []string{"1", "2"},
			artists: make([]string, 2), genres: []string{"Émo", "-"}}},
		{"discs from tags, numbering per disc", "D", []trackTags{
			tt("a", "DISCNUMBER=2", "TRACKNUMBER=1"), tt("b", "DISCNUMBER=1", "TRACKNUMBER=1"), tt("c", "DISCNUMBER=2", "TRACKNUMBER=1"), tt("d", "DISCNUMBER=0")}, none, want{
			artist: UnknownArtist, title: "D", nos: []int{1, 1, 2, 2}, titles: []string{"a", "b", "c", "d"},
			artists: make([]string, 4), genres: slices.Repeat([]string{"-"}, 4), warnings: []jobs.WarningCode{jobs.WarnTracksRenumbered, jobs.WarnTracksRenumbered}}},
		{"track 0 renumbers", "D", []trackTags{tt("x", "TRACKNUMBER=0"), tt("y", "TRACKNUMBER=5")}, none, want{
			artist: UnknownArtist, title: "D", nos: []int{1, 2}, titles: []string{"x", "y"}, artists: make([]string, 2),
			genres: []string{"-", "-"}, warnings: []jobs.WarningCode{jobs.WarnTracksRenumbered}}},
		{"track out of range", "D", []trackTags{tt("x", "TRACKNUMBER=1000")}, none, want{code: catalog.CodeInvalidTrackNumber}},
		{"disc out of range", "D", []trackTags{tt("x", "DISCNUMBER=100")}, none, want{code: catalog.CodeInvalidDisc}},
		{"control character", "D", []trackTags{tt("x", "TITLE=a\x01b")}, none, want{code: CodeInvalidTag}},
		{"title from a dot file", "D", []trackTags{tt(".flac", "TRACKNUMBER=1")}, none, want{
			artist: UnknownArtist, title: "D", nos: []int{1}, titles: []string{".flac"}, artists: []string{""}, genres: []string{"-"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := inferMetadata(tc.dir, tc.tracks, tc.ov)
			if tc.want.code != "" {
				if Code(err) != tc.want.code {
					t.Fatalf("err %v, want %s", err, tc.want.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			w := tc.want
			year := 0
			if m.Year != nil {
				year = *m.Year
			}
			genre := ""
			if m.Genre != nil {
				genre = *m.Genre
			}
			var nos []int
			var titles, artists, genres []string
			for _, tr := range m.Tracks {
				nos, titles = append(nos, tr.No), append(titles, tr.Title)
				a, g := "", "-"
				if tr.Artist != nil {
					a = *tr.Artist
				}
				if tr.Genre != nil {
					g = *tr.Genre
				}
				artists, genres = append(artists, a), append(genres, g)
			}
			var codes []jobs.WarningCode
			for _, x := range m.Warnings {
				codes = append(codes, x.Code)
			}
			got := want{artist: m.Artist, title: m.Title, year: year, genre: genre, compilation: m.Compilation,
				nos: nos, titles: titles, artists: artists, genres: genres, warnings: codes}
			if !reflect.DeepEqual(got, w) {
				t.Errorf("got  %+v\nwant %+v", got, w)
			}
		})
	}
}

// The fingerprint of §7.6, pinned: the JSON bytes and the hash. A change of
// the serializer (HTML escaping, spaces, a newline, the order, the fields)
// changes every album's identity, and fails here.
func TestFingerprintGolden(t *testing.T) {
	entries := []fingerprintEntry{
		{Path: "é/<b>&c.txt", Size: 0, Hash: strings.Repeat("c", 64)},
		{Path: "a/01.flac", Size: 1234567890123, Hash: strings.Repeat("a", 64)},
		{Path: "B.flac", Size: 10, Hash: strings.Repeat("b", 64)},
	}
	const wantJSON = `[["B.flac",10,"` + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + `"],` +
		`["a/01.flac",1234567890123,"` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `"],` +
		`["é/<b>&c.txt",0,"` + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" + `"]]`
	const wantHash = "53c7a3c4a8bedbdb08cae3ec7dd31847f60f85883dd722ced6b0156dd4573196"
	b, err := fingerprintJSON(entries)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != wantJSON {
		t.Fatalf("JSON\n%s\nwant\n%s", b, wantJSON)
	}
	fp, err := fingerprint(entries)
	if err != nil {
		t.Fatal(err)
	}
	if fp != sha([]byte(wantJSON)) || fp != wantHash {
		t.Errorf("fingerprint %s, want %s", fp, wantHash)
	}
	slices.Reverse(entries)
	if again, _ := fingerprint(entries); again != fp {
		t.Error("the fingerprint depends on the input order")
	}
}

func TestAssociateLyrics(t *testing.T) {
	tracks := []string{"01 Intro.flac", "Song.flac", "sub/Song.flac", "été.flac"}
	got, err := associateLyrics(tracks, []string{"01 INTRO.lrc", "Other.lrc", "sub/song.LRC", "été.lrc", "Song.txt"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"01 Intro.flac": "01 INTRO.lrc", "sub/Song.flac": "sub/song.LRC", "été.flac": "été.lrc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, tc := range []struct{ tracks, files []string }{
		{[]string{"a.flac", "A.flac"}, []string{"a.lrc"}},
		{[]string{"a.flac"}, []string{"a.lrc", "A.LRC"}},
	} {
		if _, err := associateLyrics(tc.tracks, tc.files); Code(err) != catalog.CodeLyricsAssociation {
			t.Errorf("%v %v: %v", tc.tracks, tc.files, err)
		}
	}
}

func TestValidUTF8(t *testing.T) {
	big := bytes.Repeat([]byte("é"), 100_000)
	for _, tc := range []struct {
		in   []byte
		want bool
	}{
		{nil, true}, {[]byte("[00:00]ok\n"), true}, {[]byte("\xef\xbb\xbfbom"), true}, {[]byte("� literal"), true},
		{big, true}, {append(bytes.Clone(big), 0xff), false}, {[]byte("caf\xe9"), false}, {[]byte("\xed\xa0\x80"), false},
		{big[:len(big)-1], false},
	} {
		got, err := validUTF8(t.Context(), bytes.NewReader(tc.in))
		if err != nil || got != tc.want {
			t.Errorf("validUTF8(%d bytes) = %v, %v; want %v", len(tc.in), got, err, tc.want)
		}
	}
}

// §8.5 in Go: JPEG or PNG by content, the size and pixel limits, a full
// decode.
func TestValidateCover(t *testing.T) {
	jpg, pngb := jpegImage(t, 16, 16, 1), pngImage(t, 16, 16, 1)
	for _, tc := range []struct {
		name   string
		img    []byte
		size   int64
		format string
	}{
		{"jpeg", jpg, 0, "jpeg"},
		{"png", pngb, 0, "png"},
		{"truncated jpeg", jpg[:len(jpg)/2], 0, ""},
		{"truncated png", pngb[:len(pngb)-20], 0, ""},
		{"header only", pngHeader(10, 10), 0, ""},
		{"too many pixels", pngHeader(8000, 5001), 0, ""},
		{"exactly 40 Mpixel header", pngHeader(8000, 5000), 0, ""}, // passes the limit, fails the decode
		{"too large", jpg, MaxCoverBytes + 1, ""},
		{"gif", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), 0, ""},
		{"text", []byte("hello"), 0, ""},
	} {
		size := tc.size
		if size == 0 {
			size = int64(len(tc.img))
		}
		got, err := validateCover(bytes.NewReader(tc.img), size)
		if got != tc.format || (err == nil) != (tc.format != "") {
			t.Errorf("%s: %q, %v; want %q", tc.name, got, err, tc.format)
		}
	}
	// The pixel check comes before the decode: no 40-Mpixel allocation.
	_, err := validateCover(bytes.NewReader(pngHeader(8000, 5001)), 100)
	if err == nil || !strings.Contains(err.Error(), "pixels") {
		t.Errorf("pixel limit: %v", err)
	}
}

// §7.4's order of the candidates, pure.
func TestCoverCandidateOrder(t *testing.T) {
	f := func(p string) coverFile { return coverFile{Path: p} }
	files := []coverFile{f("front.png"), f("Folder.JPG"), f("cover.png"), f("COVER.jpg"), f("sub/cover.jpg"),
		f("cover"), f("covers.jpg"), f("folder.jpg"), f("Cover.jpg"), f("Cover.png")}
	var got []string
	for _, c := range externalCovers(files) {
		got = append(got, c.Path)
	}
	want := []string{"COVER.jpg", "Cover.jpg", "Cover.png", "cover.png", "Folder.JPG", "folder.jpg", "front.png"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("external %v, want %v", got, want)
	}
	pic := func(i int, typ uint32, h string) media.Picture { return media.Picture{Index: i, Type: typ, SHA256: h} }
	tracks := []coverTrack{
		{Path: "1", Pictures: []media.Picture{pic(0, 0, "zz"), pic(1, 3, "bb")}},
		{Path: "2", Pictures: []media.Picture{pic(0, 3, "cc"), pic(1, 3, "aa")}},
		{Path: "3", Pictures: []media.Picture{pic(0, 3, "cc"), pic(1, 3, "bb")}},
	}
	var order []string
	for _, e := range embeddedCandidates(tracks) {
		order = append(order, e.track.Path+":"+e.pic.SHA256)
	}
	// Fronts: bb and cc twice (tie: bb first), aa once; then every picture.
	wantOrder := []string{"1:bb", "2:cc", "2:aa", "1:zz", "1:bb", "2:cc", "2:aa", "3:cc", "3:bb"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("embedded %v, want %v", order, wantOrder)
	}
}

// The source snapshot sees every kind of change (§7.1).
func TestCompareSnapshots(t *testing.T) {
	id := identity{Type: fsops.TypeRegular, Dev: 1, Ino: 2, Size: 3, MTime: 4}
	base := map[string]identity{"": {Type: fsops.TypeDir}, "a": id, "b": id}
	if _, diff := compareSnapshots(base, base); diff {
		t.Error("equal snapshots differ")
	}
	for name, change := range map[string]func(m map[string]identity){
		"added":   func(m map[string]identity) { m["c"] = id },
		"removed": func(m map[string]identity) { delete(m, "b") },
		"ino":     func(m map[string]identity) { x := id; x.Ino++; m["a"] = x },
		"dev":     func(m map[string]identity) { x := id; x.Dev++; m["a"] = x },
		"size":    func(m map[string]identity) { x := id; x.Size++; m["a"] = x },
		"mtime":   func(m map[string]identity) { x := id; x.MTime++; m["a"] = x },
		"type":    func(m map[string]identity) { x := id; x.Type = fsops.TypeSymlink; m["a"] = x },
	} {
		m := map[string]identity{}
		for k, v := range base {
			m[k] = v
		}
		change(m)
		if _, diff := compareSnapshots(base, m); !diff {
			t.Errorf("%s: not seen", name)
		}
	}
}

// N-090 and N-092 on an inspection: an ID3 tag in a FLAC, which the helper
// declares removed, is a warning and the file is accepted; every other field
// a write cannot keep refuses it, and so would an ID3 tag the helper did not
// declare removed (a helper older than N-090).
func TestTagWarnings(t *testing.T) {
	id3v2 := media.OpaqueField{Key: "id3v2", Reason: foreignTag, Removed: true}
	id3v1 := media.OpaqueField{Key: "id3v1", Reason: foreignTag, Removed: true}
	for _, tc := range []struct {
		name     string
		opaque   []media.OpaqueField
		warnings int
		code     string
	}{
		{"nothing opaque", nil, 0, ""},
		{"both ID3 tags", []media.OpaqueField{id3v2, id3v1}, 2, ""},
		{"a managed field the write removes", []media.OpaqueField{{Key: "vorbis:TITLE", Reason: "invalid_utf8", Removed: true}}, 0, ""},
		{"an ID3 tag not declared removed", []media.OpaqueField{{Key: "id3v2", Reason: foreignTag}}, 0, CodeUnrenderableTag},
		{"an ID3 tag and a blocking field", []media.OpaqueField{id3v2, {Key: "vorbis:COMMENT", Reason: "invalid_utf8"}}, 1, CodeUnrenderableTag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := tagWarnings("a/1.flac", media.Inspection{Opaque: tc.opaque})
			if Code(err) != tc.code || (tc.code == "") != (err == nil) {
				t.Fatalf("err %v, want %q", err, tc.code)
			}
			if len(ws) != tc.warnings {
				t.Fatalf("%d warnings, want %d: %+v", len(ws), tc.warnings, ws)
			}
			for _, w := range ws {
				if w.Code != jobs.WarnFLACID3 || w.Path != "a/1.flac" || !strings.Contains(w.Message, "will not carry") {
					t.Errorf("warning %+v", w)
				}
			}
		})
	}
}

// Every code the importer hands to corrupt has its own message, and a code
// without one still gives a message that names the file and the check,
// never a malformed format (round 7 review).
func TestCorruptMessages(t *testing.T) {
	for _, code := range append(slices.Clone(decodeFailures), readerFailures...) {
		if _, ok := corruptMessages[code]; !ok {
			t.Errorf("no message for %s", code)
		}
		err := corrupt("a.flac", &media.Error{Code: code}, code)
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeCorruptAudio || !strings.HasPrefix(e.Message, `"a.flac" `) || strings.Contains(e.Message, "%!") {
			t.Errorf("%s: %v", code, err)
		}
	}
	err := corrupt("b.flac", &media.Error{Code: media.CodeTagsTooLarge}, media.CodeTagsTooLarge)
	var e *Error
	if !errors.As(err, &e) || e.Message != `"b.flac" failed the check `+media.CodeTagsTooLarge {
		t.Errorf("fallback: %v", err)
	}
	other := &media.Error{Code: media.CodeTimeout}
	if got := corrupt("c.flac", other, decodeFailures...); got != other {
		t.Errorf("a code outside the list is kept: %v", got)
	}
}

// N-150 (§10.1): the message stored with a failed job never carries a
// database error's text, wherever it sits in the error; the code stays.
func TestFailureHidesDatabaseText(t *testing.T) {
	pgErr := &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: `duplicate key value violates unique constraint "albums_pkey"`}
	for _, tc := range []struct {
		name     string
		err      error
		code     string
		database bool
	}{
		{"catalog_db", &catalog.Error{Code: catalog.CodeDB, Message: "reading", Err: pgErr}, catalog.CodeDB, true},
		{"a PgError under an importer error", &Error{Code: CodeInvalidTag, Message: "tag", Err: pgErr}, CodeInvalidTag, true},
		{"a PgError alone", fmt.Errorf("commit: %w", pgErr), "import_failed", true},
		{"a content error", &Error{Code: CodeMixedAlbum, Message: "two album tags", Err: errors.New("A and B")}, CodeMixedAlbum, false},
	} {
		code, msg := failure(tc.err)
		if code != tc.code {
			t.Errorf("%s: code %q, want %q", tc.name, code, tc.code)
		}
		if tc.database != (msg == catalog.DatabaseJobMessage) || strings.Contains(msg, "albums_pkey") {
			t.Errorf("%s: message %q", tc.name, msg)
		}
	}
}
