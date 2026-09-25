package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"musiclib/internal/fsops"
)

// The TagLib helper runs for real (DESIGN.md §12.1): the pinned static
// binary, and for the hostile inputs also its ASan/UBSan build. Fixtures are
// generated at test time: audio from the pinned ffmpeg, metadata written by
// the independent codec of flacmeta_test.go (never by the helper itself,
// except where a test is about a second write).

// tagsASanPath is the sanitized build of the helper, in the toolchain
// images only (Dockerfile, stage build-tags).
const tagsASanPath = "/usr/local/bin/musiclib-tags-asan"

// helpers returns Tools on the release helper and on its sanitized build.
func helpers(t testing.TB) map[string]*Tools {
	t.Helper()
	asan, err := NewTools(t.Context(), NewRunner(4), FFmpegPath, FFprobePath, tagsASanPath)
	if err != nil {
		t.Fatalf("NewTools with the sanitized helper: %v", err)
	}
	return map[string]*Tools{"release": newTools(t), "asan": asan}
}

// openRW opens a staging file read-write through internal/fsops, as the
// renderer will.
func openRW(t testing.TB, path string) *os.File {
	t.Helper()
	root, err := fsops.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	f, err := root.OpenFile(filepath.Base(path), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Base(path), err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

// createEmpty creates an empty file for ExtractImages through internal/fsops.
func createEmpty(t testing.TB, dir, name string) *os.File {
	t.Helper()
	root, err := fsops.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	f, err := root.CreateExclusive(name, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func inspectFile(t testing.TB, tools *Tools, path string) Inspection {
	t.Helper()
	in, err := tools.Inspect(t.Context(), open(t, path), FormatFLAC)
	if err != nil {
		t.Fatalf("Inspect(%s): %v", filepath.Base(path), err)
	}
	return in
}

// coverFile is an image file to embed and what VerifyTags expects of it.
type coverFile struct {
	path   string
	format string
}

func (c *coverFile) cover(t testing.TB) *Cover {
	if c == nil {
		return nil
	}
	return &Cover{File: open(t, c.path), Format: c.format}
}

func (c *coverFile) expected(t testing.TB) *ExpectedCover {
	if c == nil {
		return nil
	}
	b := readFile(t, c.path)
	sum := sha256.Sum256(b)
	return &ExpectedCover{MIME: "image/" + c.format, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}
}

// writeChecked is the sequence of §9.1 step 6 on a staging file: audio
// digest, inspection, write, inspection, VerifyTags, audio digest. The
// samples must be the same before and after (§12.2: "Tag writer altera i
// campioni"). It returns the inspection after the write.
func writeChecked(t testing.TB, tools *Tools, path string, v TagValues, cover *coverFile) Inspection {
	t.Helper()
	d0 := digest(t, tools, path)
	after := writeVerified(t, tools, path, v, cover)
	if err := audioChanged(t, tools, path, d0); err != nil {
		t.Fatal(err)
	}
	return after
}

// audioChanged is the audio check of §9.1 step 6: the digest of the file
// after the write must equal d0, the one before.
func audioChanged(t testing.TB, tools *Tools, path string, d0 Digest) error {
	t.Helper()
	if d1 := digest(t, tools, path); d1 != d0 {
		return errors.New("the write changed the audio of " + filepath.Base(path) + ": " +
			d0.PCMSHA256 + " -> " + d1.PCMSHA256)
	}
	return nil
}

// writeVerified is writeChecked without the audio digest, for the files the
// pinned ffmpeg refuses to open; it compares the audio frames byte for byte
// instead.
func writeVerified(t testing.TB, tools *Tools, path string, v TagValues, cover *coverFile) Inspection {
	t.Helper()
	audio := parseFLAC(t, readFile(t, path)).audio
	before := inspectFile(t, tools, path)
	if err := tools.WriteManagedTags(t.Context(), openRW(t, path), FormatFLAC, v, cover.cover(t)); err != nil {
		t.Fatalf("WriteManagedTags(%s): %v", filepath.Base(path), err)
	}
	after := inspectFile(t, tools, path)
	if err := VerifyTags(v, cover.expected(t), before, after); err != nil {
		t.Fatalf("VerifyTags(%s): %v\nbefore: %+v\nafter:  %+v", filepath.Base(path), err, before, after)
	}
	if !bytes.Equal(parseFLAC(t, readFile(t, path)).audio, audio) {
		t.Fatalf("the write changed the audio frames of %s", filepath.Base(path))
	}
	return after
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fullValues sets every managed field.
var fullValues = TagValues{
	Title: "Title", Artist: "Artist", AlbumArtist: "Album Artist", Album: "Album",
	Track: 3, TrackTotal: 12, Disc: 1, DiscTotal: 2, Date: "1999", Genre: "Genre", Compilation: true,
}

// ---------------------------------------------------------------------------

// A file with no tag at all: nothing managed, no picture, nothing opaque;
// the only unmanaged field is the list of kept blocks. A write creates the
// Vorbis comment block with exactly the managed values.
func TestTagsFLACNoTags(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	base := baseFLAC(t, dir, "src.flac", sine3s)
	p := writeFLAC(t, dir, "none.flac", base)

	in := inspectFile(t, tools, p)
	if !reflect.DeepEqual(expectedManaged(TagValues{}), normalized(in.Managed)) || len(in.Conflicts) > 0 ||
		len(in.Pictures) > 0 || len(in.Opaque) > 0 {
		t.Fatalf("inspection of an untagged file: %+v", in)
	}
	if len(in.Unmanaged) != 1 || in.Unmanaged[0].Key != "flac.blocks" ||
		in.Unmanaged[0].Values[0] != "0:"+sha(base.blocks[0].data) {
		t.Fatalf("unmanaged of an untagged file: %+v", in.Unmanaged)
	}

	// "Spring 1999" is not a number: TagLib's Tag::duplicate, which runs
	// when the file had no comment block, would delete it (N-085).
	v := fullValues
	v.Date = "Spring 1999"
	writeChecked(t, tools, p, v, nil)
	c := onlyComment(t, parseFLAC(t, readFile(t, p)))
	want := []string{"ALBUM=Album", "ALBUMARTIST=Album Artist", "ARTIST=Artist", "COMPILATION=1", "DATE=Spring 1999",
		"DISCNUMBER=1", "DISCTOTAL=2", "GENRE=Genre", "TITLE=Title", "TRACKNUMBER=3", "TRACKTOTAL=12"}
	if !slices.Equal(c.entries, want) {
		t.Fatalf("written comment:\n got %q\nwant %q", c.entries, want)
	}
}

// normalized turns empty lists into nil, for comparisons.
func normalized(m ManagedTags) ManagedTags {
	for _, f := range m.fields() {
		if len(*f.v) == 0 {
			*f.v = nil
		}
	}
	return m
}

// Every managed field under its canonical key, multi-valued ARTIST and
// GENRE, keys in any case: the values come back as ordered lists, and the Go
// side joins them with "; " (§7.3).
func TestTagsFLACManagedFields(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	f := baseFLAC(t, dir, "src.flac", sine3s).with(vorbisBlock("vendor",
		"TITLE=Song", "ARTIST=First", "album=Album", "Artist=Second", "ALBUMARTIST=Band", "TRACKNUMBER=03",
		"TRACKTOTAL=12", "DISCNUMBER=1", "DISCTOTAL=2", "DATE=1999-05-01", "GENRE=Rock", "genre=Pop", "COMPILATION=1"))
	in := inspectFile(t, tools, writeFLAC(t, dir, "all.flac", f))
	want := ManagedTags{
		Title: []string{"Song"}, Artist: []string{"First", "Second"}, AlbumArtist: []string{"Band"},
		Album: []string{"Album"}, Track: []string{"03"}, TrackTotal: []string{"12"}, Disc: []string{"1"},
		DiscTotal: []string{"2"}, Date: []string{"1999-05-01"}, Genre: []string{"Rock", "Pop"}, Compilation: []string{"1"},
	}
	if got := normalized(in.Managed); !reflect.DeepEqual(got, want) {
		t.Fatalf("managed:\n got %+v\nwant %+v", got, want)
	}
	if len(in.Conflicts) > 0 {
		t.Fatalf("conflicts without aliases: %+v", in.Conflicts)
	}
	if JoinValues(in.Managed.Artist) != "First; Second" || JoinValues(in.Managed.Genre) != "Rock; Pop" {
		t.Fatalf("joined: %q %q", JoinValues(in.Managed.Artist), JoinValues(in.Managed.Genre))
	}
	if n, ok := TagNumber(in.Managed.Track); !ok || n != 3 {
		t.Fatalf("TagNumber(%q) = %d, %v", in.Managed.Track, n, ok)
	}
	if c, ok := TagBool(in.Managed.Compilation); !ok || !c {
		t.Fatal("compilation not true")
	}
}

// §8.1: the canonical key wins over its aliases, which are only a fallback,
// and a disagreement is reported. §8.2: a write removes every alias.
func TestTagsFLACAliases(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	base := baseFLAC(t, dir, "src.flac", sine3s)

	t.Run("aliases only", func(t *testing.T) {
		p := writeFLAC(t, dir, "aliases.flac", base.with(vorbisBlock("v",
			"ALBUM ARTIST=Alias", "ALBUM_ARTIST=Other", "TRACKNUM=4/10", "TOTALDISCS=3", "DISCNUMBER=2/5", "YEAR=1987")))
		in := inspectFile(t, tools, p)
		want := ManagedTags{AlbumArtist: []string{"Alias"}, Track: []string{"4"}, TrackTotal: []string{"10"},
			Disc: []string{"2"}, DiscTotal: []string{"3"}, Date: []string{"1987"}}
		if got := normalized(in.Managed); !reflect.DeepEqual(got, want) {
			t.Fatalf("managed:\n got %+v\nwant %+v", got, want)
		}
		wantConflicts := []Conflict{
			{Field: "album_artist", Sources: []KeyValues{{"ALBUM ARTIST", []string{"Alias"}}, {"ALBUM_ARTIST", []string{"Other"}}}},
			{Field: "disc_total", Sources: []KeyValues{{"TOTALDISCS", []string{"3"}}, {"DISCNUMBER/total", []string{"5"}}}},
		}
		if !reflect.DeepEqual(in.Conflicts, wantConflicts) {
			t.Fatalf("conflicts:\n got %+v\nwant %+v", in.Conflicts, wantConflicts)
		}
		writeChecked(t, tools, p, fullValues, nil)
		keys := onlyComment(t, parseFLAC(t, readFile(t, p))).keys()
		for _, alias := range []string{"ALBUM ARTIST", "ALBUM_ARTIST", "TRACKNUM", "TOTALTRACKS", "TOTALDISCS", "YEAR"} {
			if slices.Contains(keys, alias) {
				t.Fatalf("alias %s survived the write: %q", alias, keys)
			}
		}
	})

	t.Run("canonical wins", func(t *testing.T) {
		p := writeFLAC(t, dir, "canonical.flac", base.with(vorbisBlock("v",
			"ALBUM ARTIST=Alias", "ALBUMARTIST=Canonical", "TOTALTRACKS=9", "TRACKTOTAL=12", "TRACKNUMBER=5/11",
			"YEAR=1987", "DATE=1988", "TOTALDISCS=2", "DISCTOTAL=2")))
		in := inspectFile(t, tools, p)
		if !slices.Equal(in.Managed.AlbumArtist, []string{"Canonical"}) || !slices.Equal(in.Managed.TrackTotal, []string{"12"}) ||
			!slices.Equal(in.Managed.Date, []string{"1988"}) || !slices.Equal(in.Managed.Track, []string{"5"}) {
			t.Fatalf("managed: %+v", in.Managed)
		}
		want := []Conflict{
			{Field: "album_artist", Sources: []KeyValues{{"ALBUMARTIST", []string{"Canonical"}}, {"ALBUM ARTIST", []string{"Alias"}}}},
			{Field: "track_total", Sources: []KeyValues{{"TRACKTOTAL", []string{"12"}}, {"TOTALTRACKS", []string{"9"}},
				{"TRACKNUMBER/total", []string{"11"}}}},
			{Field: "date", Sources: []KeyValues{{"DATE", []string{"1988"}}, {"YEAR", []string{"1987"}}}},
		}
		if !reflect.DeepEqual(in.Conflicts, want) {
			t.Fatalf("conflicts (equal sources, like the two disc totals, are not one):\n got %+v\nwant %+v", in.Conflicts, want)
		}
		// A write that sets nothing still removes every alias.
		writeChecked(t, tools, p, TagValues{}, nil)
		if keys := onlyComment(t, parseFLAC(t, readFile(t, p))).keys(); len(keys) > 0 {
			t.Fatalf("keys left: %q", keys)
		}
	})
}

// §8.2: the sort keys of the four managed names are removed; other sort
// keys are unmanaged and stay.
func TestTagsFLACSortFieldsRemoved(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	p := writeFLAC(t, dir, "sort.flac", baseFLAC(t, dir, "src.flac", sine3s).with(vorbisBlock("v",
		"TITLESORT=Title, The", "artistsort=Artist, The", "ALBUMARTISTSORT=Band, The", "ALBUMSORT=Album, The",
		"COMPOSERSORT=Bach, Johann Sebastian")))
	before := inspectFile(t, tools, p)
	// flac.blocks and vorbis.vendor sort first.
	if !reflect.DeepEqual(before.Unmanaged[2:], []KeyValues{{"vorbis:COMPOSERSORT", []string{"Bach, Johann Sebastian"}}}) {
		t.Fatalf("unmanaged: %+v", before.Unmanaged)
	}
	writeChecked(t, tools, p, fullValues, nil)
	keys := onlyComment(t, parseFLAC(t, readFile(t, p))).keys()
	for _, k := range []string{"TITLESORT", "ARTISTSORT", "ALBUMARTISTSORT", "ALBUMSORT"} {
		if slices.Contains(keys, k) {
			t.Fatalf("sort key %s survived: %q", k, keys)
		}
	}
	if !slices.Contains(keys, "COMPOSERSORT") {
		t.Fatalf("COMPOSERSORT was removed: %q", keys)
	}
}

// Text is returned and written byte for byte: no normalization, NFD stays
// NFD, and characters outside the BMP survive TagLib's UTF-16 strings.
func TestTagsFLACUnicodeByteExact(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	nfd := "Cafe\u0301 d\u0065\u0301ja\u0300"
	astral := "𝄞 Ωmega 日本語 🎵"
	p := writeFLAC(t, dir, "unicode.flac", baseFLAC(t, dir, "src.flac", sine3s).with(vorbisBlock("vendor ✓",
		"TITLE="+nfd, "ARTIST="+astral, "COMMENT="+nfd+"\n"+astral)))
	in := inspectFile(t, tools, p)
	if !slices.Equal(in.Managed.Title, []string{nfd}) || !slices.Equal(in.Managed.Artist, []string{astral}) {
		t.Fatalf("managed: %q %q", in.Managed.Title, in.Managed.Artist)
	}
	v := TagValues{Title: astral, Artist: nfd, Album: "\u00e9\u0301\U0010FFFD"}
	writeChecked(t, tools, p, v, nil)
	c := onlyComment(t, parseFLAC(t, readFile(t, p)))
	if !slices.Equal(c.values("TITLE"), []string{astral}) || !slices.Equal(c.values("ARTIST"), []string{nfd}) ||
		!slices.Equal(c.values("ALBUM"), []string{v.Album}) || !slices.Equal(c.values("COMMENT"), []string{nfd + "\n" + astral}) ||
		c.vendor != "vendor ✓" {
		t.Fatalf("written bytes differ: %q", c.entries)
	}
}

// §8.3: unmanaged fields stay (ReplayGain, composer, comments, identifiers,
// custom keys, embedded lyrics) and so do the metadata blocks that are not
// tags; the canonical form compares them semantically.
func TestTagsFLACUnmanagedPreserved(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	app := flacBlock{typ: flacApplication, data: append([]byte("TEST"), garbage(3, 300)...)}
	riff := flacBlock{typ: flacApplication, data: append([]byte("riffLIST"), garbage(4, 64)...)}
	lyrics := "[00:01.00]First line\n[00:02.50]Second line\r\nThird line"
	entries := []string{
		"TITLE=Old title", "ARTIST=Old artist", "ALBUMARTISTSORT=x",
		"REPLAYGAIN_TRACK_GAIN=-6.50 dB", "REPLAYGAIN_TRACK_PEAK=0.988", "REPLAYGAIN_ALBUM_GAIN=-7.01 dB",
		"COMPOSER=J. S. Bach", "COMMENT=first", "comment=second", "DESCRIPTION=A description",
		"MUSICBRAINZ_TRACKID=5e7e5e5e-0000-4000-8000-000000000001", "ISRC=USRC17607839",
		"MY CUSTOM KEY=custom = value", "LYRICS=" + lyrics, "UNSYNCEDLYRICS=" + lyrics, "EMPTY=",
		"ORIGINALDATE=1970", "ENCODER=Lavf",
	}
	f := baseFLAC(t, dir, "src.flac", noise3s)
	f = f.with(vorbisBlock("reference libFLAC 1.4.3 20230623", entries...), app, riff)
	p := writeFLAC(t, dir, "unmanaged.flac", f)

	before := inspectFile(t, tools, p)
	wantKeys := []string{"flac.blocks", "vorbis.vendor", "vorbis:COMMENT", "vorbis:COMPOSER", "vorbis:DESCRIPTION",
		"vorbis:ENCODER", "vorbis:ISRC", "vorbis:LYRICS", "vorbis:MUSICBRAINZ_TRACKID", "vorbis:MY CUSTOM KEY",
		"vorbis:ORIGINALDATE", "vorbis:REPLAYGAIN_ALBUM_GAIN", "vorbis:REPLAYGAIN_TRACK_GAIN",
		"vorbis:REPLAYGAIN_TRACK_PEAK", "vorbis:UNSYNCEDLYRICS"}
	var keys []string
	for _, kv := range before.Unmanaged {
		keys = append(keys, kv.Key)
	}
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("unmanaged keys:\n got %q\nwant %q", keys, wantKeys)
	}
	if blocks := before.Unmanaged[0].Values; len(blocks) != len(f.blocks)-1 ||
		blocks[len(blocks)-2] != "2:"+sha(app.data) || blocks[len(blocks)-1] != "2:"+sha(riff.data) {
		t.Fatalf("kept blocks: %q", blocks)
	}

	after := writeChecked(t, tools, p, fullValues, nil)
	if !reflect.DeepEqual(before.Unmanaged, after.Unmanaged) {
		t.Fatalf("unmanaged changed:\nbefore %+v\nafter  %+v", before.Unmanaged, after.Unmanaged)
	}
	out := parseFLAC(t, readFile(t, p))
	c := onlyComment(t, out)
	if !slices.Equal(c.values("COMMENT"), []string{"first", "second"}) || !slices.Equal(c.values("LYRICS"), []string{lyrics}) ||
		!slices.Equal(c.values("MY CUSTOM KEY"), []string{"custom = value"}) {
		t.Fatalf("written comment: %q", c.entries)
	}
	apps := out.ofType(flacApplication)
	if len(apps) != 2 || !bytes.Equal(apps[0].data, app.data) || !bytes.Equal(apps[1].data, riff.data) {
		t.Fatal("the APPLICATION blocks were not kept byte for byte")
	}
	if !bytes.Equal(out.audio, f.audio) || !bytes.Equal(out.blocks[0].data, f.blocks[0].data) {
		t.Fatal("the audio frames or STREAMINFO changed")
	}
}

// Several pictures of different types, in a PICTURE block or in the
// comment: all listed with their identity, extracted byte for byte, and
// replaced by exactly one front cover (JPEG or PNG) or removed completely
// (§8.2: "Una cover assente nel DB significa nessuna immagine incorporata").
func TestTagsFLACPictures(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	jpg := jpegImage(t, dir, "front.jpg", "red")
	png1, png2 := pngImage(t, 16, 16, 1), pngImage(t, 8, 4, 2)
	front := flacPictureData{typ: 3, mime: "image/jpeg", desc: "front", width: 24, height: 16, depth: 24, data: jpg}
	back := flacPictureData{typ: 4, mime: "image/png", width: 16, height: 16, depth: 32, data: png1}
	other := flacPictureData{typ: 0, mime: "image/png", desc: "scan", data: png2}
	inComment := flacPictureData{typ: 3, mime: "image/png", data: png2}
	legacy := jpegImage(t, dir, "legacy.jpg", "blue")
	f := baseFLAC(t, dir, "src.flac", sine3s).with(
		vorbisBlock("v", "TITLE=x", inComment.entry(), "COVERART="+base64.StdEncoding.EncodeToString(legacy),
			"COVERARTMIME=image/jpeg"),
		front.block(), back.block(), other.block())
	p := writeFLAC(t, dir, "pictures.flac", f)

	in := inspectFile(t, tools, p)
	want := []Picture{
		{Index: 0, Location: "comment", Type: 3, MIME: "image/png", Size: int64(len(png2)), SHA256: sha(png2)},
		{Index: 1, Location: "comment", Type: 0, MIME: "", Size: int64(len(legacy)), SHA256: sha(legacy)},
		{Index: 2, Location: "block", Type: 3, MIME: "image/jpeg", Width: 24, Height: 16, Depth: 24, Size: int64(len(jpg)), SHA256: sha(jpg)},
		{Index: 3, Location: "block", Type: 4, MIME: "image/png", Width: 16, Height: 16, Depth: 32, Size: int64(len(png1)), SHA256: sha(png1)},
		{Index: 4, Location: "block", Type: 0, MIME: "image/png", Size: int64(len(png2)), SHA256: sha(png2)},
	}
	if !reflect.DeepEqual(in.Pictures, want) {
		t.Fatalf("pictures:\n got %+v\nwant %+v", in.Pictures, want)
	}
	for _, kv := range in.Unmanaged {
		if strings.Contains(kv.Key, "COVERART") || strings.Contains(kv.Key, "PICTURE") {
			t.Fatalf("a picture key is reported as unmanaged: %s", kv.Key)
		}
	}

	t.Run("extract", func(t *testing.T) {
		out := t.TempDir()
		targets := []ImageTarget{{4, createEmpty(t, out, "a")}, {0, createEmpty(t, out, "b")}, {2, createEmpty(t, out, "c")}, {1, createEmpty(t, out, "d")}}
		got, err := tools.ExtractImages(t.Context(), open(t, p), FormatFLAC, targets)
		if err != nil {
			t.Fatal(err)
		}
		for i, content := range [][]byte{png2, png2, jpg, legacy} {
			if b := readFile(t, filepath.Join(out, string(rune('a'+i)))); !bytes.Equal(b, content) {
				t.Fatalf("image %d: %d bytes, not the embedded ones", i, len(b))
			}
			if got[i].Index != targets[i].Index || got[i].SHA256 != sha(content) || got[i].Size != int64(len(content)) {
				t.Fatalf("result %d: %+v", i, got[i])
			}
		}
	})

	for _, tc := range []struct {
		name  string
		cover *coverFile
		pic   *flacPictureData
	}{
		{"png cover", &coverFile{writeFile(t, filepath.Join(dir, "cover.png"), png1), FormatPNG},
			&flacPictureData{typ: 3, mime: "image/png", width: 16, height: 16, depth: 32, data: png1}},
		{"jpeg cover", &coverFile{writeFile(t, filepath.Join(dir, "cover.jpg"), jpg), FormatJPEG},
			&flacPictureData{typ: 3, mime: "image/jpeg", width: 24, height: 16, depth: 24, data: jpg}},
		{"no cover", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := writeFLAC(t, t.TempDir(), "p.flac", f)
			after := writeChecked(t, tools, q, fullValues, tc.cover)
			out := parseFLAC(t, readFile(t, q))
			pics := out.ofType(flacPicture)
			keys := onlyComment(t, out).keys()
			for _, k := range []string{"METADATA_BLOCK_PICTURE", "COVERART", "COVERARTMIME"} {
				if slices.Contains(keys, k) {
					t.Fatalf("picture key %s survived: %q", k, keys)
				}
			}
			if tc.pic == nil {
				if len(pics) != 0 || len(after.Pictures) != 0 {
					t.Fatalf("%d picture blocks left", len(pics))
				}
				return
			}
			if len(pics) != 1 {
				t.Fatalf("%d picture blocks, want exactly one", len(pics))
			}
			if got := parsePictureBlock(t, pics[0].data); !reflect.DeepEqual(got, *tc.pic) {
				t.Fatalf("picture:\n got %+v\nwant %+v", got, *tc.pic)
			}
		})
	}
}

// A write that only removes: every managed field absent, no cover. What is
// left is exactly the unmanaged fields.
func TestTagsFLACWriteOnlyRemoves(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	cover := flacPictureData{typ: 3, mime: "image/png", data: pngImage(t, 4, 4, 9)}
	p := writeFLAC(t, dir, "full.flac", baseFLAC(t, dir, "src.flac", sine3s).with(vorbisBlock("v",
		"TITLE=T", "ARTIST=A", "ARTIST=B", "ALBUMARTIST=AA", "ALBUM ARTIST=AA2", "ALBUM=Al", "TRACKNUMBER=1/2",
		"TRACKTOTAL=2", "TOTALTRACKS=2", "DISCNUMBER=1", "DISCTOTAL=1", "TOTALDISCS=1", "DATE=2001", "YEAR=2001",
		"GENRE=G", "COMPILATION=1", "TITLESORT=T", "ARTISTSORT=A", "ALBUMSORT=Al", "ALBUMARTISTSORT=AA", "COMPOSER=C"),
		cover.block()))
	after := writeChecked(t, tools, p, TagValues{}, nil)
	if got := normalized(after.Managed); !reflect.DeepEqual(got, ManagedTags{}) || len(after.Pictures) > 0 {
		t.Fatalf("after: %+v", after)
	}
	c := onlyComment(t, parseFLAC(t, readFile(t, p)))
	if !slices.Equal(c.entries, []string{"COMPOSER=C"}) {
		t.Fatalf("comment left: %q", c.entries)
	}
}

// Writing the same values twice gives the same decoded result, and here
// the same bytes: the second write fits in the padding of the first.
func TestTagsFLACIdempotent(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	cover := &coverFile{writeFile(t, filepath.Join(dir, "cover.png"), pngImage(t, 16, 16, 5)), FormatPNG}
	p := writeFLAC(t, dir, "idem.flac", baseFLAC(t, dir, "src.flac", sine3s).with(vorbisBlock("v",
		"TITLE=Old", "ALBUM ARTIST=Old", "COMPOSER=Keep")))
	first := writeChecked(t, tools, p, fullValues, cover)
	b1 := readFile(t, p)
	second := writeChecked(t, tools, p, fullValues, cover)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second write:\n%+v\n%+v", first, second)
	}
	if !bytes.Equal(b1, readFile(t, p)) {
		t.Fatal("the second write of the same values changed the bytes")
	}
}

// §9.2: music files are deterministic for the same input and versions. The
// same file and the same request give the same bytes, whether the file had
// a comment block or not, a cover or not.
func TestTagsFLACDeterministic(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	base := baseFLAC(t, dir, "src.flac", noise3s)
	cover := &coverFile{writeFile(t, filepath.Join(dir, "cover.jpg"), jpegImage(t, dir, "c.jpg", "green")), FormatJPEG}
	for _, tc := range []struct {
		name  string
		f     flacFile
		cover *coverFile
	}{
		{"untagged", base, nil},
		{"tagged with cover", base.with(vorbisBlock("v", "TITLE=x", "COMMENT=y"),
			flacPictureData{typ: 4, mime: "image/png", data: pngImage(t, 3, 3, 1)}.block()), cover},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sums []string
			for i := range 3 {
				p := writeFLAC(t, t.TempDir(), "copy"+string(rune('0'+i))+".flac", tc.f)
				writeChecked(t, tools, p, fullValues, tc.cover)
				sums = append(sums, sha(readFile(t, p)))
			}
			if sums[0] != sums[1] || sums[1] != sums[2] {
				t.Fatalf("the same write gave different files: %q", sums)
			}
		})
	}
}

// Inspect and ExtractImages never write; WriteManagedTags writes only the
// descriptor it is given.
func TestTagsInspectDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	p := writeFLAC(t, dir, "ro.flac", baseFLAC(t, dir, "src.flac", sine3s).with(vorbisBlock("v", "TITLE=x")))
	before := readFile(t, p)
	st0, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// A read-write descriptor: the helper still must not write.
	if _, err := tools.Inspect(t.Context(), openRW(t, p), FormatFLAC); err != nil {
		t.Fatal(err)
	}
	st1, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, readFile(t, p)) || !st0.ModTime().Equal(st1.ModTime()) {
		t.Fatal("Inspect modified the file")
	}
}

// M4A is refused with a typed error until its reader and writer exist.
func TestTagsUnsupportedFormats(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	fx := fixtures(t, dir)
	for _, tc := range []struct{ fixture, format string }{
		{"aac", FormatM4AAAC}, {"alac", FormatM4AALAC},
	} {
		_, err := tools.Inspect(t.Context(), open(t, fx[tc.fixture]), tc.format)
		wantCode(t, err, CodeTagsUnsupported)
		err = tools.WriteManagedTags(t.Context(), openRW(t, fx[tc.fixture]), tc.format, fullValues, nil)
		wantCode(t, err, CodeTagsUnsupported)
		_, err = tools.ExtractImages(t.Context(), open(t, fx[tc.fixture]), tc.format,
			[]ImageTarget{{0, createEmpty(t, t.TempDir(), "x")}})
		wantCode(t, err, CodeTagsUnsupported)
	}
	_, err := tools.Inspect(t.Context(), open(t, fx["flac16"]), "ogg")
	wantCode(t, err, CodeTagsUnsupported)
	// An MP3 declared as FLAC, and the reverse, are refused on the format.
	_, err = tools.Inspect(t.Context(), open(t, fx["mp3-cbr"]), FormatFLAC)
	wantCode(t, err, CodeTagsFormatMismatch)
	_, err = tools.Inspect(t.Context(), open(t, fx["flac16"]), FormatMP3)
	wantCode(t, err, CodeTagsFormatMismatch)
	err = tools.WriteManagedTags(t.Context(), openRW(t, fx["flac16"]), FormatMP3, fullValues, nil)
	wantCode(t, err, CodeTagsFormatMismatch)
}

func TestTagsErrorsAreTyped(t *testing.T) {
	err := &Error{Code: CodeTagsOpaque, Op: "musiclib-tags write-managed-tags", Msg: "fields ...", ExitCode: 3, Stderr: []byte("secret")}
	if Code(err) != CodeTagsOpaque || strings.Contains(err.Error(), "secret") {
		t.Fatalf("%v", err)
	}
	if !errors.Is(fmtWrap(err), err) {
		t.Fatal("not unwrappable")
	}
}

func fmtWrap(err error) error { return errors.Join(errors.New("context"), err) }

// The cover fixtures of the probe tests: a JPEG muxed by the pinned ffmpeg
// and a PNG written by the test codec. Each is one picture, extracted byte
// for byte (§7.4, §8.5: no conversion), and a write that replaces it keeps
// the audio.
func TestTagsFixtureCovers(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	fx := fixtures(t, dir)
	// ffmpeg writes its attached picture as type 0, "Other", unless told
	// otherwise: a fact the importer's cover selection (§7.4) must expect.
	for _, tc := range []struct {
		fixture, image, mime string
		typ                  uint32
	}{
		{"flac-cover", "cover.jpg", "image/jpeg", 0},
		{"flac-cover-png", "cover.png", "image/png", PictureFrontCover},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			img := readFile(t, fx[tc.image])
			in := inspectFile(t, tools, fx[tc.fixture])
			if len(in.Pictures) != 1 || in.Pictures[0].Type != tc.typ || in.Pictures[0].MIME != tc.mime ||
				in.Pictures[0].SHA256 != sha(img) {
				t.Fatalf("pictures: %+v", in.Pictures)
			}
			outDir := t.TempDir()
			out := createEmpty(t, outDir, "x")
			if _, err := tools.ExtractImages(t.Context(), open(t, fx[tc.fixture]), FormatFLAC, []ImageTarget{{0, out}}); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(outDir, "x")); !bytes.Equal(got, img) {
				t.Fatalf("extracted %d bytes, not the %d embedded ones", len(got), len(img))
			}
			p := writeFile(t, filepath.Join(t.TempDir(), "w.flac"), readFile(t, fx[tc.fixture]))
			writeChecked(t, tools, p, fullValues, &coverFile{fx["cover.png"], FormatPNG})
		})
	}
}

// A write that frees more than TagLib's padding threshold (1% of the file,
// at least 4 KiB, at most 1 MiB) shrinks the file: the audio frames move
// back (FdStream::removeBlock), byte for byte. A write that frees less
// keeps the size and pads.
func TestTagsFLACShrinks(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	big := append(pngImage(t, 4, 4, 1), garbage(9, 256<<10)...)
	f := baseFLAC(t, dir, "src.flac", noise3s).with(vorbisBlock("v", "TITLE=x", "COMMENT=keep"),
		flacPictureData{typ: 3, mime: "image/png", width: 4, height: 4, depth: 32, data: big}.block())
	p := writeFLAC(t, dir, "big.flac", f)
	size0 := len(readFile(t, p))
	writeChecked(t, tools, p, fullValues, nil)
	// The cover goes, and TagLib pads with 4 KiB.
	if size1 := len(readFile(t, p)); size1 > size0-(256<<10)+(8<<10) {
		t.Fatalf("the file did not shrink: %d -> %d bytes", size0, size1)
	}
	// A value of the same length: the metadata is rewritten in place, and the
	// audio does not move. (Any change that leaves more padding than the
	// threshold resets it to 4 KiB, so even a few bytes less can shrink the
	// file.)
	size1 := len(readFile(t, p))
	v := fullValues
	v.Title = "Tible"
	writeChecked(t, tools, p, v, nil)
	if size2 := len(readFile(t, p)); size2 != size1 {
		t.Fatalf("a small write changed the size: %d -> %d bytes", size1, size2)
	}
}

// §12.2 "Tag writer altera i campioni": a writer that changed one sample
// and nothing else would pass every tag check, so only the audio digest of
// §9.1 step 6 can catch it. Two files with the same tags and the same
// metadata blocks (STREAMINFO of the first, MD5 zeroed as some encoders
// leave it) and one differing sample: VerifyTags accepts the pair,
// audioChanged does not.
func TestTagsAlteredSamplesAreCaught(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	wav := wav16(t, dir, "a.wav", noise3s)
	b := readFile(t, wav)
	off, n := wavData(t, b)
	b[off+(n/4)*2] ^= 1 // one LSB of a sample in the middle
	changed := writeFile(t, filepath.Join(dir, "b.wav"), b)
	flac := func(name, src string) flacFile {
		f := parseFLAC(t, readFile(t, gen(t, dir, name, "-i", src, "-c:a", "flac")))
		return f.without(flacPadding, flacVorbisComment, flacPicture, flacApplication)
	}
	fa, fb := flac("a.flac", wav), flac("b.flac", changed)
	info := bytes.Clone(fa.blocks[0].data)
	clear(info[18:34]) // the MD5 of the samples
	tags := vorbisBlock("v", "TITLE=x", "COMMENT=y")
	fa.blocks, fb.blocks = []flacBlock{{typ: flacStreamInfo, data: info}, tags}, []flacBlock{{typ: flacStreamInfo, data: info}, tags}
	pa, pb := writeFLAC(t, dir, "a-tagged.flac", fa), writeFLAC(t, dir, "b-tagged.flac", fb)

	if err := VerifyTags(TagValues{Title: "x"}, nil, inspectFile(t, tools, pa), inspectFile(t, tools, pb)); err != nil {
		t.Fatalf("the tag check must be blind to the samples for this test to mean anything: %v", err)
	}
	if audioChanged(t, tools, pb, digest(t, tools, pa)) == nil {
		t.Fatal("a changed sample was not detected")
	}
	if err := audioChanged(t, tools, pa, digest(t, tools, pa)); err != nil {
		t.Fatalf("the same file: %v", err)
	}
}
