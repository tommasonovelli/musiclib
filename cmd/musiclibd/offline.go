package main

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/unix"

	"musiclib/internal/maintenance"
	"musiclib/internal/volume"
)

// offlineConnectTimeout bounds the connection and the preflight queries of
// an offline command: with the volume lock held it must refuse promptly
// rather than wait silently for PostgreSQL (DESIGN.md §11.3).
const offlineConnectTimeout = 10 * time.Second

// startOffline is the common preamble of doctor, rebuild, backup and restore
// (DESIGN.md §11.1, §11.3): umask 022, never root, configuration from the
// environment and the exclusive non-blocking volume flock. Every error is a
// refusal (exit 2); nothing was written except, at most, the .lock file.
func startOffline(getenv func(string) string, p paths) (Config, *volume.Volume, error) {
	unix.Umask(0o022)
	if err := checkNotRoot(unix.Geteuid()); err != nil {
		return Config{}, nil, err
	}
	cfg, err := loadConfig(getenv, runtime.GOMAXPROCS(0))
	if err != nil {
		return Config{}, nil, err
	}
	v, err := volume.Acquire(p.data)
	if err != nil {
		return Config{}, nil, err
	}
	return cfg, v, nil
}

// connectOffline opens a pool and pings it. It never migrates. pgx errors
// can name hosts or echo parts of the URL, so neither is ever logged (N-064).
func connectOffline(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, &bootError{code: "maintenance_database", msg: "invalid database connection"}
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, &bootError{code: "maintenance_database", msg: "PostgreSQL is unavailable"}
	}
	return pool, nil
}

// closeVolume releases the flock; a close error is logged, never ignored.
func closeVolume(log *slog.Logger, v *volume.Volume) {
	if err := v.Close(); err != nil {
		logFatal(log, err)
	}
}

// operationExit logs a failed maintenance operation with its own stable code
// and returns the exit status (N-221): 2 for a refusal of a precondition,
// including an incomplete or foreign maintenance marker, 1 for an attempted
// operation that failed, logged with the operator's advice.
func operationExit(log *slog.Logger, err error, advice string) int {
	var me *maintenance.Error
	if errors.As(err, &me) && me.Refusal {
		logFatal(log, err)
		return exitUsage
	}
	if c := volume.Code(err); c == volume.CodeMaintenance || c == volume.CodeMaintenanceMalformed {
		logFatal(log, err)
		return exitUsage
	}
	log.Error(err.Error(), "code", codeOf(err), "advice", advice)
	return exitFailure
}
