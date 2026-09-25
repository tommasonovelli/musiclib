package media

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// N-091: the limit is the helper's own. A cover of exactly
// MaxEmbeddedCover bytes is written and verified by the real helper; one
// byte more is refused by it with CodeTagsTooLarge, for JPEG and PNG (the
// MIME types differ by one byte).
func TestMaxEmbeddedCoverIsTheHelpersLimit(t *testing.T) {
	dir := t.TempDir()
	base := baseFLAC(t, dir, "src.flac", sine3s)
	src := writeFLAC(t, dir, "small.flac", base.with(vorbisBlock("v", "TITLE=x")))
	var jpg bytes.Buffer
	img := image.NewGray(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.Gray{Y: 200})
	if err := jpeg.Encode(&jpg, img, nil); err != nil {
		t.Fatal(err)
	}
	headers := map[string][]byte{FormatPNG: pngImage(t, 4, 4, 1), FormatJPEG: jpg.Bytes()}
	tools := newTools(t)
	for _, format := range []string{FormatJPEG, FormatPNG} {
		t.Run(format, func(t *testing.T) {
			limit, ok := MaxEmbeddedCover(FormatFLAC, format)
			if !ok {
				t.Fatal("no FLAC limit")
			}
			if want := int64(0xFFFFFF - 32 - len("image/"+format)); limit != want {
				t.Fatalf("limit %d, want %d", limit, want)
			}
			// Images padded after their end: DecodeConfig reads the header only.
			padded := func(name string, size int64) *coverFile {
				h := headers[format]
				b := append(bytes.Clone(h), make([]byte, int(size)-len(h))...)
				return &coverFile{writeFile(t, filepath.Join(t.TempDir(), name), b), format}
			}
			if err := EmbeddedCoverFits(FormatFLAC, format, limit); err != nil {
				t.Fatalf("EmbeddedCoverFits at the limit: %v", err)
			}
			wantCode(t, EmbeddedCoverFits(FormatFLAC, format, limit+1), CodeTagsTooLarge)

			p := writeFile(t, filepath.Join(t.TempDir(), "fits.flac"), readFile(t, src))
			after := writeChecked(t, tools, p, fullValues, padded("fits", limit))
			if len(after.Pictures) != 1 || after.Pictures[0].Size != limit {
				t.Fatalf("pictures after the write: %+v", after.Pictures)
			}
			q := writeFile(t, filepath.Join(t.TempDir(), "over.flac"), readFile(t, src))
			wantUnchanged(t, q, func() error {
				return tools.WriteManagedTags(t.Context(), openRW(t, q), FormatFLAC, fullValues, padded("over", limit+1).cover(t))
			}, CodeTagsTooLarge)
		})
	}
}

// Formats without a writer, or covers that are not JPEG or PNG, have no
// limit and are refused, never accepted by default.
func TestEmbeddedCoverFitsUnknownFormats(t *testing.T) {
	for _, tc := range []struct{ audio, cover string }{
		{FormatM4AAAC, FormatPNG}, {FormatM4AALAC, FormatJPEG},
		{FormatFLAC, "gif"}, {FormatMP3, "gif"}, {"", FormatJPEG},
	} {
		if _, ok := MaxEmbeddedCover(tc.audio, tc.cover); ok {
			t.Errorf("MaxEmbeddedCover(%q, %q) has a limit", tc.audio, tc.cover)
		}
		wantCode(t, EmbeddedCoverFits(tc.audio, tc.cover, 10), CodeTagsUnsupported)
	}
	wantCode(t, EmbeddedCoverFits(FormatFLAC, FormatPNG, -1), CodeTagsTooLarge)
}

// CoverMIME is the MIME type the writer embeds: the one describeCover
// reads from the image, for each cover format, and nothing for others.
func TestCoverMIMEIsTheWritersMIME(t *testing.T) {
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewGray(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for format, img := range map[string][]byte{FormatJPEG: jpg.Bytes(), FormatPNG: pngImage(t, 2, 2, 1)} {
		c, err := describeCover(&Cover{File: open(t, writeFile(t, filepath.Join(dir, format), img)), Format: format})
		if err != nil {
			t.Fatal(err)
		}
		if mime, ok := CoverMIME(format); !ok || mime != c.MIME {
			t.Errorf("CoverMIME(%s) = %q, %v; the writer embeds %q", format, mime, ok, c.MIME)
		}
	}
	for _, format := range []string{"", "gif", "flac", "JPEG"} {
		if mime, ok := CoverMIME(format); ok {
			t.Errorf("CoverMIME(%q) = %q", format, mime)
		}
	}
}

// The MP3 limit is the helper's: an APIC frame body of 256 MiB - 1. A cover
// one byte over it is refused by the helper before it is read (a sparse
// file: nothing is allocated); a cover larger than FLAC's limit embeds.
func TestMaxEmbeddedCoverMP3(t *testing.T) {
	for format, mime := range map[string]string{FormatJPEG: "image/jpeg", FormatPNG: "image/png"} {
		limit, ok := MaxEmbeddedCover(FormatMP3, format)
		if !ok || limit != 1<<28-1-4-int64(len(mime)) {
			t.Fatalf("MaxEmbeddedCover(mp3, %s) = %d, %v", format, limit, ok)
		}
		if EmbeddedCoverFits(FormatMP3, format, limit) != nil {
			t.Fatal("the limit itself does not fit")
		}
		wantCode(t, EmbeddedCoverFits(FormatMP3, format, limit+1), CodeTagsTooLarge)
	}
	dir := t.TempDir()
	src := mp3Source(t, dir)
	png := pngImage(t, 4, 4, 1)
	over := filepath.Join(dir, "over.png")
	f, err := os.Create(over)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(png); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 28); err != nil { // the helper's own bound plus one
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for name, tools := range helpers(t) {
		t.Run(name, func(t *testing.T) {
			p := mp3At(t, t.TempDir(), "f.mp3", src)
			wantUnchanged(t, p, func() error {
				return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatMP3, fullValues, &Cover{File: open(t, over), Format: FormatPNG})
			}, CodeTagsTooLarge)
		})
	}
	big := &coverFile{writeFile(t, filepath.Join(dir, "big.png"), append(bytes.Clone(png), make([]byte, 17<<20)...)), FormatPNG}
	writeMP3Checked(t, newTools(t), mp3At(t, t.TempDir(), "big.mp3", src), fullValues, big)
}
