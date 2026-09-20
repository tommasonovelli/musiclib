// Package names contiene l'unica implementazione della normalizzazione di
// testi, segmenti di percorso e chiavi di confronto (DESIGN.md §5.2).
//
// Il pacchetto è puro: non fa I/O, non conosce il database e non conosce
// percorsi assoluti. Le colonne folder_key e path_key, le prenotazioni di
// path_claims e i nomi prodotti dal planner derivano tutti da qui.
//
// L'algoritmo è congelato nella v1: cambiarlo richiede la migrazione delle
// chiavi e una verifica preventiva dei conflitti, non un semplice bump di
// render_version.
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

const (
	// MaxTextRunes è il limite dei testi dei metadati, in caratteri.
	MaxTextRunes = 1024
	// MaxSegmentBytes è il limite di un componente di percorso, in byte
	// UTF-8, inclusi prefissi ed estensione.
	MaxSegmentBytes = 180
	// MaxPathDepth è il numero massimo di livelli di un percorso relativo.
	MaxPathDepth = 16
	// MaxPathBytes è la dimensione massima di un percorso relativo dopo la
	// trasformazione, in byte.
	MaxPathBytes = 1024

	// hashSuffixHexLen è il numero di caratteri esadecimali di SHA-256
	// aggiunti dopo "~" quando un segmento viene troncato.
	hashSuffixHexLen = 8
)

// forbiddenSegmentRunes sono i caratteri sostituiti con "_" in un segmento
// di percorso. Oltre a questi si sostituiscono i caratteri di controllo:
// non possono finire in un nome di file dell'output.
const forbiddenSegmentRunes = `/\:*?"<>|`

// NormalizeText normalizza un testo dei metadati: NFC, trim esterno, rifiuto
// dei caratteri di controllo, massimo MaxTextRunes caratteri.
//
// La stringa vuota è ammessa e viene restituita invariata: i campi che non
// possono essere vuoti usano NormalizeRequiredText.
func NormalizeText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errf(CodeInvalidUTF8, "il testo non è UTF-8 valido")
	}
	s = norm.NFC.String(s)
	s = strings.TrimFunc(s, unicode.IsSpace)
	for i, r := range s {
		if unicode.IsControl(r) {
			return "", errf(CodeTextControlChar,
				"il testo contiene un carattere di controllo U+%04X alla posizione %d", r, i)
		}
	}
	if n := utf8.RuneCountInString(s); n > MaxTextRunes {
		return "", errf(CodeTextTooLong,
			"il testo ha %d caratteri, il massimo è %d", n, MaxTextRunes)
	}
	return s, nil
}

// NormalizeRequiredText è NormalizeText per i campi obbligatori: un valore
// vuoto, o che diventa vuoto dopo il trim, è un errore.
func NormalizeRequiredText(s string) (string, error) {
	out, err := NormalizeText(s)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", errf(CodeTextEmpty, "il testo è obbligatorio e non può essere vuoto")
	}
	return out, nil
}

// Segment sanitizza un segmento di percorso senza trattamento
// dell'estensione: si usa per le directory (artista, album, "Disc N", le
// directory intermedie degli allegati).
//
// Il risultato non è mai vuoto, non contiene i caratteri vietati né
// caratteri di controllo, non inizia né termina con spazi o punti, non è un
// nome DOS riservato e non supera MaxSegmentBytes byte.
func Segment(name string) string {
	return sanitize(name, false)
}

// FileSegment sanitizza un segmento che è un nome di file: identico a
// Segment, ma se serve troncare preserva l'estensione.
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

// truncateSegment riduce s a MaxSegmentBytes byte troncando su un confine
// UTF-8 e aggiungendo "~" più i primi hashSuffixHexLen caratteri dello
// SHA-256 del segmento completo normalizzato. Se richiesto, l'estensione
// viene preservata; se l'estensione da sola non lascia spazio allo stem, si
// rinuncia a preservarla anziché produrre un nome più lungo del limite.
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
	budget := MaxSegmentBytes - len(suffix) - len(ext) // >= 1 per costruzione
	return truncateUTF8(stem, budget) + suffix + ext
}

// truncateUTF8 restituisce il più lungo prefisso di s lungo al più max byte
// che finisce su un confine di rune.
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

// coerceUTF8 sostituisce le sequenze di byte non valide con "_". I percorsi
// delle sorgenti vengono validati prima (SplitRelPath) e i testi dei
// metadati pure; questa è la garanzia che sanitize sia una funzione totale.
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

// isReservedDOS riconosce i nomi riservati di DOS/Windows, anche quando
// portano un'estensione: CON, NUL, PRN, AUX, COM1..9, LPT1..9.
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

// Key è la chiave di confronto di un segmento già finale (cioè già passato
// da Segment o FileSegment): NFC(casefold(segmento)).
//
// Il case folding è quello completo di Unicode, non lower() di SQL: "ß" e
// "SS" hanno la stessa chiave. Non si usano nemmeno strings.ToLower o
// strings.ToUpper, che applicano il case mapping semplice.
func Key(finalSegment string) string {
	folded := strings.Map(foldCherokee, cases.Fold().String(finalSegment))
	return norm.NFC.String(folded)
}

// foldCherokee corregge le uniche mappature in cui cases.Fold() di
// golang.org/x/text non coincide con il full case folding di Unicode.
//
// CaseFolding.txt mappa le minuscole cherokee sulle maiuscole
// (AB70..ABBF -> 13A0..13EF e 13F8..13FD -> 13F0..13F5), perché le
// maiuscole sono state codificate per prime. cases.Fold() esegue invece
// anche la mappatura opposta, quindi il folding oscilla:
// fold(U+ABB8) = U+13E8 e fold(U+13E8) = U+ABB8. Senza questa correzione
// "Ꮸ" e "ꮸ" avrebbero folder_key diverse e Key non sarebbe idempotente.
//
// Applicata dopo il folding, la correzione porta il risultato nell'insieme
// dei target canonici, dove è un punto fisso. La copertura è verificata su
// tutti i code point in TestKeyCopreTuttoUnicode.
func foldCherokee(r rune) rune {
	switch {
	case r >= 0xAB70 && r <= 0xABBF:
		return r - 0xAB70 + 0x13A0
	case r >= 0x13F8 && r <= 0x13FD:
		return r - 0x13F8 + 0x13F0
	}
	return r
}

// FolderKey è la chiave di una directory a partire dal nome desiderato:
// è il valore delle colonne artists.folder_key e albums.folder_key.
func FolderKey(name string) string {
	return Key(Segment(name))
}

// PathKey unisce con "/" le chiavi dei singoli segmenti già finali.
func PathKey(finalSegments []string) string {
	keys := make([]string, len(finalSegments))
	for i, s := range finalSegments {
		keys[i] = Key(s)
	}
	return strings.Join(keys, "/")
}
