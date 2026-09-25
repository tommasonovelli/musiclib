package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"testing"
)

// failingReader is an image whose read fails after n bytes: a failing disk
// is not an invalid image.
type failingReader struct {
	*bytes.Reader
	n int64
}

func (f *failingReader) Read(p []byte) (int, error) {
	pos, _ := f.Seek(0, io.SeekCurrent)
	if pos >= f.n {
		return 0, errors.New("EIO")
	}
	if rest := f.n - pos; int64(len(p)) > rest {
		p = p[:rest]
	}
	return f.Reader.Read(p)
}

// §8.5 through ValidateCover: the importer's cases are in
// importer.TestValidateCover; here the typed outcomes the API relies on.
func TestValidateCoverCodes(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 31)
	}
	img.Set(0, 0, color.RGBA{1, 2, 3, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	good := b.Bytes()
	if f, err := ValidateCover(bytes.NewReader(good), int64(len(good))); err != nil || f != FormatPNG {
		t.Fatalf("a PNG: %q %v", f, err)
	}
	for name, tc := range map[string]struct {
		r    io.ReadSeeker
		size int64
		code string
	}{
		"too large":         {bytes.NewReader(good), MaxCoverBytes + 1, CodeInvalidImage},
		"truncated":         {bytes.NewReader(good[:len(good)-30]), int64(len(good) - 30), CodeInvalidImage},
		"not an image":      {bytes.NewReader([]byte("hello")), 5, CodeInvalidImage},
		"read error, head":  {&failingReader{bytes.NewReader(good), 10}, int64(len(good)), CodeIO},
		"read error, pixel": {&failingReader{bytes.NewReader(good), int64(len(good) - 40)}, int64(len(good)), CodeIO},
	} {
		_, err := ValidateCover(tc.r, tc.size)
		if Code(err) != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}
}
