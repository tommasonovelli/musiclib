package catalog

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

// Limits of one import candidate (§7.2) and of a cover (§8.5).
const (
	MaxTracks     = 1000
	MaxFiles      = 10000
	MaxCoverBytes = 20 << 20
)

// The values of blobs.format (§4.2). They come from the content, never
// from an extension; "" is any other content (NULL).
const (
	FormatFLAC    = "flac"
	FormatMP3     = "mp3"
	FormatM4AAAC  = "m4a-aac"
	FormatM4AALAC = "m4a-alac"
	FormatJPEG    = "jpeg"
	FormatPNG     = "png"
)

var (
	audioFormats = map[string]bool{FormatFLAC: true, FormatMP3: true, FormatM4AAAC: true, FormatM4AALAC: true}
	coverFormats = map[string]bool{FormatJPEG: true, FormatPNG: true}
	knownFormats = map[string]bool{"": true, FormatFLAC: true, FormatMP3: true, FormatM4AAAC: true,
		FormatM4AALAC: true, FormatJPEG: true, FormatPNG: true}
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// CodeDuplicateImport is the reason of a skipped import: the same
// fingerprint is already an album (§7.6).
const CodeDuplicateImport = "duplicate_import"

// ImportCandidate is what the importer hands to the import commit (§7.6):
// one album, fully read, every blob already durable in originals/ (§7.5).
// Every source path is relative to the candidate's root, exactly as on disk
// (§5.2); the overrides of §7.3 are already applied to Artist and Title.
//
// The commit validates it against the domain rules and never inspects
// media: the importer has already read, verified and chosen everything
// (§7.3, §7.4, §7.6), including the owner's decisions N-091 (the cover must
// be embeddable in every audio format of the album, asked here through the
// service's CoverFits) and N-092 (a file with invalid UTF-8 in an unmanaged
// field is refused by the importer, before the commit).
type ImportCandidate struct {
	// Attempt is the running import job's attempt (jobs.Claim).
	Attempt jobs.Attempt
	// Fingerprint is §7.6's SHA-256 of the candidate's file list.
	Fingerprint string
	// Blobs lists every blob referenced below, once each.
	Blobs       []Blob
	Artist      string
	Title       string
	Year        *int
	Genre       *string
	Compilation bool
	// CoverHash is the chosen cover (§7.4), a JPEG or PNG blob; nil for
	// none.
	CoverHash   *string
	Tracks      []ImportTrack
	Attachments []ImportAttachment
	// Warnings are stored with the job's outcome (§7.2, §7.3).
	Warnings []jobs.Warning
}

// Blob is a pinned blob with its content-derived format ("" for any other
// content).
type Blob struct {
	Hash   string
	Size   int64
	Format string
}

// ImportTrack is one track of a candidate. Artist nil inherits the album
// artist; Genre nil inherits the album genre, "" is explicitly none.
type ImportTrack struct {
	SourcePath string
	Disc       int
	No         int
	Title      string
	Artist     *string
	Genre      *string
	BlobHash   string
	// Lyrics is the associated LRC file (§7.4), or nil.
	Lyrics *ImportLyrics
}

// ImportLyrics is an LRC file associated with a track (§7.4).
type ImportLyrics struct {
	SourcePath string
	BlobHash   string
}

// ImportAttachment is a file kept under Extras/ (§5.1, §7.4). RelPath is
// the original relative path; the output sanitizes it per segment.
type ImportAttachment struct {
	RelPath  string
	BlobHash string
}

// ImportOutcome is the durable outcome of an import job.
type ImportOutcome struct {
	// State is done, skipped (an identical import exists, §7.6) or failed
	// (the candidate breaks a domain rule, or its name is taken).
	State jobs.State
	// AlbumID is the album created (done) or the identical one (skipped).
	AlbumID      uuid.UUID
	ErrorCode    string
	ErrorMessage string
	// AlreadyCompleted: the job already had its outcome when the commit
	// ran (a retry, or a commit whose acknowledgement was lost), and
	// nothing was written. The fields above are that outcome.
	AlreadyCompleted bool
}

// CommitImport is the import commit of §7.6: one transaction under the
// catalog lock that
//
//  1. rechecks that the job is not already completed: if it is, it returns
//     that outcome and writes nothing (AlreadyCompleted), so a retry or a
//     lost commit acknowledgement never makes a second album (§7.6, §12.2);
//     an attempt that is no longer the running one is
//     jobs.CodeAttemptStale;
//  2. registers the blobs and resolves or creates the artist;
//  3. creates the album, its tracks and attachments at revision 1;
//  4. reserves its path and enqueues its render;
//  5. writes result_album_id and marks the job done.
//
// An identical import (same fingerprint, the album in the trash or not)
// makes the job skipped with a reference to that album. A candidate that
// breaks a domain rule, a folder taken by another album of the artist, a
// path reserved by another album, or an artist name that collides with
// another only through sanitization make it failed, with the error's code
// and message; nothing else is written. In all these cases the outcome is
// committed and the error is nil. An error means that nothing was
// committed, except for store.CodeCommitUncertain; store.IsFatal tells the
// cases of §6.4.
func (s *Service) CommitImport(ctx context.Context, c ImportCandidate) (ImportOutcome, error) {
	plan, invalid := validateCandidate(c, s.coverFits)
	var out ImportOutcome
	err := store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		var err error
		out, err = commitImport(ctx, tx, c, plan, invalid)
		return err
	})
	if err != nil {
		return ImportOutcome{}, err
	}
	if out.State == jobs.StateDone && !out.AlreadyCompleted {
		s.notify()
	}
	return out, nil
}

// commitImport is CommitImport's transaction. invalid is the candidate's
// validation error, plan its validated form when invalid is nil.
func commitImport(ctx context.Context, tx *store.CatalogTx, c ImportCandidate, plan *importPlan, invalid error) (ImportOutcome, error) {
	st, err := jobs.LockStatus(ctx, tx, c.Attempt.JobID)
	if err != nil {
		return ImportOutcome{}, err
	}
	if st.Kind != jobs.KindImport {
		return ImportOutcome{}, &jobs.Error{Code: jobs.CodeAttemptStale, Msg: fmt.Sprintf("job %s is a %s job", st.ID, st.Kind)}
	}
	if st.State.Terminal() {
		return ImportOutcome{State: st.State, AlbumID: st.AlbumID, ErrorCode: st.ErrorCode,
			ErrorMessage: st.ErrorMessage, AlreadyCompleted: true}, nil
	}
	if !st.Runs(c.Attempt) {
		return ImportOutcome{}, &jobs.Error{Code: jobs.CodeAttemptStale,
			Msg: fmt.Sprintf("%s is not the running attempt (state %s, claimed %d)", c.Attempt, st.State, st.Claimed)}
	}
	if sha256Pattern.MatchString(c.Fingerprint) {
		dup, err := tx.GetAlbumByFingerprint(ctx, &c.Fingerprint)
		switch {
		case err == nil:
			msg := fmt.Sprintf("an identical import is album %s", dup.ID)
			if dup.DeletedAt != nil {
				msg += ", in the trash: restore it instead"
			}
			return finish(ctx, tx, c, jobs.Result{State: jobs.StateSkipped, AlbumID: dup.ID,
				ErrorCode: CodeDuplicateImport, ErrorMessage: msg})
		case !errors.Is(err, pgx.ErrNoRows):
			return ImportOutcome{}, dbErr("looking up the import fingerprint", err)
		}
	}
	if invalid == nil {
		invalid = checkImport(ctx, tx, plan)
	}
	if invalid != nil {
		e, ok := rejection(invalid)
		if !ok {
			return ImportOutcome{}, invalid
		}
		return finish(ctx, tx, c, jobs.Result{State: jobs.StateFailed, ErrorCode: e.Code, ErrorMessage: e.text()})
	}
	if err := writeImport(ctx, tx, plan); err != nil {
		return ImportOutcome{}, err
	}
	return finish(ctx, tx, c, jobs.Result{State: jobs.StateDone, AlbumID: plan.albumID, Warnings: c.Warnings})
}

// finish records the job's outcome in the commit's transaction.
func finish(ctx context.Context, tx *store.CatalogTx, c ImportCandidate, r jobs.Result) (ImportOutcome, error) {
	if err := jobs.Finish(ctx, tx, jobs.KindImport, c.Attempt, r); err != nil {
		return ImportOutcome{}, err
	}
	return ImportOutcome{State: r.State, AlbumID: r.AlbumID, ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage}, nil
}

// rejection returns the domain error in err, if err is one: a *Error whose
// code is not CodeDB. It fails the import instead of the transaction.
func rejection(err error) (*Error, bool) {
	e, ok := AsError(err)
	if !ok || e.Code == CodeDB {
		return nil, false
	}
	return e, true
}

// text is the error without its code, for jobs.error_message.
func (e *Error) text() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

// importPlan is a validated candidate, normalized and ready to insert. It
// is built once, outside the transaction, and only read inside it, so a
// retried transaction sees the same plan.
type importPlan struct {
	albumID     uuid.UUID
	fingerprint string
	blobs       []Blob
	artist      string
	album       albumFields
	path        Path
	cover       *string
	tracks      []store.InsertTracksParams
	attachments []store.InsertAttachmentsParams
	// roles is the use of each blob, checked again against the formats
	// already recorded in the database.
	roles map[string]blobRole
}

type blobRole int

const (
	roleAttachment blobRole = 1 << iota
	roleAudio
	roleCover
	roleLyrics
)

// checkImport runs, under the lock and before any write, the checks that
// need the database: the blobs already recorded, the artist, the folder and
// the path reservation. A domain rejection is a *Error (see rejection);
// anything else is a database failure.
func checkImport(ctx context.Context, tx *store.CatalogTx, plan *importPlan) error {
	if err := checkBlobs(ctx, tx, plan); err != nil {
		return err
	}
	artist, err := resolveArtist(ctx, tx, plan.artist)
	if err != nil {
		return err
	}
	if artist.Exists {
		if err := checkFolderFree(ctx, tx, artist.ID, plan.album.FolderKey, plan.albumID); err != nil {
			return err
		}
	}
	return checkAvailable(ctx, tx, plan.albumID, []Path{plan.path})
}

// checkBlobs compares the candidate's blobs with the rows already in
// blobs: the same hash with another size is corruption or a bug
// (CodeBlobMismatch); a format known on both sides must agree
// (CodeInvalidBlobFormat). A format known on one side only is the
// content's format, and each blob's roles are checked against it.
func checkBlobs(ctx context.Context, tx *store.CatalogTx, plan *importPlan) error {
	rows, err := tx.GetBlobs(ctx, blobHashes(plan.blobs))
	if err != nil {
		return dbErr("reading blobs", err)
	}
	known := make(map[string]store.GetBlobsRow, len(rows))
	for _, r := range rows {
		known[r.Hash] = r
	}
	for _, b := range plan.blobs {
		r, ok := known[b.Hash]
		if !ok {
			continue
		}
		if r.Size != b.Size {
			return errorf(CodeBlobMismatch, "blob %s is recorded with %d bytes, the candidate has %d", b.Hash, r.Size, b.Size)
		}
		format := deref(r.Format)
		if format != "" && b.Format != "" && format != b.Format {
			return errorf(CodeInvalidBlobFormat, "blob %s is recorded as %q, the candidate says %q", b.Hash, format, b.Format)
		}
		if format == "" {
			format = b.Format
		}
		if err := checkRole(b.Hash, format, plan.roles[b.Hash]); err != nil {
			return err
		}
	}
	return nil
}

// writeImport writes the validated and checked candidate: blobs, artist,
// album, tracks, attachments, then the claims and the render (§7.6 steps
// 2-4).
func writeImport(ctx context.Context, tx *store.CatalogTx, plan *importPlan) error {
	if err := registerBlobs(ctx, tx, plan.blobs); err != nil {
		return err
	}
	artist, err := resolveArtist(ctx, tx, plan.artist)
	if err != nil {
		return err
	}
	if !artist.Exists {
		if err := tx.InsertArtist(ctx, store.InsertArtistParams{ID: artist.ID, Name: artist.Name, FolderKey: artist.FolderKey}); err != nil {
			return dbErr("creating artist "+artist.Name, err)
		}
	}
	err = tx.InsertAlbum(ctx, store.InsertAlbumParams{
		ID: plan.albumID, ArtistID: artist.ID, Title: plan.album.Title, FolderKey: plan.album.FolderKey,
		Year: plan.album.Year, Genre: plan.album.Genre, Compilation: plan.album.Compilation,
		CoverHash: plan.cover, ImportFingerprint: &plan.fingerprint,
	})
	if err != nil {
		return dbErr("creating the album", err)
	}
	if _, err := tx.InsertTracks(ctx, plan.tracks); err != nil {
		return dbErr("creating the tracks", err)
	}
	if len(plan.attachments) > 0 {
		if _, err := tx.InsertAttachments(ctx, plan.attachments); err != nil {
			return dbErr("creating the attachments", err)
		}
	}
	_, err = outputChanged(ctx, tx, plan.albumID, true)
	return err
}

// registerBlobs records the candidate's blobs (§7.6 step 2). Existing rows
// are kept; one whose format was unknown gets the content's format.
func registerBlobs(ctx context.Context, tx *store.CatalogTx, blobs []Blob) error {
	p := store.InsertBlobsParams{
		Hashes: make([]string, len(blobs)), Sizes: make([]int64, len(blobs)), Formats: make([]string, len(blobs)),
	}
	for i, b := range blobs {
		p.Hashes[i], p.Sizes[i], p.Formats[i] = b.Hash, b.Size, b.Format
	}
	if err := tx.InsertBlobs(ctx, p); err != nil {
		return dbErr("registering blobs", err)
	}
	for _, b := range blobs {
		if b.Format == "" {
			continue
		}
		if _, err := tx.SetBlobFormat(ctx, store.SetBlobFormatParams{Hash: b.Hash, Format: &b.Format}); err != nil {
			return dbErr("recording the format of blob "+b.Hash, err)
		}
	}
	return nil
}

func blobHashes(blobs []Blob) []string {
	hs := make([]string, len(blobs))
	for i, b := range blobs {
		hs[i] = b.Hash
	}
	return hs
}

// checkRole checks a blob's format against its uses (§4.2: audio blobs for
// tracks, JPEG or PNG for covers; §7.4: an LRC file is text, neither audio
// nor image). An attachment may be anything.
func checkRole(hash, format string, role blobRole) error {
	switch {
	case role&roleAudio != 0 && !audioFormats[format]:
		return errorf(CodeInvalidBlobFormat, "blob %s is a track but its format is %q, not audio", hash, format)
	case role&roleCover != 0 && !coverFormats[format]:
		return errorf(CodeInvalidCover, "blob %s is the cover but its format is %q, not JPEG or PNG", hash, format)
	case role&roleLyrics != 0 && format != "":
		return errorf(CodeInvalidBlobFormat, "blob %s is an LRC file but its content is %q", hash, format)
	}
	return nil
}

// validateCandidate checks everything that needs no database. The error is
// always a *Error: a domain rejection that fails the job.
func validateCandidate(c ImportCandidate, coverFits CoverFits) (*importPlan, error) {
	if !sha256Pattern.MatchString(c.Fingerprint) {
		return nil, errorf(CodeInvalidFingerprint, "the fingerprint %q is not a SHA-256", c.Fingerprint)
	}
	plan := &importPlan{albumID: store.NewID(), fingerprint: c.Fingerprint, roles: map[string]blobRole{}}
	var err error
	if plan.artist, err = names.NormalizeRequiredText(c.Artist); err != nil {
		return nil, textError("album artist", err)
	}
	if plan.album, err = normalizeAlbum(c.Title, c.Year, c.Genre, c.Compilation); err != nil {
		return nil, err
	}
	plan.path = AlbumPath(plan.artist, plan.album.Title)
	if _, err := jobs.EncodeWarnings(c.Warnings); err != nil {
		return nil, &Error{Code: CodeInvalidImportWarnings, Message: "the import warnings", Err: err}
	}
	blobs, err := indexBlobs(c.Blobs)
	if err != nil {
		return nil, err
	}
	use := func(hash string, role blobRole) error {
		b, ok := blobs[hash]
		if !ok {
			return errorf(CodeInvalidBlob, "blob %q is used but not listed", hash)
		}
		plan.roles[hash] |= role
		return checkRole(hash, b.Format, role)
	}
	if c.CoverHash != nil {
		if err := use(*c.CoverHash, roleCover); err != nil {
			return nil, err
		}
		if size := blobs[*c.CoverHash].Size; size > MaxCoverBytes {
			return nil, errorf(CodeInvalidCover, "the cover takes %d bytes, the maximum is %d", size, MaxCoverBytes)
		}
		plan.cover = c.CoverHash
	}
	if err := validateFiles(c, plan, use); err != nil {
		return nil, err
	}
	if c.CoverHash != nil {
		if err := checkCoverFits(blobs[*c.CoverHash], c.Tracks, blobs, coverFits); err != nil {
			return nil, err
		}
	}
	for _, b := range c.Blobs {
		if plan.roles[b.Hash] == 0 {
			return nil, errorf(CodeInvalidBlob, "blob %s is listed but not used", b.Hash)
		}
	}
	plan.blobs = slices.Clone(c.Blobs)
	slices.SortFunc(plan.blobs, func(a, b Blob) int { return strings.Compare(a.Hash, b.Hash) })
	return plan, nil
}

// checkCoverFits asks coverFits (N-091) whether the cover can be embedded
// in every audio format of the album's tracks, in a fixed order.
func checkCoverFits(cover Blob, tracks []ImportTrack, blobs map[string]Blob, coverFits CoverFits) error {
	var formats []string
	for _, t := range tracks {
		if f := blobs[t.BlobHash].Format; !slices.Contains(formats, f) {
			formats = append(formats, f)
		}
	}
	slices.Sort(formats)
	for _, f := range formats {
		if err := coverFits(cover, f); err != nil {
			return &Error{Code: CodeInvalidCover,
				Message: fmt.Sprintf("the cover %s cannot be embedded in %s files", cover.Hash, f), Err: err}
		}
	}
	return nil
}

// indexBlobs validates the candidate's blob list.
func indexBlobs(list []Blob) (map[string]Blob, error) {
	blobs := make(map[string]Blob, len(list))
	for _, b := range list {
		switch {
		case !sha256Pattern.MatchString(b.Hash):
			return nil, errorf(CodeInvalidBlob, "%q is not a SHA-256", b.Hash)
		case b.Size < 0:
			return nil, errorf(CodeInvalidBlob, "blob %s has a negative size", b.Hash)
		case !knownFormats[b.Format]:
			return nil, errorf(CodeInvalidBlobFormat, "blob %s has the unknown format %q", b.Hash, b.Format)
		}
		if _, dup := blobs[b.Hash]; dup {
			return nil, errorf(CodeInvalidBlob, "blob %s is listed twice", b.Hash)
		}
		blobs[b.Hash] = b
	}
	return blobs, nil
}

// validateFiles checks the tracks, their LRC files and the attachments:
// the limits of §7.2, the ranges and texts of the tracks, unique source
// paths, the LRC association of §7.4, and attachment paths that stay
// distinct after normalization, file/directory collisions included (§5.2).
func validateFiles(c ImportCandidate, plan *importPlan, use func(string, blobRole) error) error {
	if len(c.Tracks) == 0 {
		return errorf(CodeNoTracks, "an album needs at least one track")
	}
	if len(c.Tracks) > MaxTracks {
		return errorf(CodeTooManyFiles, "%d tracks, the maximum is %d", len(c.Tracks), MaxTracks)
	}
	files := len(c.Tracks) + len(c.Attachments)
	for _, t := range c.Tracks {
		if t.Lyrics != nil {
			files++
		}
	}
	if files > MaxFiles {
		return errorf(CodeTooManyFiles, "%d files, the maximum is %d", files, MaxFiles)
	}
	sources := map[string]bool{}
	source := func(p, what string) error {
		if _, err := names.SplitRelPath(p); err != nil {
			return textError(what+" "+p, err)
		}
		if sources[p] {
			return &Error{Code: CodeDuplicateSource, Message: fmt.Sprintf("%s %q is listed twice", what, p), Details: Details{Path: p}}
		}
		sources[p] = true
		return nil
	}
	fields := make([]trackFields, len(c.Tracks))
	for i, t := range c.Tracks {
		if err := source(t.SourcePath, "track"); err != nil {
			return err
		}
		var err error
		if fields[i], err = normalizeTrack("track "+t.SourcePath, t.Disc, t.No, t.Title, t.Artist, t.Genre); err != nil {
			return err
		}
		if err := use(t.BlobHash, roleAudio); err != nil {
			return err
		}
	}
	if err := checkTrackNumbers(fields, func(i int) string { return c.Tracks[i].SourcePath }); err != nil {
		return err
	}
	for i, t := range c.Tracks {
		p := store.InsertTracksParams{
			ID: store.NewID(), AlbumID: plan.albumID, Disc: fields[i].Disc, No: fields[i].No,
			Title: fields[i].Title, Artist: fields[i].Artist, Genre: fields[i].Genre,
			BlobHash: t.BlobHash, SourcePath: t.SourcePath,
		}
		if t.Lyrics != nil {
			if err := source(t.Lyrics.SourcePath, "LRC file"); err != nil {
				return err
			}
			if err := checkLyrics(c.Tracks, i); err != nil {
				return err
			}
			if err := use(t.Lyrics.BlobHash, roleLyrics); err != nil {
				return err
			}
			p.LyricsHash = &t.Lyrics.BlobHash
		}
		plan.tracks = append(plan.tracks, p)
	}
	for _, a := range c.Attachments {
		if err := source(a.RelPath, "attachment"); err != nil {
			return err
		}
		if err := use(a.BlobHash, roleAttachment); err != nil {
			return err
		}
	}
	atts, err := attachmentPaths(c.Attachments)
	if err != nil {
		return err
	}
	for i, a := range c.Attachments {
		plan.attachments = append(plan.attachments, store.InsertAttachmentsParams{
			ID: store.NewID(), AlbumID: plan.albumID, RelPath: a.RelPath, PathKey: atts[i].Key, BlobHash: a.BlobHash,
		})
	}
	return nil
}

// checkLyrics is §7.4's association rule for the LRC file of track i: an
// .lrc file in the same directory as the track, with the same stem compared
// with NFC and casefold, and exactly one track of that directory with that
// stem; more than one makes the association ambiguous.
func checkLyrics(tracks []ImportTrack, i int) error {
	t := tracks[i]
	lrc := t.Lyrics.SourcePath
	dir, base := path.Split(lrc)
	if !strings.EqualFold(path.Ext(base), ".lrc") {
		return &Error{Code: CodeLyricsAssociation,
			Message: fmt.Sprintf("%q is associated with %q but is not an .lrc file", lrc, t.SourcePath),
			Details: Details{Names: []string{lrc, t.SourcePath}}}
	}
	key := stemKey(base)
	if trackDir, trackBase := path.Split(t.SourcePath); trackDir != dir || stemKey(trackBase) != key {
		return &Error{Code: CodeLyricsAssociation,
			Message: fmt.Sprintf("%q does not have the directory and stem of %q", lrc, t.SourcePath),
			Details: Details{Names: []string{lrc, t.SourcePath}}}
	}
	var same []string
	for _, o := range tracks {
		if d, b := path.Split(o.SourcePath); d == dir && stemKey(b) == key {
			same = append(same, o.SourcePath)
		}
	}
	if len(same) != 1 {
		return &Error{Code: CodeLyricsAssociation,
			Message: fmt.Sprintf("%q matches %d tracks of its directory: the association is ambiguous", lrc, len(same)),
			Details: Details{Names: append([]string{lrc}, same...)}}
	}
	return nil
}

// stemKey is the comparison key of a file name without its extension:
// NFC and casefold (§7.4).
func stemKey(base string) string {
	return names.Key(strings.TrimSuffix(base, path.Ext(base)))
}

// attachmentPaths sanitizes the attachments' paths (§5.2) and refuses two
// that collide after normalization, including a file whose path is the
// directory of another, naming both: the user corrects them, no file is
// lost to a deduplication of names (§5.2). The check is over the whole
// namespace under Extras/.
func attachmentPaths(atts []ImportAttachment) ([]names.SanitizedPath, error) {
	out := make([]names.SanitizedPath, len(atts))
	files := make(map[string]string, len(atts))
	dirs := map[string]string{}
	for i, a := range atts {
		sp, err := names.SanitizeRelFilePath(a.RelPath)
		if err != nil {
			return nil, textError("attachment "+a.RelPath, err)
		}
		out[i] = sp
		if other, ok := files[sp.Key]; ok {
			return nil, collision(other, a.RelPath)
		}
		files[sp.Key] = a.RelPath
		for d := 1; d < len(sp.Segments); d++ {
			k := names.PathKey(sp.Segments[:d])
			if _, ok := dirs[k]; !ok {
				dirs[k] = a.RelPath
			}
		}
	}
	for i, a := range atts {
		if dirOwner, ok := dirs[out[i].Key]; ok {
			return nil, collision(a.RelPath, dirOwner)
		}
	}
	return out, nil
}

func collision(a, b string) *Error {
	return &Error{
		Code:    CodeAttachmentCollision,
		Message: fmt.Sprintf("the attachments %q and %q have the same name after normalization", a, b),
		Details: Details{Names: []string{a, b}},
	}
}
