package fsops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMkdirAndMkdirAll(t *testing.T) {
	r, dir := newRoot(t)

	if err := r.Mkdir("a", 0o755); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.Mkdir("a", 0o755), CodeExists)
	wantCode(t, r.Mkdir("missing/b", 0o755), CodeNotFound)

	created, err := r.MkdirAll("a/b/c/d", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/b", "a/b/c", "a/b/c/d"}; !slices.Equal(created, want) {
		t.Fatalf("created %q, want %q", created, want)
	}
	created, err = r.MkdirAll("a/b/c/d", 0o755)
	if err != nil || len(created) != 0 {
		t.Fatalf("idempotent MkdirAll: %q, %v", created, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "a", "b", "c", "d")); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}

	writeAbs(t, filepath.Join(dir, "file"), "x")
	_, err = r.MkdirAll("file/below", 0o755)
	wantCode(t, err, CodeNotDirectory)
	_, err = r.MkdirAll("file", 0o755)
	wantCode(t, err, CodeNotDirectory)

	// On error the directories created before the failure are reported:
	// "ro" is created without write permission, so "ro/x" fails.
	if os.Geteuid() != 0 {
		created, err = r.MkdirAll("ro/x/y", 0o500)
		wantCode(t, err, CodePermission)
		if want := []string{"ro"}; !slices.Equal(created, want) {
			t.Fatalf("created on error %q, want %q", created, want)
		}
		if err := os.Chmod(filepath.Join(dir, "ro"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.MkdirAllSync("originals/ab/cd", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.MkdirAllSync("originals/ab/cd", 0o755); err != nil {
		t.Fatal(err)
	}
}

// A symlink in place of an expected directory is not adopted, neither as a
// leaf nor as an intermediate component, even if it points inside the root.
func TestMkdirAllRejectsSymlink(t *testing.T) {
	r, dir := newRoot(t)
	outside := filepath.Join(filepath.Dir(dir), "outside-dir")
	mkdirAbs(t, outside)
	mkdirAbs(t, filepath.Join(dir, "real"))
	symlinkAbs(t, outside, filepath.Join(dir, "to-outside"))
	symlinkAbs(t, "real", filepath.Join(dir, "to-inside"))

	for _, p := range []string{"to-outside", "to-outside/x", "to-inside", "to-inside/x/y"} {
		_, err := r.MkdirAll(p, 0o755)
		wantCode(t, err, CodeSymlink)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("created something outside the root: %v", entries)
	}
}

func TestRmdirAndRemove(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "artist", "album", "f"), "x")
	mkdirAbs(t, filepath.Join(dir, "empty"))
	mkfifoAbs(t, filepath.Join(dir, "fifo"))

	wantCode(t, r.Rmdir("artist"), CodeDirNotEmpty)
	wantCode(t, r.Remove("artist"), CodeIsDirectory)
	wantCode(t, r.Rmdir("artist/album/f"), CodeNotDirectory)
	if err := r.Remove("artist/album/f"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.Remove("artist/album/f"), CodeNotFound)
	if err := r.Rmdir("empty"); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.Rmdir("empty"), CodeNotFound)
	// A special file can be removed (never opened): work/ must be cleanable.
	if err := r.Remove("fifo"); err != nil {
		t.Fatal(err)
	}
}

// Remove deletes a symlink without following it: the outside target stays.
func TestRemoveSymlinkDoesNotFollowTarget(t *testing.T) {
	r, dir := newRoot(t)
	outside := filepath.Join(filepath.Dir(dir), "target")
	writeAbs(t, outside, "stays")
	mkdirAbs(t, filepath.Join(filepath.Dir(dir), "target-dir"))
	symlinkAbs(t, outside, filepath.Join(dir, "link"))
	symlinkAbs(t, filepath.Join(filepath.Dir(dir), "target-dir"), filepath.Join(dir, "dir-link"))

	for _, name := range []string{"link", "dir-link"} {
		if err := r.Remove(name); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the symlink %s was not removed", name)
		}
	}
	if got := readAbs(t, outside); got != "stays" {
		t.Fatalf("target altered: %q", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "target-dir")); err != nil {
		t.Fatalf("directory target removed: %v", err)
	}
}

// Confined RemoveAll: a tree with symlinks pointing outside (files and
// directories), inner symlinks and FIFOs is removed without ever traversing
// a link. Everything outside the root survives.
func TestRemoveAllConfined(t *testing.T) {
	r, dir := newRoot(t)
	outside := filepath.Join(filepath.Dir(dir), "outside-dir")
	writeAbs(t, filepath.Join(outside, "precious"), "do not touch")
	writeAbs(t, filepath.Join(outside, "sub", "nested"), "not this either")

	tree := filepath.Join(dir, "work", "retired", "build")
	writeAbs(t, filepath.Join(tree, "01 - So What.flac"), "audio")
	writeAbs(t, filepath.Join(tree, "Extras", "Scans", "front.jpg"), "img")
	mkdirAbs(t, filepath.Join(tree, "empty"))
	symlinkAbs(t, outside, filepath.Join(tree, "link-to-outside-dir"))
	symlinkAbs(t, filepath.Join(outside, "precious"), filepath.Join(tree, "Extras", "link-to-outside-file"))
	symlinkAbs(t, "..", filepath.Join(tree, "Extras", "link-to-parent"))
	symlinkAbs(t, "/", filepath.Join(tree, "link-to-fs-root"))
	mkfifoAbs(t, filepath.Join(tree, "fifo"))
	writeAbs(t, filepath.Join(dir, "work", "retired", "other", "f"), "stays")

	if err := r.RemoveAll(t.Context(), "work/retired/build"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the tree still exists: %v", err)
	}
	if got := readAbs(t, filepath.Join(outside, "precious")); got != "do not touch" {
		t.Fatalf("outside file altered: %q", got)
	}
	if got := readAbs(t, filepath.Join(outside, "sub", "nested")); got != "not this either" {
		t.Fatalf("nested outside file altered: %q", got)
	}
	if got := readAbs(t, filepath.Join(dir, "work", "retired", "other", "f")); got != "stays" {
		t.Fatalf("sibling directory altered: %q", got)
	}

	// Absence is not an error, not even of the parent; a single file is
	// removed.
	if err := r.RemoveAll(t.Context(), "work/retired/build"); err != nil {
		t.Fatalf("RemoveAll of a missing path: %v", err)
	}
	if err := r.RemoveAll(t.Context(), "work/missing/deeper/still"); err != nil {
		t.Fatalf("RemoveAll with a missing parent: %v", err)
	}
	if err := r.RemoveAll(t.Context(), "work/retired/other/f"); err != nil {
		t.Fatal(err)
	}
	// A symlink as the root of the removal is deleted, not followed.
	symlinkAbs(t, outside, filepath.Join(dir, "work", "link-to-outside"))
	if err := r.RemoveAll(t.Context(), "work/link-to-outside"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Fatalf("the link target was removed: %v", err)
	}
	// The parent must be a directory.
	writeAbs(t, filepath.Join(dir, "plain"), "x")
	wantCode(t, r.RemoveAll(t.Context(), "plain/x"), CodeNotDirectory)
}

// removeEntry trusts the kernel over the listed type: an entry listed as a
// file that is now a directory, and the reverse, are both removed.
func TestRemoveEntryHandlesReplacedType(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "was-file", "inner"), "x")
	writeAbs(t, filepath.Join(dir, "was-dir"), "x")
	symlinkAbs(t, filepath.Dir(dir), filepath.Join(dir, "was-dir-now-link"))

	fd, err := r.openDir("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeFD(r.name, "", fd); err != nil {
			t.Error(err)
		}
	}()
	for _, tc := range []struct {
		name  string
		isDir bool
	}{{"was-file", false}, {"was-dir", true}, {"was-dir-now-link", true}} {
		if err := r.removeEntry(t.Context(), fd, tc.name, tc.name, tc.isDir, 0); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, tc.name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists", tc.name)
		}
	}
	if _, err := os.Stat(filepath.Dir(dir)); err != nil {
		t.Fatalf("the link target was removed: %v", err)
	}
}

func TestRemoveAllCancelledContext(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "d", "f"), "x")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.RemoveAll(ctx, "d"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d", "f")); err != nil {
		t.Fatalf("removed despite the cancelled context: %v", err)
	}
}

func TestRemoveAllMaxDepth(t *testing.T) {
	r, dir := newRoot(t)
	mkdirAbs(t, filepath.Join(dir, "deep", strings.Repeat("x/", maxRemoveAllDepth+2)))
	wantCode(t, r.RemoveAll(t.Context(), "deep"), CodeTooDeep)

	mkdirAbs(t, filepath.Join(dir, "shallow", strings.Repeat("x/", maxRemoveAllDepth-1)))
	if err := r.RemoveAll(t.Context(), "shallow"); err != nil {
		t.Fatalf("a tree within the limit: %v", err)
	}
}
