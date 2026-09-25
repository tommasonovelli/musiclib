package importer

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// MP3 albums (DESIGN.md §7.2-§7.4, §8.1): audio from the pinned LAME, tags
// written byte by byte here from the ID3v2.3, APE and ID3v1 specifications,
// never by the helper under test.

// lamePath is the test-only MP3 encoder of the toolchain images.
const lamePath = "/usr/local/bin/lame"

var (
	mp3Mu    sync.Mutex
	mp3Cache = map[int][]byte{}
)

// baseMP3 is 0.3 s of a sine at freq Hz, encoded by LAME (VBR, with its
// gapless header), without any tag.
func baseMP3(t testing.TB, freq int) []byte {
	t.Helper()
	mp3Mu.Lock()
	defer mp3Mu.Unlock()
	if b, ok := mp3Cache[freq]; ok {
		return b
	}
	dir := t.TempDir()
	wav := filepath.Join(dir, "a.wav")
	ffmpeg(t, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=44100:duration=0.3", freq),
		"-ac", "2", "-c:a", "pcm_s16le", wav)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out := filepath.Join(dir, "a.mp3")
	if b, err := exec.CommandContext(ctx, lamePath, "--quiet", "-V2", wav, out).CombinedOutput(); err != nil {
		t.Fatalf("lame: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	mp3Cache[freq] = b
	return b
}

// id3Frame23 is an ID3v2.3 frame: identifier, no flags, body.
func id3Frame23(id string, body []byte) []byte {
	out := append([]byte(id), binary.BigEndian.AppendUint32(nil, uint32(len(body)))...)
	return append(append(out, 0, 0), body...)
}

// latin1 is an ID3v2 text frame body in ISO-8859-1.
func latin1(s string) []byte {
	out := []byte{0}
	for _, r := range s {
		out = append(out, byte(r))
	}
	return out
}

// utf16Text is an ID3v2 text frame body in UTF-16 with a little-endian BOM.
func utf16Text(s string) []byte {
	out := []byte{1, 0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

func id3v23(frames ...[]byte) []byte {
	body := bytes.Join(frames, nil)
	return append(append([]byte{'I', 'D', '3', 3, 0, 0}, syncsafe(len(body))...), body...)
}

// apic23 is an APIC frame: Latin-1, MIME, picture type, empty description.
func apic23(mime string, typ byte, img []byte) []byte {
	return id3Frame23("APIC", append(append([]byte{0}, mime...), append([]byte{0, typ, 0}, img...)...))
}

// apeTag is an APEv2 tag with a header, of text items.
func apeTag(items ...[2]string) []byte {
	var body []byte
	for _, it := range items {
		body = binary.LittleEndian.AppendUint32(body, uint32(len(it[1])))
		body = binary.LittleEndian.AppendUint32(body, 0)
		body = append(append(append(body, it[0]...), 0), it[1]...)
	}
	hf := func(flags uint32) []byte {
		out := binary.LittleEndian.AppendUint32([]byte("APETAGEX"), 2000)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(body)+32))
		out = binary.LittleEndian.AppendUint32(out, uint32(len(items)))
		out = binary.LittleEndian.AppendUint32(out, flags)
		return append(out, make([]byte, 8)...)
	}
	return append(append(hf(1<<31|1<<29), body...), hf(1<<31)...)
}

// id3v1Tag is an ID3v1.1 tag (Latin-1 text).
func id3v1Tag(title, artist, album, year, comment string, trackNo, genre byte) []byte {
	field := func(s string, n int) []byte { return append(latin1(s)[1:], make([]byte, n-len(latin1(s)[1:]))...) }
	out := append([]byte("TAG"), field(title, 30)...)
	out = append(out, field(artist, 30)...)
	out = append(out, field(album, 30)...)
	out = append(out, field(year, 4)...)
	out = append(out, field(comment, 28)...)
	return append(out, 0, trackNo, genre)
}

// mp3 is a track: tags before and after the audio of baseMP3(freq).
func mp3(t testing.TB, freq int, before []byte, after ...[]byte) []byte {
	return bytes.Join(append([][]byte{before, baseMP3(t, freq)}, after...), nil)
}

func warningCodes(ws []jobs.Warning) []jobs.WarningCode {
	var out []jobs.WarningCode
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}

// A normal MP3 album: ID3v2.3 tags in Latin-1 and UTF-16, a genre
// reference, the year of TYER, the embedded front cover (§7.4), and the
// unmanaged frames left alone.
func TestImportMP3Album(t *testing.T) {
	e := newEnv(t)
	cover := jpegImage(t, 16, 16, 7)
	tags := func(no int, title string) []byte {
		return id3v23(
			id3Frame23("TIT2", utf16Text(title)), id3Frame23("TPE1", utf16Text("John Coltrane")),
			id3Frame23("TALB", latin1("Blue Train")), id3Frame23("TRCK", latin1(fmt.Sprintf("%d/2", no))),
			id3Frame23("TYER", latin1("1957")), id3Frame23("TCON", latin1("(8)")),
			id3Frame23("COMM", append(latin1("eng"), append([]byte{0}, "Rudy Van Gelder"...)...)),
			id3Frame23("TCOM", latin1("Kenny Dorham, arr. Müller")),
			apic23("image/jpeg", 3, cover))
	}
	e.put("Blue Train/01.mp3", mp3(t, 440, tags(1, "Blue Train")))
	e.put("Blue Train/02.mp3", mp3(t, 550, tags(2, "Moment’s Notice")))
	b := e.importDir("")
	a := e.done(b, "Blue Train")
	if a.Artist != "John Coltrane" || a.Title != "Blue Train" || a.Year == nil || *a.Year != 1957 ||
		a.Genre == nil || *a.Genre != "Jazz" || a.Cover == nil || *a.Cover != sha(cover) {
		t.Fatalf("album %+v", a)
	}
	for i, title := range []string{"Blue Train", "Moment’s Notice"} {
		tr := a.Tracks[i]
		if tr.Title != title || int(tr.No) != i+1 || tr.BlobFormat == nil || *tr.BlobFormat != media.FormatMP3 || tr.Artist != nil {
			t.Errorf("track %d: %+v", i, tr)
		}
	}
	if ws := e.importJob(b, "Blue Train").Warnings; len(ws) != 0 {
		t.Errorf("warnings %+v", ws)
	}
}

// §8.1: ID3v2, then APE, then ID3v1; a disagreement is a warning, and a
// track with only an ID3v1 tag is read from it.
func TestImportMP3Precedence(t *testing.T) {
	e := newEnv(t)
	e.put("Mixed Tags/01.mp3", mp3(t, 440, id3v23(id3Frame23("TIT2", latin1("ID3 title")), id3Frame23("TPE1", latin1("Artist"))),
		apeTag([2]string{"Title", "APE title"}, [2]string{"Album", "The Album"}),
		id3v1Tag("v1 title", "Artist", "The Album", "2004", "", 1, 17)))
	e.put("Mixed Tags/02.mp3", mp3(t, 550, nil, id3v1Tag("Only ID3v1", "Artist", "The Album", "2004", "old comment", 2, 17)))
	b := e.importDir("")
	a := e.done(b, "Mixed Tags")
	if a.Title != "The Album" || a.Artist != "Artist" || *a.Year != 2004 || *a.Genre != "Rock" ||
		a.Tracks[0].Title != "ID3 title" || a.Tracks[1].Title != "Only ID3v1" {
		t.Fatalf("album %+v, tracks %+v", a, a.Tracks)
	}
	j := e.importJob(b, "Mixed Tags")
	if !hasWarning(j.Warnings, jobs.WarnTagConflict, "01.mp3") {
		t.Errorf("no conflict warning: %v", warningCodes(j.Warnings))
	}
}

// FLAC and MP3 in one candidate: §7.2 makes a directory with audio files a
// candidate, whatever their formats, and §8.1 supports both (NOTES.md
// N-157). Each track keeps its format.
func TestImportMixedFLACAndMP3(t *testing.T) {
	e := newEnv(t)
	e.flac("Mixed/01.flac", track{tags: []string{"TITLE=One", "ARTIST=A", "ALBUM=Mixed", "TRACKNUMBER=1"}})
	e.put("Mixed/02.mp3", mp3(t, 550, id3v23(id3Frame23("TIT2", latin1("Two")), id3Frame23("TPE1", latin1("A")),
		id3Frame23("TALB", latin1("Mixed")), id3Frame23("TRCK", latin1("2")))))
	a := e.done(e.importDir(""), "Mixed")
	if len(a.Tracks) != 2 || *a.Tracks[0].BlobFormat != media.FormatFLAC || *a.Tracks[1].BlobFormat != media.FormatMP3 ||
		a.Tracks[1].Title != "Two" {
		t.Fatalf("tracks %+v", a.Tracks)
	}
}

// N-092 for MP3: text is decoded in its encoding (Latin-1 and UTF-16 are
// valid text), and only text that does not decode refuses the file, when
// it is in an unmanaged frame; a damaged MP3 is corrupt audio.
func TestImportMP3Refusals(t *testing.T) {
	e := newEnv(t)
	lone := []byte{1, 0xFF, 0xFE, 0x00, 0xD8}
	e.put("Bad Text/01.mp3", mp3(t, 440, id3v23(id3Frame23("TIT2", latin1("t")), id3Frame23("TCOM", lone))))
	e.put("Bad Title/01.mp3", mp3(t, 440, id3v23(id3Frame23("TIT2", lone), id3Frame23("TALB", latin1("Bad Title")))))
	whole := mp3(t, 440, nil)
	e.put("Cut/01.mp3", whole[:len(whole)/2])
	e.put("Junk/01.mp3", append(bytes.Clone(whole), make([]byte, 300)...))
	b := e.importDir("")
	if j := e.failed(b, "Bad Text", CodeUnrenderableTag); !strings.Contains(j.Message, "id3v2:TCOM") || !strings.Contains(j.Message, "invalid_text") {
		t.Errorf("message %q", j.Message)
	}
	// A managed frame that does not decode is replaced at render: the track
	// is imported, its title from the file name.
	if a := e.done(b, "Bad Title"); a.Tracks[0].Title != "01" {
		t.Errorf("tracks %+v", a.Tracks)
	}
	e.failed(b, "Cut", CodeCorruptAudio)
	e.failed(b, "Junk", CodeCorruptAudio)
}

// Owner decision N-162: a genre read from an MP3 (ID3v2 or APE) that an MP3
// cannot hold as it is is imported as read, never rewritten, with a warning
// naming the file and the value; a genre an MP3 holds gets none.
func TestImportMP3GenreWarning(t *testing.T) {
	e := newEnv(t)
	e.put("Genres/01.mp3", mp3(t, 440, id3v23(id3Frame23("TIT2", latin1("One")), id3Frame23("TCON", latin1("(Rock)")))))
	e.put("Genres/02.mp3", mp3(t, 550, nil, apeTag([2]string{"Title", "Two"}, [2]string{"Genre", "101"})))
	e.put("Genres/03.mp3", mp3(t, 660, id3v23(id3Frame23("TIT2", latin1("Three")), id3Frame23("TCON", latin1("(17)")))))
	b := e.importDir("")
	a := e.done(b, "Genres")
	var genres []string
	for _, tr := range a.Tracks {
		if tr.Genre != nil {
			genres = append(genres, *tr.Genre)
		}
	}
	if a.Genre == nil {
		t.Fatalf("album genre nil, tracks %v", genres)
	}
	all := append([]string{*a.Genre}, genres...)
	if !slices.Contains(all, "(Rock)") || !slices.Contains(all, "101") || !slices.Contains(all, "Rock") {
		t.Fatalf("the genres were not imported as read: album %q, tracks %q", *a.Genre, genres)
	}
	ws := e.importJob(b, "Genres").Warnings
	var got []string
	for _, w := range ws {
		if w.Code == jobs.WarnGenreNotWritable {
			got = append(got, w.Path)
			if !strings.Contains(w.Message, w.Path) {
				t.Errorf("warning %q does not name its file", w.Message)
			}
		}
	}
	if !slices.Equal(got, []string{"01.mp3", "02.mp3"}) {
		t.Fatalf("genre warnings on %q: %+v", got, ws)
	}
	for _, w := range ws {
		if w.Path == "01.mp3" && w.Code == jobs.WarnGenreNotWritable && !strings.Contains(w.Message, `"(Rock)"`) {
			t.Errorf("warning %q does not name the value", w.Message)
		}
	}
}
