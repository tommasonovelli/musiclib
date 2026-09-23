package media

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// VerifyTags (§9.1 step 6) and the value helpers, as pure functions: every
// way a written file can differ from what was asked must be caught
// (§12.2: "Tag writer ... perde un tag non gestito: album non pubblicato").

func verifyFixture() (TagValues, *ExpectedCover, Inspection, Inspection) {
	v := TagValues{Title: "T", Artist: "A", Track: 3, TrackTotal: 12, Date: "1999", Compilation: true}
	cover := &ExpectedCover{MIME: "image/png", Size: 10, SHA256: strings.Repeat("ab", 32)}
	unmanaged := func() []KeyValues {
		return []KeyValues{
			{"flac.blocks", []string{"0:" + strings.Repeat("cd", 32)}},
			{"vorbis.vendor", []string{"v"}},
			{"vorbis:COMMENT", []string{"a", "b"}},
		}
	}
	before := Inspection{
		Format:    FormatFLAC,
		Managed:   ManagedTags{Title: []string{"old"}, AlbumArtist: []string{"x"}},
		Conflicts: []Conflict{{Field: "album_artist"}},
		Pictures:  []Picture{{Index: 0, Location: "block", Type: 4}},
		Unmanaged: unmanaged(),
		// Before the write, opaque fields that the write removes are fine,
		// the ID3 tags of a FLAC included (N-090).
		Opaque: []OpaqueField{{Key: "id3v2", Reason: "foreign_tag", Removed: true},
			{Key: "vorbis:TITLE", Reason: "invalid_utf8", Removed: true}, {Key: "id3v1", Reason: "foreign_tag", Removed: true}},
	}
	after := Inspection{
		Format: FormatFLAC,
		Managed: ManagedTags{Title: []string{"T"}, Artist: []string{"A"}, Track: []string{"3"}, TrackTotal: []string{"12"},
			Date: []string{"1999"}, Compilation: []string{"1"}},
		Pictures:  []Picture{{Index: 0, Location: "block", Type: 3, MIME: "image/png", Size: 10, SHA256: strings.Repeat("ab", 32)}},
		Unmanaged: unmanaged(),
	}
	return v, cover, before, after
}

func TestVerifyTags(t *testing.T) {
	v, cover, before, after := verifyFixture()
	if err := VerifyTags(v, cover, before, after); err != nil {
		t.Fatalf("a correct write: %v", err)
	}
	after.Pictures = nil
	if err := VerifyTags(v, nil, before, after); err != nil {
		t.Fatalf("a correct write without cover: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(v *TagValues, c **ExpectedCover, before, after *Inspection)
	}{
		{"different managed value", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Managed.Title = []string{"X"} }},
		{"managed value missing", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Managed.Artist = nil }},
		{"absent field present", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Managed.Genre = []string{"Rock"} }},
		{"value written twice", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Managed.Title = []string{"T", "T"} }},
		{"number with a leading zero", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Managed.Track = []string{"03"} }},
		{"compilation as true", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Managed.Compilation = []string{"true"} }},
		{"compilation removed", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Managed.Compilation = nil }},
		{"compilation left when false", func(v *TagValues, _ **ExpectedCover, _, _ *Inspection) { v.Compilation = false }},
		{"total left when absent", func(v *TagValues, _ **ExpectedCover, _, _ *Inspection) { v.TrackTotal = 0 }},
		{"conflict left", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Conflicts = []Conflict{{Field: "date", Sources: []KeyValues{{"DATE", []string{"1999"}}, {"YEAR", []string{"1998"}}}}}
		}},
		{"opaque field", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Opaque = []OpaqueField{{Key: "vorbis:COMMENT", Reason: "invalid_utf8"}}
		}},
		{"blocking opaque field before the write", func(_ *TagValues, _ **ExpectedCover, b, _ *Inspection) {
			b.Opaque = append(b.Opaque, OpaqueField{Key: "vorbis:COMMENT", Reason: "invalid_utf8"})
		}},
		{"opaque field a write removes", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Opaque = []OpaqueField{{Key: "vorbis:TITLE", Reason: "nul_byte", Removed: true}}
		}},
		// N-090: the ID3 tags are a declared removal only because the write
		// strips them: one left after the write, or one an older helper
		// reported as blocking, fails.
		{"ID3v2 tag left after the write", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Opaque = []OpaqueField{{Key: "id3v2", Reason: "foreign_tag", Removed: true}}
		}},
		{"ID3v1 tag left after the write", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Opaque = []OpaqueField{{Key: "id3v1", Reason: "foreign_tag", Removed: true}}
		}},
		{"ID3 tag reported as blocking before the write", func(_ *TagValues, _ **ExpectedCover, b, _ *Inspection) {
			b.Opaque = []OpaqueField{{Key: "id3v2", Reason: "foreign_tag"}}
		}},
		{"cover missing", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Pictures = nil }},
		{"cover of another hash", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Pictures[0].SHA256 = strings.Repeat("00", 32)
		}},
		{"cover of another size", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Pictures[0].Size = 11 }},
		{"cover of another MIME type", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Pictures[0].MIME = "image/jpeg" }},
		{"cover not a front cover", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Pictures[0].Type = 4 }},
		{"extra picture", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Pictures = append(a.Pictures, Picture{Index: 1, Location: "comment", Type: 0})
		}},
		{"picture left without cover", func(_ *TagValues, c **ExpectedCover, _, _ *Inspection) { *c = nil }},
		{"unmanaged field lost", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Unmanaged = a.Unmanaged[:2] }},
		{"vendor lost", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Unmanaged = slices.Delete(slices.Clone(a.Unmanaged), 1, 2)
		}},
		{"kept block changed", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Unmanaged[0].Values = []string{"0:" + strings.Repeat("ee", 32)}
		}},
		{"unmanaged value changed", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Unmanaged[2].Values = []string{"a", "c"}
		}},
		{"unmanaged value lost", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Unmanaged[2].Values = []string{"a"} }},
		{"unmanaged values reordered", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Unmanaged[2].Values = []string{"b", "a"}
		}},
		{"unmanaged field added", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Unmanaged = append(a.Unmanaged, KeyValues{"vorbis:NEW", []string{"x"}})
		}},
		{"unmanaged key duplicated", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) {
			a.Unmanaged = append(a.Unmanaged, a.Unmanaged[2])
		}},
		{"formats differ", func(_ *TagValues, _ **ExpectedCover, _, a *Inspection) { a.Format = FormatMP3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, cover, before, after := verifyFixture()
			tc.mutate(&v, &cover, &before, &after)
			wantCode(t, VerifyTags(v, cover, before, after), CodeTagsVerification)
		})
	}
}

// The values a write writes are how Inspect reports them: nothing for the
// zero values.
func TestExpectedManaged(t *testing.T) {
	if got := expectedManaged(TagValues{}); !reflect.DeepEqual(got, ManagedTags{}) {
		t.Fatalf("zero values: %+v", got)
	}
	got := expectedManaged(TagValues{Title: "a; b", Disc: 1, DiscTotal: 99, Compilation: true})
	want := ManagedTags{Title: []string{"a; b"}, Disc: []string{"1"}, DiscTotal: []string{"99"}, Compilation: []string{"1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestTagNumber(t *testing.T) {
	for _, tc := range []struct {
		in []string
		n  int
		ok bool
	}{
		{[]string{"3"}, 3, true},
		{[]string{"03"}, 3, true},
		{[]string{" 7 "}, 7, true},
		{[]string{"0"}, 0, true},
		{[]string{"000000000000000000001"}, 1, true},
		{[]string{"999"}, 999, true},
		{[]string{"123456789"}, 123456789, true},
		// Too large for the schema: never truncated or wrapped.
		{[]string{"1000000000"}, math.MaxInt32, true},
		{[]string{"99999999999999999999999"}, math.MaxInt32, true},
		{[]string{"A1"}, 0, false},
		{[]string{"3/12"}, 0, false},
		{[]string{"-1"}, 0, false},
		{[]string{"+1"}, 0, false},
		{[]string{"1.0"}, 0, false},
		{[]string{"１"}, 0, false}, // fullwidth digit
		{[]string{""}, 0, false},
		{[]string{"  "}, 0, false},
		{[]string{"1", "2"}, 0, false},
		{nil, 0, false},
	} {
		if n, ok := TagNumber(tc.in); n != tc.n || ok != tc.ok {
			t.Errorf("TagNumber(%q) = %d, %v; want %d, %v", tc.in, n, ok, tc.n, tc.ok)
		}
	}
}

func TestTagBool(t *testing.T) {
	for _, tc := range []struct {
		in    []string
		v, ok bool
	}{
		{[]string{"1"}, true, true},
		{[]string{"true"}, true, true},
		{[]string{"TRUE"}, true, true},
		{[]string{" True "}, true, true},
		{[]string{"0"}, false, true},
		{[]string{"false"}, false, true},
		{[]string{"yes"}, false, false},
		{[]string{"01"}, false, false},
		{[]string{""}, false, false},
		{[]string{"1", "1"}, false, false},
		{nil, false, false},
	} {
		if v, ok := TagBool(tc.in); v != tc.v || ok != tc.ok {
			t.Errorf("TagBool(%q) = %v, %v; want %v, %v", tc.in, v, ok, tc.v, tc.ok)
		}
	}
}

func TestJoinValues(t *testing.T) {
	for in, want := range map[string]string{"": "", "a": "a", "a\x00b": "a; b", "a\x00 b\x00c": "a;  b; c"} {
		var values []string
		if in != "" {
			values = strings.Split(in, "\x00")
		}
		if got := JoinValues(values); got != want {
			t.Errorf("JoinValues(%q) = %q, want %q", values, got, want)
		}
	}
}

// libFLAC's rule for the bits per pixel of a PNG (share/grabbag/picture.c):
// IHDR bit depth times samples per pixel; 24 for any palette image.
func TestPNGDepth(t *testing.T) {
	for _, tc := range []struct {
		bitDepth, colorType byte
		want                int
	}{
		{1, 0, 1}, {4, 0, 4}, {8, 0, 8}, {16, 0, 16},
		{8, 2, 24}, {16, 2, 48},
		{1, 3, 24}, {2, 3, 24}, {8, 3, 24},
		{8, 4, 16}, {16, 4, 32},
		{8, 6, 32}, {16, 6, 64},
	} {
		if got := pngDepth(tc.bitDepth, tc.colorType); got != tc.want {
			t.Errorf("pngDepth(%d, %d) = %d, want %d", tc.bitDepth, tc.colorType, got, tc.want)
		}
	}
}

// encodePNG encodes an image with image/png.
func encodePNG(t testing.TB, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The attributes of the embedded picture come from the image header, as
// metaflac computes them, and end up in the written PICTURE block.
func TestDescribeCover(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	opaque := func(m interface{ Set(x, y int, c color.Color) }) {
		m.Set(0, 0, color.White)
	}
	gray := image.NewGray(image.Rect(0, 0, 3, 2))
	opaque(gray)
	gray16 := image.NewGray16(image.Rect(0, 0, 3, 2))
	rgb := image.NewRGBA(image.Rect(0, 0, 5, 4))
	for i := range rgb.Pix {
		rgb.Pix[i] = 0xFF // opaque: image/png writes truecolor without alpha
	}
	nrgba := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	nrgba.Set(0, 0, color.NRGBA{1, 2, 3, 4})
	rgb48 := image.NewRGBA64(image.Rect(0, 0, 2, 2))
	for i := range rgb48.Pix {
		rgb48.Pix[i] = 0xFF
	}
	rgb48.Set(0, 0, color.RGBA64{1, 2, 3, 0xFFFF})
	nrgba64 := image.NewNRGBA64(image.Rect(0, 0, 2, 2))
	nrgba64.Set(0, 0, color.NRGBA64{1, 2, 3, 4})
	palette4 := image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White, color.Gray{9}, color.Gray{200}})
	var palette200 color.Palette
	for i := range 200 {
		palette200 = append(palette200, color.RGBA{byte(i), 0, 0, 255})
	}
	paletted200 := image.NewPaletted(image.Rect(0, 0, 7, 3), palette200)

	for _, tc := range []struct {
		name   string
		data   []byte
		format string
		want   coverJSON
	}{
		{"gray PNG", encodePNG(t, gray), FormatPNG, coverJSON{"image/png", 3, 2, 8, 0}},
		{"16-bit gray PNG", encodePNG(t, gray16), FormatPNG, coverJSON{"image/png", 3, 2, 16, 0}},
		{"RGB PNG", encodePNG(t, rgb), FormatPNG, coverJSON{"image/png", 5, 4, 24, 0}},
		{"RGBA PNG", encodePNG(t, nrgba), FormatPNG, coverJSON{"image/png", 2, 2, 32, 0}},
		{"48-bit RGB PNG", encodePNG(t, rgb48), FormatPNG, coverJSON{"image/png", 2, 2, 48, 0}},
		{"64-bit RGBA PNG", encodePNG(t, nrgba64), FormatPNG, coverJSON{"image/png", 2, 2, 64, 0}},
		{"4-color palette PNG", encodePNG(t, palette4), FormatPNG, coverJSON{"image/png", 4, 4, 24, 4}},
		{"200-color palette PNG", encodePNG(t, paletted200), FormatPNG, coverJSON{"image/png", 7, 3, 24, 200}},
		{"color JPEG", jpegImage(t, dir, "c.jpg", "red"), FormatJPEG, coverJSON{"image/jpeg", 24, 16, 24, 0}},
		// The pinned ffmpeg has no gray JPEG encoder; image/jpeg writes one
		// component for *image.Gray.
		{"gray JPEG", encodeJPEG(t, image.NewGray(image.Rect(0, 0, 10, 6))), FormatJPEG, coverJSON{"image/jpeg", 10, 6, 8, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "cover"), tc.data)
			got, err := describeCover(&Cover{File: open(t, path), Format: tc.format})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("describeCover = %+v, want %+v", got, tc.want)
			}
			flac := writeFLAC(t, t.TempDir(), "f.flac", baseFLAC(t, t.TempDir(), "src.flac", sine3s))
			writeChecked(t, tools, flac, TagValues{Title: "x"}, &coverFile{path, tc.format})
			pics := parseFLAC(t, readFile(t, flac)).ofType(flacPicture)
			want := flacPictureData{typ: 3, mime: tc.want.MIME, width: uint32(tc.want.Width), height: uint32(tc.want.Height),
				depth: uint32(tc.want.Depth), colors: uint32(tc.want.Colors), data: tc.data}
			if len(pics) != 1 || !reflect.DeepEqual(parsePictureBlock(t, pics[0].data), want) {
				t.Fatalf("written picture blocks: %d", len(pics))
			}
		})
	}
}

// Invalid arguments are refused by the adapter before the helper runs, and
// the file is not touched.
func TestWriteManagedTagsInvalidArguments(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	p := writeFLAC(t, dir, "f.flac", baseFLAC(t, dir, "src.flac", sine3s).with(vorbisBlock("v", "TITLE=x")))
	pngPath := writeFile(t, filepath.Join(dir, "c.png"), pngImage(t, 2, 2, 1))
	jpgPath := writeFile(t, filepath.Join(dir, "c.jpg"), jpegImage(t, dir, "src.jpg", "blue"))
	for _, tc := range []struct {
		name  string
		v     TagValues
		cover func(t *testing.T) *Cover
	}{
		{"title not UTF-8", TagValues{Title: "caf\xe9"}, nil},
		{"artist with NUL", TagValues{Artist: "a\x00b"}, nil},
		{"genre with a lone surrogate encoding", TagValues{Genre: "\xed\xa0\x80"}, nil},
		{"negative track", TagValues{Track: -1}, nil},
		{"disc total over int32", TagValues{DiscTotal: math.MaxInt32 + 1}, nil},
		{"PNG declared as JPEG", TagValues{}, func(t *testing.T) *Cover { return &Cover{File: open(t, pngPath), Format: FormatJPEG} }},
		{"JPEG declared as PNG", TagValues{}, func(t *testing.T) *Cover { return &Cover{File: open(t, jpgPath), Format: FormatPNG} }},
		{"GIF", TagValues{}, func(t *testing.T) *Cover { return &Cover{File: open(t, pngPath), Format: "gif"} }},
		{"no file", TagValues{}, func(*testing.T) *Cover { return &Cover{Format: FormatPNG} }},
		{"not an image", TagValues{}, func(t *testing.T) *Cover { return &Cover{File: open(t, p), Format: FormatPNG} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cover *Cover
			if tc.cover != nil {
				cover = tc.cover(t)
			}
			wantUnchanged(t, p, func() error {
				return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatFLAC, tc.v, cover)
			}, CodeInvalidArgument)
		})
	}
}

// encodeJPEG encodes an image with image/jpeg.
func encodeJPEG(t testing.TB, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
