package fsops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"

	"golang.org/x/sys/unix"
)

// RenameNoReplace moves srcRel of src to dstRel of dst with
// renameat2(RENAME_NOREPLACE): if the destination exists, whatever its type,
// the operation fails with [CodeExists] and touches neither side.
//
// It is the primitive used to commit a blob (§7.5 step 4), to install a new
// album and to retire a published directory (§9.3). src and dst may be the
// same Root or two different Roots on the same mounted filesystem: moving
// the staging directory from work/ to library/ is exactly the latter case.
// Different filesystems yield [CodeCrossDevice]; moving a directory into its
// own subtree yields [CodeInvalidArgument].
//
// Source and destination parents are resolved with openat2 and the
// confinement flags; the rename then acts on two directory descriptors and
// two names without "/". The source must be a regular file or a directory:
// symlinks and special files are rejected (§9.3).
//
// The rename does **not** run fsync: the directories to sync depend on the
// sequence in progress and must be made durable by the caller with
// [Root.SyncDirAndParents] on source and destination (§7.5 step 5, §9.3).
func RenameNoReplace(src *Root, srcRel string, dst *Root, dstRel string) error {
	return rename(src, srcRel, dst, dstRel, unix.RENAME_NOREPLACE)
}

// RenameExchange atomically swaps two existing entries with
// renameat2(RENAME_EXCHANGE).
//
// It is the primitive for replacement at the same path (§9.3, INSTALL
// phase): after the swap the old album sits in the staging directory. Both
// entries must exist and be regular files or directories.
//
// Like [RenameNoReplace], it does not run fsync.
func RenameExchange(a *Root, aRel string, b *Root, bRel string) error {
	return rename(a, aRel, b, bRel, unix.RENAME_EXCHANGE)
}

// rename is the single implementation of renames: it resolves the two
// parents, checks the types and calls renameat2.
//
// The type check precedes the syscall because renameat2 offers no flag
// equivalent to O_NOFOLLOW. It is not an escape window: both names stay
// inside descriptors already resolved beneath their respective roots, and
// renameat2 moves directory entries without following them, so the worst
// outcome of a concurrent replacement is moving an unexpected entry **within**
// the roots, which the publication preflight detects (§9.3). No path can
// escape the roots.
func rename(src *Root, srcRel string, dst *Root, dstRel string, flag uint) (err error) {
	srcFd, srcName, err := src.resolveParent(srcRel)
	if err != nil {
		return err
	}
	defer closeInto(&err, src.name, srcRel, srcFd)
	dstFd, dstName, err := dst.resolveParent(dstRel)
	if err != nil {
		return err
	}
	defer closeInto(&err, dst.name, dstRel, dstFd)

	if err := checkRenameable(src, srcRel, srcFd, srcName); err != nil {
		return err
	}
	if flag&unix.RENAME_EXCHANGE != 0 {
		if err := checkRenameable(dst, dstRel, dstFd, dstName); err != nil {
			return err
		}
	}
	if err := retryEINTR(func() error {
		return unix.Renameat2(srcFd, srcName, dstFd, dstName, flag)
	}); err != nil {
		return renameErr(src.name, srcRel, dst.name, dstRel, err)
	}
	return nil
}

// renameErr types a renameat2 error. The errno refers to the pair, so the
// error carries both locations. The paths have already been resolved with
// openat2, so a remaining EXDEV means that the two sides are on different
// mounted filesystems (DESIGN.md §3.1), not an escape attempt.
func renameErr(srcRoot, srcRel, dstRoot, dstRel string, err error) *Error {
	e := errnoErr("renameat2", srcRoot, srcRel, err)
	e.DstRoot, e.DstPath = dstRoot, dstRel
	if errors.Is(err, unix.EXDEV) {
		e.Code = CodeCrossDevice
	}
	return e
}

// checkRenameable rejects symlinks and special files before a rename
// (DESIGN.md §9.3: symlinks and special files are always rejected).
func checkRenameable(r *Root, rel string, dirfd int, name string) error {
	st, err := statAt(dirfd, name)
	if err != nil {
		return errnoErr("fstatat", r.name, rel, err)
	}
	if t := fileTypeFromMode(st.Mode); t.IsSpecial() {
		return errf(codeForType(t), "renameat2", r.name, rel,
			"the entry is a %s: symlinks and special files are not renamed", t)
	}
	return nil
}

// SameFilesystem reports whether two roots live on the same filesystem, by
// comparing st_dev.
//
// DESIGN.md §3.1: at boot the application checks that library and work share
// the filesystem. Equal st_dev is necessary but **not sufficient** for a
// rename: two bind mounts of the same filesystem share st_dev, yet rename(2)
// between them fails with EXDEV. [ProbeRenameExchange] is the authoritative
// check and must run at boot as well.
func SameFilesystem(a, b *Root) (bool, error) {
	da, err := a.Stat("")
	if err != nil {
		return false, err
	}
	db, err := b.Stat("")
	if err != nil {
		return false, err
	}
	return da.Dev == db.Dev, nil
}

// probeDirPrefix is the prefix of the probe's temporary directories. A
// leftover after a crash during the probe is visible and harmless: doctor
// reports it as an extra entry (§11.3).
const probeDirPrefix = ".musiclib-probe-"

// ProbeRenameExchange checks on the real volume that renameat2 with
// RENAME_EXCHANGE works between the two roots, as required by DESIGN.md §3.1.
//
// It does not merely check that the syscall does not fail: it creates a test
// directory in each root, puts distinguishable content in it, performs the
// swap and checks that the contents were actually swapped. It is the same
// operation that the INSTALL phase performs between staging and library
// (§9.3), so it also catches two roots that share st_dev but not the mount.
//
// Errors: [CodeCrossDevice] if the roots are not on the same mounted
// filesystem; [CodeUnsupportedOp] if the volume does not offer
// RENAME_EXCHANGE. In both cases the application must refuse to start: ext4
// and Linux are requirements, with no fallback offering weaker guarantees.
//
// The test directories are removed on every path that created them, success
// or failure; cleanup uses context.Background() because it is a boot check
// that takes milliseconds and must not leave leftovers when something else
// is being cancelled.
func ProbeRenameExchange(a, b *Root) (err error) {
	same, err := SameFilesystem(a, b)
	if err != nil {
		return err
	}
	if !same {
		return errf(CodeCrossDevice, "probe", a.name, "",
			"%s and %s are not on the same filesystem", a.name, b.name)
	}
	suffix, err := probeSuffix()
	if err != nil {
		return err
	}
	nameA := probeDirPrefix + "a" + suffix
	nameB := probeDirPrefix + "b" + suffix

	// Each cleanup is registered as soon as its directory exists, before
	// anything else can fail.
	if err := a.Mkdir(nameA, 0o700); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, a.RemoveAll(context.Background(), nameA)) }()
	if err := b.Mkdir(nameB, 0o700); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, b.RemoveAll(context.Background(), nameB)) }()
	if err := writeProbeMarker(a, nameA+"/marker", "a"); err != nil {
		return err
	}
	if err := writeProbeMarker(b, nameB+"/marker", "b"); err != nil {
		return err
	}

	if err := RenameExchange(a, nameA, b, nameB); err != nil {
		// Neither directory is an ancestor of the other, so EINVAL can only
		// mean that the filesystem does not support the flag.
		if Code(err) == CodeInvalidArgument {
			var e *Error
			if errors.As(err, &e) {
				e.Code = CodeUnsupportedOp
			}
		}
		return err
	}
	// After the swap the directory of a must contain the marker of b and
	// vice versa: this is the proof that the swap really happened.
	if err := checkProbeMarker(a, nameA+"/marker", "b"); err != nil {
		return err
	}
	return checkProbeMarker(b, nameB+"/marker", "a")
}

func probeSuffix() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", errf(CodeIO, "probe", "", "", "cannot generate a probe name: %v", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

func writeProbeMarker(r *Root, rel, marker string) error {
	f, err := r.CreateExclusive(rel, 0o600)
	if err != nil {
		return err
	}
	if _, werr := io.WriteString(f, marker); werr != nil {
		return errors.Join(
			&Error{Code: errnoCode(werr), Op: "write", Root: r.name, Path: rel, Err: werr},
			closeFile(f, r.name, rel))
	}
	return closeFile(f, r.name, rel)
}

func checkProbeMarker(r *Root, rel, want string) error {
	f, err := r.Open(rel)
	if err != nil {
		return err
	}
	got, rerr := io.ReadAll(f)
	cerr := closeFile(f, r.name, rel)
	if rerr != nil {
		return errors.Join(
			&Error{Code: errnoCode(rerr), Op: "read", Root: r.name, Path: rel, Err: rerr}, cerr)
	}
	if cerr != nil {
		return cerr
	}
	if string(got) != want {
		return errf(CodeUnsupportedOp, "probe", r.name, rel,
			"RENAME_EXCHANGE did not swap the directories: marker %q instead of %q", got, want)
	}
	return nil
}
