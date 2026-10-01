package http

import (
	"encoding/json"
	"errors"
	"fmt"
	nethttp "net/http"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

// Error codes of the HTTP layer (§10.1). They are stable, like the domain
// codes they sit next to: an API error is {code, message, details}, and
// its code is either one of these or the code of the domain error it
// translates (catalog, names, store). The status of each code is in
// statusOf and in NOTES.md N-149.
const (
	// The request does not reach the API (§10.4).
	CodeHostNotAllowed        = "host_not_allowed"        // 421
	CodeOriginNotAllowed      = "origin_not_allowed"      // 403
	CodeRequestHeaderRequired = "request_header_required" // 403
	// CodeLoginRequired: no live session; sign in at /login. The 401
	// carries no WWW-Authenticate: a cookie session has no standard
	// challenge, and Basic would open the browser's own password dialog.
	CodeLoginRequired = "login_required" // 401

	// The API cannot serve now (§10.1, §11.1): boot, recovery, a suspended
	// publication (the code of publish), shutdown.
	CodeNotReady     = "not_ready"     // 503
	CodeShuttingDown = "shutting_down" // 503

	// Routing.
	CodeNotFound         = "not_found"          // 404
	CodeMethodNotAllowed = "method_not_allowed" // 405

	// The request's body and headers (§10.1). 400: the body is not exactly
	// one well-formed JSON value in UTF-8. 422: it is, but it does not fit
	// the endpoint's schema.
	CodeUnsupportedMediaType = "unsupported_media_type" // 415
	CodeBodyTooLarge         = "body_too_large"         // 413
	CodeBodyNotAllowed       = "body_not_allowed"       // 400
	CodeInvalidJSON          = "invalid_json"           // 400
	CodeInvalidUTF8          = names.CodeInvalidUTF8    // 400
	CodeDuplicateKey         = "duplicate_key"          // 400
	CodeUnknownField         = "unknown_field"          // 422
	CodeMissingField         = "missing_field"          // 422
	CodeInvalidField         = "invalid_field"          // 422
	CodeDuplicateID          = "duplicate_id"           // 422
	CodeInvalidIfMatch       = "invalid_if_match"       // 400

	// CodeInternal is anything unexpected. Its message says nothing more;
	// the log has the cause.
	CodeInternal = "internal" // 500

	// CodeInvalidConfig is a constructor refusal (New), never a response.
	CodeInvalidConfig = "http_invalid_config"
)

// Error is an API error: the status and the {code, message, details} body
// of §10.1. Details is never nil in a response. Nothing in it is an
// absolute path, a query or a tool's output.
type Error struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func newError(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// with adds a detail and returns e.
func (e *Error) with(key string, value any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

// statusOf maps the domain codes the endpoints can return to their status
// (§10.1): 409 for name and reservation conflicts, 422 for invalid
// metadata or content, 428 without a precondition, 412 for a stale
// revision, 503 when the database is unavailable. A code missing here is
// unexpected: 500 CodeInternal.
var statusOf = map[string]int{
	catalog.CodePreconditionRequired: nethttp.StatusPreconditionRequired,
	catalog.CodePreconditionFailed:   nethttp.StatusPreconditionFailed,

	catalog.CodeAlbumNotFound:      nethttp.StatusNotFound,
	catalog.CodeArtistNotFound:     nethttp.StatusNotFound,
	catalog.CodeAttachmentNotFound: nethttp.StatusNotFound,
	catalog.CodeTrackNotFound:      nethttp.StatusNotFound,

	catalog.CodePathReserved:         nethttp.StatusConflict,
	catalog.CodeAlbumFolderConflict:  nethttp.StatusConflict,
	catalog.CodeArtistFolderConflict: nethttp.StatusConflict,
	catalog.CodeArtistExists:         nethttp.StatusConflict,
	// Two attachments whose output paths collide are a conflict of names
	// (§10.1), found when an attachment is uploaded (N-172).
	catalog.CodeAttachmentCollision: nethttp.StatusConflict,
	// A file uploaded as a track that is already one of the album's: two
	// tracks are never merged.
	catalog.CodeTrackExists: nethttp.StatusConflict,
	// Tracks moved to an album in the trash: restore it first.
	catalog.CodeAlbumTrashed: nethttp.StatusConflict,
	// The imports and the queue (round 16, N-193, N-195).
	catalog.CodeImportBatchNotFound: nethttp.StatusNotFound,
	catalog.CodeJobNotFound:         nethttp.StatusNotFound,
	catalog.CodeImportBatchConflict: nethttp.StatusConflict,
	jobs.CodeNotRetryable:           nethttp.StatusConflict,
	jobs.CodeNotDismissable:         nethttp.StatusConflict,
	jobs.CodeInProgress:             nethttp.StatusConflict,
	jobs.CodeOverridesNotAllowed:    nethttp.StatusUnprocessableEntity,
	jobs.CodeInvalidOverrides:       nethttp.StatusUnprocessableEntity,

	names.CodeTextEmpty:              nethttp.StatusUnprocessableEntity,
	names.CodeTextTooLong:            nethttp.StatusUnprocessableEntity,
	names.CodeTextControlChar:        nethttp.StatusUnprocessableEntity,
	names.CodeInvalidUTF8:            nethttp.StatusUnprocessableEntity,
	catalog.CodeInvalidYear:          nethttp.StatusUnprocessableEntity,
	catalog.CodeInvalidDisc:          nethttp.StatusUnprocessableEntity,
	catalog.CodeInvalidTrackNumber:   nethttp.StatusUnprocessableEntity,
	catalog.CodeDuplicateTrackNumber: nethttp.StatusUnprocessableEntity,
	catalog.CodeTrackListMismatch:    nethttp.StatusUnprocessableEntity,
	catalog.CodeNoTracks:             nethttp.StatusUnprocessableEntity,
	catalog.CodeTooManyFiles:         nethttp.StatusUnprocessableEntity,
	catalog.CodeSameAlbum:            nethttp.StatusUnprocessableEntity,
	catalog.CodeInvalidCover:         nethttp.StatusUnprocessableEntity,
	catalog.CodeGenreNotWritable:     nethttp.StatusUnprocessableEntity,
	catalog.CodeLyricsAssociation:    nethttp.StatusUnprocessableEntity,
	catalog.CodeInvalidBlobFormat:    nethttp.StatusUnprocessableEntity,
	catalog.CodeCoverNotEmbeddable:   nethttp.StatusUnprocessableEntity,
	catalog.CodeInvalidLyrics:        nethttp.StatusUnprocessableEntity,
	names.CodePathEmpty:              nethttp.StatusUnprocessableEntity,
	names.CodePathAbsolute:           nethttp.StatusUnprocessableEntity,
	names.CodePathDotSegment:         nethttp.StatusUnprocessableEntity,
	names.CodePathEmptySegment:       nethttp.StatusUnprocessableEntity,
	names.CodePathNulByte:            nethttp.StatusUnprocessableEntity,
	names.CodePathTooDeep:            nethttp.StatusUnprocessableEntity,
	names.CodePathTooLong:            nethttp.StatusUnprocessableEntity,
	store.CodeConnectionLost:         nethttp.StatusServiceUnavailable,
	store.CodeCommitUncertain:        nethttp.StatusServiceUnavailable,
	store.CodeRetriesExhausted:       nethttp.StatusServiceUnavailable,
	store.CodeCanceled:               nethttp.StatusServiceUnavailable,
	// catalog_db and jobs' job_db are deliberately absent: an unexpected
	// database failure that is not fatal is 500 internal, with no text of
	// its cause (N-149).
}

// storeMessages are the messages of the database codes: the error's own
// text carries pgx details, which are not for the client (§10.1).
var storeMessages = map[string]string{
	store.CodeConnectionLost: "the database is not reachable; the server restarts",
	store.CodeCommitUncertain: "the database did not confirm the outcome of the change; the server restarts: " +
		"reload before trying again",
	store.CodeRetriesExhausted: "the database is busy: try again",
	store.CodeCanceled:         "the request was cancelled",
}

// translate turns an error of the catalog (or of what it wraps) into an
// API error. A fatal store error (§6.4) wins over the domain error it may
// wrap, as catalog.Code decides. Messages come from the typed errors, never
// from err.Error(): the causes they wrap (pgx, names) are logged, not sent.
func translate(err error) *Error {
	code := catalog.Code(err)
	if msg, ok := storeMessages[code]; ok {
		return &Error{Status: statusOf[code], Code: code, Message: msg}
	}
	status := statusOf[code]
	if status == 0 {
		return internalError()
	}
	ce, ok := catalog.AsError(err)
	if !ok {
		// The queue's own refusals (a retry, N-195) carry their message.
		var je *jobs.Error
		if errors.As(err, &je) && je.Code == code {
			return &Error{Status: status, Code: code, Message: je.Msg}
		}
		return internalError()
	}
	e := &Error{Status: status, Code: code, Message: ce.Message, Details: detailsOf(ce.Details)}
	var ne *names.Error
	if errors.As(ce.Err, &ne) {
		// A text error names the field in Message and the reason in the
		// names error: "album title: text_empty ...".
		e.Message = ce.Message + ": " + ne.Message
	}
	return e
}

const internalMessage = "unexpected error; see the server log"

func internalError() *Error {
	return newError(nethttp.StatusInternalServerError, CodeInternal, internalMessage)
}

// detailsOf is the details object of a catalog error: only the parts it
// has, under fixed keys.
func detailsOf(d catalog.Details) map[string]any {
	out := map[string]any{}
	if d.AlbumID != uuid.Nil {
		out["album_id"] = d.AlbumID.String()
	}
	if d.ArtistID != uuid.Nil {
		out["artist_id"] = d.ArtistID.String()
	}
	if d.TrackID != uuid.Nil {
		out["track_id"] = d.TrackID.String()
	}
	if d.Path != "" {
		out["path"] = d.Path
	}
	if len(d.Names) > 0 {
		out["names"] = d.Names
	}
	if d.Revision > 0 {
		out["revision"] = d.Revision
	}
	return out
}

// errorBody is the JSON of an Error (§10.1).
type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// writeError writes e. Maps are encoded with sorted keys, so the body is
// deterministic.
func (a *API) writeError(w nethttp.ResponseWriter, e *Error) {
	d := e.Details
	if d == nil {
		d = map[string]any{}
	}
	a.writeJSON(w, e.Status, errorBody{Code: e.Code, Message: e.Message, Details: d})
}

// writeJSON writes body as the response. Struct fields keep their declared
// order and maps their sorted keys: the same value is always the same
// bytes (§10.1).
func (a *API) writeJSON(w nethttp.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		// A programming error: every body is a plain struct or map.
		a.log.Error("encoding a response", "error", err.Error())
		status = nethttp.StatusInternalServerError
		b = []byte(`{"code":"` + CodeInternal + `","message":"` + internalMessage + `","details":{}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(append(b, '\n')); err != nil {
		// The client went away: nothing left to tell it.
		a.log.Debug("writing a response", "error", err.Error())
	}
}
