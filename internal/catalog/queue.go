package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

// The queue as the API shows it and changes it (§10.2): the jobs and the
// import report, read in one snapshot each; the retries, render-all and
// the retention of the reports, each one catalog transaction (§13.2). A
// retry and render-all change no catalog row: they only give jobs a new
// ticket (§10.2).

// Page sizes of the API's lists (§10.2: "paginazione 50 max 200").
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ReportRetentionDays is how long the outcomes of an import batch are kept
// (§6.4: "Esiti conservati 90 giorni").
const ReportRetentionDays = 90

// JobView is a job as the API shows it: the jobs row, with its overrides
// and warnings decoded into the closed types of internal/jobs (§4.2).
type JobView struct {
	ID    uuid.UUID
	Kind  jobs.Kind
	State jobs.State
	// Ticket is jobs.requested: a retry gives a new one (§10.2).
	Ticket int64
	// AlbumID is the album of a render; BatchID the batch of a scan or an
	// import; SourceRel the candidate of an import, relative to /import as
	// on disk.
	AlbumID   *uuid.UUID
	BatchID   *uuid.UUID
	SourceRel *string
	Overrides jobs.Overrides
	// ResultAlbumID is the album an import created, or the identical one
	// that made it skipped (§7.6).
	ResultAlbumID *uuid.UUID
	// ErrorCode and ErrorMessage are stored as the executors wrote them;
	// the API shows the message through its own database-text rule
	// (N-150).
	ErrorCode    *string
	ErrorMessage *string
	Warnings     []jobs.Warning
	QueuedAt     time.Time
	UpdatedAt    time.Time
}

func jobView(j store.Job) (JobView, error) {
	v := JobView{
		ID: j.ID, Kind: jobs.Kind(j.Kind), State: jobs.State(j.State), Ticket: j.Requested,
		AlbumID: j.AlbumID, BatchID: j.BatchID, SourceRel: j.SourceRel, ResultAlbumID: j.ResultAlbumID,
		ErrorCode: j.ErrorCode, ErrorMessage: j.ErrorMessage, QueuedAt: j.QueuedAt, UpdatedAt: j.UpdatedAt,
	}
	var err error
	if v.Overrides, err = jobs.DecodeOverrides(j.Overrides); err != nil {
		return JobView{}, err
	}
	if v.Warnings, err = jobs.DecodeWarnings(j.Warnings); err != nil {
		return JobView{}, err
	}
	return v, nil
}

func jobViews(rows []store.Job) ([]JobView, error) {
	out := make([]JobView, len(rows))
	for i, r := range rows {
		var err error
		if out[i], err = jobView(r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// JobFilter selects the jobs of GET /api/jobs.
type JobFilter struct {
	// States and Kinds are the states and kinds listed; empty means
	// pending, running and failed (§10.2), respectively every kind.
	States []jobs.State
	Kinds  []jobs.Kind
	// After is the id the page starts after; uuid.Nil is the first page.
	After uuid.UUID
	// Limit is 1..MaxPageSize.
	Limit int
}

// JobPage is a page of jobs, ordered by id, and the id to continue after
// (uuid.Nil on the last page).
type JobPage struct {
	Jobs []JobView
	Next uuid.UUID
}

// ListJobs is §10.2 GET /api/jobs: pending, running and failed jobs, by
// id (a UUIDv7: creation order), one page in one snapshot. Done and
// skipped jobs are in the import reports (GetImportReport).
func (s *Service) ListJobs(ctx context.Context, f JobFilter) (JobPage, error) {
	if f.Limit < 1 || f.Limit > MaxPageSize {
		return JobPage{}, errorf(CodeInvalidArgument, "a page holds 1 to %d jobs, not %d", MaxPageSize, f.Limit)
	}
	states := []string{string(jobs.StatePending), string(jobs.StateRunning), string(jobs.StateFailed)}
	if len(f.States) > 0 {
		states = states[:0]
		for _, st := range f.States {
			states = append(states, string(st))
		}
	}
	kinds := []string{string(jobs.KindScan), string(jobs.KindImport), string(jobs.KindRender)}
	if len(f.Kinds) > 0 {
		kinds = kinds[:0]
		for _, k := range f.Kinds {
			kinds = append(kinds, string(k))
		}
	}
	var page JobPage
	err := store.InSnapshotTx(ctx, s.db, func(q *store.Queries) error {
		rows, err := q.ListJobs(ctx, store.ListJobsParams{States: states, Kinds: kinds, After: f.After, Lim: int32(f.Limit + 1)})
		if err != nil {
			return dbErr("listing the jobs", err)
		}
		page = JobPage{}
		if len(rows) > f.Limit {
			rows = rows[:f.Limit]
			page.Next = rows[f.Limit-1].ID
		}
		page.Jobs, err = jobViews(rows)
		return err
	})
	return page, err
}

// CodeJobNotFound: no job has that id (404).
const CodeJobNotFound = jobs.CodeNotFound

// GetJob reads a job. CodeJobNotFound if there is none.
func (s *Service) GetJob(ctx context.Context, id uuid.UUID) (JobView, error) {
	var v JobView
	err := store.InSnapshotTx(ctx, s.db, func(q *store.Queries) error {
		j, err := q.GetJob(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Code: CodeJobNotFound, Message: "job " + id.String() + " does not exist"}
		}
		if err != nil {
			return dbErr("reading job "+id.String(), err)
		}
		v, err = jobView(j)
		return err
	})
	return v, err
}

// The states of an import batch, derived from its jobs (§10.2 GET
// /api/imports/{id}).
const (
	// BatchScanning: the scan is pending or running.
	BatchScanning = "scanning"
	// BatchImporting: the scan has an outcome, an import is pending or
	// running.
	BatchImporting = "importing"
	// BatchCompleted: every job has an outcome. A batch without any valid
	// candidate is completed with the scan's explanation, not an empty
	// success (§7.2: its scan is failed, no_valid_candidate).
	BatchCompleted = "completed"
)

// ImportReport is the report of an import batch (§7.2, §10.2): the scan,
// whose warnings are the unassigned files and the rejected entries, and
// one import job per candidate or failed branch, with its state, typed
// error, warnings, overrides and result album.
type ImportReport struct {
	Batch   ImportBatch
	Scan    JobView
	Imports []JobView
}

// State is the batch's state: BatchScanning, BatchImporting or
// BatchCompleted.
func (r ImportReport) State() string {
	if !r.Scan.State.Terminal() {
		return BatchScanning
	}
	for _, j := range r.Imports {
		if !j.State.Terminal() {
			return BatchImporting
		}
	}
	return BatchCompleted
}

// GetImportReport reads a batch and its jobs in one snapshot, the
// candidates by the bytes of their path. CodeImportBatchNotFound if there
// is none.
func (s *Service) GetImportReport(ctx context.Context, id uuid.UUID) (ImportReport, error) {
	var r ImportReport
	err := store.InSnapshotTx(ctx, s.db, func(q *store.Queries) error {
		b, err := q.GetImportBatch(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(CodeImportBatchNotFound, "import batch %s does not exist", id)
		}
		if err != nil {
			return dbErr("reading import batch "+id.String(), err)
		}
		scan, err := q.GetBatchScanJob(ctx, &id)
		if err != nil {
			return dbErr("reading the scan job of batch "+id.String(), err)
		}
		imports, err := q.ListBatchImportJobs(ctx, &id)
		if err != nil {
			return dbErr("listing the import jobs of batch "+id.String(), err)
		}
		r = ImportReport{Batch: ImportBatch{ID: b.ID, RootRel: b.RootRel, CreatedAt: b.CreatedAt, ScanJobID: scan.ID}}
		if r.Scan, err = jobView(scan); err != nil {
			return err
		}
		r.Imports, err = jobViews(imports)
		return err
	})
	return r, err
}

// RetryJob is §10.2 POST /api/jobs/{id}/retry (jobs.Retry) in one catalog
// transaction: a failed job gets a new ticket, a pending or running one is
// left as it is. ov, if not nil, replaces the overrides of a failed import
// (§7.3); its texts are normalized here (§5.2), a refusal typed with the
// names code. It returns the job after the retry and whether it changed.
func (s *Service) RetryJob(ctx context.Context, id uuid.UUID, ov *jobs.Overrides) (JobView, bool, error) {
	if ov != nil {
		n, err := normalizeOverrides(*ov)
		if err != nil {
			return JobView{}, false, err
		}
		ov = &n
	}
	var changed bool
	err := store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		var err error
		changed, err = jobs.Retry(ctx, tx, id, ov)
		return err
	})
	if err != nil {
		return JobView{}, false, err
	}
	if changed {
		s.notify()
	}
	v, err := s.GetJob(ctx, id)
	return v, changed, err
}

// normalizeOverrides applies §5.2's text rule to the overrides of §7.3:
// the values the import commit would use as the album's artist and title.
func normalizeOverrides(ov jobs.Overrides) (jobs.Overrides, error) {
	var out jobs.Overrides
	for _, f := range []struct {
		name string
		in   *string
		out  **string
	}{{"override artist", ov.Artist, &out.Artist}, {"override title", ov.Title, &out.Title}} {
		if f.in == nil {
			continue
		}
		n, err := names.NormalizeRequiredText(*f.in)
		if err != nil {
			return jobs.Overrides{}, textError(f.name, err)
		}
		*f.out = &n
	}
	return out, nil
}

// RetryFailed is §10.2 POST /api/jobs/retry-failed (jobs.RetryFailed) in
// one catalog transaction. It returns how many jobs were retried.
func (s *Service) RetryFailed(ctx context.Context) (int, error) {
	var n int
	err := store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		var err error
		n, err = jobs.RetryFailed(ctx, tx)
		return err
	})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.notify()
	}
	return n, nil
}

// RenderAll is §10.2 POST /api/render-all: in one catalog transaction, the
// render of every active album and of every trashed album whose output is
// still published (a deletion still to materialize) is enqueued through
// the single enqueue, which coalesces on the album's one render row
// (§6.3). No revision changes. It returns how many albums were enqueued.
func (s *Service) RenderAll(ctx context.Context) (int, error) {
	var n int
	err := store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		ids, err := tx.ListRenderAllAlbums(ctx)
		if err != nil {
			return dbErr("listing the albums to render", err)
		}
		for _, id := range ids {
			if _, err := jobs.EnqueueRender(ctx, tx, id); err != nil {
				return err
			}
		}
		n = len(ids)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.notify()
	}
	return n, nil
}

// PurgeImportReports is the retention of §6.4 ("Esiti conservati 90
// giorni; non si eliminano batch con job non terminali"), in one catalog
// transaction: every import batch created more than ReportRetentionDays
// ago whose jobs all have an outcome (done, skipped or failed) last
// changed more than ReportRetentionDays ago is deleted with its jobs. A
// batch with a pending or running job is never touched. Albums are not
// affected: a job references its album, never the reverse. It returns how
// many batches were deleted.
func (s *Service) PurgeImportReports(ctx context.Context) (int, error) {
	var n int
	err := store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		n = 0
		ids, err := tx.ListExpiredBatches(ctx, ReportRetentionDays)
		if err != nil {
			return dbErr("listing the expired import batches", err)
		}
		if len(ids) == 0 {
			return nil
		}
		if _, err := tx.DeleteBatchJobs(ctx, ids); err != nil {
			return dbErr("deleting the jobs of the expired import batches", err)
		}
		deleted, err := tx.DeleteImportBatches(ctx, ids)
		if err != nil {
			return dbErr("deleting the expired import batches", err)
		}
		n = int(deleted)
		return nil
	})
	return n, err
}
