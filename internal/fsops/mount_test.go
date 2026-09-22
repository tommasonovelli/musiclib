package fsops

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Tests of the boot-check primitives (DESIGN.md §3.1, §11.1 step 3):
// SameMount, CheckAccess and RemoveProbeLeftovers.

func TestSameMount(t *testing.T) {
	data, dir := newRoot(t)
	mkdirAbs(t, filepath.Join(dir, "library"))
	lib, err := data.SubRoot("library")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, lib)
	if same, err := SameMount(data, lib); err != nil || !same {
		t.Fatalf("SameMount(data, library) = %v, %v; want true", same, err)
	}

	other, err := OpenRoot(otherFilesystemDir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, other)
	if same, err := SameMount(data, other); err != nil || same {
		t.Fatalf("SameMount across filesystems = %v, %v; want false", same, err)
	}

	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = SameMount(data, lib)
	wantCode(t, err, CodeRootClosed)
}

// N-032: two mounts of the same filesystem share st_dev, so SameFilesystem
// cannot tell them apart, but rename(2) between them fails with EXDEV.
// SameMount and the probe must both catch it. Creating such a mount needs
// privileges, so the test looks for one that already exists: in the Docker
// gate the named volumes (the ext4 TMPDIR and the Go build cache) are
// separate mounts of the same disk.
func TestSameMountSeesBindMountsOfOneFilesystem(t *testing.T) {
	data, dir := newRoot(t)
	sibling := sameDeviceOtherMount(t, dir)
	other, err := OpenRoot(sibling)
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, other)
	t.Logf("same st_dev, other mount: %s", sibling)

	if same, err := SameFilesystem(data, other); err != nil || !same {
		t.Fatalf("SameFilesystem = %v, %v; the fixture should share st_dev", same, err)
	}
	if same, err := SameMount(data, other); err != nil || same {
		t.Fatalf("SameMount = %v, %v; want false for two mounts of one filesystem", same, err)
	}
	if unix.Access(sibling, unix.W_OK) != nil {
		t.Logf("%s is not writable: probe part skipped", sibling)
		return
	}
	wantCode(t, ProbeRenameExchange(data, other), CodeCrossDevice)
	for _, d := range []string{dir, sibling} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), probeDirPrefix) {
				t.Fatalf("probe leftover in %s: %s", d, e.Name())
			}
		}
	}
}

func TestCheckAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	data, dir := newRoot(t)
	if err := data.CheckAccess(true); err != nil {
		t.Fatalf("CheckAccess(write) on a 0755 directory: %v", err)
	}
	for _, tc := range []struct {
		mode        os.FileMode
		read, write string // expected code, "" = allowed
	}{
		{0o755, "", ""},
		{0o555, "", CodePermission},
		{0o311, CodePermission, CodePermission}, // no read: cannot list
		{0o644, CodePermission, CodePermission}, // no search permission
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			if err := os.Chmod(dir, tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Error(err)
				}
			})
			for _, c := range []struct {
				write bool
				want  string
			}{{false, tc.read}, {true, tc.write}} {
				err := data.CheckAccess(c.write)
				if c.want == "" {
					if err != nil {
						t.Fatalf("CheckAccess(write=%v): %v", c.write, err)
					}
					continue
				}
				wantCode(t, err, c.want)
			}
		})
	}

	// A read-only mount is reported as such. The kernel checks the
	// permission bits first and the mount afterwards (do_faccessat), so the
	// case needs a directory the test user owns on a read-only mount: the
	// home directory in the gate container, whose root is read-only.
	if ro := ownedReadOnlyDir(); ro == "" {
		t.Log("no directory owned by the test user on a read-only mount: read-only case skipped")
	} else {
		ro, err := OpenRoot(ro)
		if err != nil {
			t.Fatal(err)
		}
		defer mustClose(t, ro)
		wantCode(t, ro.CheckAccess(true), CodeReadOnly)
		if err := ro.CheckAccess(false); err != nil {
			t.Fatalf("CheckAccess(read) on a read-only mount: %v", err)
		}
	}

	if err := data.Close(); err != nil {
		t.Fatal(err)
	}
	wantCode(t, data.CheckAccess(false), CodeRootClosed)
}

func TestRemoveProbeLeftovers(t *testing.T) {
	work, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, probeDirPrefix+"a0011", "marker"), "b")
	mkdirAbs(t, filepath.Join(dir, probeDirPrefix+"b0011"))
	// Not probe directories: left alone.
	writeAbs(t, filepath.Join(dir, probeDirPrefix+"file"), "x")
	writeAbs(t, filepath.Join(dir, "blobs", "x.tmp"), "x")
	mkdirAbs(t, filepath.Join(dir, "render", probeDirPrefix+"nested"))
	symlinkAbs(t, "blobs", filepath.Join(dir, probeDirPrefix+"link"))

	// A real probe leaves nothing behind for the cleanup to find.
	if err := ProbeRenameExchange(work, work); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveProbeLeftovers(t.Context(), work)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{probeDirPrefix + "a0011", probeDirPrefix + "b0011"}; strings.Join(removed, ",") != strings.Join(want, ",") {
		t.Fatalf("removed %q, want %q", removed, want)
	}
	var left []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := []string{probeDirPrefix + "file", probeDirPrefix + "link", "blobs", "render"}
	if strings.Join(left, ",") != strings.Join(want, ",") {
		t.Fatalf("left %q, want %q", left, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "render", probeDirPrefix+"nested")); err != nil {
		t.Fatalf("a nested directory with the prefix was touched: %v", err)
	}

	mkdirAbs(t, filepath.Join(dir, probeDirPrefix+"c0011"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := RemoveProbeLeftovers(ctx, work); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cleanup: %v, want context.Canceled", err)
	}
}

// ownedReadOnlyDir returns a directory owned by the effective user, with
// owner write permission, on a read-only mount; "" if there is none.
func ownedReadOnlyDir() string {
	for _, p := range []string{os.Getenv("HOME")} {
		var st unix.Stat_t
		var fs unix.Statfs_t
		if p == "" || unix.Stat(p, &st) != nil || unix.Statfs(p, &fs) != nil {
			continue
		}
		if int(st.Uid) == os.Geteuid() && st.Mode&0o300 == 0o300 && fs.Flags&unix.ST_RDONLY != 0 {
			return p
		}
	}
	return ""
}

// sameDeviceOtherMount returns a directory that is on the same filesystem
// (st_dev) as ref but on a different mount, or skips the test. It reads
// /proc/self/mountinfo: "id parent major:minor root mountpoint ...".
func sameDeviceOtherMount(t *testing.T, ref string) string {
	t.Helper()
	var refSt unix.Stat_t
	if err := unix.Stat(ref, &refSt); err != nil {
		t.Fatal(err)
	}
	refMnt := mountIDOf(t, ref)
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		t.Skipf("no /proc/self/mountinfo: %v", err)
	}
	defer mustClose(t, f)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || strings.Contains(fields[4], `\`) {
			continue
		}
		majMin := strings.Split(fields[2], ":")
		if len(majMin) != 2 {
			continue
		}
		maj, err1 := strconv.ParseUint(majMin[0], 10, 32)
		mnr, err2 := strconv.ParseUint(majMin[1], 10, 32)
		if err1 != nil || err2 != nil || unix.Mkdev(uint32(maj), uint32(mnr)) != uint64(refSt.Dev) {
			continue
		}
		p := fields[4]
		var st unix.Stat_t
		if unix.Stat(p, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(st.Dev) != uint64(refSt.Dev) {
			continue
		}
		if unix.Access(p, unix.R_OK|unix.X_OK) != nil || mountIDOf(t, p) == refMnt {
			continue
		}
		return p
	}
	t.Skip("no other mount of the TMPDIR filesystem is visible (the Docker gate has one)")
	return ""
}

func mountIDOf(t *testing.T, p string) uint64 {
	t.Helper()
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, p, 0, unix.STATX_MNT_ID, &stx); err != nil {
		t.Fatalf("statx %s: %v", p, err)
	}
	if stx.Mask&unix.STATX_MNT_ID == 0 {
		t.Skip("statx does not report the mount id")
	}
	return stx.Mnt_id
}
