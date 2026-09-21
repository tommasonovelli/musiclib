package fsops

import (
	"context"
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"musiclib/internal/names"
)

const (
	// maxRemoveAllDepth bounds the recursion of RemoveAll, and therefore
	// the number of descriptors it holds at once. The application's
	// hierarchies never exceed 16 levels (§5.2); the limit guards against a
	// pathological tree found under work/.
	maxRemoveAllDepth = 64
	// maxRemoveAllPasses bounds the read/delete rounds on a single
	// directory: a directory that keeps refilling stops the removal instead
	// of spinning.
	maxRemoveAllPasses = 128
)

// Mkdir creates a directory beneath the root. If the name already exists,
// even as a symlink or a file, it fails with [CodeExists].
//
// The new entry is durable only after [Root.SyncDir] of its parent.
func (r *Root) Mkdir(rel string, perm os.FileMode) (err error) {
	dirfd, name, err := r.resolveParent(rel)
	if err != nil {
		return err
	}
	defer closeInto(&err, r.name, rel, dirfd)
	if err := retryEINTR(func() error { return unix.Mkdirat(dirfd, name, syscallMode(perm)) }); err != nil {
		return errnoErr("mkdirat", r.name, rel, err)
	}
	return nil
}

// MkdirAll creates the given directory and all missing parents, and returns
// the relative paths of the directories actually created, top-down. On error
// it also returns the directories created before the failure.
//
// A component that already exists must be a real directory: a symlink in
// place of an expected directory is an error ([CodeSymlink]), not a path to
// adopt (§9.3, §10.4). The descent happens one component at a time with
// openat2 relative to the directory just verified.
//
// The list of created directories is for callers that must make them
// durable: creating a directory becomes durable with the fsync of its parent
// (§7.5 step 3, §9.1 step 8). Callers that do not need the list use
// [Root.MkdirAllSync].
func (r *Root) MkdirAll(rel string, perm os.FileMode) (created []string, err error) {
	segs, err := names.SplitRelPath(rel)
	if err != nil {
		return nil, err
	}
	cur, err := r.openDir("")
	if err != nil {
		return nil, err
	}
	// cur is owned by this deferred close; every reassignment of cur hands
	// ownership of the new descriptor to it, after closing the old one.
	curRel := ""
	defer func() { closeInto(&err, r.name, curRel, cur) }()

	for i, seg := range segs {
		sub := strings.Join(segs[:i+1], "/")
		merr := retryEINTR(func() error { return unix.Mkdirat(cur, seg, syscallMode(perm)) })
		switch {
		case merr == nil:
			created = append(created, sub)
		case errors.Is(merr, unix.EEXIST):
			st, serr := statAt(cur, seg)
			if serr != nil {
				return created, errnoErr("fstatat", r.name, sub, serr)
			}
			if t := fileTypeFromMode(st.Mode); t != TypeDir {
				return created, errf(codeForType(t), "mkdirat", r.name, sub,
					"the path already exists and is a %s, not a directory", t)
			}
		default:
			return created, errnoErr("mkdirat", r.name, sub, merr)
		}
		if i == len(segs)-1 {
			break
		}
		// The entry may have been replaced since the check: openat2 refuses
		// a symlink (ELOOP) and a non-directory (ENOTDIR).
		next, oerr := sysOpenat2(cur, seg, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if oerr != nil {
			return created, errnoErr("openat2", r.name, sub, oerr)
		}
		prev, prevRel := cur, curRel
		cur, curRel = next, sub
		if cerr := closeFD(r.name, prevRel, prev); cerr != nil {
			return created, cerr
		}
	}
	return created, nil
}

// MkdirAllSync creates the chain of directories and makes it durable: fsync
// of the given directory and of all its parents, bottom-up up to and
// including the root (§7.5 step 3, §9.1 step 8).
//
// If no directory was created there is nothing to make durable and no fsync
// is performed.
func (r *Root) MkdirAllSync(rel string, perm os.FileMode) error {
	created, err := r.MkdirAll(rel, perm)
	if err != nil {
		return err
	}
	if len(created) == 0 {
		return nil
	}
	return r.SyncDirAndParents(rel)
}

// Rmdir removes an empty directory. A non-empty directory yields
// [CodeDirNotEmpty].
//
// DESIGN.md §3.3: artist directories are removed only with rmdir, when
// empty. There is no variant that empties the directory in order to succeed.
func (r *Root) Rmdir(rel string) (err error) {
	dirfd, name, err := r.resolveParent(rel)
	if err != nil {
		return err
	}
	defer closeInto(&err, r.name, rel, dirfd)
	if err := retryEINTR(func() error { return unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR) }); err != nil {
		return errnoErr("unlinkat", r.name, rel, err)
	}
	return nil
}

// Remove removes an entry that is not a directory. A directory yields
// [CodeIsDirectory]: use [Root.Rmdir] or [Root.RemoveAll].
//
// The check and the removal are one syscall: unlinkat without AT_REMOVEDIR
// fails with EISDIR on a directory, so there is no window in which a
// directory could be swapped in.
//
// A symlink is removed, not followed: unlinkat operates on the name inside
// an already resolved descriptor and cannot reach the link's target.
// Refusing to remove it would leave in work/ forever an entry that the
// application did not create but must be able to clean up (§9.3).
func (r *Root) Remove(rel string) (err error) {
	dirfd, name, err := r.resolveParent(rel)
	if err != nil {
		return err
	}
	defer closeInto(&err, r.name, rel, dirfd)
	if err := retryEINTR(func() error { return unix.Unlinkat(dirfd, name, 0) }); err != nil {
		return errnoErr("unlinkat", r.name, rel, err)
	}
	return nil
}

// RemoveAll recursively removes an entry and its contents. A missing path,
// or a missing parent, is not an error. The empty path (the root itself) is
// rejected by validation.
//
// Deletion is confined like every other operation: the path is resolved once
// with openat2, the descent happens only with openat2 relative to already
// open descriptors, and every unlinkat acts on a name inside an open
// directory. It never enters a symlink; a symlink found in the tree is
// removed as an entry, never traversed. There is no equivalent of
// os.RemoveAll on a path built from strings.
//
// Entries replaced concurrently (a file by a directory or the reverse) are
// handled by their actual type; a directory that keeps refilling stops the
// removal with [CodeIO]. On error the tree may be partially removed.
//
// It is used to clean up work/ after a publication (§9.3) and by rebuild
// (§11.3): it is a long operation, so it takes a context (§13.2).
func (r *Root) RemoveAll(ctx context.Context, rel string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	dirfd, name, err := r.resolveParent(rel)
	if err != nil {
		if Code(err) == CodeNotFound {
			return nil
		}
		return err
	}
	defer closeInto(&err, r.name, rel, dirfd)

	st, err := statAt(dirfd, name)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return errnoErr("fstatat", r.name, rel, err)
	}
	return r.removeEntry(ctx, dirfd, name, rel, fileTypeFromMode(st.Mode) == TypeDir, 0)
}

// removeEntry removes name from dirfd (owned by the caller). isDir is the
// type observed when the entry was listed; if the entry has been replaced by
// the other kind in the meantime, the kernel's answer wins.
func (r *Root) removeEntry(ctx context.Context, dirfd int, name, rel string, isDir bool, depth int) error {
	if !isDir {
		err := retryEINTR(func() error { return unix.Unlinkat(dirfd, name, 0) })
		switch {
		case err == nil, errors.Is(err, unix.ENOENT):
			return nil
		case !errors.Is(err, unix.EISDIR):
			return errnoErr("unlinkat", r.name, rel, err)
		}
		// Replaced by a directory: remove it as such.
	}
	return r.removeDir(ctx, dirfd, name, rel, depth)
}

// removeDir removes the directory name of dirfd (owned by the caller) and
// everything it contains.
func (r *Root) removeDir(ctx context.Context, dirfd int, name, rel string, depth int) error {
	sub, err := sysOpenat2(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		// Replaced by a symlink or a file: remove the entry, never follow it.
		uerr := retryEINTR(func() error { return unix.Unlinkat(dirfd, name, 0) })
		if uerr != nil && !errors.Is(uerr, unix.ENOENT) {
			return errnoErr("unlinkat", r.name, rel, uerr)
		}
		return nil
	case err != nil:
		return errnoErr("openat2", r.name, rel, err)
	}
	if err := r.removeContents(ctx, sub, rel, depth+1); err != nil {
		return errors.Join(err, closeFD(r.name, rel, sub))
	}
	if err := closeFD(r.name, rel, sub); err != nil {
		return err
	}
	uerr := retryEINTR(func() error { return unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR) })
	if uerr != nil && !errors.Is(uerr, unix.ENOENT) {
		return errnoErr("unlinkat", r.name, rel, uerr)
	}
	return nil
}

// removeContents empties a directory descriptor owned by the caller.
func (r *Root) removeContents(ctx context.Context, dirfd int, rel string, depth int) error {
	if depth > maxRemoveAllDepth {
		return errf(CodeTooDeep, "removeall", r.name, rel,
			"the tree exceeds %d levels", maxRemoveAllDepth)
	}
	for pass := 0; pass < maxRemoveAllPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := readDirFD(dirfd)
		if err != nil {
			return errnoErr("getdents", r.name, rel, err)
		}
		if len(entries) == 0 {
			return nil
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := r.removeEntry(ctx, dirfd, e.Name, rel+"/"+e.Name, e.Type == TypeDir, depth); err != nil {
				return err
			}
		}
	}
	return errf(CodeIO, "removeall", r.name, rel,
		"the directory keeps refilling after %d passes", maxRemoveAllPasses)
}
