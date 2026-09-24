package blobstore

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"musiclib/internal/faulttest"
)

// §12.2 with real crashes: a child process puts a blob and is killed with
// SIGKILL at each named point of §7.5; the parent is the next boot. §3.2
// guarantee 2 and §7.5: no pinned blob is ever corrupt, the temporaries
// are removed by CleanTemps (§11.1 step 5), and putting the same content
// again is idempotent and never overwrites a pinned blob.

// putPoints are the named points of Put, in protocol order.
var putPoints = []string{"temp_synced", "temp_verified", "shards_synced", "pinned"}

// crashData is the content the children put: several copy buffers.
func crashData() []byte { return randomBytes(rand.New(rand.NewPCG(10, 11)), 3*bufSize+17) }

func TestHelperProcess(t *testing.T) {
	mode := faulttest.Mode()
	if mode == "" {
		return
	}
	e := newEnvAt(t, os.Getenv("BLOB_DIR"))
	e.s.failpoints = faulttest.Crash(os.Getenv("CRASH_AT"))
	switch mode {
	case "put":
		b, err := e.s.Put(context.Background(), bytes.NewReader(crashData()))
		if err != nil {
			faulttest.Exit("error " + Code(err))
		}
		faulttest.Exit("put " + b.SHA256)
	}
	faulttest.Exit("unknown mode " + mode)
}

func TestCrashDuringPut(t *testing.T) {
	data := crashData()
	want := blobOf(data)
	other := []byte("an unrelated pinned blob")
	for _, at := range putPoints {
		t.Run(at, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := newEnvAt(t, dir).s.Put(context.Background(), bytes.NewReader(other)); err != nil {
				t.Fatal(err)
			}
			if got := faulttest.RunChild(t, time.Minute, "put", "BLOB_DIR="+dir, "CRASH_AT="+at); got != faulttest.Killed {
				t.Fatalf("child: %q, want killed at %s", got, at)
			}

			// The next boot: a fresh store on the same volume.
			e := newEnvAt(t, dir)
			pinned := at == "pinned"
			temps, err := os.ReadDir(e.temps)
			if err != nil {
				t.Fatal(err)
			}
			if wantTemps := map[bool]int{true: 0, false: 1}[pinned]; len(temps) != wantTemps {
				t.Fatalf("%d temporaries after a crash at %s, want %d", len(temps), at, wantTemps)
			}
			if pinned {
				// Pinned means complete: the rename happened after the
				// verified temporary was fsynced.
				e.assertBlob(t, data)
				if size, err := e.s.Verify(context.Background(), want.SHA256); err != nil || size != want.Size {
					t.Fatalf("Verify: %d %v", size, err)
				}
			} else {
				e.assertAbsent(t, want.SHA256)
			}
			removed, err := e.s.CleanTemps(context.Background())
			if err != nil || len(removed) != len(temps) {
				t.Fatalf("CleanTemps removed %q (%v), want %d", removed, err, len(temps))
			}
			e.assertNoTemps(t)
			e.assertBlob(t, other)

			// The same content again, twice: the same blob, and a pinned
			// file is never replaced (same inode).
			var ino uint64
			for round := range 2 {
				got, err := e.s.Put(context.Background(), bytes.NewReader(data))
				if err != nil || got != want {
					t.Fatalf("re-put %d: %+v %v", round, got, err)
				}
				e.assertBlob(t, data)
				e.assertNoTemps(t)
				var st unix.Stat_t
				if err := unix.Stat(e.path(want.SHA256), &st); err != nil {
					t.Fatal(err)
				}
				if round > 0 && st.Ino != ino {
					t.Fatalf("the pinned blob was replaced by the re-put (inode %d -> %d)", ino, st.Ino)
				}
				ino = st.Ino
			}
		})
	}
}

// N-045, §12.2 "Disco pieno": on a really full filesystem (the daemon's
// small tmpfs, internal/faulttest.FullFS), the kernel's own ENOSPC fails
// the put with CodeNoSpace, keeps the errno, leaves no temporary and no
// blob, and leaves every pinned blob intact. With exactly the blob's
// blocks free, the put succeeds.
func TestPutOnAReallyFullFilesystem(t *testing.T) {
	d := faulttest.FullFS(t)
	e := newEnvAt(t, d.Dir)
	other := []byte("an unrelated pinned blob")
	if _, err := e.s.Put(context.Background(), bytes.NewReader(other)); err != nil {
		t.Fatal(err)
	}
	bs, err := d.BlockSize()
	if err != nil {
		t.Fatal(err)
	}
	data := crashData()
	blocks := (int64(len(data)) + bs - 1) / bs * bs
	for _, leave := range []int64{0, bs, blocks / 2, blocks - bs} {
		if err := d.Fill(leave); err != nil {
			t.Fatal(err)
		}
		_, err := e.s.Put(context.Background(), bytes.NewReader(data))
		wantCode(t, err, CodeNoSpace)
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("leave %d: the errno is lost: %v", leave, err)
		}
		e.assertNoTemps(t)
		e.assertAbsent(t, blobOf(data).SHA256)
		e.assertBlob(t, other)
		if _, err := os.Stat(filepath.Join(d.Dir, "originals")); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Fill(blocks); err != nil {
		t.Fatal(err)
	}
	if got, err := e.s.Put(context.Background(), bytes.NewReader(data)); err != nil || got != blobOf(data) {
		t.Fatalf("with exactly its blocks free: %+v %v", got, err)
	}
	e.assertBlob(t, data)
	e.assertNoTemps(t)
}
