// Package importer is the import of DESIGN.md §7: the scan of a batch
// (§7.2) and the import of one album candidate (§7.1–§7.6).
//
// The scan walks the batch's directory under /import, groups the audio into
// candidates and records every import job, pending or already failed, in one
// transaction (catalog.CommitScan). The import of a candidate revalidates it
// against the current disk, copies every file into the blob store, reads
// everything again from the verified copies (probe, full decode, tags,
// images), infers the album's metadata, chooses the cover, and hands the
// closed candidate to catalog.CommitImport.
//
// The source is never modified: /import is read only through the source
// type (stat, list, open for reading), and a change of the source during an
// import refuses it (§7.1). Every decision is deterministic: directories and
// files are sorted explicitly, never in filesystem order (§7.3).
//
// This is Phase 2, the FLAC vertical slice (§13.1): MP3 and M4A are refused
// with CodeFormatNotSupportedYet until Phase 4, and the multi-disc layouts
// of §7.2 rules 2 and 3 with CodeMultiDiscNotSupported until Phase 5.
package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/store"
)

// workDir is the importer's directory under /data/work: embedded pictures
// extracted for the cover selection (§7.4), removed after use and at boot
// (CleanWork).
const workDir = "import"

// Config are the importer's dependencies, passed explicitly (§2.3).
type Config struct {
	Catalog *catalog.Service
	Tools   *media.Tools
	Blobs   *blobstore.Store
	// Source is /import. The importer only stats, lists and opens it for
	// reading.
	Source *fsops.Root
	// Work is /data/work, on the filesystem of originals (§3.1): its
	// free space is the space of the import (§11.2).
	Work *fsops.Root
	// Budget is the process-wide space budget (§11.2), shared with the
	// builder.
	Budget *jobs.Budget
	Log    *slog.Logger
}

// Importer runs scan and import jobs. It is safe for concurrent use by the
// pool's workers: every job has its own state.
type Importer struct {
	catalog *catalog.Service
	tools   *media.Tools
	blobs   *blobstore.Store
	src     source
	work    *fsops.Root
	budget  *jobs.Budget
	log     *slog.Logger
}

// New returns an importer and creates work/import durably if missing.
func New(cfg Config) (*Importer, error) {
	if cfg.Catalog == nil || cfg.Tools == nil || cfg.Blobs == nil || cfg.Source == nil || cfg.Work == nil || cfg.Budget == nil || cfg.Log == nil {
		return nil, errorf(CodeInvalidArgument, "the importer needs a catalog, the tools, a blob store, /import, work, the space budget and a logger")
	}
	if err := cfg.Work.MkdirAllSync(workDir, 0o755); err != nil {
		return nil, err
	}
	return &Importer{
		catalog: cfg.Catalog, tools: cfg.Tools, blobs: cfg.Blobs,
		src: source{root: cfg.Source}, work: cfg.Work, budget: cfg.Budget, log: cfg.Log,
	}, nil
}

// CleanWork removes whatever an interrupted import left in work/import
// (§11.1 step 5). It must run before any worker starts.
func CleanWork(ctx context.Context, work *fsops.Root) error {
	err := work.RemoveAll(ctx, workDir)
	if err != nil && fsops.Code(err) != fsops.CodeNotFound {
		return err
	}
	return work.MkdirAllSync(workDir, 0o755)
}

// CoverFits is the owner's rule N-091 in the form catalog.New expects: a
// cover must be embeddable in every audio format of its album. The limits
// are the tag adapter's (media.EmbeddedCoverFits).
func CoverFits(cover catalog.Blob, audioFormat string) error {
	return media.EmbeddedCoverFits(audioFormat, cover.Format, cover.Size)
}

// ExecuteScan runs a claimed scan job (§7.2), in the form jobs.Pool
// expects. Every outcome that is not a shutdown or a fatal database error
// ends in a completion of the job (N-107): done, or failed with its
// explanation.
func (im *Importer) ExecuteScan(ctx context.Context, c *jobs.Claim) error {
	if c == nil || c.Kind != jobs.KindScan {
		return errorf(CodeInvalidArgument, "ExecuteScan needs a scan claim")
	}
	o, err := im.scan(ctx, c)
	if err != nil {
		return im.fail(ctx, jobs.KindScan, c.Attempt, err, nil)
	}
	n, err := im.catalog.CommitScan(ctx, o)
	if err != nil {
		return im.fail(ctx, jobs.KindScan, c.Attempt, err, nil)
	}
	im.log.Info("scan finished", "job_id", c.Attempt.JobID, "batch_id", c.BatchID, "state", o.Result.State,
		"imports", len(o.Imports), "inserted", n, "warnings", len(o.Result.Warnings))
	return nil
}

// ExecuteImport runs a claimed import job (§7.1–§7.6), in the form
// jobs.Pool expects. Every outcome that is not a shutdown or a fatal
// database error ends in a completion (N-107): the commit's done or
// skipped, or failed with a typed code.
func (im *Importer) ExecuteImport(ctx context.Context, c *jobs.Claim) error {
	if c == nil || c.Kind != jobs.KindImport {
		return errorf(CodeInvalidArgument, "ExecuteImport needs an import claim")
	}
	cand, warnings, err := im.importCandidate(ctx, c)
	if err != nil {
		return im.fail(ctx, jobs.KindImport, c.Attempt, err, warnings)
	}
	out, err := im.catalog.CommitImport(ctx, cand)
	if err != nil {
		return im.fail(ctx, jobs.KindImport, c.Attempt, err, warnings)
	}
	im.log.Info("import finished", "job_id", c.Attempt.JobID, "batch_id", c.BatchID, "state", out.State,
		"album_id", out.AlbumID, "code", out.ErrorCode, "already_completed", out.AlreadyCompleted)
	return nil
}

// fail completes an attempt as failed with err's code and message (§6.4:
// visible, no automatic retry). A shutdown (the context ended) or a fatal
// database error (§6.4) is returned as it is and completes nothing: the job
// stays running until the next boot puts it back to pending (§11.1 step 5).
func (im *Importer) fail(ctx context.Context, kind jobs.Kind, a jobs.Attempt, err error, warnings []jobs.Warning) error {
	if store.IsFatal(err) || ctx.Err() != nil {
		return err
	}
	code, msg := failure(err)
	im.log.Info("job failed", "job_id", a.JobID, "kind", kind, "code", code)
	if ferr := im.catalog.FailJob(ctx, kind, a, code, msg, warnings); ferr != nil {
		return errors.Join(fmt.Errorf("%s: %w", a, err), ferr)
	}
	return nil
}

// reserveSpace is §11.2 before the copies: the conservative estimate is
// reserved in the process-wide budget, which requires the free space of
// /data minus the 1 GiB margin minus every other job's reservation to cover
// it (NOTES.md N-114). The caller releases the reservation when the job's
// copies are done. Every write still handles ENOSPC.
func (im *Importer) reserveSpace(estimate int64) (*jobs.Reservation, error) {
	fs, err := im.work.StatFS()
	if err != nil {
		return nil, err
	}
	r, avail := im.budget.Reserve(fs.FreeBytes, estimate)
	if r == nil {
		return nil, errorf(CodeInsufficientSpace,
			"the import needs about %d bytes; %d are free, of which %d are kept as a margin and %d are reserved by other jobs in progress",
			estimate, fs.FreeBytes, int64(jobs.SpaceMargin), max(0, fs.FreeBytes-jobs.SpaceMargin-avail))
	}
	return r, nil
}

// testHook, when set by a test, runs at named points of an import so that a
// test can change the source at an exact moment (§12.2). It is nil in
// production.
var testHook func(point string)

func hook(point string) {
	if testHook != nil {
		testHook(point)
	}
}
