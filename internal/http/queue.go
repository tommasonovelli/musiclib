package http

import (
	"errors"
	nethttp "net/http"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/names"
)

// The import and queue endpoints of §10.2 (round 16): the /import
// listing, the import batches and their report, the jobs, the retries and
// render-all. Every write is one transaction of internal/catalog (§13.2);
// a retry and render-all change no catalog row, and take no If-Match
// (N-196). The job representations are the processing state of §10.1: no
// ETag, never cached.

// queueJobJSON is a job (N-193): the jobs row of §4.2 without the claim,
// with its error message shown through jobMessage (N-150) and its
// overrides and warnings decoded.
type queueJobJSON struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	// Ticket is the job's requested ticket: a retry gives a new one.
	Ticket        int64          `json:"ticket"`
	AlbumID       *string        `json:"album_id"`
	BatchID       *string        `json:"batch_id"`
	SourceRel     *string        `json:"source_rel"`
	Overrides     *overridesJSON `json:"overrides"`
	ResultAlbumID *string        `json:"result_album_id"`
	ErrorCode     *string        `json:"error_code"`
	ErrorMessage  *string        `json:"error_message"`
	Warnings      []warningJSON  `json:"warnings"`
	QueuedAt      time.Time      `json:"queued_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	// DismissedAt, Superseded and NeedsAttention say whether a failed job
	// still needs attention (NOTES.md N-285): a dismissed or superseded
	// failed scan or import does not, and «Retry all» leaves it alone.
	DismissedAt    *time.Time `json:"dismissed_at"`
	Superseded     bool       `json:"superseded"`
	NeedsAttention bool       `json:"needs_attention"`
}

// overridesJSON are the §7.3 overrides of an import; null values are
// absent overrides. Other kinds of job have none (overrides null).
type overridesJSON struct {
	Artist *string `json:"artist"`
	Title  *string `json:"title"`
}

// warningJSON is one structured warning (§4.2); path is relative to
// /import as on disk, or null.
type warningJSON struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Path    *string `json:"path"`
}

func idString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

func queueJobRep(v catalog.JobView) queueJobJSON {
	out := queueJobJSON{
		ID: v.ID.String(), Kind: string(v.Kind), State: string(v.State), Ticket: v.Ticket,
		AlbumID: idString(v.AlbumID), BatchID: idString(v.BatchID), SourceRel: v.SourceRel,
		ResultAlbumID: idString(v.ResultAlbumID), ErrorCode: v.ErrorCode,
		ErrorMessage: jobMessage(v.ErrorCode, v.ErrorMessage),
		Warnings:     make([]warningJSON, len(v.Warnings)),
		QueuedAt:     v.QueuedAt.UTC(), UpdatedAt: v.UpdatedAt.UTC(),
		Superseded: v.Superseded, NeedsAttention: v.Attention,
	}
	if v.DismissedAt != nil {
		t := v.DismissedAt.UTC()
		out.DismissedAt = &t
	}
	if v.Kind == jobs.KindImport {
		out.Overrides = &overridesJSON{Artist: v.Overrides.Artist, Title: v.Overrides.Title}
	}
	for i, w := range v.Warnings {
		out.Warnings[i] = warningJSON{Code: string(w.Code), Message: w.Message}
		if w.Path != "" {
			p := w.Path
			out.Warnings[i].Path = &p
		}
	}
	return out
}

// importJSON is the report of an import batch (§7.2, §10.2 GET
// /api/imports/{id}; N-193): its root relative to /import, its state
// (scanning, importing, completed), the scan, whose warnings are the
// unassigned files and the rejected entries, and one import job per
// candidate or failed branch, by the bytes of its path.
type importJSON struct {
	ID         string         `json:"id"`
	Path       string         `json:"path"`
	CreatedAt  time.Time      `json:"created_at"`
	State      string         `json:"state"`
	Scan       queueJobJSON   `json:"scan"`
	Candidates []queueJobJSON `json:"candidates"`
}

func importRep(r catalog.ImportReport) importJSON {
	out := importJSON{
		ID: r.Batch.ID.String(), Path: r.Batch.RootRel, CreatedAt: r.Batch.CreatedAt.UTC(), State: r.State(),
		Scan: queueJobRep(r.Scan), Candidates: make([]queueJobJSON, len(r.Imports)),
	}
	for i, j := range r.Imports {
		out.Candidates[i] = queueJobRep(j)
	}
	return out
}

// sourceEntryJSON is an entry of GET /api/import-source.
type sourceEntryJSON struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type sourceListJSON struct {
	Path    string            `json:"path"`
	Entries []sourceEntryJSON `json:"entries"`
}

// importSource is GET /api/import-source?path=<relative path> (§7.1,
// §10.2): the entries of a directory under /import, sorted by the bytes of
// their names, with their types; symlinks are listed as such and never
// followed (N-197). No path means /import itself.
func (h *handlers) importSource(w nethttp.ResponseWriter, r *nethttp.Request) {
	q, e := queryParams(r, "path")
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	rel := q["path"]
	ents, err := importer.Browse(h.source, rel)
	if err != nil {
		h.browseFailed(w, r, rel, err)
		return
	}
	out := sourceListJSON{Path: rel, Entries: make([]sourceEntryJSON, len(ents))}
	for i, en := range ents {
		out.Entries[i] = sourceEntryJSON{Name: importer.DisplayName(en.Name), Type: en.Type}
	}
	h.api.writeJSON(w, nethttp.StatusOK, out)
}

// browseFailed answers a failure of importer.Browse: an invalid path 422
// with its names code, the source codes of the importer with their
// status, anything else 500.
func (h *handlers) browseFailed(w nethttp.ResponseWriter, r *nethttp.Request, rel string, err error) {
	var ne *names.Error
	if errors.As(err, &ne) {
		h.api.writeError(w, newError(nethttp.StatusUnprocessableEntity, ne.Code,
			"the path %q is not a relative path under /import: %s", rel, ne.Message).with("field", "path"))
		return
	}
	var ie *importer.Error
	if errors.As(err, &ie) {
		if status, ok := browseStatus[ie.Code]; ok {
			h.api.writeError(w, newError(status, ie.Code, "%s", ie.Message).with("path", rel))
			return
		}
	}
	h.api.fail(w, r, err)
}

// browseStatus are the statuses of the source codes of importer.Browse.
var browseStatus = map[string]int{
	importer.CodeSourceNotFound:     nethttp.StatusNotFound,
	importer.CodeSourceNotDirectory: nethttp.StatusUnprocessableEntity,
	importer.CodeSourceRejected:     nethttp.StatusUnprocessableEntity,
	importer.CodeSourceNotReadable:  nethttp.StatusUnprocessableEntity,
}

// The keys of POST /api/imports.
var importKeys = []string{"id", "path"}

// createImport is POST /api/imports {"id", "path"} (§7.1): id is the
// client's request UUID, path the directory to import relative to /import
// ("" is /import itself). One transaction creates the batch and its scan
// job. The same id with the same path is the same batch (200); with
// another path it is 409 import_batch_conflict, details.path naming the
// path recorded. 201 with the report and its Location for a new batch.
func (h *handlers) createImport(w nethttp.ResponseWriter, r *nethttp.Request) {
	body, e := readObject(w, r, importKeys...)
	var id uuid.UUID
	var rel string
	if e == nil {
		id, e = body.ID("id")
	}
	if e == nil && id == uuid.Nil {
		e = invalidField("id", "must not be the nil UUID")
	}
	if e == nil {
		rel, e = body.String("path")
	}
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	b, err := h.catalog.CreateImportBatch(r.Context(), id, rel)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	rep, err := h.catalog.GetImportReport(r.Context(), b.ID)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	status := nethttp.StatusOK
	if b.Created {
		status = nethttp.StatusCreated
		w.Header().Set("Location", "/api/imports/"+b.ID.String())
	}
	h.api.writeJSON(w, status, importRep(rep))
}

// getImport is GET /api/imports/{id}: the report of the batch.
func (h *handlers) getImport(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, ok := h.subID(w, r, "id", catalog.CodeImportBatchNotFound)
	if !ok {
		return
	}
	rep, err := h.catalog.GetImportReport(r.Context(), id)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	h.api.writeJSON(w, nethttp.StatusOK, importRep(rep))
}

type jobListJSON struct {
	Jobs []queueJobJSON `json:"jobs"`
	Next *string        `json:"next"`
}

// The values of the filters of GET /api/jobs.
var (
	listedStates = map[string]jobs.State{"pending": jobs.StatePending, "running": jobs.StateRunning, "failed": jobs.StateFailed}
	listedKinds  = map[string]jobs.Kind{"scan": jobs.KindScan, "import": jobs.KindImport, "render": jobs.KindRender}
)

// listJobs is GET /api/jobs (§10.2): pending, running and failed jobs,
// by id, with their errors (N-194). The query parameters are
//
//	state  pending, running or failed: only that state
//	kind   scan, import or render: only that kind
//	limit  1..200, default 50
//	after  the "next" of the previous page (a job id)
func (h *handlers) listJobs(w nethttp.ResponseWriter, r *nethttp.Request) {
	q, e := queryParams(r, "state", "kind", "limit", "after")
	var f catalog.JobFilter
	if e == nil {
		if v, ok := q["state"]; ok {
			st, known := listedStates[v]
			if !known {
				e = invalidField("state", "must be pending, running or failed")
			}
			f.States = []jobs.State{st}
		}
	}
	if e == nil {
		if v, ok := q["kind"]; ok {
			k, known := listedKinds[v]
			if !known {
				e = invalidField("kind", "must be scan, import or render")
			}
			f.Kinds = []jobs.Kind{k}
		}
	}
	if e == nil {
		f.Limit, e = pageLimit(q)
	}
	if e == nil {
		f.After, e = queryID(q, "after")
	}
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	page, err := h.catalog.ListJobs(r.Context(), f)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	out := jobListJSON{Jobs: make([]queueJobJSON, len(page.Jobs))}
	for i, j := range page.Jobs {
		out.Jobs[i] = queueJobRep(j)
	}
	if page.Next != uuid.Nil {
		n := page.Next.String()
		out.Next = &n
	}
	h.api.writeJSON(w, nethttp.StatusOK, out)
}

// The keys of the body of a retry: the §7.3 overrides and nothing else.
var overrideKeys = []string{"artist", "title"}

// retryJob is POST /api/jobs/{id}/retry (§10.2, N-195): a failed job gets
// a new ticket. The body is empty, or for an import {"artist", "title"},
// each a string or null, which replace the import's overrides (§7.3); an
// empty body keeps them. A pending or running job is left as it is
// (idempotent); done and skipped are 409 job_not_retryable; overrides for
// another kind of job are 422. No If-Match: a retry changes no catalog row
// (N-196). 202 with the job.
func (h *handlers) retryJob(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, ok := h.subID(w, r, "id", catalog.CodeJobNotFound)
	if !ok {
		return
	}
	ov, e := readOverrides(w, r)
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	v, _, err := h.catalog.RetryJob(r.Context(), id, ov)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	h.api.writeJSON(w, nethttp.StatusAccepted, queueJobRep(v))
}

// readOverrides reads the optional body of a retry: nil for an empty body,
// otherwise exactly {"artist", "title"} under N-148's rules.
func readOverrides(w nethttp.ResponseWriter, r *nethttp.Request) (*jobs.Overrides, *Error) {
	b, e := readBody(w, r)
	if e != nil || len(b) == 0 {
		return nil, e
	}
	if e := checkContentType(r.Header); e != nil {
		return nil, e
	}
	if e := checkJSON(b); e != nil {
		return nil, e
	}
	body, e := decodeObject(b, "", overrideKeys...)
	if e != nil {
		return nil, e
	}
	var ov jobs.Overrides
	if ov.Artist, e = body.NullableString("artist"); e != nil {
		return nil, e
	}
	if ov.Title, e = body.NullableString("title"); e != nil {
		return nil, e
	}
	return &ov, nil
}

// dismissJob is POST /api/jobs/{id}/dismiss (owner, NOTES.md N-285): a
// failed scan or import stops needing attention, without any other change.
// No body, no If-Match (a job has no revision, N-196). Dismissing it again
// is the same answer; a render or a job that is not failed is 409
// job_not_dismissable. 200 with the job.
func (h *handlers) dismissJob(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, ok := h.subID(w, r, "id", catalog.CodeJobNotFound)
	if !ok {
		return
	}
	if e := refuseBody(w, r); e != nil {
		h.api.writeError(w, e)
		return
	}
	v, err := h.catalog.DismissJob(r.Context(), id)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	h.api.writeJSON(w, nethttp.StatusOK, queueJobRep(v))
}

// retryFailed is POST /api/jobs/retry-failed (§10.2): every failed job
// that still needs attention gets a new ticket (a dismissed or superseded
// scan or import is left alone, N-285), running ones are never duplicated.
// 202 with the count.
func (h *handlers) retryFailed(w nethttp.ResponseWriter, r *nethttp.Request) {
	if e := refuseBody(w, r); e != nil {
		h.api.writeError(w, e)
		return
	}
	n, err := h.catalog.RetryFailed(r.Context())
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	h.api.writeJSON(w, nethttp.StatusAccepted, map[string]int{"retried": n})
}

// renderAll is POST /api/render-all (§10.2): the render of every active
// album and of every deletion still to materialize, through the single
// enqueue (N-198). 202 with the count.
func (h *handlers) renderAll(w nethttp.ResponseWriter, r *nethttp.Request) {
	if e := refuseBody(w, r); e != nil {
		h.api.writeError(w, e)
		return
	}
	n, err := h.catalog.RenderAll(r.Context())
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	h.api.writeJSON(w, nethttp.StatusAccepted, map[string]int{"enqueued": n})
}
