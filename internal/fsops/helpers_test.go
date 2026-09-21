package fsops

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The tests of this package run on the real filesystem of t.TempDir():
// DESIGN.md §12.1 asks for Linux/ext4 tests of the actual primitives, without
// mocking the renames. The setup of hostile scenarios (symlinks, FIFOs,
// replaced directories) deliberately uses os and absolute paths: it is the
// adversary, not the application.
//
// Every call that could block if the package regressed (opening a FIFO) runs
// through callWithin, so a regression fails the test instead of hanging the
// suite.

var logFSOnce sync.Once

// newRoot creates a temporary directory, opens it as a Root and also returns
// the absolute path, which is used only to set up the scenarios.
func newRoot(t *testing.T) (*Root, string) {
	t.Helper()
	dir := t.TempDir()
	logFSOnce.Do(func() { logFilesystem(t, dir) })
	r, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close of the root: %v", err)
		}
	})
	return r, dir
}

// logFilesystem logs the type of filesystem the tests run on: §12.1
// requires ext4 and the report must be able to say so. ext2, ext3 and ext4
// share the superblock magic, so statfs cannot tell them apart.
func logFilesystem(t *testing.T, dir string) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Logf("statfs of the TempDir failed: %v", err)
		return
	}
	if st.Type == unix.EXT4_SUPER_MAGIC {
		t.Logf("test filesystem: ext2/ext3/ext4 family (magic 0x%x)", st.Type)
		return
	}
	t.Logf("WARNING: test filesystem is NOT ext4 (magic 0x%x): §12.1 requires ext4", st.Type)
}

func mkdirAbs(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeAbs(t *testing.T, p, content string) {
	t.Helper()
	mkdirAbs(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readAbs(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func symlinkAbs(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func mkfifoAbs(t *testing.T, p string) {
	t.Helper()
	if err := unix.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mksockAbs(t *testing.T, p string) {
	t.Helper()
	if err := unix.Mknod(p, unix.S_IFSOCK|0o600, 0); err != nil {
		t.Fatal(err)
	}
}

// mkdevAbs creates a character device equivalent to /dev/null, or skips the
// test: mknod of a device requires CAP_MKNOD, which an unprivileged user or
// a default container does not have.
func mkdevAbs(t *testing.T, p string) {
	t.Helper()
	if err := unix.Mknod(p, unix.S_IFCHR|0o600, int(unix.Mkdev(1, 3))); err != nil {
		t.Skipf("cannot create a device node (%v): CAP_MKNOD is required", err)
	}
}

// wantCode checks the stable code of the error.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %q, got nil", code)
	}
	if got := Code(err); got != code {
		t.Fatalf("code %q, want %q (error: %v)", got, code, err)
	}
}

// mustClose closes f and fails the test on error.
func mustClose(t *testing.T, f io.Closer) {
	t.Helper()
	if err := f.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// readRoot reads a file through the Root.
func readRoot(t *testing.T, r *Root, rel string) string {
	t.Helper()
	f, err := r.Open(rel)
	if err != nil {
		t.Fatalf("Open(%q): %v", rel, err)
	}
	defer mustClose(t, f)
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("reading %q: %v", rel, err)
	}
	return string(b)
}

// openClose opens rel with OpenFile and closes it at once, returning the
// open error.
func openClose(r *Root, rel string, flags int) error {
	f, err := r.OpenFile(rel, flags, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// callWithin runs fn and fails the test if it does not return within d. On
// timeout it calls unblock, which must make fn return, so that a regression
// fails the test instead of hanging the whole suite.
func callWithin(t *testing.T, d time.Duration, fn func() error, unblock func()) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
	}
	t.Errorf("the call was still blocked after %v", d)
	unblock()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("the call was still blocked after unblocking it")
		return nil
	}
}

// unblockFIFO opens the FIFO read-write, which Linux never blocks on and
// which counts as both a reader and a writer: any open of the FIFO blocked
// waiting for a partner returns.
func unblockFIFO(t *testing.T, p string) func() {
	return func() {
		fd, err := unix.Open(p, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Errorf("unblocking the FIFO: %v", err)
			return
		}
		time.Sleep(100 * time.Millisecond)
		if err := unix.Close(fd); err != nil {
			t.Errorf("closing the FIFO unblocker: %v", err)
		}
	}
}

// hasCloexec reports whether the descriptor has FD_CLOEXEC.
func hasCloexec(t *testing.T, fd int) bool {
	t.Helper()
	fl, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("fcntl(F_GETFD): %v", err)
	}
	return fl&unix.FD_CLOEXEC != 0
}

// openFDs returns the descriptors open in the process, as listed by
// /proc/self/fd, or skips the test if /proc is not mounted.
func openFDs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd is not readable: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// otherFilesystemDir returns a directory on a filesystem other than that of
// ref, or skips the test.
func otherFilesystemDir(t *testing.T, ref string) string {
	t.Helper()
	var a unix.Stat_t
	if err := unix.Stat(ref, &a); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"/dev/shm", "/run/user/" + strconv.Itoa(os.Getuid())} {
		var b unix.Stat_t
		if err := unix.Stat(base, &b); err != nil || b.Dev == a.Dev {
			continue
		}
		d, err := os.MkdirTemp(base, "fsops-test-")
		if err != nil {
			continue
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(d); err != nil {
				t.Errorf("cleanup of %s: %v", d, err)
			}
		})
		return d
	}
	t.Skip("no second writable filesystem available (/dev/shm, /run/user)")
	return ""
}
