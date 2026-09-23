package jobs

import (
	"errors"
	"fmt"

	"musiclib/internal/store"
)

// Error codes of the queue. They are stable: callers branch on them and the
// API will expose them in the {code, message, details} body (§10.1).
const (
	// CodeAttemptStale is a completion or a commit for an attempt that is
	// no longer the running one: the job is not running, or it runs with
	// another ticket (§6.3). Nothing was changed.
	CodeAttemptStale = "job_attempt_stale"
	// CodeNotFound is a job id that does not exist.
	CodeNotFound = "job_not_found"
	// CodeInvalidResult is an outcome that the queue refuses to store: a
	// state that does not fit the kind, a malformed error code, an unknown
	// warning (§4.2).
	CodeInvalidResult = "job_invalid_result"
	// CodeInvalidOverrides is a jobs.overrides value outside the closed
	// type of §7.3.
	CodeInvalidOverrides = "job_invalid_overrides"
	// CodeInvalidArgument is a call that cannot be served as made: an empty
	// render version, a worker count outside 1..MaxWorkers.
	CodeInvalidArgument = "job_invalid_argument"
	// CodeDB is an unexpected database failure. Whether it is fatal is
	// store.IsFatal's answer, through Err.
	CodeDB = "job_db"
)

// Error is the package's typed error. Err keeps the cause: a *store.Error
// for the fatal cases of §6.4, a pgx error, or a names error.
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

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// dbErr wraps a database failure with the operation that failed.
func dbErr(op string, err error) error {
	return &Error{Code: CodeDB, Msg: op, Err: err}
}

// Code returns the code of the first *Error in err's tree, otherwise the
// store code, otherwise "". A fatal store error (store.IsFatal, §6.4) wins
// over everything it wraps: it may wrap an error of the transaction's
// function, and it must never be read as that ordinary failure.
func Code(err error) string {
	if store.IsFatal(err) {
		return store.Code(err)
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return store.Code(err)
}
