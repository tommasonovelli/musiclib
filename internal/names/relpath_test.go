package names

import (
	"strings"
	"testing"
)

func TestSplitRelPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"single", "album", []string{"album"}},
		{"nested", "Miles Davis/Kind of Blue/01.flac", []string{"Miles Davis", "Kind of Blue", "01.flac"}},
		{"exotic name kept", "AC_DC/  spaces  /CON", []string{"AC_DC", "  spaces  ", "CON"}},
		{"backslash is an ordinary character", `a\b`, []string{`a\b`}},
		{"dot inside the name", "..hidden", []string{"..hidden"}},
		{"name starting with a dot", ".DS_Store", []string{".DS_Store"}},
		{"exactly 16 levels", strings.Repeat("a/", MaxPathDepth-1) + "a", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitRelPath(tc.in)
			if err != nil {
				t.Fatalf("SplitRelPath(%q) = error %v", tc.in, err)
			}
			if tc.want == nil {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("SplitRelPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("SplitRelPath(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestSplitRelPathErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		code string
	}{
		{"empty", "", CodePathEmpty},
		{"absolute", "/etc/passwd", CodePathAbsolute},
		{"root", "/", CodePathAbsolute},
		{"dot", ".", CodePathDotSegment},
		{"dot dot", "..", CodePathDotSegment},
		{"traversal", "a/../../etc", CodePathDotSegment},
		{"trailing traversal", "a/..", CodePathDotSegment},
		{"inner dot", "a/./b", CodePathDotSegment},
		{"empty segment", "a//b", CodePathEmptySegment},
		{"trailing slash", "a/", CodePathEmptySegment},
		{"invalid utf8", "a/\xff/b", CodeInvalidUTF8},
		{"NUL byte", "a\x00b", CodePathNulByte},
		{"too deep", strings.Repeat("a/", MaxPathDepth) + "a", CodePathTooDeep},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SplitRelPath(tc.in)
			if err == nil {
				t.Fatalf("SplitRelPath(%q): want error %s", tc.in, tc.code)
			}
			if got := Code(err); got != tc.code {
				t.Errorf("SplitRelPath(%q): code %q, want %q", tc.in, got, tc.code)
			}
		})
	}
}

func TestSplitRelPathOrRoot(t *testing.T) {
	got, err := SplitRelPathOrRoot("")
	if err != nil {
		t.Fatalf("the empty string must denote the root: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("SplitRelPathOrRoot(\"\") = %q, want zero segments", got)
	}
	// The other rules stay the same.
	if Code(mustErrSlice(SplitRelPathOrRoot("../x"))) != CodePathDotSegment {
		t.Error("SplitRelPathOrRoot must reject traversal")
	}
}

func TestSanitizeRelFilePath(t *testing.T) {
	got, err := SanitizeRelFilePath("Scans/Front Cover.JPG")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Path != "Scans/Front Cover.JPG" {
		t.Errorf("Path = %q", got.Path)
	}
	if got.Key != "scans/front cover.jpg" {
		t.Errorf("Key = %q", got.Key)
	}

	// Every segment is sanitized; only the last one keeps its extension.
	got, err = SanitizeRelFilePath("CON/a:b/" + strings.Repeat("x", 200) + ".pdf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Segments[0] != "_CON" {
		t.Errorf("segment 0 = %q, want _CON", got.Segments[0])
	}
	if got.Segments[1] != "a_b" {
		t.Errorf("segment 1 = %q, want a_b", got.Segments[1])
	}
	if !strings.HasSuffix(got.Segments[2], ".pdf") || len(got.Segments[2]) != MaxSegmentBytes {
		t.Errorf("segment 2 = %q (len %d)", got.Segments[2], len(got.Segments[2]))
	}
}

func TestSanitizeRelFilePathLimits(t *testing.T) {
	// 16 segments of 180 bytes exceed the 1,024-byte path limit.
	long := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 180)+"/", MaxPathDepth), "/")
	_, err := SanitizeRelFilePath(long)
	if got := Code(err); got != CodePathTooLong {
		t.Errorf("code %q, want %q", got, CodePathTooLong)
	}

	// Depth is checked before sanitization.
	_, err = SanitizeRelFilePath(strings.Repeat("a/", MaxPathDepth) + "a")
	if got := Code(err); got != CodePathTooDeep {
		t.Errorf("code %q, want %q", got, CodePathTooDeep)
	}

	// A long path within both limits passes: 16 levels and exactly
	// 1,024 bytes after the transformation (15 separators included).
	ok := strings.Repeat(strings.Repeat("a", 63)+"/", MaxPathDepth-1) + strings.Repeat("a", 64)
	got, err := SanitizeRelFilePath(ok)
	if err != nil {
		t.Fatalf("path at the limit rejected: %v", err)
	}
	if len(got.Path) != MaxPathBytes {
		t.Errorf("length %d, want %d", len(got.Path), MaxPathBytes)
	}

	// One more byte is rejected.
	if _, err := SanitizeRelFilePath(ok + "a"); Code(err) != CodePathTooLong {
		t.Errorf("code %q, want %q", Code(err), CodePathTooLong)
	}
}

func TestSanitizeRelFilePathDoesNotEscapeRoot(t *testing.T) {
	// Sanitization is not confinement (DESIGN.md §10.4), but it must still
	// be unable to produce segments that climb up the tree.
	for _, in := range []string{"..", "a/../b", "/abs", "a/./b"} {
		if _, err := SanitizeRelFilePath(in); err == nil {
			t.Errorf("SanitizeRelFilePath(%q) accepted", in)
		}
	}
	// A name that contains dots but is not a special segment stays valid
	// and introduces no separators.
	got, err := SanitizeRelFilePath("..a/b..c")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Segments[0] != "a" || got.Segments[1] != "b..c" {
		t.Errorf("segments = %q", got.Segments)
	}
	for _, s := range got.Segments {
		if strings.Contains(s, "/") || s == "." || s == ".." {
			t.Errorf("dangerous segment: %q", s)
		}
	}
}

func mustErrSlice(_ []string, err error) error { return err }
