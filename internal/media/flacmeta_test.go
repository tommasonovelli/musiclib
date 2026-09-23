package media

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
)

// An independent FLAC metadata codec for the tests (RFC 9639 §8), written
// from the format and not from TagLib: it builds fixtures that no encoder
// writes (aliases, multi-valued fields, several pictures, hostile blocks)
// and reads back what the helper wrote, so that a test never trusts the
// helper to check itself.

// FLAC metadata block types.
const (
	flacStreamInfo    = 0
	flacPadding       = 1
	flacApplication   = 2
	flacSeekTable     = 3
	flacVorbisComment = 4
	flacCueSheet      = 5
	flacPicture       = 6
)

type flacBlock struct {
	typ  byte
	data []byte
}

// flacFile is a FLAC file split into its metadata blocks and the rest (the
// audio frames and anything after them). prefix is written before "fLaC".
type flacFile struct {
	prefix []byte
	blocks []flacBlock
	audio  []byte
}

// parseFLAC splits a well-formed FLAC file that starts with "fLaC".
func parseFLAC(t testing.TB, b []byte) flacFile {
	t.Helper()
	if !bytes.HasPrefix(b, []byte("fLaC")) {
		t.Fatal("not a FLAC file")
	}
	var f flacFile
	pos := 4
	for {
		if len(b)-pos < 4 {
			t.Fatal("truncated block header")
		}
		h := b[pos]
		n := int(b[pos+1])<<16 | int(b[pos+2])<<8 | int(b[pos+3])
		pos += 4
		if len(b)-pos < n {
			t.Fatal("truncated block")
		}
		f.blocks = append(f.blocks, flacBlock{typ: h & 0x7F, data: bytes.Clone(b[pos : pos+n])})
		pos += n
		if h&0x80 != 0 {
			break
		}
	}
	f.audio = bytes.Clone(b[pos:])
	return f
}

// bytes renders the file, with the last-block flag on the last block.
func (f flacFile) bytes() []byte {
	var out bytes.Buffer
	out.Write(f.prefix)
	out.WriteString("fLaC")
	for i, blk := range f.blocks {
		h := blk.typ
		if i == len(f.blocks)-1 {
			h |= 0x80
		}
		n := len(blk.data)
		out.Write([]byte{h, byte(n >> 16), byte(n >> 8), byte(n)})
		out.Write(blk.data)
	}
	out.Write(f.audio)
	return out.Bytes()
}

// without returns a copy without the blocks of the given types.
func (f flacFile) without(types ...byte) flacFile {
	out := flacFile{prefix: f.prefix, audio: f.audio}
	for _, blk := range f.blocks {
		drop := false
		for _, ty := range types {
			drop = drop || blk.typ == ty
		}
		if !drop {
			out.blocks = append(out.blocks, blk)
		}
	}
	return out
}

// with returns a copy with the blocks appended.
func (f flacFile) with(blocks ...flacBlock) flacFile {
	out := f
	out.blocks = append(append([]flacBlock(nil), f.blocks...), blocks...)
	return out
}

// ofType returns the blocks of one type, in order.
func (f flacFile) ofType(ty byte) []flacBlock {
	var out []flacBlock
	for _, blk := range f.blocks {
		if blk.typ == ty {
			out = append(out, blk)
		}
	}
	return out
}

// vorbisBlock builds a VORBIS_COMMENT block from raw entries ("KEY=value",
// or anything else for hostile cases).
func vorbisBlock(vendor string, entries ...string) flacBlock {
	var b bytes.Buffer
	le := func(n int) { _ = binary.Write(&b, binary.LittleEndian, uint32(n)) }
	le(len(vendor))
	b.WriteString(vendor)
	le(len(entries))
	for _, e := range entries {
		le(len(e))
		b.WriteString(e)
	}
	return flacBlock{typ: flacVorbisComment, data: b.Bytes()}
}

// comment is a decoded Vorbis comment: the vendor and the raw entries.
type comment struct {
	vendor  string
	entries []string
}

// parseVorbis decodes a well-formed VORBIS_COMMENT block.
func parseVorbis(t testing.TB, d []byte) comment {
	t.Helper()
	r := bytes.NewReader(d)
	u32 := func() int {
		var n uint32
		if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
			t.Fatal(err)
		}
		return int(n)
	}
	str := func(n int) string {
		b := make([]byte, n)
		if _, err := r.Read(b); err != nil && n > 0 {
			t.Fatal(err)
		}
		return string(b)
	}
	var c comment
	c.vendor = str(u32())
	for n := u32(); n > 0; n-- {
		c.entries = append(c.entries, str(u32()))
	}
	return c
}

// keys returns the upper-case keys of the entries.
func (c comment) keys() []string {
	var out []string
	for _, e := range c.entries {
		k, _, _ := strings.Cut(e, "=")
		out = append(out, strings.ToUpper(k))
	}
	return out
}

// values returns the values of one key (compared in upper case), in order.
func (c comment) values(key string) []string {
	var out []string
	for _, e := range c.entries {
		k, v, _ := strings.Cut(e, "=")
		if strings.EqualFold(k, key) {
			out = append(out, v)
		}
	}
	return out
}

// the written file's only Vorbis comment.
func onlyComment(t testing.TB, f flacFile) comment {
	t.Helper()
	blocks := f.ofType(flacVorbisComment)
	if len(blocks) != 1 {
		t.Fatalf("%d Vorbis comment blocks, want 1", len(blocks))
	}
	return parseVorbis(t, blocks[0].data)
}

// flacPictureData is the FLAC picture structure (RFC 9639 §8.8).
type flacPictureData struct {
	typ                          uint32
	mime, desc                   string
	width, height, depth, colors uint32
	data                         []byte
}

func (p flacPictureData) bytes() []byte {
	var b bytes.Buffer
	be := func(n uint32) { _ = binary.Write(&b, binary.BigEndian, n) }
	be(p.typ)
	be(uint32(len(p.mime)))
	b.WriteString(p.mime)
	be(uint32(len(p.desc)))
	b.WriteString(p.desc)
	be(p.width)
	be(p.height)
	be(p.depth)
	be(p.colors)
	be(uint32(len(p.data)))
	b.Write(p.data)
	return b.Bytes()
}

func (p flacPictureData) block() flacBlock { return flacBlock{typ: flacPicture, data: p.bytes()} }

// entry is the picture as a METADATA_BLOCK_PICTURE Vorbis entry.
func (p flacPictureData) entry() string {
	return "METADATA_BLOCK_PICTURE=" + base64.StdEncoding.EncodeToString(p.bytes())
}

func parsePictureBlock(t testing.TB, d []byte) flacPictureData {
	t.Helper()
	r := bytes.NewReader(d)
	u32 := func() uint32 {
		var n uint32
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	str := func(n uint32) []byte {
		b := make([]byte, n)
		if _, err := r.Read(b); err != nil && n > 0 {
			t.Fatal(err)
		}
		return b
	}
	var p flacPictureData
	p.typ = u32()
	p.mime = string(str(u32()))
	p.desc = string(str(u32()))
	p.width, p.height, p.depth, p.colors = u32(), u32(), u32(), u32()
	p.data = str(u32())
	return p
}

// baseFLAC generates a FLAC file of the given source with the pinned ffmpeg
// and strips every metadata block but STREAMINFO and SEEKTABLE: a file with
// no tag at all, to which a test adds exactly the metadata it needs.
func baseFLAC(t testing.TB, dir, name, src string) flacFile {
	t.Helper()
	p := gen(t, dir, name, append(lavfi(src), "-ac", "2", "-c:a", "flac")...)
	return parseFLAC(t, readFile(t, p)).without(flacPadding, flacVorbisComment, flacPicture, flacApplication)
}

// pngImage encodes a small deterministic PNG with Go's image/png (the
// pinned ffmpeg has no PNG encoder, N-073). seed changes the pixels.
func pngImage(t testing.TB, w, h int, seed byte) []byte {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range m.Pix {
		m.Pix[i] = byte(i)*7 + seed
	}
	m.Set(0, 0, color.NRGBA{seed, 2, 3, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// jpegImage generates a small JPEG with the pinned ffmpeg.
func jpegImage(t testing.TB, dir, name, colour string) []byte {
	t.Helper()
	return readFile(t, gen(t, dir, name, append(lavfi("color=c="+colour+":size=24x16"), "-frames:v", "1")...))
}

// writeFLAC writes f to dir/name and returns the path.
func writeFLAC(t testing.TB, dir, name string, f flacFile) string {
	t.Helper()
	return writeFile(t, filepath.Join(dir, name), f.bytes())
}
