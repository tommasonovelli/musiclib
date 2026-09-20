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
		{"singolo", "album", []string{"album"}},
		{"annidato", "Miles Davis/Kind of Blue/01.flac", []string{"Miles Davis", "Kind of Blue", "01.flac"}},
		{"nome esotico conservato", "AC_DC/  spazi  /CON", []string{"AC_DC", "  spazi  ", "CON"}},
		{"backslash e un carattere normale", `a\b`, []string{`a\b`}},
		{"punto interno al nome", "..hidden", []string{"..hidden"}},
		{"nome che inizia per punto", ".DS_Store", []string{".DS_Store"}},
		{"16 livelli esatti", strings.Repeat("a/", MaxPathDepth-1) + "a", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitRelPath(tc.in)
			if err != nil {
				t.Fatalf("SplitRelPath(%q) = errore %v", tc.in, err)
			}
			if tc.want == nil {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("SplitRelPath(%q) = %q, atteso %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("SplitRelPath(%q) = %q, atteso %q", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestSplitRelPathErrori(t *testing.T) {
	tests := []struct {
		name string
		in   string
		code string
	}{
		{"vuoto", "", CodePathEmpty},
		{"assoluto", "/etc/passwd", CodePathAbsolute},
		{"radice", "/", CodePathAbsolute},
		{"punto", ".", CodePathDotSegment},
		{"punto punto", "..", CodePathDotSegment},
		{"traversal", "a/../../etc", CodePathDotSegment},
		{"traversal in coda", "a/..", CodePathDotSegment},
		{"punto interno", "a/./b", CodePathDotSegment},
		{"segmento vuoto", "a//b", CodePathEmptySegment},
		{"slash finale", "a/", CodePathEmptySegment},
		{"utf8 non valido", "a/\xff/b", CodeInvalidUTF8},
		{"byte NUL", "a\x00b", CodePathNulByte},
		{"troppo profondo", strings.Repeat("a/", MaxPathDepth) + "a", CodePathTooDeep},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SplitRelPath(tc.in)
			if err == nil {
				t.Fatalf("SplitRelPath(%q): atteso errore %s", tc.in, tc.code)
			}
			if got := Code(err); got != tc.code {
				t.Errorf("SplitRelPath(%q): codice %q, atteso %q", tc.in, got, tc.code)
			}
		})
	}
}

func TestSplitRelPathOrRoot(t *testing.T) {
	got, err := SplitRelPathOrRoot("")
	if err != nil {
		t.Fatalf("la stringa vuota deve indicare la root: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("SplitRelPathOrRoot(\"\") = %q, attesi zero segmenti", got)
	}
	// Le altre regole restano identiche.
	if Code(mustErrSlice(SplitRelPathOrRoot("../x"))) != CodePathDotSegment {
		t.Error("SplitRelPathOrRoot deve rifiutare il traversal")
	}
}

func TestSanitizeRelFilePath(t *testing.T) {
	got, err := SanitizeRelFilePath("Scans/Front Cover.JPG")
	if err != nil {
		t.Fatalf("errore inatteso: %v", err)
	}
	if got.Path != "Scans/Front Cover.JPG" {
		t.Errorf("Path = %q", got.Path)
	}
	if got.Key != "scans/front cover.jpg" {
		t.Errorf("Key = %q", got.Key)
	}

	// Ogni segmento viene sanitizzato; solo l'ultimo conserva l'estensione.
	got, err = SanitizeRelFilePath("CON/a:b/" + strings.Repeat("x", 200) + ".pdf")
	if err != nil {
		t.Fatalf("errore inatteso: %v", err)
	}
	if got.Segments[0] != "_CON" {
		t.Errorf("segmento 0 = %q, atteso _CON", got.Segments[0])
	}
	if got.Segments[1] != "a_b" {
		t.Errorf("segmento 1 = %q, atteso a_b", got.Segments[1])
	}
	if !strings.HasSuffix(got.Segments[2], ".pdf") || len(got.Segments[2]) != MaxSegmentBytes {
		t.Errorf("segmento 2 = %q (len %d)", got.Segments[2], len(got.Segments[2]))
	}
}

func TestSanitizeRelFilePathLimiti(t *testing.T) {
	// 16 segmenti da 180 byte superano i 1.024 byte del percorso.
	long := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 180)+"/", MaxPathDepth), "/")
	_, err := SanitizeRelFilePath(long)
	if got := Code(err); got != CodePathTooLong {
		t.Errorf("codice %q, atteso %q", got, CodePathTooLong)
	}

	// La profondita si controlla prima della sanitizzazione.
	_, err = SanitizeRelFilePath(strings.Repeat("a/", MaxPathDepth) + "a")
	if got := Code(err); got != CodePathTooDeep {
		t.Errorf("codice %q, atteso %q", got, CodePathTooDeep)
	}

	// Un percorso lungo ma entro entrambi i limiti passa: 16 livelli e
	// 1.024 byte esatti dopo la trasformazione (15 separatori compresi).
	ok := strings.Repeat(strings.Repeat("a", 63)+"/", MaxPathDepth-1) + strings.Repeat("a", 64)
	got, err := SanitizeRelFilePath(ok)
	if err != nil {
		t.Fatalf("percorso al limite rifiutato: %v", err)
	}
	if len(got.Path) != MaxPathBytes {
		t.Errorf("lunghezza %d, attesa %d", len(got.Path), MaxPathBytes)
	}

	// Un byte in piu viene rifiutato.
	if _, err := SanitizeRelFilePath(ok + "a"); Code(err) != CodePathTooLong {
		t.Errorf("codice %q, atteso %q", Code(err), CodePathTooLong)
	}
}

func TestSanitizeRelFilePathNonEvadeLaRoot(t *testing.T) {
	// La sanitizzazione non e il confinamento (DESIGN.md §10.4), ma non deve
	// comunque poter produrre segmenti che risalgono l'albero.
	for _, in := range []string{"..", "a/../b", "/abs", "a/./b"} {
		if _, err := SanitizeRelFilePath(in); err == nil {
			t.Errorf("SanitizeRelFilePath(%q) accettato", in)
		}
	}
	// Un nome che contiene punti ma non e un segmento speciale resta valido
	// e non introduce separatori.
	got, err := SanitizeRelFilePath("..a/b..c")
	if err != nil {
		t.Fatalf("errore inatteso: %v", err)
	}
	if got.Segments[0] != "a" || got.Segments[1] != "b..c" {
		t.Errorf("segmenti = %q", got.Segments)
	}
	for _, s := range got.Segments {
		if strings.Contains(s, "/") || s == "." || s == ".." {
			t.Errorf("segmento pericoloso: %q", s)
		}
	}
}

func mustErrSlice(_ []string, err error) error { return err }
