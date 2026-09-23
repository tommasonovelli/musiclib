package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"musiclib/internal/media"
)

// Fixtures are generated at test time (DESIGN.md §12.1): FLAC audio by the
// pinned ffmpeg from lavfi sources, its metadata written by the small FLAC
// codec below (RFC 9639 §8), from the format and not from the helper, so
// that a test never trusts the code under test to build its input. Images
// come from Go's encoders. Nothing is committed.

const (
	blockStreamInfo = 0
	blockVorbis     = 4
	blockPicture    = 6
)

type flacBlock struct {
	typ  byte
	data []byte
}

// flacFile is a FLAC file: an optional prefix (an ID3v2 tag), its metadata
// blocks, its audio frames and an optional suffix (an ID3v1 tag).
type flacFile struct {
	prefix []byte
	blocks []flacBlock
	audio  []byte
	suffix []byte
}

func (f flacFile) bytes() []byte {
	var out bytes.Buffer
	out.Write(f.prefix)
	out.WriteString("fLaC")
	for i, b := range f.blocks {
		h := b.typ
		if i == len(f.blocks)-1 {
			h |= 0x80
		}
		n := len(b.data)
		out.Write([]byte{h, byte(n >> 16), byte(n >> 8), byte(n)})
		out.Write(b.data)
	}
	out.Write(f.audio)
	out.Write(f.suffix)
	return out.Bytes()
}

// parseFLAC splits a FLAC file without prefix or suffix.
func parseFLAC(t testing.TB, b []byte) flacFile {
	t.Helper()
	if !bytes.HasPrefix(b, []byte("fLaC")) {
		t.Fatal("not a FLAC file")
	}
	var f flacFile
	pos := 4
	for {
		h := b[pos]
		n := int(b[pos+1])<<16 | int(b[pos+2])<<8 | int(b[pos+3])
		pos += 4
		f.blocks = append(f.blocks, flacBlock{typ: h & 0x7F, data: bytes.Clone(b[pos : pos+n])})
		pos += n
		if h&0x80 != 0 {
			break
		}
	}
	f.audio = bytes.Clone(b[pos:])
	return f
}

func (f flacFile) without(typ byte) flacFile {
	var keep []flacBlock
	for _, b := range f.blocks {
		if b.typ != typ {
			keep = append(keep, b)
		}
	}
	f.blocks = keep
	return f
}

var (
	baseMu    sync.Mutex
	baseCache = map[string]flacFile{}
)

// baseFLAC is a stereo FLAC of the lavfi source src (for example
// "sine=frequency=440:duration=0.3"), with STREAMINFO (and SEEKTABLE if
// ffmpeg wrote one) only.
func baseFLAC(t testing.TB, src string) flacFile {
	t.Helper()
	baseMu.Lock()
	defer baseMu.Unlock()
	if f, ok := baseCache[src]; ok {
		return f
	}
	out := filepath.Join(t.TempDir(), "base.flac")
	ffmpeg(t, "-f", "lavfi", "-i", src+":sample_rate=44100", "-ac", "2", "-c:a", "flac", out)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	f := parseFLAC(t, b)
	var keep []flacBlock
	for _, blk := range f.blocks {
		if blk.typ == blockStreamInfo || blk.typ == 3 {
			keep = append(keep, blk)
		}
	}
	f.blocks = keep
	baseCache[src] = f
	return f
}

func sine(freq int, seconds float64) string {
	return fmt.Sprintf("sine=frequency=%d:duration=%g", freq, seconds)
}

func ffmpeg(t testing.TB, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	full := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, args...)
	if out, err := exec.CommandContext(ctx, media.FFmpegPath, full...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// vorbis builds a VORBIS_COMMENT block from "KEY=value" entries.
func vorbis(entries ...string) flacBlock {
	var b bytes.Buffer
	le := func(n int) { _ = binary.Write(&b, binary.LittleEndian, uint32(n)) }
	vendor := "musiclib fixture"
	le(len(vendor))
	b.WriteString(vendor)
	le(len(entries))
	for _, e := range entries {
		le(len(e))
		b.WriteString(e)
	}
	return flacBlock{typ: blockVorbis, data: b.Bytes()}
}

// comments reads the entries of a VORBIS_COMMENT block.
func comments(t testing.TB, blk flacBlock) (vendor string, entries []string) {
	t.Helper()
	d := blk.data
	u32 := func() int {
		n := int(binary.LittleEndian.Uint32(d))
		d = d[4:]
		return n
	}
	n := u32()
	vendor, d = string(d[:n]), d[n:]
	for range u32() {
		n := u32()
		entries, d = append(entries, string(d[:n])), d[n:]
	}
	return vendor, entries
}

// picture builds a PICTURE block of the given type around an image.
func picture(t testing.TB, typ uint32, img []byte) flacBlock {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	mime := "image/" + format
	var b bytes.Buffer
	be := func(n int) { _ = binary.Write(&b, binary.BigEndian, uint32(n)) }
	be(int(typ))
	be(len(mime))
	b.WriteString(mime)
	be(0)
	be(cfg.Width)
	be(cfg.Height)
	be(24)
	be(0)
	be(len(img))
	b.Write(img)
	return flacBlock{typ: blockPicture, data: b.Bytes()}
}

// song is a FLAC fixture: audio from src, the given tags and pictures, and
// optionally an ID3v2 tag before "fLaC" and an ID3v1 tag at the end.
type song struct {
	src      string
	tags     []string
	pictures []flacBlock
	id3v2    bool
	id3v1    bool
}

func (s song) flac(t testing.TB) []byte {
	t.Helper()
	src := s.src
	if src == "" {
		src = sine(440, 0.3)
	}
	f := baseFLAC(t, src)
	f.blocks = append(append(append([]flacBlock(nil), f.blocks...), vorbis(s.tags...)), s.pictures...)
	if s.id3v2 {
		f.prefix = id3v2Tag("An ID3 title")
	}
	if s.id3v1 {
		f.suffix = id3v1Tag("An ID3v1 title")
	}
	return f.bytes()
}

// id3v2Tag is an ID3v2.3 tag with one TIT2 frame.
func id3v2Tag(title string) []byte {
	frame := append([]byte("TIT2"), 0, 0, 0, byte(len(title)+1), 0, 0, 0)
	frame = append(frame, title...)
	size := len(frame)
	h := []byte{'I', 'D', '3', 3, 0, 0, byte(size >> 21 & 0x7f), byte(size >> 14 & 0x7f), byte(size >> 7 & 0x7f), byte(size & 0x7f)}
	return append(h, frame...)
}

// id3v1Tag is a 128-byte ID3v1 tag.
func id3v1Tag(title string) []byte {
	b := make([]byte, 128)
	copy(b, "TAG")
	copy(b[3:33], title)
	b[127] = 255
	return b
}

func jpegImage(t testing.TB, w, h int, seed uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x*7) + seed, G: uint8(y * 5), B: seed, A: 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func pngImage(t testing.TB, w, h int, seed uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{R: uint8(x) + seed, G: uint8(y), B: seed, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
