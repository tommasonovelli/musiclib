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

// runRestore restores a verified backup onto a new, empty database and data
// volume (DESIGN.md §11.4). Exit 0: done; 1: attempted and failed; 2: refused
// before anything but .lock was written.
func runRestore(getenv func(string) string, p paths, from string, stdout io.Writer, log *slog.Logger) int {
	cfg, v, err := startOffline(getenv, p)
	if err != nil {
		logFatal(log, err)
		return exitUsage
	}
	defer closeVolume(log, v)
	ctx, cancel := context.WithTimeout(context.Background(), offlineConnectTimeout)
	defer cancel()
	pool, err := connectOffline(ctx, cfg.DatabaseURL)
	if err != nil {
		logFatal(log, err)
		return exitUsage
	}
	defer pool.Close()
	// No marker or identity checks: Restore refuses any volume entry other
	// than .lock and an empty lost+found, and any database object.
	workCtx, stop := signal.NotifyContext(context.Background(), unix.SIGTERM, unix.SIGINT)
	defer stop()
	if err := maintenance.Restore(workCtx, pool, v, from, cfg.DatabaseURL, nil); err != nil {
		advice := "restore failed before its maintenance marker: nothing was restored; fix the cause and retry on the same empty destinations"
		if v.CheckMaintenance() != nil {
			advice = "restore incomplete; .maintenance blocks server boot: recreate BOTH a new empty database and a new empty data volume and repeat the restore; never remove .maintenance manually"
		}
		return operationExit(log, err, advice)
	}
	if _, err := fmt.Fprintln(stdout, "Restore completed. Start the app to render active albums."); err != nil {
		logFatal(log, &bootError{code: "restore_output", msg: "cannot write report", err: err})
		return exitFailure
	}
	return exitOK
}
