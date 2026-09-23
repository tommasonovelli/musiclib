package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/blobstore"
	"musiclib/internal/fsops"
	"musiclib/internal/importer"
	"musiclib/internal/media"
	"musiclib/internal/store"
	"musiclib/internal/volume"
)

const (
	// dbRetryFirst and dbRetryMax bound the backoff while waiting for
	// PostgreSQL (§11.1 step 2); dbPingTimeout bounds one attempt.
	dbRetryFirst  = 250 * time.Millisecond
	dbRetryMax    = 5 * time.Second
	dbPingTimeout = 5 * time.Second
	// httpShutdownTimeout bounds the graceful close of the HTTP server;
	// requests still running after it are cut.
	httpShutdownTimeout = 10 * time.Second
)

// daemon is the state of one run of the server. Its fields are filled by
// the boot steps, in order, and released by shutdown in reverse order, the
// volume lock last.
type daemon struct {
	cfg   Config
	paths paths
	log   *slog.Logger

	srv    *http.Server
	srvErr chan error // the error of srv.Serve, if it stops by itself

	vol  *volume.Volume
	pool *pgxpool.Pool

	// runner is the one Runner of the process: every native tool runs
	// through it, bounded by WORKERS (§6.1). tools are the verified ffmpeg,
	// ffprobe and musiclib-tags; their versions feed render_version (§2.1,
	// N-010).
	runner *media.Runner
	tools  *media.Tools

	// ready holds the pool once the boot is complete, nil before and during
	// shutdown: /health/ready is positive only while it is set (§11.1).
	ready atomic.Pointer[pgxpool.Pool]
}

// run boots the server on ln, serves until ctx is cancelled, then shuts
// down. A nil error means a normal termination, including a cancellation
// that interrupts the boot; any other error is fatal.
func run(ctx context.Context, cfg Config, p paths, ln net.Listener, log *slog.Logger) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d := &daemon{cfg: cfg, paths: p, log: log, srvErr: make(chan error, 1)}
	d.startHTTP(ln, cancel)
	defer func() { err = errors.Join(err, d.shutdown()) }()

	if err := d.boot(ctx); err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return d.serveErr()
		}
		return err
	}
	log.Info("ready", "store_id", d.vol.StoreID().String())
	<-ctx.Done()
	return d.serveErr()
}

// boot runs the startup sequence of §11.1, one small step per line. The
// steps that belong to later phases are marked where they will go.
func (d *daemon) boot(ctx context.Context) error {
	// Step 1: HTTP already answers, with negative readiness; take the lock.
	if err := d.acquireVolume(); err != nil {
		return err
	}
	// §11.3: an incomplete rebuild or restore blocks everything, before
	// anything is written, migrations included (N-065).
	if err := d.vol.CheckMaintenance(); err != nil {
		return err
	}

	// Step 2: wait for PostgreSQL, then migrate, before any worker.
	if err := d.connectDB(ctx); err != nil {
		return err
	}
	if err := store.Migrate(ctx, d.pool); err != nil {
		return err
	}

	// Step 3: volume identity, root, permissions, st_dev, primitives.
	if err := d.identifyVolume(ctx); err != nil {
		return err
	}
	if err := d.vol.OpenLayout(); err != nil {
		return err
	}
	if err := d.vol.CheckFilesystem(); err != nil {
		return err
	}
	if err := d.checkTools(ctx); err != nil {
		return err
	}
	if err := checkImport(d.paths.imports); err != nil {
		return err
	}

	// Step 4, journal recovery (§9.4): Phase 2, internal/publish.

	// Step 5: clean what work/ no longer needs. Recovering running jobs
	// (running -> pending, §6.4) comes with internal/jobs in Phase 2, and so
	// does the cleanup of work/render and work/retired, which depends on
	// the journal.
	if err := d.cleanWork(ctx); err != nil {
		return err
	}

	// Step 6, enqueue of renders with a stale renderer: Phase 2,
	// internal/jobs.

	// Step 7: there is no worker pool yet (Phase 2); readiness turns
	// positive.
	d.ready.Store(d.pool)
	return nil
}

// acquireVolume takes the exclusive flock on /data/.lock (§2.2).
func (d *daemon) acquireVolume() error {
	v, err := volume.Acquire(d.paths.data)
	if err != nil {
		return err
	}
	d.vol = v
	d.log.Info("volume lock acquired", "lock", volume.LockFile)
	return nil
}

// connectDB creates the pool (WORKERS + 8 connections, §11.1) and waits,
// retrying with backoff, until PostgreSQL answers or ctx is cancelled.
func (d *daemon) connectDB(ctx context.Context) error {
	pool, err := store.NewPool(ctx, d.cfg.DatabaseURL, d.cfg.Workers)
	if err != nil {
		// DATABASE_URL was validated at startup; the error text is withheld
		// for the same reason (N-064).
		return &bootError{code: codeConfig, msg: "cannot configure the database pool"}
	}
	d.pool = pool
	delay := dbRetryFirst
	for attempt := 1; ; attempt++ {
		pctx, cancel := context.WithTimeout(ctx, dbPingTimeout)
		err := pool.Ping(pctx)
		cancel()
		if err == nil {
			d.log.Info("database reachable", "attempts", attempt)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// pgx connection errors name host, user and database, never the
		// password.
		d.log.Warn("database not reachable yet", "attempt", attempt,
			"retry_in", delay.String(), "error", err.Error())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, dbRetryMax)
	}
}

// identifyVolume pairs the volume with the database (§2.2, §11.1).
func (d *daemon) identifyVolume(ctx context.Context) error {
	id, err := d.vol.Identify(ctx, d.pool)
	if err != nil {
		return err
	}
	d.log.Info("volume identified", "store_id", id.String())
	return nil
}

// checkTools creates the process's tool Runner, with WORKERS slots (§6.1),
// and verifies that ffmpeg, ffprobe and the TagLib helper musiclib-tags are
// present and at the pinned versions (§2.1, §11.1 step 3). A missing or
// different tool is fatal: media_tool_unavailable or media_tool_version.
func (d *daemon) checkTools(ctx context.Context) error {
	d.runner = media.NewRunner(d.cfg.Workers)
	tools, err := media.NewTools(ctx, d.runner, d.paths.ffmpeg, d.paths.ffprobe, d.paths.tags)
	if err != nil {
		return err
	}
	d.tools = tools
	v := tools.Versions()
	d.log.Info("media tools verified", "ffmpeg", v.FFmpeg, "ffprobe", v.FFprobe,
		"musiclib_tags", v.Tags, "taglib", v.TagLib)
	return nil
}

// checkImport verifies that /import is a directory the process can list
// (§7.1, §11.1 step 3). It is mounted read-only; nothing writes to it.
func checkImport(path string) error {
	r, err := fsops.OpenRoot(path)
	if err != nil {
		return &bootError{code: codeImport, msg: "cannot open the import source", err: err}
	}
	if err := r.CheckAccess(false); err != nil {
		return errors.Join(&bootError{code: codeImport, msg: "cannot read the import source", err: err}, r.Close())
	}
	if err := r.Close(); err != nil {
		return &bootError{code: codeImport, msg: "cannot close the import source", err: err}
	}
	return nil
}

// cleanWork runs the part of §11.1 step 5 that exists today: temporaries of
// interrupted blob puts (N-048), directories of an interrupted boot probe
// (N-033) and the importer's work/import. It runs after the probe of step 3
// and before anything can start a put, so nothing it removes can be in use.
func (d *daemon) cleanWork(ctx context.Context) error {
	blobs, err := blobstore.New(d.vol.Originals(), d.vol.Work())
	if err != nil {
		return err
	}
	temps, err := blobs.CleanTemps(ctx)
	if err != nil {
		return err
	}
	probes, err := fsops.RemoveProbeLeftovers(ctx, d.vol.Work())
	if err != nil {
		return err
	}
	// Pictures extracted by an interrupted import (work/import).
	if err := importer.CleanWork(ctx, d.vol.Work()); err != nil {
		return err
	}
	if len(temps) > 0 || len(probes) > 0 {
		d.log.Info("work cleaned", "blob_temporaries", len(temps), "probe_directories", len(probes))
	}
	return nil
}

// startHTTP serves the health endpoints on ln from the first moment of the
// boot (§11.1 step 1). If the server stops by itself, the run is cancelled.
func (d *daemon) startHTTP(ln net.Listener, cancel context.CancelFunc) {
	d.srv = &http.Server{
		Handler:           d.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(d.log.Handler(), slog.LevelWarn),
	}
	d.log.Info("http listening", "addr", ln.Addr().String())
	go func() {
		if err := d.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			d.srvErr <- &bootError{code: codeHTTP, msg: "the HTTP server stopped", err: err}
			cancel()
		}
	}()
}

// serveErr is the error of an HTTP server that stopped by itself, if any.
func (d *daemon) serveErr() error {
	select {
	case err := <-d.srvErr:
		return err
	default:
		return nil
	}
}

// shutdown releases what the boot acquired, in this order (§11.1):
//
//  1. readiness turns negative;
//  2. the HTTP server stops accepting and finishes its requests (the
//     mutations of later phases come through it);
//  3. (Phase 2: stop claims, cancel builds, end child processes, give a
//     prepared publication up to 30 s; the run context is already
//     cancelled, N-031);
//  4. the database pool is closed;
//  5. the volume's roots are closed and the flock is released, last:
//     nothing can publish any more.
func (d *daemon) shutdown() error {
	d.ready.Store(nil)
	var errs []error

	ctx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := d.srv.Shutdown(ctx); err != nil {
		errs = append(errs, &bootError{code: codeHTTP, msg: "graceful HTTP shutdown", err: errors.Join(err, d.srv.Close())})
	}
	d.log.Info("http server stopped")

	if d.pool != nil {
		d.pool.Close()
		d.log.Info("database pool closed")
	}
	if d.vol != nil {
		if err := d.vol.Close(); err != nil {
			errs = append(errs, err)
		}
		d.log.Info("volume lock released", "lock", volume.LockFile)
	}
	return errors.Join(errs...)
}
