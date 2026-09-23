// Package jobs is the durable queue of DESIGN.md §6: the single enqueue of
// a render (§6.3), the claim with its REPEATABLE READ snapshot (§6.2), the
// completions conditioned on the claimed ticket (§6.3, §6.4), the boot
// helpers of §11.1 steps 5 and 6, and the worker pool (§6.1).
//
// There is no job framework (§2.3): a job is a row of the jobs table, an
// attempt is its id plus the ticket copied into claimed, and the work itself
// is a function supplied by the caller. Every write outside the claim runs
// in a transaction holding the catalog lock (store.CatalogTx); the claim is a
// short transaction of its own, with FOR UPDATE SKIP LOCKED (§6.2).
package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/google/uuid"

	"musiclib/internal/names"
)

// Kind is jobs.kind.
type Kind string

// The kinds of job (§4.2). The claim order is render, scan, import (§6.1).
const (
	KindRender Kind = "render"
	KindScan   Kind = "scan"
	KindImport Kind = "import"
)

// claimOrder is the fixed priority of §6.1: renders before the scan, the
// scan before the imports of single albums.
var claimOrder = [...]Kind{KindRender, KindScan, KindImport}

// State is jobs.state.
type State string

// The states of a job (§4.2). Renders are only pending, running or failed:
// a successful render deletes its row (§6.4).
const (
	StatePending State = "pending"
	StateRunning State = "running"
	StateDone    State = "done"
	StateSkipped State = "skipped"
	StateFailed  State = "failed"
)

// Terminal reports whether s is an outcome kept for the report (§6.4).
func (s State) Terminal() bool {
	return s == StateDone || s == StateSkipped || s == StateFailed
}

// Attempt identifies one execution of a job: the row and the ticket that
// the claim copied from requested into claimed (§6.2). Every completion is
// conditioned on both (§6.3): an old attempt cannot complete a newer one.
type Attempt struct {
	JobID  uuid.UUID
	Ticket int64
}

func (a Attempt) String() string { return fmt.Sprintf("job %s ticket %d", a.JobID, a.Ticket) }

// WarningCode is the closed set of structured warnings kept in
// jobs.warnings (§4.2: "array di messaggi strutturati", validated by a
// closed Go type).
type WarningCode string

// The warnings the import and the scan report (§7.2, §7.3, §8.1). A new
// kind of warning is a new constant here, never a free string.
const (
	// WarnUnassignedFile: a file outside every candidate (§7.2).
	WarnUnassignedFile WarningCode = "unassigned_file"
	// WarnTracksRenumbered: a disc numbered by the natural order of the
	// basenames (§7.3).
	WarnTracksRenumbered WarningCode = "tracks_renumbered"
	// WarnYearDiscordant: the tracks disagree on the year (§7.3).
	WarnYearDiscordant WarningCode = "year_discordant"
	// WarnTagConflict: conflicting values of a managed field (§8.1).
	WarnTagConflict WarningCode = "tag_conflict"
)

var warningCodes = map[WarningCode]bool{
	WarnUnassignedFile:   true,
	WarnTracksRenumbered: true,
	WarnYearDiscordant:   true,
	WarnTagConflict:      true,
}

// maxWarningMessage bounds a warning's text, in bytes.
const maxWarningMessage = 4096

// Warning is one element of jobs.warnings.
type Warning struct {
	Code    WarningCode `json:"code"`
	Message string      `json:"message"`
	// Path is the source path concerned, relative and exactly as on disk
	// (§5.2), or empty.
	Path string `json:"path,omitempty"`
}

func (w Warning) validate() error {
	if !warningCodes[w.Code] {
		return errorf(CodeInvalidResult, "unknown warning code %q", w.Code)
	}
	if w.Message == "" || len(w.Message) > maxWarningMessage || !utf8.ValidString(w.Message) {
		return errorf(CodeInvalidResult, "warning %s: the message must be valid UTF-8 of 1 to %d bytes", w.Code, maxWarningMessage)
	}
	if w.Path != "" {
		if _, err := names.SplitRelPath(w.Path); err != nil {
			return &Error{Code: CodeInvalidResult, Msg: fmt.Sprintf("warning %s: invalid path", w.Code), Err: err}
		}
	}
	return nil
}

// EncodeWarnings validates ws and returns the value of jobs.warnings: a
// JSON array, "[]" when empty.
func EncodeWarnings(ws []Warning) ([]byte, error) {
	for _, w := range ws {
		if err := w.validate(); err != nil {
			return nil, err
		}
	}
	if ws == nil {
		ws = []Warning{}
	}
	return json.Marshal(ws)
}

// DecodeWarnings parses jobs.warnings strictly: unknown fields, unknown
// codes and trailing data are errors.
func DecodeWarnings(b []byte) ([]Warning, error) {
	var ws []Warning
	if err := decodeStrict(b, &ws); err != nil {
		return nil, &Error{Code: CodeInvalidResult, Msg: "decoding warnings", Err: err}
	}
	if ws == nil {
		return nil, errorf(CodeInvalidResult, "warnings must be a JSON array")
	}
	for _, w := range ws {
		if err := w.validate(); err != nil {
			return nil, err
		}
	}
	return ws, nil
}

// Overrides are jobs.overrides (§4.2, §7.3): the album artist and title
// that the user may impose on a failed import before retrying it. A nil
// field is absent; there are no other overrides.
type Overrides struct {
	Artist *string `json:"artist,omitempty"`
	Title  *string `json:"title,omitempty"`
}

func (o Overrides) validate() error {
	for _, v := range []*string{o.Artist, o.Title} {
		if v == nil {
			continue
		}
		n, err := names.NormalizeRequiredText(*v)
		if err != nil {
			return &Error{Code: CodeInvalidOverrides, Msg: "override", Err: err}
		}
		if n != *v {
			return errorf(CodeInvalidOverrides, "override %q is not normalized", *v)
		}
	}
	return nil
}

// Encode validates o and returns the value of jobs.overrides. The values
// must already be normalized texts (§5.2), so the stored value is final.
func (o Overrides) Encode() ([]byte, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(o)
}

// DecodeOverrides parses jobs.overrides into the closed type: an object
// with at most the keys artist and title, string values, nothing else.
//
// It checks the shape only, not that the values are normalized texts. The
// claim decodes the overrides of the job it takes, and a value refused
// there would abort every claim transaction: the oldest pending import
// would be retried at every poll and would starve every import queued
// after it. Encode, the only writer, refuses non-normalized values, and the
// import commit normalizes the album's artist and title again, failing the
// job with a typed error if they are not valid texts.
func DecodeOverrides(b []byte) (Overrides, error) {
	var o Overrides
	if err := decodeStrict(b, &o); err != nil {
		return Overrides{}, &Error{Code: CodeInvalidOverrides, Msg: "decoding overrides", Err: err}
	}
	return o, nil
}

// decodeStrict decodes exactly one JSON value without unknown fields.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data after the JSON value")
	}
	return nil
}

// errorCodePattern is the shape of jobs.error_code: a stable lowercase code
// such as the ones of names, fsops, media or catalog.
var errorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// maxErrorMessage bounds jobs.error_message, in bytes. Longer messages are
// cut on a rune boundary: they are for the user, never parsed.
const maxErrorMessage = 4096

func validErrorCode(code string) bool { return errorCodePattern.MatchString(code) }

// clipMessage returns msg cut to maxErrorMessage bytes, on a rune boundary,
// with invalid UTF-8 replaced: the column is text shown to the user.
func clipMessage(msg string) string {
	msg = string([]rune(msg)) // replaces invalid bytes with U+FFFD
	if len(msg) <= maxErrorMessage {
		return msg
	}
	cut := maxErrorMessage
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}
