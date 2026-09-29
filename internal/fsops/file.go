package fsops

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// FileType is the type of a filesystem entry.
//
// The package distinguishes the types instead of settling for "file or
// directory" because DESIGN.md §5.2 and §9.3 require the explicit rejection
// of symlinks and special files, and the scan report must be able to say
// which type it found.
type FileType uint8

const (
	TypeUnknown FileType = iota
	TypeRegular
	TypeDir
	TypeSymlink
	TypeFIFO
	TypeSocket
	TypeBlockDevice
	TypeCharDevice
)

func (t FileType) String() string {
	switch t {
	case TypeRegular:
		return "regular file"
	case TypeDir:
		return "directory"
	case TypeSymlink:
		return "symlink"
	case TypeFIFO:
		return "FIFO"
	case TypeSocket:
		return "socket"
	case TypeBlockDevice:
		return "block device"
	case TypeCharDevice:
		return "character device"
	default:
		return "unknown type"
	}
}

// IsSpecial reports true for everything that is neither a regular file nor a
// directory: symlinks, FIFOs, sockets, devices and unknown types. DESIGN.md
// §9.3: symlinks and special files are always rejected.
func (t FileType) IsSpecial() bool {
	return t != TypeRegular && t != TypeDir
}

// FileInfo describes a filesystem entry. Name is the last segment of the
// path, never an absolute path (DESIGN.md §13.2).
//
// Dev and Ino are the file's identity: import uses them to detect that the
// source changed while it was being processed (§7.1).
type FileInfo struct {
	Name string
	Type FileType
	Size int64
	// Perm holds the permission bits plus os.ModeSetuid, os.ModeSetgid and
	// os.ModeSticky; the type is in Type.
	Perm  os.FileMode
	Dev   uint64
	Ino   uint64
	MTime time.Time
}

// DirEntry is a directory entry: name and type, without the information that
// would require a stat for each file. Callers that need size or mtime call
// [Root.Stat] on the entries they care about.
type DirEntry struct {
	Name string
	Type FileType
}

func fileTypeFromMode(mode uint32) FileType {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return TypeRegular
	case unix.S_IFDIR:
		return TypeDir
	case unix.S_IFLNK:
		return TypeSymlink
	case unix.S_IFIFO:
		return TypeFIFO
	case unix.S_IFSOCK:
		return TypeSocket
	case unix.S_IFBLK:
		return TypeBlockDevice
	case unix.S_IFCHR:
		return TypeCharDevice
	default:
		return TypeUnknown
	}
}

// fileModePerm is the inverse of syscallMode: the raw setuid, setgid and
// sticky bits are not valid os.FileMode bits and are translated.
func fileModePerm(mode uint32) os.FileMode {
	m := os.FileMode(mode & 0o777)
	if mode&unix.S_ISUID != 0 {
		m |= os.ModeSetuid
	}
	if mode&unix.S_ISGID != 0 {
		m |= os.ModeSetgid
	}
	if mode&unix.S_ISVTX != 0 {
		m |= os.ModeSticky
	}
	return m
}

func statToInfo(name string, st *unix.Stat_t) FileInfo {
	return FileInfo{
		Name:  name,
		Type:  fileTypeFromMode(st.Mode),
		Size:  st.Size,
		Perm:  fileModePerm(st.Mode),
		Dev:   uint64(st.Dev), // Dev is not uint64 on every architecture.
		Ino:   st.Ino,
		MTime: time.Unix(st.Mtim.Sec, st.Mtim.Nsec),
	}
}

// Stat describes the given entry **without following symlinks**: a symlink
// as the final component is reported as such, not resolved; a symlink as an
// intermediate component is rejected with [CodeSymlink]. The empty path
// describes the root.
//
// Stat reports the type, it does not reject it: the scan must be able to
// list a symlink or a FIFO in its report (§5.2) before opening rejects them.
func (r *Root) Stat(rel string) (_ FileInfo, err error) {
	var st unix.Stat_t
	if rel == "" {
		if err := r.withFD("fstat", "", func(fd int) error {
			return retryEINTR(func() error { return unix.Fstat(fd, &st) })
		}); err != nil {
			return FileInfo{}, r.opErr("fstat", "", err)
		}
		return statToInfo(r.name, &st), nil
	}
	dirfd, name, err := r.resolveParent(rel)
	if err != nil {
		return FileInfo{}, err
	}
	defer closeInto(&err, r.name, rel, dirfd)
	st, err = statAt(dirfd, name)
	if err != nil {
		return FileInfo{}, errnoErr("fstatat", r.name, rel, err)
	}
	return statToInfo(name, &st), nil
}

// opErr types a raw errno from withFD, passing through the errors the
// package has already typed (closed root).
func (r *Root) opErr(op, rel string, err error) error {
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	return errnoErr(op, r.name, rel, err)
}

// statAt describes an entry starting from an already resolved directory
// descriptor, without following symlinks. name is a single segment.
func statAt(dirfd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := retryEINTR(func() error {
		return unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	})
	return st, err
}

// Open opens a regular file read-only. Directories, symlinks and special
// files are rejected with [CodeIsDirectory], [CodeSymlink] or
// [CodeSpecialFile], without blocking.
func (r *Root) Open(rel string) (*os.File, error) {
	return r.OpenFile(rel, os.O_RDONLY, 0)
}

// CreateExclusive creates a new file and opens it for writing. If the name
// already exists — even as a symlink, a FIFO or a directory — it fails with
// [CodeExists] and touches nothing.
//
// It is the primitive behind the blob put and the staging build: DESIGN.md
// §3.2 and §7.5 do not allow a write to overwrite anything. The new entry is
// durable only after [SyncAndClose] of the file and [Root.SyncDir] of its
// parent.
func (r *Root) CreateExclusive(rel string, perm os.FileMode) (*os.File, error) {
	return r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}

// allowedOpenFlags are the flags OpenFile accepts besides the access mode.
// Anything else (O_PATH, O_TMPFILE, O_DIRECTORY, O_NOATIME, ...) would change
// what kind of descriptor is returned and is rejected.
const allowedOpenFlags = unix.O_CREAT | unix.O_EXCL | unix.O_TRUNC | unix.O_APPEND |
	unix.O_SYNC | unix.O_DSYNC | unix.O_CLOEXEC | unix.O_LARGEFILE

// OpenFile opens a regular file beneath the root with the given flags: an
// access mode (O_RDONLY, O_WRONLY, O_RDWR) plus O_CREATE, O_EXCL, O_TRUNC,
// O_APPEND, O_SYNC or O_DSYNC. Other flags yield [CodeInvalidArgument];
// O_DIRECTORY yields [CodeIsDirectory].
//
// Only regular files are ever returned, and the check never blocks:
//
//  1. openat2 always runs with O_NONBLOCK (and O_NOCTTY, O_CLOEXEC), so
//     opening a FIFO returns at once (or fails with ENXIO when opened for
//     writing with no reader), and opening a socket fails with ENXIO;
//  2. fstat on the returned descriptor checks the type of the inode that was
//     actually opened, so a concurrent replacement of the path cannot slip
//     a special file past the check (no TOCTOU: the check is on the
//     descriptor, not on the path);
//  3. only then is O_NONBLOCK cleared, before the file is handed to the
//     caller and before any read or write can happen.
//
// A device node that is opened reaches its driver's open routine (with
// O_NONBLOCK) before being rejected; see N-030 in NOTES.md.
//
// The file's name (f.Name()) is the root's label plus rel, never an absolute
// path.
func (r *Root) OpenFile(rel string, flags int, perm os.FileMode) (f *os.File, err error) {
	p, err := relPath(rel)
	if err != nil {
		return nil, err
	}
	if flags&unix.O_DIRECTORY != 0 {
		return nil, errf(CodeIsDirectory, "open", r.name, rel,
			"OpenFile does not open directories: use ReadDir or SyncDir")
	}
	if err := checkOpenFlags(flags); err != nil {
		return nil, errf(CodeInvalidArgument, "open", r.name, rel, "%v", err)
	}
	var mode uint32
	if flags&unix.O_CREAT != 0 {
		mode = syscallMode(perm)
	}
	fd, err := r.openat2(p, flags, mode)
	if err != nil {
		return nil, r.openErr(rel, err)
	}
	// From here until os.NewFile the descriptor is owned by this function.
	owned := true
	defer func() {
		if owned {
			closeInto(&err, r.name, rel, fd)
		}
	}()

	var st unix.Stat_t
	if err := retryEINTR(func() error { return unix.Fstat(fd, &st) }); err != nil {
		return nil, errnoErr("fstat", r.name, rel, err)
	}
	if t := fileTypeFromMode(st.Mode); t != TypeRegular {
		return nil, errf(codeForType(t), "open", r.name, rel,
			"the path is a %s, not a regular file", t)
	}
	fl, err := retryEINTR2(func() (int, error) { return unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0) })
	if err != nil {
		return nil, errnoErr("fcntl", r.name, rel, err)
	}
	if _, err := retryEINTR2(func() (int, error) {
		return unix.FcntlInt(uintptr(fd), unix.F_SETFL, fl&^unix.O_NONBLOCK)
	}); err != nil {
		return nil, errnoErr("fcntl", r.name, rel, err)
	}
	f = os.NewFile(uintptr(fd), location(r.name, rel))
	if f == nil {
		return nil, errf(CodeIO, "open", r.name, rel, "invalid descriptor %d", fd)
	}
	owned = false
	return f, nil
}

func checkOpenFlags(flags int) error {
	acc := flags & unix.O_ACCMODE
	switch {
	case acc != unix.O_RDONLY && acc != unix.O_WRONLY && acc != unix.O_RDWR:
		return errors.New("invalid access mode")
	case flags&^(unix.O_ACCMODE|allowedOpenFlags) != 0:
		return errors.New("unsupported open flags")
	case flags&unix.O_EXCL != 0 && flags&unix.O_CREAT == 0:
		return errors.New("O_EXCL without O_CREATE")
	case flags&unix.O_TRUNC != 0 && acc == unix.O_RDONLY:
		return errors.New("O_TRUNC on a read-only open")
	}
	return nil
}

// ReadDir lists a directory beneath the root, **explicitly sorted** by the
// bytes of the name. The empty path lists the root.
//
// DESIGN.md §7.3: directories and files are always sorted explicitly, never
// in the order returned by the filesystem. The natural number ordering
// required by import is a domain rule and lives elsewhere: here the order is
// total, stable and independent of the filesystem.
//
// The "." and ".." entries are not included. Symlinks and special files are
// listed with their type: rejecting them is up to whoever opens them.
func (r *Root) ReadDir(rel string) (_ []DirEntry, err error) {
	dirfd, err := r.openDir(rel)
	if err != nil {
		return nil, err
	}
	defer closeInto(&err, r.name, rel, dirfd)
	entries, err := readDirFD(dirfd)
	if err != nil {
		return nil, errnoErr("getdents", r.name, rel, err)
	}
	slices.SortFunc(entries, func(a, b DirEntry) int { return strings.Compare(a.Name, b.Name) })
	return entries, nil
}

// readDirFD reads all entries of a directory descriptor owned by the caller,
// starting from the beginning. The type comes from the d_type field of
// getdents64; if the filesystem does not provide it, it falls back to an
// fstatat confined to the descriptor.
func readDirFD(dirfd int) ([]DirEntry, error) {
	if _, err := retryEINTR2(func() (int64, error) {
		return unix.Seek(dirfd, 0, unix.SEEK_SET)
	}); err != nil {
		return nil, err
	}
	buf := make([]byte, 16<<10)
	var out []DirEntry
	for {
		n, err := retryEINTR2(func() (int, error) { return unix.Getdents(dirfd, buf) })
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return out, nil
		}
		for off := 0; off < n; {
			rec := buf[off:n]
			if len(rec) < direntNameOffset {
				return nil, unix.EIO
			}
			reclen := int(binary.NativeEndian.Uint16(rec[direntReclenOffset:]))
			if reclen < direntNameOffset || reclen > len(rec) {
				return nil, unix.EIO
			}
			dtype := rec[direntTypeOffset]
			raw := rec[direntNameOffset:reclen]
			if i := bytes.IndexByte(raw, 0); i >= 0 {
				raw = raw[:i]
			}
			off += reclen
			name := string(raw)
			if name == "." || name == ".." || name == "" {
				continue
			}
			t := fileTypeFromDirent(dtype)
			if t == TypeUnknown {
				st, err := statAt(dirfd, name)
				if err != nil {
					// The entry may have vanished between getdents and
					// fstatat: that is not a read error.
					if errors.Is(err, unix.ENOENT) {
						continue
					}
					return nil, err
				}
				t = fileTypeFromMode(st.Mode)
			}
			out = append(out, DirEntry{Name: name, Type: t})
		}
	}
}

// Offsets within struct linux_dirent64: d_ino (8) + d_off (8) +
// d_reclen (2) + d_type (1) + d_name.
const (
	direntReclenOffset = 16
	direntTypeOffset   = 18
	direntNameOffset   = 19
)

func fileTypeFromDirent(d byte) FileType {
	switch d {
	case unix.DT_REG:
		return TypeRegular
	case unix.DT_DIR:
		return TypeDir
	case unix.DT_LNK:
		return TypeSymlink
	case unix.DT_FIFO:
		return TypeFIFO
	case unix.DT_SOCK:
		return TypeSocket
	case unix.DT_BLK:
		return TypeBlockDevice
	case unix.DT_CHR:
		return TypeCharDevice
	default:
		return TypeUnknown
	}
}

// codeForType is the error code matching the type found where a regular
// file or a directory was expected.
func codeForType(t FileType) string {
	switch t {
	case TypeSymlink:
		return CodeSymlink
	case TypeDir:
		return CodeIsDirectory
	case TypeRegular:
		return CodeNotDirectory
	default:
		return CodeSpecialFile
	}
}

// Describe returns the FileInfo of an open file, read from its descriptor
// with fstat: the identity of what was actually opened. A caller compares
// it with an earlier [Root.Stat] of the same path to detect that the entry
// was replaced in between (§7.1). Name is the last element of f.Name().
func Describe(f *os.File) (FileInfo, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return FileInfo{}, &Error{Code: CodeIO, Op: "fstat", Path: f.Name(), Err: err}
	}
	var st unix.Stat_t
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = retryEINTR(func() error { return unix.Fstat(int(fd), &st) })
	}); err != nil {
		return FileInfo{}, &Error{Code: CodeIO, Op: "fstat", Path: f.Name(), Err: err}
	}
	if serr != nil {
		return FileInfo{}, errnoErr("fstat", "", f.Name(), serr)
	}
	name := f.Name()
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return statToInfo(name, &st), nil
}
