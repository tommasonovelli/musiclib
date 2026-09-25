package media

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The MP3 contract of the tag helper (DESIGN.md §8.1-§8.3, NOTES.md N-152,
// N-153): tags built by the independent codec of id3meta_test.go (and by
// LAME itself) around audio from the pinned LAME, read and written by the
// real helper, checked by §9.1 step 6 (digest, VerifyTags) and by a second,
// independent reading of the written file.

// mp3Source is 3 s of sine encoded by LAME in VBR with its gapless (LAME)
// header, without any tag.
func mp3Source(t testing.TB, dir string) []byte {
	t.Helper()
	return readFile(t, lame(t, wav16(t, dir, "src.wav", sine3s), dir, "src.mp3", "-V2"))
}

// mp3At writes the concatenated parts to dir/name.
func mp3At(t testing.TB, dir, name string, parts ...[]byte) string {
	t.Helper()
	return writeFile(t, filepath.Join(dir, name), bytes.Join(parts, nil))
}

func inspectMP3(t testing.TB, tools *Tools, path string) Inspection {
	t.Helper()
	in, err := tools.Inspect(t.Context(), open(t, path), FormatMP3)
	if err != nil {
		t.Fatalf("Inspect(%s): %v", filepath.Base(path), err)
	}
	return in
}

// writeMP3Checked is §9.1 step 6 on an MP3 (digest, inspection, write,
// inspection, VerifyTags, digest), plus the independent reading of the
// written file: one ID3v2.4 tag, no ID3v1 tag, the audio bytes of the
// original exactly. It returns the inspections and the written parts.
func writeMP3Checked(t testing.TB, tools *Tools, path string, v TagValues, cover *coverFile) (before, after Inspection, out mp3Parts) {
	t.Helper()
	orig := readFile(t, path)
	d0 := digest(t, tools, path)
	before = inspectMP3(t, tools, path)
	if err := tools.WriteManagedTags(t.Context(), openRW(t, path), FormatMP3, v, cover.cover(t)); err != nil {
		t.Fatalf("WriteManagedTags(%s): %v", filepath.Base(path), err)
	}
	after = inspectMP3(t, tools, path)
	if err := VerifyTags(v, cover.expected(t), before, after); err != nil {
		t.Fatalf("VerifyTags(%s): %v\nbefore: %+v\nafter:  %+v", filepath.Base(path), err, before.Unmanaged, after.Unmanaged)
	}
	if err := audioChanged(t, tools, path, d0); err != nil {
		t.Fatal(err)
	}
	out = parseWritten(t, readFile(t, path))
	if out.major != 4 || out.id3v1 != nil {
		t.Fatalf("written file: ID3v2.%d, ID3v1 %v", out.major, out.id3v1 != nil)
	}
	if !bytes.Equal(out.audio, orig[before.Audio.Start:before.Audio.End]) {
		t.Fatal("the write changed the audio bytes")
	}
	if after.Audio.Start != int64(out.tagSize) || after.Audio.End-after.Audio.Start != int64(len(out.audio)) {
		t.Fatalf("the reported audio range %+v is not the independent one", after.Audio)
	}
	return before, after, out
}

func managedOf(in Inspection) map[string][]string {
	m := map[string][]string{}
	for _, f := range in.Managed.fields() {
		if len(*f.v) > 0 {
			m[f.name] = *f.v
		}
	}
	return m
}

func unmanagedOf(in Inspection) map[string][]string {
	m := map[string][]string{}
	for _, kv := range in.Unmanaged {
		m[kv.Key] = kv.Values
	}
	return m
}

func pngCover(t testing.TB, dir string) *coverFile {
	t.Helper()
	return &coverFile{writeFile(t, filepath.Join(dir, "cover.png"), pngImage(t, 8, 8, 3)), FormatPNG}
}

// ---------------------------------------------------------------------------

// No tag at all: nothing managed, the audio's hash as the one unmanaged
// field; a write adds exactly the managed frames and the cover, and a
// second write gives the same bytes.
func TestTagsMP3NoTags(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := mp3Source(t, dir)
	p := mp3At(t, dir, "plain.mp3", src)
	in := inspectMP3(t, tools, p)
	if len(managedOf(in)) > 0 || len(in.Conflicts) > 0 || len(in.Pictures) > 0 || len(in.Opaque) > 0 {
		t.Fatalf("inspection of an untagged file: %+v", in)
	}
	if !reflect.DeepEqual(in.Unmanaged, []KeyValues{{"mpeg.audio", []string{sha(src)}}}) ||
		in.Audio != (AudioRange{0, int64(len(src))}) {
		t.Fatalf("unmanaged %+v, audio %+v", in.Unmanaged, in.Audio)
	}

	cover := pngCover(t, dir)
	_, after, out := writeMP3Checked(t, tools, p, fullValues, cover)
	want := []id3Frame{
		frame("TIT2", textBody(encUTF8, "Title")), frame("TPE1", textBody(encUTF8, "Artist")),
		frame("TPE2", textBody(encUTF8, "Album Artist")), frame("TALB", textBody(encUTF8, "Album")),
		frame("TRCK", textBody(encUTF8, "3/12")), frame("TPOS", textBody(encUTF8, "1/2")),
		frame("TDRC", textBody(encUTF8, "1999")), frame("TCON", textBody(encUTF8, "Genre")),
		frame("TCMP", textBody(encUTF8, "1")),
		frame("APIC", apicBody("image/png", 3, "", readFile(t, cover.path))),
	}
	if !reflect.DeepEqual(out.frames, want) || len(out.padding) != 1024 || out.ape != nil {
		t.Fatalf("written frames %q, padding %d, APE %v", frameIDs(out.frames), len(out.padding), out.ape != nil)
	}
	if len(after.Pictures) != 1 || after.Pictures[0].Location != "id3v2" {
		t.Fatalf("pictures after the write: %+v", after.Pictures)
	}

	// Idempotent, and deterministic over copies.
	first := readFile(t, p)
	writeMP3Checked(t, tools, p, fullValues, cover)
	if !bytes.Equal(first, readFile(t, p)) {
		t.Fatal("a second identical write changed the bytes")
	}
	for i := range 2 {
		q := mp3At(t, t.TempDir(), "copy.mp3", src)
		writeMP3Checked(t, tools, q, fullValues, cover)
		if !bytes.Equal(first, readFile(t, q)) {
			t.Fatalf("copy %d: the same write gave other bytes", i)
		}
	}

	// A write that removes everything leaves an empty tag, no picture.
	_, _, out = writeMP3Checked(t, tools, p, TagValues{}, nil)
	if len(out.frames) != 0 {
		t.Fatalf("frames left: %q", frameIDs(out.frames))
	}

	// What an MP3 cannot hold is refused before anything is written.
	for _, v := range []TagValues{
		{Genre: "(13)"}, {Genre: "13"}, {Genre: "(RX)Remix"}, {Genre: "(Rock)"}, {TrackTotal: 3}, {DiscTotal: 2},
	} {
		wantUnchanged(t, p, func() error {
			return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatMP3, v, nil)
		}, CodeTagsInvalidRequest)
	}
	// Genres that do read back as themselves are written.
	for _, g := range []string{"200", "(Rock", "Rock (13)", "13 Songs"} {
		_, after, _ := writeMP3Checked(t, tools, p, TagValues{Genre: g}, nil)
		if !slices.Equal(after.Managed.Genre, []string{g}) {
			t.Fatalf("genre %q read back as %q", g, after.Managed.Genre)
		}
	}
}

// richV23 is an ID3v2.3 tag with every kind of frame: the managed ones and
// their aliases, the old date frames, the sort frames, and unmanaged frames
// of every form (§12.1: "tag non gestiti").
func richV23() ([]id3Frame, []id3Frame) {
	managed := []id3Frame{
		frame("TIT2", textBody(encLatin1, "Old title")),
		frame("TPE1", textBody(encUTF16, "Artïst")),
		frame("TPE2", textBody(encLatin1, "Old album artist")),
		frame("TXXX", txxxBody(encLatin1, "ALBUM ARTIST", "Other album artist")),
		frame("TALB", textBody(encUTF8, "Old album")),
		frame("TRCK", textBody(encLatin1, "3/12")),
		frame("TXXX", txxxBody(encLatin1, "TOTALTRACKS", "12")),
		frame("TPOS", textBody(encLatin1, "1/2")),
		frame("TYER", textBody(encLatin1, "2001")),
		frame("TDAT", textBody(encLatin1, "0102")),
		frame("TIME", textBody(encLatin1, "1200")),
		frame("TRDA", textBody(encLatin1, "Jan 2001")),
		frame("TCON", textBody(encLatin1, "(13)")),
		frame("TCMP", textBody(encLatin1, "1")),
		frame("TSOT", textBody(encLatin1, "Title, Old")),
		frame("TSOP", textBody(encLatin1, "Artist sort")),
		frame("TSO2", textBody(encLatin1, "AA sort")),
		frame("TSOA", textBody(encLatin1, "Album sort")),
		frame("XSOP", textBody(encLatin1, "MusicBrainz sort")),
		frame("TXXX", txxxBody(encLatin1, "ALBUMARTISTSORT", "AA sort")),
		frame("APIC", apicBody("image/jpeg", 3, "front", []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2})),
		frame("APIC", apicBody("image/png", 4, "back", []byte("\x89PNG back"))),
	}
	unmanaged := []id3Frame{
		frame("COMM", commBody(encLatin1, "eng", "", "a comment")),
		frame("COMM", commBody(encUTF16, "deu", "second", "zweite Anmerkung")),
		frame("USLT", commBody(encUTF8, "eng", "", "la la\nla")),
		frame("TXXX", txxxBody(encLatin1, "REPLAYGAIN_TRACK_GAIN", "-6.50 dB")),
		frame("TCOM", textBody(encLatin1, "Composer")),
		frame("TSOC", textBody(encLatin1, "Composer, The")),
		frame("UFID", ownerBody("http://musicbrainz.org", []byte("abc-123"))),
		frame("PRIV", ownerBody("com.example", []byte{1, 2, 3})),
		frame("WOAR", []byte("http://artist.example")),
		frame("POPM", ownerBody("me@example.org", []byte{196, 0, 0, 0, 7})),
		frame("TSIZ", textBody(encLatin1, "12345")),
		frame("XYZW", []byte{9, 8, 7}),
		// The original release year: unmanaged, kept (owner decision N-161).
		frame("TORY", textBody(encLatin1, "1999")),
	}
	return managed, unmanaged
}

// An ID3v2.3 tag, then the same frames in ID3v2.4: the managed fields by
// §8.1, every unmanaged frame kept byte for byte, every alias, old date and
// sort frame removed (§8.2), and the audio untouched.
func TestTagsMP3RichTags(t *testing.T) {
	dir := t.TempDir()
	src := mp3Source(t, dir)
	managed, unmanaged := richV23()
	frames := append(slices.Clone(managed), unmanaged...)
	wantManaged := map[string][]string{
		"title": {"Old title"}, "artist": {"Artïst"}, "album_artist": {"Old album artist"}, "album": {"Old album"},
		"track": {"3"}, "track_total": {"12"}, "disc": {"1"}, "disc_total": {"2"}, "date": {"2001"},
		"genre": {"Pop"}, "compilation": {"1"},
	}
	wantUnmanaged := map[string][]string{
		"id3v2:COMM:eng:":                   {"a comment"},
		"id3v2:COMM:deu:second":             {"zweite Anmerkung"},
		"id3v2:USLT:eng:":                   {"la la\nla"},
		"id3v2:TXXX:REPLAYGAIN_TRACK_GAIN":  {"-6.50 dB"},
		"id3v2:TCOM":                        {"Composer"},
		"id3v2:TSOC":                        {"Composer, The"},
		"id3v2:UFID:http://musicbrainz.org": {"hex:" + hex.EncodeToString([]byte("abc-123"))},
		"id3v2:PRIV:com.example":            {"sha256:" + sha([]byte{1, 2, 3})},
		"id3v2:WOAR":                        {"http://artist.example"},
		"id3v2:POPM":                        {"sha256:" + sha(ownerBody("me@example.org", []byte{196, 0, 0, 0, 7}))},
		"id3v2:TSIZ":                        {"12345"},
		"id3v2:XYZW":                        {"sha256:" + sha([]byte{9, 8, 7})},
		"id3v2:TORY":                        {"1999"},
		"mpeg.audio":                        {sha(src)},
	}
	for name, tools := range helpers(t) {
		for _, major := range []byte{3, 4} {
			t.Run(name+"/v2."+string(rune('0'+major)), func(t *testing.T) {
				p := mp3At(t, t.TempDir(), "rich.mp3", id3Tag(major, 0, 64, frames...), src)
				in := inspectMP3(t, tools, p)
				if !reflect.DeepEqual(managedOf(in), wantManaged) {
					t.Fatalf("managed: %v", managedOf(in))
				}
				if len(in.Conflicts) != 1 || in.Conflicts[0].Field != "album_artist" ||
					in.Conflicts[0].Sources[0].Key != "id3v2:TPE2" || in.Conflicts[0].Sources[1].Key != "id3v2:TXXX:ALBUM ARTIST" {
					t.Fatalf("conflicts: %+v", in.Conflicts)
				}
				if !reflect.DeepEqual(unmanagedOf(in), wantUnmanaged) || len(in.Opaque) > 0 {
					t.Fatalf("unmanaged: %v\nopaque: %+v", unmanagedOf(in), in.Opaque)
				}
				if len(in.Pictures) != 2 || in.Pictures[0].Type != 3 || in.Pictures[0].MIME != "image/jpeg" ||
					in.Pictures[1].Type != 4 || in.Pictures[1].Location != "id3v2" {
					t.Fatalf("pictures: %+v", in.Pictures)
				}
				cover := pngCover(t, t.TempDir())
				_, after, out := writeMP3Checked(t, tools, p, fullValues, cover)
				if !slices.Equal(unmanagedOf(after)["id3v2:TORY"], []string{"1999"}) {
					t.Fatalf("TORY after the write: %v", unmanagedOf(after))
				}
				// The managed frames, then every unmanaged frame as it was,
				// in order, then the cover: nothing of the old names left.
				got := out.frames[9 : len(out.frames)-1]
				if len(got) != len(unmanaged) {
					t.Fatalf("written frames %q", frameIDs(out.frames))
				}
				for i, f := range got {
					if f.id != unmanaged[i].id || !bytes.Equal(f.body, unmanaged[i].body) || f.flags != [2]byte{} {
						t.Fatalf("frame %d: %s %x, want %s %x", i, f.id, f.body, unmanaged[i].id, unmanaged[i].body)
					}
				}
			})
		}
	}
}

// §8.1: the first non-empty value in ID3v2, then APE, then ID3v1, every
// disagreeing source reported as a conflict.
func TestTagsMP3ReadPrecedence(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := mp3Source(t, dir)
	id3 := id3Tag(4, 0, 0, frame("TIT2", textBody(encUTF8, "id3 title")), frame("TDRC", textBody(encUTF8, "2003-04-05")))
	ape := apeTag(true,
		apeItem("Title", 0, []byte("ape title")), apeItem("Album Artist", 0, []byte("ape album artist")),
		apeItem("Track", 0, []byte("4/10")), apeItem("Year", 0, []byte("2003-04-05")))
	v1 := id3v1("v1 title", "v1 artist", "", "1990", "", 5, 17)

	for _, tc := range []struct {
		name      string
		parts     [][]byte
		managed   map[string][]string
		conflicts map[string][]string
	}{
		{"all three", [][]byte{id3, src, ape, v1},
			map[string][]string{"title": {"id3 title"}, "album_artist": {"ape album artist"}, "artist": {"v1 artist"},
				"track": {"4"}, "track_total": {"10"}, "date": {"2003-04-05"}, "genre": {"Rock"}},
			map[string][]string{"title": {"id3v2:TIT2", "ape:TITLE", "id3v1:title"}, "track": {"ape:TRACK", "id3v1:track"},
				"date": {"id3v2:TDRC", "ape:YEAR", "id3v1:year"}}},
		{"APE and ID3v1", [][]byte{src, ape, v1},
			map[string][]string{"title": {"ape title"}, "album_artist": {"ape album artist"}, "artist": {"v1 artist"},
				"track": {"4"}, "track_total": {"10"}, "date": {"2003-04-05"}, "genre": {"Rock"}},
			map[string][]string{"title": {"ape:TITLE", "id3v1:title"}, "track": {"ape:TRACK", "id3v1:track"},
				"date": {"ape:YEAR", "id3v1:year"}}},
		{"ID3v1 alone", [][]byte{src, v1},
			map[string][]string{"title": {"v1 title"}, "artist": {"v1 artist"}, "track": {"5"}, "date": {"1990"}, "genre": {"Rock"}},
			map[string][]string{}},
		{"ID3v2 and APE agreeing", [][]byte{id3, src, apeTag(false, apeItem("TITLE", 0, []byte("id3 title")))},
			map[string][]string{"title": {"id3 title"}, "date": {"2003-04-05"}}, map[string][]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mp3At(t, t.TempDir(), "f.mp3", tc.parts...)
			in := inspectMP3(t, tools, p)
			if !reflect.DeepEqual(managedOf(in), tc.managed) {
				t.Fatalf("managed: %v", managedOf(in))
			}
			conflicts := map[string][]string{}
			for _, c := range in.Conflicts {
				for _, s := range c.Sources {
					conflicts[c.Field] = append(conflicts[c.Field], s.Key)
				}
			}
			if !reflect.DeepEqual(conflicts, tc.conflicts) {
				t.Fatalf("conflicts: %v", conflicts)
			}
			// Every source is a managed field: the write leaves no APE and no
			// ID3v1 tag.
			_, _, out := writeMP3Checked(t, tools, p, TagValues{Title: "db"}, nil)
			if out.ape != nil || !reflect.DeepEqual(frameIDs(out.frames), []string{"TIT2"}) {
				t.Fatalf("written: APE %v, frames %q", out.ape != nil, frameIDs(out.frames))
			}
		})
	}
}

// §8.3: the APE tag keeps its unmanaged items byte for byte and loses only
// the managed keys, their aliases, the sort keys and the pictures; an APE
// tag left without items is removed.
func TestTagsMP3APE(t *testing.T) {
	dir := t.TempDir()
	src := mp3Source(t, dir)
	png := pngImage(t, 4, 4, 9)
	managed := [][]byte{
		apeItem("Title", 0, []byte("t")), apeItem("ARTIST", 0, []byte("a")), apeItem("album artist", 0, []byte("aa")),
		apeItem("AlbumArtist", 0, []byte("aa")), apeItem("Album", 0, []byte("al")), apeItem("Track", 0, []byte("1/9")),
		apeItem("TOTALTRACKS", 0, []byte("9")), apeItem("Disc", 0, []byte("1")), apeItem("Year", 0, []byte("2000")),
		apeItem("Genre", 0, []byte("Jazz")), apeItem("Compilation", 0, []byte("1")),
		apeItem("TitleSort", 0, []byte("t")), apeItem("ALBUMSORT", 0, []byte("al")),
		apeItem("Cover Art (Front)", 2, append([]byte("front.png\x00"), png...)),
		apeItem("Cover Art (Back)", 2, append([]byte("back.png\x00"), png...)),
	}
	kept := [][]byte{
		apeItem("REPLAYGAIN_TRACK_GAIN", 0, []byte("-3.1 dB")),
		apeItem("Comment", 0, []byte("one\x00two")),
		apeItem("Blob", 2, []byte{0, 1, 2, 255}),
		apeItem("Catalog", 1, []byte("CAT-1")),
		apeItem("Link", 4, []byte("http://example.org")),
		apeItem("ArtistSortOrder", 0, []byte("not a managed key")),
	}
	all := append(slices.Clone(managed), kept...)
	for name, tools := range helpers(t) {
		for _, header := range []bool{true, false} {
			t.Run(name+"/header="+map[bool]string{true: "yes", false: "no"}[header], func(t *testing.T) {
				p := mp3At(t, t.TempDir(), "ape.mp3", src, apeTag(header, all...))
				in := inspectMP3(t, tools, p)
				um := unmanagedOf(in)
				if !slices.Equal(um["ape:COMMENT"], []string{"one", "two"}) ||
					!slices.Equal(um["ape:BLOB"], []string{"flags=00000002 sha256:" + sha([]byte{0, 1, 2, 255})}) ||
					!slices.Equal(um["ape:CATALOG"], []string{"flags=00000001 sha256:" + sha([]byte("CAT-1"))}) ||
					len(um) != len(kept)+1 {
					t.Fatalf("unmanaged: %v", um)
				}
				if len(in.Pictures) != 2 || in.Pictures[0].Location != "ape" || in.Pictures[0].Type != 3 ||
					in.Pictures[1].Type != 4 || in.Pictures[0].SHA256 != sha(png) {
					t.Fatalf("pictures: %+v", in.Pictures)
				}
				if !slices.Equal(in.Managed.Track, []string{"1"}) || !slices.Equal(in.Managed.TrackTotal, []string{"9"}) {
					t.Fatalf("track %v / %v", in.Managed.Track, in.Managed.TrackTotal)
				}
				_, _, out := writeMP3Checked(t, tools, p, fullValues, nil)
				if !reflect.DeepEqual(out.items, kept) {
					t.Fatalf("APE items after the write: %q", out.items)
				}
				if header != bytes.HasPrefix(out.ape, []byte("APETAGEX")) {
					t.Fatal("the APE header was not kept as it was")
				}
			})
		}
	}
	t.Run("only managed items", func(t *testing.T) {
		tools := newTools(t)
		p := mp3At(t, t.TempDir(), "ape.mp3", src, apeTag(true, managed...))
		_, _, out := writeMP3Checked(t, tools, p, fullValues, nil)
		if out.ape != nil {
			t.Fatal("an APE tag without items was kept")
		}
	})
}

// §8.3: the ID3v1 tag is removed; its non-empty comment moves to a COMM
// frame, unless one already holds it; otherwise its description is
// "legacy-id3v1".
func TestTagsMP3ID3v1Migration(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := mp3Source(t, dir)
	legacy := func(text string) id3Frame { return frame("COMM", commBody(encUTF8, "XXX", "legacy-id3v1", text)) }
	existing := frame("COMM", commBody(encUTF16, "eng", "", "Café comment"))
	other := frame("COMM", commBody(encLatin1, "eng", "", "another comment"))
	for _, tc := range []struct {
		name    string
		parts   [][]byte
		comment []string // the id3v1:comment before
		want    []id3Frame
	}{
		{"ID3v1.1 alone", [][]byte{src, id3v1("T", "A", "Al", "1999", "Café comment", 7, 255)},
			[]string{"Café comment"}, []id3Frame{legacy("Café comment")}},
		{"ID3v1.0, 30 characters", [][]byte{src, id3v1("", "", "", "", strings.Repeat("c", 30), 0, 255)},
			[]string{strings.Repeat("c", 30)}, []id3Frame{legacy(strings.Repeat("c", 30))}},
		{"surrounding spaces", [][]byte{src, id3v1("", "", "", "", "  padded  ", 0, 255)},
			[]string{"padded"}, []id3Frame{legacy("padded")}},
		{"a COMM already holds it", [][]byte{id3Tag(3, 0, 0, existing), src, id3v1("", "", "", "", "Café comment", 0, 255)},
			[]string{"Café comment"}, []id3Frame{existing}},
		{"another COMM", [][]byte{id3Tag(3, 0, 0, other), src, id3v1("", "", "", "", "Café comment", 0, 255)},
			[]string{"Café comment"}, []id3Frame{other, legacy("Café comment")}},
		{"an empty comment", [][]byte{src, id3v1("T", "", "", "", "   ", 0, 255)}, nil, nil},
		{"with an APE tag", [][]byte{src, apeTag(true, apeItem("Mood", 0, []byte("calm"))), id3v1("", "", "", "", "x", 0, 255)},
			[]string{"x"}, []id3Frame{legacy("x")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mp3At(t, t.TempDir(), "f.mp3", tc.parts...)
			before, after, out := writeMP3Checked(t, tools, p, TagValues{}, nil)
			if !slices.Equal(unmanagedOf(before)["id3v1:comment"], tc.comment) {
				t.Fatalf("id3v1:comment before: %q", unmanagedOf(before)["id3v1:comment"])
			}
			if _, ok := unmanagedOf(after)["id3v1:comment"]; ok {
				t.Fatal("id3v1:comment after the write")
			}
			if !reflect.DeepEqual(out.frames, tc.want) {
				t.Fatalf("frames: %q", frameIDs(out.frames))
			}
		})
	}
	// A "legacy-id3v1" comment that holds another text: a second one would
	// break the uniqueness of COMM frames. Refused, never overwritten.
	p := mp3At(t, t.TempDir(), "f.mp3", id3Tag(4, 0, 0, legacy("old")), src, id3v1("", "", "", "", "new", 0, 255))
	in := inspectMP3(t, tools, p)
	if !reflect.DeepEqual(in.Blocking(), []OpaqueField{{Key: "id3v1:comment", Reason: "migration_conflict"}}) {
		t.Fatalf("opaque: %+v", in.Opaque)
	}
	wantUnchanged(t, p, func() error {
		return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatMP3, TagValues{}, nil)
	}, CodeTagsOpaque)
}

// Text in every encoding, decoded correctly (N-092: only genuinely invalid
// text is refused): Latin-1, UTF-16 with either byte order, UTF-16BE, UTF-8,
// astral characters, NFD kept byte for byte, multi-valued frames.
func TestTagsMP3Text(t *testing.T) {
	dir := t.TempDir()
	src := mp3Source(t, dir)
	nfd := "Café"
	good := id3Tag(4, 0, 0,
		frame("TIT2", append([]byte{encUTF16}, encode(encUTF16, "𝄞 Title", true)...)),
		frame("TPE1", textBody(encUTF16BE, "Björk", "Guðmundsdóttir")),
		frame("TALB", textBody(encUTF8, nfd)),
		frame("TCOM", textBody(encLatin1, "Müller")),
		frame("TXXX", txxxBody(encUTF16, "Mood", "Ruhig", "Still")),
		frame("TIT3", textBody(encUTF16, "a", "b")),
	)
	for name, tools := range helpers(t) {
		t.Run(name, func(t *testing.T) {
			p := mp3At(t, t.TempDir(), "text.mp3", good, src)
			in := inspectMP3(t, tools, p)
			if !slices.Equal(in.Managed.Title, []string{"𝄞 Title"}) || !slices.Equal(in.Managed.Artist, []string{"Björk", "Guðmundsdóttir"}) ||
				!slices.Equal(in.Managed.Album, []string{nfd}) {
				t.Fatalf("managed: %v", managedOf(in))
			}
			um := unmanagedOf(in)
			if !slices.Equal(um["id3v2:TCOM"], []string{"Müller"}) || !slices.Equal(um["id3v2:TXXX:Mood"], []string{"Ruhig", "Still"}) ||
				!slices.Equal(um["id3v2:TIT3"], []string{"a", "b"}) {
				t.Fatalf("unmanaged: %v", um)
			}
			writeMP3Checked(t, tools, p, TagValues{Title: "𝄞 " + nfd}, nil)
		})
	}

	// Invalid text: in a managed frame, the write replaces it (reported as
	// removed); in an unmanaged frame, the file is refused (N-092).
	tools := newTools(t)
	loneSurrogate := []byte{encUTF16, 0xFF, 0xFE, 0x00, 0xD8}
	for _, tc := range []struct {
		name  string
		f     id3Frame
		want  OpaqueField
		write string
	}{
		{"managed, lone surrogate", frame("TIT2", loneSurrogate), OpaqueField{"id3v2:TIT2", "invalid_text", true}, ""},
		{"managed alias, bad UTF-8", frame("TXXX", append(txxxBody(encUTF8, "ALBUMARTIST"), 0xC3)), OpaqueField{"id3v2:TXXX:ALBUMARTIST", "invalid_text", true}, ""},
		{"unmanaged, lone surrogate", frame("TCOM", loneSurrogate), OpaqueField{"id3v2:TCOM", "invalid_text", false}, CodeTagsOpaque},
		{"unmanaged, UTF-16 without BOM", frame("TCOM", []byte{encUTF16, 'a', 0}), OpaqueField{"id3v2:TCOM", "invalid_text", false}, CodeTagsOpaque},
		{"unmanaged, encoding 4", frame("COMM", []byte{4, 'e', 'n', 'g', 0, 'x'}), OpaqueField{"id3v2:COMM", "invalid_text", false}, CodeTagsOpaque},
		{"unmanaged, bad UTF-8 description", frame("TXXX", []byte{encUTF8, 0xFF, 0, 'x'}), OpaqueField{"id3v2:TXXX", "invalid_text", false}, CodeTagsOpaque},
		{"UFID without owner end", frame("UFID", []byte("owner")), OpaqueField{"id3v2:UFID", "invalid_text", false}, CodeTagsOpaque},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mp3At(t, t.TempDir(), "bad.mp3", id3Tag(4, 0, 0, tc.f), src)
			in := inspectMP3(t, tools, p)
			if !reflect.DeepEqual(in.Opaque, []OpaqueField{tc.want}) {
				t.Fatalf("opaque: %+v", in.Opaque)
			}
			if tc.write == "" {
				writeMP3Checked(t, tools, p, fullValues, nil)
				return
			}
			wantUnchanged(t, p, func() error {
				return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatMP3, fullValues, nil)
			}, tc.write)
		})
	}
}

// Unsynchronisation: of the whole ID3v2.3 tag, of one ID3v2.4 frame, and of
// every ID3v2.4 frame (the tag flag). The values are read without the
// inserted bytes, and written back without unsynchronisation.
func TestTagsMP3Unsynchronisation(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := mp3Source(t, dir)
	jpegLike := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0xFF, 0x00, 0xFF}
	title := textBody(encLatin1, "ÿà") // 0xFF 0xE0: needs unsynchronisation
	unsyncFrame := func(id string, body []byte) id3Frame {
		return id3Frame{id: id, flags: [2]byte{0, 0x02}, body: unsynchronise(body)}
	}
	for _, tc := range []struct {
		name string
		tag  []byte
	}{
		{"ID3v2.3 tag", id3Tag(3, 0x80, 0, frame("TIT2", title), frame("APIC", apicBody("image/jpeg", 3, "", jpegLike)),
			frame("COMM", commBody(encLatin1, "eng", "", "ÿà comment")))},
		{"ID3v2.4 frames", id3Tag(4, 0, 0, unsyncFrame("TIT2", title), unsyncFrame("APIC", apicBody("image/jpeg", 3, "", jpegLike)),
			unsyncFrame("COMM", commBody(encLatin1, "eng", "", "ÿà comment")))},
		{"ID3v2.4 tag flag", id3Tag(4, 0x80, 0, unsyncFrame("TIT2", title), unsyncFrame("APIC", apicBody("image/jpeg", 3, "", jpegLike)),
			unsyncFrame("COMM", commBody(encLatin1, "eng", "", "ÿà comment")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mp3At(t, t.TempDir(), "u.mp3", tc.tag, src)
			in := inspectMP3(t, tools, p)
			if !slices.Equal(in.Managed.Title, []string{"ÿà"}) || len(in.Pictures) != 1 || in.Pictures[0].SHA256 != sha(jpegLike) {
				t.Fatalf("managed %v, pictures %+v, opaque %+v", managedOf(in), in.Pictures, in.Opaque)
			}
			picDir := t.TempDir()
			dst := createEmpty(t, picDir, "pic")
			if _, err := tools.ExtractImages(t.Context(), open(t, p), FormatMP3, []ImageTarget{{0, dst}}); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(picDir, "pic")); !bytes.Equal(got, jpegLike) {
				t.Fatalf("extracted %x", got)
			}
			_, _, out := writeMP3Checked(t, tools, p, TagValues{}, nil)
			if len(out.frames) != 1 || !bytes.Equal(out.frames[0].body, commBody(encLatin1, "eng", "", "ÿà comment")) {
				t.Fatalf("frames: %q", frameIDs(out.frames))
			}
		})
	}
}

// Frames with flags are kept byte for byte, their flags converted to their
// ID3v2.4 form (no zlib in the build, N-152): an ID3v2.3 compressed frame,
// an encrypted and grouped one, a read-only one. A managed frame the reader
// cannot read is removed like any managed frame; frames that cannot be kept
// refuse the write.
func TestTagsMP3FlaggedFrames(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := mp3Source(t, dir)
	compressed := id3Frame{id: "COMM", flags: [2]byte{0, 0x80}, body: append(be32(99), "zzz"...)}
	encrypted := id3Frame{id: "PRIV", flags: [2]byte{0, 0x60}, body: []byte{7, 9, 'p', 'p'}}
	readOnly := id3Frame{id: "TCOM", flags: [2]byte{0x20, 0}, body: textBody(encLatin1, "c")}
	compressedTitle := id3Frame{id: "TIT2", flags: [2]byte{0, 0x80}, body: append(be32(5), "yyy"...)}
	p := mp3At(t, dir, "flags.mp3", id3Tag(3, 0, 0, compressed, encrypted, readOnly, compressedTitle), src)
	in := inspectMP3(t, tools, p)
	um := unmanagedOf(in)
	if !slices.Equal(um["id3v2:COMM"], []string{"flags=0009 length=99 sha256:" + sha([]byte("zzz"))}) ||
		!slices.Equal(um["id3v2:PRIV"], []string{"flags=0044 group=09 method=07 sha256:" + sha([]byte("pp"))}) ||
		!slices.Equal(um["id3v2:TCOM"], []string{"flags=1000 sha256:" + sha(textBody(encLatin1, "c"))}) {
		t.Fatalf("unmanaged: %v", um)
	}
	if !reflect.DeepEqual(in.Opaque, []OpaqueField{{"id3v2:TIT2", "compressed_frame", true}}) {
		t.Fatalf("opaque: %+v", in.Opaque)
	}
	_, _, out := writeMP3Checked(t, tools, p, TagValues{Title: "new"}, nil)
	want := []id3Frame{
		frame("TIT2", textBody(encUTF8, "new")),
		{id: "COMM", flags: [2]byte{0, 0x09}, body: append(synchsafe(99), "zzz"...)},
		{id: "PRIV", flags: [2]byte{0, 0x44}, body: []byte{9, 7, 'p', 'p'}},
		{id: "TCOM", flags: [2]byte{0x10, 0}, body: textBody(encLatin1, "c")},
	}
	if !reflect.DeepEqual(out.frames, want) {
		t.Fatalf("written frames: %+v", out.frames)
	}

	// ID3v2.2: frames mapped to ID3v2.4; PIC read as a picture; CRM, LNK
	// and unknown identifiers cannot be kept.
	v22 := id3Tag(2, 0, 0,
		frame("TT2", textBody(encLatin1, "v22 title")), frame("TP1", textBody(encLatin1, "v22 artist")),
		frame("COM", commBody(encLatin1, "eng", "", "v22 comment")), frame("TYE", textBody(encLatin1, "1998")),
		frame("TOA", textBody(encLatin1, "Original artist")),
		frame("TOR", textBody(encLatin1, "1997")),
		frame("PIC", append(append([]byte{encLatin1}, "PNG"...), append([]byte{3, 0}, "\x89PNGdata"...)...)))
	p = mp3At(t, t.TempDir(), "v22.mp3", v22, src)
	in = inspectMP3(t, tools, p)
	if !slices.Equal(in.Managed.Title, []string{"v22 title"}) || !slices.Equal(in.Managed.Date, []string{"1998"}) ||
		len(in.Pictures) != 1 || in.Pictures[0].MIME != "image/png" || in.Pictures[0].Type != 3 ||
		!slices.Equal(unmanagedOf(in)["id3v2:TOPE"], []string{"Original artist"}) {
		t.Fatalf("ID3v2.2: %v %v %+v", managedOf(in), unmanagedOf(in), in.Pictures)
	}
	_, _, out = writeMP3Checked(t, tools, p, TagValues{}, nil)
	if !reflect.DeepEqual(out.frames, []id3Frame{frame("COMM", commBody(encLatin1, "eng", "", "v22 comment")),
		frame("TOPE", textBody(encLatin1, "Original artist")), frame("TORY", textBody(encLatin1, "1997"))}) {
		t.Fatalf("ID3v2.2 written: %+v", out.frames)
	}

	for _, tc := range []struct {
		name string
		tag  []byte
		want OpaqueField
	}{
		{"ID3v2.2 CRM", id3Tag(2, 0, 0, frame("CRM", []byte("x"))), OpaqueField{"id3v2:CRM", "unsupported_frame", false}},
		{"ID3v2.2 unknown", id3Tag(2, 0, 0, frame("XYZ", []byte("x"))), OpaqueField{"id3v2:XYZ", "unsupported_frame", false}},
		{"ID3v2.3 unknown flag", id3Tag(3, 0, 0, id3Frame{id: "TCOM", flags: [2]byte{0, 0x01}, body: []byte("\x00c")}), OpaqueField{"id3v2:TCOM", "unknown_flags", false}},
		{"ID3v2.4 unknown flag", id3Tag(4, 0, 0, id3Frame{id: "TCOM", flags: [2]byte{0x80, 0}, body: []byte("\x00c")}), OpaqueField{"id3v2:TCOM", "unknown_flags", false}},
		{"ID3v2.4 compression without length", id3Tag(4, 0, 0, id3Frame{id: "TCOM", flags: [2]byte{0, 0x08}, body: []byte("zz")}), OpaqueField{"id3v2:TCOM", "malformed_frame", false}},
		{"lower-case identifier", id3Tag(4, 0, 0, frame("tcom", []byte("\x00c"))), OpaqueField{"id3v2#0", "malformed_frame", false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := mp3At(t, t.TempDir(), "f.mp3", tc.tag, src)
			if in := inspectMP3(t, tools, q); !reflect.DeepEqual(in.Opaque, []OpaqueField{tc.want}) {
				t.Fatalf("opaque: %+v", in.Opaque)
			}
			wantUnchanged(t, q, func() error {
				return tools.WriteManagedTags(t.Context(), openRW(t, q), FormatMP3, fullValues, nil)
			}, CodeTagsOpaque)
		})
	}
	// An empty frame holds nothing: removed, not blocking.
	q := mp3At(t, t.TempDir(), "empty.mp3", id3Tag(3, 0, 0, frame("TCOM", nil), frame("TPE3", textBody(encLatin1, "c"))), src)
	if in := inspectMP3(t, tools, q); !reflect.DeepEqual(in.Opaque, []OpaqueField{{"id3v2:TCOM", "empty_frame", true}}) {
		t.Fatalf("opaque: %+v", in.Opaque)
	}
	writeMP3Checked(t, tools, q, TagValues{}, nil)
}

// TCON: ID3v2.3 references "(n)", "(RX)", "(CR)" and refinements, ID3v2.4
// numbers, and plain text (ID3v1 genres by TagLib's list).
func TestTagsMP3Genre(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := mp3Source(t, dir)
	for _, tc := range []struct {
		values []string
		want   []string
	}{
		{[]string{"(13)"}, []string{"Pop"}},
		{[]string{"(17)Rock"}, []string{"Rock"}},
		{[]string{"(17)Hard Rock"}, []string{"Rock", "Hard Rock"}},
		{[]string{"(13)(RX)"}, []string{"Pop", "Remix"}},
		{[]string{"(CR)"}, []string{"Cover"}},
		{[]string{"((13) literal"}, []string{"(13) literal"}},
		{[]string{"13"}, []string{"Pop"}},
		{[]string{"200"}, []string{"200"}},
		{[]string{"(999)"}, []string{"(999)"}},
		{[]string{"Rock", "0"}, []string{"Rock", "Blues"}},
	} {
		p := mp3At(t, t.TempDir(), "g.mp3", id3Tag(4, 0, 0, frame("TCON", textBody(encLatin1, tc.values...))), src)
		if got := inspectMP3(t, tools, p).Managed.Genre; !slices.Equal(got, tc.want) {
			t.Errorf("TCON %q read as %q, want %q", tc.values, got, tc.want)
		}
	}
	// ID3v1 genre byte: 17 is Rock, 255 is none, 200 is out of the list.
	for g, want := range map[byte][]string{17: {"Rock"}, 255: nil, 200: nil} {
		p := mp3At(t, t.TempDir(), "g.mp3", src, id3v1("", "", "", "", "", 0, g))
		if got := inspectMP3(t, tools, p).Managed.Genre; !slices.Equal(got, want) {
			t.Errorf("ID3v1 genre %d read as %q", g, got)
		}
	}
}

// Pictures of every form are listed and extracted byte for byte: several
// APIC frames, an APE cover; a write keeps exactly the one cover asked.
func TestTagsMP3Pictures(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := mp3Source(t, dir)
	datas := [][]byte{[]byte("front"), []byte("back"), []byte("other"), []byte("ape front")}
	p := mp3At(t, dir, "pics.mp3", id3Tag(4, 0, 0,
		frame("APIC", apicBody("image/jpeg", 3, "a", datas[0])),
		frame("APIC", append([]byte{encUTF16}, append(append([]byte("image/png\x00\x04"), encode(encUTF16, "desc", false)...), append([]byte{0, 0}, datas[1]...)...)...)),
		frame("APIC", apicBody("", 0, "", datas[2]))),
		src, apeTag(true, apeItem("Cover Art (Front)", 2, append([]byte("f.jpg\x00"), datas[3]...))))
	in := inspectMP3(t, tools, p)
	if len(in.Pictures) != 4 {
		t.Fatalf("pictures: %+v", in.Pictures)
	}
	var targets []ImageTarget
	picDir := t.TempDir()
	for i := range in.Pictures {
		targets = append(targets, ImageTarget{i, createEmpty(t, picDir, strconv.Itoa(i))})
	}
	if _, err := tools.ExtractImages(t.Context(), open(t, p), FormatMP3, targets); err != nil {
		t.Fatal(err)
	}
	for i, tg := range targets {
		if got := readFile(t, filepath.Join(picDir, strconv.Itoa(tg.Index))); !bytes.Equal(got, datas[i]) || in.Pictures[i].SHA256 != sha(datas[i]) {
			t.Fatalf("picture %d: %q", i, got)
		}
	}
	cover := &coverFile{writeFile(t, filepath.Join(dir, "c.jpg"), jpegImage(t, dir, "c.jpg", "red")), FormatJPEG}
	_, after, out := writeMP3Checked(t, tools, p, TagValues{}, cover)
	if len(after.Pictures) != 1 || out.ape != nil || len(out.frames) != 1 {
		t.Fatalf("after the write: %+v", after.Pictures)
	}
	// An invalid APIC is a picture that a write removes anyway.
	q := mp3At(t, t.TempDir(), "bad.mp3", id3Tag(4, 0, 0, frame("APIC", []byte{0, 'x'})), src)
	if in := inspectMP3(t, tools, q); !reflect.DeepEqual(in.Opaque, []OpaqueField{{"id3v2:APIC", "invalid_picture", true}}) {
		t.Fatalf("opaque: %+v", in.Opaque)
	}
	writeMP3Checked(t, tools, q, TagValues{}, nil)
}

// A real tag writer's output: LAME's own ID3v2.3 and ID3v1 tags, with an
// embedded picture.
func TestTagsMP3LAMETags(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	jpg := filepath.Join(dir, "c.jpg")
	writeFile(t, jpg, jpegImage(t, dir, "c.jpg", "blue"))
	p := lame(t, wav16(t, dir, "src.wav", sine3s), dir, "lame.mp3", "-V2", "--add-id3v2", "--tt", "Lame title",
		"--ta", "Lame artist", "--tl", "Lame album", "--ty", "2001", "--tn", "3/12", "--tc", "lame comment", "--tg", "Rock", "--ti", jpg)
	in := inspectMP3(t, tools, p)
	want := map[string][]string{"title": {"Lame title"}, "artist": {"Lame artist"}, "album": {"Lame album"}, "date": {"2001"},
		"track": {"3"}, "track_total": {"12"}, "genre": {"Rock"}}
	if !reflect.DeepEqual(managedOf(in), want) || len(in.Conflicts) > 0 || len(in.Pictures) != 1 {
		t.Fatalf("LAME tags: %v %+v %d", managedOf(in), in.Conflicts, len(in.Pictures))
	}
	_, after, _ := writeMP3Checked(t, tools, p, fullValues, nil)
	if _, ok := unmanagedOf(after)["id3v2:COMM:XXX:legacy-id3v1"]; ok {
		t.Fatal("the ID3v1 comment LAME also wrote as COMM was duplicated")
	}
}

// padding follows the written size of the tag: the original room is kept
// when the frames fit (the audio does not move), else 1,024 bytes.
func TestTagsMP3Padding(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := mp3Source(t, dir)
	p := mp3At(t, dir, "p.mp3", id3Tag(4, 0, 4000, frame("TIT2", textBody(encLatin1, "x"))), src)
	_, _, out := writeMP3Checked(t, tools, p, TagValues{Title: "y"}, nil)
	if out.tagSize != 10+4000+10+2 {
		t.Fatalf("tag size %d: the room of the original tag was not kept", out.tagSize)
	}
	p = mp3At(t, t.TempDir(), "p.mp3", id3Tag(4, 0, 2<<20, frame("TIT2", textBody(encLatin1, "x"))), src)
	if _, _, out = writeMP3Checked(t, tools, p, TagValues{Title: "y"}, nil); len(out.padding) != 1024 {
		t.Fatalf("padding %d after a tag with 2 MiB of padding", len(out.padding))
	}
	p = mp3At(t, t.TempDir(), "p.mp3", id3Tag(4, 0, 0, frame("TIT2", textBody(encLatin1, "x"))), src)
	if _, _, out = writeMP3Checked(t, tools, p, fullValues, nil); len(out.padding) != 1024 {
		t.Fatalf("padding %d after a full tag", len(out.padding))
	}
}
