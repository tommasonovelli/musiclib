package render

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/names"
)

// hashOf is a fixed, valid blob hash for a label.
func hashOf(label string) string {
	s := sha256.Sum256([]byte(label))
	return hex.EncodeToString(s[:])
}

func flacBlob(label string) jobs.SnapshotBlob {
	return jobs.SnapshotBlob{Hash: hashOf(label), Size: int64(1000 + len(label)), Format: catalog.FormatFLAC}
}

func textOf(s string) jobs.Text { return jobs.Text{String: s, Valid: true} }

func track(disc, no int, title string) jobs.SnapshotTrack {
	return jobs.SnapshotTrack{ID: uuid.New(), Disc: disc, No: no, Title: title, SourcePath: title + ".flac",
		Blob: flacBlob(strconv.Itoa(disc) + "/" + strconv.Itoa(no) + title)}
}

func attachment(rel string) jobs.SnapshotAttachment {
	return jobs.SnapshotAttachment{ID: uuid.New(), RelPath: rel, Blob: jobs.SnapshotBlob{Hash: hashOf("att " + rel), Size: 10}}
}

// kindOfBlue is the example of DESIGN.md §1.1.
func kindOfBlue() *jobs.RenderSnapshot {
	t1 := track(1, 1, "So What")
	t1.Lyrics = jobs.SnapshotBlob{Hash: hashOf("lrc"), Size: 42}
	return &jobs.RenderSnapshot{
		Attempt:       jobs.Attempt{JobID: uuid.New(), Ticket: 7},
		RenderVersion: Version,
		Artist:        jobs.SnapshotArtist{ID: uuid.New(), Name: "Miles Davis", Revision: 1},
		Album: jobs.SnapshotAlbum{ID: uuid.New(), Title: "Kind of Blue", Year: 1959, Genre: textOf("Jazz"),
			Revision: 3},
		Cover:       jobs.SnapshotBlob{Hash: hashOf("cover"), Size: 5000, Format: catalog.FormatJPEG},
		Tracks:      []jobs.SnapshotTrack{t1, track(1, 2, "Freddie Freeloader")},
		Attachments: []jobs.SnapshotAttachment{attachment("booklet.pdf"), attachment("Scans/front.jpg"), attachment("rip.log")},
	}
}

func mustPlan(t *testing.T, s *jobs.RenderSnapshot) Plan {
	t.Helper()
	p, err := NewPlan(s, Version)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return p
}

func wantRenderCode(t *testing.T, err error, code string) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error %v, want %s", err, code)
	}
	return e
}

// The layout of §1.1 and §5.1, with the album's identity and revision.
func TestPlanKindOfBlue(t *testing.T) {
	s := kindOfBlue()
	p := mustPlan(t, s)
	want := []string{
		"01 - So What.flac", "01 - So What.lrc", "02 - Freddie Freeloader.flac",
		"Extras/Scans/front.jpg", "Extras/booklet.pdf", "Extras/rip.log", "cover.jpg",
	}
	if got := p.Files(); !slices.Equal(got, want) {
		t.Fatalf("files %q\nwant  %q", got, want)
	}
	if p.Dir != catalog.AlbumPath("Miles Davis", "Kind of Blue") || p.Dir.Path != "Miles Davis/Kind of Blue" {
		t.Fatalf("dir %+v", p.Dir)
	}
	if p.AlbumID != s.Album.ID || p.AlbumRevision != 3 || p.RenderVersion != Version || p.Removal {
		t.Fatalf("identity %+v", p)
	}
	if p.Cover == nil || p.Cover.Path != "cover.jpg" || p.Cover.MIME != "image/jpeg" || p.Cover.Blob.Hash != s.Cover.Hash {
		t.Fatalf("cover %+v", p.Cover)
	}
	for i, tr := range p.Tracks {
		if tr.Blob.Hash != s.Tracks[i].Blob.Hash || tr.Blob.Size != s.Tracks[i].Blob.Size || tr.Format != "flac" {
			t.Errorf("track %d: %+v", i, tr)
		}
	}
	for _, c := range p.Copies {
		if c.Path == "01 - So What.lrc" && c.Blob.Hash != hashOf("lrc") {
			t.Errorf("lyrics blob %+v", c)
		}
		if c.Path == "Extras/booklet.pdf" && c.Blob.Hash != hashOf("att booklet.pdf") {
			t.Errorf("attachment blob %+v", c)
		}
	}
	want1 := media.TagValues{Title: "So What", Artist: "Miles Davis", AlbumArtist: "Miles Davis", Album: "Kind of Blue",
		Track: 1, TrackTotal: 2, Disc: 1, DiscTotal: 1, Date: "1959", Genre: "Jazz"}
	if p.Tracks[0].Tags != want1 {
		t.Fatalf("tags %+v\nwant %+v", p.Tracks[0].Tags, want1)
	}
}

// §5.1: the paths of tracks, discs, LRC files and the cover.
func TestPlanPaths(t *testing.T) {
	withTracks := func(ts ...jobs.SnapshotTrack) *jobs.RenderSnapshot {
		s := kindOfBlue()
		s.Tracks, s.Attachments, s.Cover = ts, nil, jobs.SnapshotBlob{}
		return s
	}
	lyric := func(tr jobs.SnapshotTrack) jobs.SnapshotTrack {
		tr.Lyrics = jobs.SnapshotBlob{Hash: hashOf("lrc " + tr.Title), Size: 1}
		return tr
	}
	pngCover := withTracks(track(1, 1, "a"))
	pngCover.Cover = jobs.SnapshotBlob{Hash: hashOf("png"), Size: 9, Format: catalog.FormatPNG}
	tests := []struct {
		name string
		s    *jobs.RenderSnapshot
		want []string
	}{
		{"single disc", withTracks(track(1, 1, "a"), track(1, 2, "b")), []string{"01 - a.flac", "02 - b.flac"}},
		{"multi-disc", withTracks(track(1, 1, "a"), track(2, 1, "b")), []string{"Disc 1/01 - a.flac", "Disc 2/01 - b.flac"}},
		{"a single disc numbered 2", withTracks(track(2, 1, "a"), track(2, 2, "b")),
			[]string{"Disc 2/01 - a.flac", "Disc 2/02 - b.flac"}},
		{"disc 10", withTracks(track(10, 3, "a")), []string{"Disc 10/03 - a.flac"}},
		{"widths", withTracks(track(1, 9, "a"), track(1, 10, "b"), track(1, 100, "c"), track(1, 999, "d")),
			[]string{"09 - a.flac", "10 - b.flac", "100 - c.flac", "999 - d.flac"}},
		{"png cover", pngCover, []string{"01 - a.flac", "cover.png"}},
		{"LRC next to its track", withTracks(lyric(track(1, 1, "a")), track(1, 2, "b")),
			[]string{"01 - a.flac", "01 - a.lrc", "02 - b.flac"}},
		{"LRC in its disc", withTracks(track(1, 1, "a"), lyric(track(2, 3, "b"))),
			[]string{"Disc 1/01 - a.flac", "Disc 2/03 - b.flac", "Disc 2/03 - b.lrc"}},
		{"title sanitized", withTracks(track(1, 1, `AC/DC: "Live"?`), track(1, 2, " .dots. ")),
			[]string{"01 - AC_DC_ _Live__.flac", "02 -  .dots. .flac"}},
		{"title NFC", withTracks(track(1, 1, "Cafe"+acute)), []string{"01 - Café.flac"}},
		{"unordered snapshot", withTracks(track(2, 1, "b"), track(1, 2, "a2"), track(1, 1, "a1")),
			[]string{"Disc 1/01 - a1.flac", "Disc 1/02 - a2.flac", "Disc 2/01 - b.flac"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mustPlan(t, tc.s)
			if got := p.Files(); !slices.Equal(got, tc.want) {
				t.Fatalf("files %q\nwant  %q", got, tc.want)
			}
			for i := 1; i < len(p.Tracks); i++ {
				a, b := p.Tracks[i-1].Tags, p.Tracks[i].Tags
				if a.Disc > b.Disc || a.Disc == b.Disc && a.Track >= b.Track {
					t.Fatalf("tracks not in disc and number order: %+v then %+v", a, b)
				}
			}
		})
	}
	// %02d gives the §5.1 number exactly: at least two digits, no extra zero.
	for no := 1; no <= 999; no++ {
		s := withTracks(track(1, no, "t"))
		want := strconv.Itoa(no)
		if no < 10 {
			want = "0" + want
		}
		if got := mustPlan(t, s).Tracks[0].Path; got != want+" - t.flac" {
			t.Fatalf("number %d: %q", no, got)
		}
	}
}

// §5.1, §5.2: every attachment under Extras/, sanitized per segment.
func TestPlanAttachments(t *testing.T) {
	s := kindOfBlue()
	s.Attachments = []jobs.SnapshotAttachment{
		attachment("Scans/Ünïcödé/a:b?.jpg"),
		attachment("NFD/Cafe" + acute + ".txt"),
		attachment(".hidden"),
		attachment("CON.txt"),
		attachment("deep/" + strings.Repeat("d/", 14) + "f"),
		attachment("cover.jpg"),
		attachment(ReceiptName),
		attachment("01 - So What.flac"),
		attachment("Disc 1/x"),
	}
	want := []string{
		"Extras/.musiclib.json", "Extras/01 - So What.flac", "Extras/CON.txt", "Extras/Disc 1/x", "Extras/NFD/Café.txt",
		"Extras/Scans/Ünïcödé/a_b_.jpg", "Extras/cover.jpg", "Extras/deep/" + strings.Repeat("d/", 14) + "f", "Extras/hidden",
	}
	var got []string
	for _, c := range mustPlan(t, s).Copies {
		if strings.HasPrefix(c.Path, "Extras/") {
			got = append(got, c.Path)
		}
	}
	// "CON.txt" becomes "_CON.txt"; "musiclib.json" loses its dot.
	want[0], want[2] = "Extras/musiclib.json", "Extras/_CON.txt"
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("attachments %q\nwant        %q", got, want)
	}
}

// §5.2, §9.1 step 2: collisions after normalization are errors naming both
// entries, found before any copy; nothing is suffixed or dropped.
func TestPlanCollisions(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(*jobs.RenderSnapshot)
		names []string
	}{
		{"two attachments by case", func(s *jobs.RenderSnapshot) {
			s.Attachments = []jobs.SnapshotAttachment{attachment("Scans/a.jpg"), attachment("scans/A.JPG")}
		}, []string{"Scans/a.jpg", "scans/A.JPG"}},
		{"two attachments by sanitization", func(s *jobs.RenderSnapshot) {
			s.Attachments = []jobs.SnapshotAttachment{attachment("a:b.txt"), attachment("a?b.txt")}
		}, []string{"a:b.txt", "a?b.txt"}},
		{"two attachments by NFC and casefold", func(s *jobs.RenderSnapshot) {
			s.Attachments = []jobs.SnapshotAttachment{attachment("Straße.txt"), attachment("STRASSE.txt")}
		}, []string{"Straße.txt", "STRASSE.txt"}},
		{"a file and a directory", func(s *jobs.RenderSnapshot) {
			s.Attachments = []jobs.SnapshotAttachment{attachment("notes"), attachment("Notes/x.txt")}
		}, []string{"notes", "Notes/x.txt"}},
		{"a directory and a later file", func(s *jobs.RenderSnapshot) {
			s.Attachments = []jobs.SnapshotAttachment{attachment("a/b/c.txt"), attachment("a/B")}
		}, []string{"a/B", "a/b/c.txt"}},
		// Owner decision 2026-09-23 (N-131): one directory spelled two ways.
		{"directories differing in case", func(s *jobs.RenderSnapshot) {
			s.Attachments = []jobs.SnapshotAttachment{attachment("Scans/a.jpg"), attachment("scans/b.jpg")}
		}, []string{"Scans/a.jpg", "scans/b.jpg"}},
		{"nested directories differing in case", func(s *jobs.RenderSnapshot) {
			s.Attachments = []jobs.SnapshotAttachment{attachment("Art/Scans/a.jpg"), attachment("Art/SCANS/b.jpg")}
		}, []string{"Art/Scans/a.jpg", "Art/SCANS/b.jpg"}},
		{"two tracks after sanitization", func(s *jobs.RenderSnapshot) {
			s.Tracks = []jobs.SnapshotTrack{track(1, 1, "A/B"), track(1, 1, "A_B")}
		}, []string{`track 1.1 "A/B"`, `track 1.1 "A_B"`}},
		{"two tracks by case", func(s *jobs.RenderSnapshot) {
			s.Tracks = []jobs.SnapshotTrack{track(1, 1, "Intro"), track(1, 1, "INTRO")}
		}, []string{`track 1.1 "Intro"`, `track 1.1 "INTRO"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := kindOfBlue()
			tc.edit(s)
			_, err := NewPlan(s, Version)
			e := wantRenderCode(t, err, CodePathCollision)
			if !slices.Equal(e.Names, tc.names) {
				t.Fatalf("names %q, want %q", e.Names, tc.names)
			}
			for _, n := range tc.names {
				if !strings.Contains(e.Message, n) && !strings.Contains(e.Message, strconv.Quote(n)) {
					t.Errorf("the message %q does not name %q", e.Message, n)
				}
			}
		})
	}
}

// §5.2's limits: a component is truncated with its hash suffix and keeps
// its extension, and the LRC follows the track's truncated name; a path
// beyond 1,024 bytes or 16 levels is refused.
func TestPlanLimits(t *testing.T) {
	s := kindOfBlue()
	long := strings.Repeat("Título largo ", 30)
	s.Artist.Name = strings.Repeat("Artista ", 40)
	s.Album.Title = strings.Repeat("Álbum ", 50)
	s.Tracks[0].Title = long
	p := mustPlan(t, s)
	for _, seg := range strings.Split(p.Dir.Path, "/") {
		if len(seg) > names.MaxSegmentBytes || !strings.Contains(seg, "~") {
			t.Fatalf("dir segment %q (%d bytes)", seg, len(seg))
		}
	}
	tr := p.Tracks[0].Path
	if len(tr) > names.MaxSegmentBytes || !strings.HasSuffix(tr, ".flac") || !strings.HasPrefix(tr, "01 - Título") {
		t.Fatalf("track %q (%d bytes)", tr, len(tr))
	}
	if tr != names.FileSegment("01 - "+long+".flac") {
		t.Fatalf("track %q is not the sanitized name", tr)
	}
	stem := strings.TrimSuffix(tr, ".flac")
	if !slices.Contains(p.Files(), stem+".lrc") {
		t.Fatalf("no LRC with the track's basename %q in %q", stem, p.Files())
	}
	if p.Tracks[0].Tags.Title != long {
		t.Fatal("the tag keeps the full title")
	}

	for name, rel := range map[string]string{
		"1,025 bytes": strings.Repeat(strings.Repeat("x", 169)+"/", 6) + strings.Repeat("y", 1025-6*170),
		"17 levels":   strings.Repeat("d/", 16) + "f",
		"absolute":    "/etc/passwd",
		"dot-dot":     "a/../../b",
	} {
		t.Run(name, func(t *testing.T) {
			s := kindOfBlue()
			s.Attachments = append(s.Attachments, attachment(rel))
			_, err := NewPlan(s, Version)
			e := wantRenderCode(t, err, CodePathInvalid)
			if !slices.Equal(e.Names, []string{rel}) || names.Code(err) == "" {
				t.Fatalf("names %q, names code %q", e.Names, names.Code(err))
			}
		})
	}
	// Exactly 1,024 bytes and 16 levels are allowed.
	s = kindOfBlue()
	ok := strings.Repeat(strings.Repeat("x", 169)+"/", 6) + strings.Repeat("y", 1024-6*170)
	s.Attachments = []jobs.SnapshotAttachment{attachment(ok), attachment(strings.Repeat("d/", 15) + "f")}
	mustPlan(t, s)
}

// §8.2 and §4.1: inherited and overridden artist and genre, the explicit
// empty genre, totals, year, compilation, and the cover.
func TestPlanTags(t *testing.T) {
	s := kindOfBlue()
	s.Album.Compilation = true
	a := track(1, 1, "a")
	b := track(1, 5, "b")
	b.Artist = textOf("Coltrane")
	b.Genre = textOf("Bebop")
	c := track(1, 2, "c")
	c.Genre = textOf("")
	d := track(3, 1, "d")
	s.Tracks = []jobs.SnapshotTrack{a, c, b, d}
	p := mustPlan(t, s)
	base := media.TagValues{AlbumArtist: "Miles Davis", Album: "Kind of Blue", DiscTotal: 3, Date: "1959", Compilation: true}
	want := []media.TagValues{
		{Title: "a", Artist: "Miles Davis", Track: 1, TrackTotal: 5, Disc: 1, Genre: "Jazz"},
		{Title: "c", Artist: "Miles Davis", Track: 2, TrackTotal: 5, Disc: 1, Genre: ""},
		{Title: "b", Artist: "Coltrane", Track: 5, TrackTotal: 5, Disc: 1, Genre: "Bebop"},
		{Title: "d", Artist: "Miles Davis", Track: 1, TrackTotal: 1, Disc: 3, Genre: "Jazz"},
	}
	for i, w := range want {
		w.AlbumArtist, w.Album, w.DiscTotal, w.Date, w.Compilation = base.AlbumArtist, base.Album, base.DiscTotal, base.Date, base.Compilation
		if p.Tracks[i].Tags != w {
			t.Errorf("track %d: %+v\nwant     %+v", i, p.Tracks[i].Tags, w)
		}
	}

	s = kindOfBlue()
	s.Album.Genre, s.Album.Year, s.Album.Compilation, s.Cover = jobs.Text{}, 0, false, jobs.SnapshotBlob{}
	s.Tracks[1].Genre = textOf("Modal")
	p = mustPlan(t, s)
	if g := p.Tracks[0].Tags; g.Genre != "" || g.Date != "" || g.Compilation {
		t.Errorf("no album genre, year or compilation: %+v", g)
	}
	if p.Tracks[1].Tags.Genre != "Modal" || p.Cover != nil {
		t.Errorf("%+v %+v", p.Tracks[1].Tags, p.Cover)
	}
	for year, want := range map[int]string{1: "0001", 999: "0999", 1959: "1959", 9999: "9999"} {
		s := kindOfBlue()
		s.Album.Year = year
		if got := mustPlan(t, s).Tracks[0].Tags.Date; got != want {
			t.Errorf("year %d: %q, want %q", year, got, want)
		}
	}
}

// §9.1 step 3: a trashed album is a removal, with no file.
func TestPlanRemoval(t *testing.T) {
	s := kindOfBlue()
	s.Album.Deleted = true
	s.Album.PublishedPath = "Miles Davis/Kind of Blue"
	s.Tracks = append(s.Tracks, track(1, 1, "a collision that does not matter"))
	p := mustPlan(t, s)
	if !p.Removal || p.Cover != nil || p.Tracks != nil || p.Copies != nil || p.Dir != (catalog.Path{}) || len(p.Files()) != 0 {
		t.Fatalf("removal plan %+v", p)
	}
	if p.AlbumID != s.Album.ID || p.AlbumRevision != s.Album.Revision || p.RenderVersion != Version {
		t.Fatalf("identity %+v", p)
	}
	// A trashed album may have no track left (its tracks were moved): still
	// a removal.
	s.Tracks = nil
	if p := mustPlan(t, s); !p.Removal || len(p.Files()) != 0 {
		t.Fatalf("removal plan without tracks %+v", p)
	}
}

func TestPlanRefusals(t *testing.T) {
	tests := []struct {
		name string
		edit func(*jobs.RenderSnapshot)
		code string
	}{
		{"another render version", func(s *jobs.RenderSnapshot) { s.RenderVersion = "other" }, CodeVersionMismatch},
		{"no tracks", func(s *jobs.RenderSnapshot) { s.Tracks = nil }, CodeInvalidSnapshot},
		{"not audio", func(s *jobs.RenderSnapshot) { s.Tracks[0].Blob.Format = "" }, CodeInvalidSnapshot},
		{"a GIF cover", func(s *jobs.RenderSnapshot) { s.Cover.Format = "" }, CodeInvalidSnapshot},
		{"an invalid hash", func(s *jobs.RenderSnapshot) { s.Tracks[1].Blob.Hash = strings.ToUpper(s.Tracks[1].Blob.Hash) }, CodeInvalidSnapshot},
		{"an invalid lyrics hash", func(s *jobs.RenderSnapshot) { s.Tracks[0].Lyrics.Hash = "x" }, CodeInvalidSnapshot},
		{"an invalid attachment hash", func(s *jobs.RenderSnapshot) { s.Attachments[0].Blob.Hash = "" }, CodeInvalidSnapshot},
		{"a negative size", func(s *jobs.RenderSnapshot) { s.Cover.Size = -1 }, CodeInvalidSnapshot},
		{"disc 0", func(s *jobs.RenderSnapshot) { s.Tracks[0].Disc = 0 }, CodeInvalidSnapshot},
		{"number 1000", func(s *jobs.RenderSnapshot) { s.Tracks[0].No = 1000 }, CodeInvalidSnapshot},
		{"an empty title", func(s *jobs.RenderSnapshot) { s.Tracks[0].Title = "" }, CodeInvalidSnapshot},
		{"an empty artist override", func(s *jobs.RenderSnapshot) { s.Tracks[0].Artist = textOf("") }, CodeInvalidSnapshot},
		{"revision 0", func(s *jobs.RenderSnapshot) { s.Album.Revision = 0 }, CodeInvalidSnapshot},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := kindOfBlue()
			tc.edit(s)
			_, err := NewPlan(s, Version)
			wantRenderCode(t, err, tc.code)
		})
	}
	if _, err := NewPlan(kindOfBlue(), ""); Code(err) != CodeVersionMismatch {
		t.Errorf("an empty render version: %v", err)
	}
	if _, err := NewPlan(nil, Version); Code(err) != CodeInvalidArgument {
		t.Errorf("no snapshot: %v", err)
	}
}

// §6.2: the plan is a function of the snapshot and render_version only: the
// same input gives the same plan, and it shares no memory with the snapshot.
func TestPlanIsAPureValue(t *testing.T) {
	s := kindOfBlue()
	p1, p2 := mustPlan(t, s), mustPlan(t, kindOfBlueCopy(s))
	if !reflect.DeepEqual(p1, p2) {
		t.Fatal("two plans of the same snapshot differ")
	}
	before := mustPlan(t, s)
	s.Tracks[0].Title, s.Tracks[0].Blob.Hash, s.Attachments[0].RelPath = "changed", hashOf("x"), "changed"
	s.Artist.Name = "changed"
	if !reflect.DeepEqual(before, p1) {
		t.Fatal("changing the snapshot changed the plan")
	}
}

func kindOfBlueCopy(s *jobs.RenderSnapshot) *jobs.RenderSnapshot {
	c := *s
	c.Tracks = slices.Clone(s.Tracks)
	c.Attachments = slices.Clone(s.Attachments)
	return &c
}

// §6.2, §13.2: the planner does no I/O and reads no clock. plan.go imports
// nothing that could, and uses the blob store only for ValidateSHA.
func TestPlannerIsPure(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "plan.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{`"fmt"`: true, `"slices"`: true, `"strings"`: true, `"github.com/google/uuid"`: true,
		`"musiclib/internal/blobstore"`: true, `"musiclib/internal/catalog"`: true, `"musiclib/internal/jobs"`: true,
		`"musiclib/internal/media"`: true, `"musiclib/internal/names"`: true}
	for _, imp := range f.Imports {
		if !allowed[imp.Path.Value] {
			t.Errorf("plan.go imports %s", imp.Path.Value)
		}
	}
	body, err := os.ReadFile("plan.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, used := range regexp.MustCompile(`blobstore\.[A-Za-z]+`).FindAllString(string(body), -1) {
		if used != "blobstore.ValidateSHA" {
			t.Errorf("plan.go uses %s", used)
		}
	}
}

// acute is U+0301 COMBINING ACUTE ACCENT: "Cafe"+acute is the NFD spelling
// of "Café", built from its code point so that no editor can normalize it.
var acute = string(rune(0x0301))

// MP3 tracks (since RendererRevision 2): named with ".mp3", the same
// expected tags as any track; an album may mix FLAC and MP3 (N-157).
func TestPlanMP3(t *testing.T) {
	s := kindOfBlue()
	s.Tracks[1].Blob.Format = catalog.FormatMP3
	p, err := NewPlan(s, Version)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p.Tracks[0].Path, ".flac") || !strings.HasSuffix(p.Tracks[1].Path, ".mp3") ||
		p.Tracks[1].Format != catalog.FormatMP3 || p.Tracks[1].Tags.Title != s.Tracks[1].Title {
		t.Fatalf("tracks %+v", p.Tracks)
	}
}

// M4A tracks (since RendererRevision 3): AAC and ALAC alike are named with
// ".m4a", with the same expected tags as any track; an album may mix every
// format (N-157). Two tracks whose names differ only by the codec collide.
func TestPlanM4A(t *testing.T) {
	s := kindOfBlue()
	s.Tracks[0].Blob.Format = catalog.FormatM4AAAC
	s.Tracks[1].Blob.Format = catalog.FormatM4AALAC
	p, err := NewPlan(s, Version)
	if err != nil {
		t.Fatal(err)
	}
	for i, tr := range p.Tracks {
		if !strings.HasSuffix(tr.Path, ".m4a") || tr.Format != s.Tracks[i].Blob.Format || tr.Tags.Title != s.Tracks[i].Title {
			t.Fatalf("track %d: %+v", i, tr)
		}
	}
	s.Tracks[1].Title, s.Tracks[1].No = s.Tracks[0].Title, s.Tracks[0].No
	if _, err := NewPlan(s, Version); Code(err) != CodePathCollision && Code(err) != CodeInvalidSnapshot {
		t.Fatalf("two tracks of one name: %v", err)
	}
}
