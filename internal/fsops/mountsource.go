package fsops

import (
	"os"
	"path"
	"strconv"
	"strings"
)

// MountSource locates a root's directory inside its filesystem,
// independently of where that filesystem is mounted in this process: Dev is
// the "major:minor" of /proc/self/mountinfo and Path the directory's
// absolute path from the filesystem's own root.
//
// Two bind mounts of one host filesystem (for example a Compose /data and
// /backup whose host paths are nested) have different mount points but the
// same Dev, and one Path is a prefix of the other: [MountSource.Contains]
// sees what the container's own paths and inodes cannot (NOTES.md N-228).
type MountSource struct {
	Dev  string
	Path string
}

// Contains reports whether other is s itself or lies under it on the same
// filesystem.
func (s MountSource) Contains(other MountSource) bool {
	if s.Dev != other.Dev {
		return false
	}
	return s.Path == "/" || other.Path == s.Path || strings.HasPrefix(other.Path, s.Path+"/")
}

// SourceOf returns where the root's directory lies inside its filesystem.
// It identifies the mount by statx(2) mount id (not by path), reads the
// directory's path in this process from its descriptor, and maps it through
// /proc/self/mountinfo.
func SourceOf(r *Root) (MountSource, error) {
	id, err := r.mountID()
	if err != nil {
		return MountSource{}, err
	}
	var abs string
	var info []byte
	if err := r.withFD("mountinfo", "", func(fd int) error {
		var procErr error
		abs, info, procErr = procView(fd)
		return procErr
	}); err != nil {
		return MountSource{}, r.opErr("mountinfo", "", err)
	}
	return mountSourceFrom(info, id, abs)
}

// procView reads the kernel's fixed /proc/self views: the path of an open
// descriptor and the mount table. These are kernel interfaces at constant
// locations, not paths built from user data (the confinement test allows
// os.ReadFile here only).
func procView(fd int) (abs string, mountinfo []byte, err error) {
	abs, err = os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return "", nil, err
	}
	mountinfo, err = os.ReadFile("/proc/self/mountinfo")
	return abs, mountinfo, err
}

// mountSourceFrom maps abs, a directory path in this process on the mount
// with the given id, to its filesystem location. It does no I/O.
func mountSourceFrom(mountinfo []byte, id uint64, abs string) (MountSource, error) {
	if !path.IsAbs(abs) || path.Clean(abs) != abs {
		return MountSource{}, errf(CodeInvalidArgument, "mountinfo", "", "", "unusable directory path %q", abs)
	}
	want := strconv.FormatUint(id, 10)
	for _, line := range strings.Split(string(mountinfo), "\n") {
		// id parent major:minor root mountpoint options ... - fstype source ...
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != want {
			continue
		}
		root, point := unescapeMountinfo(f[3]), unescapeMountinfo(f[4])
		var rel string
		switch {
		case point == "/":
			rel = abs
		case abs == point:
			rel = "/"
		case strings.HasPrefix(abs, point+"/"):
			rel = strings.TrimPrefix(abs, point)
		default:
			return MountSource{}, errf(CodeInvalidArgument, "mountinfo", "", "", "%q is not under mount point %q", abs, point)
		}
		// root and rel are both absolute and clean; concatenating them keeps
		// that, except that one of them may be "/".
		switch {
		case root == "/":
		case rel == "/":
			rel = root
		default:
			rel = root + rel
		}
		return MountSource{Dev: f[2], Path: rel}, nil
	}
	return MountSource{}, errf(CodeUnsupportedOp, "mountinfo", "", "", "mount id %s not listed", want)
}

// unescapeMountinfo decodes the octal escapes the kernel writes for space,
// tab, newline and backslash (\040, \011, \012, \134).
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
