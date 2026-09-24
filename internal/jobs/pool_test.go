package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// finishExec completes a render claim, as a publisher's FINALIZE would.
func finishExec(db *fixture) func(ctx context.Context, c *Claim) error {
	return func(ctx context.Context, c *Claim) error {
		return store.InCatalogTx(ctx, db.db, func(tx *store.CatalogTx) error {
			_, err := FinishRender(ctx, tx, c.Attempt)
			return err
		})
	}
}

func startPool(t *testing.T, p *Pool) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			t.Fatal("Run did not return after the cancellation")
			return nil
		}
	}
}

func TestNewPoolArguments(t *testing.T) {
	f := newFixture(t)
	exec := func(context.Context, *Claim) error { return nil }
	for _, tc := range []struct {
		name    string
		workers int
		rv      string
		exec    Executor
		ok      bool
	}{
		{"one", 1, "rv", exec, true},
		{"sixteen", 16, "rv", exec, true},
		{"zero", 0, "rv", exec, false},
		{"seventeen", 17, "rv", exec, false},
		{"no renderer", 1, "", exec, false},
		{"no executor", 1, "rv", nil, false},
	} {
		_, err := NewPool(f.db, tc.workers, tc.rv, tc.exec, discard)
		if (err == nil) != tc.ok || (!tc.ok && Code(err) != CodeInvalidArgument) {
			t.Errorf("%s: err = %v, ok want %v", tc.name, err, tc.ok)
		}
	}
}

// The in-memory signal shortens the wait (§6.4): with a poll of an hour, a
// job enqueued after every worker went idle runs as soon as Wake is called.
func TestPoolWake(t *testing.T) {
	f := newFixture(t)
	const workers = 3
	var idle atomic.Int32
	f.setHook(func(point string) {
		if point == "claim_selected_import" {
			idle.Add(1)
		}
	})
	ran := make(chan uuid.UUID, 1)
	fin := finishExec(f)
	p, err := NewPool(f.db, workers, testRenderer, func(ctx context.Context, c *Claim) error {
		ran <- c.Attempt.JobID
		return fin(ctx, c)
	}, discard)
	if err != nil {
		t.Fatal(err)
	}
	p.failpoints = f.fp.Hook()
	p.poll = time.Hour
	stop := startPool(t, p)
	waitUntil(t, "every worker to find nothing", func() bool { return idle.Load() >= workers })

	e := f.enqueue(f.album("wake"))
	start := time.Now()
	p.Wake()
	select {
	case id := <-ran:
		if id != e.JobID {
			t.Errorf("ran %s, want %s", id, e.JobID)
		}
		t.Logf("latency after Wake: %v", time.Since(start))
	case <-time.After(10 * time.Second):
		t.Fatal("the job did not run after Wake")
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// Without a signal the 2 s poll still finds the work (§6.4: the signal is
// not needed for correctness).
func TestPoolPolls(t *testing.T) {
	f := newFixture(t)
	ran := make(chan struct{}, 1)
	fin := finishExec(f)
	p, err := NewPool(f.db, 1, testRenderer, func(ctx context.Context, c *Claim) error {
		ran <- struct{}{}
		return fin(ctx, c)
	}, discard)
	if err != nil {
		t.Fatal(err)
	}
	if p.poll != 2*time.Second {
		t.Errorf("poll = %v, want 2s (§6.4)", p.poll)
	}
	p.poll = 50 * time.Millisecond
	stop := startPool(t, p)
	f.enqueue(f.album("polled"))
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("the poll never found the job")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// errorCounter is a slog handler counting the records at level Error.
type errorCounter struct{ n *atomic.Int32 }

func (h errorCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h errorCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.n.Add(1)
	}
	return nil
}
func (h errorCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h errorCounter) WithGroup(string) slog.Handler      { return h }

// serializationCounter is a pgx tracer counting the statements that fail
// with a serialization failure (40001).
type serializationCounter struct{ n atomic.Int32 }

func (c *serializationCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (c *serializationCounter) TraceQueryEnd(_ context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	var pgErr *pgconn.PgError
	if errors.As(d.Err, &pgErr) && pgErr.Code == "40001" {
		c.n.Add(1)
	}
}

// Every job is executed exactly once by a pool of the largest size, and the
// workers never make each other's claims fail: no statement of theirs meets
// a serialization failure (N-110).
func TestPoolExecutesEachJobOnce(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(pgtest.EmptyDB(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = MaxWorkers + 8
	var conflicts serializationCounter
	cfg.ConnConfig.Tracer = &conflicts
	db, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, db: db}
	const n = 120
	for range n {
		f.enqueue(f.album("many"))
	}
	var errorsLogged atomic.Int32
	var (
		mu   sync.Mutex
		runs = map[uuid.UUID]int{}
	)
	fin := finishExec(f)
	p, err := NewPool(db, MaxWorkers, testRenderer, func(ctx context.Context, c *Claim) error {
		mu.Lock()
		runs[c.Attempt.JobID]++
		mu.Unlock()
		return fin(ctx, c)
	}, slog.New(errorCounter{&errorsLogged}))
	if err != nil {
		t.Fatal(err)
	}
	p.poll = 20 * time.Millisecond
	stop := startPool(t, p)
	waitUntil(t, "every job to finish", func() bool {
		var left int
		if err := f.db.QueryRow(context.Background(), `SELECT count(*) FROM jobs`).Scan(&left); err != nil {
			t.Fatal(err)
		}
		return left == 0
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if len(runs) != n {
		t.Errorf("%d jobs executed, want %d", len(runs), n)
	}
	for id, k := range runs {
		if k != 1 {
			t.Errorf("job %s executed %d times", id, k)
		}
	}
	if n := errorsLogged.Load(); n != 0 {
		t.Errorf("%d errors logged by the workers, want none", n)
	}
	if n := conflicts.n.Load(); n != 0 {
		t.Errorf("%d serialization failures between the workers' claims, want none", n)
	}
}

// Shutdown (§11.1): the cancellation reaches the job in progress, Run waits
// for it and returns nil, and no goroutine of the pool survives.
func TestPoolShutdown(t *testing.T) {
	f := newFixture(t)
	f.enqueue(f.album("long job"))
	started := make(chan struct{})
	var sawCancel atomic.Bool
	p, err := NewPool(f.db, 3, testRenderer, func(ctx context.Context, c *Claim) error {
		close(started)
		<-ctx.Done()
		sawCancel.Store(true)
		return ctx.Err()
	}, discard)
	if err != nil {
		t.Fatal(err)
	}
	p.poll = 10 * time.Millisecond
	before := runtime.NumGoroutine()
	stop := startPool(t, p)
	<-started
	if err := stop(); err != nil {
		t.Fatalf("Run after a cancellation = %v, want nil", err)
	}
	if !sawCancel.Load() {
		t.Error("the executor returned before seeing the cancellation")
	}
	waitUntil(t, "the pool's goroutines to exit", func() bool { return runtime.NumGoroutine() <= before })
}

// A fatal database error (§6.4) from one executor stops every worker: the
// others see the cancellation, and Run returns the fatal error.
func TestPoolFatalStopsEveryWorker(t *testing.T) {
	f := newFixture(t)
	const workers = 3
	for range workers {
		f.enqueue(f.album("fatal"))
	}
	var started, canceled atomic.Int32
	blocked := make(chan struct{}, workers)
	fatal := &store.Error{Code: store.CodeCommitUncertain, Msg: "injected"}
	p, err := NewPool(f.db, workers, testRenderer, func(ctx context.Context, c *Claim) error {
		if started.Add(1) < workers {
			blocked <- struct{}{}
			<-ctx.Done()
			canceled.Add(1)
			return ctx.Err()
		}
		for range workers - 1 {
			<-blocked
		}
		return fatal
	}, discard)
	if err != nil {
		t.Fatal(err)
	}
	p.poll = 10 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- p.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, fatal) || !store.IsFatal(err) || Code(err) != store.CodeCommitUncertain {
			t.Errorf("Run = %v, want the fatal error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop on a fatal error")
	}
	if n := canceled.Load(); n != workers-1 {
		t.Errorf("%d executors cancelled, want %d", n, workers-1)
	}
}

// An executor error marked with Stop (a publication left pending, §9.4)
// stops the pool like a fatal one; the same error unmarked does not.
func TestPoolStop(t *testing.T) {
	f := newFixture(t)
	f.enqueue(f.album("plain"))
	f.enqueue(f.album("stop"))
	cause := &Error{Code: "publish_suspended", Msg: "injected"}
	var runs atomic.Int32
	p, err := NewPool(f.db, 1, testRenderer, func(ctx context.Context, c *Claim) error {
		if runs.Add(1) == 1 {
			return cause // an ordinary failure: the worker goes on
		}
		return Stop(cause)
	}, discard)
	if err != nil {
		t.Fatal(err)
	}
	p.poll = 10 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- p.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, cause) || !Stops(err) || store.IsFatal(err) || Code(err) != "publish_suspended" {
			t.Errorf("Run = %v, want the stop error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop on a Stop error")
	}
	if n := runs.Load(); n != 2 {
		t.Errorf("%d executions, want 2: the unmarked error must not stop the pool", n)
	}
	if Stops(cause) || Stop(nil) != nil {
		t.Error("an unmarked error stops the pool, or Stop(nil) is not nil")
	}
}

// A fatal error of the claim itself stops the pool too.
func TestPoolFatalClaim(t *testing.T) {
	f := newFixture(t)
	p, err := NewPool(f.db, 2, testRenderer, func(context.Context, *Claim) error { return nil }, discard)
	if err != nil {
		t.Fatal(err)
	}
	p.poll = 10 * time.Millisecond
	f.db.Close() // every later acquire fails: a lost database (§6.4)
	done := make(chan error, 1)
	go func() { done <- p.Run(context.Background()) }()
	select {
	case err := <-done:
		if Code(err) != store.CodeConnectionLost {
			t.Errorf("Run = %v, want %s", err, store.CodeConnectionLost)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop on a lost database")
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
