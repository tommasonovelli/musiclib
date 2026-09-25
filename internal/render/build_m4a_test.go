package render

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// M4A tracks in a build (DESIGN.md §9.1 step 6 for M4A): AAC and ALAC from
// the pinned FFmpeg, tagged by FFmpeg's MP4 muxer, never by the helper
// under test.

// baseM4A encodes the lavfi source in codec with FFmpeg's MP4 muxer and the
// given metadata; faststart puts moov before the media data.
func baseM4A(t testing.TB, src, codec string, faststart bool, cover []byte, metadata ...string) []byte {
	t.Helper()
	dir := t.TempDir()
	args := []string{"-f", "lavfi", "-i", src}
	if cover != nil {
		p := filepath.Join(dir, "c.jpg")
		if err := os.WriteFile(p, cover, 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-i", p, "-map", "0", "-map", "1", "-c:v", "copy", "-disposition:v", "attached_pic")
	}
	args = append(args, "-ac", "2", "-c:a", codec)
	if faststart {
		args = append(args, "-movflags", "+faststart")
	}
	for _, kv := range metadata {
		args = append(args, "-metadata", kv)
	}
	out := filepath.Join(dir, "a.m4a")
	ffmpeg(t, append(args, out)...)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// m4aTrack adds an M4A track of format to the album.
func (a *album) m4aTrack(disc, no int, title, format string, b []byte) *jobs.SnapshotTrack {
	a.s.Tracks = append(a.s.Tracks, jobs.SnapshotTrack{ID: uuid.New(), Disc: disc, No: no, Title: title,
		SourcePath: title + ".m4a", Blob: a.e.put(b, format)})
	return &a.s.Tracks[len(a.s.Tracks)-1]
}

// An album of an AAC, an ALAC, an MP3 and a FLAC track: every M4A output has
// the planned tags, its unmanaged items, the album's cover as its one covr,
// and the same audio; two builds give the same bytes.
func TestBuildM4AAlbum(t *testing.T) {
	e := newEnv(t)
	a := e.album("Miles Davis", "Kind of Blue")
	a.m4aTrack(1, 1, "So What", catalog.FormatM4AAAC, baseM4A(t, sine(440, 0.4), "aac", true, jpegImage(t, 8, 8, 2),
		"title=Old", "artist=Old", "sort_artist=Davis, Miles", "comment=keep me", "composer=Miles Davis", "genre=Jazz"))
	a.m4aTrack(1, 2, "Freddie Freeloader", catalog.FormatM4AALAC, baseM4A(t, sine(550, 0.4), "alac", false, nil, "title=Old"))
	a.mp3Track(1, 3, "Blue in Green", baseMP3(t, sine(660, 0.4)))
	a.track(1, 4, "All Blues", song{src: sine(770, 0.3)})
	a.cover(jpegImage(t, 32, 24, 1), catalog.FormatJPEG)
	p := a.plan()
	res := e.mustBuild(p)
	files, names, _ := e.staged(res)
	want := []string{ReceiptName, "01 - So What.m4a", "02 - Freddie Freeloader.m4a", "03 - Blue in Green.mp3", "04 - All Blues.flac", "cover.jpg"}
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
	sw := e.inspectAs("sw", files["01 - So What.m4a"], catalog.FormatM4AAAC)
	if !slices.Equal(sw.Managed.Title, []string{"So What"}) || !slices.Equal(sw.Managed.Artist, []string{"Miles Davis"}) ||
		!slices.Equal(sw.Managed.TrackTotal, []string{"4"}) || !slices.Equal(sw.Managed.Genre, []string{"Jazz"}) {
		t.Errorf("managed %+v", sw.Managed)
	}
	for key, want := range map[string][]string{"ilst:©cmt": {"keep me"}, "ilst:©wrt": {"Miles Davis"}} {
		if got := find(sw.Unmanaged, key); !slices.Equal(got, want) {
			t.Errorf("%s: %q", key, got)
		}
	}
	if find(sw.Unmanaged, "ilst:soar") != nil || len(sw.Pictures) != 1 || sw.Pictures[0].SHA256 != a.s.Cover.Hash {
		t.Errorf("unmanaged %+v, pictures %+v", sw.Unmanaged, sw.Pictures)
	}
	res2 := e.mustBuild(p)
	files2, _, _ := e.staged(res2)
	for _, tr := range p.Tracks {
		if !bytes.Equal(files[tr.Path], files2[tr.Path]) {
			t.Errorf("%s differs between two builds", tr.Path)
		}
	}
}

// A metadata structure an M4A write cannot keep fails the build (§8.3).
func TestBuildM4AOpaqueField(t *testing.T) {
	e := newEnv(t)
	a := e.album("A", "B")
	foreign := bytes.Replace(baseM4A(t, sine(440, 0.4), "alac", false, nil, "title=x"), []byte("mdir"), []byte("mdta"), 1)
	a.m4aTrack(1, 1, "t", catalog.FormatM4AALAC, foreign)
	if _, err := e.build(a.plan()); media.Code(err) != media.CodeTagsOpaque {
		t.Fatalf("Build: %v", err)
	}
}
