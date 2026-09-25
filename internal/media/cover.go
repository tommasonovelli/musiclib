package media

import (
	"fmt"
	"image"
	_ "image/jpeg" // the cover formats of §8.5, registered for image.Decode
	_ "image/png"
	"io"
)

// Limits of a cover (DESIGN.md §8.5): JPEG or PNG, at most 20 MiB and 40
// million pixels, and a valid decode. catalog.MaxCoverBytes is the same
// value, checked again by the catalog where a cover is chosen.
const (
	MaxCoverBytes  = 20 << 20
	MaxCoverPixels = 40_000_000
)

// CodeInvalidImage is ValidateCover's refusal of an image as a cover: not a
// JPEG or PNG by content, too large, too many pixels, or not decoding
// completely. Msg says which, in words a user can act on.
const CodeInvalidImage = "media_invalid_image"

// ValidateCover is §8.5's check of an image, done in Go (N-073: the pinned
// ffmpeg has no PNG decoder): JPEG or PNG by content, at most
// MaxCoverBytes, at most MaxCoverPixels, and a complete decode. It returns
// the blobs.format value ("jpeg" or "png"). The pixel count is checked on
// the header before anything is decoded, so an oversized image is never
// allocated.
//
// A refusal is CodeInvalidImage; a failure to read r is CodeIO, never a
// refusal. The bytes are never converted (§8.5): the caller keeps them as
// they are. The importer uses it for the cover selection (§7.4), the API
// for the cover endpoints of §10.2.
func ValidateCover(r io.ReadSeeker, size int64) (string, error) {
	const op = "cover check"
	refuse := func(format string, args ...any) (string, error) {
		return "", newErr(CodeInvalidImage, op, fmt.Sprintf(format, args...), nil)
	}
	if size > MaxCoverBytes {
		return refuse("it takes %d bytes, the maximum is %d", size, MaxCoverBytes)
	}
	src := &readErrors{r: r}
	if err := src.rewind(); err != nil {
		return "", err
	}
	cfg, format, err := image.DecodeConfig(src)
	switch {
	case src.err != nil:
		return "", newErr(CodeIO, op, "reading the image", src.err)
	case err != nil || (format != FormatJPEG && format != FormatPNG):
		return refuse("it is not a JPEG or PNG image")
	}
	if px := int64(cfg.Width) * int64(cfg.Height); px <= 0 || px > MaxCoverPixels {
		return refuse("it has %d×%d pixels, the maximum is %d", cfg.Width, cfg.Height, MaxCoverPixels)
	}
	if err := src.rewind(); err != nil {
		return "", err
	}
	_, decoded, err := image.Decode(src)
	switch {
	case src.err != nil:
		return "", newErr(CodeIO, op, "reading the image", src.err)
	case err != nil || decoded != format:
		return refuse("the %s image does not decode completely", format)
	}
	return format, nil
}

// readErrors remembers the first read error that is not the end of the
// input, so that ValidateCover tells a damaged image from a failing disk.
type readErrors struct {
	r   io.ReadSeeker
	err error
}

func (e *readErrors) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}

func (e *readErrors) rewind() error {
	if _, err := e.r.Seek(0, io.SeekStart); err != nil {
		return newErr(CodeIO, "cover check", "rewinding the image", err)
	}
	return nil
}
