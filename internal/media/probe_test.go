package media

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtures generates one of each kind of file the probe must classify, in
// dir, and returns their paths by name. Everything comes from lavfi
// sources, the pinned LAME and Go's image/png (the pinned ffmpeg has no PNG
// encoder: it is built without zlib, N-073).
func fixtures(t testing.TB, dir string) map[string]string {
	t.Helper()
	wav := wav16(t, dir, "source.wav", sine3s)
	jpg := gen(t, dir, "cover.jpg", append(lavfi("color=c=red:size=32x32"), "-frames:v", "1")...)
	f := map[string]string{
		"wav":        wav,
		"cover.jpg":  jpg,
		"flac16":     gen(t, dir, "s16.flac", "-i", wav, "-c:a", "flac"),
		"flac24":     gen(t, dir, "s24.flac", append(lavfi(noise3s), "-ac", "2", "-c:a", "flac", "-sample_fmt", "s32", "-bits_per_raw_sample", "24")...),
		"flac-mono":  gen(t, dir, "mono.flac", append(lavfi(sine3s), "-ac", "1", "-c:a", "flac")...),
		"flac-5.1":   gen(t, dir, "six.flac", append(lavfi(sine3s), "-af", "pan=5.1|c0=c0|c1=c0|c2=c0|c3=c0|c4=c0|c5=c0", "-c:a", "flac")...),
		"flac-cover": gen(t, dir, "cover.flac", "-i", wav, "-i", jpg, "-map", "0", "-map", "1", "-c:a", "flac", "-c:v", "copy", "-disposition:v", "attached_pic"),
		"mp3-cbr":    lame(t, wav, dir, "cbr.mp3", "-b", "192"),
		"mp3-vbr":    lame(t, wav, dir, "vbr.mp3", "-V", "2"),
		"mp3-noxing": lame(t, wav, dir, "noxing.mp3", "-b", "192", "-t"),
		"mp3-cover":  lame(t, wav, dir, "cover.mp3", "-b", "128", "--ti", jpg),
		"aac":        gen(t, dir, "aac.m4a", "-i", wav, "-c:a", "aac", "-b:a", "128k"),
		"alac":       gen(t, dir, "alac.m4a", "-i", wav, "-c:a", "alac"),
		"aac-cover":  gen(t, dir, "aac-cover.m4a", "-i", wav, "-i", jpg, "-map", "0", "-map", "1", "-c:a", "aac", "-c:v", "copy", "-disposition:v", "attached_pic"),
		"video":      gen(t, dir, "video.mp4", append(lavfi("testsrc=size=64x64:rate=5:duration=3"), "-i", wav, "-c:v", "mpeg4", "-c:a", "aac")...),
		"video-only": gen(t, dir, "silent.mp4", append(lavfi("testsrc=size=64x64:rate=5:duration=3"), "-c:v", "mpeg4")...),
		"two-audio":  gen(t, dir, "two.m4a", "-i", wav, "-i", wav, "-map", "0", "-map", "1", "-c:a", "aac"),
		"opus":       gen(t, dir, "s.opus", "-i", wav, "-c:a", "opus", "-strict", "experimental"),
		"adts":       gen(t, dir, "s.aac", "-i", wav, "-c:a", "aac", "-f", "adts"),
		"mp2":        gen(t, dir, "layer2.mp3", "-i", wav, "-c:a", "mp2", "-f", "mp2"),
		"drm":        gen(t, dir, "drm.m4a", "-i", wav, "-c:a", "aac", "-encryption_scheme", "cenc-aes-ctr", "-encryption_key", "00112233445566778899aabbccddeeff", "-encryption_kid", "00112233445566778899aabbccddeeff"),
		"silence":    gen(t, dir, "silence.flac", append(lavfi("anullsrc=channel_layout=stereo:sample_rate=48000"), "-t", "2", "-c:a", "flac")...),
		"empty":      writeFile(t, filepath.Join(dir, "empty.flac"), nil),
		"text":       writeFile(t, filepath.Join(dir, "notes.txt"), []byte("Ripped with care.\nTrack list:\n01 Intro\n")),
		"pdf":        writeFile(t, filepath.Join(dir, "booklet.pdf"), []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")),
		"m3u":        writeFile(t, filepath.Join(dir, "playlist.m3u"), []byte("#EXTM3U\n#EXTINF:3,Artist - Title\ns16.flac\n")),
		"hls":        writeFile(t, filepath.Join(dir, "stream.m3u8"), []byte("#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXT-X-VERSION:3\n#EXTINF:3.0,\n"+filepath.Join(dir, "s16.flac")+"\n#EXT-X-ENDLIST\n")),
		"concat-abs": writeFile(t, filepath.Join(dir, "list-abs.txt"), []byte("ffconcat version 1.0\nfile "+filepath.Join(dir, "s16.flac")+"\n")),
		"concat-rel": writeFile(t, filepath.Join(dir, "list-rel.txt"), []byte("ffconcat version 1.0\nfile s16.flac\n")),
	}
	var img bytes.Buffer
	m := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for i := range m.Pix {
		m.Pix[i] = byte(i)
	}
	m.Set(0, 0, color.RGBA{1, 2, 3, 255})
	if err := png.Encode(&img, m); err != nil {
		t.Fatal(err)
	}
	f["cover.png"] = writeFile(t, filepath.Join(dir, "cover.png"), img.Bytes())
	// A PNG front cover in a FLAC PICTURE block. The pinned ffmpeg cannot mux
	// it (no PNG decoder, N-073), so the independent codec of
	// flacmeta_test.go writes it.
	f["flac-cover-png"] = writeFLAC(t, dir, "cover-png.flac", parseFLAC(t, readFile(t, f["flac16"])).with(
		flacPictureData{typ: 3, mime: "image/png", width: 16, height: 16, depth: 32, data: img.Bytes()}.block()))
	return f
}

// §7.2, §8.1: the classification of every kind of file, from its content.
func TestProbeClassifies(t *testing.T) {
	dir := t.TempDir()
	fx := fixtures(t, dir)
	tools := newTools(t)
	stereo := func(codec string) AudioInfo {
		return AudioInfo{Codec: codec, SampleRate: 44100, Channels: 2, Layout: "stereo", Duration: 3 * time.Second}
	}
	for _, tc := range []struct {
		fixture string
		want    ProbeResult
	}{
		{"flac16", ProbeResult{Class: ClassAudio, Container: "flac", Format: FormatFLAC, Audio: stereo("flac"), DeclaredFrames: 132300}},
		{"flac24", ProbeResult{Class: ClassAudio, Container: "flac", Format: FormatFLAC, Audio: stereo("flac"), DeclaredFrames: 132300}},
		{"flac-mono", ProbeResult{Class: ClassAudio, Container: "flac", Format: FormatFLAC, DeclaredFrames: 132300,
			Audio: AudioInfo{Codec: "flac", SampleRate: 44100, Channels: 1, Layout: "mono", Duration: 3 * time.Second}}},
		{"flac-5.1", ProbeResult{Class: ClassAudio, Container: "flac", Format: FormatFLAC, DeclaredFrames: 132300,
			Audio: AudioInfo{Codec: "flac", SampleRate: 44100, Channels: 6, Layout: "5.1", Duration: 3 * time.Second}}},
		{"flac-cover", ProbeResult{Class: ClassAudio, Container: "flac", Format: FormatFLAC, Audio: stereo("flac"), DeclaredFrames: 132300, AttachedPictures: 1}},
		{"flac-cover-png", ProbeResult{Class: ClassAudio, Container: "flac", Format: FormatFLAC, Audio: stereo("flac"), DeclaredFrames: 132300, AttachedPictures: 1}},
		{"silence", ProbeResult{Class: ClassAudio, Container: "flac", Format: FormatFLAC, DeclaredFrames: 96000,
			Audio: AudioInfo{Codec: "flac", SampleRate: 48000, Channels: 2, Layout: "stereo", Duration: 2 * time.Second}}},
		{"mp3-cbr", ProbeResult{Class: ClassAudio, Container: "mp3", Format: FormatMP3, Audio: stereo("mp3"), DeclaredFrames: 132300}},
		{"mp3-vbr", ProbeResult{Class: ClassAudio, Container: "mp3", Format: FormatMP3, Audio: stereo("mp3"), DeclaredFrames: 132300}},
		{"mp3-cover", ProbeResult{Class: ClassAudio, Container: "mp3", Format: FormatMP3, Audio: stereo("mp3"), DeclaredFrames: 132300, AttachedPictures: 1}},
		{"aac", ProbeResult{Class: ClassAudio, Container: containerMOV, Format: FormatM4AAAC, Audio: stereo("aac")}},
		{"alac", ProbeResult{Class: ClassAudio, Container: containerMOV, Format: FormatM4AALAC, Audio: stereo("alac")}},
		{"aac-cover", ProbeResult{Class: ClassAudio, Container: containerMOV, Format: FormatM4AAAC, Audio: stereo("aac"), AttachedPictures: 1}},

		{"video", ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonVideo, Container: containerMOV,
			Detail: "1 video stream(s) that are not attached pictures"}},
		{"two-audio", ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonMultipleAudio, Container: containerMOV, Detail: "2 audio streams"}},
		{"opus", ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonUnsupportedFormat, Container: "ogg", Detail: "codec opus in container ogg"}},
		{"wav", ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonUnsupportedFormat, Container: "wav", Detail: "codec pcm_s16le in container wav"}},
		{"adts", ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonUnsupportedFormat, Container: "aac", Detail: "codec aac in container aac"}},
		{"mp2", ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonUnsupportedFormat, Container: "mp3", Detail: "codec mp2 in container mp3"}},
		{"drm", ProbeResult{Class: ClassUnsupportedAudio, Reason: ReasonEncrypted, Container: containerMOV,
			Detail: "the packets carry encryption (DRM) information"}},

		{"cover.jpg", ProbeResult{Class: ClassNoAudio, Container: "jpeg_pipe"}},
		{"cover.png", ProbeResult{Class: ClassNoAudio, Container: "png_pipe"}},
		{"video-only", ProbeResult{Class: ClassNoAudio, Container: containerMOV}},

		{"empty", unreadableResult},
		{"text", unreadableResult},
		{"pdf", unreadableResult},
		{"m3u", unreadableResult},
		{"hls", unreadableResult},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			if got := probe(t, tools, fx[tc.fixture]); got != tc.want {
				t.Fatalf("Probe(%s):\n got %+v\nwant %+v", filepath.Base(fx[tc.fixture]), got, tc.want)
			}
		})
	}
}

var unreadableResult = ProbeResult{Class: ClassUnreadable, Reason: ReasonProbeFailed,
	Detail: "Invalid data found when processing input (-1094995529)"}

// The tool reads only the descriptor it is given (§8.5, N-075): a playlist
// or a concat list naming a supported file next to it is not followed. It is
// unreadable, and the file it names is never opened.
func TestProbeDoesNotFollowReferences(t *testing.T) {
	dir := t.TempDir()
	fx := fixtures(t, dir)
	tools := newTools(t)
	for _, name := range []string{"hls", "concat-abs", "concat-rel", "m3u"} {
		t.Run(name, func(t *testing.T) {
			p := probe(t, tools, fx[name])
			if p.Class != ClassUnreadable {
				t.Fatalf("a reference file was classified %+v", p)
			}
		})
	}
	// ffprobe runs with only the fd protocol, so a path-based open fails
	// even when asked for directly.
	var out bytes.Buffer
	_, err := tools.run.Run(t.Context(), Command{
		Path: FFprobePath, Args: []string{"-v", "error", "-protocol_whitelist", "fd", "-show_error", "-print_format", "json", fx["flac16"]},
		Stdout: &out, Timeout: InspectTimeout,
	})
	wantCode(t, err, CodeToolFailed)
	if !strings.Contains(out.String(), `"error"`) {
		t.Fatalf("output %q", out.String())
	}
}

// Content decides, never the name (§4.2, §7.2): the same bytes classify the
// same under any name, including Unicode names and names with no extension
// or a misleading one.
func TestProbeIgnoresNames(t *testing.T) {
	dir := t.TempDir()
	fx := fixtures(t, dir)
	tools := newTools(t)
	flac := readFile(t, fx["flac16"])
	jpg := readFile(t, fx["cover.jpg"])
	want := probe(t, tools, fx["flac16"])
	for _, name := range []string{
		"Café del Mar — Été 音楽 \U0001F3B5.flac",
		"Café (NFD).flac",
		"-i.flac",
		"file:evil.flac",
		"no extension",
		"looks like a cover.jpg",
		"x.opus",
		" leading and trailing spaces .flac ",
	} {
		t.Run(name, func(t *testing.T) {
			p := writeFile(t, filepath.Join(dir, name), flac)
			if got := probe(t, tools, p); got != want {
				t.Fatalf("Probe(%q) = %+v, want %+v", name, got, want)
			}
		})
	}
	named := writeFile(t, filepath.Join(dir, "Été.flac"), jpg)
	if got := probe(t, tools, named); got.Class != ClassNoAudio {
		t.Fatalf("a JPEG named .flac: %+v", got)
	}
}

// Failures of ffprobe itself are errors, never a classification: a non-zero
// exit without ffprobe's error object (a bad option), a non-zero exit with
// plausible output, a crash, malformed output, and an I/O error on the
// input.
func TestProbeToolFailures(t *testing.T) {
	dir := t.TempDir()
	input := writeFile(t, filepath.Join(dir, "input"), []byte("anything"))
	fake := func(name, script string) *Tools {
		p := writeFile(t, filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"))
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return &Tools{run: NewRunner(1), ffmpeg: FFmpegPath, ffprobe: p}
	}
	audioJSON := `{"streams": [{"codec_type": "audio", "codec_name": "flac", "sample_rate": "44100", "channels": 2}], "format": {"format_name": "flac"}}`
	for _, tc := range []struct {
		name, script, code string
	}{
		{"bad option", `echo "Unrecognized option 'x'" >&2; exit 1`, CodeToolFailed},
		{"exit 1 with audio output", `printf '%s' '` + audioJSON + `'; exit 1`, CodeToolFailed},
		{"exit 2 with an error object", `printf '{"error": {"code": -1094995529, "string": "Invalid data"}}'; exit 2`, CodeToolFailed},
		{"crash", `printf '%s' '` + audioJSON + `'; kill -SEGV $$`, CodeToolFailed},
		{"not JSON", `echo 'streams: none'`, CodeOutputInvalid},
		{"no format section", `echo '{"streams": []}'`, CodeOutputInvalid},
		{"I/O error on the input", `printf '{"error": {"code": -5, "string": "Input/output error"}}'; exit 1`, CodeToolFailed},
		{"out of memory", `printf '{"error": {"code": -12, "string": "Cannot allocate memory"}}'; exit 1`, CodeToolFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fake(strings.NewReplacer(" ", "-", "/", "-").Replace(tc.name), tc.script).Probe(t.Context(), open(t, input))
			wantCode(t, err, tc.code)
		})
	}

	// The reference: the same error object with exit 1 is a classification.
	p, err := fake("invalid-data", `printf '{"error": {"code": -1094995529, "string": "Invalid data"}}'; exit 1`).Probe(t.Context(), open(t, input))
	if err != nil || p.Class != ClassUnreadable {
		t.Fatalf("exit 1 with an error object: %+v, %v", p, err)
	}

	// An input that cannot be rewound is refused before any tool runs.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, err = newTools(t).Probe(t.Context(), r)
	wantCode(t, err, CodeIO)
}

// The offset of the input is irrelevant: Probe rewinds it, so a file the
// caller has already read (to hash it, for example) probes the same.
func TestProbeRewinds(t *testing.T) {
	dir := t.TempDir()
	path := gen(t, dir, "a.flac", append(lavfi(sine3s), "-c:a", "flac")...)
	tools := newTools(t)
	f := open(t, path)
	if _, err := f.Seek(0, 2); err != nil {
		t.Fatal(err)
	}
	p, err := tools.Probe(t.Context(), f)
	if err != nil || p.Class != ClassAudio {
		t.Fatalf("probe after reading to EOF: %+v, %v", p, err)
	}
	p2, err := tools.Probe(t.Context(), f)
	if err != nil || p2 != p {
		t.Fatalf("second probe of the same descriptor: %+v, %v", p2, err)
	}
}

// The MP3 estimation signal, on the real tool (N-078): the pinned ffprobe
// prints the warning for an MP3 without a Xing/Info header and not for one
// with a LAME header. A bump of FFmpeg that changes the wording fails here.
func TestMP3EstimationWarningOfThePinnedTool(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	wav := wav16(t, dir, "source.wav", sine3s)
	for name, want := range map[string]bool{
		lame(t, wav, dir, "noxing.mp3", "-b", "192", "-t"): true,
		lame(t, wav, dir, "lame.mp3", "-b", "192"):         false,
		lame(t, wav, dir, "vbr.mp3", "-V", "4"):            false,
	} {
		res, err := tools.run.Run(t.Context(), Command{
			Path: FFprobePath, Args: probeArgs(), Files: []*os.File{open(t, name)},
			Stdout: io.Discard, Timeout: InspectTimeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := mp3Estimated(res.Stderr); got != want {
			t.Fatalf("%s: estimated %v, want %v; stderr:\n%s", filepath.Base(name), got, want, res.Stderr)
		}
	}
}

// DurationMS (NOTES.md N-300, N-307): whole milliseconds, rounded down; a
// container that declares no duration (0) is unknown, never a 0 ms track.
func TestDurationMS(t *testing.T) {
	for _, tc := range []struct {
		d  time.Duration
		ms int64
		ok bool
	}{
		{0, 0, false},
		{-time.Second, 0, false},
		{1500*time.Millisecond + 999*time.Microsecond, 1500, true},
		{9*time.Minute + 22*time.Second, 562_000, true},
	} {
		if ms, ok := DurationMS(tc.d); ms != tc.ms || ok != tc.ok {
			t.Errorf("DurationMS(%v) = %d, %v; want %d, %v", tc.d, ms, ok, tc.ms, tc.ok)
		}
	}
}
