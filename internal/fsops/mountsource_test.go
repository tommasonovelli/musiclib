package fsops

import (
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// Tests of SourceOf and MountSource.Contains (NOTES.md N-228): the location
// of a directory inside its filesystem, independent of its mount point.

const sampleMountinfo = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
30 22 8:17 /srv/musiclib/data /data rw,relatime - ext4 /dev/sdb1 rw
31 22 8:17 /srv/musiclib/data/nested\040dir /backup rw,relatime - ext4 /dev/sdb1 rw
32 22 8:17 /srv/musiclib/data2 /other rw,relatime - ext4 /dev/sdb1 rw
33 22 8:33 / /mnt/disk rw,relatime - ext4 /dev/sdc1 rw
`

func TestMountSourceFromMountinfo(t *testing.T) {
	for _, tc := range []struct {
		id   uint64
		abs  string
		want MountSource
	}{
		{22, "/", MountSource{"8:1", "/"}},
		{22, "/var/tmp", MountSource{"8:1", "/var/tmp"}},
		{30, "/data", MountSource{"8:17", "/srv/musiclib/data"}},
		{30, "/data/library", MountSource{"8:17", "/srv/musiclib/data/library"}},
		{31, "/backup", MountSource{"8:17", "/srv/musiclib/data/nested dir"}},
		{31, "/backup/daily", MountSource{"8:17", "/srv/musiclib/data/nested dir/daily"}},
		{33, "/mnt/disk/x", MountSource{"8:33", "/x"}},
	} {
		got, err := mountSourceFrom([]byte(sampleMountinfo), tc.id, tc.abs)
		if err != nil || got != tc.want {
			t.Errorf("mountSourceFrom(%d, %q) = %+v, %v; want %+v", tc.id, tc.abs, got, err, tc.want)
		}
	}
	// A path outside the mount point, including a prefix that is not at a
	// component boundary, is an error, not a guess.
	for _, abs := range []string{"/database", "/other", "relative", "/data/../etc"} {
		if _, err := mountSourceFrom([]byte(sampleMountinfo), 30, abs); err == nil {
			t.Errorf("mountSourceFrom(30, %q) accepted", abs)
		}
	}
	_, err := mountSourceFrom([]byte(sampleMountinfo), 99, "/data")
	wantCode(t, err, CodeUnsupportedOp)
}

func TestMountSourceContains(t *testing.T) {
	data := MountSource{"8:17", "/srv/musiclib/data"}
	for _, tc := range []struct {
		other MountSource
		want  bool
	}{
		{MountSource{"8:17", "/srv/musiclib/data"}, true},
		{MountSource{"8:17", "/srv/musiclib/data/backup"}, true},
		{MountSource{"8:17", "/srv/musiclib/data2"}, false},
		{MountSource{"8:17", "/srv/musiclib"}, false},
		{MountSource{"8:18", "/srv/musiclib/data/backup"}, false},
	} {
		if got := data.Contains(tc.other); got != tc.want {
			t.Errorf("Contains(%+v) = %v; want %v", tc.other, got, tc.want)
		}
	}
	whole := MountSource{"8:33", "/"}
	if !whole.Contains(MountSource{"8:33", "/anything"}) || whole.Contains(MountSource{"8:17", "/"}) {
		t.Error("a whole-filesystem source must contain exactly its own device")
	}
}

func TestSourceOfRealMounts(t *testing.T) {
	data, dir := newRoot(t)
	mkdirAbs(t, filepath.Join(dir, "library"))
	lib, err := data.SubRoot("library")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, lib)
	top, err := SourceOf(data)
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}
	if dev := strconv.FormatUint(uint64(unix.Major(uint64(st.Dev))), 10) + ":" + strconv.FormatUint(uint64(unix.Minor(uint64(st.Dev))), 10); top.Dev != dev {
		t.Fatalf("Dev = %q; stat says %q", top.Dev, dev)
	}
	sub, err := SourceOf(lib)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Dev != top.Dev || sub.Path != filepath.Join(top.Path, "library") || !top.Contains(sub) || sub.Contains(top) {
		t.Fatalf("library %+v is not under %+v", sub, top)
	}

	// Another mount of the same disk (the Docker gate's named volumes)
	// shares Dev but is a sibling, not a parent.
	other, err := OpenRoot(sameDeviceOtherMount(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, other)
	src, err := SourceOf(other)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("data %+v, other mount %+v", top, src)
	if src.Dev != top.Dev || top.Contains(src) {
		t.Fatalf("other mount %+v wrongly related to %+v", src, top)
	}

	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = SourceOf(lib)
	wantCode(t, err, CodeRootClosed)
}
