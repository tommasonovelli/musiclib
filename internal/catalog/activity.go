package catalog

import (
	"context"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// The reads behind the Import and Activity views (webui-principles
// «Importa», «Attività»; NOTES.md N-286 to N-289), and the dismissal of a
// failed scan or import (owner, N-285). Each read is one snapshot; the
// dismissal is one catalog transaction.

// CodeJobNotDismissable: only a failed scan or import can be dismissed
// (409).
const CodeJobNotDismissable = jobs.CodeNotDismissable

// DismissJob is POST /api/jobs/{id}/dismiss (jobs.Dismiss) in one catalog
// transaction, the lock every change of the queue's outcomes takes, so
// that it can never race the retention purge (N-200). It returns the job
// after the dismissal.
func (s *Service) DismissJob(ctx context.Context, id uuid.UUID) (JobView, error) {
	err := store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		_, err := jobs.Dismiss(ctx, tx, id)
		return err
	})
	if err != nil {
		return JobView{}, err
	}
	return s.GetJob(ctx, id)
}

// AlbumCard is an album as the queue views show it: a cover thumbnail (or
// the initials of the title), the title and the artist.
type AlbumCard struct {
	ID         uuid.UUID
	Title      string
	ArtistName string
	Cover      bool
	Trashed    bool
}

// AlbumCards reads the albums of ids, by id. An id without an album is
// left out.
func (s *Service) AlbumCards(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]AlbumCard, error) {
	out := make(map[uuid.UUID]AlbumCard, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := store.New(s.db).ListAlbumCards(ctx, ids)
	if err != nil {
		return nil, dbErr("reading the albums of the queue", err)
	}
	for _, r := range rows {
		out[r.ID] = AlbumCard{ID: r.ID, Title: r.Title, ArtistName: r.ArtistName, Cover: r.HasCover, Trashed: r.Trashed}
	}
	return out, nil
}

// ActivityJob is a row of the Activity view: a job in progress, waiting or
// needing attention. Folder is the candidate of an import or the batch root
// of a scan, relative to /import as on disk ("" is /import itself); Album
// is the album of a render.
type ActivityJob struct {
	ID           uuid.UUID
	Kind         jobs.Kind
	State        jobs.State
	Ticket       int64
	BatchID      *uuid.UUID
	Folder       string
	Album        *AlbumCard
	ErrorCode    *string
	ErrorMessage *string
	QueuedAt     time.Time
	UpdatedAt    time.Time
}

// ActivityGroup is the jobs of one state, at most the limit asked, and
// how many there are in all.
type ActivityGroup struct {
	Jobs  []ActivityJob
	Total int
}

// Activity is the Activity view: the running jobs, the pending ones in
// queue order, and the failed ones that need attention, newest first.
type Activity struct {
	Running, Pending, Failed ActivityGroup
}

// ListActivity reads the Activity view in one snapshot, at most perState
// jobs of each state. A failed job is listed only while it needs attention
// (not dismissed, not superseded; N-285).
func (s *Service) ListActivity(ctx context.Context, perState int) (Activity, error) {
	if perState < 1 {
		return Activity{}, errorf(CodeInvalidArgument, "the activity lists at least one job per state, not %d", perState)
	}
	var a Activity
	err := store.InSnapshotTx(ctx, s.db, func(q *store.Queries) error {
		rows, err := q.ListActivity(ctx, int64(perState))
		if err != nil {
			return dbErr("listing the activity", err)
		}
		a = Activity{}
		for _, r := range rows {
			j := ActivityJob{
				ID: r.ID, Kind: jobs.Kind(r.Kind), State: jobs.State(r.State), Ticket: r.Requested, BatchID: r.BatchID,
				Folder: r.Folder, ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage, QueuedAt: r.QueuedAt, UpdatedAt: r.UpdatedAt,
			}
			if r.AlbumID != nil && r.AlbumTitle != nil && r.ArtistName != nil {
				j.Album = &AlbumCard{ID: *r.AlbumID, Title: *r.AlbumTitle, ArtistName: *r.ArtistName, Cover: r.HasCover, Trashed: r.Trashed}
			}
			var g *ActivityGroup
			switch j.State {
			case jobs.StateRunning:
				g = &a.Running
			case jobs.StatePending:
				g = &a.Pending
			default:
				g = &a.Failed
			}
			g.Jobs = append(g.Jobs, j)
			g.Total = int(r.StateTotal)
		}
		return nil
	})
	return a, err
}

// RecentImport is an import batch of «Recent imports»: its root, when it
// was created, and how many of its jobs are in progress, imported, already
// in the library and needing attention.
type RecentImport struct {
	ID                                   uuid.UUID
	RootRel                              string
	CreatedAt                            time.Time
	Active, Imported, Present, Attention int
}

// ListRecentImports reads every import batch, newest first. The retention
// of §6.4 bounds them: a batch is purged 90 days after its last outcome
// (N-200).
func (s *Service) ListRecentImports(ctx context.Context) ([]RecentImport, error) {
	rows, err := store.New(s.db).ListRecentImports(ctx)
	if err != nil {
		return nil, dbErr("listing the recent imports", err)
	}
	out := make([]RecentImport, len(rows))
	for i, r := range rows {
		out[i] = RecentImport{ID: r.ID, RootRel: r.RootRel, CreatedAt: r.CreatedAt,
			Active: int(r.Active), Imported: int(r.Imported), Present: int(r.Present), Attention: int(r.Attention)}
	}
	return out, nil
}

// CountRenderAll is how many albums RenderAll would enqueue now: every
// active album and every trashed one still to remove from the library.
func (s *Service) CountRenderAll(ctx context.Context) (int64, error) {
	n, err := store.New(s.db).CountRenderAllAlbums(ctx)
	if err != nil {
		return 0, dbErr("counting the albums to render", err)
	}
	return n, nil
}
