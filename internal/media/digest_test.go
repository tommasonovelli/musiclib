package media

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// expectedPCM computes, without ffmpeg, the digest of the samples of a PCM
// WAV file (16 or 24 bit): each sample as the float64 that FFmpeg's exact
// integer-to-double conversion gives (x / 2^(bits-1)), little-endian,
// interleaved. A lossless encode of that WAV must decode to exactly this.
func expectedPCM(t testing.TB, wav []byte) (sha string, frames int64) {
	t.Helper()
	if len(wav) < 36 || string(wav[12:16]) != "fmt " {
		t.Fatal("the fmt chunk is not first")
	}
	channels := int(binary.LittleEndian.Uint16(wav[22:24]))
	bits := int(binary.LittleEndian.Uint16(wav[34:36]))
	off, n := wavData(t, wav)
	data := wav[off : off+n]
	width := bits / 8
	if (bits != 16 && bits != 24) || len(data)%(width*channels) != 0 {
		t.Fatalf("unexpected WAV: %d bits, %d channels, %d bytes", bits, channels, len(data))
	}
	h := sha256.New()
	var buf [8]byte
	scale := math.Ldexp(1, -(bits - 1))
	for i := 0; i < len(data); i += width {
		var x int32
		if bits == 16 {
			x = int32(int16(binary.LittleEndian.Uint16(data[i:])))
		} else {
			x = int32(uint32(data[i])|uint32(data[i+1])<<8|uint32(data[i+2])<<16) << 8 >> 8
		}
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(float64(x)*scale))
		h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil)), int64(len(data) / (width * channels))
}

// §8.4: the digest is the SHA-256 of the exact decoded samples, with no
// change of rate, channels or layout, and the frame count is exact. Checked
// against an independent computation from the WAV source, for 16 and 24
// bit, mono, stereo and 5.1.
func TestAudioDigestIsTheExactPCM(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	for _, tc := range []struct {
		name   string
		source []string // ffmpeg arguments producing the WAV
		layout string
	}{
		{"16 bit stereo", append(lavfi(noise3s), "-ac", "2", "-c:a", "pcm_s16le"), "stereo"},
		{"16 bit mono", append(lavfi(sine3s), "-ac", "1", "-c:a", "pcm_s16le"), "mono"},
		{"24 bit stereo", append(lavfi(noise3s), "-ac", "2", "-c:a", "pcm_s24le"), "stereo"},
		{"16 bit 5.1", append(lavfi(noise3s), "-af", "pan=5.1|c0=c0|c1=0.5*c0|c2=-1*c0|c3=0.25*c0|c4=0.1*c0|c5=-0.7*c0", "-c:a", "pcm_s16le"), "5.1"},
		{"24 bit 48 kHz stereo", append(lavfi("anoisesrc=color=white:sample_rate=48000:seed=3:duration=2"), "-ac", "2", "-c:a", "pcm_s24le"), "stereo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wav := gen(t, dir, tc.name+".wav", tc.source...)
			wantSHA, wantFrames := expectedPCM(t, readFile(t, wav))
			flacArgs := []string{"-i", wav, "-c:a", "flac"}
			if tc.name[:2] == "24" {
				flacArgs = append(flacArgs, "-sample_fmt", "s32")
			}
			flac := gen(t, dir, tc.name+".flac", flacArgs...)
			alac := gen(t, dir, tc.name+".m4a", "-i", wav, "-c:a", "alac")
			for _, p := range []string{flac, alac} {
				d := digest(t, tools, p)
				if d.PCMSHA256 != wantSHA || d.Frames != wantFrames {
					t.Fatalf("%s: digest %s, %d frames; the samples of the source are %s, %d frames",
						filepath.Base(p), d.PCMSHA256, d.Frames, wantSHA, wantFrames)
				}
				rate, channels := 44100, 2
				if tc.name == "24 bit 48 kHz stereo" {
					rate = 48000
				}
				switch tc.layout {
				case "mono":
					channels = 1
				case "5.1":
					channels = 6
				}
				if d.SampleRate != rate || d.Channels != channels || d.Layout != tc.layout {
					t.Fatalf("%s: parameters %d Hz, %d channels, %q", filepath.Base(p), d.SampleRate, d.Channels, d.Layout)
				}
			}
		})
	}
}

// §8.4 gapless: MP3 with a LAME header decodes to exactly the source's
// length; AAC and MP3 digests are stable across runs, including concurrent
// ones. (AAC in M4A keeps the encoder's end padding: its count is stable,
// not equal to the source's, see N-078.)
func TestAudioDigestGaplessAndStable(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	wav := wav16(t, dir, "source.wav", noise3s)
	_, sourceFrames := expectedPCM(t, readFile(t, wav))
	files := map[string]string{
		"mp3 cbr": lame(t, wav, dir, "cbr.mp3", "-b", "192"),
		"mp3 vbr": lame(t, wav, dir, "vbr.mp3", "-V", "2"),
		"aac":     gen(t, dir, "aac.m4a", "-i", wav, "-c:a", "aac", "-b:a", "192k"),
		"alac":    gen(t, dir, "alac.m4a", "-i", wav, "-c:a", "alac"),
	}
	for name, p := range files {
		t.Run(name, func(t *testing.T) {
			first := digest(t, tools, p)
			var wg sync.WaitGroup
			digests := make([]Digest, 4)
			for i := range digests {
				wg.Add(1)
				go func() {
					defer wg.Done()
					digests[i] = digest(t, tools, p)
				}()
			}
			wg.Wait()
			for _, d := range digests {
				if d != first {
					t.Fatalf("digest changed between runs: %+v, then %+v", first, d)
				}
			}
			switch name {
			case "aac":
				if first.Frames < sourceFrames || first.Frames > sourceFrames+2048 {
					t.Fatalf("aac: %d frames for a %d-frame source", first.Frames, sourceFrames)
				}
			default:
				if first.Frames != sourceFrames {
					t.Fatalf("%s: %d frames, the source has %d", name, first.Frames, sourceFrames)
				}
			}
		})
	}
}

// §8.4 invariants: the digest follows the samples, not the container bytes.
// Changed tags, a re-mux, another FLAC compression level or an added cover
// give the same digest; one changed sample gives a different one; the same
// samples under another declared layout give another Layout.
func TestAudioDigestInvariants(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	wav := wav16(t, dir, "source.wav", noise3s)
	jpg := gen(t, dir, "cover.jpg", append(lavfi("color=c=blue:size=16x16"), "-frames:v", "1")...)

	same := func(t *testing.T, a, b string) {
		t.Helper()
		if string(readFile(t, a)) == string(readFile(t, b)) {
			t.Fatalf("%s and %s are the same bytes: the case proves nothing", a, b)
		}
		da, db := digest(t, tools, a), digest(t, tools, b)
		if da != db {
			t.Fatalf("%s: %+v\n%s: %+v", filepath.Base(a), da, filepath.Base(b), db)
		}
	}
	t.Run("flac tags, padding and compression", func(t *testing.T) {
		a := gen(t, dir, "a.flac", "-i", wav, "-c:a", "flac", "-compression_level", "5")
		same(t, a, gen(t, dir, "a-tagged.flac", "-i", a, "-c", "copy", "-metadata", "title=Another title", "-metadata", "artist=É"))
		same(t, a, gen(t, dir, "a-level12.flac", "-i", wav, "-c:a", "flac", "-compression_level", "12"))
		same(t, a, gen(t, dir, "a-cover.flac", "-i", a, "-i", jpg, "-map", "0", "-map", "1", "-c", "copy", "-disposition:v", "attached_pic"))
		// A PNG cover, which only the test codec can add (N-073).
		png := pngImage(t, 8, 8, 3)
		same(t, a, writeFLAC(t, dir, "a-cover-png.flac", parseFLAC(t, readFile(t, a)).with(
			flacPictureData{typ: 3, mime: "image/png", width: 8, height: 8, depth: 32, data: png}.block())))
	})
	t.Run("mp3 tags", func(t *testing.T) {
		a := lame(t, wav, dir, "a.mp3", "-V", "2")
		same(t, a, lame(t, wav, dir, "a-tagged.mp3", "-V", "2", "--tt", "Title", "--ta", "Artist", "--ti", jpg))
	})
	t.Run("m4a re-mux", func(t *testing.T) {
		a := gen(t, dir, "a.m4a", "-i", wav, "-c:a", "aac")
		same(t, a, gen(t, dir, "a-faststart.m4a", "-i", a, "-c", "copy", "-movflags", "+faststart", "-metadata", "title=X"))
		l := gen(t, dir, "l.m4a", "-i", wav, "-c:a", "alac")
		same(t, l, gen(t, dir, "l-faststart.m4a", "-i", l, "-c", "copy", "-movflags", "+faststart", "-metadata", "album=Y"))
	})
	t.Run("one changed sample", func(t *testing.T) {
		b := readFile(t, wav)
		off, n := wavData(t, b)
		i := off + (n/4)*2 // a sample in the middle
		b[i] ^= 1          // one LSB
		changed := writeFile(t, filepath.Join(dir, "changed.wav"), b)
		a := digest(t, tools, gen(t, dir, "orig.flac", "-i", wav, "-c:a", "flac"))
		c := digest(t, tools, gen(t, dir, "changed.flac", "-i", changed, "-c:a", "flac"))
		if a.PCMSHA256 == c.PCMSHA256 {
			t.Fatal("one changed sample, same digest")
		}
		if a.Frames != c.Frames || a.Layout != c.Layout {
			t.Fatalf("only the hash may differ: %+v / %+v", a, c)
		}
	})
	t.Run("declared layout", func(t *testing.T) {
		six := gen(t, dir, "six.wav", append(lavfi(noise3s), "-af", "pan=5.1|c0=c0|c1=-1*c0|c2=0.5*c0|c3=0.2*c0|c4=-0.5*c0|c5=0.9*c0", "-c:a", "pcm_s16le")...)
		b := readFile(t, six)
		off, n := wavData(t, b)
		raw := writeFile(t, filepath.Join(dir, "six.raw"), b[off:off+n])
		enc := func(name, layout string) Digest {
			return digest(t, tools, gen(t, dir, name, "-f", "s16le", "-ar", "44100", "-ch_layout", layout, "-i", raw, "-c:a", "flac"))
		}
		back, side := enc("back.flac", "5.1"), enc("side.flac", "5.1(side)")
		if back.PCMSHA256 != side.PCMSHA256 || back.Frames != side.Frames {
			t.Fatalf("the same samples decode differently: %+v / %+v", back, side)
		}
		if back.Layout != "5.1" || side.Layout != "5.1(side)" {
			t.Fatalf("layouts %q / %q: the digest must carry the declared layout", back.Layout, side.Layout)
		}
	})
}

// §7.6, §8.1: a file that does not decode completely is refused, never
// passed: truncated, damaged in the middle, with a lying header, or changing
// its parameters mid-stream (which ffmpeg would otherwise resample
// silently).
func TestAudioDigestRefusesDamage(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	wav := wav16(t, dir, "source.wav", noise3s)
	flac := gen(t, dir, "ok.flac", "-i", wav, "-c:a", "flac")
	mp3CBR := lame(t, wav, dir, "cbr.mp3", "-b", "192")
	mp3VBR := lame(t, wav, dir, "vbr.mp3", "-V", "2")
	aacFast := gen(t, dir, "fast.m4a", "-i", wav, "-c:a", "aac", "-movflags", "+faststart")
	aac := gen(t, dir, "aac.m4a", "-i", wav, "-c:a", "aac")
	alacFast := gen(t, dir, "alac.m4a", "-i", wav, "-c:a", "alac", "-movflags", "+faststart")

	cut := func(name, src string, keep float64) string {
		b := readFile(t, src)
		return writeFile(t, filepath.Join(dir, name), b[:int(float64(len(b))*keep)])
	}
	damage := func(name, src string, at float64, n int) string {
		b := slices.Clone(readFile(t, src))
		i := int(float64(len(b)) * at)
		copy(b[i:], garbage(uint64(len(name)), n))
		return writeFile(t, filepath.Join(dir, name), b)
	}
	// A FLAC whose STREAMINFO declares 1152 more samples than it has: the
	// same thing as a file cut at a frame boundary.
	lying := func() string {
		b := slices.Clone(readFile(t, flac))
		si := 8 // "fLaC" + metadata block header
		total := (uint64(b[si+13]&0x0f)<<32 | uint64(binary.BigEndian.Uint32(b[si+14:]))) + 1152
		b[si+13] = b[si+13]&0xf0 | byte(total>>32)
		binary.BigEndian.PutUint32(b[si+14:], uint32(total))
		return writeFile(t, filepath.Join(dir, "lying.flac"), b)
	}()
	// Two MP3 streams of different rate and channels, back to back.
	change := func() string {
		other := gen(t, dir, "mono22.wav", append(lavfi("sine=frequency=300:sample_rate=22050:duration=3"), "-ac", "1", "-c:a", "pcm_s16le")...)
		b := append(readFile(t, mp3CBR), readFile(t, lame(t, other, dir, "mono22.mp3", "-b", "64"))...)
		return writeFile(t, filepath.Join(dir, "change.mp3"), b)
	}()

	// Single-bit corruptions of FLAC frames: most of them still parse, and
	// only the frame CRC-16 reveals them (N-074).
	flip := func(name, src string, at float64) string {
		b := slices.Clone(readFile(t, src))
		b[int(float64(len(b))*at)] ^= 0x10
		return writeFile(t, filepath.Join(dir, name), b)
	}

	for _, tc := range []struct {
		name, path, code string
	}{
		{"flac with one flipped bit at 1/4", flip("flip1.flac", flac, 0.25), CodeDecode},
		{"flac with one flipped bit at 1/2", flip("flip2.flac", flac, 0.5), CodeDecode},
		{"flac with one flipped bit at 3/4", flip("flip3.flac", flac, 0.75), CodeDecode},
		{"flac cut in half", cut("half.flac", flac, 0.5), CodeDecode},
		{"flac missing its last bytes", cut("tail.flac", flac, 0.995), CodeDecode},
		{"flac damaged in the middle", damage("mid.flac", flac, 0.5, 64), CodeDecode},
		{"flac declaring more samples than it has", lying, CodeDecode},
		{"mp3 cbr cut in half", cut("half-cbr.mp3", mp3CBR, 0.5), CodeDecode},
		{"mp3 vbr cut in half", cut("half-vbr.mp3", mp3VBR, 0.5), CodeDecode},
		{"mp3 damaged in the middle", damage("mid.mp3", mp3CBR, 0.5, 4000), CodeDecode},
		{"mp3 changing rate and channels mid-stream", change, CodeDecode},
		{"m4a (faststart) cut in half", cut("half-fast.m4a", aacFast, 0.5), CodeDecode},
		{"m4a (moov at the end) cut in half", cut("half.m4a", aac, 0.5), CodeNotSupported},
		{"m4a damaged in the middle", damage("mid.m4a", aacFast, 0.5, 4000), CodeDecode},
		{"alac cut in half", cut("half-alac.m4a", alacFast, 0.5), CodeDecode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tools.AudioDigest(t.Context(), open(t, tc.path))
			wantCode(t, err, tc.code)
		})
	}

	// The references are fine: the refusals come from the damage. An MP3
	// with CRC protection (valid CRCs) passes the CRC check.
	protected := lame(t, wav, dir, "crc.mp3", "-b", "192", "-p")
	for _, p := range []string{flac, mp3CBR, mp3VBR, aacFast, aac, alacFast, protected} {
		digest(t, tools, p)
	}
}

// Declared lengths are enforced only where they are exact: an MP3 without a
// Xing/Info header (length estimated from the bitrate) and a FLAC written
// without a total (STREAMINFO 0) are complete files and must pass.
func TestAudioDigestWithoutDeclaredLength(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	wav := wav16(t, dir, "source.wav", noise3s)
	_, frames := expectedPCM(t, readFile(t, wav))

	noXing := lame(t, wav, dir, "noxing.mp3", "-b", "192", "-t")
	if p := probe(t, tools, noXing); p.DeclaredFrames != 0 {
		t.Fatalf("an estimated MP3 length was taken as exact: %+v", p)
	}
	d := digest(t, tools, noXing)
	// Without the LAME header, the decoder delay and padding stay in.
	if d.Frames <= frames {
		t.Fatalf("%d frames for a %d-frame source without gapless information", d.Frames, frames)
	}

	// A streaming encoder writes STREAMINFO's total as 0 (unknown), since it
	// cannot seek back; ffmpeg does so when writing to a pipe. The same
	// file is built here by clearing the total of a regular encode.
	b := slices.Clone(readFile(t, gen(t, dir, "full.flac", "-i", wav, "-c:a", "flac")))
	b[8+13] &= 0xf0
	binary.BigEndian.PutUint32(b[8+14:], 0)
	piped := writeFile(t, filepath.Join(dir, "streamed.flac"), b)
	if p := probe(t, tools, piped); p.DeclaredFrames != 0 || p.Class != ClassAudio {
		t.Fatalf("FLAC without a total: %+v", p)
	}
	if d := digest(t, tools, piped); d.Frames != frames {
		t.Fatalf("FLAC without a total: %d frames, want %d", d.Frames, frames)
	}
}

// Files that are not supported audio are refused before any decode.
func TestAudioDigestRefusesUnsupported(t *testing.T) {
	dir := t.TempDir()
	fx := fixtures(t, dir)
	tools := newTools(t)
	for _, name := range []string{"empty", "text", "pdf", "wav", "opus", "video", "two-audio", "drm", "cover.jpg", "adts"} {
		t.Run(name, func(t *testing.T) {
			_, err := tools.AudioDigest(t.Context(), open(t, fx[name]))
			wantCode(t, err, CodeNotSupported)
		})
	}
}

// The decoder's command line is §8.4's, pinned. Nothing may resample,
// remix, filter or normalize.
func TestDecodeArgs(t *testing.T) {
	want := []string{
		"-hide_banner", "-nostdin", "-nostats", "-loglevel", "error", "-xerror",
		"-threads", "1", "-err_detect", "crccheck+explode", "-reinit_filter", "0",
		"-protocol_whitelist", "fd", "-fd", "3", "-f", "flac", "-i", "fd:",
		"-map", "0:a:0", "-vn", "-sn", "-dn", "-c:a", "pcm_f64le",
		"-threads", "1", "-filter_threads", "1", "-f", "f64le", "pipe:1",
	}
	if got := decodeArgs("flac"); !slices.Equal(got, want) {
		t.Fatalf("decodeArgs:\n got %q\nwant %q", got, want)
	}
	for _, forbidden := range []string{"-ar", "-ac", "-af", "-filter:a", "-filter_complex", "-sample_fmt", "-channel_layout", "-ch_layout"} {
		if slices.Contains(decodeArgs("mov"), forbidden) {
			t.Fatalf("decodeArgs contains %s", forbidden)
		}
	}
	for format, demuxer := range map[string]string{FormatFLAC: "flac", FormatMP3: "mp3", FormatM4AAAC: "mov", FormatM4AALAC: "mov"} {
		if demuxerOf[format] != demuxer {
			t.Fatalf("demuxer of %s: %q", format, demuxerOf[format])
		}
	}
	if len(demuxerOf) != 4 {
		t.Fatalf("demuxerOf has %d entries", len(demuxerOf))
	}
}

// BenchmarkAudioDigest5MinFLAC measures AudioDigest (probe + full decode +
// SHA-256) on a typical track: 5 minutes of 44.1 kHz 16-bit stereo pink
// noise, about 36 MB of FLAC (N-079).
//
//	scripts/dev.sh go test -run '^$' -bench AudioDigest5Min -benchtime 10x ./internal/media/
func BenchmarkAudioDigest5MinFLAC(b *testing.B) {
	dir := b.TempDir()
	tools := newTools(b)
	p := gen(b, dir, "long.flac", append(lavfi("anoisesrc=color=pink:sample_rate=44100:seed=42:duration=300"), "-ac", "2", "-c:a", "flac")...)
	f := open(b, p)
	b.ResetTimer()
	for b.Loop() {
		d, err := tools.AudioDigest(b.Context(), f)
		if err != nil {
			b.Fatal(err)
		}
		if d.Frames != 300*44100 {
			b.Fatalf("%d frames", d.Frames)
		}
	}
	b.ReportMetric(float64(300*44100*2*8)/1e6, "MB_pcm/op")
}

// The checks on the decoder's output, with a fake ffmpeg (and the real
// ffprobe): a non-zero exit is a failure even after plausible PCM (§8.5),
// empty output is an error (§8.4), and so is a partial frame.
func TestAudioDigestDecoderFailures(t *testing.T) {
	dir := t.TempDir()
	flac := gen(t, dir, "ok.flac", append(lavfi(sine3s), "-ac", "2", "-c:a", "flac")...)
	for _, tc := range []struct {
		name, script, msg string
	}{
		{"exit 1 after plausible output", "head -c 16000 /dev/zero; exit 1", "exit status 1"},
		{"killed", "head -c 16000 /dev/zero; kill -KILL $$", "signal"},
		{"no output", "exit 0", "no audio"},
		{"partial frame", "head -c 16008 /dev/zero", "not a whole number"},
		{"wrong length", "head -c 16000 /dev/zero", "the container declares 132300"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := writeFile(t, filepath.Join(dir, "ffmpeg-"+tc.name), []byte("#!/bin/sh\n"+tc.script+"\n"))
			if err := os.Chmod(p, 0o755); err != nil {
				t.Fatal(err)
			}
			tools := &Tools{run: NewRunner(1), ffmpeg: p, ffprobe: FFprobePath}
			_, err := tools.AudioDigest(t.Context(), open(t, flac))
			e := wantCode(t, err, CodeDecode)
			if !strings.Contains(e.Error(), tc.msg) {
				t.Fatalf("error %q does not say %q", e.Error(), tc.msg)
			}
		})
	}
}

// The probe's command line, pinned: in particular only the "fd" protocol
// is allowed (N-075) and the log level lets the MP3 estimation warning
// through (N-078).
func TestProbeArgs(t *testing.T) {
	want := []string{
		"-hide_banner", "-loglevel", "warning", "-show_error", "-print_format", "json",
		"-show_entries", probeEntries, "-read_intervals", "%+#16",
		"-protocol_whitelist", "fd", "-fd", "3", "fd:",
	}
	if got := probeArgs(); !slices.Equal(got, want) {
		t.Fatalf("probeArgs:\n got %q\nwant %q", got, want)
	}
}
