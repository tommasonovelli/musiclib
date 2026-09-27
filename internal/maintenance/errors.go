package maintenance

import "fmt"

// Error carries a stable code and distinguishes precondition refusals from
// attempted operations that failed. Err preserves the underlying cause.
type Error struct {
	Code    string
	Message string
	Refusal bool
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}
func (e *Error) Unwrap() error          { return e.Err }
func refuse(code, message string) error { return &Error{Code: code, Message: message, Refusal: true} }
func fail(code, message string, err error) error {
	return &Error{Code: code, Message: message, Err: err}
}
