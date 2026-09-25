package media

import (
	"encoding/binary"
	"path/filepath"
	"slices"
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
