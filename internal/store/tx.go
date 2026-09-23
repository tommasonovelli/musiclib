package store

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Error codes of the transaction runner (DESIGN.md §6.4).
const (
	// CodeCommitUncertain is a COMMIT whose outcome is unknown: no answer
	// from the server came back, or the answer ended the session (severity
	// FATAL or PANIC). The transaction may or may not be durable.
	// It is never retried blindly: the process stops its mutations and
	// workers and restarts (§6.4). Fatal.
	CodeCommitUncertain = "store_commit_uncertain"
	// CodeConnectionLost is the loss of the database connection, or the
	// impossibility to obtain one, before the COMMIT: while beginning the
	// transaction or while running it. Nothing was committed, but §6.4
	// treats it like an uncertain commit: the process restarts rather than
	// reconnecting partially active workers. Fatal.
	CodeConnectionLost = "store_connection_lost"
	// CodeRetriesExhausted is a serialization failure (40001) or a deadlock
	// (40P01) that persisted over every retry of the short transaction.
	// Nothing was committed; the caller reports it like any other failure.
	CodeRetriesExhausted = "store_retries_exhausted"
	// CodeCanceled is the caller's context ending before the transaction
	// was committed. Nothing was committed.
	CodeCanceled = "store_canceled"
)

// maxRetries is how many times a transaction failing with 40001 or 40P01
// is run again (§6.4: "fino a tre volte"), so at most 1 + maxRetries runs.
const maxRetries = 3

// retryBackoff is the upper bound of the random wait before the first
// retry; it doubles at each further retry (§6.4: "con jitter").
const retryBackoff = 5 * time.Millisecond

// IsFatal reports whether err means that the process must stop its
// mutations and workers and restart (§6.4): an uncertain commit or a lost
// database connection. It is never a normal job or request failure.
func IsFatal(err error) bool {
	switch Code(err) {
	case CodeCommitUncertain, CodeConnectionLost:
		return true
	}
	return false
}

// queries is an unexported name for Queries, so that CatalogTx can embed it
// (keeping every query method promoted) through a field that no other
// package can name or set.
type queries = Queries

// CatalogTx is a transaction that holds the catalog lock (§5.3). Only
// InCatalogTx makes one: its only field is unexported, so another package
// can neither build a usable CatalogTx nor reach the *Queries inside it. A
// function that takes a *CatalogTx can therefore only run inside a
// serialized catalog mutation: that is how the single lock of §5.3 is
// enforced by the compiler rather than by convention. The zero value, the
// only one another package can make, has no connection: calling a query on
// it panics before any SQL is sent.
type CatalogTx struct {
	*queries
}

// InCatalogTx runs fn in a READ COMMITTED transaction whose first statement
// takes the catalog lock: the one pg_advisory_xact_lock of §5.3, with a
// constant key, released by the commit or the rollback. Every catalog
// mutation runs through it: imports, metadata changes, reservations, the
// journal's PREPARE and FINALIZE, and the queue's completions.
//
// On 40001 or 40P01 the whole short transaction runs again, up to
// maxRetries times with jitter (§6.4), so fn must have no effect outside
// the transaction and must reset whatever it reports to its caller. Any
// other error is returned without a retry; a commit whose outcome is
// unknown is CodeCommitUncertain, never retried (§6.4).
func InCatalogTx(ctx context.Context, db *pgxpool.Pool, fn func(tx *CatalogTx) error) error {
	return run(ctx, db, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(q *Queries) error {
		if err := q.LockCatalog(ctx); err != nil {
			return err
		}
		return fn(&CatalogTx{queries: q})
	})
}

// InSnapshotTx runs fn in a REPEATABLE READ transaction without the catalog
// lock: the claim of a job and the loading of a render snapshot (§6.2). The
// snapshot is taken by fn's first statement, so every read of fn sees the
// same committed state. Taking the catalog lock here would be wrong: the
// snapshot would predate the wait for the lock. Retries and failures are as
// in InCatalogTx; a concurrent update of a row that fn then locks is the
// 40001 that makes the transaction run again.
func InSnapshotTx(ctx context.Context, db *pgxpool.Pool, fn func(q *Queries) error) error {
	return run(ctx, db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, fn)
}

// run is the retry loop shared by both kinds of transaction.
func run(ctx context.Context, db *pgxpool.Pool, opts pgx.TxOptions, fn func(q *Queries) error) error {
	for attempt := 0; ; attempt++ {
		err := runOnce(ctx, db, opts, fn)
		if err == nil || !retryable(err) {
			return err
		}
		if attempt == maxRetries {
			return &Error{Code: CodeRetriesExhausted, Msg: "the transaction kept failing with a serialization failure or a deadlock", Err: err}
		}
		if err := sleep(ctx, jitter(attempt)); err != nil {
			return &Error{Code: CodeCanceled, Msg: "waiting to retry the transaction", Err: err}
		}
	}
}

// runOnce is one transaction: begin, fn, commit, and the classification of
// every failure (§6.4).
func runOnce(ctx context.Context, db *pgxpool.Pool, opts pgx.TxOptions, fn func(q *Queries) error) error {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		if ctx.Err() != nil {
			return &Error{Code: CodeCanceled, Msg: "beginning the transaction", Err: err}
		}
		return &Error{Code: CodeConnectionLost, Msg: "beginning the transaction", Err: err}
	}
	if err := fn(New(tx)); err != nil {
		return abort(ctx, tx, err)
	}
	// The commit runs to its end even if the caller is being cancelled: an
	// interrupted commit is the uncertain outcome §6.4 wants to avoid.
	err = tx.Commit(context.WithoutCancel(ctx))
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && sessionEnded(pgErr):
		// The server ended the session while committing (57P01 from
		// pg_terminate_backend or a fast shutdown, 57P02, 57P03, ...). The
		// answer is not a rollback: with synchronous replication the local
		// commit may already have happened. The outcome is unknown and the
		// connection is gone (NOTES.md N-113).
		return &Error{Code: CodeCommitUncertain, Msg: "the server ended the session during the commit and its outcome is unknown", Err: err}
	case errors.As(err, &pgErr):
		// The server answered: it rolled the transaction back (a deferred
		// constraint, or a serialization failure at commit).
		return err
	case errors.Is(err, pgx.ErrTxCommitRollback):
		// The transaction was already aborted by a statement error that fn
		// did not return: nothing was committed.
		return err
	}
	// No answer from the server. pgconn.SafeToRetry cannot tell a COMMIT
	// that never left from one that did: pgx v5.11.0 reports a read failure
	// after the COMMIT was sent as "conn closed" with SafeToRetry true
	// (NOTES.md N-109). So every such failure is uncertain.
	return &Error{Code: CodeCommitUncertain, Msg: "the commit got no answer and its outcome is unknown", Err: err}
}

// abort rolls back after fn failed and classifies fn's error. A connection
// that is gone makes the error fatal, whatever fn wrapped around it, unless
// the caller's context ended first (cancellation also closes a connection
// in the middle of a query).
func abort(ctx context.Context, tx pgx.Tx, fnErr error) error {
	// Read before the rollback: a pool transaction releases its connection
	// inside Rollback, and another goroutine may acquire it at once.
	closed := tx.Conn().IsClosed()
	rbErr := tx.Rollback(context.WithoutCancel(ctx))
	if ctx.Err() != nil {
		return &Error{Code: CodeCanceled, Msg: "the transaction was interrupted", Err: errors.Join(fnErr, rbErr)}
	}
	if closed || rbErr != nil {
		return &Error{Code: CodeConnectionLost, Msg: "the connection failed during the transaction", Err: errors.Join(fnErr, rbErr)}
	}
	return fnErr
}

// sessionEnded reports an error with which the server ends the session:
// severity FATAL or PANIC. It is decided by the severity alone: the
// connection has already been released to the pool when the commit returns,
// so its state must not be read (N-111). The unlocalized severity is used
// when the server sends it (PostgreSQL 9.6 and later), since the other one
// follows lc_messages.
func sessionEnded(pgErr *pgconn.PgError) bool {
	severity := pgErr.SeverityUnlocalized
	if severity == "" {
		severity = pgErr.Severity
	}
	return severity == "FATAL" || severity == "PANIC"
}

// retryable reports a serialization failure or a deadlock (§6.4), wherever
// it is in err's tree.
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01"
}

// jitter returns a random wait in [0, retryBackoff << attempt).
func jitter(attempt int) time.Duration {
	return time.Duration(rand.Int64N(int64(retryBackoff << attempt)))
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
