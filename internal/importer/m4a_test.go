package importer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// M4A albums (DESIGN.md §7.2-§7.4, §8.1): AAC and ALAC from the pinned FFmpeg,
// tagged by FFmpeg's own MP4 muxer (a real tagger: iTunes atoms, the cover as
// a covr JPEG), never by the helper under test.

// m4a is 0.3 s of a sine at freq Hz in an M4A of codec ("aac" or "alac"),
// with the metadata key=value pairs and, when cover is not nil, the JPEG
// as its covr.
func m4a(t testing.TB, freq int, codec string, cover []byte, metadata ...string) []byte {
	t.Helper()
	dir := t.TempDir()
	args := []string{"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=44100:duration=0.3", freq)}
	maps := []string{"-map", "0"}
	if cover != nil {
		writeFile(t, filepath.Join(dir, "c.jpg"), cover)
		args = append(args, "-i", filepath.Join(dir, "c.jpg"))
		maps = append(maps, "-map", "1", "-c:v", "copy", "-disposition:v", "attached_pic")
	}
	args = append(append(args, maps...), "-ac", "2", "-c:a", codec)
	for _, kv := range metadata {
		args = append(args, "-metadata", kv)
	}
	out := filepath.Join(dir, "a.m4a")
	ffmpeg(t, append(args, out)...)
	return readFile(t, out)
}

// An M4A album with an AAC and an ALAC track: the tags FFmpeg writes (title,
// artist, album, track "N/M", date, genre), the covr of the first track as
// the cover (§7.4), a genre any M4A holds without a warning (N-162 is about
// MP3 only).
func TestImportM4AAlbum(t *testing.T) {
	e := newEnv(t)
	cover := jpegImage(t, 16, 16, 9)
	tags := func(no int, title string) []string {
		return []string{"title=" + title, "artist=Miles Davis", "album=Kind of Blue", fmt.Sprintf("track=%d/2", no),
			"date=1959", "genre=(Rock)", "comment=Columbia"}
	}
	e.put("Kind of Blue/01.m4a", m4a(t, 440, "aac", cover, tags(1, "So What")...))
	e.put("Kind of Blue/02.m4a", m4a(t, 550, "alac", nil, tags(2, "Freddie Freeloader")...))
	b := e.importDir("")
	a := e.done(b, "Kind of Blue")
	if a.Artist != "Miles Davis" || a.Title != "Kind of Blue" || a.Year == nil || *a.Year != 1959 ||
		a.Genre == nil || *a.Genre != "(Rock)" || a.Cover == nil || *a.Cover != sha(cover) {
		t.Fatalf("album %+v", a)
	}
	for i, want := range []struct{ title, format string }{{"So What", media.FormatM4AAAC}, {"Freddie Freeloader", media.FormatM4AALAC}} {
		tr := a.Tracks[i]
		if tr.Title != want.title || int(tr.No) != i+1 || tr.BlobFormat == nil || *tr.BlobFormat != want.format || tr.Artist != nil {
			t.Errorf("track %d: %+v", i, tr)
		}
	}
	if ws := e.importJob(b, "Kind of Blue").Warnings; len(ws) != 0 {
		t.Errorf("warnings %+v", ws)
	}
}

// FLAC, MP3 and M4A in one candidate (N-157): each track keeps its format.
func TestImportMixedFLACMP3AndM4A(t *testing.T) {
	e := newEnv(t)
	e.flac("Mixed/01.flac", track{tags: []string{"TITLE=One", "ARTIST=A", "ALBUM=Mixed", "TRACKNUMBER=1"}})
	e.put("Mixed/02.mp3", mp3(t, 550, id3v23(id3Frame23("TIT2", latin1("Two")), id3Frame23("TPE1", latin1("A")),
		id3Frame23("TALB", latin1("Mixed")), id3Frame23("TRCK", latin1("2")))))
	e.put("Mixed/03.m4a", m4a(t, 660, "alac", nil, "title=Three", "artist=A", "album=Mixed", "track=3"))
	a := e.done(e.importDir(""), "Mixed")
	if len(a.Tracks) != 3 || *a.Tracks[0].BlobFormat != media.FormatFLAC || *a.Tracks[1].BlobFormat != media.FormatMP3 ||
		*a.Tracks[2].BlobFormat != media.FormatM4AALAC || a.Tracks[2].Title != "Three" {
		t.Fatalf("tracks %+v", a.Tracks)
	}
}

// §8.1 for M4A at import: an alias that disagrees with the canonical atom is
// a tag_conflict warning. N-092 for M4A: a metadata structure the writer
// could not keep refuses the file with the field named; a damaged M4A is
// corrupt audio.
func TestImportM4AConflictsAndRefusals(t *testing.T) {
	e := newEnv(t)
	e.put("Conflict/01.m4a", m4a(t, 440, "aac", nil, "title=One", "album_artist=AA", "album=Conflict", "genre=Jazz"))
	// FFmpeg writes the genre as ©gen; a numeric gnre of another genre next
	// to it (Rock: ID3v1 genre 17, plus one) is a conflict.
	gnre := []byte{0, 0, 0, 26, 'g', 'n', 'r', 'e', 0, 0, 0, 18, 'd', 'a', 't', 'a', 0, 0, 0, 0, 0, 0, 0, 0, 0, 18}
	e.put("Conflict/02.m4a", appendItem(t, m4a(t, 550, "aac", nil, "title=Two", "album=Conflict", "genre=Jazz"), gnre))
	foreign := bytes.Replace(m4a(t, 440, "alac", nil, "title=x"), []byte("mdir"), []byte("mdta"), 1)
	e.put("Foreign/01.m4a", foreign)
	whole := m4a(t, 440, "alac", nil)
	e.put("Cut/01.m4a", whole[:len(whole)/2])
	b := e.importDir("")
	if a := e.done(b, "Conflict"); a.Genre == nil || *a.Genre != "Jazz" {
		t.Errorf("album %+v", a)
	}
	if j := e.importJob(b, "Conflict"); !hasWarning(j.Warnings, jobs.WarnTagConflict, "02.m4a") {
		t.Errorf("no conflict warning: %v", warningCodes(j.Warnings))
	}
	if j := e.failed(b, "Foreign", CodeUnrenderableTag); !strings.Contains(j.Message, "mp4:moov/udta/meta") ||
		!strings.Contains(j.Message, "foreign_metadata") {
		t.Errorf("message %q", j.Message)
	}
	e.failed(b, "Cut", CodeCorruptAudio)
}

// appendItem adds an item at the end of the ilst of an M4A written by
// FFmpeg without faststart: the ilst ends meta, which ends udta, which ends
// moov, which ends the file, so the item goes at the end of the file and
// the four sizes grow (no chunk offset points past it).
func appendItem(t testing.TB, file, item []byte) []byte {
	t.Helper()
	out := append(bytes.Clone(file), item...)
	for _, typ := range []string{"moov", "udta", "meta", "ilst"} {
		i := bytes.LastIndex(file, []byte(typ)) - 4
		size := binary.BigEndian.Uint32(file[i:])
		if i < 0 || i+int(size) != len(file) {
			t.Fatalf("%s does not end the file", typ)
		}
		binary.BigEndian.PutUint32(out[i:], size+uint32(len(item)))
	}
	return out
}
