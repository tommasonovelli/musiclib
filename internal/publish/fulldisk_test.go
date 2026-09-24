package publish

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"musiclib/internal/failpoint"
	"musiclib/internal/faulttest"
	"musiclib/internal/render"
)

// §12.2 "Disco pieno durante build: vecchio album e originali intatti" on
// a really full filesystem (N-045): the whole data volume is on the small
// fixed-size filesystem the Compose test services mount (faulttest.FullFS),
// and a real album is rebuilt through the real render executor. Right
// after the build reserved its space (§11.2's check passed), an "external
// writer" fills the disk so that only a given number of bytes stays free
// (§11.2: "Non è una garanzia contro scritture esterne ... ogni write
// gestisce comunque ENOSPC"). The sweep of free space from nothing to more
// than the build needs makes the kernel's own ENOSPC hit the build at
// different real points: the copies of the tracks and of the attachment,
// and the receipt.
//
// Every failure fails the job before the journal with insufficient_space
// and the kernel's message (N-143: the tag helper's own ENOSPC would be
// media_tags_io, and this test would then fail on purpose); the
// published album and the originals stay byte for byte as they were; no
// staging is left. With enough space the same executor publishes.
//
// The filesystem is tmpfs, not ext4: no unprivileged container can mount
// an ext4 image (N-045). ext4's delayed-allocation ENOSPC at fsync stays
// covered by injection (render.TestBuildNoSpace).
func TestExecuteRenderOnAReallyFullDisk(t *testing.T) {
	d := faulttest.FullFS(t)
	m := newMediaEnvAt(t, filepath.Join(d.Dir, "data"))
	writeAlbum(t, m.src, "a", "Artist", "Album", 2)
	if err := os.WriteFile(filepath.Join(m.src, "a", "booklet.txt"), []byte(strings.Repeat("liner notes ", 20000)), 0o644); err != nil {
		t.Fatal(err)
	}
	m.importAll()
	m.runPool(1)
	id := m.albumID("Album")
	old := m.publishedResult(id)
	library, originals := m.tree("library"), m.tree("originals")

	bs, err := d.BlockSize()
	if err != nil {
		t.Fatal(err)
	}
	// What a build occupies: the published album's blocks, a few more for
	// the new tags.
	need := blocksOf(t, m.path("library/"+old.Dir), bs) + 4*bs
	var leaves []int64
	for k := int64(0); k <= 16; k++ {
		leaves = append(leaves, k*need/16/bs*bs)
	}
	leaves = append(leaves, need+64<<10)

	var (
		mu   sync.Mutex
		last failpoint.Point
	)
	failures := map[string]string{} // the last point reached -> the job's error
	published := false
	for i, leave := range leaves {
		m.bump(id)
		c := m.claim()
		var fillErr error
		m.bfp.Set(func(p failpoint.Point) error {
			mu.Lock()
			defer mu.Unlock()
			last = failpoint.Point{Name: p.Name, Path: p.Path}
			if p.Name == "reserved" {
				fillErr = d.Fill(leave)
			}
			return nil
		})
		err := m.p.ExecuteRender(context.Background(), c)
		m.bfp.Set(nil)
		if derr := d.Drain(); derr != nil || fillErr != nil {
			t.Fatalf("leave %d: the ballast: %v %v", leave, fillErr, derr)
		}
		if err != nil {
			t.Fatalf("leave %d: ExecuteRender: %v", leave, err)
		}
		m.wantTree("originals", originals)
		m.wantWorkClean()
		if _, pending := m.journal(); pending {
			t.Fatalf("leave %d: a journal after the build", leave)
		}
		j, ok := m.renderJob(id)
		if !ok {
			// Published: only with enough space, and only at the end.
			if i != len(leaves)-1 && leave < need/2 {
				t.Fatalf("leave %d of %d needed: published", leave, need)
			}
			published = true
			break
		}
		code, msg := deref(j.ErrorCode), deref(j.ErrorMessage)
		if j.State != "failed" || code != render.CodeInsufficientSpace {
			t.Fatalf("leave %d: job %s %s: %s", leave, j.State, code, msg)
		}
		if !strings.Contains(msg, "no space left on device") {
			t.Fatalf("leave %d: the kernel's ENOSPC is not in the error: %s", leave, msg)
		}
		m.wantTree("library", library)
		m.wantPublished(id, old)
		mu.Lock()
		failures[last.Name+" "+last.Path] = code
		mu.Unlock()
	}
	t.Logf("need %d bytes; failures by the last point reached: %v", need, failures)
	if !published {
		t.Fatalf("never published, even with %d bytes free", leaves[len(leaves)-1])
	}
	if len(failures) < 3 {
		t.Fatalf("ENOSPC hit the build at %d points only: %v", len(failures), failures)
	}
	res := m.publishedResult(id)
	if res.AlbumRevision != m.album(id).Revision {
		t.Fatalf("published revision %d, want %d", res.AlbumRevision, m.album(id).Revision)
	}
	m.wantPublished(id, res)
	m.wantTree("originals", originals)
}

// blocksOf is the space the files under dir occupy, in whole blocks.
func blocksOf(t *testing.T, dir string, bs int64) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(dir, func(p string, de fs.DirEntry, err error) error {
		if err != nil || !de.Type().IsRegular() {
			return err
		}
		fi, err := de.Info()
		if err != nil {
			return err
		}
		n += (fi.Size() + bs - 1) / bs * bs
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
