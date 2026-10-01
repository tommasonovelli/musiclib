package importer

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/names"
)

// importFile is one file of a candidate after its copy and its reading.
type importFile struct {
	src  *srcFile
	blob blobstore.Blob
	// audio, format, tags and duration are set for a track. duration is
	// what the container declares, 0 when it declares nothing (unknown,
	// NOTES.md N-300).
	audio    bool
	format   string
	tags     media.Inspection
	duration time.Duration
	// disc is the number of the track's disc directory in a multi-disc
	// candidate, 0 in a single-disc one (§7.3).
	disc int
}

// importCandidate is the work of an import job up to the commit (§7.1–§7.6):
// the candidate revalidated against the current disk, every file copied
// through the blob store, everything read again from the verified copies,
// the metadata inferred, the LRC files associated, the cover chosen, the
// fingerprint computed, and the source checked unchanged. It returns the
// closed input of catalog.CommitImport, or the album's error with the
// warnings gathered so far.
func (im *Importer) importCandidate(ctx context.Context, c *jobs.Claim) (_ catalog.ImportCandidate, ws []jobs.Warning, err error) {
	segs, err := names.SplitRelPathOrRoot(c.SourceRel)
	if err != nil {
		return catalog.ImportCandidate{}, nil, err
	}
	r, owned, err := im.src.sub(c.SourceRel)
	if err != nil {
		return catalog.ImportCandidate{}, nil, sourceError(c.SourceRel, err)
	}
	if owned {
		defer func() {
			if cerr := r.Close(); cerr != nil && err == nil {
				err = cerr
			}
		}()
	}

	// §7.2: a retry never trusts the old scan, nor its disc directories.
	tree, discs, err := im.revalidate(ctx, r, len(segs), c.SourceRel)
	if err != nil {
		return catalog.ImportCandidate{}, nil, err
	}
	before := tree.snapshot()
	srcFiles := tree.files()

	// §11.2, then §7.5 for every file. The reservation is held until the
	// job's writes are done: the blobs and the pictures of work/import.
	space, err := im.reserveSpace(estimate(srcFiles))
	if err != nil {
		return catalog.ImportCandidate{}, nil, err
	}
	defer space.Release()
	if err := im.failpoints.Hit("import_copying"); err != nil {
		return catalog.ImportCandidate{}, nil, err
	}
	files := make([]*importFile, len(srcFiles))
	for i, f := range srcFiles {
		b, err := im.copyFile(ctx, r, f)
		if err != nil {
			return catalog.ImportCandidate{}, nil, err
		}
		files[i] = &importFile{src: f, blob: b}
	}
	if err := im.failpoints.Hit("import_copied"); err != nil {
		return catalog.ImportCandidate{}, nil, err
	}

	// §7.2, §7.3, §7.6: everything read again from the verified copies.
	for _, f := range files {
		w, err := im.readFile(ctx, f)
		ws = append(ws, w...)
		if err != nil {
			return catalog.ImportCandidate{}, ws, err
		}
	}
	tracks, others, err := splitTracks(files, discs, c.SourceRel)
	if err != nil {
		return catalog.ImportCandidate{}, ws, err
	}
	cand, more, err := im.buildCandidate(ctx, c, tracks, others)
	ws = append(ws, more...)
	if err != nil {
		return catalog.ImportCandidate{}, ws, err
	}
	if err := im.failpoints.Hit("import_rechecking"); err != nil {
		return catalog.ImportCandidate{}, ws, err
	}

	// §7.1: the source must not have changed during the import.
	if err := im.checkUnchanged(ctx, r, len(segs), c.SourceRel, before); err != nil {
		return catalog.ImportCandidate{}, ws, err
	}
	cand.Warnings = ws
	return cand, ws, nil
}

// revalidate walks the candidate again and applies the scan's rules to it:
// it must still be one valid candidate (§7.2 rules 1 to 4, the limits, no
// rejected entry), single-disc or multi-disc as the disk is now, whatever
// the scan saw. The files carry the scan's view of audio. discs maps the
// path of every disc directory of a multi-disc candidate to its number; it
// is nil for a single-disc one.
func (im *Importer) revalidate(ctx context.Context, r *fsops.Root, depth int, sourceRel string) (*srcDir, map[string]int, error) {
	tree, err := walk(ctx, im.src, r, depth, "")
	if err != nil {
		return nil, nil, sourceError(sourceRel, err)
	}
	if err := im.markAudio(ctx, r, tree); err != nil {
		return nil, nil, err
	}
	g := group(tree, sourceRel)
	for _, br := range g.Branches {
		if br.Dir != tree {
			continue
		}
		if br.Err != nil {
			return nil, nil, br.Err
		}
		var discs map[string]int
		if br.Discs != nil {
			discs = map[string]int{}
			for _, d := range br.Discs {
				discs[d.Dir.Rel] = d.No
			}
		}
		return tree, discs, nil
	}
	msg := fmt.Sprintf("%s has no audio files directly in it, nor in CD<N> or Disc <N> directories", label(sourceRel, ""))
	if len(g.Branches) > 0 {
		msg += fmt.Sprintf("; the albums are in its subdirectories, such as %q", joinRel(sourceRel, g.Branches[0].Dir.Rel))
	}
	return nil, nil, errorf(CodeNotACandidate, "%s", msg)
}

// estimate is the conservative space estimate of §11.2 for a candidate:
// every file becomes a blob (deduplication only lowers it), plus an
// embedded picture extracted and pinned as the cover (at most
// MaxCoverBytes each).
func estimate(files []*srcFile) int64 {
	n := int64(2 * MaxCoverBytes)
	for _, f := range files {
		n += f.ID.Size
	}
	return n
}

// copyFile copies one source file into the blob store (§7.5). The opened
// file must be the one the walk described: same identity, size and mtime
// (§7.1); so must the bytes copied.
func (im *Importer) copyFile(ctx context.Context, r *fsops.Root, f *srcFile) (blobstore.Blob, error) {
	in, err := im.src.open(r, f.Rel)
	if err != nil {
		if code := fsops.Code(err); code == fsops.CodeNotFound || code == fsops.CodeSymlink || code == fsops.CodeSpecialFile || code == fsops.CodeIsDirectory {
			return blobstore.Blob{}, changed(f.Rel, err)
		}
		return blobstore.Blob{}, err
	}
	fi, err := fsops.Describe(in)
	if err == nil && identityOf(fi) != f.ID {
		err = changed(f.Rel, nil)
	}
	var b blobstore.Blob
	if err == nil {
		b, err = im.blobs.Put(ctx, in)
	}
	err = errors.Join(err, closeErr(in, fmt.Sprintf("%q", f.Rel)))
	if err != nil {
		return blobstore.Blob{}, err
	}
	if b.Size != f.ID.Size {
		return blobstore.Blob{}, changed(f.Rel, nil)
	}
	return b, nil
}

func changed(rel string, err error) *Error {
	return &Error{Code: CodeSourceChanged, Path: rel, Err: err,
		Message: fmt.Sprintf("%q changed during the import: leave the source unchanged until the import completes, then retry", rel)}
}

// checkUnchanged walks the candidate again and compares every directory and
// file with the walk at the start: an addition, a removal, or another
// identity, size or mtime refuses the import (§7.1).
func (im *Importer) checkUnchanged(ctx context.Context, r *fsops.Root, depth int, sourceRel string, before map[string]identity) error {
	tree, err := walk(ctx, im.src, r, depth, "")
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return changed(sourceRel, err)
	}
	if p, diff := compareSnapshots(before, tree.snapshot()); diff {
		return changed(joinRel(sourceRel, p), nil)
	}
	return nil
}

// readFile reads one verified copy of a candidate with readAudio and, for
// a track, keeps what it read.
func (im *Importer) readFile(ctx context.Context, f *importFile) ([]jobs.Warning, error) {
	a, audio, ws, err := im.readAudio(ctx, f.blob.SHA256, f.src.Rel)
	if err == nil && audio {
		f.audio, f.format, f.tags, f.duration = true, a.format, a.tags, a.duration
	}
	return ws, err
}

// audioRead is what readAudio reads from a track: its format, its tags and
// the duration its container declares (0 when it declares none).
type audioRead struct {
	format   string
	tags     media.Inspection
	duration time.Duration
}

// readAudio classifies one verified copy, the blob hash, by its content
// (§7.2) and, for a track, decodes it completely (§7.6) and reads its tags
// (§7.3). rel names the file in errors and warnings. It is the one check
// of a track, for an import (readFile) and for an upload (ReadTrack):
//   - supported audio is a track: FLAC, MP3 and M4A (AAC or ALAC), which
//     may share a candidate (NOTES.md N-157);
//   - an M4A the tag reader does not handle (fragmented, encrypted, more
//     than one track: N-165) is CodeUnsupportedAudio, like audio the probe
//     refuses;
//   - audio that is not supported is CodeUnsupportedAudio;
//   - no audio or unreadable, with a known audio extension, is
//     CodeCorruptAudio; without one, audio is false and the error nil (an
//     attachment of an import);
//   - a track that does not decode completely is CodeCorruptAudio;
//   - a field the tag writer cannot save back is CodeUnrenderableTag
//     (N-092), except ID3 tags in a FLAC, which the output drops (N-090):
//     accepted with a warning.
func (im *Importer) readAudio(ctx context.Context, hash, rel string) (_ audioRead, audio bool, _ []jobs.Warning, err error) {
	bf, err := im.blobs.Open(hash)
	if err != nil {
		return audioRead{}, false, nil, err
	}
	defer func() { err = errors.Join(err, closeErr(bf, "blob "+hash)) }()
	p, err := im.tools.Probe(ctx, bf)
	if err != nil {
		return audioRead{}, false, nil, err
	}
	switch p.Class {
	case media.ClassAudio:
	case media.ClassUnsupportedAudio:
		return audioRead{}, false, nil, &Error{Code: CodeUnsupportedAudio, Path: rel,
			Message: fmt.Sprintf("%q is audio that is not supported (%s: %s)", rel, p.Reason, p.Detail)}
	default:
		if media.HasKnownAudioExtension(rel) {
			return audioRead{}, false, nil, &Error{Code: CodeCorruptAudio, Path: rel,
				Message: fmt.Sprintf("%q has an audio extension but no readable audio", rel)}
		}
		return audioRead{}, false, nil, nil // an attachment
	}
	if _, err := im.tools.AudioDigest(ctx, bf); err != nil {
		return audioRead{}, false, nil, corrupt(rel, err, decodeFailures...)
	}
	in, err := im.tools.Inspect(ctx, bf, p.Format)
	var me *media.Error
	if media.Code(err) == media.CodeTagsUnsupported && errors.As(err, &me) {
		return audioRead{}, false, nil, &Error{Code: CodeUnsupportedAudio, Path: rel, Err: err,
			Message: fmt.Sprintf("%q is audio that is not supported: %s", rel, me.Msg)}
	}
	if err != nil {
		return audioRead{}, false, nil, corrupt(rel, err, readerFailures...)
	}
	ws, err := tagWarnings(rel, in)
	if p.Format == media.FormatMP3 {
		ws = append(ws, genreWarnings(rel, in.Managed.Genre)...)
	}
	return audioRead{format: p.Format, tags: in, duration: p.Audio.Duration}, true, ws, err
}

// corruptMessages are the messages of the failures of the decode and of the
// tag reader that are the file's fault. They say which check failed: such a
// file may well play (N-128).
var corruptMessages = map[string]string{
	media.CodeDecode: "%q does not decode completely: an audio frame is damaged or missing, " +
		"or the file has bytes after its last frame that are not audio (an ID3v2 tag appended at the end is one)",
	media.CodeNotSupported:       "%q is not supported audio",
	media.CodeTagsCorrupt:        "%q has a damaged metadata structure",
	media.CodeTagsFormatMismatch: "%q is not an audio stream of its format that the tag reader accepts",
}

// decodeFailures and readerFailures are the codes of the decode and of the
// tag reader that are the file's fault; each one has its message in
// corruptMessages (TestCorruptMessages).
var (
	decodeFailures = []string{media.CodeDecode, media.CodeNotSupported}
	readerFailures = []string{media.CodeTagsCorrupt, media.CodeTagsFormatMismatch}
)

// corrupt types a failure of the decode or of the tag reader: the file's
// fault (codes, each one of corruptMessages) is CodeCorruptAudio; anything
// else (a timeout, a tool failure) keeps its own code.
func corrupt(rel string, err error, codes ...string) error {
	code := media.Code(err)
	if !slices.Contains(codes, code) {
		return err
	}
	msg, ok := corruptMessages[code]
	if !ok {
		// A caller listed a code without a message: say which check failed
		// rather than format a missing entry.
		msg = "%q failed the check " + code
	}
	return &Error{Code: CodeCorruptAudio, Path: rel, Message: fmt.Sprintf(msg, rel), Err: err}
}

// foreignTag is the opaque reason of an ID3v2 or ID3v1 tag inside a FLAC
// (the helper's kForeignTag). The helper reports it as removed: a render
// strips it (N-090).
const foreignTag = "foreign_tag"

// tagWarnings applies N-092 and N-090 to an inspection and reports the
// conflicts of managed fields (§8.1):
//   - an ID3 tag in a FLAC, which a render strips, is a warning;
//   - any other field a write cannot keep (Inspection.Blocking) refuses the
//     file. An ID3 tag the helper did not declare removed would be one.
func tagWarnings(rel string, in media.Inspection) ([]jobs.Warning, error) {
	var ws []jobs.Warning
	for _, o := range in.Opaque {
		if o.Reason == foreignTag && o.Removed {
			ws = append(ws, jobs.Warning{Code: jobs.WarnFLACID3, Path: rel,
				Message: fmt.Sprintf("%q carries an %s tag, which the library copy will not carry: the Vorbis comments are the tags, and ID3 frames without a Vorbis equivalent are dropped", rel, o.Key)})
		}
	}
	if b := in.Blocking(); len(b) > 0 {
		o := b[0]
		return ws, &Error{Code: CodeUnrenderableTag, Path: rel,
			Message: fmt.Sprintf("%q: the field %q cannot be written back without loss (%s): fix it in the source and retry", rel, o.Key, o.Reason)}
	}
	for _, c := range in.Conflicts {
		keys := make([]string, len(c.Sources))
		for i, s := range c.Sources {
			keys[i] = s.Key
		}
		ws = append(ws, jobs.Warning{Code: jobs.WarnTagConflict, Path: rel,
			Message: fmt.Sprintf("%q: the %s field has disagreeing sources (%s); %s is used", rel, c.Field, strings.Join(keys, ", "), keys[0])})
	}
	return ws, nil
}

// splitTracks separates the tracks from the other files and checks, by
// content, the rules the scan checked by its view (§7.2): the tracks of a
// single-disc candidate (discs nil) are directly in it (rule 4); those of a
// multi-disc one are directly in one of its disc directories (rules 2 and
// 4, N-184), whose number becomes their disc (§7.3). There is at least one
// track, and at most MaxTracks over all discs. Tracks are sorted by
// pathNaturalCompare, the others by path.
func splitTracks(files []*importFile, discs map[string]int, sourceRel string) (tracks, others []*importFile, err error) {
	for _, f := range files {
		if !f.audio {
			others = append(others, f)
			continue
		}
		dir := path.Dir(f.src.Rel)
		switch no, ok := discs[dir]; {
		case discs == nil && dir == ".":
		case ok:
			f.disc = no
		case discs == nil:
			return nil, nil, &Error{Code: CodeAmbiguousCandidate, Path: f.src.Rel,
				Message: fmt.Sprintf("%s has audio files and more audio below it (%q): import its subdirectories separately",
					label(sourceRel, ""), joinRel(sourceRel, f.src.Rel))}
		default:
			return nil, nil, &Error{Code: CodeAmbiguousCandidate, Path: f.src.Rel,
				Message: fmt.Sprintf("%s is a multi-disc album, but %q is audio outside its disc directories: move it into one of them",
					label(sourceRel, ""), joinRel(sourceRel, f.src.Rel))}
		}
		tracks = append(tracks, f)
	}
	switch {
	case len(tracks) == 0:
		return nil, nil, errorf(CodeNotACandidate, "%s has no audio file that reads as audio", label(sourceRel, ""))
	case len(tracks) > MaxTracks:
		return nil, nil, errorf(catalog.CodeTooManyFiles, "%s has %d tracks, the maximum is %d", label(sourceRel, ""), len(tracks), MaxTracks)
	}
	slices.SortFunc(tracks, func(a, b *importFile) int { return pathNaturalCompare(a.src.Rel, b.src.Rel) })
	return tracks, others, nil
}

// buildCandidate infers the metadata (§7.3), associates the LRC files and
// chooses the cover (§7.4), and assembles the closed input of the commit
// with its fingerprint (§7.6).
func (im *Importer) buildCandidate(ctx context.Context, c *jobs.Claim, tracks, others []*importFile) (catalog.ImportCandidate, []jobs.Warning, error) {
	tags := make([]trackTags, len(tracks))
	trackPaths := make([]string, len(tracks))
	for i, t := range tracks {
		tags[i] = trackTags{Path: t.src.Rel, Disc: t.disc, Tags: t.tags.Managed}
		trackPaths[i] = t.src.Rel
	}
	meta, err := inferMetadata(dirName(c.SourceRel), tags, c.Overrides)
	if err != nil {
		return catalog.ImportCandidate{}, nil, err
	}
	ws := meta.Warnings

	otherPaths := make([]string, len(others))
	byPath := map[string]*importFile{}
	for i, f := range others {
		otherPaths[i] = f.src.Rel
		byPath[f.src.Rel] = f
	}
	lyrics, err := associateLyrics(trackPaths, otherPaths)
	if err != nil {
		return catalog.ImportCandidate{}, ws, err
	}
	for _, t := range trackPaths {
		lrc, ok := lyrics[t]
		if !ok {
			continue
		}
		valid, err := im.blobIsUTF8(ctx, byPath[lrc].blob)
		if err != nil {
			return catalog.ImportCandidate{}, ws, err
		}
		if !valid {
			delete(lyrics, t)
			ws = append(ws, jobs.Warning{Code: jobs.WarnLyricsNotUTF8, Path: lrc,
				Message: fmt.Sprintf("%q is not UTF-8 text: it is kept as an attachment, not as the lyrics of %q", lrc, t)})
		}
	}

	// The tracks in their final order, for the embedded pictures (§7.4).
	order := make([]int, len(tracks))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		return cmp.Or(cmp.Compare(meta.Tracks[a].Disc, meta.Tracks[b].Disc), cmp.Compare(meta.Tracks[a].No, meta.Tracks[b].No))
	})
	cts := make([]coverTrack, len(tracks))
	for i, k := range order {
		t := tracks[k]
		cts[i] = coverTrack{Path: t.src.Rel, Blob: t.blob, Format: t.format, Pictures: t.tags.Pictures}
	}
	var cfs []coverFile
	for _, f := range others {
		cfs = append(cfs, coverFile{Path: f.src.Rel, Blob: f.blob})
	}
	cover, err := im.chooseCover(ctx, cfs, cts)
	ws = append(ws, cover.Warnings...)
	if err != nil {
		return catalog.ImportCandidate{}, ws, err
	}

	cand, err := assemble(c, meta, tracks, others, lyrics, cover.Cover)
	return cand, ws, err
}

// dirName is the name of the candidate's directory, "" for /import itself:
// the fallback album title of §7.3. For a multi-disc candidate it is the
// parent of the disc directories, never a disc directory (N-183).
func dirName(sourceRel string) string {
	if sourceRel == "" {
		return ""
	}
	return lastSegment(sourceRel)
}

// blobIsUTF8 reads a blob and reports whether it is valid UTF-8.
func (im *Importer) blobIsUTF8(ctx context.Context, b blobstore.Blob) (bool, error) {
	f, err := im.blobs.Open(b.SHA256)
	if err != nil {
		return false, err
	}
	ok, err := validUTF8(ctx, f)
	return ok, errors.Join(err, closeErr(f, "blob "+b.SHA256))
}

// assemble builds the closed input of catalog.CommitImport: every blob once
// with its content-derived format (audio from the probe, the cover from its
// decode, anything else unknown: N-118) and, for a track, the duration its
// probe read (N-300), the tracks by disc and number, the
// attachments by path, and the fingerprint of every file (§7.6).
func assemble(c *jobs.Claim, meta albumMeta, tracks, others []*importFile, lyrics map[string]string, cover *catalog.Blob) (catalog.ImportCandidate, error) {
	cand := catalog.ImportCandidate{
		Attempt: c.Attempt, Artist: meta.Artist, Title: meta.Title, Year: meta.Year, Genre: meta.Genre,
		Compilation: meta.Compilation,
	}
	blobs := map[string]catalog.Blob{}
	add := func(b blobstore.Blob, format string) {
		if cur, ok := blobs[b.SHA256]; !ok || cur.Format == "" {
			blobs[b.SHA256] = catalog.Blob{Hash: b.SHA256, Size: b.Size, Format: format}
		}
	}
	var entries []fingerprintEntry
	lrcFiles := map[string]*importFile{}
	for _, f := range others {
		entries = append(entries, fingerprintEntry{Path: f.src.Rel, Size: f.blob.Size, Hash: f.blob.SHA256})
		lrcFiles[f.src.Rel] = f
	}
	associated := map[string]bool{}
	for i, t := range tracks {
		entries = append(entries, fingerprintEntry{Path: t.src.Rel, Size: t.blob.Size, Hash: t.blob.SHA256})
		add(t.blob, t.format)
		if ms, ok := media.DurationMS(t.duration); ok {
			b := blobs[t.blob.SHA256]
			b.DurationMS = &ms
			blobs[t.blob.SHA256] = b
		}
		m := meta.Tracks[i]
		it := catalog.ImportTrack{SourcePath: m.Path, Disc: m.Disc, No: m.No, Title: m.Title,
			Artist: m.Artist, Genre: m.Genre, BlobHash: t.blob.SHA256}
		if lrc, ok := lyrics[t.src.Rel]; ok {
			associated[lrc] = true
			it.Lyrics = &catalog.ImportLyrics{SourcePath: lrc, BlobHash: lrcFiles[lrc].blob.SHA256}
			add(lrcFiles[lrc].blob, "")
		}
		cand.Tracks = append(cand.Tracks, it)
	}
	slices.SortFunc(cand.Tracks, func(a, b catalog.ImportTrack) int {
		return cmp.Or(cmp.Compare(a.Disc, b.Disc), cmp.Compare(a.No, b.No))
	})
	for _, f := range others {
		if associated[f.src.Rel] {
			continue
		}
		cand.Attachments = append(cand.Attachments, catalog.ImportAttachment{RelPath: f.src.Rel, BlobHash: f.blob.SHA256})
		add(f.blob, "")
	}
	if cover != nil {
		cand.CoverHash = &cover.Hash
		blobs[cover.Hash] = *cover
	}
	for _, b := range blobs {
		cand.Blobs = append(cand.Blobs, b)
	}
	slices.SortFunc(cand.Blobs, func(a, b catalog.Blob) int { return strings.Compare(a.Hash, b.Hash) })
	fp, err := fingerprint(entries)
	if err != nil {
		return catalog.ImportCandidate{}, err
	}
	cand.Fingerprint = fp
	return cand, nil
}

// genreWarnings reports each genre read from an MP3 that an MP3 cannot hold
// as it is (media.MP3GenreWritable): "(Rock)", "13". It is imported as read,
// never rewritten; the render refuses it and the editor refuses it for an
// album with an MP3 track, so it must be corrected there (owner decision
// N-162).
func genreWarnings(rel string, genres []string) []jobs.Warning {
	var ws []jobs.Warning
	for _, g := range genres {
		if !media.MP3GenreWritable(g) {
			ws = append(ws, jobs.Warning{Code: jobs.WarnGenreNotWritable, Path: rel,
				Message: fmt.Sprintf("%q has the genre %q, which an MP3 reads back as an ID3v1 genre reference: correct it in the editor before the album renders", rel, g)})
		}
	}
	return ws
}
