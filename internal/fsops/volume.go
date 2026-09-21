package fsops

import (
	"errors"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// FSInfo describes the space of the filesystem a root lives on.
//
// FreeBytes are the bytes available to an **unprivileged** process
// (f_bavail), not the absolute free bytes: the application does not run as
// root (§11.1), so this is the only amount it can actually spend.
type FSInfo struct {
	BlockSize  int64
	TotalBytes int64
	FreeBytes  int64
}

// StatFS reads the space of the root's filesystem.
//
// DESIGN.md §11.2: before import and build a conservative estimate is made
// and checked with statfs and a 1 GiB margin. The margin and the process
// budget are domain decisions and live elsewhere: here there is only the
// measurement.
func (r *Root) StatFS() (FSInfo, error) {
	var st unix.Statfs_t
	if err := r.withFD("fstatfs", "", func(fd int) error {
		return retryEINTR(func() error { return unix.Fstatfs(fd, &st) })
	}); err != nil {
		return FSInfo{}, r.opErr("fstatfs", "", err)
	}
	// f_frsize is the block size in which f_blocks and f_bavail are
	// expressed; on ext4 it equals f_bsize. If the filesystem does not
	// report it, f_bsize is used.
	unitSize := st.Frsize
	if unitSize <= 0 {
		unitSize = st.Bsize
	}
	return FSInfo{
		BlockSize:  st.Bsize,
		TotalBytes: int64(st.Blocks) * unitSize,
		FreeBytes:  int64(st.Bavail) * unitSize,
	}, nil
}

// Lock is an exclusive flock on a file of the root, held until it is
// released with [Lock.Close].
type Lock struct {
	root, rel string // for error messages only

	mu sync.Mutex
	f  *os.File // nil once released
}

// Lock acquires an **exclusive, non-blocking** flock on the given file,
// creating it if missing.
//
// DESIGN.md §2.2 and §11.1: a single application instance per volume; the
// lock on /data/.lock is acquired before starting workers or maintenance and
// held until termination. If the lock is already held by another process the
// error is [CodeLockBusy]: there is no waiting and no retrying, because a
// second instance must not start.
//
// The lock belongs to the open file description, so a second Lock on the
// same file fails even within the same process. The descriptor has
// O_CLOEXEC: a child tool (musiclib-tags, ffmpeg, pg_dump) that inherited it
// would keep the volume locked after the application exits, and could
// release it with LOCK_UN.
//
// The file is not removed on release: deleting it would let two processes
// hold locks on different inodes with the same name.
func (r *Root) Lock(rel string) (*Lock, error) {
	f, err := r.OpenFile(rel, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	lerr := retryEINTR(func() error {
		return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	})
	if lerr == nil {
		return &Lock{f: f, root: r.name, rel: rel}, nil
	}
	e := errnoErr("flock", r.name, rel, lerr)
	if errors.Is(lerr, unix.EWOULDBLOCK) {
		e.Code = CodeLockBusy
	}
	return nil, errors.Join(e, closeFile(f, r.name, rel))
}

// Close releases the lock and closes the descriptor, checking both. Calling
// it again is not an error.
func (l *Lock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	var uerr error
	if err := retryEINTR(func() error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }); err != nil {
		uerr = errnoErr("flock", l.root, l.rel, err)
	}
	return errors.Join(uerr, closeFile(f, l.root, l.rel))
}
