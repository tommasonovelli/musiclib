package publish

import (
	"errors"
	"fmt"

	"musiclib/internal/catalog"
	"musiclib/internal/render"
	"musiclib/internal/store"
)

// Error codes of the publisher. They are stable: a render failed before the
// journal stores its code as error_code (§6.4), and the boot logs the code
// of a recovery it cannot complete (§9.4, §11.1).
const (
	// CodeDestinationOccupied: the album's new path is already a directory
	// on disk that this album has not published (§3.3: "non viene adottato
	// né cancellato automaticamente"). The job fails before the journal.
	CodeDestinationOccupied = "publish_destination_occupied"
	// CodeForeignOutput: a directory the publication would replace or
	// retire holds the receipt of another album (§9.3: it never authorizes
	// a replacement).
	CodeForeignOutput = "publish_foreign_output"
	// CodeUnsafeEntry: a symlink, a special file or a file where the
	// protocol needs a directory, in library/ or in the staging (§9.3:
	// "symlink e file speciali sono sempre rifiutati").
	CodeUnsafeEntry = "publish_unsafe_entry"
	// CodeStagingInvalid: the build's staging is missing, or its receipt is
	// not the one of the build being published.
	CodeStagingInvalid = "publish_staging_invalid"
	// CodeStateChanged: PREPARE found the album's published output
	// different from the one the preflight checked. The catalog rules make
	// it impossible; the job fails before the journal.
	CodeStateChanged = "publish_state_changed"
	// CodeIllegalState: after PREPARE, or during the boot's recovery, the
	// disk or the journal matches no legal transition of §9.3 (§9.4:
	// "non si indovina quale directory cancellare"). Nothing is deleted;
	// publishing is suspended until the state is corrected or rebuilt.
	CodeIllegalState = "publish_illegal_state"
	// CodeSuspended: a publication failed after PREPARE, so its journal is
	// pending and takes priority over everything else (§9.4): no other
	// publication runs in this process. The pool stops, the process exits,
	// and the next boot recovers the journal.
	CodeSuspended = "publish_suspended"
	// CodeIO: a filesystem failure after PREPARE other than an unexpected
	// entry, such as EIO or ENOSPC while installing or syncing. The journal
	// stays pending, like CodeIllegalState.
	CodeIO = "publish_io"
	// CodeJournalPending: PREPARE found a journal row. The boot resolves
	// the journal before any worker starts (§9.4), so this breaks the
	// protocol; publishing is suspended.
	CodeJournalPending = "publish_journal_pending"
	// CodeCanceled: the context ended before PREPARE committed (a shutdown,
	// §11.1). Nothing was prepared.
	CodeCanceled = "publish_canceled"
	// CodeInvalidArgument: a call that cannot be served as made.
	CodeInvalidArgument = "publish_invalid_argument"
	// CodeDB: an unexpected database failure; store.IsFatal tells whether
	// it is one of §6.4's.
	CodeDB = "publish_db"
)

// Error is the package's typed error. Paths in messages are relative to
// library/ or work/, never absolute (§10.1). Err keeps the cause.
type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	msg := e.Code + ": " + e.Message
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func wrap(code string, err error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}

func dbErr(op string, err error) error {
	return &Error{Code: CodeDB, Message: op, Err: err}
}

// Code returns the stable code of err: a fatal store code first (§6.4),
// then the publisher's, the catalog's (path_reserved, ...), the queue's,
// then the renderer's, which covers the media adapter, the blob store, the
// filesystem and the names codes. "" if none.
func Code(err error) string {
	if store.IsFatal(err) {
		return store.Code(err)
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if c := catalog.Code(err); c != "" {
		return c
	}
	return render.Code(err)
}
