package media

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"musiclib/internal/faulttest"
)

// Hostile M4A input (DESIGN.md §12.1 "input corrotti"), on the release and
// the ASan/UBSan helper: every damaged structure is a typed refusal that
// leaves the file as it was; what §8.1 does not support (fragments, DRM,
// more than one track, media data elsewhere) is unsupported_format; a
// metadata structure a write could not keep refuses the write (§8.3).

type hostileM4A struct {
	name    string
	data    []byte
	format  string
	inspect string        // the code Inspect fails with, "" for success
	opaque  []OpaqueField // then the opaque fields it reports
	write   string        // the code WriteManagedTags fails with
}

// patchAt returns b with v written big-endian at the offset of the n-th
// byte after the first occurrence of marker.
func patchAt(t testing.TB, b []byte, marker string, n int, v uint32) []byte {
	t.Helper()
	i := bytes.Index(b, []byte(marker))
	if i < 0 {
		t.Fatalf("no %q in the fixture", marker)
	}
	out := bytes.Clone(b)
	binary.BigEndian.PutUint32(out[i+n:], v)
	return out
}

// replaceOnce returns b with the first occurrence of old replaced.
func replaceOnce(t testing.TB, b []byte, old, repl string) []byte {
	t.Helper()
	if !bytes.Contains(b, []byte(old)) {
		t.Fatalf("no %q in the fixture", old)
	}
	return bytes.Replace(b, []byte(old), []byte(repl), 1)
}

// withSampleEntryChild adds a child box to the sample description of the
// track.
func withSampleEntryChild(t testing.TB, file, child []byte) []byte {
	t.Helper()
	return mp4RewriteMoov(t, file, func(moov []mp4Node) []mp4Node {
		return mp4Edit(t, moov, stblPath, func(stbl []mp4Node) []mp4Node {
			for i, n := range stbl {
				if n.typ != "stsd" {
					continue
				}
				entries := mp4Children(t, n.payload[8:])
				entries[0].payload = append(entries[0].payload, child...)
				stbl[i].payload = slices.Concat(n.payload[:8], mp4Render(entries))
			}
			return stbl
		})
	})
}

// largeSampleEntry replaces the audio description with a 64-bit box and a
// protection child, to ensure its wider header cannot hide sinf.
func largeSampleEntry(t testing.TB, file []byte) []byte {
	t.Helper()
	return mp4RewriteMoov(t, file, func(moov []mp4Node) []mp4Node {
		return mp4Edit(t, moov, stblPath, func(stbl []mp4Node) []mp4Node {
			for i, n := range stbl {
				if n.typ == "stsd" {
					entry := mp4Children(t, n.payload[8:])[0]
					stbl[i].payload = slices.Concat(n.payload[:8], mp4Box64(entry.typ, entry.payload, mp4Box("sinf", mp4Box("frma", []byte("alac")))))
				}
			}
			return stbl
		})
	})
}

func hostileM4AFiles(t *testing.T, dir string) []hostileM4A {
	src := m4aSource(t, dir, "alac", true)
	base := withIlst(t, src, 0, textItem(atomNam, "t"))
	wav := wav16(t, dir, "h.wav", sine3s)
	aac := func(name string, args ...string) []byte {
		return readFile(t, gen(t, dir, name, append([]string{"-i", wav, "-c:a", "aac"}, args...)...))
	}
	flac := readFile(t, gen(t, dir, "x.flac", append(lavfi(sine3s), "-c:a", "flac")...))
	moovAt := bytes.Index(base, []byte("moov")) - 4
	top := mp4Children(t, base)
	var noMoov []byte
	for _, n := range top {
		if n.typ != "moov" {
			noMoov = append(noMoov, n.bytes()...)
		}
	}
	moovOf := func(file []byte) []byte {
		for _, n := range mp4Children(t, file) {
			if n.typ == "moov" {
				return n.bytes()
			}
		}
		return nil
	}
	editMoov := func(fn func([]mp4Node) []mp4Node) []byte { return mp4RewriteMoov(t, base, fn) }
	editMeta := func(fn func([]mp4Node) []mp4Node) []byte {
		return editMoov(func(moov []mp4Node) []mp4Node { return mp4Edit(t, moov, []string{"udta", "meta"}, fn) })
	}
	h := func(name string, data []byte, code string) hostileM4A {
		return hostileM4A{name: name, data: data, format: FormatM4AALAC, inspect: code, write: code}
	}
	blocking := func(name string, data []byte, key, reason string) hostileM4A {
		return hostileM4A{name: name, data: data, format: FormatM4AALAC, opaque: []OpaqueField{{Key: key, Reason: reason}}, write: CodeTagsOpaque}
	}
	return []hostileM4A{
		// Damaged structures.
		h("moov past the end of the file", patchAt(t, base, "moov", -4, uint32(len(base))), CodeTagsCorrupt),
		h("a truncated file", base[:len(base)-10], CodeTagsCorrupt),
		h("bytes after the last box", append(bytes.Clone(base), 0, 0, 0), CodeTagsCorrupt),
		h("two moov boxes", slices.Concat(base, moovOf(base)), CodeTagsCorrupt),
		h("no moov box", noMoov, CodeTagsCorrupt),
		h("a box of size 3 in moov", patchAt(t, base, "mvhd", -4, 3), CodeTagsCorrupt),
		h("a box past moov", patchAt(t, base, "udta", -4, uint32(len(base))), CodeTagsCorrupt),
		h("an item past the ilst", patchAt(t, base, atomNam, -4, 1000), CodeTagsCorrupt),
		h("stsc not from chunk 1", patchAt(t, base, "stsc", 12, 2), CodeTagsCorrupt),
		h("stsc with sample description 2", patchAt(t, base, "stsc", 20, 2), CodeTagsCorrupt),
		h("stsz declaring more samples", patchAt(t, base, "stsz", 12, 1<<20), CodeTagsCorrupt),
		h("stco declaring more chunks", patchAt(t, base, "stco", 8, 1<<20), CodeTagsCorrupt),
		h("a chunk outside the media data", patchAt(t, base, "stco", 12, uint32(moovAt)), CodeTagsCorrupt),
		h("a chunk past the end of the file", patchAt(t, base, "stco", 12, uint32(len(base)-4)), CodeTagsCorrupt),
		h("no stbl", replaceOnce(t, base, "stbl", "xtbl"), CodeTagsCorrupt),
		h("QuickTime udta zero terminator", editMoov(func(moov []mp4Node) []mp4Node {
			for i := range moov {
				if moov[i].typ == "udta" {
					moov[i].payload = append(moov[i].payload, 0, 0, 0, 0)
				}
			}
			return moov
		}), CodeTagsCorrupt),
		h("no audio samples", patchAt(t, patchAt(t, patchAt(t, base, "stsc", 8, 0), "stsz", 12, 0), "stco", 8, 0), CodeTagsCorrupt),
		// Not an M4A of the declared codec.
		h("a FLAC", flac, CodeTagsFormatMismatch),
		h("text", []byte(strings.Repeat("not audio at all\n", 200)), CodeTagsFormatMismatch),
		h("a tiny file", []byte("ftyp"), CodeTagsFormatMismatch),
		{name: "ALAC declared as AAC", data: base, format: FormatM4AAAC, inspect: CodeTagsFormatMismatch, write: CodeTagsFormatMismatch},
		{name: "AAC declared as ALAC", data: aac("a.m4a"), format: FormatM4AALAC, inspect: CodeTagsFormatMismatch, write: CodeTagsFormatMismatch},
		// What §8.1 does not support (N-165).
		{name: "fragmented", data: aac("frag.m4a", "-movflags", "frag_keyframe+empty_moov"), format: FormatM4AAAC,
			inspect: CodeTagsUnsupported, write: CodeTagsUnsupported},
		{name: "two audio tracks", data: aac("two.m4a", "-map", "0", "-map", "0"), format: FormatM4AAAC,
			inspect: CodeTagsUnsupported, write: CodeTagsUnsupported},
		{name: "a video track", data: readFile(t, gen(t, dir, "v.mp4", append(lavfi("testsrc=size=32x32:rate=5:duration=1"), "-i", wav,
			"-c:v", "mpeg4", "-c:a", "aac")...)), format: FormatM4AAAC, inspect: CodeTagsUnsupported, write: CodeTagsUnsupported},
		{name: "CENC encryption", data: aac("cenc.m4a", "-encryption_scheme", "cenc-aes-ctr", "-encryption_key",
			"00112233445566778899aabbccddeeff", "-encryption_kid", "00112233445566778899aabbccddeeff"),
			format: FormatM4AAAC, inspect: CodeTagsUnsupported, write: CodeTagsUnsupported},
		h("FairPlay (drms)", replaceOnce(t, base, "alac", "drms"), CodeTagsUnsupported),
		h("a protection scheme in the sample description", withSampleEntryChild(t, base, mp4Box("sinf", mp4Box("frma", []byte("alac")))), CodeTagsUnsupported),
		h("a protection scheme in a 64-bit sample description", largeSampleEntry(t, base), CodeTagsUnsupported),
		h("sample description data reference 2", patchAt(t, base, "alac", 10, 0x00020000), CodeTagsUnsupported),
		h("a protection system header", editMoov(func(moov []mp4Node) []mp4Node {
			return append(moov, mp4Node{typ: "pssh", payload: make([]byte, 24)})
		}), CodeTagsUnsupported),
		h("media data in another file", patchAt(t, base, "url ", 4, 0), CodeTagsUnsupported),
		h("compact sample sizes", replaceOnce(t, base, "stsz", "stz2"), CodeTagsUnsupported),
		// Metadata structures a write could not keep.
		blocking("a meta that is not iTunes metadata", replaceOnce(t, base, "mdir", "mdta"), "mp4:moov/udta/meta", "foreign_metadata"),
		blocking("a meta without a handler", editMeta(func(meta []mp4Node) []mp4Node {
			return slices.DeleteFunc(meta, func(n mp4Node) bool { return n.typ == "hdlr" })
		}), "mp4:moov/udta/meta", "foreign_metadata"),
		blocking("two udta", editMoov(func(moov []mp4Node) []mp4Node { return append(moov, mp4Node{typ: "udta"}) }), "mp4:moov/udta", "duplicate_block"),
		blocking("two meta", editMoov(func(moov []mp4Node) []mp4Node {
			return mp4Edit(t, moov, []string{"udta"}, func(udta []mp4Node) []mp4Node { return append(udta, udta...) })
		}), "mp4:moov/udta/meta", "duplicate_block"),
		blocking("two ilst", editMeta(func(meta []mp4Node) []mp4Node { return append(meta, meta[len(meta)-1]) }),
			"mp4:moov/udta/meta/ilst", "duplicate_block"),
	}
}

func TestTagsHostileM4A(t *testing.T) {
	dir := t.TempDir()
	files := hostileM4AFiles(t, dir)
	for name, tools := range helpers(t) {
		for _, hf := range files {
			t.Run(name+"/"+hf.name, func(t *testing.T) {
				p := writeFile(t, filepath.Join(t.TempDir(), "f.m4a"), hf.data)
				in, err := tools.Inspect(t.Context(), open(t, p), hf.format)
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
					return tools.WriteManagedTags(t.Context(), openRW(t, p), hf.format, fullValues, nil)
				}, hf.write)
			})
		}
	}
}

// A moov larger than the helper's bound is refused before it is read
// (N-166): a sparse file of 300 MiB whose moov fills it.
func TestTagsM4AMoovBound(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.m4a")
	head := slices.Concat(mp4Box("ftyp", []byte("M4A "), be32(0)), be32(300<<20), []byte("moov"))
	if err := os.WriteFile(p, head, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, int64(len(head))-8+300<<20); err != nil {
		t.Fatal(err)
	}
	for name, tools := range helpers(t) {
		t.Run(name, func(t *testing.T) {
			_, err := tools.Inspect(t.Context(), open(t, p), FormatM4AAAC)
			wantCode(t, err, CodeTagsTooLarge)
			wantUnchanged(t, p, func() error {
				return tools.WriteManagedTags(t.Context(), openRW(t, p), FormatM4AAAC, fullValues, nil)
			}, CodeTagsTooLarge)
		})
	}
}

// A mutation sweep over the moov of a richly tagged M4A (the hand-written
// box walker, sample table and ilst reader, as a fuzz harness would drive
// them; NOTES.md N-170): 1 to 4 random bytes of moov changed, 200 fixed
// seeds, on the ASan/UBSan helper. Every outcome must be a success or a
// typed refusal of the file; a refused write leaves the file unchanged; a
// successful write passes the §9.1 step 6 tag check (the samples included,
// through the unmanaged "mp4.samples").
func TestTagsM4AMutationSweep(t *testing.T) {
	dir := t.TempDir()
	items, _, _ := richItems(t, dir)
	base := withIlst(t, m4aSource(t, dir, "alac", true), 0, items...)
	moovAt := bytes.Index(base, []byte("moov")) - 4
	moovLen := int(binary.BigEndian.Uint32(base[moovAt:]))
	asan := helpers(t)["asan"]
	allowed := []string{CodeTagsCorrupt, CodeTagsFormatMismatch, CodeTagsOpaque, CodeTagsUnsupported, CodeTagsTooLarge}
	outcomes := map[string]int{}
	for seed := range uint64(200) {
		r := rand.New(rand.NewPCG(seed, 13))
		b := bytes.Clone(base)
		for range 1 + r.IntN(4) {
			b[moovAt+r.IntN(moovLen)] = byte(r.IntN(256))
		}
		p := writeFile(t, filepath.Join(t.TempDir(), "m.m4a"), b)
		in, err := asan.Inspect(t.Context(), open(t, p), FormatM4AALAC)
		if err != nil {
			if !slices.Contains(allowed, Code(err)) {
				t.Fatalf("seed %d: Inspect: %v", seed, err)
			}
			outcomes["inspect "+Code(err)]++
			continue
		}
		before := readFile(t, p)
		err = asan.WriteManagedTags(t.Context(), openRW(t, p), FormatM4AALAC, fullValues, nil)
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
		after, err := asan.Inspect(t.Context(), open(t, p), FormatM4AALAC)
		if err != nil {
			t.Fatalf("seed %d: the written file does not inspect: %v", seed, err)
		}
		if err := VerifyTags(fullValues, nil, in, after); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		outcomes["written"]++
	}
	t.Logf("outcomes of 200 mutations: %v", outcomes)
	if outcomes["written"] == 0 || len(outcomes) < 3 {
		t.Fatalf("the sweep does not exercise both paths: %v", outcomes)
	}
}

// N-143 for M4A: a write that grows moov before the media data on a full
// filesystem fails with no_space and the kernel's message; with room, it
// succeeds.
func TestTagsM4ANoSpace(t *testing.T) {
	d := faulttest.FullFS(t)
	dir := t.TempDir()
	src := m4aSource(t, dir, "alac", true)
	cover := &coverFile{writeFile(t, filepath.Join(dir, "c.png"), append(pngImage(t, 4, 4, 5), make([]byte, 50000)...)), FormatPNG}
	tools := newTools(t)
	p := filepath.Join(d.Dir, "f.m4a")
	if err := os.WriteFile(p, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Fill(0); err != nil {
		t.Fatal(err)
	}
	err := tools.WriteManagedTags(t.Context(), openRW(t, p), FormatM4AALAC, fullValues, cover.cover(t))
	e := wantCode(t, err, CodeTagsNoSpace)
	if !strings.Contains(e.Msg, "No space left on device") {
		t.Fatalf("the kernel's message is lost: %q", e.Msg)
	}
	if err := d.Drain(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, src, 0o644); err != nil {
		t.Fatal(err)
	}
	writeM4AChecked(t, tools, p, FormatM4AALAC, fullValues, cover)
}
