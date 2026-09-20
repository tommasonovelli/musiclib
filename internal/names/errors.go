package names

import (
	"errors"
	"fmt"
)

// Codici di errore della normalizzazione. Sono stabili: l'API li espone
// dentro il corpo {code, message, details} (DESIGN.md §10.1).
const (
	CodeTextEmpty       = "text_empty"
	CodeTextTooLong     = "text_too_long"
	CodeTextControlChar = "text_control_char"
	CodeInvalidUTF8     = "invalid_utf8"

	CodePathEmpty        = "path_empty"
	CodePathAbsolute     = "path_absolute"
	CodePathDotSegment   = "path_dot_segment"
	CodePathEmptySegment = "path_empty_segment"
	CodePathNulByte      = "path_nul_byte"
	CodePathTooDeep      = "path_too_deep"
	CodePathTooLong      = "path_too_long"
)

// Error è l'errore tipizzato del pacchetto: un codice stabile più un
// messaggio leggibile. Non contiene mai il percorso assoluto dell'host.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Code restituisce il codice di err se è un *Error, altrimenti "".
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
