package names

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func normNFC(s string) string { return norm.NFC.String(s) }

// seeds covers the cases used by the table-driven tests and also serves as
// the initial fuzzing corpus.
var seeds = []string{
	"", " ", ".", "..", "...", "_", "~",
	"Kind of Blue", "AC/DC", "CON", "con.txt", "COM1", "LPT9", "NUL.",
	"Björk", "Björk", "straße", "STRASSE", "İstanbul",
	"東京", "Album 🎵", "a\xffb", "a\x00b", "a\x07b",
	`a/b\c:d*e?f"g<h>i|j`,
	strings.Repeat("a", 179), strings.Repeat("a", 180), strings.Repeat("a", 181),
	strings.Repeat("a", 250) + ".flac",
	strings.Repeat("é", 200), strings.Repeat("🎵", 100),
	"." + strings.Repeat("a", 300) + ".",
}

// checkSegment verifies every invariant that the rest of the system takes
// for granted about a final segment (DESIGN.md §5.1, §5.2).
func checkSegment(t *testing.T, in, got string) {
	t.Helper()
	if got == "" {
		t.Fatalf("empty segment from %q", in)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8 from %q: %q", in, got)
	}
	if len(got) > MaxSegmentBytes {
		t.Fatalf("%q -> %q: %d bytes, limit %d", in, got, len(got), MaxSegmentBytes)
	}
	if norm := normNFC(got); norm != got {
		t.Fatalf("%q -> %q: not in NFC", in, got)
	}
	for _, r := range got {
		if strings.ContainsRune(forbiddenSegmentRunes, r) {
			t.Fatalf("%q -> %q: contains forbidden character %q", in, got, r)
		}
		if unicode.IsControl(r) {
			t.Fatalf("%q -> %q: contains control character U+%04X", in, got, r)
		}
	}
	first, _ := utf8.DecodeRuneInString(got)
	last, _ := utf8.DecodeLastRuneInString(got)
	for _, r := range []rune{first, last} {
		if unicode.IsSpace(r) || r == '.' {
			t.Fatalf("%q -> %q: outer space or dot", in, got)
		}
	}
	if isReservedDOS(got) {
		t.Fatalf("%q -> %q: reserved DOS name", in, got)
	}
}

func FuzzSegment(f *testing.F) {
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := Segment(in)
		checkSegment(t, in, got)
		if again := Segment(got); again != got {
			t.Fatalf("Segment not idempotent: %q -> %q -> %q", in, got, again)
		}

		gotFile := FileSegment(in)
		checkSegment(t, in, gotFile)
		if again := FileSegment(gotFile); again != gotFile {
			t.Fatalf("FileSegment not idempotent: %q -> %q -> %q", in, gotFile, again)
		}

		// Determinism: no state shared between calls.
		if Segment(in) != got || FileSegment(in) != gotFile {
			t.Fatalf("non-deterministic result for %q", in)
		}
	})
}

func FuzzKey(f *testing.F) {
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		seg := Segment(in)
		k := Key(seg)
		if !utf8.ValidString(k) {
			t.Fatalf("key is not valid UTF-8 from %q: %q", in, k)
		}
		if strings.ContainsRune(k, '/') {
			t.Fatalf("the key of a segment cannot contain '/': %q", k)
		}
		if again := Key(k); again != k {
			t.Fatalf("Key not idempotent: %q -> %q -> %q", seg, k, again)
		}
		// The key is case-insensitive: this is the property that
		// folder_key and path_claims rely on.
		//
		// The comparison uses a case swap limited to ASCII: the
		// strings.ToUpper/ToLower functions apply simple case mapping and
		// are not invertible over all of Unicode (for example U+0130 loses
		// its dot in ToLower), so they are not a valid oracle for full
		// case folding.
		if Key(swapASCIICase(seg)) != Key(seg) {
			t.Fatalf("Key distinguishes the ASCII case of %q", seg)
		}
	})
}

func FuzzNormalizeText(f *testing.F) {
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got, err := NormalizeText(in)
		if err != nil {
			if Code(err) == "" {
				t.Fatalf("error without a code: %v", err)
			}
			return
		}
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8 from %q", in)
		}
		if normNFC(got) != got {
			t.Fatalf("%q -> %q: not in NFC", in, got)
		}
		if utf8.RuneCountInString(got) > MaxTextRunes {
			t.Fatalf("%q -> %d characters", in, utf8.RuneCountInString(got))
		}
		for _, r := range got {
			if unicode.IsControl(r) {
				t.Fatalf("%q -> %q: leftover control character", in, got)
			}
		}
		if strings.TrimFunc(got, unicode.IsSpace) != got {
			t.Fatalf("%q -> %q: leftover outer space", in, got)
		}
		again, err := NormalizeText(got)
		if err != nil || again != got {
			t.Fatalf("NormalizeText not idempotent: %q -> %q -> %q (%v)", in, got, again, err)
		}
	})
}

func FuzzSplitRelPath(f *testing.F) {
	for _, s := range seeds {
		f.Add(s)
		f.Add("a/" + s)
		f.Add(s + "/b")
	}
	f.Fuzz(func(t *testing.T, in string) {
		segs, err := SplitRelPath(in)
		if err != nil {
			if Code(err) == "" {
				t.Fatalf("error without a code: %v", err)
			}
			return
		}
		if len(segs) == 0 || len(segs) > MaxPathDepth {
			t.Fatalf("%q -> %d segments", in, len(segs))
		}
		for _, s := range segs {
			if s == "" || s == "." || s == ".." || strings.ContainsRune(s, '/') {
				t.Fatalf("%q -> segment %q", in, s)
			}
		}
		// The segments stay exactly those on disk.
		if strings.Join(segs, "/") != in {
			t.Fatalf("%q rejoined as %q", in, strings.Join(segs, "/"))
		}

		sp, err := SanitizeRelFilePath(in)
		if err != nil {
			if Code(err) != CodePathTooLong {
				t.Fatalf("%q -> unexpected error %v", in, err)
			}
			return
		}
		if len(sp.Segments) != len(segs) {
			t.Fatalf("%q: %d sanitized segments, want %d", in, len(sp.Segments), len(segs))
		}
		for _, s := range sp.Segments {
			checkSegment(t, in, s)
		}
		if len(sp.Path) > MaxPathBytes {
			t.Fatalf("%q -> path of %d bytes", in, len(sp.Path))
		}
		if sp.Path != strings.Join(sp.Segments, "/") {
			t.Fatalf("Path inconsistent with Segments for %q", in)
		}
		if sp.Key != PathKey(sp.Segments) {
			t.Fatalf("Key inconsistent with Segments for %q", in)
		}
	})
}

// swapASCIICase swaps the case of ASCII letters only, leaving everything
// else intact: it is an invertible transformation, unlike
// strings.ToUpper/ToLower on Unicode.
func swapASCIICase(s string) string {
	b := []byte(s)
	for i := range b {
		switch {
		case b[i] >= 'a' && b[i] <= 'z':
			b[i] -= 'a' - 'A'
		case b[i] >= 'A' && b[i] <= 'Z':
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
