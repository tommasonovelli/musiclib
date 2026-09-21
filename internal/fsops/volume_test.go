package fsops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStatFS(t *testing.T) {
	r, dir := newRoot(t)
	info, err := r.StatFS()
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	if info.BlockSize <= 0 || info.TotalBytes <= 0 || info.FreeBytes < 0 || info.FreeBytes > info.TotalBytes {
		t.Fatalf("implausible StatFS: %+v", info)
	}
	if want := int64(st.Blocks) * st.Frsize; info.TotalBytes != want {
		t.Fatalf("TotalBytes %d, want %d", info.TotalBytes, want)
	}
}

func TestLockExclusive(t *testing.T) {
	r, dir := newRoot(t)

	l1, err := r.Lock(".lock")
	if err != nil {
		t.Fatal(err)
	}
	if !hasCloexec(t, int(l1.f.Fd())) {
		t.Fatal("the lock descriptor does not have FD_CLOEXEC")
	}
	// A second lock, even from the same process, fails at once: flock ties
	// the lock to the open file description.
	_, err = r.Lock(".lock")
	wantCode(t, err, CodeLockBusy)

	// A second Root on the same volume sees the same lock.
	r2, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, r2)
	_, err = r2.Lock(".lock")
	wantCode(t, err, CodeLockBusy)

	if err := l1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	l2, err := r2.Lock(".lock")
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatal(err)
	}
	// The lock file is not removed.
	if _, err := os.Stat(filepath.Join(dir, ".lock")); err != nil {
		t.Fatalf("lock file removed: %v", err)
	}
}

func TestLockOnInvalidPath(t *testing.T) {
	r, dir := newRoot(t)
	mkdirAbs(t, filepath.Join(dir, "d"))
	symlinkAbs(t, "x", filepath.Join(dir, "link"))
	_, err := r.Lock("d")
	wantCode(t, err, CodeIsDirectory)
	_, err = r.Lock("link")
	wantCode(t, err, CodeSymlink)
}

// helperEnv selects the child-process mode of TestHelperProcess.
const helperEnv = "MUSICLIB_FSOPS_HELPER"

// TestHelperProcess is not a test: it is the body of the child processes
// started by the tests below, and returns at once in a normal run.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	fmt.Println("RESULT:", helperMain(mode))
	os.Exit(0)
}

// helperMain runs one child-process mode and returns its one-line result.
func helperMain(mode string) string {
	switch mode {
	case "scan-fds":
		// Is any inherited descriptor the lock file?
		dev, _ := strconv.ParseUint(os.Getenv("LOCK_DEV"), 10, 64)
		ino, _ := strconv.ParseUint(os.Getenv("LOCK_INO"), 10, 64)
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return "error: " + err.Error()
		}
		for _, e := range entries {
			fd, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			var st unix.Stat_t
			if unix.Fstat(fd, &st) == nil && uint64(st.Dev) == dev && st.Ino == ino {
				return "inherited"
			}
		}
		return "clean"
	case "try-lock":
		fd, err := unix.Open(os.Getenv("LOCK_PATH"), unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			return "error: " + err.Error()
		}
		defer unix.Close(fd)
		switch err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); {
		case err == nil:
			return "acquired"
		case errors.Is(err, unix.EWOULDBLOCK):
			return "busy"
		default:
			return "error: " + err.Error()
		}
	default:
		return "error: unknown mode " + mode
	}
}

// runHelper runs the test binary as a child process in the given mode and
// returns its result line.
func runHelper(t *testing.T, mode string, env []string, extra ...*os.File) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
	cmd.Env = append(append(os.Environ(), helperEnv+"="+mode), env...)
	cmd.ExtraFiles = extra
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process %s: %v\n%s", mode, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if res, ok := strings.CutPrefix(line, "RESULT: "); ok {
			return res
		}
	}
	t.Fatalf("child process %s: no result\n%s", mode, out)
	return ""
}

// §11.1: child tools must not inherit the volume lock. A child process
// checks every descriptor it inherited against the lock file's identity; a
// control run that passes the descriptor on purpose proves that the check
// can see an inherited lock. A second child also checks that the lock
// excludes other processes while held and is free after release.
//
// Releasing the lock in the parent and re-acquiring it would not prove
// anything: LOCK_UN releases the lock for every descriptor sharing the open
// file description, inherited ones included.
func TestLockNotInheritedByChildren(t *testing.T) {
	openFDs(t) // skips without /proc
	r, dir := newRoot(t)
	l, err := r.Lock(".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if l != nil {
			mustClose(t, l)
		}
	}()
	var st unix.Stat_t
	if err := unix.Fstat(int(l.f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	ident := []string{
		"LOCK_DEV=" + strconv.FormatUint(uint64(st.Dev), 10),
		"LOCK_INO=" + strconv.FormatUint(st.Ino, 10),
	}
	if got := runHelper(t, "scan-fds", ident); got != "clean" {
		t.Fatalf("the child inherited the lock descriptor: %s", got)
	}
	if got := runHelper(t, "scan-fds", ident, l.f); got != "inherited" {
		t.Fatalf("control: the check cannot see an inherited descriptor: %s", got)
	}

	lockPath := []string{"LOCK_PATH=" + filepath.Join(dir, ".lock")}
	if got := runHelper(t, "try-lock", lockPath); got != "busy" {
		t.Fatalf("another process could take the held lock: %s", got)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l = nil
	if got := runHelper(t, "try-lock", lockPath); got != "acquired" {
		t.Fatalf("another process could not take the released lock: %s", got)
	}
}

// Concurrent use of the Root from several goroutines, with a final close:
// under -race it checks that the descriptor protection has no data race.
func TestRootConcurrent(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "f"), "x")
	var wg sync.WaitGroup
	deadline := time.Now().Add(200 * time.Millisecond)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if err := openClose(r, "f", os.O_RDONLY); err != nil && Code(err) != CodeRootClosed {
					t.Errorf("Open: %v", err)
					return
				}
				if _, err := r.ReadDir(""); err != nil && Code(err) != CodeRootClosed {
					t.Errorf("ReadDir: %v", err)
					return
				}
				if _, err := r.Stat(""); err != nil && Code(err) != CodeRootClosed {
					t.Errorf("Stat: %v", err)
					return
				}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

// The lifetime protocol of Root: Close waits for a syscall in flight on the
// root descriptor, but operations started after Close fail at once instead
// of queuing behind it; the descriptor is closed only when the last user is
// gone, and a concurrent second Close returns only after that.
func TestRootCloseWaitsForUsersAndRejectsNewOnes(t *testing.T) {
	r, _ := newRoot(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	// Cleanups run last-in first-out: on a failure the user is released
	// before newRoot's cleanup closes the root, which would otherwise wait
	// for it forever.
	t.Cleanup(doRelease)
	userDone := make(chan error, 1)
	go func() {
		userDone <- r.withFD("test", "", func(fd int) error {
			close(entered)
			<-release
			var st unix.Stat_t
			return unix.Fstat(fd, &st) // the descriptor must still be open
		})
	}()
	<-entered

	closed := make(chan error, 2)
	go func() { closed <- r.Close() }()
	go func() { closed <- r.Close() }()
	for !isClosing(r) {
		runtime.Gosched()
	}

	// New operations fail promptly while Close is waiting.
	err := callWithin(t, 5*time.Second, func() error { return openClose(r, "x", os.O_RDONLY) }, doRelease)
	wantCode(t, err, CodeRootClosed)
	select {
	case err := <-closed:
		t.Fatalf("Close returned while a syscall was using the descriptor: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	doRelease()
	if err := <-userDone; err != nil {
		t.Fatalf("the in-flight syscall saw a closed descriptor: %v", err)
	}
	for range 2 {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not return after the last user left")
		}
	}
	if r.fd != -1 {
		t.Fatalf("descriptor not released: %d", r.fd)
	}
}

func isClosing(r *Root) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closing
}

func TestClosedRoot(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	wantCode(t, openClose(r, "x", os.O_RDONLY), CodeRootClosed)
	wantCode(t, r.SyncDir(""), CodeRootClosed)
	wantCode(t, r.SyncDir("x"), CodeRootClosed)
	wantCode(t, r.Mkdir("x", 0o755), CodeRootClosed)
	wantCode(t, r.RemoveAll(t.Context(), "x"), CodeRootClosed)
	_, err = r.StatFS()
	wantCode(t, err, CodeRootClosed)
	_, err = r.Stat("")
	wantCode(t, err, CodeRootClosed)
	_, err = r.ReadDir("")
	wantCode(t, err, CodeRootClosed)
	_, err = r.MkdirAll("x/y", 0o755)
	wantCode(t, err, CodeRootClosed)
	_, err = r.SubRoot("x")
	wantCode(t, err, CodeRootClosed)
	_, err = r.Lock("x")
	wantCode(t, err, CodeRootClosed)
}
