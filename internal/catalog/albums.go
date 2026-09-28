package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

// The schema's ranges (§4.2).
const (
	MinYear, MaxYear = 1, 9999
	MaxDisc          = 99
	MaxTrackNumber   = 999
)

// outputChanged is §4.3's one primitive for a change of an album's output,
// in the caller's transaction: it bumps the album's revision (a new album
// is born at revision 1 instead), re-derives its path claims (§5.3; a path
// reserved by another album fails the whole transaction) and enqueues its
// render (§6.3). Metadata, reservation and render request are therefore
// saved together (§3.2 guarantee 6). It returns the album's revision.
func outputChanged(ctx context.Context, tx *store.CatalogTx, albumID uuid.UUID, created bool) (int64, error) {
	revision := int64(1)
	if !created {
		var err error
		revision, err = tx.BumpAlbumRevision(ctx, albumID)
		if err != nil {
			return 0, dbErr("bumping the revision of album "+albumID.String(), err)
		}
	}
	if err := ReconcileClaims(ctx, tx, albumID); err != nil {
		return 0, err
	}
	if _, err := jobs.EnqueueRender(ctx, tx, albumID); err != nil {
		return 0, err
	}
	return revision, nil
}

// albumFields are the album's own metadata, normalized (§5.2).
type albumFields struct {
	Title       string
	FolderKey   string
	Year        *int32
	Genre       *string
	Compilation bool
}

// normalizeAlbum validates the album's metadata: a required title (§5.2),
// a year in 1..9999, a genre that is a valid text. An album has nothing to
// inherit from, so an empty genre and no genre are the same value and are
// stored as NULL.
func normalizeAlbum(title string, year *int, genre *string, compilation bool) (albumFields, error) {
	var f albumFields
	var err error
	if f.Title, err = names.NormalizeRequiredText(title); err != nil {
		return f, textError("album title", err)
	}
	f.FolderKey = names.FolderKey(f.Title)
	if year != nil {
		if *year < MinYear || *year > MaxYear {
			return f, errorf(CodeInvalidYear, "the year %d is outside %d..%d", *year, MinYear, MaxYear)
		}
		y := int32(*year)
		f.Year = &y
	}
	if genre != nil {
		g, err := names.NormalizeText(*genre)
		if err != nil {
			return f, textError("album genre", err)
		}
		if g != "" {
			f.Genre = &g
		}
	}
	f.Compilation = compilation
	return f, nil
}

// trackFields are a track's editable metadata, normalized.
type trackFields struct {
	Disc   int32
	No     int32
	Title  string
	Artist *string
	Genre  *string
}

// normalizeTrack validates a track's metadata (§4.1, §4.2, §5.2): disc
// 1..99, number 1..999, a required title; the artist is NULL to inherit
// and never empty; the genre is NULL to inherit, empty for explicitly none.
// label names the track in errors.
func normalizeTrack(label string, disc, no int, title string, artist, genre *string) (trackFields, error) {
	var f trackFields
	if disc < 1 || disc > MaxDisc {
		return f, errorf(CodeInvalidDisc, "%s: disc %d is outside 1..%d", label, disc, MaxDisc)
	}
	if no < 1 || no > MaxTrackNumber {
		return f, errorf(CodeInvalidTrackNumber, "%s: track number %d is outside 1..%d", label, no, MaxTrackNumber)
	}
	f.Disc, f.No = int32(disc), int32(no)
	var err error
	if f.Title, err = names.NormalizeRequiredText(title); err != nil {
		return f, textError(label+": title", err)
	}
	if artist != nil {
		a, err := names.NormalizeRequiredText(*artist)
		if err != nil {
			return f, textError(label+": artist (NULL inherits the album artist; empty is not allowed)", err)
		}
		f.Artist = &a
	}
	if genre != nil {
		g, err := names.NormalizeText(*genre)
		if err != nil {
			return f, textError(label+": genre", err)
		}
		f.Genre = &g
	}
	return f, nil
}

// checkTrackNumbers refuses two tracks with the same disc and number,
// naming both.
func checkTrackNumbers(tracks []trackFields, label func(i int) string) error {
	seen := make(map[[2]int32]int, len(tracks))
	for i, t := range tracks {
		k := [2]int32{t.Disc, t.No}
		if j, ok := seen[k]; ok {
			return &Error{
				Code:    CodeDuplicateTrackNumber,
				Message: fmt.Sprintf("disc %d track %d is used by both %s and %s", t.Disc, t.No, label(j), label(i)),
				Details: Details{Names: []string{label(j), label(i)}},
			}
		}
		seen[k] = i
	}
	return nil
}

// AlbumUpdate is the body of PUT /api/albums/{id} (§10.2): the album's
// metadata and exactly its current tracks. Blobs and ids are not editable.
//
// The album's artist is exactly one of ArtistID, an existing artist, and
// NewArtist, the name of an artist to create in the update's own
// transaction (NOTES.md N-298): if the update fails, for a stale revision
// or an invalid value, the artist is not created either.
type AlbumUpdate struct {
	ArtistID    uuid.UUID
	NewArtist   *string
	Title       string
	Year        *int
	Genre       *string
	Compilation bool
	Tracks      []TrackUpdate
}

// TrackUpdate is one track of an AlbumUpdate. Artist nil inherits the
// album artist; Genre nil inherits the album genre, "" is explicitly none.
type TrackUpdate struct {
	ID     uuid.UUID
	Disc   int
	No     int
	Title  string
	Artist *string
	Genre  *string
}

// UpdateAlbum saves an album's metadata and tracks in one transaction
// (§10.2 PUT /api/albums/{id}). ifMatch is the album revision seen by the
// client, compared in the same transaction (§10.1): 0 is
// CodePreconditionRequired, another revision CodePreconditionFailed.
//
// The track list must be exactly the album's current tracks
// (CodeTrackListMismatch); numbers may be permuted freely, since the unique
// constraint on (album, disc, no) is checked at commit (§4.2, §12.2). A
// save without any effective change is a no-op: no new revision, no render
// (§4.3). Otherwise the revision is bumped and the render enqueued
// atomically. For an active album the folder must be free under the
// artist (CodeAlbumFolderConflict) and the path not reserved by another
// album (CodePathReserved); an album in the trash can be renamed freely
// before its restore (§4.3).
//
// A new artist (u.NewArtist) is created after the revision check, with
// CreateArtist's rule: a name that exists already is CodeArtistExists, one
// that shares an existing artist's folder CodeArtistFolderConflict, both
// with the existing artist in Details (N-298). An artist the album leaves
// is deleted if it has no album left, in the same transaction (N-297).
//
// It returns the album's revision and whether anything changed.
func (s *Service) UpdateAlbum(ctx context.Context, albumID uuid.UUID, ifMatch int64, u AlbumUpdate) (int64, bool, error) {
	if ifMatch == 0 {
		return 0, false, checkRevision("album", albumID, 0, 0)
	}
	if (u.ArtistID == uuid.Nil) == (u.NewArtist == nil) {
		return 0, false, errorf(CodeInvalidArgument, "an album update names either an existing artist or a new one")
	}
	if u.NewArtist != nil {
		name, err := names.NormalizeRequiredText(*u.NewArtist)
		if err != nil {
			return 0, false, textError("artist name", err)
		}
		u.NewArtist = &name
	}
	fields, err := normalizeAlbum(u.Title, u.Year, u.Genre, u.Compilation)
	if err != nil {
		return 0, false, err
	}
	tracks := make([]trackFields, len(u.Tracks))
	for i, t := range u.Tracks {
		if tracks[i], err = normalizeTrack("track "+t.ID.String(), t.Disc, t.No, t.Title, t.Artist, t.Genre); err != nil {
			return 0, false, err
		}
	}
	if err := checkTrackNumbers(tracks, func(i int) string { return "track " + u.Tracks[i].ID.String() }); err != nil {
		return 0, false, err
	}
	return s.changeAlbum(ctx, albumID, ifMatch, func(tx *store.CatalogTx, al store.Album) (bool, error) {
		return applyUpdate(ctx, tx, al, u, fields, tracks, s.genreFits)
	})
}

// applyUpdate compares the update with the stored album and writes what
// differs. It returns whether anything did.
func applyUpdate(ctx context.Context, tx *store.CatalogTx, al store.Album, u AlbumUpdate, f albumFields, tracks []trackFields, genreFits GenreFits) (bool, error) {
	if u.NewArtist != nil {
		a, err := insertArtist(ctx, tx, *u.NewArtist)
		if err != nil {
			return false, err
		}
		u.ArtistID = a.ID
	} else if _, err := tx.GetArtist(ctx, u.ArtistID); errors.Is(err, pgx.ErrNoRows) {
		return false, errorf(CodeArtistNotFound, "artist %s does not exist", u.ArtistID)
	} else if err != nil {
		return false, dbErr("reading artist "+u.ArtistID.String(), err)
	}
	current, err := tx.ListAlbumTracks(ctx, al.ID)
	if err != nil {
		return false, dbErr("listing the tracks of album "+al.ID.String(), err)
	}
	byID := make(map[uuid.UUID]store.ListAlbumTracksRow, len(current))
	for _, t := range current {
		byID[t.ID] = t
	}
	if len(u.Tracks) != len(current) {
		return false, errorf(CodeTrackListMismatch, "the album has %d tracks, the update lists %d", len(current), len(u.Tracks))
	}
	var changedTracks []int
	listed := make(map[uuid.UUID]bool, len(u.Tracks))
	for i, t := range u.Tracks {
		cur, ok := byID[t.ID]
		if !ok || listed[t.ID] {
			return false, errorf(CodeTrackListMismatch, "track %s is not a track of album %s, or is listed twice", t.ID, al.ID)
		}
		listed[t.ID] = true
		n := tracks[i]
		if cur.Disc != n.Disc || cur.No != n.No || cur.Title != n.Title || !eq(cur.Artist, n.Artist) || !eq(cur.Genre, n.Genre) {
			changedTracks = append(changedTracks, i)
		}
	}
	albumChanged := al.ArtistID != u.ArtistID || al.Title != f.Title || !eq(al.Year, f.Year) ||
		!eq(al.Genre, f.Genre) || al.Compilation != f.Compilation
	if !albumChanged && len(changedTracks) == 0 {
		return false, nil
	}
	if err := checkGenres(ctx, tx, al.ID, f, tracks, genreFits); err != nil {
		return false, err
	}
	if albumChanged {
		if al.DeletedAt == nil {
			if err := checkFolderFree(ctx, tx, u.ArtistID, f.FolderKey, al.ID); err != nil {
				return false, err
			}
		}
		err := tx.UpdateAlbumMetadata(ctx, store.UpdateAlbumMetadataParams{
			ID: al.ID, ArtistID: u.ArtistID, Title: f.Title, FolderKey: f.FolderKey,
			Year: f.Year, Genre: f.Genre, Compilation: f.Compilation,
		})
		if err != nil {
			return false, dbErr("updating album "+al.ID.String(), err)
		}
		if al.ArtistID != u.ArtistID {
			if err := leaveArtist(ctx, tx, al.ArtistID); err != nil {
				return false, err
			}
		}
	}
	for _, i := range changedTracks {
		t := tracks[i]
		n, err := tx.UpdateTrack(ctx, store.UpdateTrackParams{
			ID: u.Tracks[i].ID, AlbumID: al.ID, Disc: t.Disc, No: t.No, Title: t.Title, Artist: t.Artist, Genre: t.Genre,
		})
		if err != nil {
			return false, dbErr("updating track "+u.Tracks[i].ID.String(), err)
		}
		if n != 1 {
			return false, errorf(CodeDB, "track %s vanished under the catalog lock", u.Tracks[i].ID)
		}
	}
	return true, nil
}

// checkFolderFree refuses a folder already used by another active album of
// the artist (§4.2 albums_active_folder_key, §7.6), naming that album.
func checkFolderFree(ctx context.Context, tx *store.CatalogTx, artistID uuid.UUID, folderKey string, self uuid.UUID) error {
	other, err := tx.FindActiveAlbumByFolder(ctx, store.FindActiveAlbumByFolderParams{
		ArtistID: artistID, FolderKey: folderKey, Exclude: self,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return dbErr("looking up the album folder "+folderKey, err)
	}
	return &Error{
		Code:    CodeAlbumFolderConflict,
		Message: fmt.Sprintf("the folder of %q is used by album %s (%q) of the same artist: choose a different title", folderKey, other.ID, other.Title),
		Details: Details{AlbumID: other.ID},
	}
}

// TrashAlbum moves an album to the trash (§4.3, §10.2 DELETE
// /api/albums/{id}): deleted_at is set, metadata and references stay. Its
// output becomes a removal, so the revision is bumped and the render
// enqueued; the desired path is released, the published one stays
// reserved until the removal is published (§5.3). Trashing an album
// already in the trash is a no-op. ifMatch as in UpdateAlbum.
func (s *Service) TrashAlbum(ctx context.Context, albumID uuid.UUID, ifMatch int64) (int64, bool, error) {
	return s.changeAlbum(ctx, albumID, ifMatch, func(tx *store.CatalogTx, al store.Album) (bool, error) {
		if al.DeletedAt != nil {
			return false, nil
		}
		if err := tx.SetAlbumTrashed(ctx, store.SetAlbumTrashedParams{ID: al.ID, Trashed: true}); err != nil {
			return false, dbErr("trashing album "+al.ID.String(), err)
		}
		return true, nil
	})
}

// RestoreAlbum brings an album back from the trash (§4.3, §10.2 POST
// /api/albums/{id}/restore), with the normal name checks: another active
// album with the same folder under the artist is CodeAlbumFolderConflict,
// a path reserved by another album CodePathReserved. Restoring an active
// album is a no-op. ifMatch as in UpdateAlbum.
func (s *Service) RestoreAlbum(ctx context.Context, albumID uuid.UUID, ifMatch int64) (int64, bool, error) {
	return s.changeAlbum(ctx, albumID, ifMatch, func(tx *store.CatalogTx, al store.Album) (bool, error) {
		if al.DeletedAt == nil {
			return false, nil
		}
		if err := checkFolderFree(ctx, tx, al.ArtistID, al.FolderKey, al.ID); err != nil {
			return false, err
		}
		if err := tx.SetAlbumTrashed(ctx, store.SetAlbumTrashedParams{ID: al.ID, Trashed: false}); err != nil {
			return false, dbErr("restoring album "+al.ID.String(), err)
		}
		return true, nil
	})
}

// RequestRender is the forced render of §10.2 (POST
// /api/albums/{id}/render): the album's render is enqueued through the
// single jobs.EnqueueRender (§6.3, §13.2) without changing any metadata,
// so the revision is not incremented. ifMatch is still the revision seen
// by the client, compared in the same transaction (§10.2: "I render
// manuali richiedono anch'essi la revisione vista"): 0 is
// CodePreconditionRequired, another revision CodePreconditionFailed. An
// album in the trash is enqueued too: its render is the removal of its
// output (§4.3). It returns the album's revision and the render row.
func (s *Service) RequestRender(ctx context.Context, albumID uuid.UUID, ifMatch int64) (int64, jobs.Enqueued, error) {
	if ifMatch == 0 {
		return 0, jobs.Enqueued{}, checkRevision("album", albumID, 0, 0)
	}
	var (
		revision int64
		enqueued jobs.Enqueued
	)
	err := store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		revision, enqueued = 0, jobs.Enqueued{}
		al, err := tx.GetAlbum(ctx, albumID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(CodeAlbumNotFound, "album %s does not exist", albumID)
		}
		if err != nil {
			return dbErr("reading album "+albumID.String(), err)
		}
		if err := checkRevision("album", albumID, ifMatch, al.Revision); err != nil {
			return err
		}
		if enqueued, err = jobs.EnqueueRender(ctx, tx, albumID); err != nil {
			return err
		}
		revision = al.Revision
		return nil
	})
	if err != nil {
		return 0, jobs.Enqueued{}, err
	}
	s.notify()
	return revision, enqueued, nil
}

// changeAlbum is the frame of every change of an existing album: the
// catalog transaction, the album read, the If-Match comparison in the same
// transaction (§10.1), the change itself, and, if it changed anything,
// outputChanged. It returns the album's revision and whether it changed.
func (s *Service) changeAlbum(ctx context.Context, albumID uuid.UUID, ifMatch int64,
	change func(tx *store.CatalogTx, al store.Album) (bool, error)) (int64, bool, error) {
	if ifMatch == 0 {
		return 0, false, checkRevision("album", albumID, 0, 0)
	}
	var (
		revision int64
		changed  bool
	)
	err := store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		revision, changed = 0, false
		al, err := tx.GetAlbum(ctx, albumID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(CodeAlbumNotFound, "album %s does not exist", albumID)
		}
		if err != nil {
			return dbErr("reading album "+albumID.String(), err)
		}
		if err := checkRevision("album", albumID, ifMatch, al.Revision); err != nil {
			return err
		}
		ok, err := change(tx, al)
		if err != nil {
			return err
		}
		if !ok {
			revision = al.Revision
			return nil
		}
		if revision, err = outputChanged(ctx, tx, albumID, false); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	if changed {
		s.notify()
	}
	return revision, changed, nil
}

// eq compares two nullable values.
func eq[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// checkGenres asks genreFits about the album genre and every track genre
// of a change, for every audio format of the album's tracks (owner decision
// N-162): a genre an MP3 cannot hold is refused when the album has an MP3
// track. An empty genre is written as none, and is always fine.
func checkGenres(ctx context.Context, tx *store.CatalogTx, albumID uuid.UUID, f albumFields, tracks []trackFields, genreFits GenreFits) error {
	formats, err := tx.ListAlbumAudioFormats(ctx, albumID)
	if err != nil {
		return dbErr("listing the audio formats of album "+albumID.String(), err)
	}
	var genres []string
	if f.Genre != nil && *f.Genre != "" {
		genres = append(genres, *f.Genre)
	}
	for _, t := range tracks {
		if t.Genre != nil && *t.Genre != "" {
			genres = append(genres, *t.Genre)
		}
	}
	for _, format := range formats {
		for _, g := range genres {
			if err := genreFits(g, *format); err != nil {
				return err
			}
		}
	}
	return nil
}
