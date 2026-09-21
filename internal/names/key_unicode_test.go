package names

import (
	"testing"
	"unicode"
)

// TestKeyCoversAllOfUnicode exhaustively checks, code point by code point,
// the two properties that folder_key, path_key and path_claims rely on
// (DESIGN.md §5.2, §5.3):
//
//  1. Key is idempotent, so there is a single canonical form;
//  2. code points that are equivalent up to case have the same key.
//
// Full enumeration is preferable to fuzzing for this property: the space is
// small and the algorithm is frozen in v1.
func TestKeyCoversAllOfUnicode(t *testing.T) {
	if testing.Short() {
		t.Skip("enumeration of all code points")
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // surrogates: not valid UTF-8
		}
		s := string(r)
		k := Key(s)
		if again := Key(k); again != k {
			t.Fatalf("Key not idempotent on U+%04X: %q -> %q -> %q", r, s, k, again)
		}
		// unicode.SimpleFold walks the case orbit of r.
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if fk := Key(string(f)); fk != k {
				t.Fatalf("U+%04X and U+%04X are equivalent up to case but have different keys: %q vs %q",
					r, f, k, fk)
			}
		}
	}
}

// TestKeyCherokee pins the correct behavior on Cherokee letters, the case in
// which cases.Fold() from golang.org/x/text does not match Unicode full case
// folding.
func TestKeyCherokee(t *testing.T) {
	tests := []struct {
		upper, lower string
		wantKey      rune
	}{
		{"Ꮸ", "ꮸ", 0x13E8},
		{"Ꭰ", "ꭰ", 0x13A0},
		{"Ꮿ", "ꮿ", 0x13EF},
		{"Ᏸ", "ᏸ", 0x13F0},
		{"Ᏽ", "ᏽ", 0x13F5},
	}
	for _, tc := range tests {
		want := string(tc.wantKey)
		if got := Key(tc.upper); got != want {
			t.Errorf("Key(%q) = %q (U+%04X), want U+%04X", tc.upper, got, []rune(got)[0], tc.wantKey)
		}
		if got := Key(tc.lower); got != want {
			t.Errorf("Key(%q) = %q (U+%04X), want U+%04X", tc.lower, got, []rune(got)[0], tc.wantKey)
		}
	}
	// The real-world case: two albums whose names differ only in case must
	// occupy the same path_claims row.
	if FolderKey("Ꮸsa") != FolderKey("ꮸsa") {
		t.Error("two case variants of the same name have different folder_key values")
	}
}
