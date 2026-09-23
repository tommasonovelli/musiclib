package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// NOTES.md N-128 (owner decision, 2026-09-23): for a FLAC with a trailing
// ID3v1 tag, AudioDigest decodes only the bytes before the tag, with the
// same pinned ffmpeg and exactly the same options (§8.4, N-074). The tag is
// found by the helper's rule (TagLib's findID3v1), and the bytes reach
// ffmpeg through a pipe that carries exactly that many bytes.

// zeroTotal clears STREAMINFO's total sample count ("unknown", as a
// streaming encoder writes it), so that AudioDigest has no declared length
// to compare with (N-078) and only the decoder can refuse a truncation.
func zeroTotal(b []byte) []byte {
	b = slices.Clone(b)
	b[8+13] &= 0xf0
	binary.BigEndian.PutUint32(b[8+14:], 0)
	return b
}

// falseID3v1FLAC is a valid FLAC file whose audio bytes spell "TAG" 128
// bytes before the end: TagLib's rule, and so the helper's and the
// digest's, takes them for an ID3v1 tag. Uniform 16-bit noise does not
// compress, so the encoder stores the last frame VERBATIM: its last bytes
// are the big-endian samples, then the frame's CRC-16. Two samples are set
// so that "TAG" lands 128 bytes before the end.
func falseID3v1FLAC(t testing.TB, dir string) []byte {
	t.Helper()
	const n = 3 * 44100
	samples := make([]uint16, n)
	r := rand.New(rand.NewPCG(5, 6))
	for i := range samples {
		samples[i] = uint16(r.Uint32())
	}
	// The last 63 samples are the 126 bytes before the CRC-16.
	samples[n-63] = 'T'<<8 | 'A'
	samples[n-62] = 'G'<<8 | samples[n-62]&0xff
	raw := make([]byte, 2*n)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(raw[2*i:], s)
	}
	src := writeFile(t, filepath.Join(dir, "noise.s16le"), raw)
	b := readFile(t, gen(t, dir, "false-id3v1.flac", "-f", "s16le", "-ar", "44100", "-ac", "1", "-i", src, "-c:a", "flac"))
	if tail := b[len(b)-131:]; string(tail[3:6]) != "TAG" || string(tail[:8]) == "APETAGEX" {
		t.Fatalf("the premise failed: the file ends with %q, the encoder did not store the last frame verbatim", tail[:8])
	}
	return b
}

// The rule of hasTrailingID3v1 on its own, including the branch for files
// of 128 to 130 bytes, which no FLAC can reach ("fLaC" or "ID3" occupies
// the first bytes), written as TagLib's Utils::findID3v1 is.
func TestHasTrailingID3v1(t *testing.T) {
	tag := func(n int, at int, s string) []byte {
		b := bytes.Repeat([]byte{0xAA}, n)
		copy(b[at:], s)
		return b
	}
	for _, tc := range []struct {
		name string
		file []byte
		want bool
	}{
		{"empty", nil, false},
		{"127 bytes starting with TAG", tag(127, 0, "TAG"), false},
		{"128 bytes starting with TAG", tag(128, 0, "TAG"), true},
		{"130 bytes, TAG at 2", tag(130, 2, "TAG"), true},
		{"130 bytes, TAG at 0", tag(130, 0, "TAG"), false},
		{"131 bytes, TAG at 3", tag(131, 3, "TAG"), true},
		{"131 bytes, APETAGEX at 0", tag(131, 0, "APETAGEX"), false},
		{"1000 bytes, TAG 128 from the end", tag(1000, 872, "TAG"), true},
		{"1000 bytes, APETAGEX 131 from the end", tag(1000, 869, "APETAGEX"), false},
		{"1000 bytes, APETAGEY 131 from the end", tag(1000, 869, "APETAGEY"), true},
		{"1000 bytes, XPETAGEX 131 from the end", tag(1000, 869, "XPETAGEX"), true},
		{"1000 bytes, TAG 129 from the end", tag(1000, 871, "TAG"), false},
		{"1000 bytes, TAG 127 from the end", tag(1000, 873, "TAG"), false},
		{"1000 bytes, lower-case tag", tag(1000, 872, "tag"), false},
		{"1000 bytes, TA only", tag(1000, 872, "TA"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tail := tc.file[len(tc.file)-min(len(tc.file), 131):]
			if got := hasTrailingID3v1(tail, int64(len(tc.file))); got != tc.want {
				t.Fatalf("hasTrailingID3v1 = %v, want %v", got, tc.want)
			}
			// flacAudioEnd reads the same bytes from a file.
			p := writeFile(t, filepath.Join(t.TempDir(), "f"), tc.file)
			end, err := flacAudioEnd(open(t, p))
			want := int64(wholeFile)
			if tc.want {
				want = int64(len(tc.file) - 128)
			}
			if err != nil || end != want {
				t.Fatalf("flacAudioEnd = %d, %v; want %d", end, err, want)
			}
		})
	}
}

// One rule (§13.2): the ID3v1 tag the digest leaves out is the one the
// helper reports, and therefore strips at render, file by file, with the
// release and the sanitized helper. The helper's own read is checked
// against TagLib's hasID3v1Tag() inside it (checkTagLibAgrees), so the
// three agree. A tag that would overlap the metadata blocks is refused by
// the helper as corrupt ("overlaps"): detected, and never written.
func TestTrailingID3v1IsTheHelpersRule(t *testing.T) {
	dir := t.TempDir()
	base := baseFLAC(t, dir, "src.flac", sine3s)
	plain := base.with(vorbisBlock("v", "TITLE=x")).bytes()
	ape := func(id string) []byte { return append([]byte(id), make([]byte, 123)...) }
	appendedID3v2 := id3v2With(0x10, id3v2Frame("TIT2", []byte("\x03appended")))
	cases := []struct {
		name string
		file []byte
		want bool
	}{
		{"no tag", plain, false},
		{"ID3v1", slices.Concat(plain, id3v1Tag()), true},
		{"leading ID3v2 and ID3v1", slices.Concat(id3v2With(0, id3v2Frame("TIT2", []byte("\x03x"))), plain, id3v1Tag()), true},
		{"two ID3v1", slices.Concat(plain, id3v1Tag(), id3v1Tag()), true},
		{"APETAGEX 131 bytes from the end", slices.Concat(plain, ape("APETAGEX")), false},
		{"APETAGEY 131 bytes from the end", slices.Concat(plain, ape("APETAGEY")), true},
		{"XPETAGEX 131 bytes from the end", slices.Concat(plain, ape("XPETAGEX")), true},
		{"TAG 129 bytes from the end", slices.Concat(plain, []byte("TAG"), make([]byte, 126)), false},
		{"TAG 127 bytes from the end", slices.Concat(plain, []byte("TAG"), make([]byte, 124)), false},
		{"lower-case tag", slices.Concat(plain, []byte("tag"), make([]byte, 125)), false},
		{"appended ID3v2", slices.Concat(plain, appendedID3v2), false},
		{"appended ID3v2 and ID3v1", slices.Concat(plain, appendedID3v2, id3v1Tag()), true},
		{"audio bytes that spell TAG", falseID3v1FLAC(t, dir), true},
		{"ID3v1 inside the metadata", flacFile{blocks: base.with(rawBlock(flacApplication, []byte("TEST"), make([]byte, 100), id3v1Tag())).blocks}.bytes(), true},
	}
	for name, tools := range helpers(t) {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				p := writeFile(t, filepath.Join(t.TempDir(), "f.flac"), tc.file)
				in, err := tools.Inspect(t.Context(), open(t, p), FormatFLAC)
				var helper bool
				switch {
				case err == nil:
					helper = slices.Contains(in.Opaque, OpaqueField{Key: "id3v1", Reason: "foreign_tag", Removed: true})
				case Code(err) == CodeTagsCorrupt && strings.Contains(err.Error(), "an ID3v1 tag overlaps the metadata blocks"):
					helper = true
				default:
					t.Fatalf("Inspect: %v", err)
				}
				end, err := flacAudioEnd(open(t, p))
				if err != nil {
					t.Fatal(err)
				}
				if digest := end != wholeFile; digest != helper || helper != tc.want {
					t.Fatalf("ID3v1: the helper says %v, the digest %v (end %d); want %v", helper, digest, end, tc.want)
				}
				if tc.want && end != int64(len(tc.file)-128) {
					t.Fatalf("the digest's audio ends at %d of %d bytes", end, len(tc.file))
				}
			})
		}
	}
}

// The owner's rule, proved on the decoded samples:
//   - a FLAC with a trailing ID3v1 digests exactly as the same FLAC without
//     it, with and without a declared length, and with a leading ID3v2 too;
//   - a FLAC whose audio is cut short, even by one byte, is still refused
//     when an ID3v1 follows it, also without a declared length: the frame
//     CRC-16 (N-074) or the decoder refuses the incomplete last frame;
//   - audio bytes that happen to spell "TAG" 128 bytes before the end are
//     refused, not silently accepted: the cut falls inside the last frame,
//     since a frame starts with its sync code (0xFF), never with "T";
//   - what the helper does not strip stays refused: an APEv2 identifier, a
//     second ID3v1, an ID3v2 appended at the end.
func TestAudioDigestTrailingID3v1(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	plain := readFile(t, gen(t, dir, "plain.flac", append(lavfi(noise3s), "-ac", "2", "-c:a", "flac")...))
	leading := id3v2With(0, id3v2Frame("TIT2", []byte("\x03x")))
	file := func(parts ...[]byte) string {
		return writeFile(t, filepath.Join(t.TempDir(), "f.flac"), slices.Concat(parts...))
	}

	t.Run("equal to the file without the tag", func(t *testing.T) {
		for _, src := range [][]byte{plain, zeroTotal(plain)} {
			d0 := digest(t, tools, file(src))
			for _, p := range []string{file(src, id3v1Tag()), file(leading, src, id3v1Tag())} {
				if d := digest(t, tools, p); d != d0 {
					t.Fatalf("with ID3v1 %+v, without %+v", d, d0)
				}
			}
		}
	})

	t.Run("truncated audio is still refused", func(t *testing.T) {
		for _, src := range [][]byte{plain, zeroTotal(plain)} {
			for _, cut := range []int{1, 2, 3, 10, 100, 1000} {
				p := file(src[:len(src)-cut], id3v1Tag())
				_, err := tools.AudioDigest(t.Context(), open(t, p))
				wantCode(t, err, CodeDecode)
			}
		}
	})

	t.Run("audio that spells TAG is refused", func(t *testing.T) {
		b := falseID3v1FLAC(t, dir)
		for _, src := range [][]byte{b, zeroTotal(b)} {
			p := file(src)
			// Read whole, the file is valid audio: the refusal comes from
			// the 128 bytes the rule leaves out.
			f := open(t, p)
			pr, err := tools.Probe(t.Context(), f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tools.decode(t.Context(), f, pr, wholeFile); err != nil {
				t.Fatalf("the premise failed: the whole file does not decode: %v", err)
			}
			_, err = tools.AudioDigest(t.Context(), open(t, p))
			wantCode(t, err, CodeDecode)
		}
	})

	t.Run("trailing bytes the helper keeps are refused", func(t *testing.T) {
		appended := id3v2With(0x10, id3v2Frame("TIT2", []byte("\x03appended")))
		for name, p := range map[string]string{
			"APETAGEX":                 file(plain, []byte("APETAGEX"), make([]byte, 123)),
			"two ID3v1":                file(plain, id3v1Tag(), id3v1Tag()),
			"appended ID3v2":           file(plain, appended),
			"appended ID3v2 and ID3v1": file(plain, appended, id3v1Tag()),
		} {
			t.Run(name, func(t *testing.T) {
				_, err := tools.AudioDigest(t.Context(), open(t, p))
				wantCode(t, err, CodeDecode)
			})
		}
	})

	t.Run("the offset is not used", func(t *testing.T) {
		f := open(t, file(plain, id3v1Tag()))
		d1, err := tools.AudioDigest(t.Context(), f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Seek(12345, 0); err != nil {
			t.Fatal(err)
		}
		if d2, err := tools.AudioDigest(t.Context(), f); err != nil || d2 != d1 {
			t.Fatalf("second digest %+v, %v; first %+v", d2, err, d1)
		}
	})
}

// The bytes that reach the decoder, recorded by a fake ffmpeg that copies
// its descriptor 3 to a file: exactly the file without its last 128 bytes
// for a FLAC with a trailing ID3v1, the whole file otherwise. The samples
// alone cannot show this to the byte: the pinned ffmpeg ignores a few
// bytes after the last frame (NOTES.md N-128).
func TestAudioDigestFeedsExactlyTheAudio(t *testing.T) {
	dir := t.TempDir()
	plain := readFile(t, gen(t, dir, "ok.flac", append(lavfi(noise3s), "-ac", "2", "-c:a", "flac")...))
	received := filepath.Join(dir, "received")
	script := writeFile(t, filepath.Join(dir, "ffmpeg-record"), []byte("#!/bin/sh\ncat <&3 >'"+received+"'\n"))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	tools := &Tools{run: NewRunner(1), ffmpeg: script, ffprobe: FFprobePath}
	for name, tc := range map[string]struct{ file, want []byte }{
		"ID3v1":             {slices.Concat(plain, id3v1Tag()), plain},
		"no ID3v1":          {plain, plain},
		"APETAGEX":          {slices.Concat(plain, []byte("APETAGEX"), make([]byte, 123)), slices.Concat(plain, []byte("APETAGEX"), make([]byte, 123))},
		"two ID3v1":         {slices.Concat(plain, id3v1Tag(), id3v1Tag()), slices.Concat(plain, id3v1Tag())},
		"leading ID3v2 too": {slices.Concat(id3v2With(0, nil), plain, id3v1Tag()), slices.Concat(id3v2With(0, nil), plain)},
	} {
		t.Run(name, func(t *testing.T) {
			p := writeFile(t, filepath.Join(t.TempDir(), "f.flac"), tc.file)
			// The fake prints nothing: the decode fails after the run.
			_, err := tools.AudioDigest(t.Context(), open(t, p))
			wantCode(t, err, CodeDecode)
			if got := readFile(t, received); !bytes.Equal(got, tc.want) {
				t.Fatalf("the decoder read %d bytes, want the first %d of %d", len(got), len(tc.want), len(tc.file))
			}
		})
	}
}

// The limited input's own failures, with a fake ffmpeg (and the real
// ffprobe) on a FLAC with an ID3v1, larger than a pipe's buffer: a decoder
// that exits 0 without reading the stream fails the decode; one that exits
// 1 fails it with its status; a cancellation kills the decoder and the
// feeder returns (the race detector and -count check that nothing
// leaks or blocks).
func TestAudioDigestLimitedInputFailures(t *testing.T) {
	dir := t.TempDir()
	plain := readFile(t, gen(t, dir, "ok.flac", append(lavfi(noise3s), "-ac", "2", "-c:a", "flac")...))
	if len(plain) < 4<<16 {
		t.Fatalf("the premise failed: %d bytes fit a pipe's buffer", len(plain))
	}
	p := writeFile(t, filepath.Join(dir, "id3v1.flac"), slices.Concat(plain, id3v1Tag()))
	for _, tc := range []struct {
		name, script, code, msg string
		cancel                  bool
	}{
		{"exit 0 without reading", "head -c 16000 /dev/zero", CodeDecode, "before reading the whole audio stream", false},
		{"exit 1 without reading", "exit 1", CodeDecode, "exit status 1", false},
		{"cancelled", "sleep 60", CodeCanceled, "cancellation", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := writeFile(t, filepath.Join(dir, "ffmpeg-"+strings.ReplaceAll(tc.name, " ", "-")), []byte("#!/bin/sh\n"+tc.script+"\n"))
			if err := os.Chmod(script, 0o755); err != nil {
				t.Fatal(err)
			}
			tools := &Tools{run: NewRunner(1), ffmpeg: script, ffprobe: FFprobePath}
			ctx := t.Context()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				time.AfterFunc(300*time.Millisecond, cancel)
			}
			// The probe first, uncancelled; then the limited decode.
			f := open(t, p)
			pr := probe(t, newTools(t), p)
			limit, err := flacAudioEnd(f)
			if err != nil || limit != int64(len(plain)) {
				t.Fatalf("flacAudioEnd = %d, %v", limit, err)
			}
			_, err = tools.decode(ctx, f, pr, limit)
			e := wantCode(t, err, tc.code)
			if !strings.Contains(e.Error(), tc.msg) {
				t.Fatalf("error %q does not say %q", e.Error(), tc.msg)
			}
		})
	}
}

// feedPrefix writes exactly n bytes, and reports a short or unreadable
// input as a read failure, apart from the pipe.
func TestFeedPrefix(t *testing.T) {
	data := garbage(9, 300<<10)
	p := writeFile(t, filepath.Join(t.TempDir(), "in"), data)
	feed := func(f *os.File, n int64) ([]byte, feedResult) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan feedResult, 1)
		go func() { done <- feedPrefix(w, f, n) }()
		var out bytes.Buffer
		if _, err := out.ReadFrom(r); err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		return out.Bytes(), <-done
	}
	for _, n := range []int64{0, 1, readChunk - 1, readChunk, readChunk + 1, int64(len(data)) - 128, int64(len(data))} {
		out, res := feed(open(t, p), n)
		if res.readErr != nil || res.writeErr != nil || !bytes.Equal(out, data[:n]) {
			t.Fatalf("n=%d: %d bytes, %+v", n, len(out), res)
		}
	}
	if _, res := feed(open(t, p), int64(len(data))+1); res.readErr == nil || !strings.Contains(res.readErr.Error(), "the input ended") {
		t.Fatalf("a short input: %+v", res)
	}
	wo, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := wo.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, res := feed(wo, 10); res.readErr == nil {
		t.Fatalf("an unreadable input: %+v", res)
	}
}

// The failures of flacAudioEnd are reads of the input, before any tool
// runs: they carry their own op, not the decoder's (round 7 review).
func TestFlacAudioEndReadFailure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.flac")
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 200), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = flacAudioEnd(f)
	if e := wantCode(t, err, CodeIO); e.Op != "trailing ID3v1 check" {
		t.Fatalf("op %q, want the trailing ID3v1 check", e.Op)
	}
}
