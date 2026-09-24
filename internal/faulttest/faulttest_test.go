package faulttest

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"musiclib/internal/failpoint"
)

func TestHelperProcess(t *testing.T) {
	mode := Mode()
	if mode == "" {
		return
	}
	// The child walks through the points a, b, c with a hook that crashes
	// at CRASH_AT, printing each point it passes.
	h := Crash(os.Getenv("CRASH_AT"))
	for _, p := range []string{"a", "b", "c"} {
		if err := h.Hit(p); err != nil {
			Exit("error " + err.Error())
		}
		os.Stdout.WriteString("passed " + p + "\n")
	}
	switch mode {
	case "points":
		Exit("completed")
	case "exit-1":
		os.Exit(1)
	case "silent":
		os.Exit(0)
	}
	Exit("unknown mode " + mode)
}

// A crash hook kills the child with SIGKILL at exactly its point: the
// points before it ran, the ones after did not.
func TestCrashKillsAtThePoint(t *testing.T) {
	for _, at := range []string{"a", "b", "c"} {
		c := Start(t, time.Minute, "points", "CRASH_AT="+at)
		if got := c.Wait(t); got != Killed {
			t.Fatalf("crash at %s: %q, want %q", at, got, Killed)
		}
		out := c.Output()
		for _, p := range []string{"a", "b", "c"} {
			passed := strings.Contains(out, "passed "+p+"\n")
			if want := p < at; passed != want {
				t.Fatalf("crash at %s: passed %s = %v\n%s", at, p, passed, out)
			}
		}
	}
	if got := RunChild(t, time.Minute, "points", "CRASH_AT=elsewhere"); got != "completed" {
		t.Fatalf("no crash point reached: %q", got)
	}
}

// Kill really is SIGKILL: the child cannot run a deferred function or
// catch it.
func TestKillIsSIGKILL(t *testing.T) {
	c := Start(t, time.Minute, "points", "CRASH_AT=a")
	c.Wait(t)
	ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("child state %v, want killed by SIGKILL", c.cmd.ProcessState)
	}
}

// A nil hook goes on at every point, and HitFile passes the path and the
// file.
func TestHookNilAndHitFile(t *testing.T) {
	var h failpoint.Hook
	if err := h.Hit("x"); err != nil {
		t.Fatal(err)
	}
	if err := h.HitFile("x", "p", os.Stdin); err != nil {
		t.Fatal(err)
	}
	var got failpoint.Point
	h = func(p failpoint.Point) error { got = p; return errors.New("injected") }
	if err := h.HitFile("write", "a/b", os.Stdin); err == nil || got.Name != "write" || got.Path != "a/b" || got.File != os.Stdin {
		t.Fatalf("HitFile: %v %+v", err, got)
	}
}

// A Switch changes the behaviour of a hook already handed out.
func TestSwitch(t *testing.T) {
	var s Switch
	h := s.Hook()
	if err := h.Hit("x"); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	s.Set(func(p failpoint.Point) error {
		if p.Name == "x" {
			return boom
		}
		return nil
	})
	if err := h.Hit("x"); !errors.Is(err, boom) {
		t.Fatalf("set: %v", err)
	}
	if err := h.Hit("y"); err != nil {
		t.Fatal(err)
	}
	s.Set(nil)
	if err := h.Hit("x"); err != nil {
		t.Fatalf("reset: %v", err)
	}
}

// Wait fails on anything but a result line or SIGKILL; checked on the
// classification only, since a failing Wait fails its test.
func TestChildWithoutResult(t *testing.T) {
	for _, mode := range []string{"exit-1", "silent"} {
		ft := &fakeT{TB: t}
		func() {
			defer func() { _ = recover() }()
			Start(ft, time.Minute, mode, "CRASH_AT=none").Wait(ft)
		}()
		if !ft.failed {
			t.Fatalf("mode %s: Wait did not fail", mode)
		}
	}
}

// fakeT records a fatal failure instead of ending the test.
type fakeT struct {
	testing.TB
	failed bool
}

func (f *fakeT) Fatalf(format string, args ...any) { f.failed = true; panic("fatal") }
func (f *fakeT) Helper()                           {}

// The full filesystem is really full: a write of more than what is left
// fails with ENOSPC from the kernel, and draining the ballast gives the
// space back.
func TestFullFS(t *testing.T) {
	d := FullFS(t)
	bs, err := d.BlockSize()
	if err != nil {
		t.Fatal(err)
	}
	for _, leave := range []int64{0, bs, 16 * bs, 1 << 20} {
		if err := d.Fill(leave); err != nil {
			t.Fatal(err)
		}
		got, err := d.Available()
		if err != nil || got != leave {
			t.Fatalf("available %d, want %d (%v)", got, leave, err)
		}
		p := filepath.Join(d.Dir, "f-"+strconv.FormatInt(leave, 10))
		err = os.WriteFile(p, make([]byte, leave+bs), 0o644)
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("leave %d: writing %d bytes: %v, want ENOSPC", leave, leave+bs, err)
		}
		if leave > 0 {
			if err := os.WriteFile(p, make([]byte, leave), 0o644); err != nil {
				t.Fatalf("leave %d: writing exactly what is left: %v", leave, err)
			}
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Drain(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Dir, "after"), make([]byte, 8<<20), 0o644); err != nil {
		t.Fatalf("after the drain: %v", err)
	}
	// Asking for more than the filesystem has is an error, not a partial
	// fill.
	if err := d.Fill(4 << 30); err == nil {
		t.Fatal("leaving 4 GiB on the small filesystem succeeded")
	}
}

// FullFS never cleans or fills anything but a small mount point that is
// not TMPDIR's filesystem: each guard refuses on its own.
func TestFullFSRefusesAnotherFilesystem(t *testing.T) {
	const small = 1 << 30
	for _, tc := range []struct {
		name                       string
		total                      int64
		rootDev, parentDev, tmpDev uint64
		ok                         bool
	}{
		{"the dedicated filesystem", small, 2, 1, 3, true},
		{"not a mount point", small, 2, 2, 3, false},
		{"too large", maxFullFS + 1, 2, 1, 3, false},
		{"TMPDIR's filesystem", small, 2, 1, 2, false},
	} {
		if err := dedicated(tc.total, tc.rootDev, tc.parentDev, tc.tmpDev); (err == nil) != tc.ok {
			t.Errorf("%s: %v, want ok = %v", tc.name, err, tc.ok)
		}
	}
	// And on the real filesystems: a directory of TMPDIR (not a mount
	// point, large, TMPDIR's) is refused.
	if err := checkDedicated(t.TempDir()); err == nil {
		t.Fatal("a directory of TMPDIR accepted as the dedicated filesystem")
	}
}

// Nothing linked into the production binary imports faulttest, and the
// production package failpoint imports nothing but os: the failpoints cost
// nothing and can do nothing in production.
func TestOnlyTestsImportFaulttest(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found: %v", err)
	}
	var checked int
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && p != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		checked++
		var imports []string
		for _, im := range f.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			imports = append(imports, path)
		}
		if !strings.HasPrefix(rel, "internal/faulttest/") && slices.Contains(imports, "musiclib/internal/faulttest") {
			t.Errorf("%s imports internal/faulttest outside a test", rel)
		}
		if strings.HasPrefix(rel, "internal/failpoint/") && !slices.Equal(imports, []string{"os"}) {
			t.Errorf("%s imports %q, want only os", rel, imports)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("only %d files checked: the walk did not see the module", checked)
	}
}
