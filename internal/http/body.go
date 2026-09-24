package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	nethttp "net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// MaxBodyBytes is the largest JSON body accepted (§10.1: 16 MiB); one
// byte more is 413.
const MaxBodyBytes = 16 << 20

// readObject reads the body of a request that carries one JSON object and
// checks it strictly (§10.1, NOTES.md N-148):
//
//   - Content-Type application/json, with no parameter but charset=utf-8
//     (415 otherwise);
//   - at most MaxBodyBytes (413);
//   - valid UTF-8, and no lone UTF-16 surrogate in a \u escape, which
//     encoding/json would silently turn into U+FFFD (400 invalid_utf8);
//   - exactly one well-formed JSON value, nothing after it but whitespace
//     (400 invalid_json);
//   - no object with the same key twice, at any depth (400
//     duplicate_key): encoding/json keeps the last one silently;
//   - an object at the top with exactly keys (422 invalid_field,
//     unknown_field, missing_field; decodeObject).
//
// The keys of the result are compared exactly by the field decoders: the
// case-insensitive matching of encoding/json is never used.
func readObject(w nethttp.ResponseWriter, r *nethttp.Request, keys ...string) (object, *Error) {
	if e := checkContentType(r.Header); e != nil {
		return object{}, e
	}
	b, e := readBody(w, r)
	if e != nil {
		return object{}, e
	}
	if e := checkJSON(b); e != nil {
		return object{}, e
	}
	return decodeObject(b, "", keys...)
}

// refuseBody is the body rule of the endpoints that take none (DELETE,
// restore, render): an empty body only.
func refuseBody(w nethttp.ResponseWriter, r *nethttp.Request) *Error {
	b, e := readBody(w, r)
	if e != nil {
		return e
	}
	if len(b) > 0 {
		return newError(nethttp.StatusBadRequest, CodeBodyNotAllowed, "this request takes no body")
	}
	return nil
}

func checkContentType(h nethttp.Header) *Error {
	refuse := newError(nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
		"the body must be Content-Type: application/json (charset utf-8)")
	values := h.Values("Content-Type")
	if len(values) != 1 {
		return refuse
	}
	mt, params, err := mime.ParseMediaType(values[0])
	if err != nil || mt != "application/json" {
		return refuse
	}
	for k, v := range params {
		if k != "charset" || !strings.EqualFold(v, "utf-8") {
			return refuse
		}
	}
	return nil
}

// readBody reads the whole body, at most MaxBodyBytes.
func readBody(w nethttp.ResponseWriter, r *nethttp.Request) ([]byte, *Error) {
	tooLarge := newError(nethttp.StatusRequestEntityTooLarge, CodeBodyTooLarge,
		"the body is larger than %d bytes (16 MiB)", MaxBodyBytes).with("limit", MaxBodyBytes)
	if r.ContentLength > MaxBodyBytes {
		return nil, tooLarge
	}
	b, err := io.ReadAll(nethttp.MaxBytesReader(w, r.Body, MaxBodyBytes))
	var mbe *nethttp.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		return nil, tooLarge
	case err != nil:
		return nil, newError(nethttp.StatusBadRequest, CodeInvalidJSON, "the body could not be read")
	}
	return b, nil
}

// checkJSON applies the byte-level and token-level rules of readObject.
func checkJSON(b []byte) *Error {
	if !utf8.Valid(b) {
		return newError(nethttp.StatusBadRequest, CodeInvalidUTF8, "the body is not valid UTF-8")
	}
	if e := scanTokens(b); e != nil {
		return e
	}
	return checkSurrogates(b)
}

// frame is an open object or array during scanTokens.
type frame struct {
	object  bool
	path    string
	keys    map[string]bool
	wantKey bool   // object: the next token is a key or '}'
	key     string // object: the key of the value being read
	index   int    // array: the index of the value being read
}

// child is the path of the value being read inside f.
func (f *frame) child() string {
	if f.object {
		return joinPath(f.path, f.key)
	}
	return f.path + "[" + strconv.Itoa(f.index) + "]"
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// scanTokens walks b with the tokenizer of encoding/json: syntax, exactly
// one top-level value, and unique keys in every object. Keys are compared
// after unescaping, so "a" and "a" are the same key.
func scanTokens(b []byte) *Error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var stack []*frame
	done := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if !done {
				return newError(nethttp.StatusBadRequest, CodeInvalidJSON, "the body is empty or ends inside a JSON value")
			}
			return nil
		}
		if err != nil {
			return newError(nethttp.StatusBadRequest, CodeInvalidJSON, "the body is not valid JSON near byte %d",
				dec.InputOffset())
		}
		if done {
			return newError(nethttp.StatusBadRequest, CodeInvalidJSON, "the body holds more than one JSON value")
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.wantKey {
			if k, ok := tok.(string); ok {
				if top.keys[k] {
					return newError(nethttp.StatusBadRequest, CodeDuplicateKey, "the key %q appears twice in the same object",
						joinPath(top.path, k)).with("field", joinPath(top.path, k))
				}
				top.keys[k], top.key, top.wantKey = true, k, false
				continue
			}
			// The only other token here is the '}' of an empty object or
			// after a value.
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			path := ""
			if top != nil {
				path = top.child()
			}
			f := &frame{object: tok == json.Delim('{'), path: path, wantKey: true}
			if f.object {
				f.keys = map[string]bool{}
			}
			stack = append(stack, f)
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
		// A value just ended: a scalar, or the object or array closed above.
		if len(stack) == 0 {
			done = true
			continue
		}
		if parent := stack[len(stack)-1]; parent.object {
			parent.wantKey = true
		} else {
			parent.index++
		}
	}
}

// checkSurrogates refuses a \u escape that is a lone UTF-16 surrogate: a
// high surrogate not followed by a low one, or a low one alone. b is
// already known to be valid JSON, so strings are well delimited.
func checkSurrogates(b []byte) *Error {
	refuse := newError(nethttp.StatusBadRequest, CodeInvalidUTF8, "the body escapes a lone UTF-16 surrogate")
	inString := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !inString {
			inString = c == '"'
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if b[i+1] != 'u' {
				i++ // a one-character escape, possibly \" or \\
				continue
			}
			r := hex4(b[i+2 : i+6])
			i += 5
			switch {
			case r >= 0xdc00 && r <= 0xdfff:
				return refuse
			case r >= 0xd800 && r <= 0xdbff:
				if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
					return refuse
				}
				if lo := hex4(b[i+3 : i+7]); lo < 0xdc00 || lo > 0xdfff {
					return refuse
				}
				i += 6
			}
		}
	}
	return nil
}

// hex4 decodes four hexadecimal digits, already validated by the tokenizer.
func hex4(h []byte) rune {
	var r rune
	for _, c := range h {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		default:
			r |= rune(c-'A') + 10
		}
	}
	return r
}

// object is a JSON object already checked by checkJSON, with its fields by
// exact key.
type object struct {
	path   string
	fields map[string]json.RawMessage
}

// decodeObject decodes raw, a JSON value at path, as an object whose keys
// are exactly keys: a key outside them is 422 unknown_field, a missing one
// 422 missing_field (the first in sorted, respectively declared, order, so
// that the answer is deterministic). Every key is required: a field that
// may be null is sent as null, never omitted, so that a save never clears
// a value by accident.
func decodeObject(raw []byte, path string, keys ...string) (object, *Error) {
	o := object{path: path}
	if !isKind(raw, '{') {
		return o, invalidField(path, "must be a JSON object")
	}
	if err := json.Unmarshal(raw, &o.fields); err != nil {
		return o, invalidField(path, "must be a JSON object")
	}
	var unknown []string
	for k := range o.fields {
		if !slices.Contains(keys, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		f := joinPath(path, unknown[0])
		return o, newError(nethttp.StatusUnprocessableEntity, CodeUnknownField, "unknown field %q", f).with("field", f)
	}
	for _, k := range keys {
		if _, ok := o.fields[k]; !ok {
			f := joinPath(path, k)
			return o, newError(nethttp.StatusUnprocessableEntity, CodeMissingField, "the field %q is required", f).
				with("field", f)
		}
	}
	return o, nil
}

// isKind reports whether the first non-blank byte of raw is c.
func isKind(raw []byte, c byte) bool {
	t := bytes.TrimLeft(raw, " \t\r\n")
	return len(t) > 0 && t[0] == c
}

func isNull(raw []byte) bool { return string(bytes.TrimSpace(raw)) == "null" }

func invalidField(path, what string) *Error {
	f := path
	if f == "" {
		return newError(nethttp.StatusUnprocessableEntity, CodeInvalidField, "the body %s", what)
	}
	return newError(nethttp.StatusUnprocessableEntity, CodeInvalidField, "%q %s", f, what).with("field", f)
}

// field decodes the value of key into v, which must not be null.
func (o object) field(key, what string, v any) *Error {
	raw := o.fields[key]
	if isNull(raw) || json.Unmarshal(raw, v) != nil {
		return invalidField(joinPath(o.path, key), what)
	}
	return nil
}

// String is a required string.
func (o object) String(key string) (string, *Error) {
	var s string
	if !isKind(o.fields[key], '"') {
		return "", invalidField(joinPath(o.path, key), "must be a string")
	}
	return s, o.field(key, "must be a string", &s)
}

// NullableString is a string or null.
func (o object) NullableString(key string) (*string, *Error) {
	if isNull(o.fields[key]) {
		return nil, nil
	}
	s, e := o.String(key)
	if e != nil {
		return nil, invalidField(joinPath(o.path, key), "must be a string or null")
	}
	return &s, nil
}

// Int is a required integer: no fraction, no exponent, within int64.
func (o object) Int(key string) (int, *Error) {
	var n int
	return n, o.field(key, "must be an integer", &n)
}

// NullableInt is an integer or null.
func (o object) NullableInt(key string) (*int, *Error) {
	if isNull(o.fields[key]) {
		return nil, nil
	}
	n, e := o.Int(key)
	if e != nil {
		return nil, invalidField(joinPath(o.path, key), "must be an integer or null")
	}
	return &n, nil
}

// Bool is a required boolean.
func (o object) Bool(key string) (bool, *Error) {
	var b bool
	return b, o.field(key, "must be true or false", &b)
}

// ID is a required id in canonical form (parseID).
func (o object) ID(key string) (uuid.UUID, *Error) {
	s, e := o.String(key)
	if e != nil {
		return uuid.Nil, invalidField(joinPath(o.path, key), "must be an id")
	}
	id, ok := parseID(s)
	if !ok {
		return uuid.Nil, invalidField(joinPath(o.path, key), "must be an id in canonical form (lowercase, 8-4-4-4-12)")
	}
	return id, nil
}

// Objects is a required array of objects, each with exactly keys.
func (o object) Objects(key string, keys ...string) ([]object, *Error) {
	path := joinPath(o.path, key)
	var raws []json.RawMessage
	if !isKind(o.fields[key], '[') || json.Unmarshal(o.fields[key], &raws) != nil {
		return nil, invalidField(path, "must be an array")
	}
	out := make([]object, len(raws))
	for i, raw := range raws {
		var e *Error
		if out[i], e = decodeObject(raw, fmt.Sprintf("%s[%d]", path, i), keys...); e != nil {
			return nil, e
		}
	}
	return out, nil
}

// parseID accepts an id only in the canonical form this API writes: 36
// characters, lowercase hexadecimal, hyphens at 8-4-4-4-12. The other
// spellings uuid.Parse admits (braces, urn:uuid:, no hyphens, uppercase)
// are refused, so that one id has one spelling in requests and responses.
func parseID(s string) (uuid.UUID, bool) {
	if len(s) != 36 {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s {
		return uuid.Nil, false
	}
	return id, true
}
