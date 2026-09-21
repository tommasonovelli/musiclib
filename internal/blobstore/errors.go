package blobstore

import (
	"context"
	"errors"

	"golang.org/x/sys/unix"

	"musiclib/internal/fsops"
)

// Error codes of the blob store. They are stable: callers branch on them and
// the API exposes them in the {code, message, details} body (DESIGN.md §10.1).
const (
	// CodeInvalidHash is a sha256 argument that is not 64 lowercase hex
	// digits; it is rejected before any path is built.
	CodeInvalidHash = "blob_invalid_hash"
	CodeNotFound    = "blob_not_found"
	// CodeCorrupt is an entry at a blob's name whose type, size or content
	// does not match the name (§7.5, §11.3). It is never repaired
	// automatically: a good copy comes from the backup.
	CodeCorrupt = "corrupt_blob"
	// CodeNoSpace is ENOSPC or EDQUOT while writing a temporary (§11.2).
	CodeNoSpace = "blob_no_space"
	// CodeSource is a read error of the caller's stream, as opposed to a
	// failure of the store itself.
	CodeSource   = "blob_source_read"
	CodeCanceled = "blob_canceled"
	CodeIO       = "blob_io"
)

// Error is the package's typed error. SHA is empty when the failure happens
// before the content is hashed. Err keeps the underlying *fsops.Error or
// errno, so errors.Is(err, context.Canceled) and errors.As work through it.
type Error struct {
	Code string
	Op   string
	SHA  string
	Err  error
}

func (e *Error) Error() string {
	msg := e.Code + " (" + e.Op + ")"
	if e.SHA != "" {
		msg += ": " + e.SHA
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Code returns the code of the first *Error in err's tree, otherwise the
// fsops or names code, otherwise "".
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return fsops.Code(err)
}

// wrap types a store failure. ENOSPC gets its own code because §11.2 makes
// it an expected condition that callers report differently from I/O damage.
func wrap(op, sha string, err error) *Error {
	code := CodeIO
	switch {
	case errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
		code = CodeNoSpace
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = CodeCanceled
	}
	return &Error{Code: code, Op: op, SHA: sha, Err: err}
}

// readErr types a failure to reach a pinned blob: a missing name is
// not_found, and anything but a regular file at the name is corruption,
// never something to follow (§3.1: no symlinks in originals/).
func readErr(op, sha string, err error) *Error {
	switch fsops.Code(err) {
	case fsops.CodeNotFound:
		return &Error{Code: CodeNotFound, Op: op, SHA: sha, Err: err}
	case fsops.CodeSymlink, fsops.CodeSpecialFile, fsops.CodeIsDirectory:
		return &Error{Code: CodeCorrupt, Op: op, SHA: sha, Err: err}
	}
	return wrap(op, sha, err)
}
