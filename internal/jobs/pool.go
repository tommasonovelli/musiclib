package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/store"
)

// MaxWorkers is the upper bound of WORKERS (§6.1).
const MaxWorkers = 16

// pollInterval is the fixed polling period of §6.4. The wake-up signal only
// shortens the wait; correctness never depends on it.
const pollInterval = 2 * time.Second

// Executor runs one claimed job to its end. It must leave the job
// completed, through Finish, FinishRender, RequeueRender or FailRender, or
// return an error. A job it leaves running stays so until the next boot's
// RecoverRunning. It receives the pool's context, cancelled at shutdown.
// An error for which store.IsFatal is true, or one marked with Stop, stops
// the whole pool (§6.4, §9.4).
type Executor func(ctx context.Context, c *Claim) error

// Pool is the worker pool of §6.1: a fixed number of goroutines, each
// claiming and executing one job at a time. There are no nested pools and
// no priorities beyond ClaimNext's fixed order.
type Pool struct {
	db            *pgxpool.Pool
	workers       int
	renderVersion string
	exec          Executor
	log           *slog.Logger
	poll          time.Duration
	wake          chan struct{}
	// claimMu makes the workers claim one at a time. The claim is a
	// REPEATABLE READ transaction (§6.2), and FOR UPDATE SKIP LOCKED skips
	// only rows locked right now: a job that another worker claimed and
	// committed after this snapshot raises 40001 instead. Workers racing
	// for the head of the queue would exhaust the three retries of §6.4
	// (NOTES.md N-110). The claim is a few milliseconds; the work, outside
	// the mutex, stays parallel. The database guarantee of one claim per
	// job does not depend on it.
	claimMu sync.Mutex
}

// NewPool returns a pool of workers goroutines (1..MaxWorkers, §6.1) that
// run exec on the jobs claimed from db. renderVersion goes into the render
// snapshots.
func NewPool(db *pgxpool.Pool, workers int, renderVersion string, exec Executor, log *slog.Logger) (*Pool, error) {
	if workers < 1 || workers > MaxWorkers {
		return nil, errorf(CodeInvalidArgument, "workers = %d, want 1..%d", workers, MaxWorkers)
	}
	if renderVersion == "" || exec == nil || log == nil || db == nil {
		return nil, errorf(CodeInvalidArgument, "the pool needs a database, a render version, an executor and a logger")
	}
	return &Pool{
		db: db, workers: workers, renderVersion: renderVersion, exec: exec, log: log,
		poll: pollInterval, wake: make(chan struct{}, 1),
	}, nil
}

// Wake tells an idle worker to look for work now, instead of at its next
// poll. The catalog calls it after a commit that enqueued something. It
// never blocks, and a lost wake-up only costs latency (§6.4).
func (p *Pool) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run starts the workers and blocks until ctx ends and every worker has
// returned: a job in progress receives the cancellation and Run waits for
// its executor. It returns nil after a cancellation.
//
// A fatal error (store.IsFatal: an uncertain commit or a lost connection,
// §6.4), from a claim or from an executor, or an executor error marked with
// Stop (a publication left pending after PREPARE, §9.4), stops every
// worker; Run then returns it, and the caller stops the process so that
// Docker restarts it.
func (p *Pool) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		once  sync.Once
		fatal error
		wg    sync.WaitGroup
	)
	stop := func(err error) {
		once.Do(func() {
			fatal = err
			cancel()
		})
	}
	for i := range p.workers {
		wg.Go(func() { p.worker(ctx, i, stop) })
	}
	wg.Wait()
	return fatal
}

// worker claims and executes jobs one at a time until ctx ends.
func (p *Pool) worker(ctx context.Context, id int, stop func(error)) {
	log := p.log.With("worker", id)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		busy, err := p.step(ctx, log)
		if Stops(err) {
			log.Error("fatal failure, stopping the workers", "error", err, "code", Code(err))
			stop(err)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Error("job execution failed", "error", err, "code", Code(err))
		}
		if busy {
			continue // there may be more work: claim again at once
		}
		timer.Reset(p.poll)
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-timer.C:
		}
	}
}

// step claims one job and executes it. busy is false when nothing was
// pending.
func (p *Pool) step(ctx context.Context, log *slog.Logger) (busy bool, err error) {
	p.claimMu.Lock()
	c, err := ClaimNext(ctx, p.db, p.renderVersion)
	p.claimMu.Unlock()
	if err != nil || c == nil {
		return false, err
	}
	// Another worker may find the next job while this one works.
	p.Wake()
	attrs := []any{"job_id", c.Attempt.JobID, "kind", c.Kind, "ticket", c.Attempt.Ticket}
	if c.Render != nil {
		attrs = append(attrs, "album_id", c.Render.Album.ID, "revision", c.Render.Album.Revision)
	}
	log.Info("job claimed", attrs...)
	if err := p.exec(ctx, c); err != nil {
		return true, fmt.Errorf("executing %s: %w", c.Attempt, err)
	}
	log.Info("job executed", attrs...)
	return true, nil
}

// Stop marks err, returned by an executor, as one that must stop the whole
// pool like a fatal database error (§6.4): the publisher uses it when a
// publication is left pending after PREPARE, since the journal must be
// resolved before anything else is published (§9.4). The process then
// exits and the next boot recovers the journal (§11.1 step 4).
func Stop(err error) error {
	if err == nil {
		return nil
	}
	return &stopError{err: err}
}

type stopError struct{ err error }

func (e *stopError) Error() string { return e.err.Error() }
func (e *stopError) Unwrap() error { return e.err }

// Stops reports whether err stops the pool: a fatal database error
// (store.IsFatal) or an error marked with Stop.
func Stops(err error) bool {
	var s *stopError
	return store.IsFatal(err) || errors.As(err, &s)
}
