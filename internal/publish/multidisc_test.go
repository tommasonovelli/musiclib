package publish

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"musiclib/internal/media"
)

// A two-disc FLAC album end to end (DESIGN.md §7.2 rules 2 and 3, §7.3,
// §7.4, §5.1, §9.1): written as CD1 and CD2 directories with a cover at
// the root, a booklet, an LRC and scans inside the discs, and disc tags
// that disagree with the directories; imported by the real importer, built
// and published by the real executor. The published album has the fixed
// layout of §5.1: "Disc <D>/<NN> - <title>.flac" with the LRC next to its
// track, cover.jpg at the root, every attachment under Extras/ with its
// path (disc directories included), and the disc numbers of the
// directories written into the tags.
func TestExecuteRenderMultiDiscAlbum(t *testing.T) {
	m := newMediaEnv(t)
	box := filepath.Join(m.src, "The Box")
	for _, tr := range []struct {
		dir, file, title, no, disc string
		freq                       int
	}{
		{"CD1", "01.flac", "Alpha", "1", "1", 300},
		{"CD1", "02.flac", "Beta", "2", "1", 400},
		{"CD2", "01.flac", "Gamma", "1", "1", 500}, // the directory wins
		{"CD2", "02.flac", "Delta", "2", "7", 600},
	} {
		flac(t, filepath.Join(box, tr.dir, tr.file), tr.freq, map[string]string{"ARTIST": "The Band", "TITLE": tr.title,
			"TRACKNUMBER": tr.no, "DISCNUMBER": tr.disc})
	}
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var cover bytes.Buffer
	if err := jpeg.Encode(&cover, img, nil); err != nil {
		t.Fatal(err)
	}
	for rel, b := range map[string][]byte{
		"cover.jpg":        cover.Bytes(),
		"rip.cue":          []byte("FILE \"01.flac\" WAVE\n"),
		"CD1/01.lrc":       []byte("[00:00.00]alpha\n"),
		"CD1/Scans/a.txt":  []byte("scan"),
		"CD2/booklet.pdf":  []byte("%PDF-1.4 booklet"),
		"Artwork/back.txt": []byte("back"),
		"CD2/.DS_Store":    []byte("ignored"),
	} {
		p := filepath.Join(box, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m.importAll()
	m.runPool(2)
	id := m.albumID("The Box")
	res := m.publishedResult(id)
	if res.Dir != "The Band/The Box" {
		t.Fatalf("published at %q", res.Dir)
	}
	m.wantPublished(id, res)

	var got []string
	for p := range m.tree("library/" + res.Dir) {
		got = append(got, p)
	}
	sort.Strings(got)
	want := []string{
		".", ".musiclib.json",
		"Disc 1", "Disc 1/01 - Alpha.flac", "Disc 1/01 - Alpha.lrc", "Disc 1/02 - Beta.flac",
		"Disc 2", "Disc 2/01 - Gamma.flac", "Disc 2/02 - Delta.flac",
		"Extras", "Extras/Artwork", "Extras/Artwork/back.txt", "Extras/CD1", "Extras/CD1/01.lrc", "Extras/CD1/Scans",
		"Extras/CD1/Scans/a.txt", "Extras/CD2", "Extras/CD2/booklet.pdf", "Extras/cover.jpg", "Extras/rip.cue",
		"cover.jpg",
	}
	sort.Strings(want)
	// The LRC associated with its track is not also an attachment (§7.4).
	want = slices.DeleteFunc(want, func(s string) bool { return s == "Extras/CD1/01.lrc" })
	if !slices.Equal(got, want) {
		t.Fatalf("published tree\n got  %q\n want %q", got, want)
	}
	c, err := os.ReadFile(m.path("library/" + res.Dir + "/cover.jpg"))
	if err != nil || !bytes.Equal(c, cover.Bytes()) {
		t.Fatalf("cover.jpg is not the root's cover (%v)", err)
	}

	tools, err := sharedTools()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct{ path, disc, track, trackTotal string }{
		{"Disc 1/01 - Alpha.flac", "1", "1", "2"}, {"Disc 1/02 - Beta.flac", "1", "2", "2"},
		{"Disc 2/01 - Gamma.flac", "2", "1", "2"}, {"Disc 2/02 - Delta.flac", "2", "2", "2"},
	} {
		in, err := tools.Inspect(context.Background(), openHost(t, m.path("library/"+res.Dir+"/"+w.path)), media.FormatFLAC)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(in.Managed.Disc, []string{w.disc}) || !slices.Equal(in.Managed.DiscTotal, []string{"2"}) ||
			!slices.Equal(in.Managed.Track, []string{w.track}) || !slices.Equal(in.Managed.TrackTotal, []string{w.trackTotal}) ||
			!slices.Equal(in.Managed.Album, []string{"The Box"}) || len(in.Pictures) != 1 {
			t.Errorf("%s: managed %+v, %d pictures", w.path, in.Managed, len(in.Pictures))
		}
	}
	m.wantWorkClean()
}
