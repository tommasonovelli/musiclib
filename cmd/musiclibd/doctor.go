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

// runDoctor holds the same exclusive flock as the server, but never runs its
// boot (which creates directories, recovers the journal and starts workers).
// Exit 0: no error findings; 1: damage, or an inspection that could not be
// completed; 2: refusal or usage (DESIGN.md §11.3, N-217, N-221).
func runDoctor(getenv func(string) string, p paths, deep bool, stdout io.Writer, log *slog.Logger) int {
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
	// A read-only inspection must not apply migrations: even the goose
	// version table is a write on a new database.
	ctx, cancel := context.WithTimeout(context.Background(), offlineConnectTimeout)
	defer cancel()
	pool, err := connectOffline(ctx, cfg.DatabaseURL)
	if err != nil {
		logFatal(log, err)
		return exitUsage
	}
	defer pool.Close()
	// The schema comes first, as in backup and rebuild: the identity is read
	// through sqlc queries generated for the current schema.
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
	// Hashing a library may take hours: the connection timeout does not
	// apply to the inspection itself; SIGTERM/SIGINT cancel it (§13.2).
	workCtx, stop := signal.NotifyContext(context.Background(), unix.SIGTERM, unix.SIGINT)
	defer stop()
	report, err := maintenance.Doctor(workCtx, pool, v, deep)
	if err != nil {
		logFatal(log, &bootError{code: "doctor_failed", msg: "could not complete the inspection; nothing was changed", err: err})
		return exitFailure
	}
	for _, f := range report.Findings {
		if _, err := fmt.Fprintf(stdout, "%s %s %s: %s\n", f.Severity, f.Code, f.Entity, f.Advice); err != nil {
			logFatal(log, &bootError{code: "doctor_output", msg: "cannot write report", err: err})
			return exitFailure
		}
		log.Info("doctor finding", "severity", f.Severity, "code", f.Code, "entity", f.Entity)
	}
	summary := "Doctor complete: no damage found. Nothing was changed."
	status := exitOK
	if report.HasErrors() {
		summary = "Doctor found damaged or inconsistent data. Nothing was changed."
		status = exitFailure
	}
	if _, err := fmt.Fprintln(stdout, summary); err != nil {
		logFatal(log, &bootError{code: "doctor_output", msg: "cannot write report", err: err})
		return exitFailure
	}
	return status
}
