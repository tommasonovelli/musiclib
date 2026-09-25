package publish

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"musiclib/internal/fsops"
	"musiclib/internal/media"
)

// MP3 end to end (DESIGN.md Phase 4): an MP3 album written by the pinned
// LAME with its own ID3v2 and ID3v1 tags, an APE tag added between the
// audio and the ID3v1 tag, imported by the real importer, built and
// published by the real executor; the published files carry the managed
// tags of the catalog, every unmanaged field of the originals, and the same
// samples.

const lamePath = "/usr/local/bin/lame"

// apeReplayGain is an APEv2 tag (with header) holding one unmanaged item.
func apeReplayGain(value string) []byte {
	item := binary.LittleEndian.AppendUint32(nil, uint32(len(value)))
	item = binary.LittleEndian.AppendUint32(item, 0)
	item = append(append(item, "REPLAYGAIN_ALBUM_GAIN\x00"...), value...)
	hf := func(flags uint32) []byte {
		out := binary.LittleEndian.AppendUint32([]byte("APETAGEX"), 2000)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(item)+32))
		out = binary.LittleEndian.AppendUint32(out, 1)
		out = binary.LittleEndian.AppendUint32(out, flags)
		return append(out, make([]byte, 8)...)
	}
	return slices.Concat(hf(1<<31|1<<29), item, hf(1<<31))
}

// writeMP3Album writes n MP3 tracks under src/<dir>: LAME's ID3v2 tag
// (title, artist, album, track, a comment, a composer), then an APE tag
// before LAME's ID3v1 tag.
func writeMP3Album(t testing.TB, src, dir, artist, title string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(src, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		wav := filepath.Join(t.TempDir(), "a.wav")
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if out, err := exec.CommandContext(ctx, media.FFmpegPath, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "sine=frequency="+strconv.Itoa(300+100*i)+":duration=0.4:sample_rate=44100", "-ac", "2",
			"-c:a", "pcm_s16le", wav).CombinedOutput(); err != nil {
			cancel()
			t.Fatalf("ffmpeg: %v\n%s", err, out)
		}
		mp3 := filepath.Join(t.TempDir(), "a.mp3")
		if out, err := exec.CommandContext(ctx, lamePath, "--quiet", "-V2", "--add-id3v2", "--tt", "Track "+strconv.Itoa(i),
			"--ta", artist, "--tl", title, "--tn", strconv.Itoa(i), "--tc", "an old comment", "--tv", "TCOM=A Composer",
			wav, mp3).CombinedOutput(); err != nil {
			cancel()
			t.Fatalf("lame: %v\n%s", err, out)
		}
		cancel()
		b, err := os.ReadFile(mp3)
		if err != nil {
			t.Fatal(err)
		}
		if string(b[len(b)-128:len(b)-125]) != "TAG" {
			t.Fatal("LAME wrote no ID3v1 tag")
		}
		b = slices.Concat(b[:len(b)-128], apeReplayGain("-7.00 dB"), b[len(b)-128:])
		if err := os.WriteFile(filepath.Join(src, dir, strconv.Itoa(i)+".mp3"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// openHost opens a file for the tools through fsops.
func openHost(t testing.TB, path string) *os.File {
	t.Helper()
	r, err := fsops.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	f, err := r.Open(filepath.Base(path))
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func findKV(kvs []media.KeyValues, key string) []string {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Values
		}
	}
	return nil
}

func TestExecuteRenderMP3Album(t *testing.T) {
	m := newMediaEnv(t)
	writeMP3Album(t, m.src, "m", "Lee Morgan", "The Sidewinder", 2)
	m.importAll()
	m.runPool(2)
	id := m.albumID("The Sidewinder")
	res := m.publishedResult(id)
	if res.Dir != "Lee Morgan/The Sidewinder" {
		t.Fatalf("published at %q", res.Dir)
	}
	m.wantPublished(id, res)
	tools, err := sharedTools()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		src := filepath.Join(m.src, "m", strconv.Itoa(i)+".mp3")
		out := m.path("library/" + res.Dir + "/0" + strconv.Itoa(i) + " - Track " + strconv.Itoa(i) + ".mp3")
		in, err := tools.Inspect(ctx, openHost(t, out), media.FormatMP3)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(in.Managed.Title, []string{"Track " + strconv.Itoa(i)}) || !slices.Equal(in.Managed.Artist, []string{"Lee Morgan"}) ||
			!slices.Equal(in.Managed.Track, []string{strconv.Itoa(i)}) || !slices.Equal(in.Managed.TrackTotal, []string{"2"}) ||
			len(in.Opaque) > 0 || len(in.Conflicts) > 0 {
			t.Errorf("track %d: managed %+v, opaque %+v", i, in.Managed, in.Opaque)
		}
		// The unmanaged fields of the original, the ID3v1 comment being
		// already in LAME's COMM frame, and no ID3v1 tag.
		for key, want := range map[string][]string{
			"id3v2:TCOM":                {"A Composer"},
			"ape:REPLAYGAIN_ALBUM_GAIN": {"-7.00 dB"},
		} {
			if got := findKV(in.Unmanaged, key); !slices.Equal(got, want) {
				t.Errorf("track %d: %s is %q", i, key, got)
			}
		}
		if findKV(in.Unmanaged, "id3v1:comment") != nil || findKV(in.Unmanaged, "id3v2:COMM:XXX:legacy-id3v1") != nil {
			t.Errorf("track %d: the ID3v1 comment: %+v", i, in.Unmanaged)
		}
		// The same samples as the file in /import.
		d0, err := tools.AudioDigest(ctx, openHost(t, src))
		if err != nil {
			t.Fatal(err)
		}
		d1, err := tools.AudioDigest(ctx, openHost(t, out))
		if err != nil {
			t.Fatal(err)
		}
		if d0 != d1 {
			t.Errorf("track %d: the published samples differ: %+v, %+v", i, d0, d1)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(b, []byte{'I', 'D', '3', 4}) || string(b[len(b)-128:len(b)-125]) == "TAG" {
			t.Errorf("track %d is not ID3v2.4 without ID3v1", i)
		}
	}
	m.wantWorkClean()
}
