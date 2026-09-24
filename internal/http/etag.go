package http

import (
	nethttp "net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
)

// The kinds of catalog resource that have an ETag (§10.1). Covers,
// attachments, tracks and lyrics have none: their operations require the
// album's (§10.2).
const (
	KindAlbum  = "album"
	KindArtist = "artist"
)

// ETag is the strong entity tag of a catalog representation (§10.1):
// "album:<uuid>:<revision>" or "artist:<uuid>:<revision>", quotes
// included. It changes exactly when the revision does.
func ETag(kind string, id uuid.UUID, revision int64) string {
	return `"` + kind + ":" + id.String() + ":" + strconv.FormatInt(revision, 10) + `"`
}

// noMatch is the revision handed to the catalog for an If-Match that is
// present and well formed but names no current representation of the
// resource: only weak tags, or tags of other resources. Revisions are
// positive, so the catalog's comparison, made in the transaction of the
// change after the resource is read, answers 412 with the current
// revision; a resource that does not exist is still 404 (RFC 9110 §13.2.1:
// preconditions are not evaluated when the response would not be 2xx or
// 412).
const noMatch int64 = -1

// ifMatch is §10.1's precondition: the revision seen by the client, from
// the If-Match header, for the resource (kind, id). For the sub-resources
// of an album (cover, attachments, tracks, lyrics; §10.2) the caller asks
// for the album's. The rules (NOTES.md N-147):
//
//   - no If-Match (or an empty list): 428 precondition_required;
//   - "*": 428 as well, since it would allow a blind last write (§10.1);
//   - not a list of entity tags (RFC 9110 §8.8.3, §13.1.1): 400
//     invalid_if_match;
//   - exactly one strong tag naming this resource, among any others: its
//     revision, compared by the catalog in the transaction;
//   - none (weak tags never match under the strong comparison of If-Match;
//     other resources' tags, malformed revisions): noMatch, hence 412;
//   - more than one strong tag naming this resource: 400
//     invalid_if_match, since the catalog compares one revision.
func ifMatch(h nethttp.Header, kind string, id uuid.UUID) (int64, *Error) {
	lines := h.Values("If-Match")
	if len(lines) == 0 {
		return 0, preconditionRequired(kind, id, "a change requires If-Match with the ETag of the %s last read")
	}
	// Field lines of a list combine with commas (RFC 9110 §5.3).
	tags, star, ok := parseETags(strings.Join(lines, ","))
	switch {
	case !ok:
		return 0, newError(nethttp.StatusBadRequest, CodeInvalidIfMatch,
			"If-Match is not a list of entity tags (RFC 9110 §8.8.3)")
	case star:
		return 0, preconditionRequired(kind, id, "If-Match: * is not accepted: send the ETag of the %s last read")
	case len(tags) == 0:
		return 0, preconditionRequired(kind, id, "If-Match is empty: send the ETag of the %s last read")
	}
	var found []int64
	for _, t := range tags {
		if rev, ok := revisionOf(t, kind, id); ok {
			found = append(found, rev)
		}
	}
	switch len(found) {
	case 0:
		return noMatch, nil
	case 1:
		return found[0], nil
	default:
		return 0, newError(nethttp.StatusBadRequest, CodeInvalidIfMatch,
			"If-Match names %s %s more than once", kind, id)
	}
}

func preconditionRequired(kind string, id uuid.UUID, format string) *Error {
	return newError(nethttp.StatusPreconditionRequired, catalog.CodePreconditionRequired, format, kind).
		with(kind+"_id", id.String())
}

// entityTag is one element of an If-Match list: its opaque tag without
// the quotes, and whether it is weak.
type entityTag struct {
	weak   bool
	opaque string
}

// parseETags parses the value of If-Match: "*", or a comma-separated list
// of entity tags with optional whitespace around the commas and empty
// elements (RFC 9110 §5.6.1, §8.8.3):
//
//	entity-tag = [ %s"W/" ] DQUOTE *etagc DQUOTE
//	etagc      = %x21 / %x23-7E / %x80-FF
//
// ok is false for anything else, "*" mixed with tags included.
func parseETags(s string) (tags []entityTag, star, ok bool) {
	for _, el := range strings.Split(s, ",") {
		el = strings.Trim(el, " \t")
		switch {
		case el == "":
			continue
		case el == "*":
			star = true
			continue
		}
		var t entityTag
		if strings.HasPrefix(el, "W/") {
			t.weak, el = true, el[2:]
		}
		if len(el) < 2 || el[0] != '"' || el[len(el)-1] != '"' {
			return nil, false, false
		}
		t.opaque = el[1 : len(el)-1]
		for i := 0; i < len(t.opaque); i++ {
			if c := t.opaque[i]; c < 0x21 || c == '"' || c == 0x7f {
				return nil, false, false
			}
		}
		tags = append(tags, t)
	}
	if star && len(tags) > 0 {
		return nil, false, false
	}
	return tags, star, true
}

// revisionOf returns the revision of t if t is a strong tag of the form
// ETag(kind, id, revision): the canonical id, a positive decimal revision
// without leading zeros.
func revisionOf(t entityTag, kind string, id uuid.UUID) (int64, bool) {
	if t.weak {
		return 0, false
	}
	rest, ok := strings.CutPrefix(t.opaque, kind+":"+id.String()+":")
	if !ok {
		return 0, false
	}
	rev, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || rev <= 0 || strconv.FormatInt(rev, 10) != rest {
		return 0, false
	}
	return rev, true
}
