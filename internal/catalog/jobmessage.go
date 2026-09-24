package catalog

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// DatabaseJobMessage is the message stored with a job that failed because
// of the database. The error itself is logged by the caller.
const DatabaseJobMessage = "a database error interrupted the job; the server log has the details"

// JobMessage is the message to store with a failed job whose failure is
// err and whose descriptive message is message (§6.4: the error is shown
// to the user). §10.1 forbids showing SQL or database internals, and a
// database error's text can quote them: pgx and PostgreSQL messages name
// constraints, columns, values, hosts. So when err's tree holds a database
// error anywhere (a *pgconn.PgError, a catalog_db or job_db error, or any
// store error), the stored message is DatabaseJobMessage instead of
// message; the code stays. NOTES.md N-150.
func JobMessage(err error, message string) string {
	if isDatabaseError(err) {
		return DatabaseJobMessage
	}
	return message
}

// isDatabaseError walks the whole tree of err, both Unwrap forms, since a
// database error can sit under any other typed error.
func isDatabaseError(err error) bool {
	if err == nil {
		return false
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) || store.Code(err) != "" {
		return true
	}
	switch e := err.(type) {
	case *Error:
		if e.Code == CodeDB {
			return true
		}
	case *jobs.Error:
		if e.Code == jobs.CodeDB {
			return true
		}
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return isDatabaseError(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if isDatabaseError(e) {
				return true
			}
		}
	}
	return false
}
