package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"musiclib/internal/maintenance"
)

// backupPath is the container's fixed backup folder, the mount of
// MUSICLIB_BACKUP. Only the tests use other destinations, through runBackup.
const backupPath = "/backup"

// codeBackupOutside: backup --to names a path that is not below /backup.
const codeBackupOutside = "backup_outside_backup"

// checkBackupDestination refuses a --to that is not a clean absolute path
// below /backup. Anywhere else is the one-off container's own filesystem (its
// tmpfs /tmp, for example), which is lost when the container exits although
// the backup reports success. It is lexical and runs before the volume lock,
// the database or the destination is touched. A clean path has no `.` or
// `..` component and no trailing or double slash, so /backup/../tmp and
// /backup itself (never a new folder) are refused too. maintenance.Backup
// then checks the rest: an existing parent, no symlink on the way (openat2),
// outside /data, a new final name.
func checkBackupDestination(dest string) error {
	if filepath.Clean(dest) == dest && strings.HasPrefix(dest, backupPath+"/") {
		return nil
	}
	return &bootError{code: codeBackupOutside, msg: fmt.Sprintf("backup --to %q: the destination must be a new folder under /backup, written as a plain absolute path such as /backup/2026-09-29-2130; /backup is the backup folder (MUSICLIB_BACKUP), and any other path is inside this one-off container and lost when it exits; nothing was written", dest)}
}

// runBackup writes a new, verified backup directory outside /data (DESIGN.md
// §11.4). Exit 0: the final directory exists; 1: failed, at most a
// .musiclib-backup-*.tmp directory is left for the operator; 2: refused,
// nothing written (a destination that cannot be written included).
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
