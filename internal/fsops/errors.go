package fsops

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"musiclib/internal/names"
)

// Error codes of the filesystem primitives. They are stable: the API exposes
// them inside the {code, message, details} body (DESIGN.md §10.1) and the
// upper layers use them to make decisions, not to print them.
//
// Path validation happens before any syscall and keeps the codes of
// internal/names (for example "path_dot_segment"); [Code] returns both kinds.
const (
	// CodeUnsupportedKernel means that openat2(2) is not usable: a kernel
	// older than Linux 5.6, or a seccomp filter that blocks it.
	// DESIGN.md §3.1: the application must refuse to start, not degrade to
	// primitives with weaker guarantees.
	CodeUnsupportedKernel = "fs_openat2_unsupported"
	// CodeRootClosed means a Root was used after being closed: it is a
	// programming error, not a disk condition.
	CodeRootClosed = "fs_root_closed"
	// CodeInvalidArgument is a call the kernel or the package refuses as
	// malformed: open flags outside the allowed set, or a rename that would
	// move a directory into its own subtree (renameat2 EINVAL).
	CodeInvalidArgument = "fs_invalid_argument"

	// CodePathEscape is a resolution that would escape the root
	// (RESOLVE_BENEATH, errno EXDEV).
	CodePathEscape = "fs_path_escape"
	// CodeSymlink is a symlink met during resolution or as the final
	// component (RESOLVE_NO_SYMLINKS, errno ELOOP).
	CodeSymlink = "fs_symlink"
	// CodeSpecialFile is a FIFO, a socket or a device: DESIGN.md §5.2 and
	// §9.3 always reject them.
	CodeSpecialFile = "fs_special_file"

	CodeNotDirectory = "fs_not_directory"
	CodeIsDirectory  = "fs_is_directory"
	CodeNotFound     = "fs_not_found"
	CodeExists       = "fs_exists"
	CodeDirNotEmpty  = "fs_dir_not_empty"
	// CodeCrossDevice means that two paths are not on the same mounted
	// filesystem, so a rename between them is impossible (§3.1).
	CodeCrossDevice = "fs_cross_device"
	CodeNoSpace     = "fs_no_space"
	CodePermission  = "fs_permission"
	CodeReadOnly    = "fs_read_only"
	CodeLockBusy    = "fs_lock_busy"
	CodeTooDeep     = "fs_too_deep"
	// CodeUnsupportedOp is an operation the filesystem does not offer, for
	// example RENAME_EXCHANGE on a volume that does not implement it.
	CodeUnsupportedOp = "fs_unsupported_operation"
	CodeIO            = "fs_io"
)

// Error is the package's typed error: a stable code, the operation that
// failed, the root's label and the **relative** path.
//
// DESIGN.md §13.2: the store knows nothing about absolute paths. Errors do
// not contain them either: Root is the root's label (for example "library"),
// Path is relative to that root.
//
// Two-path operations (renames) also fill DstRoot and DstPath when the
// failure cannot be attributed to one side: renameat2 reports a single errno
// for the pair.
type Error struct {
	Code    string
	Op      string
	Root    string
	Path    string
	DstRoot string
	DstPath string
	Err     error
}

func (e *Error) Error() string {
	loc := location(e.Root, e.Path)
	if dst := location(e.DstRoot, e.DstPath); dst != "" {
		loc += " -> " + dst
	}
	msg := e.Code
	if e.Op != "" {
		msg += " (" + e.Op + ")"
	}
	if loc != "" {
		msg += ": " + loc
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func location(root, path string) string {
	switch {
	case root == "":
		return path
	case path == "":
		return root
	default:
		return root + "/" + path
	}
}

// Code returns the stable code of err: that of *fsops.Error if present,
// otherwise that of *names.Error (path validation happens before touching the
// disk and keeps its own codes), otherwise "".
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return names.Code(err)
}

// errf builds a package error with no underlying errno.
func errf(code, op, root, path, format string, args ...any) *Error {
	return &Error{Code: code, Op: op, Root: root, Path: path, Err: fmt.Errorf(format, args...)}
}

// errnoCode maps an errno to the corresponding stable code.
//
// EXDEV maps to CodePathEscape: after path validation, the only way to get it
// from openat2 is a resolution that would try to escape the root. Rename
// operations, whose paths have already been resolved, use renameErr: there
// EXDEV means different mounted filesystems.
func errnoCode(err error) string {
	switch {
	case errors.Is(err, unix.ELOOP):
		return CodeSymlink
	case errors.Is(err, unix.EXDEV):
		return CodePathEscape
	case errors.Is(err, unix.ENOENT):
		return CodeNotFound
	case errors.Is(err, unix.EEXIST):
		return CodeExists
	case errors.Is(err, unix.ENOTDIR):
		return CodeNotDirectory
	case errors.Is(err, unix.EISDIR):
		return CodeIsDirectory
	case errors.Is(err, unix.ENOTEMPTY):
		return CodeDirNotEmpty
	case errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
		return CodeNoSpace
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return CodePermission
	case errors.Is(err, unix.EROFS):
		return CodeReadOnly
	case errors.Is(err, unix.ENXIO):
		// open(2) of a socket, or O_WRONLY|O_NONBLOCK on a FIFO with no
		// reader: in both cases the entry is a special file.
		return CodeSpecialFile
	case errors.Is(err, unix.EINVAL):
		return CodeInvalidArgument
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EOPNOTSUPP):
		return CodeUnsupportedOp
	default:
		return CodeIO
	}
}

// errnoErr wraps an errno in an *Error with code, operation and relative
// path. DESIGN.md §13.2: typed errors and context along the chain.
func errnoErr(op, root, path string, err error) *Error {
	return &Error{Code: errnoCode(err), Op: op, Root: root, Path: path, Err: err}
}
