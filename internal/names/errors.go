package names

import (
	"errors"
	"fmt"
)

// Normalization error codes. They are stable: the API exposes them inside
// the {code, message, details} body (DESIGN.md §10.1).
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

// Error is the package's typed error: a stable code plus a human-readable
// message. It never contains the host's absolute path.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Code returns the code of err if it is an *Error, and "" otherwise.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
