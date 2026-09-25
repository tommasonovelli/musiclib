package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// The M4A contract of the tag helper (DESIGN.md §8.1-§8.3, NOTES.md N-165
// to N-168): metadata built by the independent codec of mp4meta_test.go
// around AAC and ALAC from the pinned FFmpeg, read and written by the real
// helper, checked by §9.1 step 6 (digest, VerifyTags) and by a second,
// independent reading of the written file.

// m4aCodecs are the two codecs of §8.1 with their blobs.format.
var m4aCodecs = []struct{ codec, format string }{{"aac", FormatM4AAAC}, {"alac", FormatM4AALAC}}

// m4aSource is 3 s of pink noise encoded by FFmpeg in codec, with moov after
// the media data (FFmpeg's default) or before it (faststart), without any
// metadata (its udta removed).
func m4aSource(t testing.TB, dir, codec string, faststart bool) []byte {
	t.Helper()
	name := fmt.Sprintf("src-%s-%v", codec, faststart)
	args := []string{"-i", wav16(t, dir, name+".wav", noise3s), "-map_metadata", "-1", "-c:a", codec}
	if codec == "aac" {
		args = append(args, "-b:a", "128k")
	}
	if faststart {
		args = append(args, "-movflags", "+faststart")
	}
	return withoutUdta(t, readFile(t, gen(t, dir, name+".m4a", args...)))
}

func inspectM4A(t testing.TB, tools *Tools, path, format string) Inspection {
	t.Helper()
	in, err := tools.Inspect(t.Context(), open(t, path), format)
	if err != nil {
		t.Fatalf("Inspect(%s): %v", filepath.Base(path), err)
	}
	return in
}

// writeM4AChecked is §9.1 step 6 on an M4A (digest, inspection, write,
// inspection, VerifyTags, digest), plus the independent reading of the
// written file: the same top-level boxes, every one but moov byte for byte;
// the chunk offsets of the media after moov moved by exactly the change of
// its size; the same samples. It returns the inspections.
func writeM4AChecked(t testing.TB, tools *Tools, path, format string, v TagValues, cover *coverFile) (before, after Inspection) {
	t.Helper()
	orig := readFile(t, path)
	d0 := digest(t, tools, path)
	before = inspectM4A(t, tools, path, format)
	if err := tools.WriteManagedTags(t.Context(), openRW(t, path), format, v, cover.cover(t)); err != nil {
		t.Fatalf("WriteManagedTags(%s): %v", filepath.Base(path), err)
	}
	after = inspectM4A(t, tools, path, format)
	if err := VerifyTags(v, cover.expected(t), before, after); err != nil {
		t.Fatalf("VerifyTags(%s): %v\nbefore: %+v\nafter:  %+v", filepath.Base(path), err, before.Unmanaged, after.Unmanaged)
	}
	if err := audioChanged(t, tools, path, d0); err != nil {
		t.Fatal(err)
	}
	out := readFile(t, path)
	checkM4ALayout(t, orig, out)
	return before, after
}

// checkM4ALayout is the independent reading of a write of orig into out.
func checkM4ALayout(t testing.TB, orig, out []byte) {
	t.Helper()
	a, b := mp4Children(t, orig), mp4Children(t, out)
	if len(a) != len(b) {
		t.Fatalf("top-level boxes %v became %v", mp4TopTypes(t, orig), mp4TopTypes(t, out))
	}
	moovEnd, delta := 0, 0
	for i := range a {
		if a[i].typ != b[i].typ {
			t.Fatalf("top-level boxes %v became %v", mp4TopTypes(t, orig), mp4TopTypes(t, out))
		}
		if a[i].typ == "moov" {
			for _, n := range a[:i+1] {
				moovEnd += len(n.bytes())
			}
			delta = len(b[i].bytes()) - len(a[i].bytes())
			continue
		}
		if !bytes.Equal(a[i].payload, b[i].payload) {
			t.Fatalf("the top-level %q box changed", a[i].typ)
		}
	}
	oa, sa := mp4Chunks(t, orig)
	ob, sb := mp4Chunks(t, out)
	if !slices.Equal(sa, sb) || len(oa) != len(ob) {
		t.Fatal("the chunks changed")
	}
	for i := range oa {
		want := oa[i]
		if want >= uint64(moovEnd) {
			want = uint64(int64(want) + int64(delta))
		}
		if ob[i] != want {
			t.Fatalf("chunk %d at %d, want %d (moov grew by %d)", i, ob[i], want, delta)
		}
	}
	if !bytes.Equal(mp4Samples(t, orig), mp4Samples(t, out)) {
		t.Fatal("the samples changed")
	}
}

// m4aCopies writes b to n files in fresh directories.
func m4aAt(t testing.TB, b []byte) string {
	t.Helper()
	return writeFile(t, filepath.Join(t.TempDir(), "f.m4a"), b)
}

// fullItems are the items a write of fullValues with cover produces, in
// order (§8.2): UTF-8 text, trkn of 8 bytes, disk of 6, cpil of one byte.
func fullItems(cover []byte, coverType uint32) [][]byte {
	out := [][]byte{
		textItem(atomNam, "Title"), textItem(atomART, "Artist"), textItem("aART", "Album Artist"), textItem(atomAlb, "Album"),
		pairItem("trkn", 3, 12), pairItem("disk", 1, 2), textItem(atomDay, "1999"), textItem(atomGen, "Genre"), intItem("cpil", 1),
	}
	if cover != nil {
		out = append(out, mp4Box("covr", dataAtom(coverType, cover)))
	}
	return out
}

// dataValue is the canonical value of an unmanaged data atom that is not
// UTF-8 text with locale 0 (N-167).
func dataValue(typ, locale uint32, v []byte) string {
	return fmt.Sprintf("data type=%d locale=%08x sha256:%s", typ, locale, sha(v))
}

// ---------------------------------------------------------------------------

// No metadata at all: nothing managed, only the container's canonical
// forms; a write creates udta, meta and ilst with exactly the managed items
// and the cover; a second write gives the same bytes, and so do copies.
func TestTagsM4ANoTags(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	cover := pngCover(t, dir)
	for _, c := range m4aCodecs {
		for _, faststart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s faststart=%v", c.codec, faststart), func(t *testing.T) {
				src := m4aSource(t, dir, c.codec, faststart)
				p := m4aAt(t, src)
				in := inspectM4A(t, tools, p, c.format)
				if len(managedOf(in)) > 0 || len(in.Conflicts) > 0 || len(in.Pictures) > 0 || len(in.Opaque) > 0 {
					t.Fatalf("inspection of an untagged file: %+v", in)
				}
				u := unmanagedOf(in)
				if !reflect.DeepEqual(u["mp4.samples"], []string{"sha256:" + sha(mp4Samples(t, src))}) {
					t.Fatalf("mp4.samples %v is not the independent hash", u["mp4.samples"])
				}
				for k := range u {
					if !strings.HasPrefix(k, "mp4.") {
						t.Fatalf("unmanaged %q in an untagged file", k)
					}
				}
				offs, sizes := mp4Chunks(t, src)
				if in.Audio.Start != int64(offs[0]) || in.Audio.End != int64(offs[len(offs)-1]+sizes[len(sizes)-1]) {
					t.Fatalf("audio range %+v", in.Audio)
				}

				_, after := writeM4AChecked(t, tools, p, c.format, fullValues, cover)
				out := readFile(t, p)
				if got, want := ilstItems(t, out), fullItems(readFile(t, cover.path), 14); !reflect.DeepEqual(got, want) {
					t.Fatalf("items:\n got %q\nwant %q", got, want)
				}
				if len(after.Pictures) != 1 || after.Pictures[0].Location != "covr" || after.Pictures[0].MIME != "image/png" {
					t.Fatalf("pictures %+v", after.Pictures)
				}
				// Idempotent, and deterministic over copies.
				again := m4aAt(t, out)
				writeM4AChecked(t, tools, again, c.format, fullValues, cover)
				if !bytes.Equal(readFile(t, again), out) {
					t.Fatal("a second write changed the bytes")
				}
				for range 2 {
					q := m4aAt(t, src)
					if err := tools.WriteManagedTags(t.Context(), openRW(t, q), c.format, fullValues, cover.cover(t)); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(readFile(t, q), out) {
						t.Fatal("two writes of the same input differ")
					}
				}
				// Removing everything leaves an empty list.
				writeM4AChecked(t, tools, p, c.format, TagValues{}, nil)
				if items := ilstItems(t, readFile(t, p)); len(items) != 0 {
					t.Fatalf("items left: %q", items)
				}
			})
		}
	}
}

// utf16be encodes s as UTF-16BE (data type 2).
func utf16be(s string) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		out = binary.BigEndian.AppendUint16(out, u)
	}
	return out
}

// richItems are the items of a file tagged by several tools: managed atoms,
// aliases, sort atoms, a numeric genre, and every kind of unmanaged item;
// kept lists the unmanaged ones in order, and coverAt how many of them come
// before the first covr.
func richItems(t testing.TB, dir string) (items, kept [][]byte, coverAt int) {
	jpg := jpegImage(t, dir, "rich.jpg", "red")
	png := pngImage(t, 6, 6, 9)
	unmanaged := [][]byte{
		textItem("soco", "Composer, The"),
		textItem(atomCmt, "a comment"),
		textItem(atomWrt, "Composer"),
		textItem(atomLyr, "line 1\r\nline 2"),
		intItem("tmpo", 0, 120),
		intItem("pgap", 1),
		intItem("plID", 0, 0, 0, 0, 0, 0, 1, 2),
		textItem(atomToo, "an encoder"),
		iTunesText("iTunNORM", " 00000A2B 00000B3C"),
		iTunesText("replaygain_track_gain", "-6.00 dB"),
		iTunesText("MusicBrainz Track Id", "0f5a3a3e-2c7c-4a5c-9d3e-6b1f7e1b2a3c"),
		iTunesText("ARTISTS", "One", "Two"),
		freeformItem("org.example", "Thing", dataAtom(1, []byte("v"))),
		freeformItem("com.apple.iTunes", "Binary", dataAtom(0, []byte{1, 2, 3})),
		mp4Box("xid ", dataAtom(1, []byte("label:id"))),
		mp4Box("\xa9xyz", mp4Box("data", be32(1), be32(0x656e5553), []byte("with a locale"))),
		mp4Box("\xa9st3", dataAtom(2, utf16be("Été"))),
		mp4Box("\xa9bad", dataAtom(1, []byte{'a', 0xC3})),
		mp4Box("raw!", []byte("not boxes at all")),
		mp4Box64("\xa9big", dataAtom(1, []byte("a 64-bit item"))),
	}
	managed := [][]byte{
		textItem(atomNam, "Title one", "Title two"),
		textItem(atomART, "Artist"),
		textItem("aART", "Album Artist"),
		textItem(atomAlb, "Album"),
		pairItem("trkn", 3, 12),
		pairItem("disk", 1, 2),
		textItem(atomDay, "2001-05-06"),
		textItem(atomGen, "Jazz"),
		intItem("cpil", 1),
	}
	removed := [][]byte{
		iTunesText("ALBUM ARTIST", "Another Album Artist"),
		iTunesText("totaltracks", "12"),
		mp4Box("gnre", dataAtom(0, []byte{0, 9})), // ID3v1 genre 8, Jazz
		textItem("sonm", "Title, The"), textItem("soar", "Artist, The"), textItem("soaa", "Album Artist, The"),
		textItem("soal", "Album, The"), iTunesText("ALBUMSORT", "Album, The"),
	}
	covr := mp4Box("covr", dataAtom(13, jpg), dataAtom(14, png))
	// Interleaved: managed and removed atoms among the kept ones, the cover
	// after the tenth kept item.
	for i, u := range unmanaged {
		if i < len(managed) {
			items = append(items, managed[i])
		}
		if i < len(removed) {
			items = append(items, removed[i])
		}
		if i == 10 {
			items = append(items, covr)
		}
		items = append(items, u)
	}
	return items, unmanaged, 10
}

// A richly tagged file (§8.2, §8.3): the canonical atoms win, aliases that
// disagree are conflicts, every unmanaged item is read in its canonical form;
// a write keeps them byte for byte and in order, removes the aliases, sort
// atoms and numeric genre, and puts the cover where the first covr was.
func TestTagsM4ARichTags(t *testing.T) {
	dir := t.TempDir()
	items, kept, coverAt := richItems(t, dir)
	for name, tools := range helpers(t) {
		for _, c := range m4aCodecs {
			t.Run(name+"/"+c.codec, func(t *testing.T) {
				p := m4aAt(t, withIlst(t, m4aSource(t, dir, c.codec, true), 0, items...))
				in := inspectM4A(t, tools, p, c.format)
				want := map[string][]string{
					"title": {"Title one", "Title two"}, "artist": {"Artist"}, "album_artist": {"Album Artist"},
					"album": {"Album"}, "track": {"3"}, "track_total": {"12"}, "disc": {"1"}, "disc_total": {"2"},
					"date": {"2001-05-06"}, "genre": {"Jazz"}, "compilation": {"1"},
				}
				if !reflect.DeepEqual(managedOf(in), want) {
					t.Fatalf("managed %v", managedOf(in))
				}
				wantConflicts := []Conflict{{Field: "album_artist", Sources: []KeyValues{
					{"ilst:aART", []string{"Album Artist"}},
					{"ilst:----:com.apple.iTunes:ALBUM ARTIST", []string{"Another Album Artist"}},
				}}}
				if !reflect.DeepEqual(in.Conflicts, wantConflicts) || len(in.Opaque) > 0 || len(in.Pictures) != 2 {
					t.Fatalf("conflicts %+v, opaque %+v, pictures %d", in.Conflicts, in.Opaque, len(in.Pictures))
				}
				u := unmanagedOf(in)
				for key, values := range map[string][]string{
					"ilst:soco": {"Composer, The"},
					"ilst:©cmt": {"a comment"},
					"ilst:©lyr": {"line 1\r\nline 2"},
					"ilst:tmpo": {dataValue(21, 0, []byte{0, 120})},
					"ilst:plID": {dataValue(21, 0, []byte{0, 0, 0, 0, 0, 0, 1, 2})},
					"ilst:----:com.apple.iTunes:replaygain_track_gain": {"-6.00 dB"},
					"ilst:----:com.apple.iTunes:ARTISTS":               {"One", "Two"},
					"ilst:----:com.apple.iTunes:Binary":                {dataValue(0, 0, []byte{1, 2, 3})},
					"ilst:----:org.example:Thing":                      {"v"},
					"ilst:©xyz":                                        {dataValue(1, 0x656e5553, []byte("with a locale"))},
					"ilst:©st3":                                        {dataValue(2, 0, utf16be("Été"))},
					"ilst:©bad":                                        {dataValue(1, 0, []byte{'a', 0xC3})},
					"ilst:raw!":                                        {"raw sha256:" + sha([]byte("not boxes at all"))},
					"ilst:©big":                                        {"a 64-bit item"},
				} {
					if !reflect.DeepEqual(u[key], values) {
						t.Errorf("unmanaged %q = %q, want %q", key, u[key], values)
					}
				}
				for key := range u {
					if strings.Contains(key, "SORT") || strings.HasPrefix(key, "ilst:so") && key != "ilst:soco" ||
						strings.Contains(key, "ALBUM ARTIST") || strings.Contains(strings.ToUpper(key), "TOTALTRACKS") || key == "ilst:gnre" {
						t.Errorf("a managed alias or sort atom is unmanaged: %q", key)
					}
				}

				cover := pngCover(t, dir)
				writeM4AChecked(t, tools, p, c.format, fullValues, cover)
				got := ilstItems(t, readFile(t, p))
				want2 := fullItems(nil, 0)
				want2 = append(want2, kept[:coverAt]...)
				want2 = append(want2, mp4Box("covr", dataAtom(14, readFile(t, cover.path))))
				want2 = append(want2, kept[coverAt:]...)
				if !reflect.DeepEqual(got, want2) {
					t.Fatalf("items after the write:\n got %q\nwant %q", got, want2)
				}
			})
		}
	}
}

// §8.1 for M4A: the canonical atom wins over its aliases, which are read
// only when it is absent; every disagreeing source is a conflict.
func TestTagsM4AReadPrecedence(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := m4aSource(t, dir, "alac", false)
	for _, tc := range []struct {
		name      string
		items     [][]byte
		managed   map[string][]string
		conflicts []string
	}{
		{"aliases alone", [][]byte{iTunesText("album_artist", "AA"), iTunesText("TRACKTOTAL", "9"), iTunesText("TOTALDISCS", "3"),
			mp4Box("gnre", dataAtom(0, []byte{0, 18}))},
			map[string][]string{"album_artist": {"AA"}, "track_total": {"9"}, "disc_total": {"3"}, "genre": {"Rock"}}, nil},
		{"canonical wins", [][]byte{iTunesText("ALBUMARTIST", "B"), textItem("aART", "A"), pairItem("trkn", 2, 10),
			iTunesText("TRACKTOTAL", "11"), iTunesText("TOTALTRACKS", "10"), mp4Box("gnre", dataAtom(21, []byte{0, 18})), textItem(atomGen, "Jazz")},
			map[string][]string{"album_artist": {"A"}, "track": {"2"}, "track_total": {"10"}, "genre": {"Jazz"}},
			[]string{"album_artist", "track_total", "genre"}},
		{"agreeing aliases are no conflict", [][]byte{textItem(atomGen, "Rock"), mp4Box("gnre", dataAtom(0, []byte{0, 18})),
			pairItem("disk", 1, 2), iTunesText("DISCTOTAL", "2")},
			map[string][]string{"genre": {"Rock"}, "disc": {"1"}, "disc_total": {"2"}}, nil},
		{"a total without its number, a genre out of the list", [][]byte{pairItem("trkn", 0, 7), mp4Box("gnre", dataAtom(0, []byte{0, 200}))},
			map[string][]string{"track_total": {"7"}}, nil},
		{"several atoms of one name", [][]byte{textItem(atomART, "A"), textItem(atomART, "B", ""), intItem("cpil", 0)},
			map[string][]string{"artist": {"A", "B"}, "compilation": {"0"}}, nil},
		{"UTF-16", [][]byte{mp4Box(atomNam, dataAtom(2, utf16be("Ça 𝄞")))}, map[string][]string{"title": {"Ça 𝄞"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := inspectM4A(t, tools, m4aAt(t, withIlst(t, src, 0, tc.items...)), FormatM4AALAC)
			if !reflect.DeepEqual(managedOf(in), tc.managed) {
				t.Fatalf("managed %v, want %v", managedOf(in), tc.managed)
			}
			var fields []string
			for _, c := range in.Conflicts {
				fields = append(fields, c.Field)
			}
			if !slices.Equal(fields, tc.conflicts) || len(in.Opaque) > 0 {
				t.Fatalf("conflicts %+v, opaque %+v", in.Conflicts, in.Opaque)
			}
		})
	}
}

// Managed atoms the reader cannot read are removed opaque fields: never a
// source, never blocking, replaced by the write. The same content in an
// unmanaged item is kept as it is.
func TestTagsM4AUnreadableManagedAtoms(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := m4aSource(t, dir, "aac", true)
	items := [][]byte{
		mp4Box(atomNam, dataAtom(1, []byte{0xFF})),
		mp4Box(atomART, dataAtom(1, []byte("a\x00b"))),
		mp4Box(atomAlb, dataAtom(2, []byte{0xD8, 0x00})),
		mp4Box(atomDay, dataAtom(21, []byte{7})),
		mp4Box("trkn", dataAtom(0, []byte{0, 0, 0, 1})),
		mp4Box("cpil", []byte("junk")),
		mp4Box("gnre", dataAtom(0, []byte{1})),
		mp4Box("covr", mp4Box("name", []byte("x"))),
		mp4Box("aART", mp4Box("data", be32(1))),
		mp4Box(atomCmt, dataAtom(1, []byte{0xFF})),
	}
	p := m4aAt(t, withIlst(t, src, 0, items...))
	in := inspectM4A(t, tools, p, FormatM4AAAC)
	want := []OpaqueField{
		{"ilst:©nam", "invalid_utf8", true}, {"ilst:©ART", "nul_byte", true}, {"ilst:©alb", "invalid_text", true},
		{"ilst:©day", "unsupported_data", true}, {"ilst:trkn", "malformed_entry", true}, {"ilst:cpil", "malformed_entry", true},
		{"ilst:gnre", "malformed_entry", true}, {"ilst:covr", "invalid_picture", true}, {"ilst:aART", "malformed_entry", true},
	}
	if !reflect.DeepEqual(in.Opaque, want) || len(managedOf(in)) > 0 || len(in.Blocking()) > 0 {
		t.Fatalf("opaque %+v\nmanaged %v", in.Opaque, managedOf(in))
	}
	if !reflect.DeepEqual(unmanagedOf(in)["ilst:©cmt"], []string{dataValue(1, 0, []byte{0xFF})}) {
		t.Fatalf("the unmanaged item %v", unmanagedOf(in)["ilst:©cmt"])
	}
	writeM4AChecked(t, tools, p, FormatM4AAAC, fullValues, nil)
	got := ilstItems(t, readFile(t, p))
	if !reflect.DeepEqual(got, append(fullItems(nil, 0), items[len(items)-1])) {
		t.Fatalf("items %q", got)
	}
}

// Unicode is written byte for byte, NFD and astral characters included.
func TestTagsM4AUnicodeByteExact(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	p := m4aAt(t, m4aSource(t, dir, "alac", true))
	v := TagValues{Title: "Café 𝄞", Artist: "Björk", Album: "日本語", Genre: "(Rock)", Date: "13"}
	_, after := writeM4AChecked(t, tools, p, FormatM4AALAC, v, nil)
	if after.Managed.Title[0] != v.Title || after.Managed.Genre[0] != "(Rock)" {
		t.Fatalf("managed %+v", after.Managed)
	}
	if got := ilstItems(t, readFile(t, p))[0]; !bytes.Equal(got, textItem(atomNam, v.Title)) {
		t.Fatalf("title item %x", got)
	}
}

// Where the metadata goes and what moves (N-165): the padding next to the
// ilst keeps moov's size when the new list fits, so that nothing moves;
// otherwise the media after moov moves, and every chunk offset past it is
// fixed up, in stco and in co64. Every other box stays as it is, and a
// second write gives the same bytes.
func TestTagsM4ALayouts(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	fast := m4aSource(t, dir, "alac", true)
	slow := m4aSource(t, dir, "aac", false)
	bigCover := &coverFile{writeFile(t, filepath.Join(dir, "big.png"), append(pngImage(t, 4, 4, 2), make([]byte, 50000)...)), FormatPNG}
	huge := append(pngImage(t, 4, 4, 3), make([]byte, 2<<20)...)
	quickTimeMeta := mp4RewriteMoov(t, fast, func(moov []mp4Node) []mp4Node {
		return append(moov, mp4Node{typ: "udta", payload: mp4Box("meta", iTunesHdlr, mp4Box("ilst", textItem(atomNam, "qt")))})
	})
	extras := mp4RewriteMoov(t, fast, func(moov []mp4Node) []mp4Node {
		return append(moov, mp4Node{typ: "free", payload: make([]byte, 8)}, mp4Node{typ: "udta", payload: slices.Concat(
			mp4Box("chpl", []byte("chapters")),
			mp4Box("meta", be32(0), iTunesHdlr, mp4Box("free", make([]byte, 4)), mp4Box("xtra", []byte("kept")),
				mp4Box("ilst", textItem(atomNam, "x")), mp4Box("free", make([]byte, 100))),
			mp4Box("free", make([]byte, 16)))})
	})
	for _, tc := range []struct {
		name   string
		file   []byte
		format string
		v      TagValues
		cover  *coverFile
		moves  bool // the media after moov moves
	}{
		{"room kept", withIlst(t, fast, 3000, textItem(atomNam, "x")), FormatM4AALAC, fullValues, nil, false},
		{"exact room", withIlst(t, fast, 0, fullItems(nil, 0)...), FormatM4AALAC, fullValues, nil, false},
		{"grows before the media", withIlst(t, fast, 0), FormatM4AALAC, fullValues, bigCover, true},
		{"grows before the media, co64", toCo64(t, withIlst(t, fast, 0)), FormatM4AALAC, fullValues, bigCover, true},
		{"no udta before the media", fast, FormatM4AALAC, fullValues, nil, true},
		{"grows after the media", withIlst(t, slow, 0), FormatM4AAAC, fullValues, bigCover, false},
		{"shrinks past the padding bound", withIlst(t, fast, 0, mp4Box("covr", dataAtom(14, huge))), FormatM4AALAC, fullValues, nil, true},
		{"a QuickTime meta", quickTimeMeta, FormatM4AALAC, fullValues, nil, true},
		{"extra boxes and free boxes", extras, FormatM4AALAC, fullValues, bigCover, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := m4aAt(t, tc.file)
			o0, _ := mp4Chunks(t, tc.file)
			before, _ := writeM4AChecked(t, tools, p, tc.format, tc.v, tc.cover)
			out := readFile(t, p)
			o1, _ := mp4Chunks(t, out)
			if moved := o0[0] != o1[0]; moved != tc.moves {
				t.Fatalf("the media moved: %v, want %v", moved, tc.moves)
			}
			if tc.name == "room kept" || tc.name == "exact room" {
				if len(out) != len(tc.file) {
					t.Fatalf("the file size changed: %d -> %d", len(tc.file), len(out))
				}
			}
			if tc.name == "a QuickTime meta" {
				meta := mp4Find(t, mp4Children(t, out), "moov", "udta")
				if meta[0].typ != "meta" || mp4Prefix(meta[0]) != 0 {
					t.Fatal("the QuickTime meta became a full box")
				}
			}
			if tc.name == "extra boxes and free boxes" {
				u := unmanagedOf(before)
				if len(u["mp4.box:moov/udta/chpl"]) != 1 || len(u["mp4.box:moov/udta/meta/xtra"]) != 1 ||
					len(u["mp4.box:moov/free"]) != 1 || len(u["mp4.box:moov/udta/free"]) != 1 {
					t.Fatalf("unmanaged boxes %v", u)
				}
				var kids []string
				for _, n := range mp4Find(t, mp4Children(t, out), "moov", "udta", "meta") {
					kids = append(kids, n.typ)
				}
				if !slices.Equal(kids, []string{"hdlr", "xtra", "ilst", "free"}) {
					t.Fatalf("meta children %v", kids)
				}
			}
			if tc.name == "shrinks past the padding bound" {
				meta := mp4Find(t, mp4Children(t, out), "moov", "udta", "meta")
				if last := meta[len(meta)-1]; last.typ != "free" || len(last.bytes()) != 1024 {
					t.Fatalf("the padding is %q of %d bytes", last.typ, len(last.bytes()))
				}
			}
			again := m4aAt(t, out)
			writeM4AChecked(t, tools, again, tc.format, tc.v, tc.cover)
			if !bytes.Equal(readFile(t, again), out) {
				t.Fatal("a second write changed the bytes")
			}
		})
	}
}

// Pictures: every image of covr is a front cover, listed with the MIME type
// of its data type, extracted byte for byte; a write replaces them with the
// one cover, or removes them all.
func TestTagsM4APictures(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	jpg := jpegImage(t, dir, "p.jpg", "green")
	png := pngImage(t, 5, 5, 4)
	bmp := []byte("BM fake bitmap")
	src := withIlst(t, m4aSource(t, dir, "aac", false), 0,
		mp4Box("covr", dataAtom(13, jpg), dataAtom(14, png), dataAtom(0, jpg), dataAtom(27, bmp)), textItem(atomNam, "t"))
	p := m4aAt(t, src)
	in := inspectM4A(t, tools, p, FormatM4AAAC)
	wantMIME := []string{"image/jpeg", "image/png", "", "image/bmp"}
	data := [][]byte{jpg, png, jpg, bmp}
	if len(in.Pictures) != 4 {
		t.Fatalf("pictures %+v", in.Pictures)
	}
	var targets []ImageTarget
	for i, pic := range in.Pictures {
		if pic.Location != "covr" || pic.Type != PictureFrontCover || pic.MIME != wantMIME[i] || pic.SHA256 != sha(data[i]) {
			t.Fatalf("picture %d: %+v", i, pic)
		}
		targets = append(targets, ImageTarget{Index: i, Dst: createEmpty(t, dir, fmt.Sprintf("x%d", i))})
	}
	if _, err := tools.ExtractImages(t.Context(), open(t, p), FormatM4AAAC, targets); err != nil {
		t.Fatal(err)
	}
	for i := range data {
		if !bytes.Equal(readFile(t, filepath.Join(dir, fmt.Sprintf("x%d", i))), data[i]) {
			t.Fatalf("picture %d is not extracted byte for byte", i)
		}
	}
	jc := &coverFile{writeFile(t, filepath.Join(dir, "c.jpg"), jpg), FormatJPEG}
	_, after := writeM4AChecked(t, tools, p, FormatM4AAAC, fullValues, jc)
	if len(after.Pictures) != 1 || after.Pictures[0].MIME != "image/jpeg" {
		t.Fatalf("pictures %+v", after.Pictures)
	}
	if items := ilstItems(t, readFile(t, p)); !bytes.Equal(items[len(fullItems(nil, 0))], mp4Box("covr", dataAtom(13, jpg))) {
		t.Fatal("the cover is not where the first covr was")
	}
	writeM4AChecked(t, tools, p, FormatM4AAAC, fullValues, nil)
}

// The formats of a write request that an M4A cannot hold are refused before
// anything is written: numbers beyond what TagLib reads, a cover of another
// type.
func TestTagsM4AInvalidRequests(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	p := m4aAt(t, m4aSource(t, dir, "alac", true))
	wantUnchanged(t, p, func() error {
		return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatM4AALAC, TagValues{Track: 32768}, nil)
	}, CodeTagsInvalidRequest)
	wantUnchanged(t, p, func() error {
		return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatM4AALAC, TagValues{DiscTotal: 40000}, nil)
	}, CodeTagsInvalidRequest)
	gif := writeFile(t, filepath.Join(dir, "c.gif"), []byte("GIF89a"))
	wantUnchanged(t, p, func() error {
		return rawTags(t.Context(), tools, []string{"write-managed-tags"}, []byte(`{"format":"m4a-alac","tags":{"title":null,`+
			`"artist":null,"album_artist":null,"album":null,"track":null,"track_total":null,"disc":null,"disc_total":null,`+
			`"date":null,"genre":null,"compilation":false},"cover":{"mime":"image/gif","width":1,"height":1,"depth":8,"colors":0}}`),
			openRW(t, p), open(t, gif))
	}, CodeTagsInvalidRequest)
	// The largest number TagLib reads as a positive one is written.
	writeM4AChecked(t, tools, p, FormatM4AALAC, TagValues{Track: 32767, TrackTotal: 32767}, nil)
	if got := ilstItems(t, readFile(t, p)); !reflect.DeepEqual(got, [][]byte{pairItem("trkn", 32767, 32767)}) {
		t.Fatalf("items %x", got)
	}
}

// Every M4A picture fits under the helper's bound: a cover beyond it is
// refused before it is read; a cover larger than any FLAC can hold is
// embedded (N-166).
func TestMaxEmbeddedCoverM4A(t *testing.T) {
	for _, audio := range []string{FormatM4AAAC, FormatM4AALAC} {
		for _, format := range []string{FormatJPEG, FormatPNG} {
			if limit, ok := MaxEmbeddedCover(audio, format); !ok || limit != 1<<28-24 {
				t.Fatalf("MaxEmbeddedCover(%s, %s) = %d, %v", audio, format, limit, ok)
			}
		}
		wantCode(t, EmbeddedCoverFits(audio, FormatPNG, 1<<28-23), CodeTagsTooLarge)
		if _, ok := MaxEmbeddedCover(audio, "gif"); ok {
			t.Fatal("a GIF cover has a limit")
		}
	}
	dir := t.TempDir()
	src := m4aSource(t, dir, "alac", true)
	over := writeFile(t, filepath.Join(dir, "over.png"), pngImage(t, 4, 4, 1))
	if err := os.Truncate(over, 1<<28-23); err != nil {
		t.Fatal(err)
	}
	for name, tools := range helpers(t) {
		t.Run(name, func(t *testing.T) {
			p := m4aAt(t, src)
			wantUnchanged(t, p, func() error {
				return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatM4AALAC, fullValues, &Cover{File: open(t, over), Format: FormatPNG})
			}, CodeTagsTooLarge)
		})
	}
	big := &coverFile{writeFile(t, filepath.Join(dir, "big.png"), append(pngImage(t, 4, 4, 1), make([]byte, 17<<20)...)), FormatPNG}
	writeM4AChecked(t, newTools(t), m4aAt(t, src), FormatM4AALAC, fullValues, big)
}
