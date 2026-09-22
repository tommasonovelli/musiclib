// Package pgtest gives tests a throwaway database on a real PostgreSQL 17
// server (DESIGN.md §12.1: never SQLite). The server comes from
// MUSICLIB_TEST_DATABASE_URL: the postgres-test Compose service, reached by
// scripts/check.sh and scripts/dev.sh (NOTES.md N-024). Without it the
// tests skip, unless MUSICLIB_REQUIRE_DB=1 (the gate) makes that a failure.
package pgtest

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/store"
)

const (
	envURL     = "MUSICLIB_TEST_DATABASE_URL"
	envRequire = "MUSICLIB_REQUIRE_DB"
)

// EmptyDB creates a new empty database, dropped when the test ends, and
// returns its URL.
func EmptyDB(t testing.TB) string {
	t.Helper()
	base := os.Getenv(envURL)
	if base == "" {
		if os.Getenv(envRequire) == "1" {
			t.Fatalf("%s is unset but %s=1: the PostgreSQL tests must not skip here", envURL, envRequire)
		}
		t.Skipf("%s is unset: skipping the PostgreSQL tests (scripts/check.sh runs them)", envURL)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s: %v", envURL, err)
	}
	// rand.Text is [A-Z2-7]: lowercased, a valid identifier without quoting.
	name := "musiclib_test_" + strings.ToLower(rand.Text())
	admin(t, base, "CREATE DATABASE "+name)
	t.Cleanup(func() { admin(t, base, "DROP DATABASE "+name+" WITH (FORCE)") })
	u.Path = "/" + name
	return u.String()
}

// New returns a pool, sized for two workers, on a new migrated database.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool := Pool(t, EmptyDB(t))
	if err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

// Pool opens a pool on databaseURL, closed when the test ends.
func Pool(t testing.TB, databaseURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(t.Context(), databaseURL, 2)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// AdminExec runs one statement on the server's maintenance database, for
// tests that act on a database from outside it: for example making it
// refuse connections (ALTER DATABASE ... ALLOW_CONNECTIONS false) to check
// how the application behaves when PostgreSQL goes away.
func AdminExec(t testing.TB, sql string) {
	t.Helper()
	base := os.Getenv(envURL)
	if base == "" {
		t.Fatalf("%s is unset: call EmptyDB first, which skips without it", envURL)
	}
	admin(t, base, sql)
}

// DBName returns the database name of a URL returned by EmptyDB.
func DBName(t testing.TB, databaseURL string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(u.Path, "/")
}

// admin runs one statement on the server's maintenance database. It does not
// use t.Context: cleanups run after that context is canceled.
func admin(t testing.TB, base, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connecting to %s: %v", envURL, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
