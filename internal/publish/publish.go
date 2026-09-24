// Package publish makes a finished build visible in library/ (DESIGN.md
// §9.3) and completes, at boot, a publication a crash interrupted (§9.4).
//
// One publication at a time: publishMu, a process mutex, is taken first,
// then each short catalog transaction (PREPARE, FINALIZE); API mutations
// take the catalog lock only, never publishMu. Under publishMu:
//
//  1. preflight: the staging holds the build and its receipt, the
//     destinations and their observable ownership allow the transition; a
//     conflict fails the job without a journal;
//  2. PREPARE, a transaction: catalog.CheckFresh; a superseded build is
//     requeued and discarded; stale claims are repaired or the render fails
//     with path_reserved (N-112); otherwise the journal row is inserted and
//     the claims reconciled;
//  3. INSTALL, filesystem only: RENAME_NOREPLACE or RENAME_EXCHANGE into
//     library/, the old directory retired to work/retired/<build_id>, the
//     empty artist directory removed, every directory involved fsynced;
//  4. FINALIZE, a transaction: published_* from the journal's values, the
//     job completed by the ticket, the journal deleted, the claims
//     reconciled.
//
// Then, outside the mutex, the old staging and the retired directory are
// removed. Once PREPARE has committed, a failure is never a failed job: the
// journal stays pending and publishing is suspended in this process
// (CodeSuspended); the executor stops the pool (jobs.Stop), the process
// exits, and the next boot's Recover completes the journal forward (§9.4,
// NOTES.md N-135).
//
// The package reads library/ and work/ only through internal/fsops, and the
// catalog only through sqlc queries under store.InCatalogTx.
package publish

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
	"musiclib/internal/jobs"
	"musiclib/internal/render"
	"musiclib/internal/store"
)

// shutdownGrace is how long a prepared publication may still run after the
// process was asked to stop (§11.1: "tenta di concludere entro 30 secondi,
// altrimenti lascia il journal al recovery"). A variable for the tests.
var shutdownGrace = 30 * time.Second

// Config are the publisher's dependencies, passed explicitly (§2.3).
type Config struct {
	DB *pgxpool.Pool
	// Library and Work are /data/library and /data/work: two fsops roots on
	// the same filesystem and mount, as the boot checked (§3.1).
	Library *fsops.Root
	Work    *fsops.Root
	// Builder builds the renders that ExecuteRender publishes, and discards
	// their stagings.
	Builder *render.Builder
	Log     *slog.Logger
	// Failpoints is nil in production. Tests set it to inject an error or
	// crash the process at the protocol's named points (§12.2, NOTES.md
	// N-136, N-142), in order: preflight (checked, before PREPARE),
	// prepared (the journal committed), installed (right after the rename
	// or the exchange, before the retirement and the fsyncs), retired (the
	// old directory moved, before the rmdir and the fsyncs), synced
	// (INSTALL complete, before FINALIZE), finalized (FINALIZE committed,
	// before the release and the cleanup).
	Failpoints failpoint.Hook
}

// Publisher is the process's one publisher. It is safe for concurrent use
// by the pool's workers: every publication runs under publishMu.
type Publisher struct {
	db            *pgxpool.Pool
	library, work *fsops.Root
	builder       *render.Builder
	log           *slog.Logger
	renderVersion string
	failpoints    failpoint.Hook

	// mu is publishMu (§9.3), a channel so that waiting for it respects
	// the caller's context.
	mu chan struct{}
	// suspended, guarded by mu, is the failure that left a journal pending
	// in this process: from then on nothing else is published (§9.4).
	suspended error
}

// New returns the publisher and creates work/retired durably if missing.
func New(cfg Config) (*Publisher, error) {
	if cfg.DB == nil || cfg.Library == nil || cfg.Work == nil || cfg.Builder == nil || cfg.Log == nil {
		return nil, errorf(CodeInvalidArgument, "the publisher needs the database, library, work, the builder and a logger")
	}
	if err := cfg.Work.MkdirAllSync(retiredDir, dirPerm); err != nil {
		return nil, err
	}
	return &Publisher{
		db: cfg.DB, library: cfg.Library, work: cfg.Work, builder: cfg.Builder, log: cfg.Log,
		renderVersion: render.Version, mu: make(chan struct{}, 1), failpoints: cfg.Failpoints,
	}, nil
}

// lock takes publishMu, or gives up when ctx ends.
func (p *Publisher) lock(ctx context.Context) error {
	select {
	case p.mu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return wrap(CodeCanceled, ctx.Err(), "waiting for the publication mutex")
	}
}

func (p *Publisher) unlock() { <-p.mu }

// Report is what a publication did.
type Report struct {
	Outcome Outcome
	// Job is what FINALIZE did to the render job (deleted, or requeued for
	// a newer request); "" unless Outcome is Published.
	Job jobs.RenderOutcome
	// Journal is the journal written, or the one that would have been.
	Journal Journal
}

// Publish publishes res, the build of snap (§9.3), and returns what
// happened:
//
//   - Published: the output is res's, published_* are the journal's, the
//     job was completed by the ticket comparison;
//   - Superseded or Refused: no journal; the job is pending (superseded)
//     or failed with path_reserved (N-112); the build is discarded.
//
// An error before PREPARE's commit that is not fatal (a conflict of the
// preflight, CodeStagingInvalid, CodeStateChanged, a cancellation) left no
// journal: the build is discarded and the caller fails the job before the
// journal (§6.4). Any error after PREPARE, and a fatal store error, is
// marked with jobs.Stop: the journal may be pending, publishing is
// suspended, and the caller must stop.
func (p *Publisher) Publish(ctx context.Context, snap *jobs.RenderSnapshot, res render.Result) (Report, error) {
	j, err := journalFor(snap, res)
	if err != nil {
		return Report{}, err
	}
	if err := p.lock(ctx); err != nil {
		return Report{Journal: j}, err
	}
	rep, discard, err := p.publishLocked(ctx, snap, j)
	p.unlock()
	if discard {
		// §9.3 A: the build is discarded, outside the mutex (§2.2: no
		// recursive removal in the critical section). If the process is
		// stopping, the boot removes it (§11.1 step 5).
		if derr := p.builder.Discard(ctx, j.BuildID); derr != nil {
			p.log.Warn("discarding a build that was not published", "album_id", j.AlbumID, "build_id", j.BuildID,
				"error", derr.Error())
		}
	}
	if err == nil && rep.Outcome == Published {
		p.cleanup(ctx, j)
	}
	return rep, err
}

// publishLocked runs under publishMu. discard says whether the build must
// be discarded (no journal refers to it).
func (p *Publisher) publishLocked(ctx context.Context, snap *jobs.RenderSnapshot, j Journal) (_ Report, discard bool, _ error) {
	rep := Report{Journal: j}
	if p.suspended != nil {
		return rep, false, jobs.Stop(wrap(CodeSuspended, p.suspended, "publishing is suspended in this process"))
	}
	if err := p.preflight(j); err != nil {
		return rep, true, err
	}
	if err := p.failpoints.Hit("preflight"); err != nil {
		return rep, true, err
	}
	out, err := p.prepare(ctx, snap, j)
	switch {
	case store.IsFatal(err) || Code(err) == CodeJournalPending:
		// An uncertain commit may have written the journal; a pending
		// journal must be recovered first (§9.4).
		return rep, false, p.suspend(j, err)
	case err != nil:
		return rep, true, err
	case out != prepared:
		rep.Outcome = out
		p.log.Info("publication not prepared", "album_id", j.AlbumID, "build_id", j.BuildID, "outcome", out)
		return rep, true, nil
	}
	p.log.Info("publication prepared", "album_id", j.AlbumID, "build_id", j.BuildID, "revision", j.Revision,
		"old_path", j.OldPath, "new_path", j.NewPath)
	if err := p.failpoints.Hit("prepared"); err != nil {
		return rep, false, p.suspend(j, err)
	}

	// From here the journal is durable: finish within the shutdown grace,
	// even if ctx is cancelled (§11.1).
	gctx, cancel := graceful(ctx)
	defer cancel()
	if err := p.install(gctx, j); err != nil {
		return rep, false, p.suspend(j, err)
	}
	if err := p.failpoints.Hit("synced"); err != nil {
		return rep, false, p.suspend(j, err)
	}
	jo, err := p.finalize(gctx, j)
	if err != nil {
		return rep, false, p.suspend(j, err)
	}
	if err := p.failpoints.Hit("finalized"); err != nil {
		return rep, false, p.suspend(j, err)
	}
	rep.Outcome, rep.Job = Published, jo
	p.log.Info("album published", "album_id", j.AlbumID, "build_id", j.BuildID, "revision", j.Revision,
		"path", j.NewPath, "job", jo)
	return rep, false, nil
}

// suspend records a failure that may leave the journal pending: nothing
// else is published by this process (§9.4), and the error is marked to
// stop the pool. Called with publishMu held.
func (p *Publisher) suspend(j Journal, err error) error {
	p.suspended = err
	p.log.Error("publication left pending: publishing suspended until the journal is recovered",
		"album_id", j.AlbumID, "build_id", j.BuildID, "code", Code(err), "error", err.Error())
	if store.IsFatal(err) {
		return jobs.Stop(err)
	}
	return jobs.Stop(wrap(CodeSuspended, err, "%s left pending", j))
}

// graceful returns a context that ignores ctx's cancellation for
// shutdownGrace after it, then ends: a prepared publication tries to finish
// within the grace, otherwise its journal is left to the recovery (§11.1).
func graceful(ctx context.Context) (context.Context, context.CancelFunc) {
	gctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	grace := shutdownGrace
	go func() {
		select {
		case <-ctx.Done():
			t := time.NewTimer(grace)
			defer t.Stop()
			select {
			case <-t.C:
				cancel()
			case <-gctx.Done():
			}
		case <-gctx.Done():
		}
	}()
	return gctx, cancel
}
