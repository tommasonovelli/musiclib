package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"musiclib/internal/media"
)

// Fixtures are generated at test time (DESIGN.md §12.1): short FLAC files
// by the pinned ffmpeg from lavfi sources, their metadata rewritten by the
// small FLAC codec below (RFC 9639 §8), written from the format and not
// from the helper, so that a test never trusts the code under test to build
// its input. Images come from Go's encoders. Nothing is committed.

// FLAC metadata block types.
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
// blocks and its audio frames.
type flacFile struct {
	prefix []byte
	blocks []flacBlock
	audio  []byte
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
	return out.Bytes()
}

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

var (
	baseMu    sync.Mutex
	baseCache = map[int]flacFile{}
)

// baseFLAC is a 0.2 s stereo FLAC of a sine at freq Hz with STREAMINFO only
// (and SEEKTABLE if ffmpeg wrote one): no tags, no pictures.
func baseFLAC(t testing.TB, freq int) flacFile {
	t.Helper()
	baseMu.Lock()
	defer baseMu.Unlock()
	if f, ok := baseCache[freq]; ok {
		return f
	}
	out := filepath.Join(t.TempDir(), "base.flac")
	ffmpeg(t, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=44100:duration=0.2", freq),
		"-ac", "2", "-c:a", "flac", out)
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
	baseCache[freq] = f
	return f
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

// vorbis builds a VORBIS_COMMENT block from raw entries ("KEY=value", or
// any bytes for hostile cases).
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

// picture builds a PICTURE block (RFC 9639 §8.8) of the given type around
// an image, with its real MIME type and dimensions.
func picture(t testing.TB, typ uint32, img []byte) flacBlock {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(img))
	mime := "image/" + format
	if err != nil {
		cfg, mime = image.Config{Width: 1, Height: 1}, "image/jpeg" // garbage declared as a JPEG
	}
	var b bytes.Buffer
	be := func(n int) { _ = binary.Write(&b, binary.BigEndian, uint32(n)) }
	be(int(typ))
	be(len(mime))
	b.WriteString(mime)
	be(0) // description
	be(cfg.Width)
	be(cfg.Height)
	be(24)
	be(0)
	be(len(img))
	b.Write(img)
	return flacBlock{typ: blockPicture, data: b.Bytes()}
}

// track describes a FLAC fixture.
type track struct {
	freq     int
	tags     []string
	pictures []flacBlock
	id3v2    bool
	id3v1    bool
}

// flac renders a track: the base audio, one comment block with the tags,
// the pictures, and optionally an ID3v2 tag before "fLaC" and an ID3v1 tag
// at the end.
func (tr track) flac(t testing.TB) []byte {
	t.Helper()
	freq := tr.freq
	if freq == 0 {
		freq = 440
	}
	f := baseFLAC(t, freq)
	f.blocks = append(append(append([]flacBlock(nil), f.blocks...), vorbis(tr.tags...)), tr.pictures...)
	if tr.id3v2 {
		f.prefix = id3v2Tag("An ID3 title")
	}
	b := f.bytes()
	if tr.id3v1 {
		v1 := make([]byte, 128)
		copy(v1, "TAGAn ID3v1 title")
		b = append(b, v1...)
	}
	return b
}

// id3v2Tag is a minimal ID3v2.4 tag with one TIT2 frame.
func id3v2Tag(title string) []byte {
	frame := append([]byte{3}, title...) // UTF-8 encoding byte
	var fb bytes.Buffer
	fb.WriteString("TIT2")
	fb.Write(syncsafe(len(frame)))
	fb.Write([]byte{0, 0})
	fb.Write(frame)
	var b bytes.Buffer
	b.WriteString("ID3")
	b.Write([]byte{4, 0, 0})
	b.Write(syncsafe(fb.Len()))
	b.Write(fb.Bytes())
	return b.Bytes()
}

func syncsafe(n int) []byte {
	return []byte{byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F)}
}

// pngImage is a deterministic PNG; seed changes its pixels.
func pngImage(t testing.TB, w, h int, seed byte) []byte {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range m.Pix {
		m.Pix[i] = byte(i)*7 + seed
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// jpegImage is a deterministic JPEG; seed changes its colour.
func jpegImage(t testing.TB, w, h int, seed byte) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			m.Set(x, y, color.RGBA{seed, byte(x), byte(y), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// rawPNG is an uncompressed noise PNG of w×h RGB pixels: its size is about
// h×(3w+1) bytes, so a test can reach an exact size range.
func rawPNG(t testing.TB, w, h int) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	x := uint32(12345)
	for i := range m.Pix {
		x = x*1664525 + 1013904223
		m.Pix[i] = byte(x >> 24)
		if i%4 == 3 {
			m.Pix[i] = 255 // opaque: encoded as RGB
		}
	}
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	if err := enc.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ---------------------------------------------------------------------------
// The source must never change (§3.2 guarantee 1).

// fileState is what a test compares of every source entry: content,
// inode, mode and mtime.
type fileState struct {
	hash  string
	ino   uint64
	mode  fs.FileMode
	mtime time.Time
}

// sourceState records every entry under dir without following symlinks
// and without opening anything but regular files.
func sourceState(t testing.TB, dir string) map[string]fileState {
	t.Helper()
	m := map[string]fileState{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		s := fileState{ino: st.Ino, mode: fi.Mode(), mtime: fi.ModTime()}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			s.hash = sha(b)
		}
		m[p] = s
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// diffStates lists the entries that differ, sorted.
func diffStates(a, b map[string]fileState) []string {
	var out []string
	for p, s := range a {
		if s2, ok := b[p]; !ok || s2 != s {
			out = append(out, p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func symlink(t testing.TB, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func readFile(t testing.TB, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t testing.TB, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// pngHeader is a PNG of w×h pixels made of its signature, IHDR and IEND
// only: DecodeConfig accepts it, a full decode does not.
func pngHeader(w, h int) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(typ string, data []byte) {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.WriteString(typ)
		b.Write(data)
		_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = 8, 2 // 8-bit RGB
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	return b.Bytes()
}
