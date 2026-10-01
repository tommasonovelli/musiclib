package catalog

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/names"
	"musiclib/internal/store"
)

// The album editor's content operations of §10.2 (NOTES.md N-172 to
// N-180): the cover, the attachments, the lyrics of a track, the addition
// and the deletion of a track. Each is one change of the album through
// changeAlbum: the album's revision (the ETag of §10.2: "Gli endpoint
// relativi a cover/allegati/tracce richiedono l'ETag dell'album") compared
// in the transaction, and an effective change bumps the revision and
// enqueues the render in the same transaction (§4.3). A change that
// changes nothing (the cover already chosen, a cover already absent) is a
// no-op without bump or render.
//
// The blobs they reference are pinned by the caller before the
// transaction (§7.5, §10.2: "il blob viene fissato prima della
// transazione con If-Match"), and their content already checked: a cover
// is a JPEG or PNG validated by media.ValidateCover, a lyrics file valid
// UTF-8, a track read and decoded as an import reads one. The catalog records the blob, checks it against what it already
// knows (N-102), and never reads it.
//
// An album in the trash accepts them all, like the rename of §4.3: its
// render is the removal, and what it keeps comes back with the restore
// (N-104, N-178).

// The codes of the content operations.
const (
	// CodeAttachmentNotFound: no attachment with that id in the album
	// (§10.2: the entity is looked up by id within its album, 404).
	CodeAttachmentNotFound = "attachment_not_found"
	// CodeTrackNotFound: no track with that id in the album (404).
	CodeTrackNotFound = "track_not_found"
	// CodeCoverNotEmbeddable: a valid cover that an audio format of the
	// album cannot embed (owner decision N-091): refused when chosen,
	// never discovered by a render. Details.Names has the format.
	CodeCoverNotEmbeddable = "cover_not_embeddable"
	// CodeInvalidLyrics: an attachment chosen as a track's lyrics that is
	// not an .lrc file (§10.2).
	CodeInvalidLyrics = "invalid_lyrics"
	// CodeTrackExists: a file added as a track is already a track of the
	// album, the same audio blob (409). Details.TrackID is that track,
	// Details.Names its title. Two tracks are never merged.
	CodeTrackExists = "track_exists"
)

// CoverChoice is a cover chosen for an album (§10.2 PUT
// /api/albums/{id}/cover): a pinned JPEG or PNG blob, validated by
// media.ValidateCover (§8.5), either uploaded (Attachment is uuid.Nil) or
// the blob of an image attachment of the same album. An attachment chosen
// as the cover stays an attachment (§7.4).
type CoverChoice struct {
	Blob       Blob
	Attachment uuid.UUID
}

// SetCover makes c the album's cover (§10.2, §8.5). The blob must be a
// JPEG or PNG of at most MaxCoverBytes (CodeInvalidCover), embeddable in
// every audio format of the album (CoverFits, owner decision N-091:
// CodeCoverNotEmbeddable); a chosen attachment must be an attachment of
// the album holding that blob (CodeAttachmentNotFound). Choosing the cover
// the album already has is a no-op. ifMatch as in UpdateAlbum.
func (s *Service) SetCover(ctx context.Context, albumID uuid.UUID, ifMatch int64, c CoverChoice) (int64, bool, error) {
	if err := checkBlob(c.Blob); err != nil {
		return 0, false, err
	}
	if !coverFormats[c.Blob.Format] {
		return 0, false, errorf(CodeInvalidCover, "the cover must be a JPEG or PNG image, not %q", c.Blob.Format)
	}
	if c.Blob.Size > MaxCoverBytes {
		return 0, false, errorf(CodeInvalidCover, "the cover takes %d bytes, the maximum is %d", c.Blob.Size, MaxCoverBytes)
	}
	return s.changeAlbum(ctx, albumID, ifMatch, func(tx *store.CatalogTx, al store.Album) (bool, error) {
		if c.Attachment != uuid.Nil {
			if _, err := attachmentWithBlob(ctx, tx, al.ID, c.Attachment, c.Blob.Hash); err != nil {
				return false, err
			}
		}
		if al.CoverHash != nil && *al.CoverHash == c.Blob.Hash {
			return false, nil
		}
		if err := registerBlob(ctx, tx, c.Blob, roleCover); err != nil {
			return false, err
		}
		if err := checkAlbumCoverFits(ctx, tx, al.ID, c.Blob, s.coverFits); err != nil {
			return false, err
		}
		if err := tx.SetAlbumCover(ctx, store.SetAlbumCoverParams{ID: al.ID, CoverHash: &c.Blob.Hash}); err != nil {
			return false, dbErr("setting the cover of album "+al.ID.String(), err)
		}
		return true, nil
	})
}

// RemoveCover makes the album's cover absent (§10.2 DELETE
// /api/albums/{id}/cover: "cover assente, anche nei tag futuri"): the next
// output has no cover file and no embedded picture (§8.2). The blob stays,
// and an attachment holding the same image stays an attachment. Removing
// an absent cover is a no-op. ifMatch as in UpdateAlbum.
func (s *Service) RemoveCover(ctx context.Context, albumID uuid.UUID, ifMatch int64) (int64, bool, error) {
	return s.changeAlbum(ctx, albumID, ifMatch, func(tx *store.CatalogTx, al store.Album) (bool, error) {
		if al.CoverHash == nil {
			return false, nil
		}
		if err := tx.SetAlbumCover(ctx, store.SetAlbumCoverParams{ID: al.ID}); err != nil {
			return false, dbErr("removing the cover of album "+al.ID.String(), err)
		}
		return true, nil
	})
}

// AttachmentPath validates the relative path of an attachment and returns
// its output form under Extras/ (§5.1, §5.2): the rule of the import
// commit (names.SanitizeRelFilePath: no absolute path, no "." or ".."
// segment, no empty segment, valid UTF-8, no NUL, at most 16 levels and
// 1,024 bytes after sanitization). It needs no database, so the API
// refuses an invalid path before it reads any byte of the upload.
func AttachmentPath(relPath string) (names.SanitizedPath, error) {
	sp, err := names.SanitizeRelFilePath(relPath)
	if err != nil {
		return names.SanitizedPath{}, textError("attachment "+relPath, err)
	}
	return sp, nil
}

// AttachmentConflict is the collision rule of §4.2 and §5.2 for a new
// attachment at relPath among the album's attachments at existing (their
// rel_paths): the same output file, a file where another needs a
// directory, or one directory spelled two ways, after normalization, are
// CodeAttachmentCollision naming both paths. It is pure: AddAttachment
// applies it under the catalog lock, and the API before it reads an
// upload, so that a conflicting file is refused before it is pinned.
func AttachmentConflict(existing []string, relPath string) error {
	atts := make([]ImportAttachment, 0, len(existing)+1)
	for _, p := range existing {
		atts = append(atts, ImportAttachment{RelPath: p})
	}
	// The new path comes last: the existing ones never collide with each
	// other (the import commit and this rule refused it), so any collision
	// names it.
	_, err := attachmentPaths(append(atts, ImportAttachment{RelPath: relPath}))
	return err
}

// CheckLyricsAttachment is the rule for an attachment chosen as a track's
// lyrics (§10.2, N-177): its name ends in .lrc, case-insensitively, or it
// is CodeInvalidLyrics naming the path. It is pure: SetLyrics applies it
// in the transaction, and the API on its snapshot before it reads the
// attachment's content.
func CheckLyricsAttachment(relPath string) error {
	if strings.EqualFold(path.Ext(relPath), ".lrc") {
		return nil
	}
	return &Error{Code: CodeInvalidLyrics,
		Message: fmt.Sprintf("the attachment %q is not an .lrc file", relPath),
		Details: Details{Path: relPath}}
}

// CheckRevision is §10.1's comparison of the revision seen with the
// current one, for a caller that compares early on a snapshot to avoid
// useless work (the API before it reads an upload, N-173). The change
// itself compares again in its transaction.
func CheckRevision(kind string, id uuid.UUID, ifMatch, current int64) error {
	return checkRevision(kind, id, ifMatch, current)
}

// AddAttachment adds an uploaded file to the album's attachments under
// relPath (§10.2 POST /api/albums/{id}/attachments), materialized under
// Extras/ (§5.1). relPath is kept as given (§5.2: "il percorso relativo
// originale o scelto dall'utente") and must be valid (AttachmentPath). Its
// output path must not collide with another attachment's after
// normalization: the same file, a file where another needs a directory, or
// one directory spelled two ways are CodeAttachmentCollision naming both
// (§4.2, §5.2). It always changes the album. ifMatch as in UpdateAlbum.
// It returns the album's revision and the new attachment's id.
func (s *Service) AddAttachment(ctx context.Context, albumID uuid.UUID, ifMatch int64, relPath string, b Blob) (int64, uuid.UUID, error) {
	sp, err := AttachmentPath(relPath)
	if err != nil {
		return 0, uuid.Nil, err
	}
	if err := checkBlob(b); err != nil {
		return 0, uuid.Nil, err
	}
	id := store.NewID()
	rev, _, err := s.changeAlbum(ctx, albumID, ifMatch, func(tx *store.CatalogTx, al store.Album) (bool, error) {
		rows, err := tx.ListAlbumAttachmentPaths(ctx, al.ID)
		if err != nil {
			return false, dbErr("listing the attachments of album "+al.ID.String(), err)
		}
		existing := make([]string, len(rows))
		for i, r := range rows {
			existing[i] = r.RelPath
		}
		if err := AttachmentConflict(existing, relPath); err != nil {
			return false, err
		}
		if err := registerBlob(ctx, tx, b, roleAttachment); err != nil {
			return false, err
		}
		err = tx.InsertAttachment(ctx, store.InsertAttachmentParams{
			ID: id, AlbumID: al.ID, RelPath: relPath, PathKey: sp.Key, BlobHash: b.Hash,
		})
		if err != nil {
			return false, dbErr("adding an attachment to album "+al.ID.String(), err)
		}
		return true, nil
	})
	if err != nil {
		return 0, uuid.Nil, err
	}
	return rev, id, nil
}

// DeleteAttachment removes an attachment of the album (§4.3, §10.2): the
// row goes, with no undo; the blob stays. If the attachment's image is the
// album's cover, the cover stays: it is a reference to the blob, not to
// the attachment (N-176). ifMatch as in UpdateAlbum.
func (s *Service) DeleteAttachment(ctx context.Context, albumID uuid.UUID, ifMatch int64, attachmentID uuid.UUID) (int64, bool, error) {
	return s.changeAlbum(ctx, albumID, ifMatch, func(tx *store.CatalogTx, al store.Album) (bool, error) {
		n, err := tx.DeleteAttachment(ctx, store.DeleteAttachmentParams{ID: attachmentID, AlbumID: al.ID})
		if err != nil {
			return false, dbErr("removing attachment "+attachmentID.String(), err)
		}
		if n == 0 {
			return false, attachmentNotFound(al.ID, attachmentID)
		}
		return true, nil
	})
}

// LyricsChoice is the LRC file chosen for a track (§10.2 PUT
// /api/albums/{id}/tracks/{track}/lyrics): a pinned blob of valid UTF-8
// text, either uploaded (Attachment is uuid.Nil) or the blob of an .lrc
// attachment of the same album. A chosen attachment stays an attachment
// (N-177).
type LyricsChoice struct {
	Blob       Blob
	Attachment uuid.UUID
}

// SetLyrics makes c the lyrics of a track of the album (§5.1: the LRC file
// next to the track). The track must be the album's (CodeTrackNotFound);
// a chosen attachment must be an attachment of the album holding that
// blob (CodeAttachmentNotFound) and an .lrc file (CodeInvalidLyrics); the
// blob must not be known as audio or an image (CodeInvalidBlobFormat, as
// at the import). The lyrics the track already has are a no-op. ifMatch
// as in UpdateAlbum.
func (s *Service) SetLyrics(ctx context.Context, albumID uuid.UUID, ifMatch int64, trackID uuid.UUID, c LyricsChoice) (int64, bool, error) {
	if err := checkBlob(c.Blob); err != nil {
		return 0, false, err
	}
	return s.changeAlbum(ctx, albumID, ifMatch, func(tx *store.CatalogTx, al store.Album) (bool, error) {
		tr, err := albumTrack(ctx, tx, al.ID, trackID)
		if err != nil {
			return false, err
		}
		if c.Attachment != uuid.Nil {
			att, err := attachmentWithBlob(ctx, tx, al.ID, c.Attachment, c.Blob.Hash)
			if err != nil {
				return false, err
			}
			if err := CheckLyricsAttachment(att.RelPath); err != nil {
				return false, err
			}
		}
		if tr.LyricsHash != nil && *tr.LyricsHash == c.Blob.Hash {
			return false, nil
		}
		if err := registerBlob(ctx, tx, c.Blob, roleLyrics); err != nil {
			return false, err
		}
		return true, setLyrics(ctx, tx, al.ID, trackID, &c.Blob.Hash)
	})
}

// RemoveLyrics removes the lyrics of a track (§10.2 DELETE
// /api/albums/{id}/tracks/{track}/lyrics): lyrics_hash = NULL, the blob
// stays. A track without lyrics is a no-op. ifMatch as in UpdateAlbum.
func (s *Service) RemoveLyrics(ctx context.Context, albumID uuid.UUID, ifMatch int64, trackID uuid.UUID) (int64, bool, error) {
	return s.changeAlbum(ctx, albumID, ifMatch, func(tx *store.CatalogTx, al store.Album) (bool, error) {
		tr, err := albumTrack(ctx, tx, al.ID, trackID)
		if err != nil {
			return false, err
		}
		if tr.LyricsHash == nil {
			return false, nil
		}
		return true, setLyrics(ctx, tx, al.ID, trackID, nil)
	})
}

// NewTrack is a file read as a track of an existing album, as an import
// reads one (importer.ReadTrack), to append to the album with AddTrack.
// Blob is pinned, with its audio format and its duration (nil unknown).
// SourcePath is the file's name. Title is required; Artist, Genre, Disc and
// No are the file's tags as read: "" and 0 for an absent tag.
type NewTrack struct {
	Blob       Blob
	SourcePath string
	Title      string
	Artist     string
	Genre      string
	Disc       int
	No         int
}

// TrackFileName is the source path of an uploaded track: the last
// "/"-separated segment of name, which must be a valid relative path
// (names.SplitRelPath, the rule of the import's source paths: not empty,
// not "." or "..", valid UTF-8, no NUL). It needs no database, so the API
// refuses an invalid name before it reads any byte of the upload.
func TrackFileName(name string) (string, error) {
	base := name
	if name != "" {
		base = path.Base(name)
	}
	if _, err := names.SplitRelPath(base); err != nil {
		return "", textError("track file name "+name, err)
	}
	return base, nil
}

// AddTrack adds a file as a new track of the album (POST
// /api/albums/{id}/tracks), in one change of the album: the revision of
// ifMatch compared in the transaction, the revision bumped and the render
// enqueued. The audio blob already a track of the album is
// CodeTrackExists, naming that track: nothing is merged. The track keeps
// the disc and number of its tags when they are free, otherwise it is
// appended (landing). Its artist is the tag when it is not the album's
// artist (SameArtistName), otherwise inherited; its genre the tag when it
// is not the album's genre, otherwise inherited. With the new track, every
// genre of the album must fit every audio format of its tracks
// (CodeGenreNotWritable) and the cover must stay embeddable in them
// (CodeCoverNotEmbeddable). An album keeps at most MaxTracks tracks. It
// returns the album's revision and the new track's id.
func (s *Service) AddTrack(ctx context.Context, albumID uuid.UUID, ifMatch int64, t NewTrack) (int64, uuid.UUID, error) {
	if err := checkBlob(t.Blob); err != nil {
		return 0, uuid.Nil, err
	}
	if err := checkRole(t.Blob.Hash, t.Blob.Format, roleAudio); err != nil {
		return 0, uuid.Nil, err
	}
	base, err := TrackFileName(t.SourcePath)
	if err != nil {
		return 0, uuid.Nil, err
	}
	if base != t.SourcePath {
		return 0, uuid.Nil, errorf(CodeInvalidArgument, "the source path of a new track is a file name, not %q", t.SourcePath)
	}
	id := store.NewID()
	rev, _, err := s.changeAlbum(ctx, albumID, ifMatch, func(tx *store.CatalogTx, al store.Album) (bool, error) {
		current, err := tx.ListAlbumTracks(ctx, al.ID)
		if err != nil {
			return false, dbErr("listing the tracks of album "+al.ID.String(), err)
		}
		occupied := Slots{}
		all := make([]trackFields, 0, len(current)+1)
		for _, c := range current {
			if c.BlobHash == t.Blob.Hash {
				return false, trackExists(al.ID, c.ID, c.Title)
			}
			occupied[[2]int32{c.Disc, c.No}] = true
			all = append(all, trackFields{Genre: c.Genre})
		}
		if len(current) >= MaxTracks {
			return false, errorf(CodeTooManyFiles, "album %s has %d tracks, the maximum", al.ID, len(current))
		}
		disc, no, err := landing(occupied, t.Disc, t.No)
		if err != nil {
			return false, err
		}
		ar, err := tx.GetArtist(ctx, al.ArtistID)
		if err != nil {
			return false, dbErr("reading artist "+al.ArtistID.String(), err)
		}
		var artist, genre *string
		if t.Artist != "" && !SameArtistName(t.Artist, ar.Name) {
			artist = &t.Artist
		}
		if t.Genre != "" && t.Genre != deref(al.Genre) {
			genre = &t.Genre
		}
		f, err := normalizeTrack("track "+t.SourcePath, int(disc), int(no), t.Title, artist, genre)
		if err != nil {
			return false, err
		}
		if err := registerBlob(ctx, tx, t.Blob, roleAudio); err != nil {
			return false, err
		}
		_, err = tx.InsertTracks(ctx, []store.InsertTracksParams{{
			ID: id, AlbumID: al.ID, Disc: f.Disc, No: f.No, Title: f.Title, Artist: f.Artist, Genre: f.Genre,
			BlobHash: t.Blob.Hash, SourcePath: t.SourcePath,
		}})
		if err != nil {
			return false, dbErr("adding a track to album "+al.ID.String(), err)
		}
		// The album's formats now include the new track's.
		if err := checkGenres(ctx, tx, al.ID, albumFields{Genre: al.Genre}, append(all, f), s.genreFits); err != nil {
			return false, err
		}
		if al.CoverHash != nil {
			cover, err := tx.GetBlobs(ctx, []string{*al.CoverHash})
			if err == nil && len(cover) != 1 {
				err = pgx.ErrNoRows
			}
			if err != nil {
				return false, dbErr("reading the cover of album "+al.ID.String(), err)
			}
			c := Blob{Hash: cover[0].Hash, Size: cover[0].Size, Format: deref(cover[0].Format)}
			if err := checkAlbumCoverFits(ctx, tx, al.ID, c, s.coverFits); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return 0, uuid.Nil, err
	}
	return rev, id, nil
}

// CheckTrackAbsent refuses, on a snapshot of the album, a file whose blob
// is already one of its tracks (CodeTrackExists): the API asks it before
// it reads an uploaded track, so that a duplicate is refused without its
// decode. AddTrack asks it again in its transaction, which decides.
func CheckTrackAbsent(v AlbumView, hash string) error {
	for _, t := range v.Tracks {
		if t.Blob.Hash == hash {
			return trackExists(v.ID, t.ID, t.Title)
		}
	}
	return nil
}

func trackExists(albumID, trackID uuid.UUID, title string) *Error {
	return &Error{Code: CodeTrackExists,
		Message: fmt.Sprintf("this file is already track %s (%q) of album %s", trackID, title, albumID),
		Details: Details{TrackID: trackID, Names: []string{title}}}
}

// Slots are the (disc, number) places taken by an album's tracks.
type Slots map[[2]int32]bool

// landing is where a track lands among the occupied places: the place it
// asks for (wantDisc, wantNo) when it is valid and free; a number without
// a disc asks for disc 1, as at the import. Otherwise, or without a
// number (wantNo 0), it is appended: on the album's last disc (the highest
// disc number present, 1 when there is none), after its highest number.
// A disc already numbered up to MaxTrackNumber is CodeInvalidTrackNumber.
// Several tracks land one after the other: the caller adds each place to
// occupied before the next. It is pure.
func landing(occupied Slots, wantDisc, wantNo int) (disc, no int32, err error) {
	if wantDisc == 0 && wantNo > 0 {
		wantDisc = 1
	}
	if wantDisc >= 1 && wantDisc <= MaxDisc && wantNo >= 1 && wantNo <= MaxTrackNumber &&
		!occupied[[2]int32{int32(wantDisc), int32(wantNo)}] {
		return int32(wantDisc), int32(wantNo), nil
	}
	disc = 1
	for k := range occupied {
		disc = max(disc, k[0])
	}
	for k := range occupied {
		if k[0] == disc {
			no = max(no, k[1])
		}
	}
	if no >= MaxTrackNumber {
		return 0, 0, errorf(CodeInvalidTrackNumber,
			"disc %d already has a track %d, the highest number: renumber its tracks to make room", disc, no)
	}
	return disc, no + 1, nil
}

// DeleteTrack removes a track of the album (§4.3, §10.2 DELETE
// /api/albums/{id}/tracks/{track}): the row goes, with its lyrics
// reference, and no undo; the blobs stay. The album keeps at least one
// track (CodeNoTracks), in the trash too, since a restore must give an
// active album with tracks (§4.3). ifMatch as in UpdateAlbum.
func (s *Service) DeleteTrack(ctx context.Context, albumID uuid.UUID, ifMatch int64, trackID uuid.UUID) (int64, bool, error) {
	return s.changeAlbum(ctx, albumID, ifMatch, func(tx *store.CatalogTx, al store.Album) (bool, error) {
		if _, err := albumTrack(ctx, tx, al.ID, trackID); err != nil {
			return false, err
		}
		n, err := tx.CountAlbumTracks(ctx, al.ID)
		if err != nil {
			return false, dbErr("counting the tracks of album "+al.ID.String(), err)
		}
		if n <= 1 {
			return false, errorf(CodeNoTracks, "track %s is the last track of album %s: an album keeps at least one track", trackID, al.ID)
		}
		if _, err := tx.DeleteTrack(ctx, store.DeleteTrackParams{ID: trackID, AlbumID: al.ID}); err != nil {
			return false, dbErr("removing track "+trackID.String(), err)
		}
		return true, nil
	})
}

func attachmentNotFound(albumID, id uuid.UUID) *Error {
	return errorf(CodeAttachmentNotFound, "album %s has no attachment %s", albumID, id)
}

// attachmentWithBlob reads an attachment of the album and checks that it
// holds hash, the blob its content was checked on.
func attachmentWithBlob(ctx context.Context, tx *store.CatalogTx, albumID, id uuid.UUID, hash string) (store.GetAlbumAttachmentRow, error) {
	att, err := tx.GetAlbumAttachment(ctx, store.GetAlbumAttachmentParams{ID: id, AlbumID: albumID})
	if errors.Is(err, pgx.ErrNoRows) {
		return att, attachmentNotFound(albumID, id)
	}
	if err != nil {
		return att, dbErr("reading attachment "+id.String(), err)
	}
	if att.BlobHash != hash {
		// An attachment's blob never changes: the caller checked another
		// blob than the one it names.
		return att, errorf(CodeInvalidArgument, "attachment %s holds blob %s, not %s", id, att.BlobHash, hash)
	}
	return att, nil
}

// albumTrack reads a track of the album.
func albumTrack(ctx context.Context, tx *store.CatalogTx, albumID, id uuid.UUID) (store.GetAlbumTrackRow, error) {
	tr, err := tx.GetAlbumTrack(ctx, store.GetAlbumTrackParams{ID: id, AlbumID: albumID})
	if errors.Is(err, pgx.ErrNoRows) {
		return tr, errorf(CodeTrackNotFound, "album %s has no track %s", albumID, id)
	}
	if err != nil {
		return tr, dbErr("reading track "+id.String(), err)
	}
	return tr, nil
}

func setLyrics(ctx context.Context, tx *store.CatalogTx, albumID, trackID uuid.UUID, hash *string) error {
	n, err := tx.SetTrackLyrics(ctx, store.SetTrackLyricsParams{ID: trackID, AlbumID: albumID, LyricsHash: hash})
	if err != nil {
		return dbErr("setting the lyrics of track "+trackID.String(), err)
	}
	if n != 1 {
		return errorf(CodeDB, "track %s vanished under the catalog lock", trackID)
	}
	return nil
}

// checkBlob validates a blob handed to a content operation: a SHA-256, a
// size, a known format (a caller's error otherwise).
func checkBlob(b Blob) error {
	_, err := indexBlobs([]Blob{b})
	return err
}

// registerBlob records a pinned blob that a content operation references,
// with the import's rules (N-102): a hash already recorded with another
// size is CodeBlobMismatch, two different known formats are
// CodeInvalidBlobFormat, a format known on one side only is the content's,
// and the merged format must fit the blob's role (checkRole).
func registerBlob(ctx context.Context, tx *store.CatalogTx, b Blob, role blobRole) error {
	rows, err := tx.GetBlobs(ctx, []string{b.Hash})
	if err != nil {
		return dbErr("reading blob "+b.Hash, err)
	}
	format := b.Format
	for _, r := range rows {
		if r.Size != b.Size {
			return errorf(CodeBlobMismatch, "blob %s is recorded with %d bytes, the upload has %d", b.Hash, r.Size, b.Size)
		}
		known := deref(r.Format)
		if known != "" && format != "" && known != format {
			return errorf(CodeInvalidBlobFormat, "blob %s is recorded as %q, its content now reads as %q", b.Hash, known, format)
		}
		if format == "" {
			format = known
		}
	}
	if err := checkRole(b.Hash, format, role); err != nil {
		return err
	}
	return registerBlobs(ctx, tx, []Blob{b})
}

// checkAlbumCoverFits asks coverFits (owner decision N-091) whether cover
// can be embedded in every audio format of the album's tracks, in a fixed
// order: CodeCoverNotEmbeddable naming the first format that cannot.
func checkAlbumCoverFits(ctx context.Context, tx *store.CatalogTx, albumID uuid.UUID, cover Blob, coverFits CoverFits) error {
	formats, err := tx.ListAlbumAudioFormats(ctx, albumID)
	if err != nil {
		return dbErr("listing the audio formats of album "+albumID.String(), err)
	}
	names := make([]string, len(formats))
	for i, f := range formats {
		names[i] = *f
	}
	return coverFitsAll(coverFits, cover, names)
}

// CheckCoverFits is N-091's question for cover and the audio formats of an
// album, asked without the database: the API asks it on a snapshot before
// it pins an uploaded cover, so that a valid image the album cannot embed
// is refused without leaving a blob (N-173). SetCover asks it again in its
// transaction, which decides.
func (s *Service) CheckCoverFits(cover Blob, audioFormats []string) error {
	formats := slices.Clone(audioFormats)
	slices.Sort(formats)
	return coverFitsAll(s.coverFits, cover, slices.Compact(formats))
}

// coverFitsAll is CodeCoverNotEmbeddable for the first of formats whose
// files cannot embed cover.
func coverFitsAll(coverFits CoverFits, cover Blob, formats []string) error {
	for _, f := range formats {
		if err := coverFits(cover, f); err != nil {
			return &Error{Code: CodeCoverNotEmbeddable,
				Message: fmt.Sprintf("the %s cover of %d bytes cannot be embedded in the album's %s files", cover.Format, cover.Size, f),
				Details: Details{Names: []string{f}}, Err: err}
		}
	}
	return nil
}
