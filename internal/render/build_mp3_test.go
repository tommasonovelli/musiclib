package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// MP3 tracks in a build (DESIGN.md §9.1 step 6 for MP3): audio from the
// pinned LAME, tags built byte by byte here (ID3v2.3, APE, ID3v1), never by
// the helper under test.

const lamePath = "/usr/local/bin/lame"

var (
	mp3Mu    sync.Mutex
	mp3Cache = map[string][]byte{}
)

// baseMP3 encodes the lavfi source with LAME (VBR, gapless header).
func baseMP3(t testing.TB, src string) []byte {
	t.Helper()
	mp3Mu.Lock()
	defer mp3Mu.Unlock()
	if b, ok := mp3Cache[src]; ok {
		return b
	}
	dir := t.TempDir()
	wav := filepath.Join(dir, "a.wav")
	ffmpeg(t, "-f", "lavfi", "-i", src, "-ac", "2", "-c:a", "pcm_s16le", wav)
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
	mp3Cache[src] = b
	return b
}

func frame23(id string, body []byte) []byte {
	out := binary.BigEndian.AppendUint32([]byte(id), uint32(len(body)))
	return append(append(out, 0, 0), body...)
}

func latin1Text(s string) []byte { return append([]byte{0}, s...) }

func tag23(frames ...[]byte) []byte {
	body := bytes.Join(frames, nil)
	n := len(body)
	return append([]byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}, body...)
}

func apeText(items ...[2]string) []byte {
	var body []byte
	for _, it := range items {
		body = binary.LittleEndian.AppendUint32(body, uint32(len(it[1])))
		body = binary.LittleEndian.AppendUint32(body, 0)
		body = append(append(append(body, it[0]...), 0), it[1]...)
	}
	footer := binary.LittleEndian.AppendUint32([]byte("APETAGEX"), 2000)
	footer = binary.LittleEndian.AppendUint32(footer, uint32(len(body)+32))
	footer = binary.LittleEndian.AppendUint32(footer, uint32(len(items)))
	footer = binary.LittleEndian.AppendUint32(footer, 0)
	return append(append(body, footer...), make([]byte, 8)...)
}

func v1Tag(title, comment string) []byte {
	b := make([]byte, 128)
	copy(b, "TAG")
	copy(b[3:33], title)
	copy(b[97:127], comment)
	b[127] = 255
	return b
}

// mp3Track adds an MP3 track to the album.
func (a *album) mp3Track(disc, no int, title string, b []byte) *jobs.SnapshotTrack {
	a.s.Tracks = append(a.s.Tracks, jobs.SnapshotTrack{ID: uuid.New(), Disc: disc, No: no, Title: title,
		SourcePath: title + ".mp3", Blob: a.e.put(b, catalog.FormatMP3)})
	return &a.s.Tracks[len(a.s.Tracks)-1]
}

func (e *env) inspectAs(name string, b []byte, format string) media.Inspection {
	e.t.Helper()
	in, err := e.tools.Inspect(context.Background(), e.hostFile(name, b), format)
	if err != nil {
		e.t.Fatalf("Inspect %s: %v", name, err)
	}
	return in
}

// An album of MP3 tracks with a FLAC one: every MP3 output has the planned
// tags, its unmanaged frames and APE items, the ID3v1 comment in COMM and
// no ID3v1 tag, and the same audio; two builds give the same bytes.
func TestBuildMP3Album(t *testing.T) {
	e := newEnv(t)
	a := e.album("John Coltrane", "Blue Train")
	rich := slices.Concat(tag23(
		frame23("TIT2", latin1Text("Old title")), frame23("TPE1", latin1Text("Old artist")),
		frame23("TSOP", latin1Text("Coltrane, John")), frame23("TYER", latin1Text("1957")),
		frame23("COMM", append(latin1Text("eng"), append([]byte{0}, "keep me"...)...)),
		frame23("TXXX", append(latin1Text("REPLAYGAIN_TRACK_GAIN"), append([]byte{0}, "-6.50 dB"...)...)),
		frame23("TCOM", latin1Text("Kenny Dorham")),
		frame23("APIC", append(latin1Text("image/png"), append([]byte{0, 3, 0}, pngImage(t, 4, 4, 5)...)...))),
		baseMP3(t, sine(440, 0.4)),
		apeText([2]string{"Title", "APE title"}, [2]string{"REPLAYGAIN_ALBUM_GAIN", "-7.00 dB"}),
		v1Tag("v1 title", "a v1 comment"))
	a.mp3Track(1, 1, "Blue Train", rich)
	a.mp3Track(1, 2, "Moment's Notice", baseMP3(t, sine(550, 0.4)))
	a.track(1, 3, "Locomotion", song{src: sine(660, 0.3), tags: []string{"COMMENT=flac"}})
	a.cover(jpegImage(t, 32, 24, 1), catalog.FormatJPEG)
	p := a.plan()
	res := e.mustBuild(p)
	files, names, _ := e.staged(res)
	want := []string{ReceiptName, "01 - Blue Train.mp3", "02 - Moment's Notice.mp3", "03 - Locomotion.flac", "cover.jpg"}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("staged %q", names)
	}
	cover := &media.ExpectedCover{MIME: "image/jpeg", Size: a.s.Cover.Size, SHA256: a.s.Cover.Hash}
	for _, tr := range p.Tracks {
		in, out := e.blob(tr.Blob.Hash), files[tr.Path]
		before, after := e.inspectAs("in", in, tr.Format), e.inspectAs("out", out, tr.Format)
		if err := media.VerifyTags(tr.Tags, cover, before, after); err != nil {
			t.Errorf("%s: %v", tr.Path, err)
		}
		if e.digest("in", in) != e.digest("out", out) {
			t.Errorf("%s: the audio changed", tr.Path)
		}
	}
	bt := e.inspectAs("bt", files["01 - Blue Train.mp3"], catalog.FormatMP3)
	if !slices.Equal(bt.Managed.Title, []string{"Blue Train"}) || !slices.Equal(bt.Managed.Artist, []string{"John Coltrane"}) ||
		!slices.Equal(bt.Managed.Date, []string{"1959"}) || !slices.Equal(bt.Managed.TrackTotal, []string{"3"}) {
		t.Errorf("managed %+v", bt.Managed)
	}
	for key, want := range map[string][]string{
		"id3v2:COMM:eng:":                  {"keep me"},
		"id3v2:COMM:XXX:legacy-id3v1":      {"a v1 comment"},
		"id3v2:TXXX:REPLAYGAIN_TRACK_GAIN": {"-6.50 dB"},
		"id3v2:TCOM":                       {"Kenny Dorham"},
		"ape:REPLAYGAIN_ALBUM_GAIN":        {"-7.00 dB"},
	} {
		if got := find(bt.Unmanaged, key); !slices.Equal(got, want) {
			t.Errorf("%s: %q", key, got)
		}
	}
	if find(bt.Unmanaged, "id3v1:comment") != nil || len(bt.Pictures) != 1 {
		t.Errorf("unmanaged %+v, pictures %+v", bt.Unmanaged, bt.Pictures)
	}
	out := files["01 - Blue Train.mp3"]
	if string(out[len(out)-128:len(out)-125]) == "TAG" || !bytes.HasPrefix(out, []byte{'I', 'D', '3', 4}) {
		t.Error("the output is not ID3v2.4 without ID3v1")
	}
	// Determinism: another build of the same plan gives the same bytes.
	res2 := e.mustBuild(p)
	files2, _, _ := e.staged(res2)
	for _, tr := range p.Tracks {
		if !bytes.Equal(files[tr.Path], files2[tr.Path]) {
			t.Errorf("%s differs between two builds", tr.Path)
		}
	}
}

// A field an MP3 write cannot keep fails the build (§8.3), and nothing is
// left in work/render.
func TestBuildMP3OpaqueField(t *testing.T) {
	e := newEnv(t)
	a := e.album("A", "B")
	lone := []byte{1, 0xFF, 0xFE, 0x00, 0xD8}
	a.mp3Track(1, 1, "t", slices.Concat(tag23(frame23("TCOM", lone)), baseMP3(t, sine(440, 0.4))))
	if _, err := e.build(a.plan()); media.Code(err) != media.CodeTagsOpaque {
		t.Fatalf("Build: %v", err)
	}
}
