package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// A two-disc FLAC album (§7.2 rules 2 and 3, §7.3, §7.4, §7.6) on real
// ext4, real PostgreSQL and the real tools:
//   - the disc of every track is its directory's number, over a disagreeing
//     disc tag, with a warning (N-185);
//   - the tracks are numbered per disc: disc 1 by its tags, disc 2 by the
//     natural order of the basenames with a warning;
//   - the album title is the candidate's root, not a disc directory;
//   - the LRC of CD1 goes to its track in CD1 only; a same-stem LRC in CD2
//     stays an attachment;
//   - the cover is the root's cover.jpg; CD1/cover.jpg is an attachment
//     only (N-186);
//   - root files, siblings without audio and files inside the disc
//     directories are attachments with their relative paths; the ignored
//     files are ignored everywhere;
//   - the fingerprint covers every file with its path under the root.
func TestImportMultiDiscAlbum(t *testing.T) {
	e := newEnv(t)
	rootCover, discCover := jpegImage(t, 16, 16, 1), jpegImage(t, 16, 16, 2)
	files := map[string][]byte{
		"CD1/01 Alpha.flac":      track{freq: 300, tags: []string{"TITLE=Alpha", "ARTIST=The Band", "TRACKNUMBER=1", "DISCNUMBER=1"}}.flac(t),
		"CD1/02 Beta.flac":       track{freq: 350, tags: []string{"TITLE=Beta", "ARTIST=The Band", "TRACKNUMBER=2", "DISCNUMBER=1"}}.flac(t),
		"CD1/01 Alpha.lrc":       []byte("[00:00.00]alpha\n"),
		"CD1/cover.jpg":          discCover,
		"CD1/Scans/disc1.jpg":    []byte("scan of disc 1"),
		"CD02/b 10.flac":         track{freq: 400, tags: []string{"TITLE=Ten", "ARTIST=The Band", "DISCNUMBER=1"}}.flac(t),
		"CD02/b 9.flac":          track{freq: 450, tags: []string{"TITLE=Nine", "ARTIST=The Band", "DISCNUMBER=1"}}.flac(t),
		"CD02/01 Alpha.lrc":      []byte("[00:00.00]not for CD1\n"),
		"CD02/booklet.pdf":       []byte("%PDF-1.4 disc 2"),
		"cover.jpg":              rootCover,
		"Artwork/Inner/back.png": []byte("not really a png"),
		"rip.cue":                []byte("FILE \"01 Alpha.flac\" WAVE\n"),
		"rip.log":                []byte("EAC log"),
		".hidden":                []byte("kept"),
	}
	for rel, b := range files {
		e.put("Set/The Box/"+rel, b)
	}
	e.put("Set/The Box/.DS_Store", []byte("ignored"))
	e.put("Set/The Box/CD02/Thumbs.db", []byte("ignored"))
	e.put("Set/The Box/CD1/desktop.ini", []byte("ignored"))

	b := e.importDir("Set")
	if s := e.scanJob(b); s.State != "done" || len(s.Warnings) != 0 {
		t.Fatalf("scan %+v", s)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'import' AND batch_id = $1`, b.ID); n != 1 {
		t.Fatalf("%d import jobs, want one for the whole box", n)
	}
	a := e.done(b, "Set/The Box")
	if a.Title != "The Box" || a.Artist != "The Band" || a.Compilation {
		t.Errorf("album %+v", a)
	}
	want := []struct {
		disc, no      int32
		title, source string
		lyrics        bool
	}{
		{1, 1, "Alpha", "CD1/01 Alpha.flac", true},
		{1, 2, "Beta", "CD1/02 Beta.flac", false},
		{2, 1, "Nine", "CD02/b 9.flac", false},
		{2, 2, "Ten", "CD02/b 10.flac", false},
	}
	if len(a.Tracks) != len(want) {
		t.Fatalf("tracks %+v", a.Tracks)
	}
	for i, w := range want {
		tr := a.Tracks[i]
		if tr.Disc != w.disc || tr.No != w.no || tr.Title != w.title || tr.Source != w.source || (tr.Lyrics != nil) != w.lyrics || tr.Artist != nil {
			t.Errorf("track %d: %+v, want %+v", i, tr, w)
		}
	}
	if l := a.Tracks[0].Lyrics; l == nil || *l != sha(files["CD1/01 Alpha.lrc"]) {
		t.Errorf("lyrics of CD1/01 Alpha: %v", l)
	}
	if a.Cover == nil || *a.Cover != sha(rootCover) {
		t.Errorf("cover %v, want the root's cover.jpg", a.Cover)
	}
	var got []string
	for p := range a.Attachments {
		got = append(got, p)
	}
	wantAtt := []string{"CD1/cover.jpg", "CD1/Scans/disc1.jpg", "CD02/01 Alpha.lrc", "CD02/booklet.pdf", "cover.jpg",
		"Artwork/Inner/back.png", "rip.cue", "rip.log", ".hidden"}
	if !sameSet(got, wantAtt) {
		t.Errorf("attachments %v, want %v", got, wantAtt)
	}
	j := e.importJob(b, "Set/The Box")
	var codes []string
	for _, w := range j.Warnings {
		codes = append(codes, string(w.Code)+" "+w.Path)
	}
	if !sameSet(codes, []string{"disc_tag_ignored CD02", "tracks_renumbered CD02"}) {
		t.Errorf("warnings %v", codes)
	}
	var entries []fingerprintEntry
	for rel, content := range files {
		entries = append(entries, fingerprintEntry{Path: rel, Size: int64(len(content)), Hash: sha(content)})
	}
	if fp, err := fingerprint(entries); err != nil || a.Fingerprint != fp {
		t.Errorf("fingerprint %s, want %s (%v)", a.Fingerprint, fp, err)
	}
}

// A mixed-format two-disc album: FLAC on disc 1, MP3 on disc 2, tags that
// agree with the directories (no warning), numbering by the tags.
func TestImportMultiDiscMixedFormats(t *testing.T) {
	e := newEnv(t)
	e.flac("Box/Disc 1/1.flac", track{tags: []string{"ALBUM=Mixed", "ARTIST=X", "TITLE=f", "TRACKNUMBER=1", "DISCNUMBER=1/2"}})
	mp3file := mp3(t, 550, id3v23(id3Frame23("TIT2", latin1("m")), id3Frame23("TPE1", latin1("X")),
		id3Frame23("TALB", latin1("Mixed")), id3Frame23("TRCK", latin1("1")), id3Frame23("TPOS", latin1("2/2"))))
	e.put("Box/Disc 2/1.mp3", mp3file)
	b := e.importDir("")
	a := e.done(b, "Box")
	if a.Title != "Mixed" || len(a.Tracks) != 2 || a.Tracks[0].Disc != 1 || a.Tracks[1].Disc != 2 ||
		*a.Tracks[0].BlobFormat != media.FormatFLAC || *a.Tracks[1].BlobFormat != media.FormatMP3 {
		t.Fatalf("album %+v", a)
	}
	if j := e.importJob(b, "Box"); len(j.Warnings) != 0 {
		t.Errorf("warnings %+v", j.Warnings)
	}
}

// Without a cover at the candidate's root, a disc directory's cover.jpg is
// not chosen (§7.4 "nella radice", N-186): the embedded front cover is,
// and without one the album has no cover. The image stays an attachment.
func TestImportMultiDiscCoverOnlyFromTheRoot(t *testing.T) {
	e := newEnv(t)
	disc, embedded := jpegImage(t, 8, 8, 1), pngImage(t, 8, 8, 2)
	e.flac("A/CD1/1.flac", track{tags: []string{"ALBUM=A"}, pictures: []flacBlock{picture(t, media.PictureFrontCover, embedded)}})
	e.put("A/CD1/cover.jpg", disc)
	e.flac("B/CD1/1.flac", track{tags: []string{"ALBUM=B"}})
	e.put("B/CD1/cover.jpg", disc)
	e.put("B/CD1/folder.jpg", disc)
	b := e.importDir("")
	if a := e.done(b, "A"); a.Cover == nil || *a.Cover != sha(embedded) || a.Attachments["CD1/cover.jpg"] != sha(disc) {
		t.Errorf("A: cover %v, attachments %v", a.Cover, a.Attachments)
	}
	if a := e.done(b, "B"); a.Cover != nil || a.Attachments["CD1/cover.jpg"] != sha(disc) {
		t.Errorf("B: cover %v, attachments %v", a.Cover, a.Attachments)
	}
}

// The scan's layouts on disk (§7.2): a duplicate disc number, a disc over
// 99 and audio below a disc directory each fail their whole branch with one
// pre-failed import job naming the paths; CD0, a non-disc sibling with
// audio (CD1, CD2 and Bonus: three albums) and disc directories none of
// which has direct audio (a data-CD backup, N-184) follow rule 5; a nested
// multi-disc album imports next to a single-disc one; loose files are
// reported.
func TestScanMultiDiscLayouts(t *testing.T) {
	e := newEnv(t)
	one := func(rel string, freq int) { e.flac(rel, track{freq: freq, tags: []string{"ALBUM=" + rel}}) }
	one("in/Dup/CD1/1.flac", 300)
	one("in/Dup/Disc 1/1.flac", 310)
	one("in/Zeros/CD1/1.flac", 320)
	one("in/Zeros/cd01/1.flac", 330)
	one("in/Big/CD1/1.flac", 340)
	one("in/Big/CD100/1.flac", 350)
	one("in/Deep/CD1/1.flac", 360)
	one("in/Deep/CD2/Bonus/1.flac", 370)
	one("in/Direct/intro.flac", 380)
	one("in/Direct/CD1/1.flac", 390)
	one("in/Box/CD1/1.flac", 400)
	one("in/Box/CD2/1.flac", 405)
	one("in/Box/Bonus/1.flac", 410)
	one("in/Zero/CD0/1.flac", 420)
	e.flac("in/Rips/CD1/Artist - Album/1.flac", track{freq: 460, tags: []string{"ALBUM=Album"}})
	e.flac("in/Rips/CD1/Artist - Album/2.flac", track{freq: 465, tags: []string{"ALBUM=Album"}})
	one("in/Rips/CD2/Other/1.flac", 470)
	e.flac("in/Sets/Good/CD1/1.flac", track{freq: 430, tags: []string{"ALBUM=Good"}})
	e.flac("in/Sets/Good/CD2/1.flac", track{freq: 440, tags: []string{"ALBUM=Good"}})
	one("in/Sets/Single/1.flac", 450)
	e.put("in/Sets/loose.txt", []byte("loose"))
	b := e.importDir("in")

	for rel, w := range map[string]struct {
		code string
		msg  []string
	}{
		"in/Dup":    {CodeDuplicateDisc, []string{`"in/Dup/CD1"`, `"in/Dup/Disc 1"`}},
		"in/Zeros":  {CodeDuplicateDisc, []string{`"in/Zeros/CD1"`, `"in/Zeros/cd01"`}},
		"in/Big":    {catalog.CodeInvalidDisc, []string{`"in/Big/CD100"`}},
		"in/Deep":   {CodeAmbiguousCandidate, []string{`"in/Deep/CD2/Bonus/1.flac"`}},
		"in/Direct": {CodeAmbiguousCandidate, []string{`"in/Direct/CD1/1.flac"`}},
	} {
		j := e.failed(b, rel, w.code)
		for _, s := range w.msg {
			if !strings.Contains(j.Message, s) {
				t.Errorf("%s: message %q does not name %s", rel, j.Message, s)
			}
		}
	}
	for _, rel := range []string{"in/Box/CD1", "in/Box/CD2", "in/Box/Bonus", "in/Zero/CD0", "in/Sets/Single",
		"in/Rips/CD1/Artist - Album", "in/Rips/CD2/Other"} {
		if a := e.done(b, rel); a.Tracks[0].Disc != 1 {
			t.Errorf("%s: %+v", rel, a.Tracks)
		}
	}
	if a := e.done(b, "in/Sets/Good"); len(a.Tracks) != 2 || a.Tracks[1].Disc != 2 {
		t.Errorf("in/Sets/Good: %+v", a.Tracks)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'import' AND batch_id = $1`, b.ID); n != 13 {
		t.Errorf("%d import jobs, want 13 (one per branch, no partial import)", n)
	}
	s := e.scanJob(b)
	if s.State != "done" || !hasWarning(s.Warnings, jobs.WarnUnassignedFile, "in/Sets/loose.txt") || len(s.Warnings) != 1 {
		t.Errorf("scan %+v", s)
	}
}

// §7.2: an import revalidates its multi-disc candidate on the current disk,
// disc directories included, never trusting the scan: a duplicate disc
// added, audio added below a disc, all the audio gone, and audio found at
// the root by content (a FLAC named .jpg, which the scan does not probe,
// N-115) each fail it; a single-disc candidate that became multi-disc is
// no longer its old shape either.
func TestImportMultiDiscRevalidates(t *testing.T) {
	e := newEnv(t)
	for _, d := range []string{"Dup", "Deep", "Gone", "Root"} {
		e.flac(d+"/CD1/1.flac", track{tags: []string{"ALBUM=" + d}})
		e.flac(d+"/CD2/1.flac", track{freq: 500, tags: []string{"ALBUM=" + d}})
	}
	e.put("Root/front.jpg", track{freq: 600}.flac(t))
	e.flac("Became/1.flac", track{tags: []string{"ALBUM=Became"}})
	b := e.batch("")
	if err := e.execute(e.claim()); err != nil {
		t.Fatal(err)
	}
	e.flac("Dup/Disc 2/1.flac", track{freq: 700})
	e.flac("Deep/CD2/Extra/1.flac", track{freq: 800})
	for _, p := range []string{"Gone/CD1/1.flac", "Gone/CD2/1.flac"} {
		if err := os.Remove(filepath.Join(e.src, p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(filepath.Join(e.src, "Became/1.flac"), filepath.Join(e.src, "Became/x.flac")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.src, "Became/CD1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(e.src, "Became/x.flac"), filepath.Join(e.src, "Became/CD1/1.flac")); err != nil {
		t.Fatal(err)
	}
	e.runAll()
	e.failed(b, "Dup", CodeDuplicateDisc)
	e.failed(b, "Deep", CodeAmbiguousCandidate)
	e.failed(b, "Gone", CodeNotACandidate)
	if j := e.failed(b, "Root", CodeAmbiguousCandidate); !strings.Contains(j.Message, `"Root/front.jpg"`) {
		t.Errorf("message %q", j.Message)
	}
	// The current disk decides: Became is now a valid one-disc album of
	// the multi-disc shape, imported with its disc directory.
	if a := e.done(b, "Became"); a.Tracks[0].Source != "CD1/1.flac" || a.Tracks[0].Disc != 1 {
		t.Errorf("Became: %+v", a.Tracks)
	}
}

// §7.1 covers the disc directories: a file added, removed or rewritten in
// a disc directory during the import refuses it.
func TestImportMultiDiscRefusesAChangedSource(t *testing.T) {
	for name, change := range map[string]func(e *env){
		"added in a disc":  func(e *env) { e.put("Box/CD2/new.txt", []byte("new")) },
		"added below one":  func(e *env) { e.put("Box/CD1/Scans/new.txt", []byte("new")) },
		"rewritten in one": func(e *env) { e.put("Box/CD2/notes.txt", []byte("other bytes")) },
		"removed in one": func(e *env) {
			if err := os.Remove(filepath.Join(e.src, "Box/CD2/notes.txt")); err != nil {
				e.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.flac("Box/CD1/1.flac", track{tags: []string{"TITLE=x"}})
			e.flac("Box/CD2/1.flac", track{freq: 500, tags: []string{"TITLE=y"}})
			e.put("Box/CD2/notes.txt", []byte("same size."))
			e.put("Box/CD1/Scans/a.txt", []byte("a"))
			b := e.batch("")
			if err := e.execute(e.claim()); err != nil {
				t.Fatal(err)
			}
			fired := false
			e.setHook(func(p string) {
				if p == "import_rechecking" && !fired {
					fired = true
					change(e)
				}
			})
			e.allowChange = true
			if err := e.execute(e.claim()); err != nil {
				t.Fatal(err)
			}
			if !fired {
				t.Fatal("the hook did not run")
			}
			e.failed(b, "Box", CodeSourceChanged)
			if n := e.count(`SELECT count(*) FROM albums`); n != 0 {
				t.Errorf("%d albums", n)
			}
		})
	}
}

// §7.6: the fingerprint's paths are relative to the candidate's root, so
// a two-disc album and a one-disc album of the very same files do not
// collide: both import, sharing their blobs. The same two-disc album
// imported again is skipped.
func TestMultiDiscFingerprint(t *testing.T) {
	e := newEnv(t)
	x, y := track{freq: 300}.flac(t), track{freq: 400}.flac(t)
	e.put("Two/CD1/1.flac", x)
	e.put("Two/CD2/2.flac", y)
	e.put("One/1.flac", x)
	e.put("One/2.flac", y)
	b := e.importDir("")
	two, one := e.done(b, "Two"), e.done(b, "One")
	if two.Fingerprint == one.Fingerprint || two.Tracks[0].Blob != one.Tracks[0].Blob {
		t.Errorf("fingerprints %s %s, blobs %s %s", two.Fingerprint, one.Fingerprint, two.Tracks[0].Blob, one.Tracks[0].Blob)
	}
	want, err := fingerprint([]fingerprintEntry{{"CD1/1.flac", int64(len(x)), sha(x)}, {"CD2/2.flac", int64(len(y)), sha(y)}})
	if err != nil || two.Fingerprint != want {
		t.Errorf("fingerprint %s, want %s (%v)", two.Fingerprint, want, err)
	}
	b2 := e.importDir("")
	if j := e.importJob(b2, "Two"); j.State != "skipped" || j.AlbumID != two.ID {
		t.Errorf("re-import %+v", j)
	}
}

// The limits count every disc, on real files: 10,000 files over three
// directories of one multi-disc candidate plus one more fail it before the
// import; the other candidate still imports.
func TestScanMultiDiscFileLimit(t *testing.T) {
	e := newEnv(t)
	e.flac("Big/CD1/1.flac", track{})
	e.flac("Big/CD2/1.flac", track{freq: 500})
	for i := range MaxFiles - 1 { // plus two tracks: MaxFiles + 1
		dir := filepath.Join(e.src, "Big", []string{"CD1", "CD2", "Art"}[i%3])
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%05d.txt", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.flac("Small/CD1/1.flac", track{freq: 600})
	b := e.importDir("")
	if j := e.failed(b, "Big", catalog.CodeTooManyFiles); !strings.Contains(j.Message, fmt.Sprint(MaxFiles+1)) {
		t.Errorf("message %q", j.Message)
	}
	e.done(b, "Small")
}
