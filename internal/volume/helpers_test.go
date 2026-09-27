package volume

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/unix"

	"musiclib/internal/faulttest"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// The tests run on the real ext4 TMPDIR of the gate and on a real
// PostgreSQL 17 (DESIGN.md §12.1). Scenarios are set up with os and
// absolute paths: that is the adversary, not the application.

// fixture is one volume directory and one migrated database.
type fixture struct {
	dir   string
	dbURL string
	pool  *pgxpool.Pool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := pgtest.EmptyDB(t)
	pool := pgtest.Pool(t, url)
	if err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return &fixture{dir: t.TempDir(), dbURL: url, pool: pool}
}

// acquire takes the volume lock, released when the test ends.
func (f *fixture) acquire(t *testing.T) *Volume {
	t.Helper()
	v, err := Acquire(f.dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() {
		if err := v.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return v
}

// boot runs the volume part of the server's boot and closes the volume.
func (f *fixture) boot(t *testing.T) (uuid.UUID, error) {
	t.Helper()
	v, err := Acquire(f.dir)
	if err != nil {
		return uuid.Nil, err
	}
	id, err := bootSteps(t.Context(), v, f.pool)
	if cerr := v.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	return id, err
}

func bootSteps(ctx context.Context, v *Volume, pool *pgxpool.Pool) (uuid.UUID, error) {
	if err := v.CheckMaintenance(); err != nil {
		return uuid.Nil, err
	}
	id, err := v.Identify(ctx, pool)
	if err != nil {
		return uuid.Nil, err
	}
	if err := v.OpenLayout(); err != nil {
		return uuid.Nil, err
	}
	return id, v.CheckFilesystem()
}

func (f *fixture) mustBoot(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := f.boot(t)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	return id
}

// dbStoreID returns settings.store_id, or uuid.Nil if there is none.
func (f *fixture) dbStoreID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := store.New(f.pool).GetStoreID(t.Context())
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) setDBStoreID(t *testing.T, id uuid.UUID) {
	t.Helper()
	if n, err := store.New(f.pool).InsertStoreID(t.Context(), id); err != nil || n != 1 {
		t.Fatalf("InsertStoreID = %d, %v", n, err)
	}
}

func (f *fixture) path(rel string) string { return filepath.Join(f.dir, rel) }

func (f *fixture) write(t *testing.T, rel, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.path(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path(rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) exists(t *testing.T, rel string) bool {
	t.Helper()
	_, err := os.Lstat(f.path(rel))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// fileState is what "never rewritten" means: the same bytes in the same
// inode, not modified since.
type fileState struct {
	content []byte
	ino     uint64
	mtime   unix.Timespec
	ctime   unix.Timespec
}

func (f *fixture) state(t *testing.T, rel string) fileState {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(f.path(rel), &st); err != nil {
		t.Fatal(err)
	}
	s := fileState{ino: st.Ino, mtime: st.Mtim, ctime: st.Ctim}
	if st.Mode&unix.S_IFMT == unix.S_IFREG {
		b, err := os.ReadFile(f.path(rel))
		if err != nil {
			t.Fatal(err)
		}
		s.content = b
	}
	return s
}

func (f *fixture) assertUnchanged(t *testing.T, rel string, before fileState) {
	t.Helper()
	after := f.state(t, rel)
	if !bytes.Equal(after.content, before.content) || after.ino != before.ino ||
		after.mtime != before.mtime || after.ctime != before.ctime {
		t.Fatalf("%s was modified: before %+v, after %+v", rel, before, after)
	}
}

// assertLayout checks that the media directories exist (or not).
func (f *fixture) assertLayout(t *testing.T, want bool) {
	t.Helper()
	for _, d := range mediaDirs {
		if got := f.exists(t, d); got != want {
			t.Fatalf("%s exists = %v, want %v", d, got, want)
		}
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := Code(err); got != code {
		t.Fatalf("code %q, want %q (error: %v)", got, code, err)
	}
}

// TestHelperProcess is not a test: it is the body of the child processes
// started by the tests, and returns at once in a normal run.
func TestHelperProcess(t *testing.T) {
	mode := faulttest.Mode()
	if mode == "" {
		return
	}
	faulttest.Exit(helperMain(mode))
}

func helperMain(mode string) string {
	switch mode {
	case "try-acquire":
		v, err := Acquire(os.Getenv("VOLUME_DIR"))
		if err != nil {
			return "error " + Code(err)
		}
		if err := v.Close(); err != nil {
			return "close error " + err.Error()
		}
		return "acquired"
	case "crash-maintenance":
		v, err := Acquire(os.Getenv("VOLUME_DIR"))
		if err != nil {
			return "error " + err.Error()
		}
		v.failpoints = faulttest.Crash(os.Getenv("CRASH_AT"))
		id, err := uuid.Parse(os.Getenv("STORE_ID"))
		if err != nil {
			return "error " + err.Error()
		}
		if os.Getenv("MAINTENANCE_ACTION") == "end" {
			err = v.EndMaintenance(OpRebuild, id)
		} else {
			err = v.BeginMaintenance(OpRebuild, id)
		}
		if err != nil {
			return "error " + err.Error()
		}
		return "completed without reaching " + os.Getenv("CRASH_AT")
	case "crash-first-init":
		// A real crash: the process is killed at the named point, with no
		// deferred cleanup, no Close and the database connection dropped.
		ctx := context.Background()
		pool, err := store.NewPool(ctx, os.Getenv("VOLUME_DB"), 1)
		if err != nil {
			return "error " + err.Error()
		}
		v, err := Acquire(os.Getenv("VOLUME_DIR"))
		if err != nil {
			return "error " + err.Error()
		}
		v.failpoints = faulttest.Crash(os.Getenv("CRASH_AT"))
		if _, err := bootSteps(ctx, v, pool); err != nil {
			return "error " + err.Error()
		}
		return "completed without reaching " + os.Getenv("CRASH_AT")
	default:
		return "error: unknown mode " + mode
	}
}

// runHelper runs the test binary as a child in the given mode. It returns
// the result line, or faulttest.Killed if the child died of SIGKILL.
func runHelper(t *testing.T, mode string, env ...string) string {
	t.Helper()
	return faulttest.RunChild(t, 60*time.Second, mode, env...)
}

func unixMkfifo(p string) error { return unix.Mkfifo(p, 0o644) }
