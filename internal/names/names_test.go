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
		{"invariato", "Miles Davis", "Miles Davis"},
		{"trim esterno", "  Kind of Blue\t", "Kind of Blue"},
		{"spazi interni conservati", "So  What", "So  What"},
		{"nfc compone", "Björk", "Björk"},
		{"nfc lascia stare cio che e gia composto", "Björk", "Björk"},
		{"newline esterno trimmato", "A\n", "A"},
		{"vuoto ammesso", "", ""},
		{"solo spazi diventa vuoto", "   \t\n ", ""},
		{"ampersand e feat restano testo", "A feat. B & C", "A feat. B & C"},
		{"1024 caratteri esatti", strings.Repeat("x", MaxTextRunes), strings.Repeat("x", MaxTextRunes)},
		{"il limite e in caratteri non byte", strings.Repeat("é", MaxTextRunes), strings.Repeat("é", MaxTextRunes)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeText(tc.in)
			if err != nil {
				t.Fatalf("NormalizeText(%q) = errore %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeText(%q) = %q, atteso %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeTextErrori(t *testing.T) {
	tests := []struct {
		name string
		in   string
		code string
	}{
		{"utf8 non valido", "ab\xffcd", CodeInvalidUTF8},
		{"carattere di controllo interno", "A\x07B", CodeTextControlChar},
		{"newline interno", "A\nB", CodeTextControlChar},
		{"NUL", "A\x00B", CodeTextControlChar},
		{"C1", "AB", CodeTextControlChar},
		{"troppo lungo", strings.Repeat("x", MaxTextRunes+1), CodeTextTooLong},
		{"troppo lungo in caratteri multibyte", strings.Repeat("é", MaxTextRunes+1), CodeTextTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeText(tc.in)
			if err == nil {
				t.Fatalf("NormalizeText(%q): atteso errore %s", tc.in, tc.code)
			}
			if got := Code(err); got != tc.code {
				t.Errorf("NormalizeText(%q): codice %q, atteso %q", tc.in, got, tc.code)
			}
		})
	}
}

func TestNormalizeRequiredText(t *testing.T) {
	if _, err := NormalizeRequiredText("Miles Davis"); err != nil {
		t.Fatalf("valore valido rifiutato: %v", err)
	}
	for _, in := range []string{"", "   ", "\t\n"} {
		_, err := NormalizeRequiredText(in)
		if Code(err) != CodeTextEmpty {
			t.Errorf("NormalizeRequiredText(%q): codice %q, atteso %q", in, Code(err), CodeTextEmpty)
		}
	}
	// Un errore di forma ha la precedenza sul vuoto.
	if got := Code(mustErr(NormalizeRequiredText("\x07"))); got != CodeTextControlChar {
		t.Errorf("codice %q, atteso %q", got, CodeTextControlChar)
	}
}

func TestSegment(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"invariato", "Kind of Blue", "Kind of Blue"},
		{"slash", "AC/DC", "AC_DC"},
		{"tutti i vietati", `a/b\c:d*e?f"g<h>i|j`, "a_b_c_d_e_f_g_h_i_j"},
		{"due punti", "Vol: 2", "Vol_ 2"},
		{"punto finale", "Album.", "Album"},
		{"punti finali multipli", "Album...", "Album"},
		{"punto iniziale", ".hidden", "hidden"},
		{"spazi esterni", "  Album  ", "Album"},
		{"spazio e punto misti", " . Album . ", "Album"},
		{"estensione interna conservata", "rip.log", "rip.log"},
		{"vuoto", "", "_"},
		{"solo punti", "...", "_"},
		{"solo vietati", "///", "___"},
		{"solo spazi", "   ", "_"},
		{"nfc", "Björk", "Björk"},
		{"controllo sostituito", "A\x07B", "A_B"},
		{"tab sostituito", "A\tB", "A_B"},
		{"utf8 non valido sostituito", "a\xffb", "a_b"},
		{"unicode conservato", "東京", "東京"},
		{"emoji conservata", "Album 🎵", "Album 🎵"},
		{"tilde conservata", "A~B", "A~B"},
		{"underscore conservato", "A_B", "A_B"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Segment(tc.in); got != tc.want {
				t.Errorf("Segment(%q) = %q, atteso %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSegmentNomiDOSRiservati(t *testing.T) {
	riservati := []string{
		"CON", "NUL", "PRN", "AUX",
		"COM1", "COM9", "LPT1", "LPT9",
		"con", "Nul", "cOm3",
		"CON.txt", "nul.flac", "LPT4.tar.gz",
		"CON.", // il punto finale viene trimmato prima del controllo
	}
	for _, in := range riservati {
		t.Run(in, func(t *testing.T) {
			got := Segment(in)
			if !strings.HasPrefix(got, "_") {
				t.Errorf("Segment(%q) = %q, atteso un prefisso _", in, got)
			}
			if isReservedDOS(got) {
				t.Errorf("Segment(%q) = %q, ancora riservato", in, got)
			}
		})
	}

	nonRiservati := []string{
		"COM0", "LPT0", "COM10", "CONS", "CONSOLE", "NULL",
		"AUXILIARY", "PRNT", "_CON", "CO", "COM", "LPT",
		"my CON", "CON files",
	}
	for _, in := range nonRiservati {
		t.Run("ok/"+in, func(t *testing.T) {
			if got := Segment(in); got != in {
				t.Errorf("Segment(%q) = %q, atteso invariato", in, got)
			}
		})
	}
}

func TestSegmentTroncamento(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := Segment(long)
	if len(got) != MaxSegmentBytes {
		t.Fatalf("lunghezza %d, attesa %d", len(got), MaxSegmentBytes)
	}
	sum := sha256.Sum256([]byte(long))
	wantSuffix := "~" + hex.EncodeToString(sum[:])[:hashSuffixHexLen]
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("%q non termina con %q", got, wantSuffix)
	}
	if !strings.HasPrefix(got, strings.Repeat("a", MaxSegmentBytes-len(wantSuffix))) {
		t.Errorf("stem troncato inatteso: %q", got)
	}
}

func TestSegmentTroncamentoConfini(t *testing.T) {
	// 180 byte esatti: nessun troncamento.
	exact := strings.Repeat("a", MaxSegmentBytes)
	if got := Segment(exact); got != exact {
		t.Errorf("un segmento di %d byte e stato troncato: %q", MaxSegmentBytes, got)
	}
	// 181 byte: troncamento.
	over := strings.Repeat("a", MaxSegmentBytes+1)
	if got := Segment(over); len(got) != MaxSegmentBytes || !strings.Contains(got, "~") {
		t.Errorf("Segment(181 byte) = %q (len %d)", got, len(got))
	}
}

func TestSegmentTroncamentoSuConfineUTF8(t *testing.T) {
	// "é" occupa 2 byte: un troncamento ingenuo a 171 byte spezzerebbe una rune.
	in := strings.Repeat("é", 100)
	got := Segment(in)
	if !utf8.ValidString(got) {
		t.Fatalf("il risultato non e UTF-8 valido: %q", got)
	}
	if len(got) > MaxSegmentBytes {
		t.Fatalf("lunghezza %d oltre il limite", len(got))
	}
	// Il budget dello stem e 180-9 = 171 byte, cioe 85 caratteri "é" (170 byte).
	stem := got[:len(got)-hashSuffixHexLen-1]
	if stem != strings.Repeat("é", 85) {
		t.Errorf("stem = %q", stem)
	}

	// Rune da 4 byte.
	in4 := strings.Repeat("🎵", 60)
	got4 := Segment(in4)
	if !utf8.ValidString(got4) || len(got4) > MaxSegmentBytes {
		t.Fatalf("Segment(4-byte runes) = %q (len %d)", got4, len(got4))
	}
}

func TestSegmentTroncamentoDeterministicoEDistintivo(t *testing.T) {
	a := strings.Repeat("a", 250)
	b := strings.Repeat("a", 250) + "b"
	if Segment(a) != Segment(a) {
		t.Error("il troncamento non e deterministico")
	}
	// I primi 180 byte coincidono: solo il suffisso hash distingue i due nomi.
	if Segment(a) == Segment(b) {
		t.Error("due segmenti diversi con lo stesso prefisso hanno prodotto lo stesso nome")
	}
}

func TestFileSegmentPreservaEstensione(t *testing.T) {
	in := strings.Repeat("a", 200) + ".flac"
	got := FileSegment(in)
	if len(got) != MaxSegmentBytes {
		t.Fatalf("lunghezza %d, attesa %d", len(got), MaxSegmentBytes)
	}
	if !strings.HasSuffix(got, ".flac") {
		t.Errorf("FileSegment ha perso l'estensione: %q", got)
	}
	if !strings.Contains(got, "~") {
		t.Errorf("manca il suffisso hash: %q", got)
	}
	// Segment (directory) non preserva nulla: tronca e basta.
	if strings.HasSuffix(Segment(in), ".flac") {
		t.Errorf("Segment non deve preservare l'estensione: %q", Segment(in))
	}
}

func TestFileSegmentEstensioneIngombrante(t *testing.T) {
	// Un'estensione che non lascia spazio allo stem viene abbandonata,
	// mai troncata oltre il limite.
	in := "a" + strings.Repeat("b", 250) + "." + strings.Repeat("c", 200)
	got := FileSegment(in)
	if len(got) > MaxSegmentBytes {
		t.Fatalf("lunghezza %d oltre il limite: %q", len(got), got)
	}
	if !strings.Contains(got, "~") {
		t.Errorf("manca il suffisso hash: %q", got)
	}
}

func TestFileSegmentSenzaEstensione(t *testing.T) {
	in := strings.Repeat("a", 200)
	if got := FileSegment(in); got != Segment(in) {
		t.Errorf("senza estensione FileSegment e Segment devono coincidere: %q vs %q", got, Segment(in))
	}
}

func TestSegmentIdempotente(t *testing.T) {
	ins := []string{
		"AC/DC", "  Album. ", "CON", "...", "",
		strings.Repeat("a", 250), strings.Repeat("é", 200),
		strings.Repeat("a", 250) + ".flac", "a\xffb", "🎵" + strings.Repeat("x", 300),
	}
	for _, in := range ins {
		once := Segment(in)
		if twice := Segment(once); twice != once {
			t.Errorf("Segment non idempotente su %q: %q -> %q", in, once, twice)
		}
		onceF := FileSegment(in)
		if twiceF := FileSegment(onceF); twiceF != onceF {
			t.Errorf("FileSegment non idempotente su %q: %q -> %q", in, onceF, twiceF)
		}
	}
}

func TestKey(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		same bool
	}{
		{"maiuscole", "Abba", "ABBA", true},
		{"maiuscole miste", "Kind of Blue", "KIND OF BLUE", true},
		{"eszett piega a ss", "straße", "STRASSE", true},
		{"sigma finale", "ΟΔΥΣΣΕΥΣ", "οδυσσευς", true},
		{"nfc", "Björk", "Björk", true},
		{"accenti diversi restano diversi", "Bjork", "Björk", false},
		{"nomi diversi", "Abba", "Abbas", false},
		{"spazio significativo", "AB BA", "ABBA", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Key(tc.a) == Key(tc.b); got != tc.same {
				t.Errorf("Key(%q)=%q, Key(%q)=%q: uguali=%v, atteso %v",
					tc.a, Key(tc.a), tc.b, Key(tc.b), got, tc.same)
			}
		})
	}
}

func TestKeyIdempotente(t *testing.T) {
	for _, in := range []string{"Abba", "STRASSE", "straße", "ΟΔΥΣΣΕΥΣ", "Björk", "東京", "İstanbul"} {
		once := Key(in)
		if twice := Key(once); twice != once {
			t.Errorf("Key non idempotente su %q: %q -> %q", in, once, twice)
		}
	}
}

func TestFolderKey(t *testing.T) {
	// La chiave si calcola sul segmento finale, cioe dopo la sanitizzazione.
	if FolderKey("AC/DC") != FolderKey("ac_dc") {
		t.Errorf("FolderKey(%q)=%q, FolderKey(%q)=%q", "AC/DC", FolderKey("AC/DC"), "ac_dc", FolderKey("ac_dc"))
	}
	if FolderKey("Abba") != FolderKey("  ABBA  ") {
		t.Error("trim e case devono collassare sulla stessa chiave")
	}
	if FolderKey("Kind of Blue") == FolderKey("Kind of Blue (Mono)") {
		t.Error("edizioni diverse devono avere chiavi diverse")
	}
}

func TestPathKey(t *testing.T) {
	got := PathKey([]string{"Extras", "Scans", "Front.JPG"})
	want := "extras/scans/front.jpg"
	if got != want {
		t.Errorf("PathKey = %q, atteso %q", got, want)
	}
	if PathKey(nil) != "" {
		t.Errorf("PathKey(nil) = %q, atteso vuoto", PathKey(nil))
	}
	if PathKey([]string{"a"}) != "a" {
		t.Errorf("PathKey singolo = %q", PathKey([]string{"a"}))
	}
}

func mustErr(_ string, err error) error { return err }

func TestErrorEDecodificaCodice(t *testing.T) {
	err := &Error{Code: CodeTextEmpty, Message: "vuoto"}
	if got, want := err.Error(), "text_empty: vuoto"; got != want {
		t.Errorf("Error() = %q, atteso %q", got, want)
	}
	if Code(nil) != "" {
		t.Errorf("Code(nil) = %q, atteso vuoto", Code(nil))
	}
	if Code(errors.New("altro")) != "" {
		t.Errorf("Code su un errore estraneo = %q, atteso vuoto", Code(errors.New("altro")))
	}
	if got := Code(fmt.Errorf("contesto: %w", err)); got != CodeTextEmpty {
		t.Errorf("Code su errore incapsulato = %q, atteso %q", got, CodeTextEmpty)
	}
}

func TestFileSegmentStemNonRappresentabile(t *testing.T) {
	// Budget dello stem di 1 byte con una prima rune da 4 byte: lo stem
	// sparisce del tutto, ma il nome resta valido, entro il limite e
	// distintivo grazie al suffisso hash.
	ext := "." + strings.Repeat("c", 169) // 170 byte: budget = 180-9-170 = 1
	in := "\U0001F3B5" + strings.Repeat("b", 100) + ext
	got := FileSegment(in)
	if len(got) > MaxSegmentBytes {
		t.Fatalf("lunghezza %d oltre il limite: %q", len(got), got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("UTF-8 non valido: %q", got)
	}
	if !strings.HasPrefix(got, "~") {
		t.Errorf("atteso uno stem vuoto: %q", got)
	}
	if !strings.HasSuffix(got, ext) {
		t.Errorf("estensione persa: %q", got)
	}
	// Due input diversi con lo stesso troncamento restano distinti.
	other := "\U0001F3B6" + strings.Repeat("b", 100) + ext
	if FileSegment(other) == got {
		t.Error("due segmenti diversi hanno prodotto lo stesso nome")
	}
}
