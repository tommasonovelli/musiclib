// Package fsops holds the application's filesystem primitives, confined to
// their roots (DESIGN.md §2.3, §10.4). It is the only package allowed to
// touch the filesystem.
//
// Every disk access goes through a [Root], a directory descriptor opened
// once, and every path is turned into a descriptor by openat2(2) with
// RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS. Confinement is structural, not a
// convention:
//
//   - after [OpenRoot] no function of the package accepts an absolute path,
//     and filepath.Join(root, input) appears nowhere (§10.4);
//   - the only function that opens anything relative to a descriptor is
//     sysOpenat2, and the resolution flags are not a parameter: the kernel
//     rejects, in a single syscall and with no TOCTOU window, every
//     resolution that would escape the starting descriptor and every
//     symlink it meets, even as an intermediate component;
//   - single-component operations (mkdirat, unlinkat, fstatat, renameat2)
//     receive an already resolved directory descriptor and a name without
//     "/": they work on the inode, not on a string that is traversed again.
//
// TestNoOperationOnStringBuiltPaths enforces these rules on the source.
//
// Paths are relative to the root and are validated with [names.SplitRelPath]
// (§5.2) **before** touching the disk: absolute paths, "." and "..", empty
// segments, invalid UTF-8 and NUL bytes are validation errors, not failed
// syscalls. Validation does not replace confinement, it comes first: openat2
// would reject the escaping paths anyway.
//
// Linux 5.6 or later and ext4 are requirements (§3.1): if openat2 is not
// usable, [OpenRoot] fails with [CodeUnsupportedKernel] and the application
// must refuse to start. There is no fallback with weaker guarantees.
//
// Symlinks, FIFOs, sockets and devices are rejected wherever something is
// opened or traversed (§5.2, §9.3), and rejecting them never blocks: every
// open carries O_NONBLOCK until the type of the opened inode is verified.
// [Root.Stat] and [Root.ReadDir] instead report them as a type, because the
// scan must be able to list them in its report before rejecting them.
//
// Descriptor ownership: every descriptor the package opens has exactly one
// owner and is closed exactly once, by closeFD, and a close error is never
// discarded (§13.2). Descriptors handed to the caller (*os.File, [Root],
// [Lock]) are owned by the caller from then on.
package fsops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"musiclib/internal/names"
)

// resolveFlags are the resolution flags used by **every** openat2 in the
// package (DESIGN.md §10.4).
//
// RESOLVE_BENEATH: resolution cannot escape the starting directory
// descriptor, not even with "..".
// RESOLVE_NO_SYMLINKS: no symlink is followed, neither as an intermediate
// component nor as the final one; it implies RESOLVE_NO_MAGICLINKS.
const resolveFlags = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS

// alwaysOpenFlags are added to every open of the package.
//
// O_CLOEXEC: child tools (musiclib-tags, ffmpeg) must not inherit the
// application's descriptors, starting with the volume flock (§11.1).
// O_NONBLOCK: opening a FIFO or a device must never block before the type of
// the opened inode is checked (N-013); [Root.OpenFile] clears it once the
// inode is known to be a regular file. It has no effect on directories.
// O_NOCTTY: opening a terminal device must never make it the controlling
// terminal of the process.
const alwaysOpenFlags = unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOCTTY

// Root is a filesystem root: a directory descriptor opened once, beneath
// which every access is confined.
//
// It is safe for use by multiple goroutines, and it does not serialize I/O.
// The mutex only guards the descriptor's lifetime: a descriptor number
// recycled after Close would make the primitives operate on an unrelated
// file. The mutex is never held across a syscall; instead, every syscall
// that uses the root descriptor registers as a user, and Close closes the
// descriptor only after the last user is gone. Operations started after
// Close fail at once with [CodeRootClosed] instead of queuing behind it.
//
// Each registered use is a single syscall that cannot block indefinitely by
// construction (openat2 with O_NONBLOCK, fstat, fstatfs, fsync), so Close
// waits for a bounded time. Operations that resolved their own descriptors
// before Close (for example a RemoveAll in progress) keep running on those
// descriptors, which they own; cancel them with their context.
type Root struct {
	// name is a label for error messages (for example "library"), never an
	// absolute path (DESIGN.md §13.2). Immutable.
	name string

	mu sync.Mutex
	// idle is signalled, with mu held, when users drops to zero after
	// closing is set and when the descriptor has been closed.
	idle sync.Cond
	// fd is the root's descriptor; -1 once closed. Invariant: while
	// users > 0, fd is open and is not closed.
	fd int
	// users counts the syscalls currently using fd.
	users int
	// closing is set by the first Close: no new user is admitted.
	closing bool
}

// newRootFD takes ownership of fd, an open directory descriptor.
func newRootFD(fd int, name string) *Root {
	r := &Root{fd: fd, name: name}
	r.idle.L = &r.mu
	return r
}

// OpenRoot opens path as a confined root. It is the only place in the
// package that accepts an absolute path: roots are fixed configuration paths
// (/data, /import — §11.1), everything else is relative to a Root.
//
// The root's label, used in error messages, is the last element of path.
//
// It checks right away that openat2 is usable: a kernel older than Linux
// 5.6, or a seccomp filter that blocks the syscall, yields
// [CodeUnsupportedKernel] (§3.1).
func OpenRoot(path string) (*Root, error) {
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) {
		name = path
	}
	fd, err := retryEINTR2(func() (int, error) {
		return unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|alwaysOpenFlags, 0)
	})
	if err != nil {
		return nil, &Error{Code: errnoCode(err), Op: "open", Root: name, Err: err}
	}
	if err := probeOpenat2(fd, name); err != nil {
		return nil, errors.Join(err, closeFD(name, "", fd))
	}
	return newRootFD(fd, name), nil
}

// probeOpenat2 checks that openat2 with the confinement flags works on the
// root descriptor. DESIGN.md §3.1: no silent degradation.
func probeOpenat2(rootfd int, name string) error {
	fd, err := sysOpenat2(rootfd, ".", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	switch {
	case err == nil:
		return closeFD(name, "", fd)
	case errors.Is(err, unix.ENOSYS):
		return errf(CodeUnsupportedKernel, "openat2", name, "",
			"openat2(2) is not available: Linux 5.6 or later is required")
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EINVAL), errors.Is(err, unix.EOPNOTSUPP):
		return errf(CodeUnsupportedKernel, "openat2", name, "",
			"openat2(2) is rejected by the system (%v): kernel too old "+
				"or a seccomp profile blocking the syscall", err)
	default:
		return errnoErr("openat2", name, "", err)
	}
}

// SubRoot opens a subdirectory as an independent Root, resolving it with the
// confinement flags of the current root. The two Roots have independent
// lifetimes: closing one does not close the other.
//
// It is how the working roots (library, work, originals) are obtained without
// reintroducing absolute paths: /data and /import remain the application's
// only two absolute paths (§3.1, §13.2).
func (r *Root) SubRoot(rel string) (*Root, error) {
	if _, err := relPath(rel); err != nil {
		return nil, err // a sub-root of "" would be a second handle on r
	}
	fd, err := r.openDir(rel)
	if err != nil {
		return nil, err
	}
	return newRootFD(fd, location(r.name, rel)), nil
}

// Name is the root's label used in error messages. It is not a filesystem
// path and must not be used to build one.
func (r *Root) Name() string { return r.name }

// Close closes the root's descriptor. It waits for the syscalls that are
// using it (see [Root]); operations started afterwards fail with
// [CodeRootClosed]. Calling it again is not an error: it returns nil once the
// descriptor is closed.
func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		for r.fd >= 0 {
			r.idle.Wait()
		}
		return nil
	}
	r.closing = true
	for r.users > 0 {
		r.idle.Wait()
	}
	fd := r.fd
	r.fd = -1
	r.idle.Broadcast()
	return closeFD(r.name, "", fd)
}

// withFD runs fn, a single syscall, on the root's descriptor, keeping the
// descriptor open for its duration. It is the only way to use r.fd.
func (r *Root) withFD(op, rel string, fn func(fd int) error) error {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return errf(CodeRootClosed, op, r.name, rel, "the root is already closed")
	}
	r.users++
	fd := r.fd
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.users--
		if r.users == 0 && r.closing {
			r.idle.Broadcast()
		}
		r.mu.Unlock()
	}()
	return fn(fd)
}

// sysOpenat2 is the only function of the package that opens a path relative
// to a descriptor. The confinement flags and alwaysOpenFlags are not
// parameters. It returns the raw errno; callers type it.
func sysOpenat2(dirfd int, p string, flags int, mode uint32) (int, error) {
	how := unix.OpenHow{
		Flags:   uint64(flags) | alwaysOpenFlags,
		Mode:    uint64(mode),
		Resolve: resolveFlags,
	}
	return retryEINTR2(func() (int, error) { return unix.Openat2(dirfd, p, &how) })
}

// openat2 resolves an already validated path beneath the root and returns a
// new descriptor owned by the caller, or the raw errno.
func (r *Root) openat2(p string, flags int, mode uint32) (int, error) {
	fd := -1
	err := r.withFD("openat2", p, func(rootfd int) error {
		var err error
		fd, err = sysOpenat2(rootfd, p, flags, mode)
		return err
	})
	return fd, err
}

// openDir opens a directory beneath the root; the empty path denotes the root
// itself. The descriptor is owned by the caller. A non-directory yields
// [CodeNotDirectory] (O_DIRECTORY is checked before any device or FIFO open
// routine runs), a symlink [CodeSymlink].
func (r *Root) openDir(rel string) (int, error) {
	p, err := relPathOrRoot(rel)
	if err != nil {
		return -1, err
	}
	fd, err := r.openat2(p, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, r.openErr(rel, err)
	}
	return fd, nil
}

// resolveParent opens the directory that contains rel and returns its
// descriptor, owned by the caller, plus the name of the last segment.
//
// It is the form that mkdirat, unlinkat, fstatat and renameat2 build on,
// since they accept no resolution flags: the path is resolved only once by
// openat2, and the following operation works on a descriptor and on a name
// without "/". Replacing a directory of the path after resolution does not
// move the operation elsewhere, because the descriptor keeps referring to the
// resolved inode.
func (r *Root) resolveParent(rel string) (int, string, error) {
	segs, err := names.SplitRelPath(rel)
	if err != nil {
		return -1, "", err
	}
	name := segs[len(segs)-1]
	parent := strings.Join(segs[:len(segs)-1], "/")
	if parent == "" {
		parent = "."
	}
	fd, err := r.openat2(parent, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, "", r.openErr(rel, err)
	}
	return fd, name, nil
}

// openErr wraps a resolution error, passing through unchanged the errors
// already typed by the package (closed root, invalid path).
func (r *Root) openErr(rel string, err error) error {
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	return errnoErr("openat2", r.name, rel, err)
}

// relPath validates a non-empty relative path and returns the form to pass
// to openat2, rebuilt from the validated segments.
func relPath(rel string) (string, error) {
	segs, err := names.SplitRelPath(rel)
	if err != nil {
		return "", err
	}
	return strings.Join(segs, "/"), nil
}

// relPathOrRoot is relPath, but the empty path denotes the root itself and
// becomes ".".
func relPathOrRoot(rel string) (string, error) {
	if rel == "" {
		return ".", nil
	}
	return relPath(rel)
}

// closeFD closes a descriptor the caller owns and returns a typed error:
// DESIGN.md §13.2 forbids ignoring close errors. It is the only call site of
// close(2) in the package.
//
// Linux releases the descriptor number even when close fails, so a close is
// never retried, not even on EINTR: the number may already belong to another
// goroutine.
func closeFD(root, path string, fd int) error {
	if err := unix.Close(fd); err != nil {
		return errnoErr("close", root, path, err)
	}
	return nil
}

// closeInto closes fd and joins a close error into *errp. It is used as
// `defer closeInto(&err, ...)` right after a descriptor is obtained, so that
// the descriptor has exactly one owner and exactly one close on every path.
func closeInto(errp *error, root, path string, fd int) {
	if cerr := closeFD(root, path, fd); cerr != nil {
		*errp = errors.Join(*errp, cerr)
	}
}

// syscallMode translates the permission bits of os.FileMode into the syscall
// mode, setuid/setgid/sticky included.
func syscallMode(m os.FileMode) uint32 {
	o := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		o |= unix.S_ISUID
	}
	if m&os.ModeSetgid != 0 {
		o |= unix.S_ISGID
	}
	if m&os.ModeSticky != 0 {
		o |= unix.S_ISVTX
	}
	return o
}

// retryEINTR retries a syscall interrupted by a signal. The Go runtime's
// asynchronous preemption sends SIGURG frequently: an EINTR that is not
// retried would be a sporadic, non-reproducible error. It must not wrap
// close(2) (see closeFD).
func retryEINTR(fn func() error) error {
	for {
		err := fn()
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// retryEINTR2 is retryEINTR for syscalls that also return a value.
func retryEINTR2[T any](fn func() (T, error)) (T, error) {
	for {
		v, err := fn()
		if !errors.Is(err, unix.EINTR) {
			return v, err
		}
	}
}
