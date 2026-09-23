package media

import (
	"bytes"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// NOTES.md N-090 (owner decision, 2026-09-23): ID3v2 and ID3v1 tags inside
// a FLAC file are not part of the format, and a write strips them by a
// declared rule, an exception to §8.3 like the ID3v1 migration of MP3. The
// inspection reports them as removed opaque fields (never Blocking), and
// everything else of the file is kept as §8.2 and §8.3 say.

// synchsafe is an ID3v2 28-bit synchsafe integer.
func synchsafe(n int) []byte {
	return []byte{byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F)}
}

// id3v2Frame is an ID3v2.4 frame (synchsafe size, no flags).
func id3v2Frame(id string, body []byte) []byte {
	return slices.Concat([]byte(id), synchsafe(len(body)), []byte{0, 0}, body)
}

// id3v2With is an ID3v2.4 tag with the given flags and body; flag 0x10
// appends the footer.
func id3v2With(flags byte, body []byte) []byte {
	head := slices.Concat([]byte{'I', 'D', '3', 4, 0, flags}, synchsafe(len(body)))
	out := slices.Concat(head, body)
	if flags&0x10 != 0 {
		out = slices.Concat(out, []byte("3DI"), head[3:])
	}
	return out
}

// richID3v2 carries frames with a Vorbis equivalent (TIT2, TPE1) and frames
// without one that the output does not keep either (TXXX, PRIV, APIC:
// NOTES.md N-090 lists what is lost), then padding.
func richID3v2(t testing.TB, padding int) []byte {
	apic := slices.Concat([]byte{0}, []byte("image/png\x00"), []byte{3}, []byte("cover\x00"), pngImage(t, 4, 4, 3))
	return id3v2With(0, slices.Concat(
		id3v2Frame("TIT2", []byte("\x03An ID3 title")),
		id3v2Frame("TPE1", []byte("\x03An ID3 artist")),
		id3v2Frame("TXXX", []byte("\x03MY KEY\x00my value")),
		id3v2Frame("PRIV", []byte("owner\x00opaque bytes")),
		id3v2Frame("APIC", apic),
		make([]byte, padding),
	))
}

// id3Case is a FLAC file with ID3 tags and the opaque fields its
// inspection must report.
type id3Case struct {
	name   string
	prefix []byte
	suffix []byte
	opaque []OpaqueField
}

func id3Cases(t testing.TB) []id3Case {
	v2 := OpaqueField{Key: "id3v2", Reason: "foreign_tag", Removed: true}
	v1 := OpaqueField{Key: "id3v1", Reason: "foreign_tag", Removed: true}
	return []id3Case{
		{"leading ID3v2", richID3v2(t, 64), nil, []OpaqueField{v2}},
		{"trailing ID3v1", nil, id3v1Tag(), []OpaqueField{v1}},
		{"both", richID3v2(t, 64), id3v1Tag(), []OpaqueField{v2, v1}},
		{"ID3v2 with a footer", id3v2With(0x10, id3v2Frame("TIT2", []byte("\x03x"))), nil, []OpaqueField{v2}},
		{"ID3v2 of size 0", id3v2With(0, nil), nil, []OpaqueField{v2}},
		// TagLib parses the body (unsynchronisation, extended header,
		// frames) from garbage: under ASan too.
		{"ID3v2 with a garbage body", id3v2With(0xC0, garbage(11, 4096)), nil, []OpaqueField{v2}},
		{"ID3v2 of garbage frames", id3v2With(0, slices.Concat(id3v2Frame("TIT2", garbage(12, 300)),
			[]byte("TPE1\x7f\x7f\x7f\x7f\x00\x00"), garbage(13, 100))), nil, []OpaqueField{v2}},
		// About 800 KB of ID3v2: more than 1% of the file, so the first save
		// chooses the padding against a length the output does not have
		// (the second save of writeFlac).
		{"large ID3v2", richID3v2(t, 800<<10), nil, []OpaqueField{v2}},
	}
}

// writeStripping is writeChecked for a file with ID3 tags: the audio digest
// of the output is compared with d0, the one of the same file without its
// ID3 tags, which is also the input's (N-128).
func writeStripping(t testing.TB, tools *Tools, path string, v TagValues, cover *coverFile, d0 Digest) Inspection {
	t.Helper()
	before := inspectFile(t, tools, path)
	if err := tools.WriteManagedTags(t.Context(), openRW(t, path), FormatFLAC, v, cover.cover(t)); err != nil {
		t.Fatalf("WriteManagedTags(%s): %v", filepath.Base(path), err)
	}
	after := inspectFile(t, tools, path)
	if err := VerifyTags(v, cover.expected(t), before, after); err != nil {
		t.Fatalf("VerifyTags(%s): %v\nbefore: %+v\nafter:  %+v", filepath.Base(path), err, before, after)
	}
	if err := audioChanged(t, tools, path, d0); err != nil {
		t.Fatal(err)
	}
	return after
}

func TestTagsFLACStripsID3(t *testing.T) {
	dir := t.TempDir()
	app := flacBlock{typ: flacApplication, data: append([]byte("TEST"), garbage(3, 300)...)}
	// A padding of 8000 bytes: above TagLib's threshold for the file without
	// the large ID3v2 (1% of it, but at least 4 KiB), below the one for the
	// file with it (1% of more than 800 KiB). The first save keeps it, the
	// output must not: it resets it to 4 KiB, as the reference does.
	plain := baseFLAC(t, dir, "src.flac", noise3s).with(
		vorbisBlock("reference libFLAC 1.4.3 20230623", "TITLE=Vorbis title", "COMPOSER=Keep",
			"REPLAYGAIN_TRACK_GAIN=-6.50 dB", "COMMENT=first", "COMMENT=second"),
		app, flacBlock{typ: flacPadding, data: make([]byte, 8000)})
	cover := &coverFile{writeFile(t, filepath.Join(dir, "cover.png"), pngImage(t, 8, 8, 4)), FormatPNG}

	for name, tools := range helpers(t) {
		// The reference: the same file without any ID3 tag, written with the
		// same request.
		ref := writeFLAC(t, t.TempDir(), "plain.flac", plain)
		plainIn := inspectFile(t, tools, ref)
		d0 := digest(t, tools, ref)
		writeChecked(t, tools, ref, fullValues, cover)
		want := readFile(t, ref)
		// The premise of the large ID3v2 case: the reference resets the
		// padding to 4 KiB.
		if pad := parseFLAC(t, want).ofType(flacPadding); len(pad) != 1 || len(pad[0].data) != 4096 {
			t.Fatalf("the reference kept its padding: %d blocks", len(pad))
		}

		for _, tc := range id3Cases(t) {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				f := plain
				f.prefix = tc.prefix
				f.audio = slices.Concat(plain.audio, tc.suffix)
				p := writeFLAC(t, t.TempDir(), "id3.flac", f)

				in := inspectFile(t, tools, p)
				if !slices.Equal(in.Opaque, tc.opaque) {
					t.Fatalf("opaque:\n got %+v\nwant %+v", in.Opaque, tc.opaque)
				}
				if b := in.Blocking(); len(b) != 0 {
					t.Fatalf("the ID3 tags block a write: %+v", b)
				}
				// The ID3 values are not read: the Vorbis comment is the tags.
				if !reflect.DeepEqual(in.Managed, plainIn.Managed) || !reflect.DeepEqual(in.Unmanaged, plainIn.Unmanaged) ||
					!reflect.DeepEqual(in.Pictures, plainIn.Pictures) {
					t.Fatalf("the inspection differs from the file's without ID3:\n got %+v\nwant %+v", in, plainIn)
				}
				// The input digests to the samples of the file without ID3:
				// the pinned ffmpeg skips a leading ID3v2 itself, and
				// AudioDigest leaves a trailing ID3v1 out of the decode
				// (N-128). So §9.1 step 6 compares like with like.
				if dg, err := tools.AudioDigest(t.Context(), open(t, p)); err != nil || dg != d0 {
					t.Fatalf("AudioDigest of the input: %v, %+v", err, dg)
				}

				// §9.1 step 6: VerifyTags, the samples of the file without ID3.
				after := writeStripping(t, tools, p, fullValues, cover, d0)
				if len(after.Opaque) != 0 {
					t.Fatalf("after the write: %+v", after.Opaque)
				}
				out := readFile(t, p)
				// Stripped exactly: the output is byte for byte the output of
				// the same file without its ID3 tags. So the audio frames,
				// STREAMINFO, the APPLICATION block and every unmanaged
				// Vorbis field are the reference's, and so is the padding.
				if !bytes.Equal(out, want) {
					t.Fatalf("the output (%d bytes) is not the one of the file without ID3 (%d bytes)", len(out), len(want))
				}
				o := parseFLAC(t, out) // it starts with fLaC: no ID3v2
				if !bytes.Equal(o.audio, plain.audio) {
					t.Fatal("the audio frames changed")
				}
				if n := len(out); n >= 128 && bytes.HasPrefix(out[n-128:], []byte("TAG")) {
					t.Fatal("an ID3v1 tag is left")
				}

				// Deterministic: another copy gives the same bytes.
				p2 := writeFLAC(t, t.TempDir(), "id3.flac", f)
				writeStripping(t, tools, p2, fullValues, cover, d0)
				if !bytes.Equal(readFile(t, p2), out) {
					t.Fatal("the same write gave different files")
				}
				// Idempotent: writing the output again changes nothing.
				writeChecked(t, tools, p, fullValues, cover)
				if !bytes.Equal(readFile(t, p), out) {
					t.Fatal("a second write of the output changed its bytes")
				}
			})
		}
	}
}

// A file without a Vorbis comment block, with ID3v2 and ID3v1 tags:
// TagLib's save copies the tags it knows into the new comment block
// (Tag::duplicate). The ID3 tags are stripped before, so no ID3 value
// reaches the output, not even as an unmanaged field (COMMENT from COMM).
func TestTagsFLACStripsID3WithoutComment(t *testing.T) {
	dir := t.TempDir()
	plain := baseFLAC(t, dir, "src.flac", sine3s)
	withID3 := plain
	withID3.prefix = id3v2With(0, slices.Concat(
		id3v2Frame("TIT2", []byte("\x03ID3 title")),
		id3v2Frame("COMM", []byte("\x03eng\x00an ID3 comment")),
		id3v2Frame("TCON", []byte("\x03ID3 genre")),
		id3v2Frame("TDRC", []byte("\x032001")),
	))
	withID3.audio = slices.Concat(plain.audio, id3v1Tag())
	for name, tools := range helpers(t) {
		t.Run(name, func(t *testing.T) {
			ref := writeFLAC(t, t.TempDir(), "plain.flac", plain)
			d0 := digest(t, tools, ref)
			writeChecked(t, tools, ref, TagValues{Title: "T"}, nil)
			p := writeFLAC(t, t.TempDir(), "id3.flac", withID3)
			after := writeStripping(t, tools, p, TagValues{Title: "T"}, nil, d0)
			c := onlyComment(t, parseFLAC(t, readFile(t, p)))
			if !slices.Equal(c.entries, []string{"TITLE=T"}) || len(after.Unmanaged) != 1 {
				t.Fatalf("comment %q, unmanaged %+v", c.entries, after.Unmanaged)
			}
			if !bytes.Equal(readFile(t, p), readFile(t, ref)) {
				t.Fatal("the output is not the one of the file without ID3")
			}
		})
	}
}
