package names

import (
	"strings"
	"unicode/utf8"
)

// SanitizedPath è il risultato della sanitizzazione di un percorso relativo
// destinato all'output.
type SanitizedPath struct {
	// Segments sono i segmenti sanitizzati, nell'ordine originale.
	Segments []string
	// Path sono i segmenti uniti da "/".
	Path string
	// Key è la chiave di confronto del percorso: le chiavi dei segmenti
	// unite da "/". È il valore della colonna attachments.path_key.
	Key string
}

// SplitRelPath valida un percorso relativo e ne restituisce i segmenti
// esattamente come sono sul disco, senza sanitizzarli (DESIGN.md §5.2:
// source_rel, source_path e root_rel si validano ma si aprono con il nome
// originale).
//
// Rifiuta percorsi assoluti, ".", "..", segmenti vuoti, UTF-8 non valido,
// byte NUL e profondità oltre MaxPathDepth. La stringa vuota è un errore:
// per la root di /import si usa SplitRelPathOrRoot.
func SplitRelPath(p string) ([]string, error) {
	if p == "" {
		return nil, errf(CodePathEmpty, "il percorso relativo è vuoto")
	}
	return splitRelPath(p)
}

// SplitRelPathOrRoot è SplitRelPath ma ammette la stringa vuota, che indica
// la root di /import e restituisce zero segmenti.
func SplitRelPathOrRoot(p string) ([]string, error) {
	if p == "" {
		return []string{}, nil
	}
	return splitRelPath(p)
}

func splitRelPath(p string) ([]string, error) {
	if !utf8.ValidString(p) {
		return nil, errf(CodeInvalidUTF8, "il percorso non è UTF-8 valido")
	}
	if strings.IndexByte(p, 0) >= 0 {
		return nil, errf(CodePathNulByte, "il percorso contiene un byte NUL")
	}
	if strings.HasPrefix(p, "/") {
		return nil, errf(CodePathAbsolute, "il percorso è assoluto")
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		switch s {
		case "":
			return nil, errf(CodePathEmptySegment, "il percorso contiene un segmento vuoto")
		case ".", "..":
			return nil, errf(CodePathDotSegment, "il percorso contiene il segmento %q", s)
		}
	}
	if len(segs) > MaxPathDepth {
		return nil, errf(CodePathTooDeep,
			"il percorso ha %d livelli, il massimo è %d", len(segs), MaxPathDepth)
	}
	return segs, nil
}

// SanitizeRelFilePath valida un percorso relativo e lo sanitizza segmento
// per segmento: l'ultimo segmento è trattato come nome di file (estensione
// preservata), quelli precedenti come directory.
//
// È il percorso degli allegati: rel_path conserva il valore originale o
// scelto dall'utente, il piano di render usa questo risultato.
func SanitizeRelFilePath(p string) (SanitizedPath, error) {
	segs, err := SplitRelPath(p)
	if err != nil {
		return SanitizedPath{}, err
	}
	out := make([]string, len(segs))
	last := len(segs) - 1
	for i, s := range segs {
		if i == last {
			out[i] = FileSegment(s)
		} else {
			out[i] = Segment(s)
		}
	}
	joined := strings.Join(out, "/")
	if len(joined) > MaxPathBytes {
		return SanitizedPath{}, errf(CodePathTooLong,
			"il percorso sanitizzato occupa %d byte, il massimo è %d", len(joined), MaxPathBytes)
	}
	return SanitizedPath{Segments: out, Path: joined, Key: PathKey(out)}, nil
}
