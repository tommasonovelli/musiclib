package importer

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"musiclib/internal/jobs"
)

// fullTags are the managed fields of one track of a normal album.
func fullTags(no int, title string) []string {
	return []string{
		"TITLE=" + title, "ARTIST=Miles Davis", "ALBUMARTIST=Miles Davis", "ALBUM=Kind of Blue",
		"TRACKNUMBER=" + string(rune('0'+no)), "DISCNUMBER=1", "DATE=1959-08-17", "GENRE=Jazz",
	}
}

// A normal album (§7.1–§7.6): tags read from the verified copies, the cover
// chosen from cover.jpg and kept as an attachment too, the LRC associated,
// nested and Unicode attachments, a hidden file kept, the ignored files
// ignored, the fingerprint of every file, the render enqueued.
func TestImportNormalAlbum(t *testing.T) {
	e := newEnv(t)
	cover := jpegImage(t, 32, 32, 1)
	e.flac("Kind of Blue/01 So What.flac", track{freq: 440, tags: fullTags(1, "So What")})
	e.flac("Kind of Blue/02 Freddie Freeloader.flac", track{freq: 550, tags: append(fullTags(2, "Freddie Freeloader"),
		"COMMENT=unmanaged, kept")})
	e.put("Kind of Blue/01 So What.lrc", []byte("[00:00.00]So what\n"))
	e.put("Kind of Blue/cover.jpg", cover)
	e.put("Kind of Blue/Scans/Booklet p1.pdf", []byte("%PDF-1.4 not really"))
	e.put("Kind of Blue/Scans/Ünïcødé/日本語.txt", []byte("notes"))
	e.put("Kind of Blue/.hidden notes", []byte("kept"))
	e.put("Kind of Blue/.DS_Store", []byte("ignored"))
	e.put("Kind of Blue/Scans/THUMBS.DB", []byte("ignored"))
	e.put("Kind of Blue/Desktop.ini", []byte("ignored"))

	b := e.importDir("")
	scan := e.scanJob(b)
	if scan.State != "done" || len(scan.Warnings) != 0 {
		t.Fatalf("scan %+v", scan)
	}
	a := e.done(b, "Kind of Blue")
	if a.Artist != "Miles Davis" || a.Title != "Kind of Blue" || a.Year == nil || *a.Year != 1959 ||
		a.Genre == nil || *a.Genre != "Jazz" || a.Compilation {
		t.Errorf("album %+v", a)
	}
	if len(a.Tracks) != 2 {
		t.Fatalf("tracks %+v", a.Tracks)
	}
	for i, want := range []struct{ title, source string }{{"So What", "01 So What.flac"}, {"Freddie Freeloader", "02 Freddie Freeloader.flac"}} {
		tr := a.Tracks[i]
		if tr.Disc != 1 || int(tr.No) != i+1 || tr.Title != want.title || tr.Artist != nil || tr.Genre != nil ||
			tr.Source != want.source || tr.BlobFormat == nil || *tr.BlobFormat != "flac" {
			t.Errorf("track %d: %+v", i, tr)
		}
	}
	if a.Tracks[0].Lyrics == nil || a.Tracks[1].Lyrics != nil {
		t.Errorf("lyrics %v %v", a.Tracks[0].Lyrics, a.Tracks[1].Lyrics)
	}
	if a.Cover == nil || *a.Cover != sha(cover) || e.blobFormat(sha(cover)) != "jpeg" {
		t.Errorf("cover %v", a.Cover)
	}
	wantAtt := []string{".hidden notes", "Scans/Booklet p1.pdf", "Scans/Ünïcødé/日本語.txt", "cover.jpg"}
	var got []string
	for p := range a.Attachments {
		got = append(got, p)
	}
	if !sameSet(got, wantAtt) {
		t.Errorf("attachments %v, want %v", got, wantAtt)
	}
	if a.Attachments["cover.jpg"] != sha(cover) {
		t.Errorf("cover.jpg attachment is not the cover blob")
	}
	if e.count(`SELECT count(*) FROM jobs WHERE kind = 'render' AND album_id = $1`, a.ID) != 1 {
		t.Error("no render enqueued")
	}
	if e.count(`SELECT count(*) FROM path_claims WHERE album_id = $1`, a.ID) != 1 {
		t.Error("no path claim")
	}
	// The fingerprint covers every non-ignored file (§7.6).
	var entries []fingerprintEntry
	for rel, content := range map[string][]byte{
		"01 So What.flac":            track{freq: 440, tags: fullTags(1, "So What")}.flac(t),
		"02 Freddie Freeloader.flac": track{freq: 550, tags: append(fullTags(2, "Freddie Freeloader"), "COMMENT=unmanaged, kept")}.flac(t),
		"01 So What.lrc":             []byte("[00:00.00]So what\n"),
		"cover.jpg":                  cover,
		"Scans/Booklet p1.pdf":       []byte("%PDF-1.4 not really"),
		"Scans/Ünïcødé/日本語.txt":      []byte("notes"),
		".hidden notes":              []byte("kept"),
	} {
		entries = append(entries, fingerprintEntry{Path: rel, Size: int64(len(content)), Hash: sha(content)})
	}
	fp, err := fingerprint(entries)
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint != fp {
		t.Errorf("fingerprint %s, want %s", a.Fingerprint, fp)
	}
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return reflect.DeepEqual(x, y)
}

// Untagged files (§7.3): titles from the basenames, the album from the
// directory, Unknown Artist, numbers by the natural order of the basenames
// with a warning.
func TestImportUntagged(t *testing.T) {
	e := newEnv(t)
	for i, name := range []string{"Track 10.flac", "Track 9.flac", "Track 1.flac", "track 02.flac"} {
		e.flac("Rips/Untitled/"+name, track{freq: 300 + 50*i})
	}
	b := e.importDir("Rips")
	a := e.done(b, "Rips/Untitled")
	if a.Artist != UnknownArtist || a.Title != "Untitled" || a.Year != nil || a.Genre != nil || a.Compilation || a.Cover != nil {
		t.Errorf("album %+v", a)
	}
	var titles []string
	for _, tr := range a.Tracks {
		titles = append(titles, tr.Title)
	}
	// Digit runs compare as integers; the rest by bytes, so "T" < "t"
	// (NOTES.md N-119).
	if want := []string{"Track 1", "Track 9", "Track 10", "track 02"}; !reflect.DeepEqual(titles, want) {
		t.Errorf("titles in order %v, want %v", titles, want)
	}
	j := e.importJob(b, "Rips/Untitled")
	if !hasWarning(j.Warnings, jobs.WarnTracksRenumbered, "") {
		t.Errorf("warnings %+v", j.Warnings)
	}
}

// An ambiguous branch fails as a whole while the other candidates import;
// unassigned files and a symlink outside every candidate are reported by
// the scan (§7.2 rules 4 and 5).
func TestScanAmbiguousBranchAndUnassigned(t *testing.T) {
	e := newEnv(t)
	e.flac("in/Good/01.flac", track{tags: []string{"ALBUM=Good"}})
	e.flac("in/Bad/01.flac", track{tags: []string{"ALBUM=Bad"}})
	e.flac("in/Bad/Bonus/01.flac", track{tags: []string{"ALBUM=Bad"}})
	e.put("in/loose.txt", []byte("nobody's"))
	e.put("in/Empty/readme.nfo", []byte("no audio here"))
	symlink(t, "../Good", e.src+"/in/link")

	b := e.importDir("in")
	e.done(b, "in/Good")
	j := e.failed(b, "in/Bad", CodeAmbiguousCandidate)
	if !strings.Contains(j.Message, "in/Bad/Bonus/01.flac") {
		t.Errorf("message %q", j.Message)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'import' AND batch_id = $1`, b.ID); n != 2 {
		t.Errorf("%d import jobs, want 2 (no partial import of the ambiguous branch)", n)
	}
	scan := e.scanJob(b)
	if scan.State != "done" {
		t.Fatalf("scan %+v", scan)
	}
	for _, p := range []string{"in/loose.txt", "in/Empty/readme.nfo"} {
		if !hasWarning(scan.Warnings, jobs.WarnUnassignedFile, p) {
			t.Errorf("no unassigned warning for %s: %+v", p, scan.Warnings)
		}
	}
	if !hasWarning(scan.Warnings, jobs.WarnRejectedEntry, "in/link") {
		t.Errorf("no rejected warning for the symlink: %+v", scan.Warnings)
	}
}
