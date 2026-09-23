// Package names holds the single implementation of the normalization of
// texts, path segments and comparison keys (DESIGN.md §5.2).
//
// The package is pure: it does no I/O, knows nothing about the database and
// knows nothing about absolute paths. The folder_key and path_key columns,
// the path_claims reservations and the names produced by the planner are all
// derived from here.
//
// The algorithm is frozen in v1: changing it requires migrating the keys and
// checking for conflicts beforehand, not just bumping render_version.
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// AlgorithmVersion identifies the frozen v1 algorithm of this package: every
// output of NormalizeText, Segment, FileSegment, Key, PathKey and
// SanitizeRelFilePath. It is an input of render_version (§2.1, internal/
// render): the output's names are derived from here.
//
// It is not a switch: a change of any output requires a migration of the
// keys already stored and a check for conflicts beforehand (§5.2), and then
// a new AlgorithmVersion. TestAlgorithmVersionPinned fails on any change of
// the outputs until both are done.
const AlgorithmVersion = "1"

const (
	// MaxTextRunes is the limit for metadata texts, in characters.
	MaxTextRunes = 1024
	// MaxSegmentBytes is the limit for a path component, in UTF-8 bytes,
	// prefixes and extension included.
	MaxSegmentBytes = 180
	// MaxPathDepth is the maximum number of levels of a relative path.
	MaxPathDepth = 16
	// MaxPathBytes is the maximum size of a relative path after the
	// transformation, in bytes.
	MaxPathBytes = 1024

	// hashSuffixHexLen is the number of SHA-256 hex characters appended
	// after "~" when a segment is truncated.
	hashSuffixHexLen = 8
)

// forbiddenSegmentRunes are the characters replaced with "_" in a path
// segment. Control characters are replaced as well: they cannot end up in an
// output file name.
const forbiddenSegmentRunes = `/\:*?"<>|`

// NormalizeText normalizes a metadata text: NFC, outer trim, rejection of
// control characters, at most MaxTextRunes characters.
//
// The empty string is allowed and is returned unchanged: fields that cannot
// be empty use NormalizeRequiredText.
func NormalizeText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errf(CodeInvalidUTF8, "the text is not valid UTF-8")
	}
	s = norm.NFC.String(s)
	s = strings.TrimFunc(s, unicode.IsSpace)
	for i, r := range s {
		if unicode.IsControl(r) {
			return "", errf(CodeTextControlChar,
				"the text contains control character U+%04X at position %d", r, i)
		}
	}
	if n := utf8.RuneCountInString(s); n > MaxTextRunes {
		return "", errf(CodeTextTooLong,
			"the text has %d characters, the maximum is %d", n, MaxTextRunes)
	}
	return s, nil
}

// NormalizeRequiredText is NormalizeText for required fields: a value that
// is empty, or becomes empty after the trim, is an error.
func NormalizeRequiredText(s string) (string, error) {
	out, err := NormalizeText(s)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", errf(CodeTextEmpty, "the text is required and cannot be empty")
	}
	return out, nil
}

// Segment sanitizes a path segment without any extension handling: it is
// used for directories (artist, album, "Disc N", the intermediate
// directories of attachments).
//
// The result is never empty, contains neither forbidden characters nor
// control characters, neither starts nor ends with spaces or dots, is not a
// reserved DOS name and does not exceed MaxSegmentBytes bytes.
func Segment(name string) string {
	return sanitize(name, false)
}

// FileSegment sanitizes a segment that is a file name: identical to
// Segment, but it preserves the extension when truncation is needed.
func FileSegment(name string) string {
	return sanitize(name, true)
}

func sanitize(name string, preserveExt bool) string {
	s := norm.NFC.String(coerceUTF8(name))
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || strings.ContainsRune(forbiddenSegmentRunes, r) {
			return '_'
		}
		return r
	}, s)
	s = strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '.'
	})
	if s == "" {
		s = "_"
	}
	if isReservedDOS(s) {
		s = "_" + s
	}
	if len(s) > MaxSegmentBytes {
		s = truncateSegment(s, preserveExt)
	}
	return s
}

// truncateSegment shortens s to MaxSegmentBytes bytes by cutting on a UTF-8
// boundary and appending "~" plus the first hashSuffixHexLen characters of
// the SHA-256 of the full normalized segment. If requested, the extension is
// preserved; if the extension alone leaves no room for the stem, it is not
// preserved rather than producing a name longer than the limit.
func truncateSegment(s string, preserveExt bool) string {
	sum := sha256.Sum256([]byte(s))
	suffix := "~" + hex.EncodeToString(sum[:])[:hashSuffixHexLen]

	ext := ""
	if preserveExt {
		ext = path.Ext(s)
		if len(ext)+len(suffix) >= MaxSegmentBytes {
			ext = ""
		}
	}
	stem := s[:len(s)-len(ext)]
	budget := MaxSegmentBytes - len(suffix) - len(ext) // >= 1 by construction
	return truncateUTF8(stem, budget) + suffix + ext
}

// truncateUTF8 returns the longest prefix of s that is at most max bytes long
// and ends on a rune boundary.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for i := max; i > 0; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	return ""
}

// coerceUTF8 replaces invalid byte sequences with "_". Source paths are
// validated beforehand (SplitRelPath), and so are metadata texts; this is
// the guarantee that sanitize is a total function.
func coerceUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteByte('_')
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// isReservedDOS recognizes the reserved DOS/Windows names, even when they
// carry an extension: CON, NUL, PRN, AUX, COM1..9, LPT1..9.
func isReservedDOS(s string) bool {
	base := s
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch asciiUpper(base) {
	case "CON", "NUL", "PRN", "AUX":
		return true
	}
	if len(base) == 4 && base[3] >= '1' && base[3] <= '9' {
		switch asciiUpper(base[:3]) {
		case "COM", "LPT":
			return true
		}
	}
	return false
}

func asciiUpper(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c - ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// Key is the comparison key of a segment that is already final (that is,
// already passed through Segment or FileSegment): NFC(casefold(segment)).
//
// The case folding is Unicode full case folding, not SQL lower(): "ß" and
// "SS" have the same key. Neither strings.ToLower nor strings.ToUpper is
// used, since they apply simple case mapping.
func Key(finalSegment string) string {
	folded := strings.Map(foldCherokee, cases.Fold().String(finalSegment))
	return norm.NFC.String(folded)
}

// foldCherokee fixes the only mappings in which cases.Fold() from
// golang.org/x/text does not match Unicode full case folding.
//
// CaseFolding.txt maps Cherokee lowercase letters to uppercase
// (AB70..ABBF -> 13A0..13EF and 13F8..13FD -> 13F0..13F5), because the
// uppercase letters were encoded first. cases.Fold() also applies the
// opposite mapping, so the folding oscillates:
// fold(U+ABB8) = U+13E8 and fold(U+13E8) = U+ABB8. Without this fix
// "Ꮸ" and "ꮸ" would have different folder_key values and Key would not be
// idempotent.
//
// Applied after the folding, the fix brings the result into the set of
// canonical targets, where it is a fixed point. Coverage is verified over
// all code points in TestKeyCoversAllOfUnicode.
func foldCherokee(r rune) rune {
	switch {
	case r >= 0xAB70 && r <= 0xABBF:
		return r - 0xAB70 + 0x13A0
	case r >= 0x13F8 && r <= 0x13FD:
		return r - 0x13F8 + 0x13F0
	}
	return r
}

// FolderKey is the key of a directory, computed from the desired name: it
// is the value of the artists.folder_key and albums.folder_key columns.
func FolderKey(name string) string {
	return Key(Segment(name))
}

// PathKey joins with "/" the keys of the individual, already final, segments.
func PathKey(finalSegments []string) string {
	keys := make([]string, len(finalSegments))
	for i, s := range finalSegments {
		keys[i] = Key(s)
	}
	return strings.Join(keys, "/")
}
