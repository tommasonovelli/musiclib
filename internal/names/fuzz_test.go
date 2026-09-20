package names

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

func normNFC(s string) string { return norm.NFC.String(s) }

// semi copre i casi che i test tabellari usano e serve anche da corpus
// iniziale per il fuzzing.
var semi = []string{
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

// controllaSegmento verifica tutte le invarianti che il resto del sistema
// dà per acquisite su un segmento finale (DESIGN.md §5.1, §5.2).
func controllaSegmento(t *testing.T, in, got string) {
	t.Helper()
	if got == "" {
		t.Fatalf("segmento vuoto da %q", in)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("UTF-8 non valido da %q: %q", in, got)
	}
	if len(got) > MaxSegmentBytes {
		t.Fatalf("%q -> %q: %d byte, limite %d", in, got, len(got), MaxSegmentBytes)
	}
	if norm := normNFC(got); norm != got {
		t.Fatalf("%q -> %q: non è in NFC", in, got)
	}
	for _, r := range got {
		if strings.ContainsRune(forbiddenSegmentRunes, r) {
			t.Fatalf("%q -> %q: contiene il carattere vietato %q", in, got, r)
		}
		if unicode.IsControl(r) {
			t.Fatalf("%q -> %q: contiene un carattere di controllo U+%04X", in, got, r)
		}
	}
	first, _ := utf8.DecodeRuneInString(got)
	last, _ := utf8.DecodeLastRuneInString(got)
	for _, r := range []rune{first, last} {
		if unicode.IsSpace(r) || r == '.' {
			t.Fatalf("%q -> %q: spazio o punto esterno", in, got)
		}
	}
	if isReservedDOS(got) {
		t.Fatalf("%q -> %q: nome DOS riservato", in, got)
	}
}

func FuzzSegment(f *testing.F) {
	for _, s := range semi {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := Segment(in)
		controllaSegmento(t, in, got)
		if again := Segment(got); again != got {
			t.Fatalf("Segment non idempotente: %q -> %q -> %q", in, got, again)
		}

		gotFile := FileSegment(in)
		controllaSegmento(t, in, gotFile)
		if again := FileSegment(gotFile); again != gotFile {
			t.Fatalf("FileSegment non idempotente: %q -> %q -> %q", in, gotFile, again)
		}

		// Determinismo: nessuno stato condiviso fra chiamate.
		if Segment(in) != got || FileSegment(in) != gotFile {
			t.Fatalf("risultato non deterministico per %q", in)
		}
	})
}

func FuzzKey(f *testing.F) {
	for _, s := range semi {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		seg := Segment(in)
		k := Key(seg)
		if !utf8.ValidString(k) {
			t.Fatalf("chiave non UTF-8 valida da %q: %q", in, k)
		}
		if strings.ContainsRune(k, '/') {
			t.Fatalf("la chiave di un segmento non può contenere '/': %q", k)
		}
		if again := Key(k); again != k {
			t.Fatalf("Key non idempotente: %q -> %q -> %q", seg, k, again)
		}
		// La chiave non distingue le maiuscole: è la proprietà su cui si
		// appoggiano folder_key e path_claims.
		//
		// Il confronto usa uno scambio di case limitato ad ASCII: le
		// funzioni strings.ToUpper/ToLower applicano il case mapping
		// semplice e non sono invertibili su tutto Unicode (per esempio
		// U+0130 perde il punto in ToLower), quindi non sono un oracolo
		// valido per il full case folding.
		if Key(swapASCIICase(seg)) != Key(seg) {
			t.Fatalf("Key distingue il case ASCII di %q", seg)
		}
	})
}

func FuzzNormalizeText(f *testing.F) {
	for _, s := range semi {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got, err := NormalizeText(in)
		if err != nil {
			if Code(err) == "" {
				t.Fatalf("errore senza codice: %v", err)
			}
			return
		}
		if !utf8.ValidString(got) {
			t.Fatalf("UTF-8 non valido da %q", in)
		}
		if normNFC(got) != got {
			t.Fatalf("%q -> %q: non è in NFC", in, got)
		}
		if utf8.RuneCountInString(got) > MaxTextRunes {
			t.Fatalf("%q -> %d caratteri", in, utf8.RuneCountInString(got))
		}
		for _, r := range got {
			if unicode.IsControl(r) {
				t.Fatalf("%q -> %q: carattere di controllo residuo", in, got)
			}
		}
		if strings.TrimFunc(got, unicode.IsSpace) != got {
			t.Fatalf("%q -> %q: spazio esterno residuo", in, got)
		}
		again, err := NormalizeText(got)
		if err != nil || again != got {
			t.Fatalf("NormalizeText non idempotente: %q -> %q -> %q (%v)", in, got, again, err)
		}
	})
}

func FuzzSplitRelPath(f *testing.F) {
	for _, s := range semi {
		f.Add(s)
		f.Add("a/" + s)
		f.Add(s + "/b")
	}
	f.Fuzz(func(t *testing.T, in string) {
		segs, err := SplitRelPath(in)
		if err != nil {
			if Code(err) == "" {
				t.Fatalf("errore senza codice: %v", err)
			}
			return
		}
		if len(segs) == 0 || len(segs) > MaxPathDepth {
			t.Fatalf("%q -> %d segmenti", in, len(segs))
		}
		for _, s := range segs {
			if s == "" || s == "." || s == ".." || strings.ContainsRune(s, '/') {
				t.Fatalf("%q -> segmento %q", in, s)
			}
		}
		// I segmenti restano esattamente quelli del disco.
		if strings.Join(segs, "/") != in {
			t.Fatalf("%q ricomposto come %q", in, strings.Join(segs, "/"))
		}

		sp, err := SanitizeRelFilePath(in)
		if err != nil {
			if Code(err) != CodePathTooLong {
				t.Fatalf("%q -> errore inatteso %v", in, err)
			}
			return
		}
		if len(sp.Segments) != len(segs) {
			t.Fatalf("%q: %d segmenti sanitizzati, attesi %d", in, len(sp.Segments), len(segs))
		}
		for _, s := range sp.Segments {
			controllaSegmento(t, in, s)
		}
		if len(sp.Path) > MaxPathBytes {
			t.Fatalf("%q -> percorso di %d byte", in, len(sp.Path))
		}
		if sp.Path != strings.Join(sp.Segments, "/") {
			t.Fatalf("Path incoerente con Segments per %q", in)
		}
		if sp.Key != PathKey(sp.Segments) {
			t.Fatalf("Key incoerente con Segments per %q", in)
		}
	})
}

// swapASCIICase inverte il case delle sole lettere ASCII, lasciando intatto
// il resto: e una trasformazione invertibile, a differenza di
// strings.ToUpper/ToLower su Unicode.
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
