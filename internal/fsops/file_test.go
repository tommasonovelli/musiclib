package fsops

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCreateExclusiveAndOpen(t *testing.T) {
	r, dir := newRoot(t)
	mkdirAbs(t, filepath.Join(dir, "blobs"))

	f, err := r.CreateExclusive("blobs/x.tmp", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCloexec(t, int(f.Fd())) {
		t.Fatal("CreateExclusive: descriptor without FD_CLOEXEC")
	}
	if _, err := io.WriteString(f, "content"); err != nil {
		t.Fatal(err)
	}
	if err := SyncAndClose(f); err != nil {
		t.Fatal(err)
	}
	if got := readRoot(t, r, "blobs/x.tmp"); got != "content" {
		t.Fatalf("content %q", got)
	}

	// Second creation: the existing file stays intact.
	_, err = r.CreateExclusive("blobs/x.tmp", 0o644)
	wantCode(t, err, CodeExists)
	if got := readAbs(t, filepath.Join(dir, "blobs", "x.tmp")); got != "content" {
		t.Fatalf("the existing file was altered: %q", got)
	}

	g, err := r.Open("blobs/x.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, g)
	if !hasCloexec(t, int(g.Fd())) {
		t.Fatal("Open: descriptor without FD_CLOEXEC")
	}
	fl, err := unix.FcntlInt(g.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fl&unix.O_NONBLOCK != 0 {
		t.Fatal("O_NONBLOCK was left set on a regular file")
	}
	if _, err := g.Write([]byte("x")); err == nil {
		t.Fatal("Open returned a writable file")
	}
	// The file name does not expose absolute paths.
	if filepath.IsAbs(g.Name()) || strings.Contains(g.Name(), dir) {
		t.Fatalf("absolute file name: %q", g.Name())
	}
}

func TestOpenFileFlags(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "f"), "old content")
	mkdirAbs(t, filepath.Join(dir, "d"))

	f, err := r.OpenFile("f", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "new"); err != nil {
		t.Fatal(err)
	}
	mustClose(t, f)
	if got := readAbs(t, filepath.Join(dir, "f")); got != "new" {
		t.Fatalf("O_TRUNC: %q", got)
	}

	for name, flags := range map[string]int{
		"O_PATH":                 unix.O_PATH,
		"O_TMPFILE":              unix.O_TMPFILE | os.O_RDWR,
		"O_NOATIME":              unix.O_NOATIME,
		"O_ASYNC":                unix.O_ASYNC,
		"invalid access mode":    unix.O_ACCMODE,
		"O_EXCL without O_CREAT": os.O_WRONLY | os.O_EXCL,
		"O_TRUNC read-only":      os.O_RDONLY | os.O_TRUNC,
	} {
		code := CodeInvalidArgument
		if flags&unix.O_DIRECTORY != 0 {
			code = CodeIsDirectory
		}
		t.Run(name, func(t *testing.T) {
			wantCode(t, openClose(r, "f", flags), code)
		})
	}
	wantCode(t, openClose(r, "d", os.O_RDONLY|unix.O_DIRECTORY), CodeIsDirectory)
	if got := readAbs(t, filepath.Join(dir, "f")); got != "new" {
		t.Fatalf("a rejected open touched the file: %q", got)
	}
}

func TestOpenErrors(t *testing.T) {
	r, dir := newRoot(t)
	mkdirAbs(t, filepath.Join(dir, "d"))
	writeAbs(t, filepath.Join(dir, "f"), "x")

	wantCode(t, openClose(r, "d", os.O_RDONLY), CodeIsDirectory)
	wantCode(t, openClose(r, "d", os.O_WRONLY), CodeIsDirectory)
	wantCode(t, openClose(r, "missing", os.O_RDONLY), CodeNotFound)
	wantCode(t, openClose(r, "f/below", os.O_RDONLY), CodeNotDirectory)
	wantCode(t, openClose(r, "missing/new", os.O_WRONLY|os.O_CREATE|os.O_EXCL), CodeNotFound)
	wantCode(t, openClose(r, "d", os.O_WRONLY|os.O_CREATE|os.O_EXCL), CodeExists)
}

// Special files are rejected with CodeSpecialFile without ever blocking
// (N-013), whatever the open flags. Every open runs under callWithin, so a
// regression fails here instead of hanging the suite; afterwards Close of the
// root must not be held up either.
func TestSpecialFilesRejectedWithoutBlocking(t *testing.T) {
	kinds := []struct {
		name string
		typ  FileType
		make func(t *testing.T, p string)
	}{
		{"fifo", TypeFIFO, mkfifoAbs},
		{"socket", TypeSocket, mksockAbs},
		{"device", TypeCharDevice, mkdevAbs},
	}
	opens := map[string]int{
		"read":        os.O_RDONLY,
		"write":       os.O_WRONLY,
		"read-write":  os.O_RDWR,
		"create":      os.O_WRONLY | os.O_CREATE,
		"append":      os.O_WRONLY | os.O_APPEND,
		"truncate":    os.O_WRONLY | os.O_TRUNC,
		"synchronous": os.O_RDWR | os.O_SYNC,
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "special")
			k.make(t, p)
			mkdirAbs(t, filepath.Join(dir, "dst"))
			writeAbs(t, filepath.Join(dir, "file"), "x")
			r, err := OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { mustClose(t, r) }) // no-op after the explicit Close below
			unblock := func() {}
			if k.typ == TypeFIFO {
				unblock = unblockFIFO(t, p)
			}

			for opName, flags := range opens {
				err := callWithin(t, 5*time.Second, func() error {
					return openClose(r, "special", flags)
				}, unblock)
				if Code(err) != CodeSpecialFile {
					t.Errorf("OpenFile(%s): code %q, want %q (%v)", opName, Code(err), CodeSpecialFile, err)
				}
			}
			err = callWithin(t, 5*time.Second, func() error {
				l, err := r.Lock("special")
				return errors.Join(err, closeIfLock(l))
			}, unblock)
			wantCode(t, err, CodeSpecialFile)
			wantCode(t, openClose(r, "special", os.O_WRONLY|os.O_CREATE|os.O_EXCL), CodeExists)

			fi, err := r.Stat("special")
			if err != nil {
				t.Fatal(err)
			}
			if fi.Type != k.typ || !fi.Type.IsSpecial() {
				t.Fatalf("Stat: type %v, want %v", fi.Type, k.typ)
			}
			entries, err := r.ReadDir("")
			if err != nil {
				t.Fatal(err)
			}
			if i := slices.IndexFunc(entries, func(e DirEntry) bool { return e.Name == "special" }); i < 0 || entries[i].Type != k.typ {
				t.Fatalf("ReadDir: %v", entries)
			}
			_, err = r.ReadDir("special")
			wantCode(t, err, CodeNotDirectory)
			wantCode(t, r.SyncDir("special"), CodeNotDirectory)
			_, err = r.SubRoot("special")
			wantCode(t, err, CodeNotDirectory)
			_, err = r.MkdirAll("special/x", 0o755)
			wantCode(t, err, CodeSpecialFile)
			wantCode(t, RenameNoReplace(r, "special", r, "dst/special"), CodeSpecialFile)
			wantCode(t, RenameExchange(r, "file", r, "special"), CodeSpecialFile)

			// The root closes promptly: nothing is left holding it.
			if err := callWithin(t, 5*time.Second, r.Close, unblock); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "dst", "special")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the special file was renamed")
			}
		})
	}
}

func TestStat(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "a", "f"), "12345")
	symlinkAbs(t, "f", filepath.Join(dir, "a", "link"))
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 6000, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, "a", "f"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	fi, err := r.Stat("a/f")
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(dir, "a", "f"), &st); err != nil {
		t.Fatal(err)
	}
	if fi.Name != "f" || fi.Type != TypeRegular || fi.Size != 5 ||
		fi.Ino != st.Ino || fi.Dev != uint64(st.Dev) || !fi.MTime.Equal(mtime) || fi.Perm != 0o644 {
		t.Fatalf("Stat = %+v", fi)
	}

	li, err := r.Stat("a/link")
	if err != nil {
		t.Fatal(err)
	}
	if li.Type != TypeSymlink {
		t.Fatalf("Stat followed the symlink: type %v", li.Type)
	}

	root, err := r.Stat("")
	if err != nil {
		t.Fatal(err)
	}
	if root.Type != TypeDir || root.Name != r.Name() {
		t.Fatalf("Stat of the root = %+v", root)
	}
	_, err = r.Stat("missing")
	wantCode(t, err, CodeNotFound)
}

// The raw setuid/setgid/sticky bits are not os.FileMode bits: Stat must
// translate them.
func TestStatSpecialPermissionBits(t *testing.T) {
	r, dir := newRoot(t)
	mkdirAbs(t, filepath.Join(dir, "sticky"))
	if err := os.Chmod(filepath.Join(dir, "sticky"), 0o755|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	fi, err := r.Stat("sticky")
	if err != nil {
		t.Fatal(err)
	}
	if want := 0o755 | os.ModeSticky; fi.Perm != want {
		t.Fatalf("Perm = %v, want %v", fi.Perm, want)
	}
	if err := r.Mkdir("created", 0o750|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	fi, err = r.Stat("created")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Perm&os.ModeSticky == 0 {
		t.Fatalf("Mkdir lost the sticky bit: %v", fi.Perm)
	}
}

func TestReadDirSortedAndDeterministic(t *testing.T) {
	r, dir := newRoot(t)
	// NFC and NFD spellings of "é" are distinct names on disk and must both
	// appear, in byte order.
	names := []string{"b", "a", "Z", "\u00e9", "e\u0301", "10", "9", "_", ".hidden", "a b", "\u00e4"}
	for _, i := range rand.Perm(len(names)) {
		writeAbs(t, filepath.Join(dir, "d", names[i]), names[i])
	}
	mkdirAbs(t, filepath.Join(dir, "d", "subdir"))
	symlinkAbs(t, "a", filepath.Join(dir, "d", "link"))

	want := append(slices.Clone(names), "subdir", "link")
	slices.Sort(want) // order by UTF-8 bytes

	got, err := r.ReadDir("d")
	if err != nil {
		t.Fatal(err)
	}
	var gotNames []string
	for _, e := range got {
		gotNames = append(gotNames, e.Name)
		wantType := TypeRegular
		switch e.Name {
		case "subdir":
			wantType = TypeDir
		case "link":
			wantType = TypeSymlink
		}
		if e.Type != wantType {
			t.Fatalf("%s: type %v, want %v", e.Name, e.Type, wantType)
		}
	}
	if !slices.Equal(gotNames, want) {
		t.Fatalf("ReadDir = %q\nwant      %q", gotNames, want)
	}
	for range 5 {
		again, err := r.ReadDir("d")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(again, got) {
			t.Fatal("ReadDir is not deterministic")
		}
	}

	empty, err := r.ReadDir("d/subdir")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty directory: %v, %v", empty, err)
	}
	_, err = r.ReadDir("d/a")
	wantCode(t, err, CodeNotDirectory)
}

// A directory with many entries needs several getdents64 calls: this checks
// the parsing of records that straddle buffers.
func TestReadDirManyFiles(t *testing.T) {
	r, dir := newRoot(t)
	const n = 3000
	for i := range n {
		name := fmt.Sprintf("file-%05d-%s", i, "a-name-long-enough-to-fill-the-buffer")
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.ReadDir("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("%d entries, want %d", len(got), n)
	}
	if !slices.IsSortedFunc(got, func(a, b DirEntry) int { return strings.Compare(a.Name, b.Name) }) {
		t.Fatal("entries not sorted")
	}
}

func TestFileTypeString(t *testing.T) {
	for _, ft := range []FileType{TypeUnknown, TypeRegular, TypeDir, TypeSymlink, TypeFIFO,
		TypeSocket, TypeBlockDevice, TypeCharDevice} {
		if ft.String() == "" {
			t.Fatalf("type %d without a name", ft)
		}
	}
	if TypeRegular.IsSpecial() || TypeDir.IsSpecial() || !TypeSymlink.IsSpecial() || !TypeUnknown.IsSpecial() {
		t.Fatal("wrong IsSpecial")
	}
}

func TestErrorWithoutAbsolutePaths(t *testing.T) {
	r, dir := newRoot(t)
	_, err := r.Open("missing/entirely")
	if err == nil {
		t.Fatal("want an error")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("untyped error: %T", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("the error contains the absolute path: %v", err)
	}
	if !errors.Is(err, unix.ENOENT) {
		t.Fatal("the underlying errno is not reachable with errors.Is")
	}
}
