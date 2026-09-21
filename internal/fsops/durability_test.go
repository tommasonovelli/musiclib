package fsops

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"musiclib/internal/names"
)

func TestSyncDirAndParents(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "work", "render", "b1", "album", "Disc 1", "01.flac"), "x")
	writeAbs(t, filepath.Join(dir, "file"), "x")

	if err := r.SyncDir(""); err != nil {
		t.Fatalf("SyncDir of the root: %v", err)
	}
	if err := r.SyncDir("work/render"); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncDirAndParents("work/render/b1/album/Disc 1"); err != nil {
		t.Fatal(err)
	}
	if err := r.SyncDirAndParents(""); err != nil {
		t.Fatal(err)
	}
	wantCode(t, r.SyncDir("missing"), CodeNotFound)
	wantCode(t, r.SyncDir("file"), CodeNotDirectory)
	wantCode(t, r.SyncDirAndParents("work/missing/x"), CodeNotFound)
	wantCode(t, r.SyncDirAndParents("file"), CodeNotDirectory)
	wantCode(t, r.SyncDirAndParents("../x"), names.CodePathDotSegment)
}

// A failed fsync is not ignored: SyncFile and SyncAndClose return the error,
// and SyncAndClose also returns the close error.
func TestFsyncAndCloseErrorsNotIgnored(t *testing.T) {
	r, _ := newRoot(t)
	f, err := r.CreateExclusive("f", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "data"); err != nil {
		t.Fatal(err)
	}
	if err := SyncFile(f); err != nil {
		t.Fatalf("SyncFile on an open file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Descriptor already closed: fsync and close both fail.
	err = SyncFile(f)
	if err == nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("SyncFile on a closed file = %v", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Op != "fsync" || e.Code == "" {
		t.Fatalf("untyped fsync error: %#v", err)
	}
	err = SyncAndClose(f)
	if err == nil {
		t.Fatal("SyncAndClose ignored the errors")
	}
	msg := err.Error()
	if !strings.Contains(msg, "(fsync)") || !strings.Contains(msg, "(close)") {
		t.Fatalf("SyncAndClose must report both fsync and close: %v", err)
	}
}

// fsync of a pipe fails with EINVAL on Linux: a real kernel error, on an
// open descriptor, comes back typed.
func TestSyncFileKernelError(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer mustClose(t, pr)
	err = SyncAndClose(pw)
	wantCode(t, err, CodeInvalidArgument)
	if err := pw.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("SyncAndClose did not close after the fsync failure: %v", err)
	}
}

// SyncAndClose closes the descriptor even when the fsync succeeds.
func TestSyncAndCloseAlwaysCloses(t *testing.T) {
	r, _ := newRoot(t)
	f, err := r.CreateExclusive("f", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := SyncAndClose(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("the file had not been closed: %v", err)
	}
}
