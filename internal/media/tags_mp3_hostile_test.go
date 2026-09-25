package media

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Hostile MP3 input (DESIGN.md §12.1 "input corrotti"), on the release and
// the ASan/UBSan helper: every damaged structure is a typed refusal that
// leaves the file as it was, and a field that cannot be kept refuses the
// write (§8.3), never a silent loss.

type hostileMP3 struct {
	name    string
	data    []byte
	inspect string        // the code Inspect fails with, "" for success
	opaque  []OpaqueField // then the opaque fields it reports
	write   string        // the code WriteManagedTags fails with
}

func hostileMP3Files(t *testing.T, dir string) []hostileMP3 {
	src := mp3Source(t, dir)
	tag := id3Tag(4, 0, 0, frame("TIT2", textBody(encLatin1, "t")))
	ape := apeTag(true, apeItem("Mood", 0, []byte("calm")))
	apeFooterAt := len(ape) - 32
	withAPE := func(mut func([]byte)) []byte {
		a := bytes.Clone(ape)
		mut(a)
		return slices.Concat(src, a)
	}
	flac := readFile(t, gen(t, dir, "x.flac", append(lavfi(sine3s), "-c:a", "flac")...))
	corrupt := func(name string, data []byte) hostileMP3 {
		return hostileMP3{name: name, data: data, inspect: CodeTagsCorrupt, write: CodeTagsCorrupt}
	}
	mismatch := func(name string, data []byte) hostileMP3 {
		return hostileMP3{name: name, data: data, inspect: CodeTagsFormatMismatch, write: CodeTagsFormatMismatch}
	}
	blocking := func(name string, data []byte, key, reason string) hostileMP3 {
		return hostileMP3{name: name, data: data, opaque: []OpaqueField{{Key: key, Reason: reason}}, write: CodeTagsOpaque}
	}
	le := func(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
	return []hostileMP3{
		// The ID3v2 tag itself.
		corrupt("ID3v2 past the end of the file", slices.Concat([]byte{'I', 'D', '3', 4, 0, 0, 0x7F, 0x7F, 0x7F, 0x7F}, src)),
		corrupt("ID3v2 size not synchsafe", slices.Concat([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0x80, 0}, src)),
		corrupt("ID3v2 version 5", slices.Concat([]byte{'I', 'D', '3', 5, 0, 0, 0, 0, 0, 0}, src)),
		corrupt("ID3v2 version 0xFF", slices.Concat([]byte{'I', 'D', '3', 0xFF, 0, 0, 0, 0, 0, 0}, src)),
		corrupt("ID3v2.3 footer flag", slices.Concat([]byte{'I', 'D', '3', 3, 0, 0x10, 0, 0, 0, 0}, src)),
		corrupt("ID3v2.2 compression", slices.Concat([]byte{'I', 'D', '3', 2, 0, 0x40, 0, 0, 0, 0}, src)),
		corrupt("ID3v2.3 extended header of size 7", slices.Concat([]byte{'I', 'D', '3', 3, 0, 0x40}, synchsafe(11), be32(7), make([]byte, 7), src)),
		// The APE tag.
		corrupt("APE size under a footer", withAPE(func(a []byte) { le(a, apeFooterAt+12, 10) })),
		corrupt("APE larger than the file", withAPE(func(a []byte) { le(a, apeFooterAt+12, 1<<30) })),
		corrupt("APE version 3000", withAPE(func(a []byte) { le(a, apeFooterAt+8, 3000) })),
		corrupt("APE footer marked as header", withAPE(func(a []byte) { a[apeFooterAt+23] |= 0x20 })),
		corrupt("APE header of another size", withAPE(func(a []byte) { le(a, 12, 99) })),
		corrupt("APE header missing", withAPE(func(a []byte) { copy(a, "XXXXXXXX") })),
		corrupt("APE item past the tag", withAPE(func(a []byte) { le(a, 32, 1000) })),
		corrupt("APE more items than it holds", withAPE(func(a []byte) { le(a, apeFooterAt+16, 5); le(a, 16, 5) })),
		corrupt("APE 50001 items", withAPE(func(a []byte) { le(a, apeFooterAt+16, 50001); le(a, 16, 50001) })),
		corrupt("ID3v1 inside the ID3v2 tag", slices.Concat([]byte{'I', 'D', '3', 4, 0, 0}, synchsafe(len(src)+128), src, id3v1("", "", "", "", "", 0, 0))),
		// Not an MP3 stream.
		mismatch("a FLAC", flac),
		mismatch("text", []byte(strings.Repeat("not audio at all\n", 200))),
		mismatch("an ID3v2 tag alone", tag),
		mismatch("tags and no audio", slices.Concat(tag, ape, id3v1("x", "", "", "", "", 0, 0))),
		// Fields that cannot be kept.
		blocking("two ID3v2 tags", slices.Concat(tag, tag, src), "id3v2", "duplicate_tag"),
		blocking("Lyrics3 before ID3v1", slices.Concat(src, []byte("LYRICSBEGINxxLYRICS200"), id3v1("x", "", "", "", "", 0, 0)), "lyrics3", "foreign_tag"),
		blocking("ID3v2 appended at the end", slices.Concat(src, []byte{'3', 'D', 'I', 4, 0, 0, 0, 0, 0, 0}), "id3v2.appended", "foreign_tag"),
		blocking("APE key with a control character", slices.Concat(src, apeTag(true, apeItem("Bad\x01Key", 0, []byte("v")))), "ape#0", "invalid_key"),
		blocking("APE key TAG", slices.Concat(src, apeTag(true, apeItem("tag", 0, []byte("v")))), "ape#0", "invalid_key"),
		blocking("APE text not UTF-8", slices.Concat(src, apeTag(true, apeItem("Mood", 0, []byte{0xC3}))), "ape:MOOD", "invalid_utf8"),
		blocking("frame past the tag", slices.Concat([]byte{'I', 'D', '3', 4, 0, 0}, synchsafe(12), []byte("TCOM"), synchsafe(100),
			[]byte{0, 0, 0, 'c'}, src), "id3v2#0", "malformed_frame"),
	}
}

func TestTagsHostileMP3(t *testing.T) {
	dir := t.TempDir()
	files := hostileMP3Files(t, dir)
	for name, tools := range helpers(t) {
		for _, hf := range files {
			t.Run(name+"/"+hf.name, func(t *testing.T) {
				p := writeFile(t, filepath.Join(t.TempDir(), "f.mp3"), hf.data)
				in, err := tools.Inspect(t.Context(), open(t, p), FormatMP3)
				if hf.inspect != "" {
					wantCode(t, err, hf.inspect)
				} else {
					if err != nil {
						t.Fatalf("Inspect: %v", err)
					}
					if !slices.Equal(in.Opaque, hf.opaque) {
						t.Fatalf("opaque:\n got %+v\nwant %+v", in.Opaque, hf.opaque)
					}
				}
				wantUnchanged(t, p, func() error {
					return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatMP3, fullValues, nil)
				}, hf.write)
			})
		}
	}
}

// A mutation sweep over the tags of a rich MP3 (the hand-written parsers of
// ID3v2 and APE, as a fuzz harness would drive them; there is no fuzz
// harness for the native readers, NOTES.md N-156): 1 to 4 random bytes of
// the tags changed, 200 fixed seeds, on the ASan/UBSan helper. Every outcome
// must be a success or a typed refusal of the file; a refused write leaves
// the file unchanged; a successful write passes the §9.1 step 6 checks.
func TestTagsMP3MutationSweep(t *testing.T) {
	dir := t.TempDir()
	src := mp3Source(t, dir)
	managed, unmanaged := richV23()
	id3 := id3Tag(3, 0x80, 16, append(managed, unmanaged...)...)
	ape := apeTag(true, apeItem("Title", 0, []byte("ape")), apeItem("Mood", 0, []byte("calm")),
		apeItem("Cover Art (Front)", 2, []byte("c.png\x00png")))
	v1 := id3v1("v1", "a", "b", "1999", "comment", 3, 17)
	base := slices.Concat(id3, src, ape, v1)
	tail := len(ape) + len(v1)
	asan := helpers(t)["asan"]
	allowed := []string{CodeTagsCorrupt, CodeTagsFormatMismatch, CodeTagsOpaque}
	outcomes := map[string]int{}
	for seed := range uint64(200) {
		r := rand.New(rand.NewPCG(seed, 12))
		b := bytes.Clone(base)
		for range 1 + r.IntN(4) {
			i := r.IntN(len(id3) + tail)
			if i >= len(id3) {
				i = len(b) - tail + (i - len(id3))
			}
			b[i] = byte(r.IntN(256))
		}
		p := writeFile(t, filepath.Join(t.TempDir(), "m.mp3"), b)
		in, err := asan.Inspect(t.Context(), open(t, p), FormatMP3)
		if err != nil {
			if !slices.Contains(allowed, Code(err)) {
				t.Fatalf("seed %d: Inspect: %v", seed, err)
			}
			outcomes["inspect "+Code(err)]++
			continue
		}
		before := readFile(t, p)
		err = asan.WriteManagedTags(t.Context(), openRW(t, p), FormatMP3, fullValues, nil)
		if err != nil {
			if !slices.Contains(allowed, Code(err)) {
				t.Fatalf("seed %d: WriteManagedTags: %v", seed, err)
			}
			if !bytes.Equal(before, readFile(t, p)) {
				t.Fatalf("seed %d: a refused write changed the file", seed)
			}
			outcomes["write "+Code(err)]++
			continue
		}
		after, err := asan.Inspect(t.Context(), open(t, p), FormatMP3)
		if err != nil {
			t.Fatalf("seed %d: the written file does not inspect: %v", seed, err)
		}
		if err := VerifyTags(fullValues, nil, in, after); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		out := parseWritten(t, readFile(t, p))
		if !bytes.Equal(out.audio, before[in.Audio.Start:in.Audio.End]) {
			t.Fatalf("seed %d: the audio changed", seed)
		}
		outcomes["written"]++
	}
	t.Logf("outcomes of 200 mutations: %v", outcomes)
	if outcomes["written"] == 0 || len(outcomes) < 3 {
		t.Fatalf("the sweep does not exercise both paths: %v", outcomes)
	}
}
