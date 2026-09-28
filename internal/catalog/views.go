package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/store"
)

// The catalog's reads for the API (§10.1, §10.2). Each one runs in a single
// REPEATABLE READ snapshot (store.InSnapshotTx), so that a representation
// and the revision it carries always agree: the aggregate is never a mix of
// two commits. They take no lock and write nothing.

// Artist is an artist as the catalog knows it: name and revision, no
// derived counter (§10.2).
type Artist struct {
	ID       uuid.UUID
	Name     string
	Revision int64
}

// BlobRef is a blob as an album references it: its content address, size
// and content-derived format ("" for any other content, §4.2).
type BlobRef struct {
	Hash   string
	Size   int64
	Format string
}

// AlbumView is the desired aggregate of an album (§4.1, §10.2 GET
// /api/albums/{id}): the album, its artist's name, cover, tracks and
// attachments, with the album's revision. It carries no processing state:
// that is AlbumStatus (§10.1).
type AlbumView struct {
	ID          uuid.UUID
	Revision    int64
	ArtistID    uuid.UUID
	ArtistName  string
	Title       string
	Year        *int32
	Genre       *string
	Compilation bool
	Trashed     bool
	Cover       *BlobRef
	// Tracks are ordered by disc, number, id; Attachments by path key.
	Tracks      []TrackView
	Attachments []AttachmentView
}

// TrackView is one track of an AlbumView. Artist nil inherits the album
// artist; Genre nil inherits the album genre, "" is explicitly none (§4.1).
type TrackView struct {
	ID     uuid.UUID
	Disc   int32
	No     int32
	Title  string
	Artist *string
	Genre  *string
	// SourcePath is the file's path relative to its import candidate
	// (N-099), exactly as it was on disk.
	SourcePath string
	Blob       BlobRef
	// DurationMS is the audio's duration in milliseconds, nil while unknown
	// (NOTES.md N-300): a fact of the blob, read-only, filled by the import
	// or by a later render without a new revision.
	DurationMS *int64
	// LyricsHash is the blob of the associated LRC, if any (§7.4).
	LyricsHash *string
}

// AttachmentView is one attachment of an AlbumView: its relative path, as
// imported or chosen, before the output's sanitization (§5.2).
type AttachmentView struct {
	ID      uuid.UUID
	RelPath string
	Blob    BlobRef
}

// AlbumStatus is the mutable processing state of an album (§10.2 GET
// /api/albums/{id}/status): the desired and published revisions, the
// renderer of the published output, its path relative to library/, and
// the album's render job if any.
type AlbumStatus struct {
	AlbumID           uuid.UUID
	Revision          int64
	Trashed           bool
	PublishedRevision int64
	PublishedRenderer *string
	PublishedPath     *string
	Job               *RenderJob
}

// RenderJob is an album's render row (§4.2, §6.3): pending, running or
// failed, with the error of a failed attempt.
type RenderJob struct {
	ID           uuid.UUID
	State        string
	ErrorCode    *string
	ErrorMessage *string
	QueuedAt     time.Time
	UpdatedAt    time.Time
}

// ListArtists returns every artist of the catalog, those without albums
// included (owner decision, NOTES.md N-146: the list feeds the artist
// selector of the album editor, §10.3), ordered by folder key and id.
func (s *Service) ListArtists(ctx context.Context) ([]Artist, error) {
	var out []Artist
	err := store.InSnapshotTx(ctx, s.db, func(q *store.Queries) error {
		rows, err := q.ListArtists(ctx)
		if err != nil {
			return dbErr("listing the artists", err)
		}
		out = make([]Artist, len(rows))
		for i, r := range rows {
			out[i] = Artist{ID: r.ID, Name: r.Name, Revision: r.Revision}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetArtist reads an artist. CodeArtistNotFound if there is none.
func (s *Service) GetArtist(ctx context.Context, id uuid.UUID) (Artist, error) {
	var a Artist
	err := store.InSnapshotTx(ctx, s.db, func(q *store.Queries) error {
		r, err := q.GetArtist(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(CodeArtistNotFound, "artist %s does not exist", id)
		}
		if err != nil {
			return dbErr("reading artist "+id.String(), err)
		}
		a = Artist{ID: r.ID, Name: r.Name, Revision: r.Revision}
		return nil
	})
	return a, err
}

// GetAlbum reads an album's desired aggregate in one snapshot.
// CodeAlbumNotFound if there is none.
func (s *Service) GetAlbum(ctx context.Context, id uuid.UUID) (AlbumView, error) {
	var v AlbumView
	err := store.InSnapshotTx(ctx, s.db, func(q *store.Queries) error {
		var err error
		v, err = readAlbumView(ctx, q, id)
		return err
	})
	return v, err
}

func readAlbumView(ctx context.Context, q *store.Queries, id uuid.UUID) (AlbumView, error) {
	al, err := q.GetAlbumView(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return AlbumView{}, errorf(CodeAlbumNotFound, "album %s does not exist", id)
	}
	if err != nil {
		return AlbumView{}, dbErr("reading album "+id.String(), err)
	}
	v := AlbumView{
		ID: al.ID, Revision: al.Revision, ArtistID: al.ArtistID, ArtistName: al.ArtistName, Title: al.Title,
		Year: al.Year, Genre: al.Genre, Compilation: al.Compilation, Trashed: al.Trashed,
	}
	if al.CoverHash != nil {
		v.Cover = &BlobRef{Hash: *al.CoverHash, Size: deref(al.CoverSize), Format: deref(al.CoverFormat)}
	}
	tracks, err := q.ListAlbumTrackViews(ctx, id)
	if err != nil {
		return AlbumView{}, dbErr("listing the tracks of album "+id.String(), err)
	}
	v.Tracks = make([]TrackView, len(tracks))
	for i, t := range tracks {
		v.Tracks[i] = TrackView{
			ID: t.ID, Disc: t.Disc, No: t.No, Title: t.Title, Artist: t.Artist, Genre: t.Genre,
			SourcePath: t.SourcePath, Blob: BlobRef{Hash: t.BlobHash, Size: t.BlobSize, Format: deref(t.BlobFormat)},
			DurationMS: t.BlobDurationMs, LyricsHash: t.LyricsHash,
		}
	}
	atts, err := q.ListAlbumAttachmentViews(ctx, id)
	if err != nil {
		return AlbumView{}, dbErr("listing the attachments of album "+id.String(), err)
	}
	v.Attachments = make([]AttachmentView, len(atts))
	for i, a := range atts {
		v.Attachments[i] = AttachmentView{
			ID: a.ID, RelPath: a.RelPath, Blob: BlobRef{Hash: a.BlobHash, Size: a.BlobSize, Format: deref(a.BlobFormat)},
		}
	}
	return v, nil
}

// GetAlbumStatus reads an album's processing state. CodeAlbumNotFound if
// there is none.
func (s *Service) GetAlbumStatus(ctx context.Context, id uuid.UUID) (AlbumStatus, error) {
	var st AlbumStatus
	err := store.InSnapshotTx(ctx, s.db, func(q *store.Queries) error {
		r, err := q.GetAlbumStatus(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(CodeAlbumNotFound, "album %s does not exist", id)
		}
		if err != nil {
			return dbErr("reading the status of album "+id.String(), err)
		}
		st = AlbumStatus{
			AlbumID: r.ID, Revision: r.Revision, Trashed: r.Trashed, PublishedRevision: r.PublishedRevision,
			PublishedRenderer: r.PublishedRenderer, PublishedPath: r.PublishedPath,
		}
		if r.JobID != nil {
			st.Job = &RenderJob{
				ID: *r.JobID, State: deref(r.JobState), ErrorCode: r.JobErrorCode, ErrorMessage: r.JobErrorMessage,
				QueuedAt: deref(r.JobQueuedAt), UpdatedAt: deref(r.JobUpdatedAt),
			}
		}
		return nil
	})
	return st, err
}
