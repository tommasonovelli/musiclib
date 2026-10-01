package catalog

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/store"
)

// The codes of a move of tracks.
const (
	// CodeAlbumTrashed: the album a move of tracks would go to is in the
	// trash (409). Details.AlbumID is that album.
	CodeAlbumTrashed = "album_trashed"
	// CodeSameAlbum: a move of tracks to the album they are in (422).
	CodeSameAlbum = "same_album"
)

// MoveResult is the outcome of MoveTracks: the new revisions of both
// albums, and whether the move put the source in the trash, because it
// left it without tracks.
type MoveResult struct {
	From, To int64
	Trashed  bool
}

// MoveTracks moves tracks of album fromID to album toID (POST
// /api/albums/{id}/move-tracks), in one catalog transaction that changes
// both albums. The catalog lock serializes it with every other catalog
// mutation, so changing two albums here cannot deadlock with another one.
//
// ifMatch is the source's revision, compared in the transaction (0 is
// CodePreconditionRequired, another revision CodePreconditionFailed). The
// destination needs none, an explicit exception to the If-Match rule: the
// tracks are only appended to it, and its revision is bumped, so an editor
// still open on it gets 412 on its next save.
//
// trackIDs are tracks of the source (CodeTrackNotFound otherwise), at
// least one and each once (CodeTrackListMismatch). The destination is
// another album (CodeSameAlbum) that exists (CodeAlbumNotFound, naming it)
// and is not in the trash (CodeAlbumTrashed); the source may be, to rescue
// a track from it. A track whose audio is already a track of the
// destination is CodeTrackExists: two tracks are never merged.
//
// The tracks land in the destination one after the other, in their order
// in the source (disc, number), each at its own disc and number when free
// there, otherwise after the destination's last track (landing). Title,
// artist, genre, lyrics, source path and audio go with the track as they
// are: an artist or genre it inherited now comes from the destination. The
// destination keeps at most MaxTracks tracks (CodeTooManyFiles), and every
// genre it holds must fit every audio format of its tracks
// (CodeGenreNotWritable), its cover stay embeddable in them
// (CodeCoverNotEmbeddable). The cover and the attachments of the source
// stay with the source.
//
// A source left without tracks goes to the trash in the same transaction:
// its render becomes the removal of its output, it can be emptied from the
// trash, and RestoreAlbum refuses it while it has no track. Its artist
// keeps it, so the artist rule is unchanged. Both albums get a new
// revision, their path claims reconciled and their render enqueued; any
// refusal, a path conflict included, leaves both as they were.
func (s *Service) MoveTracks(ctx context.Context, fromID uuid.UUID, ifMatch int64, trackIDs []uuid.UUID, toID uuid.UUID) (MoveResult, error) {
	if ifMatch == 0 {
		return MoveResult{}, checkRevision("album", fromID, 0, 0)
	}
	if len(trackIDs) == 0 {
		return MoveResult{}, errorf(CodeTrackListMismatch, "no track to move")
	}
	seen := make(map[uuid.UUID]bool, len(trackIDs))
	for _, id := range trackIDs {
		if seen[id] {
			return MoveResult{}, errorf(CodeTrackListMismatch, "track %s is listed twice", id)
		}
		seen[id] = true
	}
	if fromID == toID {
		return MoveResult{}, errorf(CodeSameAlbum, "the tracks are already in album %s: choose another album", toID)
	}
	var out MoveResult
	err := store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		out = MoveResult{}
		from, err := tx.GetAlbum(ctx, fromID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(CodeAlbumNotFound, "album %s does not exist", fromID)
		}
		if err != nil {
			return dbErr("reading album "+fromID.String(), err)
		}
		if err := checkRevision("album", fromID, ifMatch, from.Revision); err != nil {
			return err
		}
		to, err := tx.GetAlbum(ctx, toID)
		if errors.Is(err, pgx.ErrNoRows) {
			e := errorf(CodeAlbumNotFound, "album %s, where the tracks would go, does not exist", toID)
			e.Details.AlbumID = toID
			return e
		}
		if err != nil {
			return dbErr("reading album "+toID.String(), err)
		}
		if to.DeletedAt != nil {
			e := errorf(CodeAlbumTrashed, "album %s (%q) is in the trash: restore it before moving tracks to it", to.ID, to.Title)
			e.Details.AlbumID = to.ID
			return e
		}
		trashed, err := moveTracks(ctx, tx, from, to, trackIDs, s.genreFits, s.coverFits)
		if err != nil {
			return err
		}
		if out.From, err = outputChanged(ctx, tx, from.ID, false); err != nil {
			return err
		}
		if out.To, err = outputChanged(ctx, tx, to.ID, false); err != nil {
			return err
		}
		out.Trashed = trashed
		return nil
	})
	if err != nil {
		return MoveResult{}, err
	}
	s.notify()
	return out, nil
}

// moveTracks moves the tracks trackIDs of from to the end of to, as
// MoveTracks says, and trashes from when it is left without tracks. It
// returns whether it did.
func moveTracks(ctx context.Context, tx *store.CatalogTx, from, to store.Album, trackIDs []uuid.UUID,
	genreFits GenreFits, coverFits CoverFits) (bool, error) {
	source, err := tx.ListAlbumTracks(ctx, from.ID)
	if err != nil {
		return false, dbErr("listing the tracks of album "+from.ID.String(), err)
	}
	moving := make(map[uuid.UUID]bool, len(trackIDs))
	for _, id := range trackIDs {
		moving[id] = true
	}
	var moved []store.ListAlbumTracksRow
	for _, t := range source {
		if moving[t.ID] {
			moved = append(moved, t)
			delete(moving, t.ID)
		}
	}
	for _, id := range trackIDs {
		if moving[id] {
			return false, errorf(CodeTrackNotFound, "album %s has no track %s", from.ID, id)
		}
	}
	dest, err := tx.ListAlbumTracks(ctx, to.ID)
	if err != nil {
		return false, dbErr("listing the tracks of album "+to.ID.String(), err)
	}
	if n := len(dest) + len(moved); n > MaxTracks {
		return false, errorf(CodeTooManyFiles, "album %s would have %d tracks, the maximum is %d", to.ID, n, MaxTracks)
	}
	occupied := Slots{}
	all := make([]trackFields, 0, len(dest)+len(moved))
	audio := make(map[string]store.ListAlbumTracksRow, len(dest))
	for _, d := range dest {
		occupied[[2]int32{d.Disc, d.No}] = true
		all = append(all, trackFields{Genre: d.Genre})
		audio[d.BlobHash] = d
	}
	for _, t := range moved {
		if d, ok := audio[t.BlobHash]; ok {
			return false, trackExists(to.ID, d.ID, d.Title)
		}
		disc, no, err := landing(occupied, int(t.Disc), int(t.No))
		if err != nil {
			return false, err
		}
		occupied[[2]int32{disc, no}] = true
		n, err := tx.MoveTrack(ctx, store.MoveTrackParams{ToAlbumID: to.ID, Disc: disc, No: no, ID: t.ID, AlbumID: from.ID})
		if err != nil {
			return false, dbErr("moving track "+t.ID.String(), err)
		}
		if n != 1 {
			return false, errorf(CodeDB, "track %s vanished under the catalog lock", t.ID)
		}
		all = append(all, trackFields{Genre: t.Genre})
	}
	// The destination's formats now include the moved tracks'.
	if err := checkGenres(ctx, tx, to.ID, albumFields{Genre: to.Genre}, all, genreFits); err != nil {
		return false, err
	}
	if err := checkCoverStillFits(ctx, tx, to, coverFits); err != nil {
		return false, err
	}
	if len(moved) < len(source) || from.DeletedAt != nil {
		return false, nil
	}
	if err := tx.SetAlbumTrashed(ctx, store.SetAlbumTrashedParams{ID: from.ID, Trashed: true}); err != nil {
		return false, dbErr("trashing album "+from.ID.String(), err)
	}
	return true, nil
}
