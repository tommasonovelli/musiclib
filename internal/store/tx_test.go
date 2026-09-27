package store_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// The advisory lock keys, as their ASCII names (NOTES.md N-053, N-095).
var (
	catalogKey   = int64(binary.BigEndian.Uint64([]byte("mlcatalg")))
	migrationKey = int64(binary.BigEndian.Uint64([]byte("mlmigrat")))
)

// txExec runs raw SQL inside a runner transaction.
func txExec(ctx context.Context, q *store.Queries, sql string, args ...any) error {
	_, err := store.DB(q).Exec(ctx, sql, args...)
	return err
}

// counterTable creates a one-row table for the retry tests.
func counterTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `CREATE TABLE counter (id int PRIMARY KEY, v int NOT NULL);
		INSERT INTO counter VALUES (1, 0), (2, 0)`); err != nil {
		t.Fatal(err)
	}
}

func counterValue(t *testing.T, pool *pgxpool.Pool, id int) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(t.Context(), `SELECT v FROM counter WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// advisoryWaiters counts the sessions waiting for the advisory lock key.
func advisoryWaiters(t *testing.T, pool *pgxpool.Pool, key int64) int {
	t.Helper()
	var n int
	err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND NOT granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND ((classid::bigint << 32) | objid::bigint) = $1`, key).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The catalog lock (§5.3) is taken by the first statement of InCatalogTx,
// with the constant key "mlcatalg"; a second catalog transaction waits for
// the first to end; the migration lock is a different key.
func TestCatalogLockSerializes(t *testing.T) {
	if catalogKey == migrationKey {
		t.Fatal("the catalog and migration keys are equal")
	}
	pool := pgtest.New(t)
	ctx := t.Context()
	entered, release := make(chan struct{}), make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		errs <- store.InCatalogTx(ctx, pool, func(tx *store.CatalogTx) error {
			var held bool
			err := store.DB(store.CatalogQueries(tx)).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
				WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid()
				  AND ((classid::bigint << 32) | objid::bigint) = $1)`, catalogKey).Scan(&held)
			if err != nil {
				return err
			}
			if !held {
				return errors.New("the catalog key is not held inside InCatalogTx")
			}
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-errs:
		t.Fatalf("first transaction: %v", err)
	}

	var free bool
	if err := pool.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, catalogKey).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if free {
		t.Error("another session took the catalog key while a catalog transaction held it")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, migrationKey).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if !free {
		t.Error("the migration lock is blocked by the catalog lock")
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrationKey); err != nil {
		t.Fatal(err)
	}
	conn.Release()

	second := make(chan struct{})
	go func() {
		errs <- store.InCatalogTx(ctx, pool, func(*store.CatalogTx) error {
			close(second)
			return nil
		})
	}()
	waitFor(t, "the second transaction to wait for the lock", func() bool { return advisoryWaiters(t, pool, catalogKey) == 1 })
	select {
	case <-second:
		t.Fatal("a second catalog transaction ran while the first held the lock")
	default:
	}
	close(release)
	<-second
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// A serialization failure (40001) reruns the whole short transaction, which
// then sees the concurrent commit (§6.2, §6.4).
func TestSerializationFailureRetried(t *testing.T) {
	pool := pgtest.New(t)
	counterTable(t, pool)
	ctx := t.Context()
	var runs int
	err := store.InSnapshotTx(ctx, pool, func(q *store.Queries) error {
		runs++
		var v int
		if err := store.DB(q).QueryRow(ctx, `SELECT v FROM counter WHERE id = 1`).Scan(&v); err != nil {
			return err
		}
		if runs == 1 {
			// Committed after this transaction's snapshot.
			if _, err := pool.Exec(ctx, `UPDATE counter SET v = v + 1 WHERE id = 1`); err != nil {
				return err
			}
		}
		return txExec(ctx, q, `UPDATE counter SET v = v + 10 WHERE id = 1`)
	})
	if err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Errorf("%d runs, want 2: the 40001 was not retried, or was not raised (REPEATABLE READ?)", runs)
	}
	if v := counterValue(t, pool, 1); v != 11 {
		t.Errorf("v = %d, want 11", v)
	}
}

// A deadlock (40P01) between two short transactions reruns the victim.
func TestDeadlockRetried(t *testing.T) {
	pool := pgtest.New(t)
	counterTable(t, pool)
	ctx := t.Context()
	var runs atomic.Int32
	var firstLocked sync.WaitGroup
	firstLocked.Add(2)
	lockBoth := func(first, second int) error {
		run := 0
		return store.InSnapshotTx(ctx, pool, func(q *store.Queries) error {
			run++
			runs.Add(1)
			if err := txExec(ctx, q, `SELECT 1 FROM counter WHERE id = $1 FOR UPDATE`, first); err != nil {
				return err
			}
			if run == 1 {
				firstLocked.Done()
				firstLocked.Wait()
			}
			return txExec(ctx, q, `SELECT 1 FROM counter WHERE id = $1 FOR UPDATE`, second)
		})
	}
	errs := make(chan error, 2)
	go func() { errs <- lockBoth(1, 2) }()
	go func() { errs <- lockBoth(2, 1) }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("deadlocked transaction not retried: %v", err)
		}
	}
	// The victim's first retry can take a REPEATABLE READ snapshot before
	// the survivor commits. PostgreSQL then reports 40001 on that retry,
	// requiring another run. Both outcomes exercise the real retry policy.
	if n := runs.Load(); n < 3 || n > 8 {
		t.Errorf("%d runs, want at least one retry and at most four per transaction", n)
	}
}

// At most three retries (§6.4): four runs, then store_retries_exhausted,
// which is not fatal and keeps the 40001.
func TestRetriesExhausted(t *testing.T) {
	pool := pgtest.New(t)
	ctx := t.Context()
	var runs int
	err := store.InCatalogTx(ctx, pool, func(tx *store.CatalogTx) error {
		runs++
		return txExec(ctx, store.CatalogQueries(tx), `DO $$ BEGIN RAISE EXCEPTION 'forced' USING ERRCODE = 'serialization_failure'; END $$`)
	})
	if runs != 4 {
		t.Errorf("%d runs, want 4", runs)
	}
	if store.Code(err) != store.CodeRetriesExhausted || store.IsFatal(err) || pgCode(err) != "40001" {
		t.Errorf("err = %v (code %q, fatal %v), want %s wrapping the 40001", err, store.Code(err), store.IsFatal(err), store.CodeRetriesExhausted)
	}
}

// Other errors are returned as they are, after one run.
func TestOtherErrorsNotRetried(t *testing.T) {
	pool := pgtest.New(t)
	ctx := t.Context()
	sentinel := errors.New("domain refusal")
	for _, tc := range []struct {
		name string
		fn   func(q *store.Queries) error
		want func(error) bool
	}{
		{"go error", func(*store.Queries) error { return sentinel }, func(err error) bool { return errors.Is(err, sentinel) }},
		{"unique violation", func(q *store.Queries) error {
			return txExec(ctx, q, `DO $$ BEGIN RAISE EXCEPTION 'dup' USING ERRCODE = 'unique_violation'; END $$`)
		}, func(err error) bool { return pgCode(err) == "23505" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var runs int
			err := store.InSnapshotTx(ctx, pool, func(q *store.Queries) error {
				runs++
				return tc.fn(q)
			})
			if runs != 1 || !tc.want(err) || store.Code(err) != "" {
				t.Errorf("runs %d, err %v (code %q): want one run and the error unchanged", runs, err, store.Code(err))
			}
		})
	}
}

// A deferred unique violation at COMMIT is an expected SQL error (§6.4):
// the server answered and rolled back, so it is neither fatal nor retried.
func TestDeferredViolationAtCommitIsNotFatal(t *testing.T) {
	pool := pgtest.New(t)
	ctx := t.Context()
	var runs int
	err := store.InCatalogTx(ctx, pool, func(tx *store.CatalogTx) error {
		runs++
		return txExec(ctx, store.CatalogQueries(tx), `CREATE TEMP TABLE d (k int UNIQUE DEFERRABLE INITIALLY DEFERRED) ON COMMIT DROP;
			INSERT INTO d VALUES (1), (1)`)
	})
	if runs != 1 || pgCode(err) != "23505" || store.IsFatal(err) || store.Code(err) != "" {
		t.Errorf("runs %d, err %v: want one run and the 23505 of the commit, not fatal", runs, err)
	}
}

// A COMMIT whose session the server ends is uncertain, not an ordinary SQL
// error (§6.4, N-113). The COMMIT blocks on a deferred unique check that
// waits for another session's uncommitted row, and its backend is then
// terminated: the server answers with FATAL 57P01, which is fatal and never
// retried.
func TestSessionEndedAtCommitIsUncertain(t *testing.T) {
	pool := pgtest.New(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `CREATE TABLE d (k int UNIQUE DEFERRABLE INITIALLY DEFERRED)`); err != nil {
		t.Fatal(err)
	}
	// The other session: an uncommitted row with the same key, so that the
	// deferred check of the COMMIT below waits for it.
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Rollback(context.Background()) }()
	if _, err := other.Exec(ctx, `INSERT INTO d VALUES (1)`); err != nil {
		t.Fatal(err)
	}

	var runs atomic.Int32
	var pid atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- store.InCatalogTx(ctx, pool, func(tx *store.CatalogTx) error {
			runs.Add(1)
			var p int32
			if err := store.DB(store.CatalogQueries(tx)).QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&p); err != nil {
				return err
			}
			pid.Store(p)
			return txExec(ctx, store.CatalogQueries(tx), `INSERT INTO d VALUES (1)`)
		})
	}()
	waitFor(t, "the COMMIT to wait on the deferred unique check", func() bool {
		var waiting bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE pid = $1 AND wait_event_type = 'Lock' AND lower(query) = 'commit')`, pid.Load()).Scan(&waiting)
		return err == nil && waiting
	})
	var terminated bool
	if err := pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid.Load()).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("pg_terminate_backend: %v, %v", terminated, err)
	}
	err = <-done
	if store.Code(err) != store.CodeCommitUncertain || !store.IsFatal(err) || pgCode(err) != "57P01" {
		t.Errorf("err = %v (code %q, fatal %v), want %s wrapping the 57P01", err, store.Code(err), store.IsFatal(err), store.CodeCommitUncertain)
	}
	if n := runs.Load(); n != 1 {
		t.Errorf("%d runs, want 1: an uncertain commit is never retried", n)
	}
}

func rowCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM marks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The connection is lost at chosen points of the wire protocol, against the
// real server (§6.4, §12.2): a lost acknowledgement and a cut before the
// COMMIT are both an uncertain commit, whatever the server did; a cut in
// the middle of the transaction is a lost connection. All are fatal and
// none is retried.
func TestLostConnection(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	direct := pgtest.Pool(t, dbURL)
	if _, err := direct.Exec(t.Context(), `CREATE TABLE marks (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		arm      func(p *pgtest.Proxy)
		midFn    func(p *pgtest.Proxy)
		wantCode string
		wantRows int
	}{
		{"commit ack lost", (*pgtest.Proxy).LoseNextCommitAck, nil, store.CodeCommitUncertain, 1},
		{"cut before commit", (*pgtest.Proxy).CutBeforeNextCommit, nil, store.CodeCommitUncertain, 0},
		{"cut during the transaction", nil, (*pgtest.Proxy).CutAll, store.CodeConnectionLost, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := direct.Exec(t.Context(), `TRUNCATE marks`); err != nil {
				t.Fatal(err)
			}
			proxy := pgtest.NewProxy(t, dbURL)
			pool := pgtest.Pool(t, proxy.URL)
			ctx := t.Context()
			if tc.arm != nil {
				tc.arm(proxy)
			}
			var runs int
			err := store.InCatalogTx(ctx, pool, func(tx *store.CatalogTx) error {
				runs++
				if err := txExec(ctx, store.CatalogQueries(tx), `INSERT INTO marks VALUES (1)`); err != nil {
					return err
				}
				if tc.midFn != nil {
					tc.midFn(proxy)
					return txExec(ctx, store.CatalogQueries(tx), `INSERT INTO marks VALUES (2)`)
				}
				return nil
			})
			if store.Code(err) != tc.wantCode || !store.IsFatal(err) || runs != 1 {
				t.Fatalf("err = %v (code %q, fatal %v), runs %d; want %s, fatal, one run", err, store.Code(err), store.IsFatal(err), runs, tc.wantCode)
			}
			// The server has seen the connection close; wait for its backend
			// to finish, then look at what is durable.
			waitFor(t, "the proxied backend to exit", func() bool {
				var n int
				err := direct.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND pid <> pg_backend_pid() AND state <> 'idle'`).Scan(&n)
				return err == nil && n == 0
			})
			if n := rowCount(t, direct); n != tc.wantRows {
				t.Errorf("%d rows durable, want %d", n, tc.wantRows)
			}
		})
	}
}

// A database that cannot be reached when the transaction begins is a lost
// connection (§6.4), unless the caller's context ended.
func TestUnreachableDatabase(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = addr
	pool := pgtest.Pool(t, u.String())
	called := false
	err = store.InCatalogTx(t.Context(), pool, func(*store.CatalogTx) error { called = true; return nil })
	if store.Code(err) != store.CodeConnectionLost || !store.IsFatal(err) || called {
		t.Errorf("err = %v (called %v), want %s", err, called, store.CodeConnectionLost)
	}
}

// Cancellation before the commit is store_canceled, never fatal: nothing
// was committed. A cancellation after the function returned does not stop
// the commit.
func TestCancellation(t *testing.T) {
	pool := pgtest.New(t)
	if _, err := pool.Exec(t.Context(), `CREATE TABLE marks (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	t.Run("before begin", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := store.InCatalogTx(ctx, pool, func(*store.CatalogTx) error { return nil })
		if store.Code(err) != store.CodeCanceled || store.IsFatal(err) {
			t.Errorf("err = %v, want %s", err, store.CodeCanceled)
		}
	})

	t.Run("waiting for the catalog lock", func(t *testing.T) {
		held, release := make(chan struct{}), make(chan struct{})
		holder := make(chan error, 1)
		go func() {
			holder <- store.InCatalogTx(t.Context(), pool, func(*store.CatalogTx) error {
				close(held)
				<-release
				return nil
			})
		}()
		<-held
		ctx, cancel := context.WithCancel(t.Context())
		waiter := make(chan error, 1)
		called := false
		go func() {
			waiter <- store.InCatalogTx(ctx, pool, func(*store.CatalogTx) error { called = true; return nil })
		}()
		waitFor(t, "the waiter to block on the lock", func() bool { return advisoryWaiters(t, pool, catalogKey) == 1 })
		cancel()
		err := <-waiter
		if store.Code(err) != store.CodeCanceled || store.IsFatal(err) || called {
			t.Errorf("err = %v (called %v), want %s", err, called, store.CodeCanceled)
		}
		close(release)
		if err := <-holder; err != nil {
			t.Errorf("the holder failed: %v", err)
		}
	})

	t.Run("after the function returned", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		err := store.InCatalogTx(ctx, pool, func(tx *store.CatalogTx) error {
			if err := txExec(ctx, store.CatalogQueries(tx), `INSERT INTO marks VALUES (7)`); err != nil {
				return err
			}
			cancel()
			return nil
		})
		if err != nil {
			t.Fatalf("the commit did not run to its end: %v", err)
		}
		if n := rowCount(t, pool); n != 1 {
			t.Errorf("%d rows, want 1", n)
		}
	})
}

// Pins the pgx v5.11.0 behaviour behind N-109: a COMMIT that reached the
// server and committed, whose answer was lost, fails with an error that
// pgconn.SafeToRetry calls safe. That is why the runner never consults
// SafeToRetry at commit.
func TestPgxSafeToRetryIsWrongAfterASentCommit(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	direct := pgtest.Pool(t, dbURL)
	if _, err := direct.Exec(t.Context(), `CREATE TABLE marks (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	proxy := pgtest.NewProxy(t, dbURL)
	pool := pgtest.Pool(t, proxy.URL)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO marks VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	proxy.LoseNextCommitAck()
	err = tx.Commit(t.Context())
	if err == nil {
		t.Fatal("the commit succeeded although its answer was dropped")
	}
	waitFor(t, "the row to be durable", func() bool { return rowCount(t, direct) == 1 })
	if !pgconn.SafeToRetry(err) {
		t.Logf("pgx no longer calls this error safe to retry (%v): N-109 can be revisited", err)
	}
}
