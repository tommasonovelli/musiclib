package http

import (
	"bytes"
	"encoding/json"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// §10.1 and NOTES.md N-148: the byte- and token-level rules.
func TestCheckJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		code  string // "" accepted
		field string // details.field, if any
	}{
		{"object", `{"a":1,"b":[1,{"c":null}],"d":"x"}`, "", ""},
		{"surrounding whitespace", " \n\t{\"a\":1}\r\n ", "", ""},
		{"empty object and array", `{"a":{},"b":[],"c":[{}]}`, "", ""},
		{"a valid surrogate pair", `{"a":"🎵"}`, "", ""},
		{"an escaped quote before a surrogate-looking text", `{"a":"\\ud800"}`, "", ""},
		{"escapes that are not surrogates", `{"a":"é\n\"\\\/"}`, "", ""},
		{"a literal replacement character", "{\"a\":\"\xef\xbf\xbd\"}", "", ""},
		{"keys equal only after case folding", `{"title":1,"Title":2}`, "", ""},

		{"empty", ``, CodeInvalidJSON, ""},
		{"blank", " \n ", CodeInvalidJSON, ""},
		{"truncated", `{"a":1`, CodeInvalidJSON, ""},
		{"syntax", `{"a":}`, CodeInvalidJSON, ""},
		{"two objects", `{"a":1}{"a":2}`, CodeInvalidJSON, ""},
		{"two objects with a space", `{"a":1} {"a":2}`, CodeInvalidJSON, ""},
		{"trailing scalar", `{"a":1} 1`, CodeInvalidJSON, ""},
		{"trailing garbage", `{"a":1}x`, CodeInvalidJSON, ""},
		{"trailing comma", `{"a":1,}`, CodeInvalidJSON, ""},
		{"byte order mark", "\xef\xbb\xbf{\"a\":1}", CodeInvalidJSON, ""},
		{"single quotes", `{'a':1}`, CodeInvalidJSON, ""},
		{"NaN", `{"a":NaN}`, CodeInvalidJSON, ""},

		{"invalid UTF-8 in a string", "{\"a\":\"\xff\"}", CodeInvalidUTF8, ""},
		{"invalid UTF-8 in a key", "{\"\xc3\":1}", CodeInvalidUTF8, ""},
		{"overlong encoding", "{\"a\":\"\xc0\xaf\"}", CodeInvalidUTF8, ""},
		{"encoded surrogate", "{\"a\":\"\xed\xa0\x80\"}", CodeInvalidUTF8, ""},
		{"lone high surrogate", `{"a":"\ud800"}`, CodeInvalidUTF8, ""},
		{"lone high surrogate at the end of a string", `{"a":"x\uD83C"}`, CodeInvalidUTF8, ""},
		{"high surrogate then a letter", `{"a":"\ud83cx"}`, CodeInvalidUTF8, ""},
		{"high surrogate then another escape", `{"a":"\ud83c\n"}`, CodeInvalidUTF8, ""},
		{"two high surrogates", `{"a":"\ud83c\ud83c"}`, CodeInvalidUTF8, ""},
		{"lone low surrogate", `{"a":"\udc00"}`, CodeInvalidUTF8, ""},
		{"lone surrogate in a key", `{"\udfff":1}`, CodeInvalidUTF8, ""},

		{"duplicate key", `{"a":1,"a":2}`, CodeDuplicateKey, "a"},
		{"duplicate key, same value", `{"a":1,"a":1}`, CodeDuplicateKey, "a"},
		{"duplicate key after escaping", `{"a":1,"a":2}`, CodeDuplicateKey, "a"},
		{"duplicate key nested", `{"a":{"b":1,"c":2,"b":3}}`, CodeDuplicateKey, "a.b"},
		{"duplicate key in an array", `{"tracks":[{"id":1},{"id":2,"id":3}]}`, CodeDuplicateKey, "tracks[1].id"},
		{"duplicate key deep", `{"x":[[{"y":[{"z":1,"z":1}]}]]}`, CodeDuplicateKey, "x[0][0].y[0].z"},
		{"duplicate key after a nested object", `{"a":{"a":1},"a":2}`, CodeDuplicateKey, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := checkJSON([]byte(tc.body))
			if tc.code == "" {
				if e != nil {
					t.Fatalf("checkJSON(%q) = %v", tc.body, e)
				}
				return
			}
			if e == nil || e.Code != tc.code {
				t.Fatalf("checkJSON(%q) = %v, want %s", tc.body, e, tc.code)
			}
			if e.Status != nethttp.StatusBadRequest {
				t.Fatalf("status %d", e.Status)
			}
			if got, _ := e.Details["field"].(string); got != tc.field {
				t.Fatalf("details.field %q, want %q", got, tc.field)
			}
		})
	}
}

// The schema-level rules: exact keys (not encoding/json's case-insensitive
// matching), every key required, types, null only where allowed,
// canonical ids.
func TestDecodeObject(t *testing.T) {
	keys := []string{"id", "name", "n", "opt", "flag", "items"}
	decode := func(body string) (object, *Error) {
		if e := checkJSON([]byte(body)); e != nil {
			t.Fatalf("checkJSON(%q): %v", body, e)
		}
		return decodeObject([]byte(body), "", keys...)
	}
	full := func(over map[string]string) string {
		fields := map[string]string{
			"id": `"01920000-0000-7000-8000-00000000abcd"`, "name": `"x"`, "n": `3`, "opt": `null`, "flag": `true`,
			"items": `[{"k":"v"}]`,
		}
		for k, v := range over {
			if v == "" {
				delete(fields, k)
			} else {
				fields[k] = v
			}
		}
		var b strings.Builder
		b.WriteString("{")
		first := true
		for _, k := range append(keys, "Name", "extra", "ſ") {
			v, ok := fields[k]
			if !ok {
				continue
			}
			if !first {
				b.WriteString(",")
			}
			first = false
			b.WriteString(`"` + k + `":` + v)
		}
		b.WriteString("}")
		return b.String()
	}

	o, e := decode(full(nil))
	if e != nil {
		t.Fatal(e)
	}
	id, e1 := o.ID("id")
	name, e2 := o.String("name")
	n, e3 := o.Int("n")
	opt, e4 := o.NullableString("opt")
	flag, e5 := o.Bool("flag")
	items, e6 := o.Objects("items", "k")
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil {
		t.Fatal(e1, e2, e3, e4, e5, e6)
	}
	if id.String() != "01920000-0000-7000-8000-00000000abcd" || name != "x" || n != 3 || opt != nil || !flag || len(items) != 1 {
		t.Fatalf("decoded %v %q %d %v %v %v", id, name, n, opt, flag, items)
	}
	if s, e := items[0].String("k"); e != nil || s != "v" {
		t.Fatalf("items[0].k = %q, %v", s, e)
	}

	for _, tc := range []struct {
		name  string
		body  string
		get   func(o object) *Error
		code  string
		field string
	}{
		{"not an object", `[1]`, nil, CodeInvalidField, ""},
		{"a string body", `"x"`, nil, CodeInvalidField, ""},
		{"null body", `null`, nil, CodeInvalidField, ""},
		{"unknown key", full(map[string]string{"extra": "1"}), nil, CodeUnknownField, "extra"},
		{"a key differing in case", full(map[string]string{"Name": `"y"`}), nil, CodeUnknownField, "Name"},
		{"a key folding to a known one", full(map[string]string{"ſ": `"y"`}), nil, CodeUnknownField, "ſ"},
		{"missing key", full(map[string]string{"n": ""}), nil, CodeMissingField, "n"},
		{"missing nullable key", full(map[string]string{"opt": ""}), nil, CodeMissingField, "opt"},
		{"string as int", full(map[string]string{"n": `"3"`}), func(o object) *Error { _, e := o.Int("n"); return e }, CodeInvalidField, "n"},
		{"fraction", full(map[string]string{"n": `3.5`}), func(o object) *Error { _, e := o.Int("n"); return e }, CodeInvalidField, "n"},
		{"exponent", full(map[string]string{"n": `1e2`}), func(o object) *Error { _, e := o.Int("n"); return e }, CodeInvalidField, "n"},
		{"int overflow", full(map[string]string{"n": `9223372036854775808`}), func(o object) *Error { _, e := o.Int("n"); return e }, CodeInvalidField, "n"},
		{"null int", full(map[string]string{"n": `null`}), func(o object) *Error { _, e := o.Int("n"); return e }, CodeInvalidField, "n"},
		{"null string", full(map[string]string{"name": `null`}), func(o object) *Error { _, e := o.String("name"); return e }, CodeInvalidField, "name"},
		{"number as string", full(map[string]string{"name": `1`}), func(o object) *Error { _, e := o.String("name"); return e }, CodeInvalidField, "name"},
		{"number as nullable string", full(map[string]string{"opt": `1`}), func(o object) *Error { _, e := o.NullableString("opt"); return e }, CodeInvalidField, "opt"},
		{"string as bool", full(map[string]string{"flag": `"true"`}), func(o object) *Error { _, e := o.Bool("flag"); return e }, CodeInvalidField, "flag"},
		{"null bool", full(map[string]string{"flag": `null`}), func(o object) *Error { _, e := o.Bool("flag"); return e }, CodeInvalidField, "flag"},
		{"uppercase id", full(map[string]string{"id": `"01920000-0000-7000-8000-00000000ABCD"`}), func(o object) *Error { _, e := o.ID("id"); return e }, CodeInvalidField, "id"},
		{"braced id", full(map[string]string{"id": `"{01920000-0000-7000-8000-00000000abcd}"`}), func(o object) *Error { _, e := o.ID("id"); return e }, CodeInvalidField, "id"},
		{"id without hyphens", full(map[string]string{"id": `"019200000000700080000000000abcd0"`}), func(o object) *Error { _, e := o.ID("id"); return e }, CodeInvalidField, "id"},
		{"urn id", full(map[string]string{"id": `"urn:uuid:01920000-0000-7000-8000-00000000abcd"`}), func(o object) *Error { _, e := o.ID("id"); return e }, CodeInvalidField, "id"},
		{"items not an array", full(map[string]string{"items": `{}`}), func(o object) *Error { _, e := o.Objects("items", "k"); return e }, CodeInvalidField, "items"},
		{"item not an object", full(map[string]string{"items": `[1]`}), func(o object) *Error { _, e := o.Objects("items", "k"); return e }, CodeInvalidField, "items[0]"},
		{"item with an unknown key", full(map[string]string{"items": `[{"k":"v"},{"k":"v","z":1}]`}), func(o object) *Error { _, e := o.Objects("items", "k"); return e }, CodeUnknownField, "items[1].z"},
		{"item missing a key", full(map[string]string{"items": `[{}]`}), func(o object) *Error { _, e := o.Objects("items", "k"); return e }, CodeMissingField, "items[0].k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, e := decode(tc.body)
			if e == nil && tc.get != nil {
				e = tc.get(o)
			}
			if e == nil || e.Code != tc.code || e.Status != nethttp.StatusUnprocessableEntity {
				t.Fatalf("got %v, want 422 %s", e, tc.code)
			}
			if got, _ := e.Details["field"].(string); got != tc.field {
				t.Fatalf("details.field %q, want %q", got, tc.field)
			}
		})
	}

	// The nullable forms accept null and their type.
	o, e = decode(full(map[string]string{"opt": `"y"`}))
	if e != nil {
		t.Fatal(e)
	}
	if s, e := o.NullableString("opt"); e != nil || s == nil || *s != "y" {
		t.Fatalf("NullableString = %v, %v", s, e)
	}
}

// readObject's HTTP-level rules: the media type and the size limit.
func TestReadObjectHeadersAndLimit(t *testing.T) {
	read := func(ct []string, body []byte) *Error {
		r := httptest.NewRequest(nethttp.MethodPost, "/api/artists", bytes.NewReader(body))
		for _, v := range ct {
			r.Header.Add("Content-Type", v)
		}
		_, e := readObject(httptest.NewRecorder(), r, "name")
		return e
	}
	ok := []byte(`{"name":"x"}`)
	for _, ct := range [][]string{{"application/json"}, {"application/json; charset=utf-8"}, {"Application/JSON; Charset=UTF-8"},
		{"application/json;"}} { // RFC 9110 §8.3.1: empty parameters are allowed
		if e := read(ct, ok); e != nil {
			t.Errorf("Content-Type %q: %v", ct, e)
		}
	}
	for _, ct := range [][]string{nil, {""}, {"text/plain"}, {"application/json; charset=latin1"},
		{"application/json; foo=bar"}, {"application/json", "application/json"}, {"application/x-www-form-urlencoded"},
		{"multipart/form-data; boundary=x"}, {"application/jsonx"}} {
		if e := read(ct, ok); e == nil || e.Status != nethttp.StatusUnsupportedMediaType || e.Code != CodeUnsupportedMediaType {
			t.Errorf("Content-Type %q: %v, want 415", ct, e)
		}
	}

	// Exactly 16 MiB is read; one byte more is 413, even without a
	// Content-Length announcing it.
	padded := func(n int) []byte {
		b := bytes.Repeat([]byte(" "), n)
		copy(b, ok)
		return b
	}
	if e := read([]string{"application/json"}, padded(MaxBodyBytes)); e != nil {
		t.Fatalf("16 MiB: %v", e)
	}
	r := httptest.NewRequest(nethttp.MethodPost, "/api/artists", struct{ *bytes.Reader }{bytes.NewReader(padded(MaxBodyBytes + 1))})
	r.ContentLength = -1
	r.Header.Set("Content-Type", "application/json")
	if _, e := readObject(httptest.NewRecorder(), r, "name"); e == nil || e.Status != nethttp.StatusRequestEntityTooLarge || e.Code != CodeBodyTooLarge {
		t.Fatalf("16 MiB + 1 without Content-Length: %v", e)
	}
	if e := read([]string{"application/json"}, padded(MaxBodyBytes+1)); e == nil || e.Status != nethttp.StatusRequestEntityTooLarge {
		t.Fatalf("16 MiB + 1: %v", e)
	}
}

// Whatever checkJSON accepts is exactly one valid JSON value in valid
// UTF-8, and decoding it keeps every string as sent: no replacement
// character appears that the body did not contain.
func FuzzCheckJSON(f *testing.F) {
	for _, s := range []string{`{"a":1}`, `{"a":"🎵"}`, `{"a":"\ud800"}`, `{"a":1,"a":2}`, `{} {}`,
		`{"a":[{"b":"\\"}]}`, "{\"a\":\"\xff\"}", `[1,2]`, `"x"`, `{"a":"\"\\u"}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if checkJSON(b) != nil {
			return
		}
		if !json.Valid(b) || !utf8.Valid(b) {
			t.Fatalf("accepted %q", b)
		}
		var v any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber() // numbers beyond float64 are valid JSON
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("accepted %q: %v", b, err)
		}
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Count(out, []byte("�")) > bytes.Count(b, []byte("�"))+bytes.Count(bytes.ToLower(b), []byte(`�`)) {
			t.Fatalf("accepted %q, which decodes with a replacement character", b)
		}
	})
}
