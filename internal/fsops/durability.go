package fsops

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"musiclib/internal/names"
)

// SyncFile runs fsync on the open file and returns a typed package error.
//
// DESIGN.md §13.2 forbids ignoring fsync errors: this function exists so
// that the error has a stable code like all the others, not to duplicate
// (*os.File).Sync. It is meant for files opened by this package, whose name
// is a root label plus a relative path.
func SyncFile(f *os.File) error {
	if err := f.Sync(); err != nil {
		return &Error{Code: errnoCode(err), Op: "fsync", Path: f.Name(), Err: err}
	}
	return nil
}

// SyncAndClose runs fsync and then close, checking **both**.
//
// It is the closing sequence required by DESIGN.md §7.5 step 2 ("runs fsync
// on the open descriptor, checks close") and the only form that makes it
// impossible to lose either error by oversight: the file is closed even if
// the fsync fails, and neither error is discarded.
func SyncAndClose(f *os.File) error {
	return errors.Join(SyncFile(f), closeFile(f, "", f.Name()))
}

// closeFile closes an *os.File and types the error.
func closeFile(f *os.File, root, rel string) error {
	if err := f.Close(); err != nil {
		return &Error{Code: errnoCode(err), Op: "close", Root: root, Path: rel, Err: err}
	}
	return nil
}

// SyncDir runs fsync on a directory beneath the root; the empty path denotes
// the root itself.
//
// The fsync of a directory is what makes the creation, renaming and removal
// of the entries it contains durable (§7.5 step 5, §9.3).
func (r *Root) SyncDir(rel string) (err error) {
	if rel == "" {
		if err := r.withFD("fsync", "", func(fd int) error {
			return retryEINTR(func() error { return unix.Fsync(fd) })
		}); err != nil {
			return r.opErr("fsync", "", err)
		}
		return nil
	}
	fd, err := r.openDir(rel)
	if err != nil {
		return err
	}
	defer closeInto(&err, r.name, rel, fd)
	if err := retryEINTR(func() error { return unix.Fsync(fd) }); err != nil {
		return errnoErr("fsync", r.name, rel, err)
	}
	return nil
}

// SyncDirAndParents runs fsync on the given directory and then on all its
// parents, bottom-up, root included.
//
// It is the primitive required by DESIGN.md §9.1 step 8 ("fsync of all new
// files and directories, bottom-up, including the parent of the staging
// directory"), by §7.5 step 3 for the blob shard directories and by §9.3 for
// all directories involved in renames and rmdirs.
//
// The chain stops at the root: the durability of the root's own entry in its
// parent is outside the Root and is established when the volume is set up.
// It stops at the first error.
func (r *Root) SyncDirAndParents(rel string) error {
	segs, err := names.SplitRelPathOrRoot(rel)
	if err != nil {
		return err
	}
	for i := len(segs); i >= 0; i-- {
		if err := r.SyncDir(strings.Join(segs[:i], "/")); err != nil {
			return err
		}
	}
	return nil
}
