package publish

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io/fs"
	"math/rand/v2"
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
// and the kernel's message, the tag helper's own ENOSPC included (N-143:
// its failure code no_space, since helper version 3); the
// published album and the originals stay byte for byte as they were; no
// staging is left. With enough space the same executor publishes.
//
// The filesystem is tmpfs, not ext4: no unprivileged container can mount
// an ext4 image (N-045). ext4's delayed-allocation ENOSPC at fsync stays
// covered by injection (render.TestBuildNoSpace).
func TestExecuteRenderOnAReallyFullDisk(t *testing.T) {
	fullDiskSweep(t, func(src string) { writeAlbum(t, src, "a", "Artist", "Album", 2) }, sixteenths)
}

// The same with MP3 tracks, whose tag write grows the file: the kernel's
// ENOSPC also hits the helper's own write, which reports it as no_space,
// and the job fails with insufficient_space as for any other write.
func TestExecuteRenderMP3OnAReallyFullDisk(t *testing.T) {
	// One block at a time over the first tracks: a small MP3's tag write
	// needs only one block more than its copy.
	blocks := func(need, bs int64) []int64 {
		var out []int64
		for k := int64(0); k <= 40; k += 2 {
			out = append(out, k*bs)
		}
		return append(out, need+64<<10)
	}
	messages := fullDiskSweep(t, func(src string) {
		writeMP3Album(t, src, "a", "Artist", "Album", 2)
		// A cover of about 50 KB: each tag write grows its file by whole
		// pages.
		if err := os.WriteFile(filepath.Join(src, "a", "cover.png"), noisePNG(t, 128, 128), 0o644); err != nil {
			t.Fatal(err)
		}
	}, blocks)
	for _, msg := range messages {
		if strings.Contains(msg, "the tags of") {
			return
		}
	}
	t.Fatalf("ENOSPC never hit the tag writer: %q", messages)
}

// sixteenths are the free-space levels of the sweep: from nothing to need
// in 16 steps, then more than need.
func sixteenths(need, bs int64) []int64 {
	var leaves []int64
	for k := int64(0); k <= 16; k++ {
		leaves = append(leaves, k*need/16/bs*bs)
	}
	return append(leaves, need+64<<10)
}

// fullDiskSweep runs the sweep on the album writeSource writes into the
// import source, over the free-space levels leavesOf gives for what a build
// needs and the block size, and returns the error messages of the failed
// jobs.
func fullDiskSweep(t *testing.T, writeSource func(src string), leavesOf func(need, bs int64) []int64) []string {
	d := faulttest.FullFS(t)
	m := newMediaEnvAt(t, filepath.Join(d.Dir, "data"))
	writeSource(m.src)
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
	leaves := leavesOf(need, bs)

	var (
		mu   sync.Mutex
		last failpoint.Point
	)
	failures := map[string]string{} // the last point reached -> the job's error
	var messages []string
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
		messages = append(messages, msg)
		// Go's errno text is lower case, the helper's strerror is not.
		if !strings.Contains(strings.ToLower(msg), "no space left on device") {
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
	return messages
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

// noisePNG is a PNG of fixed pseudo-random pixels, which do not compress.
func noisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = byte(r.IntN(256))
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
