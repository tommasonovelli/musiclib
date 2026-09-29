package volume

import (
	"errors"

	"musiclib/internal/fsops"
)

// Error codes of the volume. They are stable: boot logs them, the operator
// acts on them, and the maintenance subcommands (DESIGN.md §11.3) will
// report the same ones. Every one of them is fatal at boot; none is retried
// or repaired automatically.
const (
	// CodeUnavailable: /data or its lock file cannot be opened for a reason
	// other than permissions (see CodePermission): /data is missing or not a
	// directory, .lock is not a regular file, or an I/O error.
	CodeUnavailable = "volume_unavailable"
	// CodeLocked: another process holds /data/.lock (§2.2): a second
	// instance, or a maintenance command, is running on this volume.
	CodeLocked = "volume_locked"

	// CodeMaintenance: /data/.maintenance exists, so a rebuild or restore
	// is incomplete (§11.3). The message names the operation to repeat.
	CodeMaintenance = "volume_maintenance_pending"
	// CodeMaintenanceMalformed: /data/.maintenance exists but its content
	// is not in the format of [ParseMaintenance]. It blocks the boot too.
	CodeMaintenanceMalformed = "volume_maintenance_malformed"

	// CodeMarkerMalformed: /data/.musiclib-store exists but is not a
	// regular file in the format of [ParseStoreMarker].
	CodeMarkerMalformed = "volume_marker_malformed"
	// CodeStoreMismatch: the volume marker and settings.store_id name
	// different stores: this volume belongs to another database (§2.2).
	CodeStoreMismatch = "volume_store_mismatch"
	// CodeDBUninitialized: the volume has a marker but the database has no
	// store_id: the database is new or was reset, and must not adopt a
	// volume that belongs to another installation.
	CodeDBUninitialized = "volume_db_uninitialized"
	// CodeMarkerMissing: the database has a store_id, the volume has no
	// marker and its media storage is not empty, so the marker cannot be
	// completed (§11.1).
	CodeMarkerMissing = "volume_marker_missing"
	// CodeNotEmpty: neither the database nor the volume has an identity,
	// and the media storage is not empty: it is not a new installation.
	CodeNotEmpty = "volume_not_empty"

	// CodeCrossDevice: a media directory is on a different filesystem than
	// /data (st_dev, §3.1).
	CodeCrossDevice = "volume_cross_device"
	// CodeNestedMount: a media directory is a separate mount of the same
	// filesystem (§3.1: no nested mounts; N-032).
	CodeNestedMount = "volume_nested_mount"
	// CodePermission: the process cannot read, write or traverse a
	// directory it needs, or the volume is mounted read-only.
	CodePermission = "volume_permission"
	// CodeNoRenameExchange: renameat2(RENAME_EXCHANGE) does not work on the
	// volume (§3.1: no fallback).
	CodeNoRenameExchange = "volume_rename_exchange_unsupported"

	// CodeDB: a database error while reading or creating the store id.
	CodeDB = "volume_db"
	// CodeIO: any other filesystem failure; the fsops cause is kept.
	CodeIO = "volume_io"
)

// Error is the package's typed error. Err keeps the fsops, pgx or parse
// cause, so errors.As and fsops.Code work through it. Messages carry root
// labels and relative paths only, never an absolute path (§13.2).
type Error struct {
	Code string
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	msg := e.Code + ": " + e.Msg
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Code returns the code of the first *Error in err's tree, otherwise the
// fsops (or names) code, otherwise "".
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return fsops.Code(err)
}

func newErr(code, msg string, err error) *Error {
	return &Error{Code: code, Msg: msg, Err: err}
}
