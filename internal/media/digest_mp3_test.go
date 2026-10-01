package media

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The MP3 decode window (NOTES.md N-154): the digest of an MP3 is the one of
// its audio, whatever tags surround it, gapless trimming included, before
// and after a write; the bytes left out are exactly the ones the helper
// places after the audio.

// mp3Variants returns the source and the same audio with tags around it.
func mp3Variants(t testing.TB, src []byte) map[string][]byte {
	managed, unmanaged := richV23()
	big := make([]byte, len(src)/2) // an APE tag of more than 1/16 of the file
	ape := apeTag(true, apeItem("Title", 0, []byte("ape")), apeItem("Mood", 0, []byte("calm")))
	bigAPE := apeTag(true, apeItem("Cover Art (Front)", 2, append([]byte("c.jpg\x00"), big...)), apeItem("Mood", 0, []byte("x")))
	v1 := id3v1("t", "a", "b", "2000", "comment", 1, 17)
	return map[string][]byte{
		"plain":            src,
		"ID3v2.3":          slices.Concat(id3Tag(3, 0x80, 100, append(managed, unmanaged...)...), src),
		"ID3v2.4":          slices.Concat(id3Tag(4, 0, 5000, frame("TIT2", textBody(encUTF8, "t"))), src),
		"ID3v1":            slices.Concat(src, v1),
		"APE":              slices.Concat(src, ape),
		"APEv1":            slices.Concat(src, apeTag(false, apeItem("Mood", 0, []byte("calm")))),
		"APE and ID3v1":    slices.Concat(src, ape, v1),
		"large APE":        slices.Concat(src, bigAPE),
		"large APE, ID3v1": slices.Concat(src, bigAPE, v1),
		"all":              slices.Concat(id3Tag(3, 0, 0, managed...), src, bigAPE, v1),
	}
}

func TestAudioDigestMP3Tags(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	wav := wav16(t, dir, "src.wav", sine3s)
	_, size := wavData(t, readFile(t, wav))
	frames := size / 4 // 16-bit stereo
	for _, enc := range [][]string{{"-V2"}, {"-b", "128"}} {
		src := readFile(t, lame(t, wav, dir, "src.mp3", enc...))
		d0 := digest(t, tools, mp3At(t, t.TempDir(), "plain.mp3", src))
		if d0.Frames != int64(frames) {
			t.Fatalf("%v: %d frames decoded, the source has %d: the gapless trimming is off", enc, d0.Frames, frames)
		}
		for name, b := range mp3Variants(t, src) {
			t.Run(enc[0]+"/"+name, func(t *testing.T) {
				p := mp3At(t, t.TempDir(), "v.mp3", b)
				if d := digest(t, tools, p); d != d0 {
					t.Fatalf("digest %+v, the audio alone gives %+v", d, d0)
				}
				// The write keeps it (writeMP3Checked compares before and
				// after), and so the digest of the plain audio.
				writeMP3Checked(t, tools, p, fullValues, nil)
				if d := digest(t, tools, p); d != d0 {
					t.Fatalf("digest after the write %+v", d)
				}
			})
		}
	}
}

// The window is the helper's own: mp3AudioEnd and the helper's audio range
// agree on every file, on both helpers.
func TestMP3AudioEndIsTheHelpersRule(t *testing.T) {
	dir := t.TempDir()
	src := mp3Source(t, dir)
	cases := mp3Variants(t, src)
	// An APE footer 131 bytes from the end: its "TAG" is not an ID3v1 tag.
	cases["APETAGEX 131 bytes from the end"] = slices.Concat(src, apeTag(false, apeItem("Mood", 0, make([]byte, 60))),
		apeTag(false, apeItem("Mood", 0, make([]byte, 54)))) // 99 bytes after the first footer
	for name, tools := range helpers(t) {
		for cname, b := range cases {
			t.Run(name+"/"+cname, func(t *testing.T) {
				p := mp3At(t, t.TempDir(), "v.mp3", b)
				end, err := mp3AudioEnd(open(t, p))
				if err != nil {
					t.Fatal(err)
				}
				if end == wholeFile {
					end = int64(len(b))
				}
				if in := inspectMP3(t, tools, p); in.Audio.End != end {
					t.Fatalf("mp3AudioEnd %d, the helper's audio ends at %d", end, in.Audio.End)
				}
			})
		}
	}
}

// Trailing tags that cannot be what they claim, and bytes after the audio
// that are no tag, are refused by the decode.
func TestAudioDigestMP3Refusals(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := mp3Source(t, dir)
	ape := apeTag(true, apeItem("Mood", 0, []byte("calm")))
	footer := len(src) + len(ape) - 32
	for name, mut := range map[string]func([]byte) []byte{
		"APE of 10 bytes":          func(b []byte) []byte { binary.LittleEndian.PutUint32(b[footer+12:], 10); return b },
		"APE larger than the file": func(b []byte) []byte { binary.LittleEndian.PutUint32(b[footer+12:], 1<<30); return b },
		"junk after the audio":     func([]byte) []byte { return slices.Concat(src, make([]byte, 500)) },
		// The declared length comes from the probe of the window: whole-file,
		// the demuxer drops the Xing count behind an APE tag this large, and
		// the truncation would go unseen (N-154).
		"audio cut short before a large APE tag": func([]byte) []byte {
			return slices.Concat(src[:len(src)-2000], apeTag(true, apeItem("Blob", 2, make([]byte, len(src)))))
		},
		"Lyrics3 before an ID3v1": func([]byte) []byte {
			return slices.Concat(src, []byte("LYRICSBEGINxxLYRICS200"), id3v1("", "", "", "", "", 0, 0))
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := writeFile(t, filepath.Join(t.TempDir(), "f.mp3"), mut(slices.Concat(src, ape)))
			_, err := tools.AudioDigest(t.Context(), open(t, p))
			wantCode(t, err, CodeDecode)
		})
	}
}

func TestWindowed(t *testing.T) {
	got := windowed(decodeArgs("mp3"), 12345)
	want := slices.Clone(decodeArgs("mp3"))
	for i, a := range want {
		switch a {
		case "fd":
			want[i] = "subfile,fd"
		case "fd:":
			want[i] = "subfile,,start,0,end,12345,,:fd:"
		}
	}
	if !slices.Equal(got, want) || slices.Contains(got, "file") {
		t.Fatalf("windowed:\n got %q\nwant %q", got, want)
	}
	if !slices.Equal(windowed(probeArgs(), 7)[len(probeArgs())-1:], []string{"subfile,,start,0,end,7,,:fd:"}) {
		t.Fatalf("windowed probe: %q", windowed(probeArgs(), 7))
	}
}

// The gapless length of an MP3: the decode of a complete file is the
// declared length less exactly the shortfall its header implies, never a
// range. The parser is tested on synthetic bytes without any tool; the rule
// against the pinned FFmpeg on files from the pinned LAME.

// Frame headers of Layer III at 128 kbit/s (80 for MPEG-2 and 2.5).
const (
	hdrMPEG1Stereo  = 0xFFFB9000 // channel mode 00
	hdrMPEG1Joint   = 0xFFFB9040 // 01
	hdrMPEG1Dual    = 0xFFFB9080 // 10
	hdrMPEG1Mono    = 0xFFFB90C0 // 11
	hdrMPEG2Stereo  = 0xFFF39000
	hdrMPEG2Mono    = 0xFFF390C0
	hdrMPEG25Stereo = 0xFFE39000
	hdrMPEG25Mono   = 0xFFE390C0
)

// gaplessFrame builds the start of a first frame: header h, side
// information of off bytes, a header id ("Xing", "Info") with flags and the
// fields they announce (a frame count of 1000), the encoder string enc, the
// LAME fields and the delay and padding field, then the rest of the frame.
// The fields that are not read are filled with non-zero bytes, so a wrong
// offset reads a wrong value.
func gaplessFrame(h uint32, off int, id string, flags uint32, enc string, delay, padding int) []byte {
	b := binary.BigEndian.AppendUint32(nil, h)
	b = append(b, bytes.Repeat([]byte{0x11}, off)...)
	b = append(b, id...)
	b = binary.BigEndian.AppendUint32(b, flags)
	if flags&xingFrames != 0 {
		b = binary.BigEndian.AppendUint32(b, 1000)
	}
	if flags&xingBytes != 0 {
		b = binary.BigEndian.AppendUint32(b, 0xAAAAAAAA)
	}
	if flags&xingTOC != 0 {
		b = append(b, bytes.Repeat([]byte{0x55}, 100)...)
	}
	if flags&xingQuality != 0 {
		b = binary.BigEndian.AppendUint32(b, 0x77777777)
	}
	b = append(b, (enc + strings.Repeat("\x00", 9))[:9]...)
	b = append(b, bytes.Repeat([]byte{0x99}, lameFieldsBeforeDelays)...)
	v := delay<<12 | padding
	b = append(b, byte(v>>16), byte(v>>8), byte(v))
	return append(b, bytes.Repeat([]byte{0x33}, 200)...)
}

func TestParseMP3Gapless(t *testing.T) {
	headers := []struct {
		name string
		h    uint32
		off  int
		spf  int64
	}{
		{"MPEG-1 stereo", hdrMPEG1Stereo, 32, 1152},
		{"MPEG-1 joint stereo", hdrMPEG1Joint, 32, 1152},
		{"MPEG-1 dual channel", hdrMPEG1Dual, 32, 1152},
		{"MPEG-1 mono", hdrMPEG1Mono, 17, 1152},
		{"MPEG-2 stereo", hdrMPEG2Stereo, 17, 576},
		{"MPEG-2 mono", hdrMPEG2Mono, 9, 576},
		{"MPEG-2.5 stereo", hdrMPEG25Stereo, 17, 576},
		{"MPEG-2.5 mono", hdrMPEG25Mono, 9, 576},
	}
	for _, hc := range headers {
		for flags := range uint32(16) {
			for _, id := range []string{"Xing", "Info"} {
				for _, enc := range []string{"LAME3.100", "Lavf55.37", "Lavc60.31", "LAME"} {
					b := gaplessFrame(hc.h, hc.off, id, flags, enc, 576, 300)
					g, ok := parseMP3Gapless(b)
					want := mp3Gapless{frames: 1000, spf: hc.spf, delay: 576, padding: 300}
					if flags&xingFrames == 0 {
						// No frame count: the demuxer declares no exact
						// length from this header.
						want, ok = mp3Gapless{}, !ok
					}
					if !ok || g != want {
						t.Fatalf("%s %s flags %d %q: %+v, %v; want %+v", hc.name, id, flags, enc, g, ok, want)
					}
				}
			}
		}
	}

	// The delay and padding are the two 12-bit halves of the field.
	for _, dp := range [][2]int{{0, 0}, {576, 0}, {0, 4095}, {4095, 4095}, {1105, 1152}} {
		g, ok := parseMP3Gapless(gaplessFrame(hdrMPEG1Stereo, 32, "Info", 15, "Lavf", dp[0], dp[1]))
		if !ok || g.delay != int64(dp[0]) || g.padding != int64(dp[1]) {
			t.Fatalf("delay %d padding %d: %+v, %v", dp[0], dp[1], g, ok)
		}
	}

	// No padding known.
	good := gaplessFrame(hdrMPEG1Stereo, 32, "Info", 15, "LAME3.100", 576, 0)
	for name, b := range map[string][]byte{
		"empty":                    nil,
		"garbage":                  garbage(1, 400),
		"zeros":                    make([]byte, 400),
		"another encoder":          gaplessFrame(hdrMPEG1Stereo, 32, "Info", 15, "GOGO3.13", 576, 0),
		"lower-case lame":          gaplessFrame(hdrMPEG1Stereo, 32, "Info", 15, "lame3.100", 576, 0),
		"no encoder string":        gaplessFrame(hdrMPEG1Stereo, 32, "Info", 15, "", 576, 0),
		"lower-case xing":          gaplessFrame(hdrMPEG1Stereo, 32, "xing", 15, "LAME3.100", 576, 0),
		"VBRI":                     gaplessFrame(hdrMPEG1Stereo, 32, "VBRI", 15, "LAME3.100", 576, 0),
		"mono at the stereo place": gaplessFrame(hdrMPEG1Mono, 32, "Info", 15, "LAME3.100", 576, 0),
		"stereo at the mono place": gaplessFrame(hdrMPEG1Stereo, 17, "Info", 15, "LAME3.100", 576, 0),
		"MPEG-2 at MPEG-1's place": gaplessFrame(hdrMPEG2Stereo, 32, "Info", 15, "LAME3.100", 576, 0),
		"Layer I":                  gaplessFrame(0xFFFF9000, 32, "Info", 15, "LAME3.100", 576, 0),
		"Layer II":                 gaplessFrame(0xFFFD9000, 32, "Info", 15, "LAME3.100", 576, 0),
		"reserved version":         gaplessFrame(0xFFEB9000, 32, "Info", 15, "LAME3.100", 576, 0),
		"bad bitrate":              gaplessFrame(0xFFFBF000, 32, "Info", 15, "LAME3.100", 576, 0),
		"bad sample rate":          gaplessFrame(0xFFFB9C00, 32, "Info", 15, "LAME3.100", 576, 0),
		"no sync":                  gaplessFrame(0x7FFB9000, 32, "Info", 15, "LAME3.100", 576, 0),
		"an ID3v2 tag first":       slices.Concat(id3Tag(3, 0, 10), good),
		"a byte before the frame":  slices.Concat([]byte{0}, good),
	} {
		if g, ok := parseMP3Gapless(b); ok {
			t.Fatalf("%s: %+v", name, g)
		}
	}
	// Truncated anywhere before the end of the delay and padding field.
	end := mp3HeaderSize + 32 + 8 + 4 + 4 + 100 + 4 + 9 + lameFieldsBeforeDelays + 3
	if end != mp3GaplessWindow {
		t.Fatalf("the full header ends at %d, mp3GaplessWindow is %d", end, mp3GaplessWindow)
	}
	for n := range end {
		if g, ok := parseMP3Gapless(good[:n]); ok {
			t.Fatalf("%d bytes: %+v", n, g)
		}
	}
	if _, ok := parseMP3Gapless(good[:end]); !ok {
		t.Fatal("the complete header is refused")
	}
}

func TestMP3GaplessShortfallRule(t *testing.T) {
	for padding, want := range map[int64]int64{0: 529, 1: 528, 300: 229, 528: 1, 529: 0, 530: 0, 1152: 0, 4095: 0} {
		g := mp3Gapless{frames: 100, spf: 1152, delay: 576, padding: padding}
		if got := g.shortfall(); got != want {
			t.Fatalf("padding %d: shortfall %d, want %d", padding, got, want)
		}
		if got := g.declared(); got != 100*1152-576-padding {
			t.Fatalf("padding %d: declared %d", padding, got)
		}
	}
}

func TestID3v2TagSize(t *testing.T) {
	for name, tc := range map[string]struct {
		b    []byte
		size int64
		ok   bool
	}{
		"ID3v2.3":                  {[]byte("ID3\x03\x00\x00\x00\x00\x02\x01"), 10 + 257, true},
		"ID3v2.4":                  {[]byte("ID3\x04\x00\x00\x7f\x7f\x7f\x7f"), 10 + 1<<28 - 1, true},
		"ID3v2.4 with a footer":    {[]byte("ID3\x04\x00\x10\x00\x00\x00\x05"), 10 + 5 + 10, true},
		"ID3v2.3, footer flag":     {[]byte("ID3\x03\x00\x10\x00\x00\x00\x05"), 10 + 5, true},
		"ID3v2.2":                  {[]byte("ID3\x02\x00\x00\x00\x00\x00\x00"), 10, true},
		"unknown version, skipped": {[]byte("ID3\x07\x00\x00\x00\x00\x01\x00"), 10 + 128, true},
		"version 0xFF":             {[]byte("ID3\xff\x00\x00\x00\x00\x00\x00"), 0, false},
		"revision 0xFF":            {[]byte("ID3\x03\xff\x00\x00\x00\x00\x00"), 0, false},
		"size not synchsafe":       {[]byte("ID3\x03\x00\x00\x00\x00\x00\x80"), 0, false},
		"not ID3":                  {[]byte("ID2\x03\x00\x00\x00\x00\x00\x00"), 0, false},
		"short":                    {[]byte("ID3\x03\x00\x00\x00\x00\x00"), 0, false},
		"a frame header":           {binary.BigEndian.AppendUint32(nil, hdrMPEG1Stereo), 0, false},
	} {
		if size, ok := id3v2TagSize(tc.b); size != tc.size || ok != tc.ok {
			t.Fatalf("%s: %d, %v; want %d, %v", name, size, ok, tc.size, tc.ok)
		}
	}
}

// mp3GaplessShortfall finds the header after the ID3v2 tags at the start of
// the file, and excuses a shortfall only when the length the probe declared
// is the one the header implies.
func TestMP3GaplessShortfall(t *testing.T) {
	first := gaplessFrame(hdrMPEG1Stereo, 32, "Info", 15, "Lavf55.37", 576, 0)
	declared := int64(1000*1152 - 576)
	v24 := id3Tag(4, 0x10, 20, frame("TIT2", textBody(encUTF8, "t")))
	footer := slices.Concat([]byte("3DI"), v24[3:10])
	for name, tc := range map[string]struct {
		b        []byte
		declared int64
		want     int64
	}{
		"no tag":                    {first, declared, 529},
		"ID3v2.3":                   {slices.Concat(id3Tag(3, 0, 100, frame("TIT2", textBody(encUTF8, "t"))), first), declared, 529},
		"two tags":                  {slices.Concat(id3Tag(3, 0, 7), id3Tag(4, 0, 300), first), declared, 529},
		"ID3v2.4 with a footer":     {slices.Concat(v24, footer, first), declared, 529},
		"padding 300":               {gaplessFrame(hdrMPEG1Stereo, 32, "Info", 15, "LAME3.99r", 576, 300), declared - 300, 229},
		"padding 529":               {gaplessFrame(hdrMPEG1Stereo, 32, "Info", 15, "LAME3.100", 576, 529), declared - 529, 0},
		"padding 1500":              {gaplessFrame(hdrMPEG1Stereo, 32, "Info", 15, "LAME3.100", 576, 1500), declared - 1500, 0},
		"another declared length":   {first, declared - 529, 0},
		"no declared length":        {first, 0, 0},
		"another encoder":           {gaplessFrame(hdrMPEG1Stereo, 32, "Info", 15, "GOGO", 576, 0), declared, 0},
		"junk before the frame":     {slices.Concat([]byte{0, 0, 0}, first), declared, 0},
		"a footer the tag lacks":    {slices.Concat(v24, first), declared, 0},
		"only a tag":                {id3Tag(3, 0, 100), declared, 0},
		"an empty file":             {nil, declared, 0},
		"a file shorter than a tag": {[]byte("ID3\x03"), declared, 0},
	} {
		t.Run(name, func(t *testing.T) {
			p := writeFile(t, filepath.Join(t.TempDir(), "f.mp3"), tc.b)
			got, err := mp3GaplessShortfall(open(t, p), tc.declared)
			if err != nil || got != tc.want {
				t.Fatalf("mp3GaplessShortfall = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// withMP3Padding returns a copy of b, an MP3 from LAME without ID3v2 tag,
// whose gapless header declares an end padding of padding samples (and, if
// enc is not empty, the encoder string enc). The field is found by LAME's
// layout, independently of parseMP3Gapless: the encoder string "LAME", then
// 9 + 12 bytes to the delay and padding field.
func withMP3Padding(t testing.TB, b []byte, padding int, enc string) []byte {
	t.Helper()
	b = slices.Clone(b)
	at := bytes.Index(b[:mp3GaplessWindow], []byte("LAME"))
	if at < 0 {
		t.Fatal("no LAME tag in the first frame")
	}
	if enc != "" {
		copy(b[at:at+4], enc)
	}
	f := b[at+9+lameFieldsBeforeDelays:]
	delay := int(f[0])<<4 | int(f[1])>>4
	v := delay<<12 | padding
	f[0], f[1], f[2] = byte(v>>16), byte(v>>8), byte(v)
	return b
}

// mp3FrameStarts returns the offset of every frame of b, an MPEG-1 Layer
// III stream without tags, the Info frame first, by the frame size of each
// header: 144 × bitrate / sample rate bytes, plus the padding byte.
func mp3FrameStarts(t testing.TB, b []byte) []int {
	t.Helper()
	kbps := [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	rates := [4]int{44100, 48000, 32000}
	var starts []int
	off := 0
	for off < len(b) {
		if len(b)-off < 4 {
			t.Fatalf("%d stray bytes after the frame at byte %d", len(b)-off, off)
		}
		h := binary.BigEndian.Uint32(b[off:])
		if h&0xFFFE0000 != 0xFFFA0000 || kbps[h>>12&0xf] == 0 || rates[h>>10&3] == 0 {
			t.Fatalf("no MPEG-1 Layer III frame at byte %d", off)
		}
		starts = append(starts, off)
		off += 144000*kbps[h>>12&0xf]/rates[h>>10&3] + int(h>>9&1)
	}
	if off != len(b) {
		t.Fatalf("the last frame ends at byte %d of %d", off, len(b))
	}
	return starts
}

// The rule against the pinned FFmpeg: the probe declares frames × spf −
// delay − padding as parseMP3Gapless reads them; a complete file decodes to
// exactly that less the shortfall, whatever the padding; a file that lost
// its last frame is refused unless that frame was all padding.
func TestAudioDigestMP3GaplessPadding(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	for _, enc := range [][]string{{"-V2"}, {"-b", "128"}} {
		src := readFile(t, lame(t, wav16(t, dir, "src.wav", sine3s), dir, "src.mp3", enc...))
		g, ok := parseMP3Gapless(src)
		if !ok || g.spf != 1152 || g.padding < mp3DecoderDelay {
			t.Fatalf("%v: LAME's header %+v, %v", enc, g, ok)
		}
		starts := mp3FrameStarts(t, src)
		if int64(len(starts)) != g.frames+1 {
			t.Fatalf("%v: %d frames after the Info frame, the header counts %d", enc, len(starts)-1, g.frames)
		}
		cases := []struct {
			name    string
			padding int
			enc     string
		}{
			{"padding 0", 0, ""},
			{"padding 0, Lavf", 0, "Lavf"},
			{"padding 0, Lavc", 0, "Lavc"},
			{"padding 300", 300, ""},
			{"padding 528", 528, ""},
			{"padding 529", 529, ""},
			// Longer than a frame, shorter than a frame and the decoder
			// delay: the lost last frame takes 1681 − 1200 = 481 declared
			// samples, fewer than mp3DecoderDelay.
			{"padding 1200", 1200, ""},
			{"LAME's padding", int(g.padding), ""},
		}
		for _, tc := range cases {
			t.Run(enc[0]+"/"+tc.name, func(t *testing.T) {
				b := withMP3Padding(t, src, tc.padding, tc.enc)
				want, ok := parseMP3Gapless(b)
				if !ok || want.padding != int64(tc.padding) || want.delay != g.delay || want.frames != g.frames {
					t.Fatalf("the rewritten header reads %+v, %v", want, ok)
				}
				whole := writeFile(t, filepath.Join(t.TempDir(), "whole.mp3"), b)
				if p := probe(t, tools, whole); p.DeclaredFrames != want.declared() {
					t.Fatalf("the probe declares %d frames, frames × spf − delay − padding is %d", p.DeclaredFrames, want.declared())
				}
				if d := digest(t, tools, whole); d.Frames != want.declared()-want.shortfall() {
					t.Fatalf("decoded %d frames, want %d − %d", d.Frames, want.declared(), want.shortfall())
				}

				cut := writeFile(t, filepath.Join(t.TempDir(), "cut.mp3"), b[:starts[len(starts)-1]])
				if want.padding >= want.spf+mp3DecoderDelay {
					// The last frame held padding only: every declared
					// sample is still there.
					if d := digest(t, tools, cut); d.Frames != want.declared() {
						t.Fatalf("without the all-padding last frame: %d frames, want %d", d.Frames, want.declared())
					}
					return
				}
				_, err := tools.AudioDigest(t.Context(), open(t, cut))
				e := wantCode(t, err, CodeDecode)
				if want.shortfall() > 0 && !strings.Contains(e.Error(), "expected") {
					t.Fatalf("the error does not state the expected length: %v", e)
				}
			})
		}

		// An encoder string the demuxer does not apply: no trimming at all,
		// and the decode is exactly the declared length.
		b := withMP3Padding(t, src, 0, "GOGO")
		if _, ok := parseMP3Gapless(b); ok {
			t.Fatal("GOGO header parsed as applied")
		}
		p := writeFile(t, filepath.Join(t.TempDir(), "gogo.mp3"), b)
		pr := probe(t, tools, p)
		if d := digest(t, tools, p); pr.DeclaredFrames <= 0 || d.Frames != pr.DeclaredFrames {
			t.Fatalf("GOGO: decoded %d frames, the probe declares %d", d.Frames, pr.DeclaredFrames)
		}
	}
}
