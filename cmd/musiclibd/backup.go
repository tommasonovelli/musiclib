package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/signal"

	"golang.org/x/sys/unix"

	"musiclib/internal/maintenance"
)

// runBackup writes a new, verified backup directory outside /data (DESIGN.md
// §11.4). Exit 0: the final directory exists; 1: failed, at most a
// .musiclib-backup-*.tmp directory is left for the operator; 2: refused.
func runBackup(getenv func(string) string, p paths, dest string, stdout io.Writer, log *slog.Logger) int {
	cfg, v, err := startOffline(getenv, p)
	if err != nil {
		logFatal(log, err)
		return exitUsage
	}
	defer closeVolume(log, v)
	if err := v.CheckMaintenance(); err != nil {
		logFatal(log, err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), offlineConnectTimeout)
	defer cancel()
	pool, err := connectOffline(ctx, cfg.DatabaseURL)
	if err != nil {
		logFatal(log, err)
		return exitUsage
	}
	defer pool.Close()
	if err := maintenance.RequireCurrentSchema(ctx, pool); err != nil {
		logFatal(log, err)
		return exitUsage
	}
	if _, err := v.IdentifyExisting(ctx, pool); err != nil {
		logFatal(log, err)
		return exitUsage
	}
	if err := v.OpenExistingLayout(); err != nil {
		logFatal(log, err)
		return exitUsage
	}
	workCtx, stop := signal.NotifyContext(context.Background(), unix.SIGTERM, unix.SIGINT)
	defer stop()
	if err := maintenance.Backup(workCtx, pool, v, dest, cfg.DatabaseURL, nil); err != nil {
		return operationExit(log, err, "backup incomplete; only the named final directory is a usable backup: inspect and remove any .musiclib-backup-*.tmp directory next to it, then retry")
	}
	if _, err := fmt.Fprintf(stdout, "Backup completed: %s\n", dest); err != nil {
		logFatal(log, &bootError{code: "backup_output", msg: "cannot write report", err: err})
		return exitFailure
	}
	return exitOK
}
