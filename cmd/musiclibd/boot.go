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
	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	apihttp "musiclib/internal/http"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/publish"
	"musiclib/internal/render"
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
	// reportPurgeInterval is how often the running server applies the
	// retention of the import reports (§6.4, N-200), besides the boot.
	reportPurgeInterval = 24 * time.Hour
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

	// api serves /api from step 1: 503 until the end of the boot, then
	// the catalog (§10). apiFatal receives the first fatal database error
	// an API request met (§6.4), which ends the run like the pool's.
	api      *apihttp.API
	apiFatal chan error

	vol  *volume.Volume
	pool *pgxpool.Pool

	// runner is the one Runner of the process: every native tool runs
	// through it, bounded by WORKERS (§6.1). tools are the verified ffmpeg,
	// ffprobe and musiclib-tags; their versions feed render_version (§2.1,
	// N-010).
	runner *media.Runner
	tools  *media.Tools
	blobs  *blobstore.Store

	// The components of steps 4 to 7, built after the tool check: the one
	// space budget (§11.2), the builder, the publisher (§9.3), the catalog,
	// the importer on /import, and the worker pool.
	budget    *jobs.Budget
	publisher *publish.Publisher
	catalog   *catalog.Service
	source    *fsops.Root
	importer  *importer.Importer
	workers   *jobs.Pool
	// stopWorkers cancels the pool's context; poolDone receives the result
	// of its Run once every worker has returned.
	stopWorkers context.CancelFunc
	poolDone    chan error

	// ready holds the pool once the boot is complete, nil before and during
	// shutdown: /health/ready is positive only while it is set (§11.1).
	ready atomic.Pointer[pgxpool.Pool]
	// suspended is set when the boot found the journal in an illegal state
	// (§9.4, N-135): the process stays alive, holding the lock, with no
	// worker, and /health/ready answers this error.
	suspended atomic.Pointer[errorBody]
}

// run boots the server on ln, serves until ctx is cancelled, then shuts
// down. A nil error means a normal termination, including a cancellation
// that interrupts the boot; any other error is fatal.
func run(ctx context.Context, cfg Config, p paths, ln net.Listener, log *slog.Logger) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d := &daemon{cfg: cfg, paths: p, log: log, srvErr: make(chan error, 1), apiFatal: make(chan error, 1)}
	if err := d.startHTTP(ln, cancel); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.shutdown()) }()

	if err := d.boot(ctx); err != nil {
		if errors.Is(err, errPublishingSuspended) {
			// §9.4: suspended, not stopped. Serve the error until SIGTERM;
			// the shutdown releases everything as usual.
			<-ctx.Done()
			return d.serveErr()
		}
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return d.serveErr()
		}
		return err
	}
	log.Info("ready", "store_id", d.vol.StoreID().String())
	purge := time.NewTicker(reportPurgeInterval)
	defer purge.Stop()
	for {
		select {
		case <-ctx.Done():
			return d.serveErr()
		case <-purge.C:
			// §6.4 retention, once a day while the server runs (N-200).
			if err := d.purgeReports(ctx); err != nil {
				if store.IsFatal(err) {
					log.Error("fatal failure while purging the import reports, stopping", "code", codeOf(err))
					return err
				}
				log.Warn("purging the import reports failed; retried in a day", "code", codeOf(err), "error", err.Error())
			}
		case err := <-d.apiFatal:
			// §6.4: an API request lost the database or could not learn the
			// outcome of its commit. The process exits non-zero so that
			// Docker restarts it.
			log.Error("fatal failure in an API request, stopping", "code", codeOf(err))
			return err
		case err := <-d.poolDone:
			// §6.4: an uncertain commit, a lost database or a publication left
			// pending stopped the workers. The process exits non-zero so that
			// Docker restarts it; the next boot recovers (N-070, N-135).
			// Every worker has returned; the shutdown must not report the
			// error a second time.
			d.poolDone = make(chan error, 1)
			d.poolDone <- nil
			if err == nil {
				err = &bootError{code: codeWorkers, msg: "the worker pool stopped by itself"}
			}
			return err
		}
	}
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
	if err := d.checkImport(); err != nil {
		return err
	}

	if err := d.buildPublisher(); err != nil {
		return err
	}

	// Step 4: the pending publication, if any, completed forward (§9.4).
	if err := d.recoverJournal(ctx); err != nil {
		return err
	}

	// Step 5: clean what work/ no longer needs, then running -> pending
	// (§6.4).
	if err := d.cleanWork(ctx); err != nil {
		return err
	}
	if err := d.recoverRunning(ctx); err != nil {
		return err
	}
	// The import reports past their retention (§6.4, N-200), now that
	// no job is running.
	if err := d.buildCatalog(); err != nil {
		return err
	}
	if err := d.purgeReports(ctx); err != nil {
		return err
	}

	// Step 6: renders of active albums with a stale renderer and no job.
	if err := d.enqueueStale(ctx); err != nil {
		return err
	}

	// Step 7: start the pool, then readiness turns positive.
	if err := d.startWorkers(ctx); err != nil {
		return err
	}
	d.ready.Store(d.pool)
	d.api.Enable(apihttp.Backend{Catalog: d.catalog, Blobs: d.blobs, Budget: d.budget, Work: d.vol.Work(), Source: d.source})
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
	// render_version names these versions and the Go toolchain: the process
	// must be the renderer it claims to be (§2.1, N-130).
	if err := render.CheckTools(v); err != nil {
		return err
	}
	d.log.Info("media tools verified", "ffmpeg", v.FFmpeg, "ffprobe", v.FFprobe,
		"musiclib_tags", v.Tags, "taglib", v.TagLib, "render_version", render.Version)
	return nil
}

// checkImport verifies that /import is a directory the process can list
// (§7.1, §11.1 step 3), and that it is not the data volume: "La root non
// può essere /data" (§7.1). /import must not be the same directory (st_dev
// and st_ino) as /data or one of its media directories, which a mount of
// the data volume, or of one of its directories, at /import would be
// (N-197). It is mounted read-only; nothing writes to it.
func (d *daemon) checkImport() error {
	r, err := fsops.OpenRoot(d.paths.imports)
	if err != nil {
		return &bootError{code: codeImport, msg: "cannot open the import source", err: err}
	}
	err = r.CheckAccess(false)
	if err != nil {
		err = &bootError{code: codeImport, msg: "cannot read the import source", err: err}
	} else {
		err = d.importIsNotData(r)
	}
	if cerr := r.Close(); cerr != nil {
		err = errors.Join(err, &bootError{code: codeImport, msg: "cannot close the import source", err: cerr})
	}
	return err
}

// importIsNotData compares the identity of /import with those of /data,
// originals/, library/ and work/.
func (d *daemon) importIsNotData(imports *fsops.Root) error {
	in, err := imports.Stat("")
	if err != nil {
		return &bootError{code: codeImport, msg: "cannot describe the import source", err: err}
	}
	for _, r := range []*fsops.Root{d.vol.Root(), d.vol.Originals(), d.vol.Library(), d.vol.Work()} {
		fi, err := r.Stat("")
		if err != nil {
			return &bootError{code: codeImport, msg: "cannot describe a data directory to compare it with the import source", err: err}
		}
		if fi.Dev == in.Dev && fi.Ino == in.Ino {
			return &bootError{code: codeImportIsData,
				msg: "/import is the data volume or one of its directories: mount the collection to import there (DESIGN.md §7.1)"}
		}
	}
	return nil
}

// buildCatalog builds the catalog service (§13.2), whose commits wake the
// pool (§6.4) once it runs.
func (d *daemon) buildCatalog() error {
	var err error
	d.catalog, err = catalog.New(d.pool, func() { d.workers.Wake() }, importer.CoverFits, importer.GenreFits)
	return err
}

// purgeReports is the retention of §6.4: the import batches whose
// outcomes are older than catalog.ReportRetentionDays, and that have no
// job left to run, are deleted with their jobs (N-200). It runs at boot
// and then once a day (run).
func (d *daemon) purgeReports(ctx context.Context) error {
	n, err := d.catalog.PurgeImportReports(ctx)
	if err != nil {
		return err
	}
	d.log.Info("import reports purged", "batches", n, "retention_days", catalog.ReportRetentionDays)
	return nil
}

// cleanWork is the cleanup of §11.1 step 5: temporaries of interrupted blob
// puts (N-048), directories of an interrupted boot probe (N-033), the
// importer's work/import, and the builds and retired directories no
// journal references (work/render, work/retired). It runs after the probe
// of step 3 and the recovery of step 4, and before any worker, so nothing
// it removes can be in use. The space budget starts empty after it
// (§11.2).
func (d *daemon) cleanWork(ctx context.Context) error {
	temps, err := d.blobs.CleanTemps(ctx)
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
	builds, err := d.publisher.CleanWork(ctx)
	if err != nil {
		return err
	}
	d.log.Info("work cleaned", "blob_temporaries", len(temps), "probe_directories", len(probes),
		"builds_and_retired", len(builds))
	return nil
}

// startHTTP serves the health endpoints and /api on ln from the first
// moment of the boot (§11.1 step 1); /api answers 503 not_ready until the
// end of the boot. If the server stops by itself, the run is cancelled.
func (d *daemon) startHTTP(ln net.Listener, cancel context.CancelFunc) error {
	api, err := apihttp.New(apihttp.Config{
		PublicOrigin:  d.cfg.PublicOrigin,
		RenderVersion: render.Version,
		Fatal:         d.reportAPIFatal,
		Log:           d.log,
	})
	if err != nil {
		return errors.Join(&bootError{code: codeConfig, msg: "cannot build the API", err: err}, ln.Close())
	}
	d.api = api
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
	return nil
}

// reportAPIFatal hands the first fatal database error of an API request to
// run (§6.4); later ones add nothing.
func (d *daemon) reportAPIFatal(err error) {
	select {
	case d.apiFatal <- err:
	default:
	}
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
//  1. readiness turns negative and /api answers 503 shutting_down: no
//     mutation is accepted any more (§11.1);
//  2. the HTTP server stops accepting and finishes its requests (the
//     mutations of later phases come through it);
//  3. the workers stop claiming and their builds are cancelled, which kills
//     the child processes through the Runner (§6.1, N-031); a publication
//     already prepared gets up to 30 s to finish, then its journal is left
//     to the recovery (internal/publish); the shutdown waits for every
//     worker;
//  4. the database pool is closed;
//  5. the volume's roots are closed and the flock is released, last:
//     nothing can publish any more.
func (d *daemon) shutdown() error {
	d.ready.Store(nil)
	var errs []error
	if d.api != nil {
		// §11.1: no mutation is accepted from now on; requests already
		// running finish within the HTTP shutdown.
		d.api.Disable(apihttp.CodeShuttingDown, "the server is shutting down")
	}
	if d.stopWorkers != nil {
		d.stopWorkers() // no claim from now on; builds cancelled
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := d.srv.Shutdown(ctx); err != nil {
		errs = append(errs, &bootError{code: codeHTTP, msg: "graceful HTTP shutdown", err: errors.Join(err, d.srv.Close())})
	}
	d.log.Info("http server stopped")

	if d.poolDone != nil {
		if err := <-d.poolDone; err != nil {
			errs = append(errs, err)
		}
		d.log.Info("workers stopped")
	}
	if d.source != nil {
		if err := d.source.Close(); err != nil {
			errs = append(errs, err)
		}
	}

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

// buildPublisher builds what steps 4 to 7 need: the blob store, the
// process's one space budget (§11.2: empty at every start, N-114), the
// builder and the publisher (§9.3).
func (d *daemon) buildPublisher() error {
	var err error
	if d.blobs, err = blobstore.New(d.vol.Originals(), d.vol.Work()); err != nil {
		return err
	}
	d.budget = jobs.NewBudget()
	builder, err := render.New(render.Config{Tools: d.tools, Blobs: d.blobs, Work: d.vol.Work(), Budget: d.budget})
	if err != nil {
		return err
	}
	d.publisher, err = publish.New(publish.Config{DB: d.pool, Library: d.vol.Library(), Work: d.vol.Work(),
		Builder: builder, Log: d.log})
	return err
}

// errPublishingSuspended ends the boot after step 4 without an exit: the
// journal is in an illegal state (N-135).
var errPublishingSuspended = errors.New("publishing suspended: the pending publication is in an illegal state")

// recoverJournal is §11.1 step 4 (§9.4): no worker starts before the
// pending publication is completed. A state that matches no legal
// transition suspends publishing (§9.4: "la pubblicazione viene sospesa e
// l'errore esposto", owner decision N-135): nothing is deleted, the boot
// stops before step 5, the process stays alive with the lock held and
// /health/ready answers the error; it returns errPublishingSuspended.
// Any other error (publish_io, the database) fails the boot: it may be
// transient, and the restart retries the recovery.
func (d *daemon) recoverJournal(ctx context.Context) error {
	j, err := d.publisher.Recover(ctx)
	if publish.Code(err) == publish.CodeIllegalState {
		return d.suspendPublishing(j, err)
	}
	if err != nil {
		return err
	}
	if j == nil {
		d.log.Info("no pending publication")
		return nil
	}
	d.log.Info("journal recovered", "album_id", j.AlbumID, "build_id", j.BuildID, "revision", j.Revision)
	return nil
}

// suspendPublishing records the illegal journal for /health/ready and
// logs what the operator must do (§9.4, §11.3). The details name the
// album and the build, never a path of the host (§10.1).
func (d *daemon) suspendPublishing(j *publish.Journal, err error) error {
	body := &errorBody{Code: publish.CodeIllegalState,
		Message: "the pending publication matches no legal transition: publishing is suspended " +
			"until library/ is corrected by hand and the app restarted, or the app is stopped and rebuild is run"}
	attrs := []any{"code", publish.CodeIllegalState, "error", err.Error(),
		"action", "compare library/ and work/ with the journal; put back by hand what was moved and restart the app " +
			"to run the recovery again, or stop the app and run rebuild (DESIGN.md §9.4, §11.3). Nothing was deleted."}
	if j != nil {
		body.Details = map[string]string{"album_id": j.AlbumID.String(), "build_id": j.BuildID.String()}
		attrs = append(attrs, "album_id", j.AlbumID, "build_id", j.BuildID, "old_path", j.OldPath, "new_path", j.NewPath)
	}
	d.suspended.Store(body)
	d.api.Disable(publish.CodeIllegalState, "publishing is suspended: the pending publication is in an illegal state "+
		"(see /health/ready and the server log)")
	d.log.Error("publishing suspended: the pending publication is in an illegal state", attrs...)
	return errPublishingSuspended
}

// recoverRunning is the second half of §11.1 step 5: no attempt of the
// previous process survives (§6.4).
func (d *daemon) recoverRunning(ctx context.Context) error {
	n, err := jobs.RecoverRunning(ctx, d.pool)
	if err != nil {
		return err
	}
	d.log.Info("running jobs recovered", "jobs", n)
	return nil
}

// enqueueStale is §11.1 step 6: renders of the active albums whose
// published renderer is not render.Version and that have no job; a failed
// job stays as it is.
func (d *daemon) enqueueStale(ctx context.Context) error {
	n, err := jobs.EnqueueStaleRenders(ctx, d.pool, render.Version)
	if err != nil {
		return err
	}
	d.log.Info("stale renders enqueued", "albums", n, "render_version", render.Version)
	return nil
}

// startWorkers is §11.1 step 7: the catalog (whose commits wake the pool,
// §6.4), the importer on /import, and the pool of WORKERS workers
// executing scans, imports and renders (§6.1). It runs until shutdown or a
// fatal error (§6.4), which run turns into a non-zero exit.
func (d *daemon) startWorkers(ctx context.Context) error {
	var err error
	if d.source, err = fsops.OpenRoot(d.paths.imports); err != nil {
		return &bootError{code: codeImport, msg: "cannot open the import source", err: err}
	}
	d.importer, err = importer.New(importer.Config{Catalog: d.catalog, Tools: d.tools, Blobs: d.blobs,
		Source: d.source, Work: d.vol.Work(), Budget: d.budget, Log: d.log})
	if err != nil {
		return err
	}
	d.workers, err = jobs.NewPool(d.pool, d.cfg.Workers, render.Version, d.execute, d.log)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithCancel(ctx)
	d.stopWorkers, d.poolDone = cancel, make(chan error, 1)
	go func() { d.poolDone <- d.workers.Run(wctx) }()
	d.log.Info("workers started", "workers", d.cfg.Workers)
	return nil
}

// execute dispatches a claimed job to its executor.
func (d *daemon) execute(ctx context.Context, c *jobs.Claim) error {
	switch c.Kind {
	case jobs.KindScan:
		return d.importer.ExecuteScan(ctx, c)
	case jobs.KindImport:
		return d.importer.ExecuteImport(ctx, c)
	case jobs.KindRender:
		return d.publisher.ExecuteRender(ctx, c)
	}
	return &bootError{code: codeWorkers, msg: "a job of unknown kind " + string(c.Kind)}
}
