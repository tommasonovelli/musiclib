package names

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
	"testing"
	"unicode"
)

// algorithmDigests pins the outputs of the package for each AlgorithmVersion:
// the SHA-256 of algorithmTranscript. The algorithm is frozen in v1 (§5.2):
// the keys it produced are stored in folder_key, path_key and path_claims,
// and the names it produced are on disk in library/.
var algorithmDigests = map[string]string{
	"1": "c6e7c54a022a670b0d2c4b25c6da18732abfbc9161163dcf38b4f49717c70c87",
}

// TestAlgorithmVersionPinned fails when any output of the normalization
// changes: a change of this package, of golang.org/x/text, or of the Unicode
// tables of the Go toolchain (unicode.IsSpace, unicode.IsControl). Such a
// change is never a plain bump: it needs a migration of the stored keys and
// a check for conflicts first (§5.2), then a new AlgorithmVersion, which
// changes render_version (§2.1). Only then is its digest recorded here.
func TestAlgorithmVersionPinned(t *testing.T) {
	if testing.Short() {
		t.Skip("enumeration of all code points")
	}
	want, ok := algorithmDigests[AlgorithmVersion]
	if !ok {
		t.Fatalf("AlgorithmVersion %q has no pinned digest", AlgorithmVersion)
	}
	h := sha256.New()
	algorithmTranscript(h)
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		t.Fatalf("the outputs of internal/names changed (digest %s, pinned %s for AlgorithmVersion %q).\n"+
			"The algorithm is frozen (DESIGN.md §5.2): a change needs a migration of folder_key, path_key and "+
			"path_claims with a conflict check, then a new AlgorithmVersion (which changes render_version).",
			got, want, AlgorithmVersion)
	}
}

// algorithmTranscript writes every output of the package on a fixed input
// set to w: each code point alone, then a corpus of multi-character cases
// (truncation, extensions, DOS names, trimming, relative paths).
func algorithmTranscript(w hash.Hash) {
	emit := func(parts ...string) {
		for _, p := range parts {
			fmt.Fprintf(w, "%d:%s|", len(p), p)
		}
		w.Write([]byte{'\n'})
	}
	text := func(s string) string {
		out, err := NormalizeText(s)
		if err != nil {
			return "!" + Code(err)
		}
		return out
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		s := string(r)
		emit(s, Segment(s), FileSegment(s), Key(s), text(s))
	}
	corpus := []string{
		"", " ", ".", "..", "...", " a ", ".hidden", "trailing.", "a/b", `a\b`, "a:b*c?d\"e<f>g|h",
		"CON", "con.txt", "Com1", "LPT9.log", "COM0", "AUX.tar.gz", "nul", "prn .",
		"Kind of Blue (feat. J.C.)", "e\u0301", "É", "ß", "STRASSE", "Straße", "ﬁ", "İ", "ı", "Σίσυφος", "ΣΊΣΥΦΟΣ",
		"Ꮸꮸ", "tab\there", "nl\nhere", "\u200bzero width", "\ufeffbom",
		strings.Repeat("a", 179), strings.Repeat("a", 180), strings.Repeat("a", 181),
		strings.Repeat("é", 100), strings.Repeat("é", 91) + ".flac", strings.Repeat("😀", 50) + ".jpeg",
		strings.Repeat("x", 300) + "." + strings.Repeat("e", 200), strings.Repeat("x", 170) + ".extension",
		"01 - " + strings.Repeat("Title ", 40) + ".flac",
	}
	for _, s := range corpus {
		emit(s, Segment(s), FileSegment(s), Key(s), FolderKey(s), text(s))
		req, err := NormalizeRequiredText(s)
		if err != nil {
			req = "!" + Code(err)
		}
		emit(req)
	}
	paths := []string{
		"a", "a/b", "Scans/front.jpg", "/abs", "a//b", "a/./b", "a/../b", "a/b/", "nul\x00byte",
		strings.Repeat("d/", 15) + "f", strings.Repeat("d/", 16) + "f",
		strings.Repeat(strings.Repeat("x", 170)+"/", 6) + "f.txt",
		"CON/aux.txt", ".hidden/.file", "Ünïcödé/NFD e\u0301.pdf",
	}
	for _, p := range paths {
		sp, err := SanitizeRelFilePath(p)
		if err != nil {
			emit(p, "!"+Code(err))
			continue
		}
		emit(append([]string{p, sp.Path, sp.Key}, sp.Segments...)...)
		segs, _ := SplitRelPathOrRoot(p)
		emit(PathKey(segs))
	}
}
