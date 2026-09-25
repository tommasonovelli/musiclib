package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The TagLib helper native/musiclib-tags (DESIGN.md §2.1, §8.1): three typed
// operations, inspect, extract-images and write-managed-tags, run through
// the Runner with the 30 s limit of §8.5. The request is JSON on the
// helper's standard input, the result JSON on its standard output; files
// are descriptors 3, 4, ... and never paths (NOTES.md N-075, N-084).
//
// FLAC, MP3 and M4A (AAC and ALAC) are implemented completely.

// PictureFrontCover is the picture type of a front cover (FLAC PICTURE and
// ID3v2 APIC type 3).
const PictureFrontCover = 3

const (
	// tagsOutputLimit bounds the helper's JSON. An inspection repeats the
	// Vorbis comment, which is at most 16 MiB, with JSON escaping.
	tagsOutputLimit = 128 << 20
	// tagsExitFailure is the helper's exit status for a typed failure, with
	// {"code", "message"} on standard error.
	tagsExitFailure = 3
	// maxExtract is the most pictures one ExtractImages call writes
	// (kMaxPictures in the helper).
	maxExtract = 256
)

// tagsCodes maps the helper's failure codes (src/failure.h) to the
// adapter's.
var tagsCodes = map[string]string{
	"invalid_request":    CodeTagsInvalidRequest,
	"bad_descriptor":     CodeTagsBadDescriptor,
	"unsupported_format": CodeTagsUnsupported,
	"format_mismatch":    CodeTagsFormatMismatch,
	"corrupt":            CodeTagsCorrupt,
	"opaque_field":       CodeTagsOpaque,
	"too_large":          CodeTagsTooLarge,
	"picture_not_found":  CodeTagsNoPicture,
	"io":                 CodeTagsIO,
	"no_space":           CodeTagsNoSpace,
	"internal":           CodeTagsInternal,
}

// Inspection is what Inspect reads from one audio file (§8.1–§8.3).
type Inspection struct {
	// Format is the blobs.format the file was inspected as.
	Format string `json:"format"`
	// Managed holds the managed fields, read with the rules of §8.1.
	Managed ManagedTags `json:"managed"`
	// Conflicts lists the managed fields whose sources disagree: for FLAC,
	// the canonical key and its aliases (the canonical one wins); for MP3,
	// every non-empty source in ID3v2, APE and ID3v1 (the first wins, §8.1).
	Conflicts []Conflict `json:"conflicts"`
	// Pictures lists every embedded picture, in file order.
	Pictures []Picture `json:"pictures"`
	// Unmanaged is every unmanaged field in a canonical, comparable form,
	// sorted by key: two inspections hold the same unmanaged fields exactly
	// when these are equal (§8.3: semantic preservation of the decoded
	// fields). For FLAC the keys are "vorbis:<KEY>" (the Vorbis field name in
	// upper case, values in file order), "vorbis.vendor", and "flac.blocks"
	// (type and SHA-256 of every metadata block that is not a tag, a
	// picture or padding, in file order). For MP3 (NOTES.md N-153):
	// "id3v2:<ID>", "id3v2:TXXX:<description>", "id3v2:COMM:<language>:<description>"
	// (USLT the same), "id3v2:WXXX:<description>", "id3v2:UFID:<owner>",
	// "id3v2:PRIV:<owner>" with the decoded values of the unmanaged frames
	// (a frame with flags: its flags and the SHA-256 of its stored data);
	// "ape:<KEY>" with the unmanaged APE items; "id3v1:comment"; and
	// "mpeg.audio", the SHA-256 of the audio bytes between the tags.
	// For M4A (NOTES.md N-167): "ilst:<atom>" and
	// "ilst:----:<mean>:<name>" with the values of the unmanaged items (the
	// text of a UTF-8 data atom, else its type, locale and SHA-256);
	// "mp4.box:<path>" with the SHA-256 of every box a write keeps byte for
	// byte (the size of an mdat); "mp4.trak", the SHA-256 of the track with
	// its chunk offsets zeroed; and "mp4.samples", the SHA-256 of the audio
	// samples read through the sample table.
	Unmanaged []KeyValues `json:"unmanaged"`
	// Opaque lists the fields the helper cannot save back without loss.
	Opaque []OpaqueField `json:"opaque"`
	// Audio is where the helper's reader finds the audio in the file: after
	// the leading metadata, before the trailing tags. AudioDigest decodes
	// exactly the bytes before Audio.End (NOTES.md N-128, N-154).
	Audio AudioRange `json:"audio"`
}

// AudioRange is a byte range of a file: [Start, End).
type AudioRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// ManagedTags are the managed fields of §8.2 as read from a file: each one
// an ordered list of values, empty when the field is absent. Multi-valued
// text stays a list (JoinValues gives the single string of §7.3). Track and
// disc numbers are text: "N/M" is already split into the number and the
// total (TagNumber parses them).
type ManagedTags struct {
	Title       []string `json:"title"`
	Artist      []string `json:"artist"`
	AlbumArtist []string `json:"album_artist"`
	Album       []string `json:"album"`
	Track       []string `json:"track"`
	TrackTotal  []string `json:"track_total"`
	Disc        []string `json:"disc"`
	DiscTotal   []string `json:"disc_total"`
	Date        []string `json:"date"`
	Genre       []string `json:"genre"`
	Compilation []string `json:"compilation"`
}

// fields lists the managed fields with their JSON names, in table order.
func (m *ManagedTags) fields() []struct {
	name string
	v    *[]string
} {
	return []struct {
		name string
		v    *[]string
	}{
		{"title", &m.Title}, {"artist", &m.Artist}, {"album_artist", &m.AlbumArtist}, {"album", &m.Album},
		{"track", &m.Track}, {"track_total", &m.TrackTotal}, {"disc", &m.Disc}, {"disc_total", &m.DiscTotal},
		{"date", &m.Date}, {"genre", &m.Genre}, {"compilation", &m.Compilation},
	}
}

// KeyValues is a key and its values in order.
type KeyValues struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// Conflict reports the sources of one managed field that disagree, in
// reading order; the first is the one Managed holds.
type Conflict struct {
	Field   string      `json:"field"`
	Sources []KeyValues `json:"sources"`
}

// Picture is one embedded picture.
type Picture struct {
	// Index is its position in Inspection.Pictures, the number
	// ExtractImages takes.
	Index int `json:"index"`
	// Location is "block" (a FLAC PICTURE block), "comment" (a picture in a
	// Vorbis comment), "id3v2" (an APIC frame), "ape" (a "Cover Art (...)"
	// item) or "covr" (an image of an M4A covr atom, always a front cover,
	// NOTES.md N-166). An APIC, APE or covr picture has no width, height,
	// depth or colors.
	Location string `json:"location"`
	// Type is the picture type (PictureFrontCover, ...).
	Type   uint32 `json:"type"`
	MIME   string `json:"mime"`
	Width  uint32 `json:"width"`
	Height uint32 `json:"height"`
	Depth  uint32 `json:"depth"`
	Colors uint32 `json:"colors"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// OpaqueField is a field that cannot be saved back without loss (§8.3).
type OpaqueField struct {
	Key string `json:"key"`
	// Reason is a stable code: malformed_entry, invalid_key, invalid_utf8,
	// nul_byte, duplicate_block, foreign_metadata, foreign_tag,
	// invalid_picture (NOTES.md N-086); for MP3 also invalid_text,
	// malformed_frame, unknown_flags, unsupported_frame, empty_frame,
	// compressed_frame, encrypted_frame, duplicate_tag, migration_conflict
	// (N-153); for M4A also unsupported_data (N-167).
	Reason string `json:"reason"`
	// Removed is true when a write removes the field anyway (a managed key,
	// an alias, a sort key, a picture, an ID3v2 or ID3v1 tag in a FLAC
	// file, NOTES.md N-090): it is lost by design, not by the adapter.
	Removed bool `json:"removed"`
}

// Blocking returns the opaque fields that make WriteManagedTags refuse the
// file: the ones a write would not remove anyway.
func (in Inspection) Blocking() []OpaqueField {
	var out []OpaqueField
	for _, o := range in.Opaque {
		if !o.Removed {
			out = append(out, o)
		}
	}
	return out
}

// Inspect reads the tags of the audio file f, whose format (blobs.format)
// the probe decided from its content. f must be readable; its offset is not
// used.
func (t *Tools) Inspect(ctx context.Context, f *os.File, format string) (Inspection, error) {
	var in Inspection
	if err := t.runTags(ctx, "inspect", map[string]string{"format": format}, []*os.File{f}, &in); err != nil {
		return Inspection{}, err
	}
	if in.Format != format {
		return Inspection{}, newErr(CodeOutputInvalid, "musiclib-tags inspect", "the inspection is of format "+in.Format, nil)
	}
	for i, p := range in.Pictures {
		if p.Index != i {
			return Inspection{}, newErr(CodeOutputInvalid, "musiclib-tags inspect", "pictures out of order", nil)
		}
	}
	return in, nil
}

// ImageTarget asks for one embedded picture to be written to Dst.
type ImageTarget struct {
	// Index is Picture.Index.
	Index int
	// Dst is an empty regular file open for writing, typically created by
	// the caller in its work directory through internal/fsops. The bytes are
	// written as they are embedded, without any conversion.
	Dst *os.File
}

// ExtractedImage is what ExtractImages wrote for one target.
type ExtractedImage struct {
	Index  int    `json:"index"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ExtractImages writes the selected embedded pictures of the audio file f
// to the targets' files, byte for byte (§7.4). Nothing is written unless
// every index exists and every target is an empty, writable regular file.
func (t *Tools) ExtractImages(ctx context.Context, f *os.File, format string, targets []ImageTarget) ([]ExtractedImage, error) {
	const op = "musiclib-tags extract-images"
	if len(targets) == 0 || len(targets) > maxExtract {
		return nil, newErr(CodeInvalidArgument, op, "between 1 and "+strconv.Itoa(maxExtract)+" pictures", nil)
	}
	req := struct {
		Format   string `json:"format"`
		Pictures []int  `json:"pictures"`
	}{Format: format}
	files := []*os.File{f}
	for _, tg := range targets {
		if tg.Index < 0 || tg.Dst == nil {
			return nil, newErr(CodeInvalidArgument, op, "a target without a destination or with a negative index", nil)
		}
		req.Pictures = append(req.Pictures, tg.Index)
		files = append(files, tg.Dst)
	}
	var out struct {
		Images []ExtractedImage `json:"images"`
	}
	if err := t.runTags(ctx, "extract-images", req, files, &out); err != nil {
		return nil, err
	}
	if len(out.Images) != len(targets) {
		return nil, newErr(CodeOutputInvalid, op, "the helper reported "+strconv.Itoa(len(out.Images))+" images", nil)
	}
	for i, img := range out.Images {
		if img.Index != targets[i].Index {
			return nil, newErr(CodeOutputInvalid, op, "the images are not in request order", nil)
		}
		st, err := targets[i].Dst.Stat()
		if err != nil {
			return nil, newErr(CodeIO, op, "stat of an extracted image", err)
		}
		if st.Size() != img.Size {
			return nil, newErr(CodeOutputInvalid, op, "an extracted image does not have the size the helper reported", nil)
		}
	}
	return out.Images, nil
}

// TagValues are the managed fields to write (§8.2). The zero value of a
// field removes it: "" for text, 0 for a number, false for Compilation,
// which is written as "1" when true. Text is written as given (it must be
// valid UTF-8 without NUL); multi-valued fields are one value, already
// joined (§7.3).
type TagValues struct {
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	// Track and Disc are the numbers; TrackTotal is the highest track
	// number of the disc and DiscTotal the highest disc number of the album
	// (§8.2), computed by the caller.
	Track      int
	TrackTotal int
	Disc       int
	DiscTotal  int
	Date       string
	Genre      string
	// Compilation is written as "1", or removed when false.
	Compilation bool
}

// Cover is the image to embed as the one front cover (§8.2, §8.5).
type Cover struct {
	// File is the image, readable; its bytes are embedded as they are.
	File *os.File
	// Format is its blobs.format: "jpeg" or "png".
	Format string
}

// Values of blobs.format for covers (§4.2).
const (
	FormatJPEG = "jpeg"
	FormatPNG  = "png"
)

// WriteManagedTags rewrites every managed field of the audio file f, a
// staging copy open read-write, in place (§8.2, §8.3): absent values are
// removed, the known aliases and the sort keys of the four managed names are
// removed, and the embedded pictures are replaced by exactly one front cover
// (cover) or by none (nil). Unmanaged fields stay; the file is not rebuilt.
//
// The helper refuses the file (CodeTagsOpaque) when a field would be lost,
// and writes nothing on any refusal. A failure while writing (CodeTagsIO,
// CodeTagsInternal, a timeout) can leave the staging copy partially
// rewritten: the caller discards it (§9.1).
func (t *Tools) WriteManagedTags(ctx context.Context, f *os.File, format string, v TagValues, cover *Cover) error {
	req, files, err := writeRequest(f, format, v, cover)
	if err != nil {
		return err
	}
	var out struct{}
	return t.runTags(ctx, "write-managed-tags", req, files, &out)
}

// writeRequestJSON is the request of write-managed-tags.
type writeRequestJSON struct {
	Format string     `json:"format"`
	Tags   tagsJSON   `json:"tags"`
	Cover  *coverJSON `json:"cover"`
}

// writeRequest validates a write and returns its request and descriptors:
// the audio file as 3 and, with a cover, the image as 4.
func writeRequest(f *os.File, format string, v TagValues, cover *Cover) (writeRequestJSON, []*os.File, error) {
	const op = "musiclib-tags write-managed-tags"
	tags, err := tagsRequest(v)
	if err != nil {
		return writeRequestJSON{}, nil, newErr(CodeInvalidArgument, op, err.Error(), nil)
	}
	req := writeRequestJSON{Format: format, Tags: tags}
	files := []*os.File{f}
	if cover != nil {
		c, err := describeCover(cover)
		if err != nil {
			return writeRequestJSON{}, nil, newErr(CodeInvalidArgument, op, "the cover: "+err.Error(), err)
		}
		req.Cover = &c
		files = append(files, cover.File)
	}
	return req, files, nil
}

// tagsJSON is the "tags" object of a write request: every key present,
// null for an absent value.
type tagsJSON struct {
	Title       *string `json:"title"`
	Artist      *string `json:"artist"`
	AlbumArtist *string `json:"album_artist"`
	Album       *string `json:"album"`
	Track       *int    `json:"track"`
	TrackTotal  *int    `json:"track_total"`
	Disc        *int    `json:"disc"`
	DiscTotal   *int    `json:"disc_total"`
	Date        *string `json:"date"`
	Genre       *string `json:"genre"`
	Compilation bool    `json:"compilation"`
}

type coverJSON struct {
	MIME   string `json:"mime"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Depth  int    `json:"depth"`
	Colors int    `json:"colors"`
}

// tagsRequest validates the values and builds the request object. Text
// must be valid UTF-8 without NUL: encoding/json would silently replace
// invalid bytes, and TagLib stops at a NUL.
func tagsRequest(v TagValues) (tagsJSON, error) {
	text := func(name, s string) (*string, error) {
		if s == "" {
			return nil, nil
		}
		if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
			return nil, errors.New(name + " is not valid UTF-8 text without NUL")
		}
		return &s, nil
	}
	number := func(name string, n int) (*int, error) {
		if n == 0 {
			return nil, nil
		}
		if n < 0 || n > math.MaxInt32 {
			return nil, errors.New(name + " is out of range")
		}
		return &n, nil
	}
	var r tagsJSON
	var errs []error
	add := func(err error) { errs = append(errs, err) }
	var err error
	r.Title, err = text("title", v.Title)
	add(err)
	r.Artist, err = text("artist", v.Artist)
	add(err)
	r.AlbumArtist, err = text("album artist", v.AlbumArtist)
	add(err)
	r.Album, err = text("album", v.Album)
	add(err)
	r.Track, err = number("track", v.Track)
	add(err)
	r.TrackTotal, err = number("track total", v.TrackTotal)
	add(err)
	r.Disc, err = number("disc", v.Disc)
	add(err)
	r.DiscTotal, err = number("disc total", v.DiscTotal)
	add(err)
	r.Date, err = text("date", v.Date)
	add(err)
	r.Genre, err = text("genre", v.Genre)
	add(err)
	r.Compilation = v.Compilation
	return r, errors.Join(errs...)
}

// describeCover reads the header of the cover image (image.DecodeConfig, no
// full decode) for the attributes of the embedded picture: MIME type,
// width, height, bits per pixel and, for a palette image, the number of
// colors, computed as libFLAC's metaflac computes them (NOTES.md N-087).
// The format found must be the declared one. Whether the image decodes
// completely (§8.5) is the caller's check, when it accepts the cover.
func describeCover(c *Cover) (coverJSON, error) {
	if c.File == nil {
		return coverJSON{}, errors.New("no file")
	}
	if _, err := c.File.Seek(0, io.SeekStart); err != nil {
		return coverJSON{}, err
	}
	var out coverJSON
	switch c.Format {
	case FormatJPEG:
		cfg, err := jpeg.DecodeConfig(c.File)
		if err != nil {
			return coverJSON{}, err
		}
		out = coverJSON{MIME: "image/jpeg", Width: cfg.Width, Height: cfg.Height, Depth: jpegDepth(cfg.ColorModel)}
	case FormatPNG:
		cfg, err := png.DecodeConfig(c.File)
		if err != nil {
			return coverJSON{}, err
		}
		out = coverJSON{MIME: "image/png", Width: cfg.Width, Height: cfg.Height}
		var ihdr [26]byte // signature, IHDR length and type, width, height, bit depth, color type
		if _, err := c.File.ReadAt(ihdr[:], 0); err != nil {
			return coverJSON{}, err
		}
		out.Depth = pngDepth(ihdr[24], ihdr[25])
		if p, ok := cfg.ColorModel.(color.Palette); ok {
			out.Colors = len(p)
		}
	default:
		return coverJSON{}, errors.New("format " + strconv.Quote(c.Format) + " is not jpeg or png")
	}
	return out, nil
}

// jpegDepth is the bits per pixel of a JPEG as libFLAC computes them
// (share/grabbag/picture.c): sample precision times components. Go decodes
// only 8-bit precision; its color model gives the components.
func jpegDepth(m color.Model) int {
	switch m {
	case color.GrayModel:
		return 8
	case color.CMYKModel:
		return 32
	}
	return 24 // YCbCr
}

// pngDepth is the bits per pixel of a PNG as libFLAC computes them
// (share/grabbag/picture.c): the IHDR bit depth times the samples per
// pixel, and 24 for a palette image whatever its index depth (PNG §11.2.2:
// palette samples are 8 bits). image/png already validated the header.
func pngDepth(bitDepth, colorType byte) int {
	d := int(bitDepth)
	switch colorType {
	case 0: // grayscale
		return d
	case 2: // truecolor
		return 3 * d
	case 3: // palette
		return 24
	case 4: // grayscale and alpha
		return 2 * d
	case 6: // truecolor and alpha
		return 4 * d
	}
	return 0
}

// runTags runs one operation of the helper: req as JSON on its standard
// input, files as descriptors 3, 4, ..., its JSON result decoded strictly
// into out.
func (t *Tools) runTags(ctx context.Context, op string, req any, files []*os.File, out any) error {
	name := "musiclib-tags " + op
	body, err := json.Marshal(req)
	if err != nil {
		return newErr(CodeInvalidArgument, name, "encoding the request", err)
	}
	var stdout bytes.Buffer
	res, err := t.run.Run(ctx, Command{
		Path:        t.tags,
		Args:        []string{op},
		Files:       files,
		Stdin:       body,
		Stdout:      &stdout,
		StdoutLimit: tagsOutputLimit,
		Timeout:     t.tagsTimeoutOrDefault(),
	})
	if err != nil {
		return tagsFailure(err, name)
	}
	if err := decodeStrict(stdout.Bytes(), out); err != nil {
		return &Error{Code: CodeOutputInvalid, Op: name, Msg: "the result does not decode", Stderr: res.Stderr, Err: err}
	}
	return nil
}

func (t *Tools) tagsTimeoutOrDefault() time.Duration {
	if t.tagsTimeout > 0 {
		return t.tagsTimeout
	}
	return InspectTimeout
}

// tagsFailure turns the helper's typed failure (exit status 3 and
// {"code", "message"} on standard error) into the adapter's code. Any other
// failure (a crash, a signal, a timeout) keeps the Runner's code: standard
// error is read to classify a failure, never to declare success (§8.5).
func tagsFailure(err error, name string) error {
	var e *Error
	if !errors.As(err, &e) {
		return err
	}
	e.Op = name
	if e.Code != CodeToolFailed || e.ExitCode != tagsExitFailure {
		return err
	}
	var f struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if decodeStrict(e.Stderr, &f) != nil {
		return err
	}
	if code, ok := tagsCodes[f.Code]; ok {
		e.Code, e.Msg = code, f.Message
	}
	return err
}

// decodeStrict decodes exactly one JSON value into v: valid UTF-8 (the
// decoder would silently replace invalid bytes), no unknown field, nothing
// after the value.
func decodeStrict(data []byte, v any) error {
	if !utf8.Valid(data) {
		return errors.New("not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("data after the JSON value")
	}
	return nil
}

// readTagsVersion runs `musiclib-tags version` and returns the versions of
// the helper and of its TagLib.
func readTagsVersion(ctx context.Context, run *Runner, path string) (helper, taglib string, err error) {
	var out bytes.Buffer
	res, err := run.Run(ctx, Command{
		Path:        path,
		Args:        []string{"version"},
		Stdout:      &out,
		StdoutLimit: 1 << 16,
		Timeout:     InspectTimeout,
	})
	if err != nil {
		return "", "", unavailable(err, res)
	}
	var v struct {
		Helper string `json:"helper"`
		TagLib string `json:"taglib"`
	}
	if decodeStrict(out.Bytes(), &v) != nil || v.Helper == "" || v.TagLib == "" {
		return "", "", newErr(CodeToolUnavailable, "musiclib-tags", "unexpected version output: "+quoteShort(out.Bytes()), nil)
	}
	return v.Helper, v.TagLib, nil
}

// JoinValues is the single string of a multi-valued text field: its values
// in their original order, joined with "; " (§7.3).
func JoinValues(values []string) string { return strings.Join(values, "; ") }

// TagNumber parses a track or disc number or total as Inspect reports it:
// exactly one value made of ASCII digits, surrounding spaces allowed. ok is
// false for anything else ("A1", "3/12" was already split, several values).
// A number too large for the schema is returned as math.MaxInt32, never
// truncated, so that the caller refuses it (§7.3).
func TagNumber(values []string) (n int, ok bool) {
	if len(values) != 1 {
		return 0, false
	}
	s := strings.TrimSpace(values[0])
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	s = strings.TrimLeft(s, "0")
	if len(s) > 9 {
		return math.MaxInt32, true
	}
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// TagBool parses the compilation flag as Inspect reports it: "1" or "true"
// is true, "0" or "false" is false (ASCII case-insensitive, surrounding
// spaces allowed); ok is false for anything else or several values.
func TagBool(values []string) (v, ok bool) {
	if len(values) != 1 {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(values[0])) {
	case "1", "true":
		return true, true
	case "0", "false":
		return false, true
	}
	return false, false
}
