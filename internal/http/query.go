package http

import (
	nethttp "net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
)

// queryParams parses the query string of a GET strictly, as N-148 reads
// JSON keys and N-172 the upload's query: url.ParseQuery (so ";" and bad
// escapes are refused), each key of keys at most once, no other key. As in
// any query string, "+" is a space. Absent keys are absent from the map.
func queryParams(r *nethttp.Request, keys ...string) (map[string]string, *Error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, newError(nethttp.StatusUnprocessableEntity, CodeInvalidField,
			"the query string is not valid URL encoding (key=value pairs joined by &)")
	}
	unknown := make([]string, 0, len(q))
	for k := range q {
		if !slices.Contains(keys, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, newError(nethttp.StatusUnprocessableEntity, CodeUnknownField, "unknown query parameter %q", unknown[0]).
			with("field", unknown[0])
	}
	out := make(map[string]string, len(q))
	for _, k := range keys {
		switch vs := q[k]; len(vs) {
		case 0:
		case 1:
			out[k] = vs[0]
		default:
			return nil, invalidField(k, "must be given once")
		}
	}
	return out, nil
}

// pageLimit reads the query parameter limit: absent is
// catalog.DefaultPageSize; otherwise a plain decimal 1..catalog.MaxPageSize
// (§10.2: "paginazione 50 max 200"). A larger value is refused, not
// clamped: a client asking for 500 would otherwise believe it has
// everything (NOTES.md N-192).
func pageLimit(q map[string]string) (int, *Error) {
	v, ok := q["limit"]
	if !ok {
		return catalog.DefaultPageSize, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || strconv.Itoa(n) != v || n < 1 || n > catalog.MaxPageSize {
		return 0, invalidField("limit", "must be an integer from 1 to 200").with("limit", catalog.MaxPageSize)
	}
	return n, nil
}

// queryID reads an optional id parameter in canonical form (parseID).
func queryID(q map[string]string, key string) (uuid.UUID, *Error) {
	v, ok := q[key]
	if !ok {
		return uuid.Nil, nil
	}
	id, ok := parseID(v)
	if !ok {
		return uuid.Nil, invalidField(key, "must be an id in canonical form (lowercase, 8-4-4-4-12)")
	}
	return id, nil
}
