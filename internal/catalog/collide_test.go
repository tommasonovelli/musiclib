package catalog_test

import (
	"strings"
	"testing"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
)

// PathCollision is §5.2's rule for the catalog and the render planner: two
// files with one key, a file where another needs a directory, one directory
// spelled two ways (owner decision 2026-09-23, N-131).
func TestPathCollision(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		a, b  int
		kind  catalog.CollisionKind
	}{
		{"distinct", []string{"a.txt", "b/a.txt", "b/c/d.txt", "B.txt"}, 0, 0, catalog.NoCollision},
		{"one directory, one spelling", []string{"Scans/a.jpg", "Scans/b.jpg", "Scans/x/c.jpg"}, 0, 0, catalog.NoCollision},
		{"same file by case", []string{"x", "a.txt", "A.TXT"}, 1, 2, catalog.SameFile},
		{"same file by casefold", []string{"Straße.txt", "STRASSE.txt"}, 0, 1, catalog.SameFile},
		{"directories by case", []string{"Scans/a.jpg", "x", "scans/b.jpg"}, 0, 2, catalog.DirectorySpelling},
		{"nested directories by case", []string{"X/Scans/a", "X/scans/b"}, 0, 1, catalog.DirectorySpelling},
		{"outer directories by case", []string{"x/a/1", "X/a/2"}, 0, 1, catalog.DirectorySpelling},
		{"a file, then its directory", []string{"notes", "Notes/x.txt"}, 0, 1, catalog.FileIsDirectory},
		{"a directory, then its file", []string{"a/b/c.txt", "a/B"}, 1, 0, catalog.FileIsDirectory},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b, kind := catalog.PathCollision(tc.paths)
			if kind != tc.kind || kind != catalog.NoCollision && (a != tc.a || b != tc.b) {
				t.Fatalf("PathCollision = %d, %d, %v; want %d, %d, %v", a, b, kind, tc.a, tc.b, tc.kind)
			}
		})
	}
}

// The import commit refuses directories that differ only in case, naming
// both attachments.
func TestCommitImportDirectoriesDifferingInCase(t *testing.T) {
	e := newEnv(t)
	c := candidate(e.runningImport(), "Case", "Scans")
	c.Attachments = append(c.Attachments, catalog.ImportAttachment{RelPath: "scans/b.jpg", BlobHash: c.Attachments[0].BlobHash})
	out := e.commit(c)
	if out.State != jobs.StateFailed || out.ErrorCode != catalog.CodeAttachmentCollision ||
		!strings.Contains(out.ErrorMessage, `"Scans/Booklet.pdf"`) || !strings.Contains(out.ErrorMessage, `"scans/b.jpg"`) {
		t.Fatalf("outcome %+v", out)
	}
}
