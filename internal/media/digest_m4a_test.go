package media

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

// AudioDigest of M4A before and after a tag write (DESIGN.md §8.4 "I test
// includono AAC/MP3 gapless e ALAC"; NOTES.md N-168). FFmpeg trims the AAC
// encoder delay by the edit list of the track or, when an iTunes "iTunSMPB"
// item is read, by its priming; the end padding is decoded. A write keeps
// both (the track byte for byte, iTunSMPB as an unmanaged item), so the
// samples and their count are the same after it, wherever the media data
// moved. No decode window is needed (unlike MP3, N-154).

// withoutEditList returns the file without the edit list of its track.
func withoutEditList(t testing.TB, file []byte) []byte {
	t.Helper()
	return mp4RewriteMoov(t, file, func(moov []mp4Node) []mp4Node {
		return mp4Edit(t, moov, []string{"trak"}, func(trak []mp4Node) []mp4Node {
			return slices.DeleteFunc(trak, func(n mp4Node) bool { return n.typ == "edts" })
		})
	})
}

// iTunSMPB is the gapless item iTunes writes: priming, padding and the
// number of samples, in hex.
func iTunSMPB(priming, padding, samples uint64) []byte {
	return iTunesText("iTunSMPB", fmt.Sprintf(" 00000000 %08X %08X %016X 00000000 00000000", priming, padding, samples))
}

// aacPackets is the number of AAC access units (1,024 samples each) of the
// file's track.
func aacPackets(t testing.TB, file []byte) uint64 {
	t.Helper()
	for _, n := range mp4Find(t, mp4Children(t, file), append([]string{"moov"}, stblPath...)...) {
		if n.typ == "stsz" {
			return uint64(binary.BigEndian.Uint32(n.payload[8:]))
		}
	}
	t.Fatal("no stsz")
	return 0
}

func TestAudioDigestM4AGapless(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	_, sourceFrames := expectedPCM(t, readFile(t, wav16(t, dir, "ref.wav", noise3s)))
	aac := m4aSource(t, dir, "aac", true)
	total := aacPackets(t, aac) * 1024
	smpb := iTunSMPB(1024, total-1024-uint64(sourceFrames), uint64(sourceFrames))
	cover := pngCover(t, dir)
	files := map[string][]byte{
		"aac, edit list":              aac,
		"aac, iTunSMPB":               withIlst(t, withoutEditList(t, aac), 0, smpb),
		"aac, edit list and iTunSMPB": withIlst(t, aac, 0, smpb),
		"aac, neither":                withoutEditList(t, aac),
		"alac":                        m4aSource(t, dir, "alac", true),
	}
	want := map[string]int64{
		"aac, edit list": int64(total) - 1024, "aac, iTunSMPB": int64(total) - 1024,
		"aac, edit list and iTunSMPB": int64(total) - 1024, "aac, neither": int64(total), "alac": sourceFrames,
	}
	digests := map[string]Digest{}
	for name, b := range files {
		t.Run(name, func(t *testing.T) {
			format := FormatM4AAAC
			if name == "alac" {
				format = FormatM4AALAC
			}
			p := m4aAt(t, b)
			d0 := digest(t, tools, p)
			if d0.Frames != want[name] {
				t.Fatalf("%d frames, want %d (source %d, %d decoded with padding)", d0.Frames, want[name], sourceFrames, total)
			}
			digests[name] = d0
			// The media moves (moov grows before it), then moov shrinks.
			writeM4AChecked(t, tools, p, format, fullValues, cover)
			writeM4AChecked(t, tools, p, format, TagValues{Title: "x"}, nil)
			if d := digest(t, tools, p); d != d0 {
				t.Fatalf("the digest changed: %+v -> %+v", d0, d)
			}
		})
	}
	// The priming of the edit list and of iTunSMPB is the same trim.
	if digests["aac, edit list"] != digests["aac, iTunSMPB"] || digests["aac, edit list"] != digests["aac, edit list and iTunSMPB"] {
		t.Fatalf("the trims differ: %+v", digests)
	}
}

// FFmpeg gives the priming of iTunSMPB to the stream created last
// (mov_read_custom): a JPEG, PNG or BMP covr before iTunSMPB creates an
// attached-picture stream first and the audio is not trimmed. GIF and type
// 0 covr do not create one. The replacement cover must stay on the same
// side of iTunSMPB's stream boundary (N-168); removing a stream-creating
// cover with no edit list changes the decode, which render refuses.
func TestAudioDigestM4ACoverBeforeITunSMPB(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	aac := withoutEditList(t, m4aSource(t, dir, "aac", true))
	total := aacPackets(t, aac) * 1024
	smpb := iTunSMPB(1024, 100, total-1124)
	jpg := jpegImage(t, dir, "c.jpg", "blue")
	covr := mp4Box("covr", dataAtom(13, jpg))
	first := m4aAt(t, withIlst(t, aac, 0, covr, smpb))
	last := m4aAt(t, withIlst(t, aac, 0, smpb, covr))
	df, dl := digest(t, tools, first), digest(t, tools, last)
	if df.Frames != int64(total) || dl.Frames != int64(total)-1024 {
		t.Fatalf("FFmpeg's reading changed: %d frames with the cover first, %d with it last (N-168)", df.Frames, dl.Frames)
	}
	jc := &coverFile{writeFile(t, filepath.Join(dir, "c.jpg"), jpg), FormatJPEG}
	writeM4AChecked(t, tools, first, FormatM4AAAC, fullValues, jc)
	writeM4AChecked(t, tools, last, FormatM4AAAC, fullValues, nil)
	if err := tools.WriteManagedTags(t.Context(), openRW(t, first), FormatM4AAAC, fullValues, nil); err != nil {
		t.Fatal(err)
	}
	if audioChanged(t, tools, first, df) == nil {
		t.Fatal("removing the first cover kept the decode: the case this pins is gone")
	}
}

// A non-stream-creating cover before iTunSMPB must not be replaced there:
// the new JPEG would take the priming and change the audio's frame count.
func TestAudioDigestM4ANonStreamCoverBeforeITunSMPB(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	aac := withoutEditList(t, m4aSource(t, dir, "aac", true))
	total := aacPackets(t, aac) * 1024
	smpb := iTunSMPB(1024, 100, total-1124)
	jpg := jpegImage(t, dir, "new.jpg", "blue")
	cover := &coverFile{writeFile(t, filepath.Join(dir, "new.jpg"), jpg), FormatJPEG}
	for _, typ := range []uint32{12, 0} {
		t.Run(fmt.Sprint(typ), func(t *testing.T) {
			p := m4aAt(t, withIlst(t, aac, 0, mp4Box("covr", dataAtom(typ, jpg)), smpb))
			before := digest(t, tools, p)
			if before.Frames != int64(total)-1024 {
				t.Fatalf("non-stream cover changed priming: %d frames", before.Frames)
			}
			writeM4AChecked(t, tools, p, FormatM4AAAC, fullValues, cover)
			items := ilstItems(t, readFile(t, p))
			if !slices.Equal(items[len(items)-2], smpb) || !slices.Equal(items[len(items)-1], mp4Box("covr", dataAtom(13, jpg))) {
				t.Fatal("new cover must follow iTunSMPB")
			}
		})
	}
}

// Two separated covr items: the first stream-creating one, not the last,
// fixes the FFmpeg stream order. Pin the exact position as well as the digest.
func TestAudioDigestM4ASeparatedCovers(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	aac := withoutEditList(t, m4aSource(t, dir, "aac", true))
	total := aacPackets(t, aac) * 1024
	smpb := iTunSMPB(1024, 100, total-1124)
	jpg := jpegImage(t, dir, "new.jpg", "blue")
	covr := mp4Box("covr", dataAtom(13, jpg))
	p := m4aAt(t, withIlst(t, aac, 0, covr, smpb, textItem("soco", "kept"), covr))
	cover := &coverFile{writeFile(t, filepath.Join(dir, "new.jpg"), jpg), FormatJPEG}
	before := digest(t, tools, p)
	if before.Frames != int64(total) {
		t.Fatalf("first cover did not take priming: %d frames", before.Frames)
	}
	writeM4AChecked(t, tools, p, FormatM4AAAC, fullValues, cover)
	items := ilstItems(t, readFile(t, p))
	if !slices.Equal(items[len(items)-3], covr) || !slices.Equal(items[len(items)-2], smpb) ||
		!slices.Equal(items[len(items)-1], textItem("soco", "kept")) {
		t.Fatalf("replacement cover is not before the separated kept items: %q", items)
	}
}
