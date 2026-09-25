package importer

import (
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
)

// Discordant album tags are mixed_album (§7.2); an explicit title resolves
// it on retry (§7.3).
func TestImportMixedAlbumAndTitleOverride(t *testing.T) {
	e := newEnv(t)
	e.flac("A/1.flac", track{tags: []string{"ALBUM=One", "TITLE=a"}})
	e.flac("A/2.flac", track{freq: 500, tags: []string{"ALBUM=Two", "TITLE=b"}})
	b := e.importDir("")
	j := e.failed(b, "A", CodeMixedAlbum)
	if !strings.Contains(j.Message, `"One", "Two"`) {
		t.Errorf("message %q", j.Message)
	}
	e.retry(j.ID, jobs.Overrides{Title: ptr("Singles")})
	e.runAll()
	a := e.done(b, "A")
	if a.Title != "Singles" || a.Artist != UnknownArtist {
		t.Errorf("album %+v", a)
	}
}

// Discordant album artists are refused as ambiguous (§7.3); an explicit
// artist resolves them, and the track artists equal to it inherit.
func TestImportAmbiguousAlbumArtistAndArtistOverride(t *testing.T) {
	e := newEnv(t)
	e.flac("A/1.flac", track{tags: []string{"ALBUMARTIST=X", "ARTIST=X", "ALBUM=A"}})
	e.flac("A/2.flac", track{freq: 500, tags: []string{"ALBUMARTIST=Y", "ARTIST=Y", "ALBUM=A"}})
	b := e.importDir("")
	j := e.failed(b, "A", CodeAmbiguousAlbumArtist)
	e.retry(j.ID, jobs.Overrides{Artist: ptr("X")})
	e.runAll()
	a := e.done(b, "A")
	if a.Artist != "X" || a.Compilation {
		t.Errorf("album %+v", a)
	}
	if a.Tracks[0].Artist != nil || a.Tracks[1].Artist == nil || *a.Tracks[1].Artist != "Y" {
		t.Errorf("track artists %v %v", a.Tracks[0].Artist, a.Tracks[1].Artist)
	}
}

// Several track artists and no album artist: Various Artists, chosen
// automatically, makes a compilation; a compilation tag does too (§7.3).
func TestImportVariousArtistsAndCompilation(t *testing.T) {
	e := newEnv(t)
	e.flac("VA/1.flac", track{tags: []string{"ARTIST=A", "ALBUM=Hits"}})
	e.flac("VA/2.flac", track{freq: 500, tags: []string{"ARTIST=B", "ALBUM=Hits"}})
	e.flac("Comp/1.flac", track{tags: []string{"ARTIST=A", "ALBUM=Comp", "COMPILATION=1"}})
	b := e.importDir("")
	va := e.done(b, "VA")
	if va.Artist != VariousArtists || !va.Compilation || *va.Tracks[0].Artist != "A" || *va.Tracks[1].Artist != "B" {
		t.Errorf("album %+v", va)
	}
	comp := e.done(b, "Comp")
	if comp.Artist != "A" || !comp.Compilation || comp.Tracks[0].Artist != nil {
		t.Errorf("album %+v", comp)
	}
}

// Multi-valued tags are one string joined with "; " in their order; year
// and genre ties, and genre differences kept per track (§7.3, N-004).
func TestImportMultiValuedYearGenre(t *testing.T) {
	e := newEnv(t)
	e.flac("M/1.flac", track{tags: []string{"TRACKNUMBER=1", "ARTIST=Zed", "ARTIST=Amy", "ALBUM=M", "DATE=2001", "GENRE=Rock"}})
	e.flac("M/2.flac", track{freq: 500, tags: []string{"TRACKNUMBER=2", "ARTIST=Zed", "ARTIST=Amy", "ALBUM=M", "DATE=2000", "GENRE=Pop"}})
	e.flac("M/3.flac", track{freq: 600, tags: []string{"TRACKNUMBER=3", "ARTIST=Zed", "ARTIST=Amy", "ALBUM=M"}})
	b := e.importDir("")
	a := e.done(b, "M")
	if a.Artist != "Zed; Amy" || a.Compilation {
		t.Errorf("artist %q compilation %v", a.Artist, a.Compilation)
	}
	if a.Year == nil || *a.Year != 2000 {
		t.Errorf("year %v, want the smallest of a tie", a.Year)
	}
	if a.Genre == nil || *a.Genre != "Pop" {
		t.Errorf("genre %v, want the byte-smallest of a tie (N-004)", a.Genre)
	}
	if a.Tracks[0].Genre == nil || *a.Tracks[0].Genre != "Rock" || a.Tracks[1].Genre != nil ||
		a.Tracks[2].Genre == nil || *a.Tracks[2].Genre != "" {
		t.Errorf("track genres %v %v %v", a.Tracks[0].Genre, a.Tracks[1].Genre, a.Tracks[2].Genre)
	}
	if !hasWarning(e.importJob(b, "M").Warnings, jobs.WarnYearDiscordant, "") {
		t.Error("no year warning")
	}
}

// Numbers beyond the schema are errors, never truncated (§7.3).
func TestImportOutOfRangeNumbers(t *testing.T) {
	e := newEnv(t)
	e.flac("T/1.flac", track{tags: []string{"TRACKNUMBER=1000"}})
	e.flac("D/1.flac", track{tags: []string{"DISCNUMBER=100"}})
	e.flac("H/1.flac", track{tags: []string{"TRACKNUMBER=99999999999999999999"}})
	b := e.importDir("")
	e.failed(b, "T", catalog.CodeInvalidTrackNumber)
	e.failed(b, "D", catalog.CodeInvalidDisc)
	e.failed(b, "H", catalog.CodeInvalidTrackNumber)
}

// LRC files (§7.4): matched by NFC and casefold stem in the same directory,
// ambiguous as an error, unmatched or not UTF-8 as attachments.
func TestImportLyrics(t *testing.T) {
	e := newEnv(t)
	e.flac("L/Song.flac", track{})
	e.put("L/SONG.lrc", []byte("[00:00]ok"))
	e.put("L/Other.lrc", []byte("[00:00]no track"))
	e.put("L/Sub/Song.lrc", []byte("[00:00]other directory"))
	e.flac("N/Song.flac", track{})
	e.put("N/Song.lrc", []byte("[00:00]\xff\xfe latin-1"))
	e.flac("Amb/Song.flac", track{})
	e.flac("Amb/song.FLAC", track{freq: 500})
	e.put("Amb/song.lrc", []byte("[00:00]which one?"))
	b := e.importDir("")

	l := e.done(b, "L")
	if l.Tracks[0].Lyrics == nil || *l.Tracks[0].Lyrics != sha([]byte("[00:00]ok")) {
		t.Errorf("lyrics %v", l.Tracks[0].Lyrics)
	}
	if _, ok := l.Attachments["SONG.lrc"]; ok || len(l.Attachments) != 2 {
		t.Errorf("attachments %v", l.Attachments)
	}
	n := e.done(b, "N")
	if n.Tracks[0].Lyrics != nil || n.Attachments["Song.lrc"] == "" {
		t.Errorf("an LRC that is not UTF-8: lyrics %v attachments %v", n.Tracks[0].Lyrics, n.Attachments)
	}
	if !hasWarning(e.importJob(b, "N").Warnings, jobs.WarnLyricsNotUTF8, "Song.lrc") {
		t.Error("no lyrics warning")
	}
	e.failed(b, "Amb", catalog.CodeLyricsAssociation)
}

// N-092: an unmanaged field that cannot be written back refuses the file,
// naming file and field. N-090: ID3 tags in a FLAC do not: a render strips
// them, a trailing ID3v1 included: the full decode leaves its bytes out
// (N-128). An ID3v2 appended at the end is not stripped by a render, and
// the decode still refuses it, with a message that says what failed.
func TestImportUnrenderableTagsAndID3(t *testing.T) {
	e := newEnv(t)
	e.flac("Latin1/1.flac", track{tags: []string{"ALBUM=L", "COMMENT=caf\xe9"}})
	e.flac("ID3/1.flac", track{tags: []string{"ALBUM=I", "TITLE=Vorbis title"}, id3v2: true})
	e.flac("ID3v1/1.flac", track{tags: []string{"ALBUM=J", "TITLE=Vorbis title"}, id3v1: true})
	e.put("Appended/1.flac", append(track{tags: []string{"ALBUM=K"}}.flac(t), id3v2Tag("appended")...))
	b := e.importDir("")
	j := e.failed(b, "Latin1", CodeUnrenderableTag)
	if !strings.Contains(j.Message, `"1.flac"`) || !strings.Contains(j.Message, "vorbis:COMMENT") || !strings.Contains(j.Message, "invalid_utf8") {
		t.Errorf("message %q", j.Message)
	}
	a := e.done(b, "ID3")
	if a.Tracks[0].Title != "Vorbis title" {
		t.Errorf("title %q", a.Tracks[0].Title)
	}
	if !hasWarning(e.importJob(b, "ID3").Warnings, jobs.WarnFLACID3, "1.flac") {
		t.Error("no ID3 warning")
	}
	a = e.done(b, "ID3v1")
	if a.Tracks[0].Title != "Vorbis title" {
		t.Errorf("title %q", a.Tracks[0].Title)
	}
	if !hasWarning(e.importJob(b, "ID3v1").Warnings, jobs.WarnFLACID3, "1.flac") {
		t.Error("no ID3v1 warning")
	}
	j = e.failed(b, "Appended", CodeCorruptAudio)
	if !strings.Contains(j.Message, `"1.flac" does not decode completely`) || !strings.Contains(j.Message, "media_decode") {
		t.Errorf("message %q", j.Message)
	}
}

// Content decides (§7.2): a corrupt .flac is an album error, M4A imports
// (Phase 4) but a fragmented one is not supported (N-165), WAV is not
// supported, a FLAC without the extension is a track, and a non-audio file
// with an audio extension is corrupt audio.
func TestImportFormats(t *testing.T) {
	e := newEnv(t)
	good := track{tags: []string{"TITLE=x"}}.flac(t)
	e.put("Corrupt/1.flac", good[:len(good)/2])
	e.put("Text/1.mp3", []byte("not audio at all"))
	dir := t.TempDir()
	ffmpeg(t, "-f", "lavfi", "-i", "sine=duration=0.2", "-c:a", "aac", dir+"/a.m4a")
	ffmpeg(t, "-f", "lavfi", "-i", "sine=duration=0.2", "-c:a", "pcm_s16le", dir+"/a.wav")
	ffmpeg(t, "-f", "lavfi", "-i", "sine=duration=0.2", "-c:a", "aac", "-movflags", "frag_keyframe+empty_moov", dir+"/frag.m4a")
	e.put("M4A/1.m4a", readFile(t, dir+"/a.m4a"))
	e.put("Fragmented/1.m4a", readFile(t, dir+"/frag.m4a"))
	e.put("WAV/1.wav", readFile(t, dir+"/a.wav"))
	e.put("NoExt/track01", good)
	b := e.importDir("")
	if j := e.failed(b, "Corrupt", CodeCorruptAudio); !strings.Contains(j.Message, "does not decode completely") {
		t.Errorf("message %q", j.Message)
	}
	e.failed(b, "Text", CodeCorruptAudio)
	if a := e.done(b, "M4A"); len(a.Tracks) != 1 || a.Tracks[0].Title != "1" {
		t.Errorf("M4A tracks %+v", a.Tracks)
	}
	if j := e.failed(b, "Fragmented", CodeUnsupportedAudio); !strings.Contains(j.Message, "fragmented MP4") {
		t.Errorf("message %q", j.Message)
	}
	e.failed(b, "WAV", CodeUnsupportedAudio)
	a := e.done(b, "NoExt")
	if len(a.Tracks) != 1 || a.Tracks[0].Title != "x" {
		t.Errorf("tracks %+v", a.Tracks)
	}
}

// A CD<N> / Disc <N> layout (§7.2 rules 2 and 3) fails as not supported
// yet (Phase 5), never grouped otherwise.
func TestScanMultiDiscNotSupported(t *testing.T) {
	e := newEnv(t)
	e.flac("Box/CD1/1.flac", track{})
	e.flac("Box/cd02/1.flac", track{freq: 500})
	e.put("Box/Artwork/front.jpg", jpegImage(t, 8, 8, 1))
	e.flac("Set/Disc 1/1.flac", track{})
	b := e.importDir("")
	e.failed(b, "Box", CodeMultiDiscNotSupported)
	e.failed(b, "Set", CodeMultiDiscNotSupported)
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'import' AND batch_id = $1`, b.ID); n != 2 {
		t.Errorf("%d import jobs, want 2", n)
	}
	if s := e.scanJob(b); s.State != "failed" || s.Code != CodeNoValidCandidate {
		t.Errorf("scan %+v", s)
	}
}

// Symlinks, FIFOs, sockets and devices inside a candidate are rejected
// without being followed or opened (§5.2, N-030).
func TestScanRejectsSpecialFiles(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"Link", "Fifo", "Sock", "Dev"} {
		e.flac(name+"/1.flac", track{})
	}
	symlink(t, "1.flac", e.src+"/Link/2.flac")
	if err := unix.Mkfifo(e.src+"/Fifo/pipe", 0o644); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: e.src + "/Sock/sock"}); err != nil {
		t.Fatal(err)
	}
	device := unix.Mknod(e.src+"/Dev/null", syscall.S_IFCHR|0o644, int(unix.Mkdev(1, 3))) == nil
	b := e.importDir("")
	for _, name := range []string{"Link", "Fifo", "Sock"} {
		e.failed(b, name, CodeSourceRejected)
	}
	if device {
		e.failed(b, "Dev", CodeSourceRejected)
	} else {
		t.Log("no CAP_MKNOD: the device case is not covered here (N-038)")
	}
}

// A failure of the batch root is the scan's: missing, not a directory, a
// symlink; a directory without audio completes the batch with an
// explanation (§7.2).
func TestScanRootFailures(t *testing.T) {
	e := newEnv(t)
	e.put("file", []byte("x"))
	e.put("NoAudio/readme.txt", []byte("x"))
	symlink(t, "NoAudio", e.src+"/link")
	for root, code := range map[string]string{
		"missing": CodeSourceNotFound, "file": CodeSourceNotDirectory, "link": CodeSourceRejected, "NoAudio": CodeNoValidCandidate,
	} {
		b := e.importDir(root)
		if s := e.scanJob(b); s.State != "failed" || s.Code != code || s.Message == "" {
			t.Errorf("root %q: scan %+v, want failed %s", root, s, code)
		}
	}
}

// Owner decision (2026-09-23, N-131): two directories of a candidate that
// differ only in case are a collision after normalization (§5.2), refused
// with both names, never merged. On ext4 they are two real directories.
func TestImportDirectoriesDifferingInCase(t *testing.T) {
	e := newEnv(t)
	e.flac("A/1.flac", track{tags: []string{"ALBUM=A", "TITLE=a"}})
	e.put("A/Scans/front.jpg", []byte("front"))
	e.put("A/scans/back.jpg", []byte("back"))
	b := e.importDir("")
	j := e.failed(b, "A", catalog.CodeAttachmentCollision)
	if !strings.Contains(j.Message, `"Scans/front.jpg"`) || !strings.Contains(j.Message, `"scans/back.jpg"`) {
		t.Errorf("message %q does not name both", j.Message)
	}
}
