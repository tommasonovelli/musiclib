package publish

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"musiclib/internal/media"
)

// M4A end to end (DESIGN.md Phase 4): an album of AAC and ALAC tracks tagged
// by FFmpeg's MP4 muxer (one with moov after the media data, the other with
// moov first, so that the write moves the media), imported by the real
// importer, built and published by the real executor with two workers; the
// published files carry the managed tags of the catalog, the unmanaged
// items of the originals, and the same samples.

// m4aCodec is the codec and format of track i: AAC for odd tracks, ALAC for
// even ones.
func m4aCodec(i int) (codec, format string) {
	if i%2 == 1 {
		return "aac", media.FormatM4AAAC
	}
	return "alac", media.FormatM4AALAC
}

// writeM4AAlbum writes n M4A tracks under src/<dir>, tagged by FFmpeg with
// a title, the artist, the album, the track, a comment and a composer; the
// even tracks have moov before the media data.
func writeM4AAlbum(t testing.TB, src, dir, artist, title string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(src, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		codec, _ := m4aCodec(i)
		args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "sine=frequency=" + strconv.Itoa(300+100*i) + ":duration=0.4:sample_rate=44100", "-ac", "2",
			"-c:a", codec, "-metadata", "title=Track " + strconv.Itoa(i), "-metadata", "artist=" + artist,
			"-metadata", "album=" + title, "-metadata", "track=" + strconv.Itoa(i), "-metadata", "comment=an old comment",
			"-metadata", "composer=A Composer"}
		if i%2 == 0 {
			args = append(args, "-movflags", "+faststart")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, err := exec.CommandContext(ctx, media.FFmpegPath, append(args, filepath.Join(src, dir, strconv.Itoa(i)+".m4a"))...).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("ffmpeg: %v\n%s", err, out)
		}
	}
}

func TestExecuteRenderM4AAlbum(t *testing.T) {
	m := newMediaEnv(t)
	writeM4AAlbum(t, m.src, "m", "Wayne Shorter", "Speak No Evil", 2)
	if err := os.WriteFile(filepath.Join(m.src, "m", "cover.png"), noisePNG(t, 32, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	m.importAll()
	m.runPool(2)
	id := m.albumID("Speak No Evil")
	res := m.publishedResult(id)
	if res.Dir != "Wayne Shorter/Speak No Evil" {
		t.Fatalf("published at %q", res.Dir)
	}
	m.wantPublished(id, res)
	tools, err := sharedTools()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		_, format := m4aCodec(i)
		src := filepath.Join(m.src, "m", strconv.Itoa(i)+".m4a")
		out := m.path("library/" + res.Dir + "/0" + strconv.Itoa(i) + " - Track " + strconv.Itoa(i) + ".m4a")
		in, err := tools.Inspect(ctx, openHost(t, out), format)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(in.Managed.Title, []string{"Track " + strconv.Itoa(i)}) || !slices.Equal(in.Managed.Artist, []string{"Wayne Shorter"}) ||
			!slices.Equal(in.Managed.Track, []string{strconv.Itoa(i)}) || !slices.Equal(in.Managed.TrackTotal, []string{"2"}) ||
			len(in.Opaque) > 0 || len(in.Conflicts) > 0 || len(in.Pictures) != 1 || in.Pictures[0].MIME != "image/png" {
			t.Errorf("track %d: managed %+v, opaque %+v, pictures %+v", i, in.Managed, in.Opaque, in.Pictures)
		}
		orig, err := tools.Inspect(ctx, openHost(t, src), format)
		if err != nil {
			t.Fatal(err)
		}
		// Every unmanaged item and box of the original, the samples included.
		for _, kv := range orig.Unmanaged {
			if got := findKV(in.Unmanaged, kv.Key); !slices.Equal(got, kv.Values) {
				t.Errorf("track %d: %s is %q, was %q", i, kv.Key, got, kv.Values)
			}
		}
		if !slices.Equal(findKV(in.Unmanaged, "ilst:©wrt"), []string{"A Composer"}) {
			t.Errorf("track %d: the composer %q", i, findKV(in.Unmanaged, "ilst:©wrt"))
		}
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
	}
	m.wantWorkClean()
}

// The same full-disk sweep with M4A tracks whose moov is before the media
// data: the tag write grows moov and moves the media, so the kernel's ENOSPC
// also hits the helper's own write (no_space), and the job fails with
// insufficient_space as for any other write.
func TestExecuteRenderM4AOnAReallyFullDisk(t *testing.T) {
	blocks := func(need, bs int64) []int64 {
		var out []int64
		for k := int64(0); k <= 40; k += 2 {
			out = append(out, k*bs)
		}
		return append(out, need+64<<10)
	}
	messages := fullDiskSweep(t, func(src string) {
		writeM4AAlbum(t, src, "a", "Artist", "Album", 2)
		if err := os.WriteFile(filepath.Join(src, "a", "cover.png"), noisePNG(t, 128, 128), 0o644); err != nil {
			t.Fatal(err)
		}
	}, blocks)
	for _, msg := range messages {
		if strings.Contains(msg, "the tags of") {
			return
		}
	}
	t.Fatalf("ENOSPC never hit the tag writer: %q", messages)
}
