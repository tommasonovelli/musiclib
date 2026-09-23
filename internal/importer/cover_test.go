package importer

import (
	"bytes"
	"testing"

	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// The external covers come first, in the order cover.*, folder.*, front.*,
// ties by normalized name then bytes; only at the root (§7.4).
func TestCoverExternalOrder(t *testing.T) {
	e := newEnv(t)
	c1, c2, f, fr, nested := jpegImage(t, 8, 8, 1), pngImage(t, 8, 8, 2), jpegImage(t, 8, 8, 3), jpegImage(t, 8, 8, 4), jpegImage(t, 8, 8, 5)
	e.flac("A/1.flac", track{pictures: []flacBlock{picture(t, media.PictureFrontCover, pngImage(t, 4, 4, 9))}})
	e.put("A/front.jpg", fr)
	e.put("A/folder.jpg", f)
	// By key "cover.jpg" < "cover.png"; by bytes "Cover.png" < "cover.jpg":
	// the normalized name decides.
	e.put("A/Cover.png", c2)
	e.put("A/cover.jpg", c1)
	e.put("A/Scans/cover.jpg", nested)
	e.flac("B/1.flac", track{})
	e.put("B/front.jpg", fr)
	e.put("B/folder.png", c2)
	e.flac("C/1.flac", track{})
	e.put("C/front.jpg", fr)
	b := e.importDir("")
	for dir, want := range map[string][]byte{"A": c1, "B": c2, "C": fr} {
		a := e.done(b, dir)
		if a.Cover == nil || *a.Cover != sha(want) {
			t.Errorf("%s: cover %v, want %s", dir, a.Cover, sha(want)[:8])
		}
	}
}

// The embedded covers (§7.4): the most frequent front cover, a tie going to
// the smallest hash; without a front cover, the first valid picture in
// track order whatever its type (ffmpeg's type 0, N-087).
func TestCoverEmbedded(t *testing.T) {
	e := newEnv(t)
	x, y, z := pngImage(t, 6, 6, 1), pngImage(t, 6, 6, 2), jpegImage(t, 6, 6, 3)
	front := func(img []byte) flacBlock { return picture(t, media.PictureFrontCover, img) }
	other := func(img []byte) flacBlock { return picture(t, 0, img) }
	// Freq: x twice, y once -> x.
	e.flac("Freq/1.flac", track{tags: []string{"TRACKNUMBER=1"}, pictures: []flacBlock{other(z), front(y)}})
	e.flac("Freq/2.flac", track{freq: 500, tags: []string{"TRACKNUMBER=2"}, pictures: []flacBlock{front(x)}})
	e.flac("Freq/3.flac", track{freq: 600, tags: []string{"TRACKNUMBER=3"}, pictures: []flacBlock{front(x)}})
	// Tie: x and y once each -> the smaller hash.
	e.flac("Tie/1.flac", track{tags: []string{"TRACKNUMBER=1"}, pictures: []flacBlock{front(x)}})
	e.flac("Tie/2.flac", track{freq: 500, tags: []string{"TRACKNUMBER=2"}, pictures: []flacBlock{front(y)}})
	// Type 0 only, in track order: track 1 is "b.flac".
	e.flac("Zero/a.flac", track{tags: []string{"TRACKNUMBER=2"}, pictures: []flacBlock{other(y)}})
	e.flac("Zero/b.flac", track{freq: 500, tags: []string{"TRACKNUMBER=1"}, pictures: []flacBlock{other(z), other(x)}})
	// An invalid front cover is skipped with a warning.
	e.flac("Bad/1.flac", track{pictures: []flacBlock{front([]byte("\xff\xd8\xff garbage")), other(x)}})
	b := e.importDir("")

	want := map[string]string{"Freq": sha(x), "Zero": sha(z), "Bad": sha(x)}
	if sha(x) < sha(y) {
		want["Tie"] = sha(x)
	} else {
		want["Tie"] = sha(y)
	}
	for dir, h := range want {
		a := e.done(b, dir)
		if a.Cover == nil || *a.Cover != h {
			t.Errorf("%s: cover %v, want %s", dir, a.Cover, h[:8])
			continue
		}
		if f := e.blobFormat(h); f != "png" && f != "jpeg" {
			t.Errorf("%s: cover blob format %q", dir, f)
		}
		if _, ok := a.Attachments["cover.png"]; ok {
			t.Errorf("%s: an embedded cover became an attachment", dir)
		}
	}
	if !hasWarning(e.importJob(b, "Bad").Warnings, jobs.WarnCoverSkipped, "1.flac") {
		t.Error("no warning for the invalid embedded cover")
	}
	// A real ffmpeg-muxed attached picture: type 0 (N-087), still chosen.
	dir := t.TempDir()
	writeFile(t, dir+"/in.flac", track{}.flac(t))
	writeFile(t, dir+"/c.jpg", z)
	ffmpeg(t, "-i", dir+"/in.flac", "-i", dir+"/c.jpg", "-map", "0", "-map", "1", "-c", "copy", "-disposition:v", "attached_pic", dir+"/out.flac")
	e.put("Muxed/1.flac", readFile(t, dir+"/out.flac"))
	b = e.importDir("Muxed")
	if a := e.done(b, "Muxed"); a.Cover == nil || *a.Cover != sha(z) {
		t.Errorf("ffmpeg-muxed cover %v, want %s", a.Cover, sha(z)[:8])
	}
}

// Invalid, oversized (bytes or pixels) and non-embeddable (N-091) covers
// are skipped with a warning; the next candidate is chosen, and a skipped
// external image stays an attachment (§7.4, §8.5).
func TestCoverSkipped(t *testing.T) {
	e := newEnv(t)
	good := jpegImage(t, 8, 8, 7)
	// 2400×2340 uncompressed RGB: about 16.85 MB, between the FLAC block
	// limit and 20 MiB.
	big := rawPNG(t, 2400, 2340)
	if limit, _ := media.MaxEmbeddedCover(media.FormatFLAC, media.FormatPNG); int64(len(big)) <= limit || len(big) > MaxCoverBytes {
		t.Fatalf("the N-091 fixture has %d bytes", len(big))
	}
	huge := append(bytes.Clone(good), make([]byte, MaxCoverBytes)...) // valid header, > 20 MiB
	e.flac("A/1.flac", track{})
	e.put("A/cover.jpg", []byte("\xff\xd8\xff not a jpeg"))
	e.put("A/cover.png", big)
	e.put("A/folder.jpg", huge)
	e.put("A/folder.png", pngHeader(8000, 5001)) // 40,008,000 pixels
	e.put("A/front.jpg", good)
	b := e.importDir("")
	a := e.done(b, "A")
	if a.Cover == nil || *a.Cover != sha(good) {
		t.Fatalf("cover %v, want front.jpg", a.Cover)
	}
	for _, p := range []string{"cover.jpg", "cover.png", "folder.jpg", "folder.png", "front.jpg"} {
		if a.Attachments[p] == "" {
			t.Errorf("%s is not an attachment", p)
		}
	}
	ws := e.importJob(b, "A").Warnings
	for p, code := range map[string]jobs.WarningCode{
		"cover.jpg": jobs.WarnCoverSkipped, "cover.png": jobs.WarnCoverNotEmbeddable,
		"folder.jpg": jobs.WarnCoverSkipped, "folder.png": jobs.WarnCoverSkipped,
	} {
		if !hasWarning(ws, code, p) {
			t.Errorf("no %s warning for %s: %+v", code, p, ws)
		}
	}
	// Nothing valid: no cover (§7.4).
	e.flac("N/1.flac", track{})
	e.put("N/cover.jpg", []byte("garbage"))
	b = e.importDir("N")
	if a := e.done(b, "N"); a.Cover != nil {
		t.Errorf("cover %v, want none", *a.Cover)
	}
}
