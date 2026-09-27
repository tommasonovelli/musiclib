package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/signal"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"musiclib/internal/maintenance"
)

// runRebuild regenerates only derived state after an explicit confirmation
// of the store id (DESIGN.md §11.3). Exit 0: done; 1: attempted and
// incomplete (the marker blocks boot, repeat the same command); 2: refused.
func runRebuild(getenv func(string) string, p paths, id uuid.UUID, stdout io.Writer, log *slog.Logger) int {
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
	if err := maintenance.RequireCurrentSchema(ctx, pool); err != nil {
		logFatal(log, err)
		return exitUsage
	}
	if _, err := v.IdentifyExisting(ctx, pool); err != nil {
		logFatal(log, err)
		return exitUsage
	}
	if id != v.StoreID() {
		logFatal(log, &bootError{code: "rebuild_store_id", msg: "--store-id must match both settings.store_id and .musiclib-store; no data was deleted"})
		return exitUsage
	}
	// No marker check here: a matching rebuild marker is deliberately reused
	// by Rebuild (repeat after an interruption); a foreign one is refused.
	workCtx, stop := signal.NotifyContext(context.Background(), unix.SIGTERM, unix.SIGINT)
	defer stop()
	if err := maintenance.Rebuild(workCtx, pool, v, id, nil); err != nil {
		return operationExit(log, err, "rebuild incomplete; .maintenance blocks server boot: repeat the same rebuild command with the same store id; never remove the marker manually")
	}
	if _, err := fmt.Fprintln(stdout, "Rebuild prepared. Start the app to render active albums; originals and catalog were kept."); err != nil {
		logFatal(log, &bootError{code: "rebuild_output", msg: "cannot write report", err: err})
		return exitFailure
	}
	return exitOK
}
