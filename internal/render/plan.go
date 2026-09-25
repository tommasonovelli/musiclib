// Package render builds the output directory of one album (DESIGN.md §9.1,
// §9.2): a pure plan from the claim's snapshot and render_version, then a
// complete build in work/render/<build_id>/album, verified file by file and
// closed by its receipt. Publishing the build is internal/publish's (§9.3).
//
// The planner does no I/O and reads no clock (§6.2, §13.2): NewPlan is a
// function of its arguments only. The builder reads the originals through
// internal/blobstore, writes only through internal/fsops under work/, and
// never reads library/ (§9.1 step 4).
package render

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/names"
)

// ReceiptName is the receipt at the root of every album directory (§1.1,
// §9.2).
const ReceiptName = ".musiclib.json"

// extrasDir holds every attachment (§5.1).
const extrasDir = "Extras"

// Plan is what a build produces for one album, decided before any copy
// (§9.1 step 2): every path, the blob of every file and the expected tags of
// every track. Paths are relative to the album directory, made of final
// segments (§5.2), joined by "/".
//
// A Plan is a value: it shares no memory with the snapshot it came from,
// and nothing modifies it after NewPlan returns it (the builder only reads
// it).
type Plan struct {
	AlbumID uuid.UUID
	// AlbumRevision is the revision the snapshot read; the receipt and the
	// journal record it (§6.3: never the current revision).
	AlbumRevision int64
	RenderVersion string
	// Removal: the album is in the trash. Its publication removes the
	// output, and the plan has no file (§9.1 step 3).
	Removal bool
	// Dir is the album's desired directory under library/, <artist>/<album>
	// (§5.1), with its claim key: catalog.AlbumPath. Zero for a removal.
	Dir catalog.Path
	// Cover is the cover file at the root; nil when the album has none. Its
	// bytes are also the one embedded front cover of every track (§8.2).
	Cover *Cover
	// Tracks are ordered by disc and number (§6.1: one at a time).
	Tracks []Track
	// Copies are the other byte-for-byte copies: each LRC next to its track
	// and every attachment under Extras/, ordered by path bytes.
	Copies []Copy
}

// Blob is the original a file is copied from: its SHA-256, which is its name
// in originals/, and its size.
type Blob struct {
	Hash string
	Size int64
}

// Cover is the album's cover.
type Cover struct {
	// Path is cover.jpg or cover.png (§5.1).
	Path string
	Blob Blob
	// Format is the blob's format, "jpeg" or "png"; MIME the type the tag
	// writer embeds (media.CoverMIME).
	Format string
	MIME   string
}

// Track is one track file.
type Track struct {
	// Path is "<NN> - <title>.<ext>", under "Disc <D>/" when the album is
	// multi-disc (§5.1).
	Path string
	Blob Blob
	// Format is the blob's audio format (blobs.format).
	Format string
	// Tags are the managed tags the file must carry (§8.2); zero values are
	// removals.
	Tags media.TagValues
}

// Copy is a file copied byte for byte (§9.1 step 7).
type Copy struct {
	Path string
	Blob Blob
}

// Files returns the path of every file of the plan except the receipt,
// sorted by bytes: exactly the files the receipt lists (§9.2).
func (p Plan) Files() []string {
	var out []string
	if p.Cover != nil {
		out = append(out, p.Cover.Path)
	}
	for _, t := range p.Tracks {
		out = append(out, t.Path)
	}
	for _, c := range p.Copies {
		out = append(out, c.Path)
	}
	slices.Sort(out)
	return out
}

// extensions are the file extensions of the formats this renderer builds:
// FLAC, MP3 and M4A (AAC and ALAC alike) for tracks, JPEG and PNG for the
// cover.
var (
	trackExtensions = map[string]string{catalog.FormatFLAC: "flac", catalog.FormatMP3: "mp3",
		catalog.FormatM4AAAC: "m4a", catalog.FormatM4AALAC: "m4a"}
	coverExtensions = map[string]string{catalog.FormatJPEG: "jpg", catalog.FormatPNG: "png"}
)

// NewPlan computes the plan of a render from the claim's snapshot and
// render_version (§6.2): a pure function, with no query, no filesystem
// access and no clock. It validates every conflict before anything is
// copied (§9.1 step 2): two entries whose paths collide after
// normalization, a file whose path is a directory of another (§5.2), and
// the limits of §5.2. There are no suffixes and no deduplication: a
// collision is a CodePathCollision naming both entries.
func NewPlan(s *jobs.RenderSnapshot, renderVersion string) (Plan, error) {
	if s == nil {
		return Plan{}, errorf(CodeInvalidArgument, "no snapshot")
	}
	if renderVersion == "" || s.RenderVersion != renderVersion {
		return Plan{}, errorf(CodeVersionMismatch, "the snapshot was claimed for render_version %q, the plan is for %q",
			s.RenderVersion, renderVersion)
	}
	p := Plan{AlbumID: s.Album.ID, AlbumRevision: s.Album.Revision, RenderVersion: renderVersion}
	if s.Album.Revision <= 0 {
		return Plan{}, errorf(CodeInvalidSnapshot, "album %s has revision %d", s.Album.ID, s.Album.Revision)
	}
	if s.Album.Deleted {
		p.Removal = true
		return p, nil
	}
	if len(s.Tracks) == 0 {
		return Plan{}, errorf(CodeInvalidSnapshot, "album %s is active without tracks (§4.3)", s.Album.ID)
	}
	p.Dir = catalog.AlbumPath(s.Artist.Name, s.Album.Title)
	ns := &namespace{}
	if err := ns.add(entry{path: ReceiptName, name: "the receipt " + ReceiptName}); err != nil {
		return Plan{}, err
	}
	cover, err := planCover(s.Cover, ns)
	if err != nil {
		return Plan{}, err
	}
	p.Cover = cover
	if p.Tracks, p.Copies, err = planTracks(s, cover, ns); err != nil {
		return Plan{}, err
	}
	attachments, err := planAttachments(s.Attachments, ns)
	if err != nil {
		return Plan{}, err
	}
	p.Copies = append(p.Copies, attachments...)
	slices.SortFunc(p.Copies, func(a, b Copy) int { return strings.Compare(a.Path, b.Path) })
	if err := ns.check(); err != nil {
		return Plan{}, err
	}
	return p, nil
}

// planCover places the cover at the root, named by its blob's format.
func planCover(b jobs.SnapshotBlob, ns *namespace) (*Cover, error) {
	if b.Hash == "" {
		return nil, nil
	}
	blob, err := planBlob(b, "the cover")
	if err != nil {
		return nil, err
	}
	ext, ok := coverExtensions[b.Format]
	mime, known := media.CoverMIME(b.Format)
	if !ok || !known {
		return nil, errorf(CodeInvalidSnapshot, "the cover %s has format %q, not JPEG or PNG", b.Hash, b.Format)
	}
	c := &Cover{Path: "cover." + ext, Blob: blob, Format: b.Format, MIME: mime}
	return c, ns.add(entry{path: c.Path, name: "the cover"})
}

// planTracks places every track and its LRC (§5.1) and computes its
// expected tags (§8.2).
func planTracks(s *jobs.RenderSnapshot, cover *Cover, ns *namespace) ([]Track, []Copy, error) {
	tracks := slices.Clone(s.Tracks)
	slices.SortStableFunc(tracks, func(a, b jobs.SnapshotTrack) int {
		if a.Disc != b.Disc {
			return a.Disc - b.Disc
		}
		return a.No - b.No
	})
	multi := multiDisc(tracks)
	discTotal, trackTotals := totals(tracks)
	var out []Track
	var lyrics []Copy
	for _, t := range tracks {
		name := trackName(t)
		if t.Disc < 1 || t.Disc > 99 || t.No < 1 || t.No > 999 {
			return nil, nil, errorf(CodeInvalidSnapshot, "%s is out of the schema's range", name)
		}
		if t.Title == "" || (t.Artist.Valid && t.Artist.String == "") {
			return nil, nil, errorf(CodeInvalidSnapshot, "%s has an empty title or artist override (§4.1)", name)
		}
		blob, err := planBlob(t.Blob, name)
		if err != nil {
			return nil, nil, err
		}
		ext, ok := trackExtensions[t.Blob.Format]
		if !ok {
			return nil, nil, errorf(CodeInvalidSnapshot, "%s has blob format %q, not audio", name, t.Blob.Format)
		}
		dir := ""
		if multi {
			dir = names.Segment(fmt.Sprintf("Disc %d", t.Disc)) + "/"
		}
		// %02d: at least two digits, and no zero beyond them (§5.1).
		file := names.FileSegment(fmt.Sprintf("%02d - %s.%s", t.No, t.Title, ext))
		stem, ok := strings.CutSuffix(file, "."+ext)
		if !ok {
			return nil, nil, errorf(CodeInvalidSnapshot, "the file name of %s lost its extension", name)
		}
		tr := Track{Path: dir + file, Blob: blob, Format: t.Blob.Format,
			Tags: expectedTags(s, t, discTotal, trackTotals[t.Disc])}
		if err := ns.add(entry{path: tr.Path, name: name}); err != nil {
			return nil, nil, err
		}
		out = append(out, tr)
		if t.Lyrics.Hash == "" {
			continue
		}
		lname := "the lyrics of " + name
		lb, err := planBlob(t.Lyrics, lname)
		if err != nil {
			return nil, nil, err
		}
		// The same basename as the track (§5.1). ".lrc" is not longer than
		// the track's extension, so the name stays within the limit.
		lrc := Copy{Path: dir + stem + ".lrc", Blob: lb}
		if err := ns.add(entry{path: lrc.Path, name: lname}); err != nil {
			return nil, nil, err
		}
		lyrics = append(lyrics, lrc)
	}
	return out, lyrics, nil
}

// trackName names a track for the user: disc, number and title.
func trackName(t jobs.SnapshotTrack) string {
	return fmt.Sprintf("track %d.%d %q", t.Disc, t.No, t.Title)
}

// multiDisc is §5.1's rule: more than one disc value, or a value other than
// 1.
func multiDisc(tracks []jobs.SnapshotTrack) bool {
	for _, t := range tracks {
		if t.Disc != 1 {
			return true
		}
	}
	return false
}

// totals are §8.2's: the highest disc number of the album, and for each
// disc its highest track number. Gaps are kept, never filled.
func totals(tracks []jobs.SnapshotTrack) (disc int, perDisc map[int]int) {
	perDisc = map[int]int{}
	for _, t := range tracks {
		disc = max(disc, t.Disc)
		perDisc[t.Disc] = max(perDisc[t.Disc], t.No)
	}
	return disc, perDisc
}

// expectedTags maps the catalog to the managed tags of one track (§8.2,
// §4.1): the track's artist override or the album artist; the track's genre
// when it has one ("" is explicitly none) or the album's; the year as four
// digits (NOTES.md N-131); every absent value empty, which removes it.
func expectedTags(s *jobs.RenderSnapshot, t jobs.SnapshotTrack, discTotal, trackTotal int) media.TagValues {
	artist := s.Artist.Name
	if t.Artist.Valid {
		artist = t.Artist.String
	}
	genre := ""
	switch {
	case t.Genre.Valid:
		genre = t.Genre.String
	case s.Album.Genre.Valid:
		genre = s.Album.Genre.String
	}
	date := ""
	if s.Album.Year > 0 {
		date = fmt.Sprintf("%04d", s.Album.Year)
	}
	return media.TagValues{
		Title: t.Title, Artist: artist, AlbumArtist: s.Artist.Name, Album: s.Album.Title,
		Track: t.No, TrackTotal: trackTotal, Disc: t.Disc, DiscTotal: discTotal,
		Date: date, Genre: genre, Compilation: s.Album.Compilation,
	}
}

// planAttachments places every attachment under Extras/, its rel_path
// sanitized segment by segment with the §5.2 limits (the catalog's own
// rule, names.SanitizeRelFilePath).
func planAttachments(atts []jobs.SnapshotAttachment, ns *namespace) ([]Copy, error) {
	out := make([]Copy, 0, len(atts))
	for _, a := range atts {
		name := fmt.Sprintf("the attachment %q", a.RelPath)
		sp, err := names.SanitizeRelFilePath(a.RelPath)
		if err != nil {
			return nil, &Error{Code: CodePathInvalid, Names: []string{a.RelPath},
				Message: name + " has no valid output path", Err: err}
		}
		blob, err := planBlob(a.Blob, name)
		if err != nil {
			return nil, err
		}
		c := Copy{Path: extrasDir + "/" + sp.Path, Blob: blob}
		if err := ns.add(entry{path: c.Path, name: name, user: a.RelPath}); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// planBlob validates a blob reference of the snapshot.
func planBlob(b jobs.SnapshotBlob, name string) (Blob, error) {
	if err := blobstore.ValidateSHA(b.Hash); err != nil || b.Size < 0 {
		return Blob{}, errorf(CodeInvalidSnapshot, "%s has an invalid blob %q of size %d", name, b.Hash, b.Size)
	}
	return Blob{Hash: b.Hash, Size: b.Size}, nil
}

// entry is one file of the album's namespace.
type entry struct {
	path string
	// name is how the user knows the entry; user, when set, is the value to
	// report in Error.Names (an attachment's rel_path), name otherwise.
	name string
	user string
}

func (e entry) label() string {
	if e.user != "" {
		return e.user
	}
	return e.name
}

// namespace holds the album's files and checks them on their normalized
// keys (§5.2) with catalog.PathCollision, the catalog's own rule.
type namespace struct {
	entries []entry
}

// add records a file, refusing a path that breaks the limits of §5.2
// (checkOutputPath, the rule the receipt applies too).
func (ns *namespace) add(e entry) error {
	if err := checkOutputPath(e.path); err != nil {
		return &Error{Code: CodePathInvalid, Names: []string{e.label()},
			Message: fmt.Sprintf("the output path %q of %s breaks a limit", e.path, e.name), Err: err}
	}
	ns.entries = append(ns.entries, e)
	return nil
}

// check refuses the first collision after normalization (§5.2): two files
// with one key, a file where another needs a directory, or one directory
// spelled two ways (owner decision, N-131). It runs once every file is
// added.
func (ns *namespace) check() error {
	paths := make([]string, len(ns.entries))
	for i, e := range ns.entries {
		paths[i] = e.path
	}
	i, j, kind := catalog.PathCollision(paths)
	if kind == catalog.NoCollision {
		return nil
	}
	a, b := ns.entries[i], ns.entries[j]
	switch kind {
	case catalog.SameFile:
		return collision(a, b, "have the same path after normalization")
	case catalog.FileIsDirectory:
		return collision(a, b, "collide: the first is a file where the second needs a directory")
	case catalog.DirectorySpelling:
		return collision(a, b, "need the same directory after normalization, spelled two ways")
	}
	return nil
}

func collision(a, b entry, what string) *Error {
	return &Error{Code: CodePathCollision, Names: []string{a.label(), b.label()},
		Message: fmt.Sprintf("%s and %s %s (%q, %q): rename one of them", a.name, b.name, what, a.path, b.path)}
}
