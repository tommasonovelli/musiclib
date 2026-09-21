package fsops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenameNoReplace(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "work", "blobs", "x.tmp"), "new")
	mkdirAbs(t, filepath.Join(dir, "originals", "ab"))

	if err := RenameNoReplace(r, "work/blobs/x.tmp", r, "originals/ab/hash"); err != nil {
		t.Fatal(err)
	}
	if got := readAbs(t, filepath.Join(dir, "originals", "ab", "hash")); got != "new" {
		t.Fatalf("destination %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "work", "blobs", "x.tmp")); !os.IsNotExist(err) {
		t.Fatal("the source still exists")
	}
	wantCode(t, RenameNoReplace(r, "work/blobs/missing", r, "originals/ab/other"), CodeNotFound)
	wantCode(t, RenameNoReplace(r, "originals/ab/hash", r, "missing/hash"), CodeNotFound)
}

// RENAME_NOREPLACE does not touch an existing destination, whatever its
// type, and leaves the source in place.
func TestRenameNoReplaceExistingDestination(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "src"), "source")
	writeAbs(t, filepath.Join(dir, "dst-file"), "existing")
	writeAbs(t, filepath.Join(dir, "dst-dir", "inner"), "existing")
	symlinkAbs(t, "dst-file", filepath.Join(dir, "dst-link"))
	symlinkAbs(t, "missing", filepath.Join(dir, "dst-dangling"))
	mkfifoAbs(t, filepath.Join(dir, "dst-fifo"))
	mkdirAbs(t, filepath.Join(dir, "srcdir"))

	for _, dst := range []string{"dst-file", "dst-dir", "dst-link", "dst-dangling", "dst-fifo"} {
		wantCode(t, RenameNoReplace(r, "src", r, dst), CodeExists)
		wantCode(t, RenameNoReplace(r, "srcdir", r, dst), CodeExists)
	}
	if got := readAbs(t, filepath.Join(dir, "dst-file")); got != "existing" {
		t.Fatalf("destination altered: %q", got)
	}
	if got := readAbs(t, filepath.Join(dir, "dst-dir", "inner")); got != "existing" {
		t.Fatalf("destination altered: %q", got)
	}
	if target, err := os.Readlink(filepath.Join(dir, "dst-link")); err != nil || target != "dst-file" {
		t.Fatalf("destination symlink altered: %q, %v", target, err)
	}
	if got := readAbs(t, filepath.Join(dir, "src")); got != "source" {
		t.Fatalf("source altered: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "srcdir")); err != nil {
		t.Fatalf("source directory moved: %v", err)
	}
}

// renameat2 reports one errno for the pair: the error names both sides.
// Errors found before the syscall name the side they concern.
func TestRenameErrorLocations(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "src"), "x")
	writeAbs(t, filepath.Join(dir, "dst"), "y")

	var e *Error
	err := RenameNoReplace(r, "missing-src", r, "somewhere")
	wantCode(t, err, CodeNotFound)
	if !errors.As(err, &e) || e.Path != "missing-src" || e.DstPath != "" {
		t.Fatalf("missing source attributed to %+v", e)
	}
	err = RenameNoReplace(r, "src", r, "dst")
	wantCode(t, err, CodeExists)
	if !errors.As(err, &e) || e.Path != "src" || e.DstPath != "dst" || e.DstRoot != r.Name() {
		t.Fatalf("rename error locations %+v", e)
	}
	if !strings.Contains(err.Error(), "src -> ") || strings.Contains(err.Error(), dir) {
		t.Fatalf("rename error message: %v", err)
	}
	err = RenameExchange(r, "src", r, "missing-dst")
	wantCode(t, err, CodeNotFound)
	if !errors.As(err, &e) || e.Path != "missing-dst" {
		t.Fatalf("missing exchange destination attributed to %+v", e)
	}
}

// Moving a directory into its own subtree is EINVAL: an invalid call, not an
// unsupported filesystem.
func TestRenameIntoOwnSubtree(t *testing.T) {
	r, dir := newRoot(t)
	mkdirAbs(t, filepath.Join(dir, "a", "b"))
	wantCode(t, RenameNoReplace(r, "a", r, "a/b/c"), CodeInvalidArgument)
	wantCode(t, RenameExchange(r, "a", r, "a/b"), CodeInvalidArgument)
	if _, err := os.Stat(filepath.Join(dir, "a", "b")); err != nil {
		t.Fatal(err)
	}
}

func TestRenameExchangeDirectory(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "library", "Artist", "Album", "01.flac"), "old")
	writeAbs(t, filepath.Join(dir, "work", "render", "b1", "album", "01.flac"), "new")
	writeAbs(t, filepath.Join(dir, "work", "render", "b1", "album", ".musiclib.json"), "receipt")

	if err := RenameExchange(r, "library/Artist/Album", r, "work/render/b1/album"); err != nil {
		t.Fatal(err)
	}
	if got := readAbs(t, filepath.Join(dir, "library", "Artist", "Album", "01.flac")); got != "new" {
		t.Fatalf("library after the swap: %q", got)
	}
	if got := readAbs(t, filepath.Join(dir, "library", "Artist", "Album", ".musiclib.json")); got != "receipt" {
		t.Fatalf("receipt after the swap: %q", got)
	}
	if got := readAbs(t, filepath.Join(dir, "work", "render", "b1", "album", "01.flac")); got != "old" {
		t.Fatalf("staging after the swap: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "work", "render", "b1", "album", ".musiclib.json")); !os.IsNotExist(err) {
		t.Fatal("the old album contains the new receipt")
	}

	// Both entries must exist.
	wantCode(t, RenameExchange(r, "library/Artist/Album", r, "work/missing"), CodeNotFound)
	// Symlinks rejected on both sides.
	symlinkAbs(t, "Album", filepath.Join(dir, "library", "Artist", "link"))
	wantCode(t, RenameExchange(r, "library/Artist/link", r, "work/render/b1/album"), CodeSymlink)
	wantCode(t, RenameExchange(r, "work/render/b1/album", r, "library/Artist/link"), CodeSymlink)
	if got := readAbs(t, filepath.Join(dir, "library", "Artist", "Album", "01.flac")); got != "new" {
		t.Fatalf("rejected swap altered the library: %q", got)
	}
}

func TestRenameExchangeFiles(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "x"), "first")
	writeAbs(t, filepath.Join(dir, "d", "y"), "second")
	if err := RenameExchange(r, "x", r, "d/y"); err != nil {
		t.Fatal(err)
	}
	if readAbs(t, filepath.Join(dir, "x")) != "second" || readAbs(t, filepath.Join(dir, "d", "y")) != "first" {
		t.Fatal("RENAME_EXCHANGE did not swap the files")
	}
}

// Rename between two different Roots on the same filesystem: it is the move
// of the staging directory from work/ to library/ (§9.3).
func TestRenameAcrossRoots(t *testing.T) {
	data, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "work", "render", "b1", "album", "01.flac"), "new")
	writeAbs(t, filepath.Join(dir, "work", "render", "b2", "album", "01.flac"), "other")
	writeAbs(t, filepath.Join(dir, "library", "Artist", "Existing", "01.flac"), "old")

	work, err := data.SubRoot("work")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, work)
	lib, err := data.SubRoot("library")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, lib)

	if err := RenameNoReplace(work, "render/b1/album", lib, "Artist/New"); err != nil {
		t.Fatal(err)
	}
	if got := readAbs(t, filepath.Join(dir, "library", "Artist", "New", "01.flac")); got != "new" {
		t.Fatalf("installation: %q", got)
	}
	wantCode(t, RenameNoReplace(work, "render/b2/album", lib, "Artist/Existing"), CodeExists)
	if err := RenameExchange(lib, "Artist/Existing", work, "render/b2/album"); err != nil {
		t.Fatal(err)
	}
	if got := readAbs(t, filepath.Join(dir, "library", "Artist", "Existing", "01.flac")); got != "other" {
		t.Fatalf("swap across roots: %q", got)
	}
	if got := readAbs(t, filepath.Join(dir, "work", "render", "b2", "album", "01.flac")); got != "old" {
		t.Fatalf("swap across roots, old side: %q", got)
	}
	if err := work.Mkdir("retired", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(lib, "Artist/New", work, "retired/b1"); err != nil {
		t.Fatal(err)
	}
	if got := readAbs(t, filepath.Join(dir, "work", "retired", "b1", "01.flac")); got != "new" {
		t.Fatalf("retirement: %q", got)
	}
	// The sequence the caller must perform after renames across roots (§9.3).
	if err := work.SyncDirAndParents("retired"); err != nil {
		t.Fatal(err)
	}
	if err := lib.SyncDirAndParents("Artist"); err != nil {
		t.Fatal(err)
	}
}

func TestRenameAcrossFilesystems(t *testing.T) {
	r, dir := newRoot(t)
	otherDir := otherFilesystemDir(t, dir)
	other, err := OpenRoot(otherDir)
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, other)
	writeAbs(t, filepath.Join(dir, "f"), "x")

	wantCode(t, RenameNoReplace(r, "f", other, "f"), CodeCrossDevice)
	same, err := SameFilesystem(r, other)
	if err != nil {
		t.Fatal(err)
	}
	if same {
		t.Fatal("SameFilesystem = true on different filesystems")
	}
	wantCode(t, ProbeRenameExchange(r, other), CodeCrossDevice)
}

func TestSameFilesystem(t *testing.T) {
	data, dir := newRoot(t)
	mkdirAbs(t, filepath.Join(dir, "library"))
	mkdirAbs(t, filepath.Join(dir, "work"))
	lib, err := data.SubRoot("library")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, lib)
	work, err := data.SubRoot("work")
	if err != nil {
		t.Fatal(err)
	}

	same, err := SameFilesystem(lib, work)
	if err != nil || !same {
		t.Fatalf("SameFilesystem(library, work) = %v, %v", same, err)
	}
	if err := work.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = SameFilesystem(lib, work)
	wantCode(t, err, CodeRootClosed)
}

func TestProbeRenameExchange(t *testing.T) {
	data, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "library", "Artist", "Album", "01.flac"), "published")
	mkdirAbs(t, filepath.Join(dir, "work"))
	lib, err := data.SubRoot("library")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, lib)
	work, err := data.SubRoot("work")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, work)

	for range 3 {
		if err := ProbeRenameExchange(lib, work); err != nil {
			t.Fatalf("probe: %v", err)
		}
	}
	// The probe also works on a single root.
	if err := ProbeRenameExchange(work, work); err != nil {
		t.Fatalf("probe on the same root: %v", err)
	}
	assertNoProbeLeftovers(t, dir)
	if got := readAbs(t, filepath.Join(dir, "library", "Artist", "Album", "01.flac")); got != "published" {
		t.Fatalf("the probe altered the library: %q", got)
	}
}

// A probe that fails after creating its first directory removes it (N-015).
func TestProbeRenameExchangeCleansUpOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions: the failure cannot be provoked")
	}
	data, dir := newRoot(t)
	mkdirAbs(t, filepath.Join(dir, "library"))
	mkdirAbs(t, filepath.Join(dir, "work"))
	lib, err := data.SubRoot("library")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, lib)
	work, err := data.SubRoot("work")
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, work)
	if err := os.Chmod(filepath.Join(dir, "work"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Join(dir, "work"), 0o755); err != nil {
			t.Error(err)
		}
	})

	wantCode(t, ProbeRenameExchange(lib, work), CodePermission)
	assertNoProbeLeftovers(t, dir)
}

func assertNoProbeLeftovers(t *testing.T, dir string) {
	t.Helper()
	for _, sub := range []string{"library", "work"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), probeDirPrefix) {
				t.Fatalf("probe leftover in %s: %s", sub, e.Name())
			}
		}
	}
}
