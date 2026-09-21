package names

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unchanged", "Miles Davis", "Miles Davis"},
		{"outer trim", "  Kind of Blue\t", "Kind of Blue"},
		{"inner spaces kept", "So  What", "So  What"},
		{"nfc composes", "Björk", "Björk"},
		{"nfc leaves already composed text alone", "Björk", "Björk"},
		{"outer newline trimmed", "A\n", "A"},
		{"empty allowed", "", ""},
		{"only spaces becomes empty", "   \t\n ", ""},
		{"ampersand and feat stay text", "A feat. B & C", "A feat. B & C"},
		{"exactly 1024 characters", strings.Repeat("x", MaxTextRunes), strings.Repeat("x", MaxTextRunes)},
		{"the limit is in characters not bytes", strings.Repeat("é", MaxTextRunes), strings.Repeat("é", MaxTextRunes)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeText(tc.in)
			if err != nil {
				t.Fatalf("NormalizeText(%q) = error %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeTextErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		code string
	}{
		{"invalid utf8", "ab\xffcd", CodeInvalidUTF8},
		{"inner control character", "A\x07B", CodeTextControlChar},
		{"inner newline", "A\nB", CodeTextControlChar},
		{"NUL", "A\x00B", CodeTextControlChar},
		{"C1", "AB", CodeTextControlChar},
		{"too long", strings.Repeat("x", MaxTextRunes+1), CodeTextTooLong},
		{"too long in multibyte characters", strings.Repeat("é", MaxTextRunes+1), CodeTextTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeText(tc.in)
			if err == nil {
				t.Fatalf("NormalizeText(%q): want error %s", tc.in, tc.code)
			}
			if got := Code(err); got != tc.code {
				t.Errorf("NormalizeText(%q): code %q, want %q", tc.in, got, tc.code)
			}
		})
	}
}

func TestNormalizeRequiredText(t *testing.T) {
	if _, err := NormalizeRequiredText("Miles Davis"); err != nil {
		t.Fatalf("valid value rejected: %v", err)
	}
	for _, in := range []string{"", "   ", "\t\n"} {
		_, err := NormalizeRequiredText(in)
		if Code(err) != CodeTextEmpty {
			t.Errorf("NormalizeRequiredText(%q): code %q, want %q", in, Code(err), CodeTextEmpty)
		}
	}
	// A form error takes precedence over emptiness.
	if got := Code(mustErr(NormalizeRequiredText("\x07"))); got != CodeTextControlChar {
		t.Errorf("code %q, want %q", got, CodeTextControlChar)
	}
}

func TestSegment(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unchanged", "Kind of Blue", "Kind of Blue"},
		{"slash", "AC/DC", "AC_DC"},
		{"all forbidden", `a/b\c:d*e?f"g<h>i|j`, "a_b_c_d_e_f_g_h_i_j"},
		{"colon", "Vol: 2", "Vol_ 2"},
		{"trailing dot", "Album.", "Album"},
		{"multiple trailing dots", "Album...", "Album"},
		{"leading dot", ".hidden", "hidden"},
		{"outer spaces", "  Album  ", "Album"},
		{"mixed space and dot", " . Album . ", "Album"},
		{"inner extension kept", "rip.log", "rip.log"},
		{"empty", "", "_"},
		{"only dots", "...", "_"},
		{"only forbidden", "///", "___"},
		{"only spaces", "   ", "_"},
		{"nfc", "Björk", "Björk"},
		{"control replaced", "A\x07B", "A_B"},
		{"tab replaced", "A\tB", "A_B"},
		{"invalid utf8 replaced", "a\xffb", "a_b"},
		{"unicode kept", "東京", "東京"},
		{"emoji kept", "Album 🎵", "Album 🎵"},
		{"tilde kept", "A~B", "A~B"},
		{"underscore kept", "A_B", "A_B"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Segment(tc.in); got != tc.want {
				t.Errorf("Segment(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSegmentReservedDOSNames(t *testing.T) {
	reserved := []string{
		"CON", "NUL", "PRN", "AUX",
		"COM1", "COM9", "LPT1", "LPT9",
		"con", "Nul", "cOm3",
		"CON.txt", "nul.flac", "LPT4.tar.gz",
		"CON.", // the trailing dot is trimmed before the check
	}
	for _, in := range reserved {
		t.Run(in, func(t *testing.T) {
			got := Segment(in)
			if !strings.HasPrefix(got, "_") {
				t.Errorf("Segment(%q) = %q, want a _ prefix", in, got)
			}
			if isReservedDOS(got) {
				t.Errorf("Segment(%q) = %q, still reserved", in, got)
			}
		})
	}

	notReserved := []string{
		"COM0", "LPT0", "COM10", "CONS", "CONSOLE", "NULL",
		"AUXILIARY", "PRNT", "_CON", "CO", "COM", "LPT",
		"my CON", "CON files",
	}
	for _, in := range notReserved {
		t.Run("ok/"+in, func(t *testing.T) {
			if got := Segment(in); got != in {
				t.Errorf("Segment(%q) = %q, want it unchanged", in, got)
			}
		})
	}
}

func TestSegmentTruncation(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := Segment(long)
	if len(got) != MaxSegmentBytes {
		t.Fatalf("length %d, want %d", len(got), MaxSegmentBytes)
	}
	sum := sha256.Sum256([]byte(long))
	wantSuffix := "~" + hex.EncodeToString(sum[:])[:hashSuffixHexLen]
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("%q does not end with %q", got, wantSuffix)
	}
	if !strings.HasPrefix(got, strings.Repeat("a", MaxSegmentBytes-len(wantSuffix))) {
		t.Errorf("unexpected truncated stem: %q", got)
	}
}

func TestSegmentTruncationBoundaries(t *testing.T) {
	// Exactly 180 bytes: no truncation.
	exact := strings.Repeat("a", MaxSegmentBytes)
	if got := Segment(exact); got != exact {
		t.Errorf("a segment of %d bytes was truncated: %q", MaxSegmentBytes, got)
	}
	// 181 bytes: truncation.
	over := strings.Repeat("a", MaxSegmentBytes+1)
	if got := Segment(over); len(got) != MaxSegmentBytes || !strings.Contains(got, "~") {
		t.Errorf("Segment(181 bytes) = %q (len %d)", got, len(got))
	}
}

func TestSegmentTruncationOnUTF8Boundary(t *testing.T) {
	// "é" takes 2 bytes: a naive cut at 171 bytes would split a rune.
	in := strings.Repeat("é", 100)
	got := Segment(in)
	if !utf8.ValidString(got) {
		t.Fatalf("the result is not valid UTF-8: %q", got)
	}
	if len(got) > MaxSegmentBytes {
		t.Fatalf("length %d over the limit", len(got))
	}
	// The stem budget is 180-9 = 171 bytes, that is 85 "é" characters (170 bytes).
	stem := got[:len(got)-hashSuffixHexLen-1]
	if stem != strings.Repeat("é", 85) {
		t.Errorf("stem = %q", stem)
	}

	// 4-byte runes.
	in4 := strings.Repeat("🎵", 60)
	got4 := Segment(in4)
	if !utf8.ValidString(got4) || len(got4) > MaxSegmentBytes {
		t.Fatalf("Segment(4-byte runes) = %q (len %d)", got4, len(got4))
	}
}

func TestSegmentTruncationDeterministicAndDistinct(t *testing.T) {
	a := strings.Repeat("a", 250)
	b := strings.Repeat("a", 250) + "b"
	if Segment(a) != Segment(a) {
		t.Error("truncation is not deterministic")
	}
	// The first 180 bytes are equal: only the hash suffix tells the two names apart.
	if Segment(a) == Segment(b) {
		t.Error("two different segments with the same prefix produced the same name")
	}
}

func TestFileSegmentPreservesExtension(t *testing.T) {
	in := strings.Repeat("a", 200) + ".flac"
	got := FileSegment(in)
	if len(got) != MaxSegmentBytes {
		t.Fatalf("length %d, want %d", len(got), MaxSegmentBytes)
	}
	if !strings.HasSuffix(got, ".flac") {
		t.Errorf("FileSegment lost the extension: %q", got)
	}
	if !strings.Contains(got, "~") {
		t.Errorf("hash suffix missing: %q", got)
	}
	// Segment (directories) preserves nothing: it just truncates.
	if strings.HasSuffix(Segment(in), ".flac") {
		t.Errorf("Segment must not preserve the extension: %q", Segment(in))
	}
}

func TestFileSegmentOversizedExtension(t *testing.T) {
	// An extension that leaves no room for the stem is dropped, never
	// truncated beyond the limit.
	in := "a" + strings.Repeat("b", 250) + "." + strings.Repeat("c", 200)
	got := FileSegment(in)
	if len(got) > MaxSegmentBytes {
		t.Fatalf("length %d over the limit: %q", len(got), got)
	}
	if !strings.Contains(got, "~") {
		t.Errorf("hash suffix missing: %q", got)
	}
}

func TestFileSegmentWithoutExtension(t *testing.T) {
	in := strings.Repeat("a", 200)
	if got := FileSegment(in); got != Segment(in) {
		t.Errorf("without an extension FileSegment and Segment must agree: %q vs %q", got, Segment(in))
	}
}

func TestSegmentIdempotent(t *testing.T) {
	ins := []string{
		"AC/DC", "  Album. ", "CON", "...", "",
		strings.Repeat("a", 250), strings.Repeat("é", 200),
		strings.Repeat("a", 250) + ".flac", "a\xffb", "🎵" + strings.Repeat("x", 300),
	}
	for _, in := range ins {
		once := Segment(in)
		if twice := Segment(once); twice != once {
			t.Errorf("Segment not idempotent on %q: %q -> %q", in, once, twice)
		}
		onceF := FileSegment(in)
		if twiceF := FileSegment(onceF); twiceF != onceF {
			t.Errorf("FileSegment not idempotent on %q: %q -> %q", in, onceF, twiceF)
		}
	}
}

func TestKey(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		same bool
	}{
		{"uppercase", "Abba", "ABBA", true},
		{"mixed case", "Kind of Blue", "KIND OF BLUE", true},
		{"eszett folds to ss", "straße", "STRASSE", true},
		{"final sigma", "ΟΔΥΣΣΕΥΣ", "οδυσσευς", true},
		{"nfc", "Björk", "Björk", true},
		{"different accents stay different", "Bjork", "Björk", false},
		{"different names", "Abba", "Abbas", false},
		{"significant space", "AB BA", "ABBA", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Key(tc.a) == Key(tc.b); got != tc.same {
				t.Errorf("Key(%q)=%q, Key(%q)=%q: equal=%v, want %v",
					tc.a, Key(tc.a), tc.b, Key(tc.b), got, tc.same)
			}
		})
	}
}

func TestKeyIdempotent(t *testing.T) {
	for _, in := range []string{"Abba", "STRASSE", "straße", "ΟΔΥΣΣΕΥΣ", "Björk", "東京", "İstanbul"} {
		once := Key(in)
		if twice := Key(once); twice != once {
			t.Errorf("Key not idempotent on %q: %q -> %q", in, once, twice)
		}
	}
}

func TestFolderKey(t *testing.T) {
	// The key is computed on the final segment, that is after sanitization.
	if FolderKey("AC/DC") != FolderKey("ac_dc") {
		t.Errorf("FolderKey(%q)=%q, FolderKey(%q)=%q", "AC/DC", FolderKey("AC/DC"), "ac_dc", FolderKey("ac_dc"))
	}
	if FolderKey("Abba") != FolderKey("  ABBA  ") {
		t.Error("trim and case must collapse onto the same key")
	}
	if FolderKey("Kind of Blue") == FolderKey("Kind of Blue (Mono)") {
		t.Error("different editions must have different keys")
	}
}

func TestPathKey(t *testing.T) {
	got := PathKey([]string{"Extras", "Scans", "Front.JPG"})
	want := "extras/scans/front.jpg"
	if got != want {
		t.Errorf("PathKey = %q, want %q", got, want)
	}
	if PathKey(nil) != "" {
		t.Errorf("PathKey(nil) = %q, want empty", PathKey(nil))
	}
	if PathKey([]string{"a"}) != "a" {
		t.Errorf("single-segment PathKey = %q", PathKey([]string{"a"}))
	}
}

func mustErr(_ string, err error) error { return err }

func TestErrorAndCodeDecoding(t *testing.T) {
	err := &Error{Code: CodeTextEmpty, Message: "empty"}
	if got, want := err.Error(), "text_empty: empty"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if Code(nil) != "" {
		t.Errorf("Code(nil) = %q, want empty", Code(nil))
	}
	if Code(errors.New("other")) != "" {
		t.Errorf("Code on a foreign error = %q, want empty", Code(errors.New("other")))
	}
	if got := Code(fmt.Errorf("context: %w", err)); got != CodeTextEmpty {
		t.Errorf("Code on a wrapped error = %q, want %q", got, CodeTextEmpty)
	}
}

func TestFileSegmentUnrepresentableStem(t *testing.T) {
	// A 1-byte stem budget with a 4-byte first rune: the stem disappears
	// entirely, but the name stays valid, within the limit and distinct
	// thanks to the hash suffix.
	ext := "." + strings.Repeat("c", 169) // 170 bytes: budget = 180-9-170 = 1
	in := "\U0001F3B5" + strings.Repeat("b", 100) + ext
	got := FileSegment(in)
	if len(got) > MaxSegmentBytes {
		t.Fatalf("length %d over the limit: %q", len(got), got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "~") {
		t.Errorf("want an empty stem: %q", got)
	}
	if !strings.HasSuffix(got, ext) {
		t.Errorf("extension lost: %q", got)
	}
	// Two different inputs with the same truncation stay distinct.
	other := "\U0001F3B6" + strings.Repeat("b", 100) + ext
	if FileSegment(other) == got {
		t.Error("two different segments produced the same name")
	}
}
