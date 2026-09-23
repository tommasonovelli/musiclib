package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/store"
)

// Claim is a job taken by a worker: the row is running, with claimed equal
// to the ticket of Attempt (§6.2). The worker must complete it through
// FinishRender, RequeueRender, FailRender or Finish; a process that stops
// first leaves it to RecoverRunning at the next boot.
type Claim struct {
	Attempt Attempt
	Kind    Kind
	// BatchID is set for scan and import.
	BatchID uuid.UUID
	// SourceRel and Overrides are set for import: the candidate, relative
	// to /import exactly as on disk (§5.2), and the user's overrides (§7.3).
	SourceRel string
	Overrides Overrides
	// Render is set for render: the snapshot read in the claim's own
	// transaction (§6.2).
	Render *RenderSnapshot
}

// RenderSnapshot is the coherent state of one album that a render is built
// from (§6.2): read in the REPEATABLE READ transaction of the claim, so the
// artist, the album, the tracks and the attachments are those of a single
// committed catalog state, and Attempt.Ticket is the request that state
// answers. The pure planner (§6.2, §9.1) consumes it with render_version.
//
// It is a plain value: nullable columns are zero values documented field by
// field, and nothing in the queue keeps a reference to it after ClaimNext
// returns it. It carries no absolute path (§13.2).
type RenderSnapshot struct {
	Attempt Attempt
	// RenderVersion is the renderer the claim was made for. PREPARE's
	// recheck compares it with the current one (§6.3).
	RenderVersion string
	Artist        SnapshotArtist
	Album         SnapshotAlbum
	// Cover is the album's cover blob; a zero Hash is no cover.
	Cover SnapshotBlob
	// Tracks are ordered by disc and number.
	Tracks []SnapshotTrack
	// Attachments are ordered by path_key.
	Attachments []SnapshotAttachment
}

// SnapshotArtist is the album's artist (§4.1: it decides the folder and the
// album artist tag).
type SnapshotArtist struct {
	ID       uuid.UUID
	Name     string
	Revision int64
}

// SnapshotAlbum is the albums row.
type SnapshotAlbum struct {
	ID    uuid.UUID
	Title string
	// Year is 0 when absent (the column only admits 1..9999).
	Year        int
	Genre       Text
	Compilation bool
	Revision    int64
	// Deleted: the album is in the trash, and the render is a removal
	// (§9.1 step 3).
	Deleted bool
	// The published state (§4.2). PublishedRenderer is "" when never
	// published; PublishedPath, PublishedBuild and PublishedReceiptHash are
	// zero when there is no output.
	PublishedPath        string
	PublishedRevision    int64
	PublishedRenderer    string
	PublishedBuild       uuid.UUID
	PublishedReceiptHash string
}

// SnapshotTrack is one tracks row with its blobs.
type SnapshotTrack struct {
	ID    uuid.UUID
	Disc  int
	No    int
	Title string
	// Artist invalid inherits the album's artist; Genre invalid inherits
	// the album's genre, and a valid empty Genre is explicitly none (§4.1).
	Artist     Text
	Genre      Text
	SourcePath string
	Blob       SnapshotBlob
	// Lyrics is the LRC; a zero Hash is none.
	Lyrics SnapshotBlob
}

// SnapshotAttachment is one attachments row with its blob.
type SnapshotAttachment struct {
	ID      uuid.UUID
	RelPath string
	PathKey string
	Blob    SnapshotBlob
}

// SnapshotBlob is a blob reference: the content hash, the size and
// blobs.format ("" is any other content, §4.2).
type SnapshotBlob struct {
	Hash   string
	Size   int64
	Format string
}

// Text is a nullable text column as a value: Valid false is NULL.
type Text struct {
	String string
	Valid  bool
}

func text(p *string) Text {
	if p == nil {
		return Text{}
	}
	return Text{String: *p, Valid: true}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// testHook, when set by a test, runs at named points of the claim
// transaction, so that a test can commit a concurrent change at an exact
// point (§12.2). It is nil in production.
var testHook func(point string)

func hook(point string) {
	if testHook != nil {
		testHook(point)
	}
}

// ClaimNext claims the next pending job in the fixed order of §6.1: render,
// then scan, then import, each by queued_at and id. It returns nil when
// nothing is pending.
//
// The claim is one short REPEATABLE READ transaction (§6.2): SELECT ... FOR
// UPDATE SKIP LOCKED, then running with claimed = requested. For a render
// the same transaction loads the snapshot, so the snapshot and the claimed
// ticket describe the same committed state. A serialization failure retries
// that transaction only (store.InSnapshotTx); nothing else is repeated.
// renderVersion is recorded in the snapshot.
//
// Concurrent claimers never claim a job twice, but in REPEATABLE READ a
// job claimed by another claimer after this snapshot is a 40001, not a
// skipped row: under a storm of claimers one may exhaust its retries
// (store.CodeRetriesExhausted, having claimed nothing). Pool serializes the
// claims of its own workers for that reason (N-110).
func ClaimNext(ctx context.Context, db *pgxpool.Pool, renderVersion string) (*Claim, error) {
	var claim *Claim
	err := store.InSnapshotTx(ctx, db, func(q *store.Queries) error {
		claim = nil
		for _, kind := range claimOrder {
			c, err := claimKind(ctx, q, kind, renderVersion)
			if err != nil || c != nil {
				claim = c
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claim, nil
}

// claimKind claims the oldest pending job of one kind, or returns nil.
func claimKind(ctx context.Context, q *store.Queries, kind Kind, renderVersion string) (*Claim, error) {
	row, err := q.NextPendingJob(ctx, string(kind))
	hook("after-select-" + string(kind))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, dbErr("selecting a pending "+string(kind)+" job", err)
	}
	ticket, err := q.MarkJobRunning(ctx, row.ID)
	if err != nil {
		return nil, dbErr("marking job "+row.ID.String()+" running", err)
	}
	c := &Claim{Attempt: Attempt{JobID: row.ID, Ticket: ticket}, Kind: kind}
	switch kind {
	case KindRender:
		c.Render, err = loadSnapshot(ctx, q, *row.AlbumID, c.Attempt, renderVersion)
	case KindScan:
		c.BatchID = *row.BatchID
	case KindImport:
		c.BatchID, c.SourceRel = *row.BatchID, *row.SourceRel
		c.Overrides, err = DecodeOverrides(row.Overrides)
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// loadSnapshot reads the album of a render claim, in the claim's snapshot.
func loadSnapshot(ctx context.Context, q *store.Queries, albumID uuid.UUID, a Attempt, renderVersion string) (*RenderSnapshot, error) {
	al, err := q.SnapshotAlbum(ctx, albumID)
	if err != nil {
		return nil, dbErr("loading album "+albumID.String(), err)
	}
	hook("after-album")
	tracks, err := q.SnapshotTracks(ctx, albumID)
	if err != nil {
		return nil, dbErr("loading the tracks of album "+albumID.String(), err)
	}
	atts, err := q.SnapshotAttachments(ctx, albumID)
	if err != nil {
		return nil, dbErr("loading the attachments of album "+albumID.String(), err)
	}
	s := &RenderSnapshot{
		Attempt:       a,
		RenderVersion: renderVersion,
		Artist:        SnapshotArtist{ID: al.ArtistID, Name: al.ArtistName, Revision: al.ArtistRevision},
		Album: SnapshotAlbum{
			ID:                   al.ID,
			Title:                al.Title,
			Year:                 int(deref(al.Year)),
			Genre:                text(al.Genre),
			Compilation:          al.Compilation,
			Revision:             al.Revision,
			Deleted:              al.Deleted,
			PublishedPath:        deref(al.PublishedPath),
			PublishedRevision:    al.PublishedRevision,
			PublishedRenderer:    deref(al.PublishedRenderer),
			PublishedBuild:       deref(al.PublishedBuild),
			PublishedReceiptHash: deref(al.PublishedReceiptHash),
		},
		Tracks:      make([]SnapshotTrack, len(tracks)),
		Attachments: make([]SnapshotAttachment, len(atts)),
	}
	if al.CoverHash != nil {
		s.Cover = SnapshotBlob{Hash: *al.CoverHash, Size: deref(al.CoverSize), Format: deref(al.CoverFormat)}
	}
	for i, t := range tracks {
		s.Tracks[i] = SnapshotTrack{
			ID: t.ID, Disc: int(t.Disc), No: int(t.No), Title: t.Title,
			Artist: text(t.Artist), Genre: text(t.Genre), SourcePath: t.SourcePath,
			Blob: SnapshotBlob{Hash: t.BlobHash, Size: t.BlobSize, Format: deref(t.BlobFormat)},
		}
		if t.LyricsHash != nil {
			s.Tracks[i].Lyrics = SnapshotBlob{Hash: *t.LyricsHash, Size: deref(t.LyricsSize)}
		}
	}
	for i, a := range atts {
		s.Attachments[i] = SnapshotAttachment{
			ID: a.ID, RelPath: a.RelPath, PathKey: a.PathKey,
			Blob: SnapshotBlob{Hash: a.BlobHash, Size: a.BlobSize, Format: deref(a.BlobFormat)},
		}
	}
	return s, nil
}

// String names the attempt and album for logs (§11.1: album_id, job_id).
func (s *RenderSnapshot) String() string {
	return fmt.Sprintf("render of album %s revision %d (%s)", s.Album.ID, s.Album.Revision, s.Attempt)
}
