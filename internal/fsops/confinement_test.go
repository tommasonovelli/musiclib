package fsops

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"musiclib/internal/names"
)

// Invalid paths are rejected by validation (internal/names codes) before any
// syscall: the secret outside the root really exists, so a disk error could
// not mask a successful resolution.
func TestInvalidPathsRejectedBeforeDisk(t *testing.T) {
	r, dir := newRoot(t)
	secret := filepath.Join(filepath.Dir(dir), "secret")
	writeAbs(t, secret, "outside")
	writeAbs(t, filepath.Join(dir, "a", "f"), "inside")

	tests := []struct {
		name string
		in   string
		code string
	}{
		{"dot dot", "..", names.CodePathDotSegment},
		{"traversal", "../secret", names.CodePathDotSegment},
		{"nested traversal", "a/../../secret", names.CodePathDotSegment},
		{"dot", "a/./f", names.CodePathDotSegment},
		{"absolute", "/etc/passwd", names.CodePathAbsolute},
		{"absolute to the root itself", dir + "/a/f", names.CodePathAbsolute},
		{"empty segment", "a//f", names.CodePathEmptySegment},
		{"trailing slash", "a/", names.CodePathEmptySegment},
		{"empty", "", names.CodePathEmpty},
		{"NUL byte", "a/f\x00", names.CodePathNulByte},
		{"invalid utf8", "a/\xff", names.CodeInvalidUTF8},
	}
	ops := map[string]func(string) error{
		"Open":            func(p string) error { return openClose(r, p, os.O_RDONLY) },
		"OpenFile":        func(p string) error { return openClose(r, p, os.O_RDWR|os.O_CREATE) },
		"CreateExclusive": func(p string) error { return openClose(r, p, os.O_WRONLY|os.O_CREATE|os.O_EXCL) },
		"Stat":            func(p string) error { _, err := r.Stat(p); return err },
		"ReadDir":         func(p string) error { _, err := r.ReadDir(p); return err },
		"SyncDir":         func(p string) error { return r.SyncDir(p) },
		"SyncDirAndParents": func(p string) error {
			return r.SyncDirAndParents(p)
		},
		"Mkdir":        func(p string) error { return r.Mkdir(p, 0o755) },
		"MkdirAll":     func(p string) error { _, err := r.MkdirAll(p, 0o755); return err },
		"MkdirAllSync": func(p string) error { return r.MkdirAllSync(p, 0o755) },
		"Remove":       func(p string) error { return r.Remove(p) },
		"Rmdir":        func(p string) error { return r.Rmdir(p) },
		"RemoveAll":    func(p string) error { return r.RemoveAll(t.Context(), p) },
		"RenameNoReplace source": func(p string) error {
			return RenameNoReplace(r, p, r, "a/g")
		},
		"RenameNoReplace destination": func(p string) error {
			return RenameNoReplace(r, "a/f", r, p)
		},
		"RenameExchange": func(p string) error { return RenameExchange(r, "a/f", r, p) },
		"SubRoot": func(p string) error {
			s, err := r.SubRoot(p)
			if err == nil {
				return s.Close()
			}
			return err
		},
		"Lock": func(p string) error {
			l, err := r.Lock(p)
			if err == nil {
				return l.Close()
			}
			return err
		},
	}
	// The empty path denotes the root for these operations, by contract.
	rootOK := map[string]bool{"Stat": true, "ReadDir": true, "SyncDir": true, "SyncDirAndParents": true}
	for _, tc := range tests {
		for opName, op := range ops {
			if tc.in == "" && rootOK[opName] {
				continue
			}
			t.Run(tc.name+"/"+opName, func(t *testing.T) {
				wantCode(t, op(tc.in), tc.code)
			})
		}
	}
	if got := readAbs(t, secret); got != "outside" {
		t.Fatalf("the file outside the root was touched: %q", got)
	}
	if got := readAbs(t, filepath.Join(dir, "a", "f")); got != "inside" {
		t.Fatalf("the file inside the root was touched: %q", got)
	}
}

// Even when validation is bypassed, the kernel rejects the escape:
// confinement is structural, not left to sanitization alone (§10.4).
func TestOpenat2RejectsEscapeWithoutValidation(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(filepath.Dir(dir), "secret"), "outside")
	mkdirAbs(t, filepath.Join(dir, "a"))

	for _, p := range []string{"..", "../secret", "a/../../secret", "/etc/passwd", dir} {
		fd, err := r.openat2(p, unix.O_RDONLY, 0)
		if err == nil {
			_ = closeFD("test", p, fd)
			t.Fatalf("openat2(%q) succeeded: escape from the root", p)
		}
		if !errors.Is(err, unix.EXDEV) {
			t.Fatalf("openat2(%q) = %v, want EXDEV", p, err)
		}
	}
	// A ".." that stays inside the root is allowed by the kernel: validation
	// rejects it anyway, but the kernel is not the source of that ban.
	fd, err := r.openat2("a/..", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatalf("openat2(\"a/..\") = %v", err)
	}
	if err := closeFD("test", "", fd); err != nil {
		t.Fatal(err)
	}
}

func TestLeafSymlinkRejected(t *testing.T) {
	r, dir := newRoot(t)
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	writeAbs(t, outside, "secret")
	writeAbs(t, filepath.Join(dir, "inside.txt"), "inner")
	mkdirAbs(t, filepath.Join(dir, "inner-dir"))
	symlinkAbs(t, outside, filepath.Join(dir, "to-outside"))
	symlinkAbs(t, "inside.txt", filepath.Join(dir, "to-inside"))
	symlinkAbs(t, "inner-dir", filepath.Join(dir, "to-inner-dir"))
	symlinkAbs(t, "missing", filepath.Join(dir, "dangling"))

	for _, name := range []string{"to-outside", "to-inside", "to-inner-dir", "dangling"} {
		t.Run(name, func(t *testing.T) {
			wantCode(t, openClose(r, name, os.O_RDONLY), CodeSymlink)
			wantCode(t, openClose(r, name, os.O_WRONLY|os.O_TRUNC), CodeSymlink)
			// O_CREAT without O_EXCL on a dangling symlink would create the
			// target: RESOLVE_NO_SYMLINKS prevents it.
			wantCode(t, openClose(r, name, os.O_WRONLY|os.O_CREATE), CodeSymlink)
			wantCode(t, openClose(r, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL), CodeExists)
			_, err := r.ReadDir(name)
			wantCode(t, err, CodeSymlink)
			wantCode(t, r.SyncDir(name), CodeSymlink)
			_, err = r.SubRoot(name)
			wantCode(t, err, CodeSymlink)
			_, err = r.MkdirAll(name, 0o755)
			wantCode(t, err, CodeSymlink)
			_, err = r.Lock(name)
			wantCode(t, err, CodeSymlink)
			wantCode(t, RenameNoReplace(r, name, r, "renamed"), CodeSymlink)
			wantCode(t, RenameExchange(r, "inside.txt", r, name), CodeSymlink)
			fi, err := r.Stat(name)
			if err != nil || fi.Type != TypeSymlink {
				t.Fatalf("Stat must report the symlink, not follow it: %+v, %v", fi, err)
			}
		})
	}
	if _, err := os.Lstat(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the target of the dangling symlink was created: %v", err)
	}
	if got := readAbs(t, outside); got != "secret" {
		t.Fatalf("the outside file was modified: %q", got)
	}
}

func TestIntermediateSymlinkComponentRejected(t *testing.T) {
	r, dir := newRoot(t)
	outsideDir := filepath.Join(filepath.Dir(dir), "outside-dir")
	writeAbs(t, filepath.Join(outsideDir, "sub", "f"), "secret")
	writeAbs(t, filepath.Join(dir, "real", "sub", "f"), "inner")
	symlinkAbs(t, outsideDir, filepath.Join(dir, "link-outside"))
	symlinkAbs(t, "real", filepath.Join(dir, "link-inside"))

	for _, base := range []string{"link-outside", "link-inside"} {
		t.Run(base, func(t *testing.T) {
			wantCode(t, openClose(r, base+"/sub/f", os.O_RDONLY), CodeSymlink)
			wantCode(t, openClose(r, base+"/sub/new-file", os.O_WRONLY|os.O_CREATE|os.O_EXCL), CodeSymlink)
			wantCode(t, r.Mkdir(base+"/sub/new-dir", 0o755), CodeSymlink)
			_, err := r.MkdirAll(base+"/sub/x/y", 0o755)
			wantCode(t, err, CodeSymlink)
			wantCode(t, r.Remove(base+"/sub/f"), CodeSymlink)
			wantCode(t, r.Rmdir(base+"/sub"), CodeSymlink)
			wantCode(t, r.RemoveAll(t.Context(), base+"/sub"), CodeSymlink)
			_, err = r.Stat(base + "/sub/f")
			wantCode(t, err, CodeSymlink)
			_, err = r.ReadDir(base + "/sub")
			wantCode(t, err, CodeSymlink)
			wantCode(t, r.SyncDir(base+"/sub"), CodeSymlink)
			wantCode(t, r.SyncDirAndParents(base+"/sub"), CodeSymlink)
			_, err = r.SubRoot(base + "/sub")
			wantCode(t, err, CodeSymlink)
			_, err = r.Lock(base + "/sub/lock")
			wantCode(t, err, CodeSymlink)
			wantCode(t, RenameNoReplace(r, base+"/sub/f", r, "stolen"), CodeSymlink)
			wantCode(t, RenameNoReplace(r, "real/sub/f", r, base+"/sub/g"), CodeSymlink)
		})
	}
	if got := readAbs(t, filepath.Join(outsideDir, "sub", "f")); got != "secret" {
		t.Fatalf("outside file altered: %q", got)
	}
	entries, err := os.ReadDir(filepath.Join(outsideDir, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries created outside the root: %v", entries)
	}
	if got := readAbs(t, filepath.Join(dir, "real", "sub", "f")); got != "inner" {
		t.Fatalf("inner file altered through the inner link: %q", got)
	}
}

// TOCTOU: the path is validated while it is still harmless, then an
// intermediate directory is replaced by a symlink pointing outside before
// use. Resolution happens in a single confined syscall, so the replacement
// produces an error, not an escape.
func TestTOCTOUDirectoryReplacedBySymlink(t *testing.T) {
	r, dir := newRoot(t)
	outsideDir := filepath.Join(filepath.Dir(dir), "outside-dir")
	writeAbs(t, filepath.Join(outsideDir, "secret"), "outside")
	writeAbs(t, filepath.Join(dir, "a", "b", "secret"), "inside")

	const rel = "a/b/secret"
	if _, err := names.SplitRelPath(rel); err != nil {
		t.Fatalf("validation: %v", err)
	}
	if got := readRoot(t, r, rel); got != "inside" {
		t.Fatalf("before the replacement: %q", got)
	}

	// Replacement between validation and use.
	if err := os.Rename(filepath.Join(dir, "a", "b"), filepath.Join(dir, "a", "b-old")); err != nil {
		t.Fatal(err)
	}
	symlinkAbs(t, outsideDir, filepath.Join(dir, "a", "b"))

	wantCode(t, openClose(r, rel, os.O_RDONLY), CodeSymlink)
	wantCode(t, r.Remove(rel), CodeSymlink)
	wantCode(t, r.RemoveAll(t.Context(), rel), CodeSymlink)
	wantCode(t, RenameNoReplace(r, rel, r, "a/stolen"), CodeSymlink)
	if got := readAbs(t, filepath.Join(outsideDir, "secret")); got != "outside" {
		t.Fatalf("outside file altered: %q", got)
	}
}

// TOCTOU under a real race: a goroutine keeps swapping a directory of the
// path with a symlink pointing outside (atomically, with RENAME_EXCHANGE)
// while the root opens, creates and removes through that path. Every
// operation must either act inside the root or fail with CodeSymlink; the
// outside directory must never be read or written.
func TestTOCTOUConcurrentSymlinkSwap(t *testing.T) {
	r, dir := newRoot(t)
	outsideDir := filepath.Join(filepath.Dir(dir), "outside-dir")
	writeAbs(t, filepath.Join(outsideDir, "secret"), "outside")
	writeAbs(t, filepath.Join(dir, "a", "b", "secret"), "inside")
	symlinkAbs(t, outsideDir, filepath.Join(dir, "a", "swap"))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b, swap := filepath.Join(dir, "a", "b"), filepath.Join(dir, "a", "swap")
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := unix.Renameat2(unix.AT_FDCWD, b, unix.AT_FDCWD, swap, unix.RENAME_EXCHANGE); err != nil {
				t.Errorf("swap: %v", err)
				return
			}
		}
	}()

	var inside, rejected int
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; time.Now().Before(deadline) && (i < 500 || inside == 0 || rejected == 0); i++ {
		f, err := r.Open("a/b/secret")
		switch {
		case err == nil:
			got, rerr := io.ReadAll(f)
			mustClose(t, f)
			if rerr != nil || string(got) != "inside" {
				t.Fatalf("read %q (%v) through the swapped path", got, rerr)
			}
			inside++
		case Code(err) == CodeSymlink:
			rejected++
		default:
			t.Fatalf("Open: unexpected error %v", err)
		}
		err = openClose(r, fmt.Sprintf("a/b/new-%d", i), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil && Code(err) != CodeSymlink {
			t.Fatalf("CreateExclusive: unexpected error %v", err)
		}
		_, err = r.MkdirAll(fmt.Sprintf("a/b/dir-%d/x", i), 0o755)
		if err != nil && Code(err) != CodeSymlink {
			t.Fatalf("MkdirAll: unexpected error %v", err)
		}
	}
	close(stop)
	wg.Wait()
	t.Logf("%d reads inside the root, %d rejected as symlink", inside, rejected)
	if inside == 0 || rejected == 0 {
		t.Fatalf("the race was not exercised: %d inside, %d rejected", inside, rejected)
	}
	entries, err := os.ReadDir(outsideDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret" {
		t.Fatalf("entries created outside the root: %v", entries)
	}
	if got := readAbs(t, filepath.Join(outsideDir, "secret")); got != "outside" {
		t.Fatalf("outside file altered: %q", got)
	}
}

// An already resolved directory descriptor keeps referring to the resolved
// inode even if the path is later replaced: the single-component operation
// does not escape the root.
func TestResolvedDescriptorSurvivesReplacement(t *testing.T) {
	r, dir := newRoot(t)
	outsideDir := filepath.Join(filepath.Dir(dir), "outside-dir")
	mkdirAbs(t, outsideDir)
	mkdirAbs(t, filepath.Join(dir, "a", "b"))

	dirfd, name, err := r.resolveParent("a/b/new-dir")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeFD("test", "", dirfd); err != nil {
			t.Error(err)
		}
	}()

	if err := os.Rename(filepath.Join(dir, "a", "b"), filepath.Join(dir, "a", "moved")); err != nil {
		t.Fatal(err)
	}
	symlinkAbs(t, outsideDir, filepath.Join(dir, "a", "b"))

	if err := unix.Mkdirat(dirfd, name, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a", "moved", "new-dir")); err != nil {
		t.Fatalf("the directory was not created in the resolved inode: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outsideDir, "new-dir")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("directory created outside the root")
	}
}

func TestOpenRoot(t *testing.T) {
	dir := t.TempDir()
	writeAbs(t, filepath.Join(dir, "file"), "x")
	symlinkAbs(t, "file", filepath.Join(dir, "link"))

	_, err := OpenRoot(filepath.Join(dir, "file"))
	wantCode(t, err, CodeNotDirectory)
	_, err = OpenRoot(filepath.Join(dir, "missing"))
	wantCode(t, err, CodeNotFound)
	if err != nil && strings.Contains(err.Error(), dir) {
		t.Fatalf("the error contains the absolute path: %v", err)
	}

	r, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, r)
	if !hasCloexec(t, r.fd) {
		t.Fatal("the root descriptor does not have FD_CLOEXEC")
	}
	if strings.Contains(r.Name(), "/") {
		t.Fatalf("the root name %q contains a path", r.Name())
	}
}

func TestSubRootConfined(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "work", "f"), "w")
	writeAbs(t, filepath.Join(dir, "library", "g"), "l")

	w, err := r.SubRoot("work")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, w)
	if got := readRoot(t, w, "f"); got != "w" {
		t.Fatalf("read from the sub-root: %q", got)
	}
	// The sub-root is itself a boundary: it cannot climb up to the parent.
	fd, err := w.openat2("../library/g", unix.O_RDONLY, 0)
	if err == nil {
		_ = closeFD("test", "", fd)
		t.Fatal("the sub-root allows climbing up to the parent")
	}
	if !errors.Is(err, unix.EXDEV) {
		t.Fatalf("want EXDEV, got %v", err)
	}
	if w.Name() != filepath.Base(dir)+"/work" {
		t.Fatalf("sub-root name %q", w.Name())
	}
	// The two roots have independent lifetimes.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readRoot(t, w, "f"); got != "w" {
		t.Fatalf("read from the sub-root after closing the parent: %q", got)
	}
}

// Structural guard against the regressions forbidden by §10.4 and §13.2. The
// analysis is on the AST of the non-test files, so comments do not count:
//
//   - no function that operates on a path built from strings;
//   - unix.Open only in OpenRoot, the only place that receives an absolute
//     path; unix.Openat2 only in sysOpenat2, which fixes the resolve flags;
//     no unix.Openat and no AT_FDCWD anywhere;
//   - unix.Close only in closeFD, so that no close error can be discarded
//     and every descriptor has a single, visible close.
func TestNoOperationOnStringBuiltPaths(t *testing.T) {
	forbidden := map[string]map[string]bool{
		"filepath": {"Join": true, "Abs": true, "EvalSymlinks": true, "Walk": true, "WalkDir": true, "Glob": true},
		"path":     {"Join": true},
		"os": {
			"Open": true, "OpenFile": true, "Create": true, "Mkdir": true, "MkdirAll": true,
			"Remove": true, "RemoveAll": true, "Rename": true, "Stat": true, "Lstat": true,
			"ReadDir": true, "ReadFile": true, "WriteFile": true, "Symlink": true,
			"Link": true, "Chdir": true, "Chmod": true, "Chown": true, "Truncate": true,
			"DirFS": true, "OpenRoot": true, "MkdirTemp": true, "CreateTemp": true,
		},
		"unix": {
			"Openat": true, "Rename": true, "Renameat": true, "Unlink": true, "Mkdir": true,
			"Rmdir": true, "Stat": true, "Lstat": true, "Chdir": true, "Statfs": true,
			"Creat": true, "Link": true, "Linkat": true, "Symlink": true, "Symlinkat": true,
			"Mknod": true, "Mknodat": true, "Mkfifo": true, "Mkfifoat": true, "AT_FDCWD": true,
			"Readlink": true, "Readlinkat": true, "Fchmodat": true, "Fchownat": true,
		},
	}
	// Selectors allowed only inside one function.
	onlyIn := map[string]string{
		"unix.Open":    "OpenRoot",
		"unix.Openat2": "sysOpenat2",
		"unix.Close":   "closeFD",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, decl := range file.Decls {
			fnName := ""
			if fn, ok := decl.(*ast.FuncDecl); ok {
				fnName = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				full := id.Name + "." + sel.Sel.Name
				if want, ok := onlyIn[full]; ok {
					if fnName != want {
						t.Errorf("%s: %s is allowed only in %s", fset.Position(sel.Pos()), full, want)
					}
					return true
				}
				if forbidden[id.Name][sel.Sel.Name] {
					t.Errorf("%s: %s operates on a path built from strings (§10.4)",
						fset.Position(sel.Pos()), full)
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("no source file analyzed")
	}
}

// No operation leaks a descriptor, on success or on error. Together with
// the guard above (every close goes through closeFD and its error is never
// discarded) this is the evidence for single ownership (N-014): a double
// close would surface as an EBADF error from the operation.
func TestNoDescriptorLeaks(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "tree", "a", "b", "f"), "x")
	writeAbs(t, filepath.Join(dir, "file"), "x")
	mkfifoAbs(t, filepath.Join(dir, "fifo"))
	symlinkAbs(t, "file", filepath.Join(dir, "link"))
	mkdirAbs(t, filepath.Join(dir, "full", "x"))

	run := func(i int) {
		t.Helper()
		steps := []func() error{
			func() error { return openClose(r, "file", os.O_RDONLY) },
			func() error { return openClose(r, "fifo", os.O_RDONLY) },
			func() error { return openClose(r, "fifo", os.O_WRONLY) },
			func() error { return openClose(r, "link", os.O_RDONLY) },
			func() error { return openClose(r, "missing/x", os.O_RDONLY) },
			func() error { return openClose(r, "tree", os.O_RDONLY) },
			func() error { _, err := r.Stat("tree/a/b/f"); return err },
			func() error { _, err := r.Stat("tree/missing/f"); return err },
			func() error { _, err := r.ReadDir("tree/a"); return err },
			func() error { return r.SyncDirAndParents("tree/a/b") },
			func() error { return r.Mkdir("tree/a", 0o755) },
			func() error { _, err := r.MkdirAll(fmt.Sprintf("made/%d/x/y", i), 0o755); return err },
			func() error { _, err := r.MkdirAll("file/x", 0o755); return err },
			func() error { return r.Rmdir("full") },
			func() error { return r.Remove("tree") },
			func() error { return r.Remove("missing") },
			func() error { return RenameNoReplace(r, "file", r, "tree/a/b/f") },
			func() error { return RenameNoReplace(r, "missing", r, "x") },
			func() error { return RenameExchange(r, "tree/a", r, "full") },
			func() error { return removeTree(t, r, dir, i) },
			func() error { l, err := r.Lock("lock"); return errors.Join(err, closeIfLock(l)) },
			func() error { s, err := r.SubRoot("tree"); return errors.Join(err, closeIfRoot(s)) },
			func() error { return ProbeRenameExchange(r, r) },
		}
		for _, step := range steps {
			_ = step() // errors are expected on the error paths: only descriptors matter
		}
	}
	run(0) // warm-up: lazily opened runtime descriptors are not leaks
	before := openFDs(t)
	for i := 1; i <= 3; i++ {
		run(i)
	}
	after := openFDs(t)
	if !slices.Equal(before, after) {
		t.Fatalf("descriptor leak: before %v, after %v", before, after)
	}
}

// removeTree builds a small tree with a symlink and a FIFO and removes it.
func removeTree(t *testing.T, r *Root, dir string, i int) error {
	t.Helper()
	base := filepath.Join(dir, fmt.Sprintf("doomed-%d", i))
	writeAbs(t, filepath.Join(base, "x", "y", "f"), "x")
	symlinkAbs(t, "/", filepath.Join(base, "x", "root-link"))
	mkfifoAbs(t, filepath.Join(base, "x", "y", "fifo"))
	return r.RemoveAll(t.Context(), fmt.Sprintf("doomed-%d", i))
}

func closeIfLock(l *Lock) error {
	if l == nil {
		return nil
	}
	return l.Close()
}

func closeIfRoot(r *Root) error {
	if r == nil {
		return nil
	}
	return r.Close()
}
