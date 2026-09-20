package names

import (
	"testing"
	"unicode"
)

// TestKeyCopreTuttoUnicode verifica in modo esaustivo, code point per code
// point, le due proprietà su cui si appoggiano folder_key, path_key e
// path_claims (DESIGN.md §5.2, §5.3):
//
//  1. Key è idempotente, quindi esiste una sola forma canonica;
//  2. code point equivalenti a meno del case hanno la stessa chiave.
//
// L'enumerazione completa è preferibile al fuzzing per questa proprietà:
// lo spazio è piccolo e l'algoritmo è congelato nella v1.
func TestKeyCopreTuttoUnicode(t *testing.T) {
	if testing.Short() {
		t.Skip("enumerazione di tutti i code point")
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // surrogati: non sono UTF-8 valido
		}
		s := string(r)
		k := Key(s)
		if again := Key(k); again != k {
			t.Fatalf("Key non idempotente su U+%04X: %q -> %q -> %q", r, s, k, again)
		}
		// unicode.SimpleFold percorre l'orbita di case di r.
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if fk := Key(string(f)); fk != k {
				t.Fatalf("U+%04X e U+%04X sono equivalenti a meno del case ma hanno chiavi diverse: %q vs %q",
					r, f, k, fk)
			}
		}
	}
}

// TestKeyCherokee fissa il comportamento corretto sulle lettere cherokee,
// il caso in cui cases.Fold() di golang.org/x/text non coincide con il full
// case folding di Unicode.
func TestKeyCherokee(t *testing.T) {
	tests := []struct {
		maiuscola, minuscola string
		vuoleChiave          rune
	}{
		{"Ꮸ", "ꮸ", 0x13E8},
		{"Ꭰ", "ꭰ", 0x13A0},
		{"Ꮿ", "ꮿ", 0x13EF},
		{"Ᏸ", "ᏸ", 0x13F0},
		{"Ᏽ", "ᏽ", 0x13F5},
	}
	for _, tc := range tests {
		want := string(tc.vuoleChiave)
		if got := Key(tc.maiuscola); got != want {
			t.Errorf("Key(%q) = %q (U+%04X), attesa U+%04X", tc.maiuscola, got, []rune(got)[0], tc.vuoleChiave)
		}
		if got := Key(tc.minuscola); got != want {
			t.Errorf("Key(%q) = %q (U+%04X), attesa U+%04X", tc.minuscola, got, []rune(got)[0], tc.vuoleChiave)
		}
	}
	// Il caso reale: due album il cui nome differisce solo per il case
	// devono occupare la stessa riga di path_claims.
	if FolderKey("Ꮸsa") != FolderKey("ꮸsa") {
		t.Error("due varianti di case dello stesso nome hanno folder_key diverse")
	}
}
