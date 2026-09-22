package volume

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"musiclib/internal/fsops"
)

// layoutRoots opens /data and its three media directories under a fresh
// temporary directory, closed when the test ends.
func layoutRoots(t *testing.T) (dir string, data, originals, library, work *fsops.Root) {
	t.Helper()
	dir = t.TempDir()
	for _, d := range mediaDirs {
		if err := os.Mkdir(dir+"/"+d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	data = openRoot(t, dir)
	originals, library, work = subRoot(t, data, Originals), subRoot(t, data, Library), subRoot(t, data, Work)
	return dir, data, originals, library, work
}

func openRoot(t *testing.T, p string) *fsops.Root {
	t.Helper()
	r, err := fsops.OpenRoot(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func subRoot(t *testing.T, r *fsops.Root, rel string) *fsops.Root {
	t.Helper()
	s, err := r.SubRoot(rel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestCheckFilesystemPasses(t *testing.T) {
	dir, data, originals, library, work := layoutRoots(t)
	if err := checkFilesystem(data, originals, library, work); err != nil {
		t.Fatal(err)
	}
	// The probe ran in work/ only and cleaned up after itself (N-033).
	for _, d := range mediaDirs {
		entries, err := os.ReadDir(dir + "/" + d)
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s after the checks: %v, %v", d, entries, err)
		}
	}
}

// A media directory on another filesystem. The layout cannot contain a
// mount without privileges, so the other filesystem (/dev/shm) is passed in
// its place: checkFilesystem sees only roots.
func TestCheckFilesystemCrossDevice(t *testing.T) {
	_, data, originals, library, work := layoutRoots(t)
	other := otherFilesystemRoot(t, data)
	for name, roots := range map[string][3]*fsops.Root{
		"originals": {other, library, work},
		"library":   {originals, other, work},
		"work":      {originals, library, other},
	} {
		t.Run(name, func(t *testing.T) {
			wantCode(t, checkFilesystem(data, roots[0], roots[1], roots[2]), CodeCrossDevice)
		})
	}
}

// N-032: a separate mount of the same filesystem has the st_dev of /data
// but renames to it fail. It is found, not created: in the Docker gate the
// Go build cache is another named volume on the TMPDIR's disk.
func TestCheckFilesystemNestedMount(t *testing.T) {
	_, data, originals, library, work := layoutRoots(t)
	sibling := openRoot(t, sameDeviceOtherMount(t, data))
	for name, roots := range map[string][3]*fsops.Root{
		"originals": {sibling, library, work},
		"library":   {originals, sibling, work},
		"work":      {originals, library, sibling},
	} {
		t.Run(name, func(t *testing.T) {
			wantCode(t, checkFilesystem(data, roots[0], roots[1], roots[2]), CodeNestedMount)
		})
	}
}

func TestCheckFilesystemPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	for _, d := range []string{"", Originals, Library, Work} {
		for _, mode := range []os.FileMode{0o555, 0o355, 0o655} {
			t.Run(d+"/"+mode.String(), func(t *testing.T) {
				dir, data, originals, library, work := layoutRoots(t)
				p := dir + "/" + d
				if err := os.Chmod(p, mode); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(p, 0o755); err != nil {
						t.Error(err)
					}
				})
				wantCode(t, checkFilesystem(data, originals, library, work), CodePermission)
			})
		}
	}
}

// A filesystem without RENAME_EXCHANGE cannot be produced without
// privileges: the probe's result is injected (N-063).
func TestCheckFilesystemProbeFailures(t *testing.T) {
	for _, tc := range []struct {
		probeCode, want string
	}{
		{fsops.CodeUnsupportedOp, CodeNoRenameExchange},
		{fsops.CodeCrossDevice, CodeNoRenameExchange},
		{fsops.CodePermission, CodePermission},
		{fsops.CodeNoSpace, CodeIO},
	} {
		t.Run(tc.probeCode, func(t *testing.T) {
			_, data, originals, library, work := layoutRoots(t)
			var probed [2]*fsops.Root
			saved := probeRenameExchange
			probeRenameExchange = func(a, b *fsops.Root) error {
				probed = [2]*fsops.Root{a, b}
				return &fsops.Error{Code: tc.probeCode, Op: "probe", Root: a.Name()}
			}
			t.Cleanup(func() { probeRenameExchange = saved })
			err := checkFilesystem(data, originals, library, work)
			wantCode(t, err, tc.want)
			if fsops.Code(unwrapVolume(err)) != tc.probeCode {
				t.Fatalf("the fsops cause is lost: %v", err)
			}
			if probed != [2]*fsops.Root{work, work} {
				t.Fatalf("the probe ran on %s and %s, want work twice (N-033)", probed[0].Name(), probed[1].Name())
			}
		})
	}
}

func unwrapVolume(err error) error {
	if e, ok := err.(*Error); ok {
		return e.Err
	}
	return nil
}

// otherFilesystemRoot opens a directory on a filesystem other than ref's,
// or skips.
func otherFilesystemRoot(t *testing.T, ref *fsops.Root) *fsops.Root {
	t.Helper()
	refInfo, err := ref.Stat("")
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"/dev/shm", "/run/user/" + strconv.Itoa(os.Getuid())} {
		var st unix.Stat_t
		if unix.Stat(base, &st) != nil || uint64(st.Dev) == refInfo.Dev {
			continue
		}
		d, err := os.MkdirTemp(base, "volume-test-")
		if err != nil {
			continue
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(d); err != nil {
				t.Error(err)
			}
		})
		return openRoot(t, d)
	}
	t.Skip("no second writable filesystem available (/dev/shm, /run/user)")
	return nil
}

// sameDeviceOtherMount returns a directory on the filesystem of ref but on
// another mount, from /proc/self/mountinfo ("id parent major:minor root
// mountpoint ..."), or skips.
func sameDeviceOtherMount(t *testing.T, ref *fsops.Root) string {
	t.Helper()
	refInfo, err := ref.Stat("")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		t.Skipf("no /proc/self/mountinfo: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || strings.Contains(fields[4], `\`) {
			continue
		}
		var maj, mnr uint32
		if _, err := parseMajMin(fields[2], &maj, &mnr); err != nil || unix.Mkdev(maj, mnr) != refInfo.Dev {
			continue
		}
		p := fields[4]
		var st unix.Stat_t
		if unix.Stat(p, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(st.Dev) != refInfo.Dev ||
			unix.Access(p, unix.R_OK|unix.X_OK) != nil {
			continue
		}
		r, err := fsops.OpenRoot(p)
		if err != nil {
			continue
		}
		same, err := fsops.SameMount(ref, r)
		if cerr := r.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		if err == nil && !same {
			return p
		}
	}
	t.Skip("no other mount of the TMPDIR filesystem is visible (the Docker gate has one)")
	return ""
}

func parseMajMin(s string, maj, mnr *uint32) (int, error) {
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return 0, strconv.ErrSyntax
	}
	x, err := strconv.ParseUint(a, 10, 32)
	if err != nil {
		return 0, err
	}
	y, err := strconv.ParseUint(b, 10, 32)
	if err != nil {
		return 0, err
	}
	*maj, *mnr = uint32(x), uint32(y)
	return 2, nil
}
