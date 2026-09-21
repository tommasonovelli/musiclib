package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"testing/iotest"

	"golang.org/x/sys/unix"

	"musiclib/internal/fsops"
)

// Tests run on the real filesystem of t.TempDir(), ext4 in the Docker gate
// (§12.1). The adversary (tampering, leftovers, symlinks) uses os and
// absolute paths; the code under test only sees fsops roots.

type env struct {
	s                *Store
	originals, temps string // absolute, for the adversary only
}

func newEnv(t *testing.T) env {
	t.Helper()
	dir := t.TempDir()
	e := env{originals: filepath.Join(dir, "originals"), temps: filepath.Join(dir, "work", tempDir)}
	var err error
	if e.s, err = New(openRoot(t, e.originals), openRoot(t, filepath.Join(dir, "work"))); err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func openRoot(t *testing.T, dir string) *fsops.Root {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := fsops.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return r
}

func blobOf(data []byte) Blob {
	sum := sha256.Sum256(data)
	return Blob{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
}

func randomBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

func (e env) path(sha string) string {
	return filepath.Join(e.originals, filepath.FromSlash(relOf(sha)))
}

// plant writes content at sha's blob path as the adversary would.
func (e env) plant(t *testing.T, sha string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.path(sha)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.path(sha), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e env) assertNoTemps(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(e.temps)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("leftover temporaries: %v", entries)
	}
}

// assertBlob checks the pinned file byte for byte, and that it is read-only.
func (e env) assertBlob(t *testing.T, data []byte) {
	t.Helper()
	p := e.path(blobOf(data).SHA256)
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("pinned blob: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("pinned blob has %d different bytes", len(got))
	}
	if fi, err := os.Lstat(p); err != nil || fi.Mode() != blobPerm {
		t.Errorf("pinned blob mode %v (err %v), want %v", fi.Mode(), err, os.FileMode(blobPerm))
	}
}

func (e env) assertAbsent(t *testing.T, sha string) {
	t.Helper()
	if _, err := os.Lstat(e.path(sha)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("blob %s exists after a failed put (err %v)", sha, err)
	}
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if Code(err) != code {
		t.Fatalf("error %v: code %q, want %q", err, Code(err), code)
	}
}

// TestPutProperties: for any content, Put returns its SHA-256 and size, the
// pinned file holds exactly those bytes read-only, every read path agrees,
// no temporary is left, and a second put keeps the same inode (no
// overwrite, §3.2).
func TestPutProperties(t *testing.T) {
	e := newEnv(t)
	rng := rand.New(rand.NewPCG(1, 2))
	sizes := []int{0, 1, bufSize - 1, bufSize, bufSize + 1, 3*bufSize + 17}
	for range 8 {
		sizes = append(sizes, rng.IntN(4*bufSize))
	}
	for _, n := range sizes {
		data := randomBytes(rng, n)
		want := blobOf(data)
		// HalfReader exercises short reads from the source.
		got, err := e.s.Put(context.Background(), iotest.HalfReader(bytes.NewReader(data)))
		if err != nil || got != want {
			t.Fatalf("Put(%d bytes) = %+v, %v; want %+v", n, got, err, want)
		}
		e.assertBlob(t, data)
		ino := inode(t, e.path(want.SHA256))
		if again, err := e.s.Put(context.Background(), bytes.NewReader(data)); err != nil || again != want {
			t.Fatalf("second Put = %+v, %v", again, err)
		}
		if inode(t, e.path(want.SHA256)) != ino {
			t.Fatal("second Put replaced the pinned file")
		}
		if err := e.s.Check(want); err != nil {
			t.Errorf("Check: %v", err)
		}
		if size, err := e.s.Verify(context.Background(), want.SHA256); err != nil || size != want.Size {
			t.Errorf("Verify = %d, %v", size, err)
		}
		f, err := e.s.Open(want.SHA256)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		read, rerr := io.ReadAll(f)
		if cerr := f.Close(); rerr != nil || cerr != nil || !bytes.Equal(read, data) {
			t.Fatalf("Open read %d bytes, errors %v %v", len(read), rerr, cerr)
		}
	}
	e.assertNoTemps(t)
}

// TestPutConcurrentSameContent is the §12.2 row "Stesso blob fissato
// contemporaneamente": every caller succeeds and one intact file remains.
func TestPutConcurrentSameContent(t *testing.T) {
	e := newEnv(t)
	data := randomBytes(rand.New(rand.NewPCG(3, 4)), 2*bufSize+5)
	want := blobOf(data)
	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			<-start
			var got Blob
			got, errs[i] = e.s.Put(context.Background(), bytes.NewReader(data))
			if errs[i] == nil && got != want {
				errs[i] = errors.New("wrong blob returned")
			}
		})
	}
	close(start)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	e.assertBlob(t, data)
	shard, err := os.ReadDir(filepath.Dir(e.path(want.SHA256)))
	if err != nil || len(shard) != 1 {
		t.Fatalf("shard holds %v (err %v), want only the blob", shard, err)
	}
	e.assertNoTemps(t)
}

// TestPutDoesNotTrustExisting is the §12.2 row "Blob esistente corrotto":
// whatever sits at the name is verified, reported as corrupt_blob and left
// exactly as it was.
func TestPutDoesNotTrustExisting(t *testing.T) {
	data := []byte("the real content")
	sha := blobOf(data).SHA256
	cases := []struct {
		name  string
		setup func(t *testing.T, e env)
	}{
		{"same size, other bytes", func(t *testing.T, e env) { e.plant(t, sha, []byte("the fake content")) }},
		{"truncated", func(t *testing.T, e env) { e.plant(t, sha, data[:4]) }},
		{"symlink to a good copy", func(t *testing.T, e env) {
			good := filepath.Join(t.TempDir(), "good")
			if err := os.WriteFile(good, data, 0o644); err != nil {
				t.Fatal(err)
			}
			e.plant(t, sha, nil)
			if err := os.Remove(e.path(sha)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(good, e.path(sha)); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, e env) {
			if err := os.MkdirAll(e.path(sha), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			c.setup(t, e)
			before, err := os.Lstat(e.path(sha))
			if err != nil {
				t.Fatal(err)
			}
			_, err = e.s.Put(context.Background(), bytes.NewReader(data))
			wantCode(t, err, CodeCorrupt)
			after, err := os.Lstat(e.path(sha))
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || before.Size() != after.Size() ||
				before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
				t.Error("the existing entry was modified")
			}
			e.assertNoTemps(t)
		})
	}
}

// TestPutFailures: every failure before the rename leaves neither a
// temporary nor a blob; a failure after it leaves an intact blob that a
// retry accepts. Unrelated pinned blobs are never touched.
func TestPutFailures(t *testing.T) {
	data := randomBytes(rand.New(rand.NewPCG(5, 6)), 3*bufSize)
	other := []byte("an unrelated pinned blob")
	boom := errors.New("injected")
	cases := []struct {
		name       string
		src        func(cancel context.CancelFunc) io.Reader
		failpoint  func(e env, name string) error
		code       string
		wantPinned bool
		removalErr bool
	}{
		{name: "source read error", code: CodeSource,
			src: func(context.CancelFunc) io.Reader {
				return io.MultiReader(bytes.NewReader(data[:bufSize]), iotest.ErrReader(boom))
			}},
		{name: "cancel mid-copy", code: CodeCanceled,
			src: func(cancel context.CancelFunc) io.Reader {
				return io.MultiReader(bytes.NewReader(data[:bufSize]),
					readerFunc(func([]byte) (int, error) { cancel(); return 0, io.EOF }),
					bytes.NewReader(data[bufSize:]))
			}},
		{name: "ENOSPC at fsync", code: CodeNoSpace, failpoint: at("temp_synced",
			&os.PathError{Op: "fsync", Path: "blobs/x.tmp", Err: unix.ENOSPC})},
		{name: "temp altered before the re-read", code: CodeIO,
			failpoint: func(e env, name string) error {
				if name != "temp_synced" {
					return nil
				}
				return e.onTemp(func(p string) error {
					return errors.Join(os.Chmod(p, 0o644), os.WriteFile(p, []byte("bit rot"), 0o644))
				})
			}},
		{name: "failure before the rename", code: CodeIO, failpoint: at("shards_synced", boom)},
		{name: "failure after the rename", code: CodeIO, failpoint: at("pinned", boom), wantPinned: true},
		{name: "temp removal failure is reported", code: CodeIO, removalErr: true,
			failpoint: func(e env, name string) error {
				if name != "temp_verified" {
					return nil
				}
				return errors.Join(boom, e.onTemp(os.Remove))
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if _, err := e.s.Put(context.Background(), bytes.NewReader(other)); err != nil {
				t.Fatal(err)
			}
			if c.failpoint != nil {
				e.s.failpoint = func(name string) error { return c.failpoint(e, name) }
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var src io.Reader = bytes.NewReader(data)
			if c.src != nil {
				src = c.src(cancel)
			}
			_, err := e.s.Put(ctx, src)
			wantCode(t, err, c.code)
			if hasOp(err, "remove temp") != c.removalErr {
				t.Errorf("removal error reported = %v, want %v: %v", !c.removalErr, c.removalErr, err)
			}
			e.assertNoTemps(t)
			e.assertBlob(t, other)
			if !c.wantPinned {
				e.assertAbsent(t, blobOf(data).SHA256)
				return
			}
			e.assertBlob(t, data)
			e.s.failpoint = nil
			if got, err := e.s.Put(context.Background(), bytes.NewReader(data)); err != nil || got != blobOf(data) {
				t.Fatalf("retry = %+v, %v", got, err)
			}
		})
	}
}

// TestPutProtocolOrder pins the §7.5 step order at the points where the
// disk state is observable. Durability itself needs a power cut, not a test.
func TestPutProtocolOrder(t *testing.T) {
	e := newEnv(t)
	data := []byte("ordered")
	sha := blobOf(data).SHA256
	var seen []string
	e.s.failpoint = func(name string) error {
		seen = append(seen, name)
		temps, err := os.ReadDir(e.temps)
		if err != nil {
			return err
		}
		_, blobErr := os.Lstat(e.path(sha))
		_, shardErr := os.Lstat(filepath.Dir(e.path(sha)))
		pinned := name == "pinned"
		wantShard := pinned || name == "shards_synced"
		if (len(temps) == 1) == pinned || (blobErr == nil) != pinned || (shardErr == nil) != wantShard {
			return errors.New("unexpected disk state at " + name)
		}
		return nil
	}
	if _, err := e.s.Put(context.Background(), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"temp_synced", "temp_verified", "shards_synced", "pinned"}; !slices.Equal(seen, want) {
		t.Errorf("points %v, want %v", seen, want)
	}
}

// TestReadChecks covers the doctor's normal (Check) and deep (Verify) checks
// and Open against each state a blob name can be in (§11.3).
func TestReadChecks(t *testing.T) {
	data := []byte("pinned content")
	want := blobOf(data)
	cases := []struct {
		name                string
		tamper              func(t *testing.T, e env)
		check, verify, open string
	}{
		{"intact", nil, "", "", ""},
		{"missing", func(t *testing.T, e env) {
			if err := os.Remove(e.path(want.SHA256)); err != nil {
				t.Fatal(err)
			}
		}, CodeNotFound, CodeNotFound, CodeNotFound},
		{"same size, other bytes", func(t *testing.T, e env) {
			e.replace(t, want.SHA256, []byte("pinned CONTENT"))
		}, "", CodeCorrupt, ""},
		{"truncated", func(t *testing.T, e env) { e.replace(t, want.SHA256, data[:3]) }, CodeCorrupt, CodeCorrupt, ""},
		{"symlink", func(t *testing.T, e env) {
			if err := os.Remove(e.path(want.SHA256)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/etc/hostname", e.path(want.SHA256)); err != nil {
				t.Fatal(err)
			}
		}, CodeCorrupt, CodeCorrupt, CodeCorrupt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if _, err := e.s.Put(context.Background(), bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			if c.tamper != nil {
				c.tamper(t, e)
			}
			if err := e.s.Check(want); Code(err) != c.check {
				t.Errorf("Check: %v, want code %q", err, c.check)
			}
			if _, err := e.s.Verify(context.Background(), want.SHA256); Code(err) != c.verify {
				t.Errorf("Verify: %v, want code %q", err, c.verify)
			}
			f, err := e.s.Open(want.SHA256)
			if Code(err) != c.open {
				t.Errorf("Open: %v, want code %q", err, c.open)
			}
			if err == nil {
				if err := f.Close(); err != nil {
					t.Error(err)
				}
			}
		})
	}
}

func (e env) replace(t *testing.T, sha string, content []byte) {
	t.Helper()
	if err := os.Chmod(e.path(sha), 0o644); err != nil {
		t.Fatal(err)
	}
	e.plant(t, sha, content)
}

func TestInvalidHashRejected(t *testing.T) {
	e := newEnv(t)
	good := blobOf(nil).SHA256
	for _, sha := range []string{
		"", good[:63], good + "0", "../" + good[3:], good[:63] + "G", good[:63] + "A", good[:62] + "/x",
	} {
		_, verr := e.s.Verify(context.Background(), sha)
		_, oerr := e.s.Open(sha)
		for _, err := range []error{ValidateSHA(sha), e.s.Check(Blob{SHA256: sha}), verr, oerr} {
			wantCode(t, err, CodeInvalidHash)
		}
	}
}

// TestCleanTemps: only regular *.tmp entries of work/blobs go (§11.1 step 5);
// pinned blobs and foreign entries stay.
func TestCleanTemps(t *testing.T) {
	e := newEnv(t)
	data := []byte("kept")
	if _, err := e.s.Put(context.Background(), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.tmp", "b.tmp", "notes"} {
		if err := os.WriteFile(filepath.Join(e.temps, name), []byte("x"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(e.temps, "d.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := e.s.CleanTemps(context.Background())
	if err != nil || !slices.Equal(removed, []string{"a.tmp", "b.tmp"}) {
		t.Fatalf("CleanTemps = %v, %v", removed, err)
	}
	left, err := os.ReadDir(e.temps)
	if err != nil || len(left) != 2 {
		t.Errorf("left %v (err %v), want d.tmp and notes", left, err)
	}
	e.assertBlob(t, data)
}

func at(point string, err error) func(env, string) error {
	return func(_ env, name string) error {
		if name == point {
			return err
		}
		return nil
	}
}

// onTemp applies fn to the single in-flight temporary.
func (e env) onTemp(fn func(path string) error) error {
	entries, err := os.ReadDir(e.temps)
	if err != nil || len(entries) != 1 {
		return errors.Join(err, errors.New("expected one temporary"))
	}
	return fn(filepath.Join(e.temps, entries[0].Name()))
}

// hasOp reports whether any *Error in err's tree, joined errors included,
// failed in op.
func hasOp(err error, op string) bool {
	if e, ok := err.(*Error); ok && e.Op == op {
		return true
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return slices.ContainsFunc(j.Unwrap(), func(x error) bool { return hasOp(x, op) })
	}
	return false
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
