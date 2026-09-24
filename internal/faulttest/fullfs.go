package faulttest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	// fullFSEnv names the mount point of the small fixed-size filesystem
	// the Compose test services mount for the full-disk tests (compose.yaml,
	// docs/docker.md, NOTES.md N-045).
	fullFSEnv = "MUSICLIB_FULLFS"
	// requireFullFSEnv makes a missing filesystem a failure instead of a
	// skip: the gate sets it, so these tests never silently vanish.
	requireFullFSEnv = "MUSICLIB_REQUIRE_FULLFS"
	// maxFullFS bounds the size of a filesystem FullFS agrees to fill: it
	// refuses anything larger, which cannot be the dedicated one and may be
	// shared with other tests or with the host.
	maxFullFS = 2 << 30
	lockName  = ".lock"
	ballast   = ".ballast"
)

// Disk is a fresh directory on the small dedicated filesystem, held by one
// test at a time (an exclusive flock serializes the test binaries of
// different packages, which go test runs in parallel), and a ballast file
// that fills the filesystem to an exact level.
type Disk struct {
	// Dir is the test's empty directory on the filesystem.
	Dir  string
	root string
	bal  *os.File
}

// FullFS takes the dedicated filesystem for the test and returns a fresh
// directory on it; everything is released at the end of the test. It skips
// when the filesystem is not configured, unless MUSICLIB_REQUIRE_FULLFS=1
// (the gate), which fails instead. Before it removes or fills anything it
// requires the root to be a mount point, of at most 2 GiB, and not the
// filesystem of TMPDIR: filling another could hurt other tests.
func FullFS(t testing.TB) *Disk {
	t.Helper()
	root := os.Getenv(fullFSEnv)
	if root == "" {
		if os.Getenv(requireFullFSEnv) == "1" {
			t.Fatalf("%s is not set, and %s=1: the full-disk tests must run (docs/docker.md)", fullFSEnv, requireFullFSEnv)
		}
		t.Skipf("%s is not set: run the full-disk tests in Docker (scripts/check.sh, docs/docker.md)", fullFSEnv)
	}
	if err := checkDedicated(root); err != nil {
		t.Fatalf("%s=%s: %v", fullFSEnv, root, err)
	}
	lock, err := os.OpenFile(filepath.Join(root, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(errors.Join(err, lock.Close()))
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil { // closing releases the flock
			t.Error(err)
		}
	})
	// Under the lock nothing else uses the filesystem: what is there is
	// the leftover of a test binary that was killed.
	if err := clean(root); err != nil {
		t.Fatal(err)
	}
	d := &Disk{root: root}
	if d.Dir, err = os.MkdirTemp(root, "t-"); err != nil {
		t.Fatal(err)
	}
	if d.bal, err = os.OpenFile(filepath.Join(root, ballast), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(d.bal.Close(), clean(root)); err != nil {
			t.Error(err)
		}
	})
	return d
}

// checkDedicated refuses a root that is not a mount point, a filesystem
// that is not small, and the one that holds TMPDIR.
func checkDedicated(root string) error {
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		return err
	}
	var st, parent, tmp unix.Stat_t
	if err := unix.Stat(root, &st); err != nil {
		return err
	}
	if err := unix.Stat(filepath.Join(root, ".."), &parent); err != nil {
		return err
	}
	if err := unix.Stat(os.TempDir(), &tmp); err != nil {
		return err
	}
	return dedicated(int64(fs.Blocks)*fs.Bsize, uint64(st.Dev), uint64(parent.Dev), uint64(tmp.Dev))
}

// dedicated is the decision of checkDedicated, each guard on its own:
// the root must be a mount point (its device differs from its parent's: a
// positive sign that it is the filesystem mounted for these tests, not a
// directory of a shared one), small, and not TMPDIR's filesystem. All three
// are checked before anything is removed or filled.
func dedicated(total int64, rootDev, parentDev, tmpDev uint64) error {
	switch {
	case rootDev == parentDev:
		return fmt.Errorf("it is not a mount point: not the dedicated filesystem")
	case total > maxFullFS:
		return fmt.Errorf("the filesystem has %d bytes, more than %d: it is not the dedicated one", total, maxFullFS)
	case rootDev == tmpDev:
		return fmt.Errorf("it is the filesystem of TMPDIR")
	}
	return nil
}

// clean removes everything in root but the lock file.
func clean(root string) error {
	ents, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.Name() == lockName {
			continue
		}
		p := filepath.Join(root, e.Name())
		// Test trees may hold read-only directories.
		if err := filepath.WalkDir(p, func(q string, de os.DirEntry, err error) error {
			if err == nil && de.IsDir() {
				err = os.Chmod(q, 0o700)
			}
			return err
		}); err != nil {
			return err
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// Available is the space an unprivileged process can still allocate
// (statfs f_bavail).
func (d *Disk) Available() (int64, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(d.root, &fs); err != nil {
		return 0, err
	}
	return int64(fs.Bavail) * fs.Bsize, nil
}

// BlockSize is the filesystem's allocation unit.
func (d *Disk) BlockSize() (int64, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(d.root, &fs); err != nil {
		return 0, err
	}
	return fs.Bsize, nil
}

// Fill resizes the ballast, with really allocated blocks (fallocate), so
// that exactly leave bytes stay available, rounded down to the block size.
// It returns an error rather than failing the test, so that a failpoint
// hook running in a worker may call it.
func (d *Disk) Fill(leave int64) error {
	avail, err := d.Available()
	if err != nil {
		return err
	}
	bs, err := d.BlockSize()
	if err != nil {
		return err
	}
	leave -= leave % bs
	fi, err := d.bal.Stat()
	if err != nil {
		return err
	}
	target := fi.Size() + avail - leave
	switch {
	case target < 0:
		return fmt.Errorf("cannot leave %d bytes: only %d are free without the ballast", leave, avail+fi.Size())
	case target < fi.Size():
		err = d.bal.Truncate(target)
	default:
		err = unix.Fallocate(int(d.bal.Fd()), 0, 0, target)
	}
	if err != nil {
		return fmt.Errorf("resizing the ballast to %d bytes: %w", target, err)
	}
	if got, err := d.Available(); err != nil || got != leave {
		return fmt.Errorf("after the ballast %d bytes are available, want %d (%v)", got, leave, err)
	}
	return nil
}

// Drain gives back all the ballast's space.
func (d *Disk) Drain() error {
	return d.bal.Truncate(0)
}
