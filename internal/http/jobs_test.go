package http

import (
	"bytes"
	"fmt"
	nethttp "net/http"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// Round 16: GET /api/jobs, the retries and render-all over a real server
// on the real catalog (§10.2, N-193 to N-196, N-198).

// importJob inserts an import job of batch by SQL, as the scan would
// have, in state: failed with mixed_album, or pending.
func (e *env) importJob(batch uuid.UUID, source, state string) uuid.UUID {
	e.t.Helper()
	id := store.NewID()
	e.exec(`INSERT INTO jobs (id, kind, batch_id, source_rel, state, queued_at, updated_at)
		VALUES ($1, 'import', $2, $3, 'pending', now(), now())`, id, batch, source)
	switch state {
	case "failed":
		e.exec(`UPDATE jobs SET state = 'failed', error_code = 'mixed_album', error_message = 'two album tags: A, B',
			warnings = '[{"code":"year_discordant","message":"1959 and 1960"}]' WHERE id = $1`, id)
	case "running":
		e.exec(`UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1`, id)
	}
	return id
}

func (e *env) newBatch() uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	e.must(req{method: "POST", path: "/api/imports", body: importBody(id, "in")}, nethttp.StatusCreated)
	return id
}

func jobIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	var out []string
	for _, raw := range body["jobs"].([]any) {
		out = append(out, raw.(map[string]any)["id"].(string))
	}
	return out
}

func retryPath(id uuid.UUID) string { return "/api/jobs/" + id.String() + "/retry" }

func TestJobsList(t *testing.T) {
	e := newEnv(t)
	album := e.seed("Miles Davis", "Kind of Blue") // its render job is pending
	batch := e.newBatch()                          // its scan job is pending
	failed := e.importJob(batch, "in/A", "failed")
	e.importJob(batch, "in/B", "running")
	done := e.importJob(batch, "in/C", "pending")
	e.exec(`UPDATE jobs SET state = 'done', result_album_id = $2 WHERE id = $1`, done, album)

	all := e.must(req{method: "GET", path: "/api/jobs"}, ok)
	if n := len(all.body["jobs"].([]any)); n != 4 || all.body["next"] != nil {
		t.Fatalf("jobs: %s", all.raw)
	}
	for _, id := range jobIDs(t, all.body) {
		if id == done.String() {
			t.Fatal("a done job is listed")
		}
	}
	f := e.must(req{method: "GET", path: "/api/jobs?state=failed"}, ok)
	if ids := jobIDs(t, f.body); len(ids) != 1 || ids[0] != failed.String() {
		t.Fatalf("failed: %s", f.raw)
	}
	j := f.body["jobs"].([]any)[0].(map[string]any)
	ov, _ := j["overrides"].(map[string]any)
	ws, _ := j["warnings"].([]any)
	if j["kind"] != "import" || j["state"] != "failed" || j["source_rel"] != "in/A" || j["batch_id"] != batch.String() ||
		j["error_code"] != "mixed_album" || j["error_message"] != "two album tags: A, B" || ov == nil ||
		ov["artist"] != nil || ov["title"] != nil || len(ov) != 2 || len(ws) != 1 || j["album_id"] != nil ||
		ws[0].(map[string]any)["path"] != nil || j["ticket"].(float64) <= 0 || len(j) != 17 ||
		j["dismissed_at"] != nil || j["superseded"] != false || j["needs_attention"] != true {
		t.Fatalf("a failed import: %v", j)
	}
	if !bytes.Contains(f.raw, []byte(`{"id":"`+failed.String()+`","kind":"import","state":"failed","ticket":`)) {
		t.Fatalf("field order: %s", f.raw)
	}
	r := e.must(req{method: "GET", path: "/api/jobs?kind=render"}, ok)
	rj := r.body["jobs"].([]any)[0].(map[string]any)
	if len(r.body["jobs"].([]any)) != 1 || rj["album_id"] != album.String() || rj["overrides"] != nil || rj["batch_id"] != nil {
		t.Fatalf("renders: %s", r.raw)
	}
	// Pages of one, through "next".
	var paged []string
	for after := ""; ; {
		q := "/api/jobs?limit=1"
		if after != "" {
			q += "&after=" + after
		}
		p := e.must(req{method: "GET", path: q}, ok)
		paged = append(paged, jobIDs(t, p.body)...)
		n, _ := p.body["next"].(string)
		if n == "" {
			break
		}
		after = n
	}
	if len(paged) != 4 || paged[0] != jobIDs(t, all.body)[0] || paged[3] != jobIDs(t, all.body)[3] {
		t.Fatalf("paged %v, all %v", paged, jobIDs(t, all.body))
	}
	for _, q := range []string{"state=done", "state=skipped", "state=", "kind=x", "limit=201", "limit=0", "after=x", "state=failed&state=pending"} {
		e.wantError(req{method: "GET", path: "/api/jobs?" + q}, nethttp.StatusUnprocessableEntity, CodeInvalidField)
	}
	e.wantError(req{method: "GET", path: "/api/jobs?batch=x"}, nethttp.StatusUnprocessableEntity, CodeUnknownField)
	// A stored message holding database text is never shown (N-150).
	e.exec(`UPDATE jobs SET error_code = 'catalog_db', error_message = 'ERROR: duplicate key (SQLSTATE 23505)' WHERE id = $1`, failed)
	if f := e.must(req{method: "GET", path: "/api/jobs?state=failed"}, ok); bytes.Contains(f.raw, []byte("SQLSTATE")) {
		t.Fatalf("database text shown: %s", f.raw)
	}
}

func TestRetryAPI(t *testing.T) {
	e := newEnv(t)
	album := e.seed("Miles Davis", "Kind of Blue")
	batch := e.newBatch()
	failed := e.importJob(batch, "in/A", "failed")
	before := e.count(`SELECT requested FROM jobs WHERE id = $1`, failed)

	// The overrides of §7.3 fix mixed_album: a new ticket, pending, the
	// outcome cleared, the title normalized.
	body := `{"artist": null, "title": " Singleś "}`
	r := e.must(req{method: "POST", path: retryPath(failed), body: body}, nethttp.StatusAccepted)
	ov := r.body["overrides"].(map[string]any)
	if r.body["state"] != "pending" || int(r.body["ticket"].(float64)) <= before || r.body["error_code"] != nil ||
		r.body["error_message"] != nil || len(r.body["warnings"].([]any)) != 0 || ov["title"] != "Singleś" || ov["artist"] != nil {
		t.Fatalf("retried: %s", r.raw)
	}
	// Idempotent while pending: the same request, an empty body, the same
	// answer and ticket.
	again := e.must(req{method: "POST", path: retryPath(failed), body: body}, nethttp.StatusAccepted)
	empty := e.must(req{method: "POST", path: retryPath(failed)}, nethttp.StatusAccepted)
	if !bytes.Equal(again.raw, r.raw) || !bytes.Equal(empty.raw, r.raw) {
		t.Fatalf("a retry of a pending job changed it:\n%s\n%s\n%s", r.raw, again.raw, empty.raw)
	}
	e.wantError(req{method: "POST", path: retryPath(failed), body: `{"artist": "Queen", "title": null}`},
		nethttp.StatusConflict, jobs.CodeInProgress)
	// Running: idempotent too.
	e.exec(`UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1`, failed)
	running := e.must(req{method: "POST", path: retryPath(failed)}, nethttp.StatusAccepted)
	if running.body["state"] != "running" || running.body["ticket"] != r.body["ticket"] {
		t.Fatalf("a retry of a running job: %s", running.raw)
	}

	// The body: nothing but the two overrides, each a string or null.
	other := e.importJob(batch, "in/B", "failed")
	for b, want := range map[string]struct {
		status int
		code   string
	}{
		`{"artist": null}`: {nethttp.StatusUnprocessableEntity, CodeMissingField},
		`{"artist": null, "title": null, "genre": "x"}`:   {nethttp.StatusUnprocessableEntity, CodeUnknownField},
		`{"artist": null, "title": 5}`:                    {nethttp.StatusUnprocessableEntity, CodeInvalidField},
		`{"Artist": null, "title": null}`:                 {nethttp.StatusUnprocessableEntity, CodeUnknownField},
		`{"artist": "", "title": null}`:                   {nethttp.StatusUnprocessableEntity, names.CodeTextEmpty},
		`{"artist": "a\u0001", "title": null}`:            {nethttp.StatusUnprocessableEntity, names.CodeTextControlChar},
		`{"artist": null, "title": "a\u0000b"}`:           {nethttp.StatusUnprocessableEntity, names.CodeTextControlChar},
		`[]`:                                              {nethttp.StatusUnprocessableEntity, CodeInvalidField},
		`{"artist": null, "title": null} {}`:              {nethttp.StatusBadRequest, CodeInvalidJSON},
		`{"artist": null, "artist": null, "title": null}`: {nethttp.StatusBadRequest, CodeDuplicateKey},
	} {
		e.wantError(req{method: "POST", path: retryPath(other), body: b}, want.status, want.code)
	}
	e.wantError(req{method: "POST", path: retryPath(other), body: `{"artist": null, "title": null}`,
		headers: map[string][]string{"Content-Type": {"text/plain"}}}, nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	e.wantError(req{method: "POST", path: retryPath(other), headers: map[string][]string{RequestHeader: {""}}},
		nethttp.StatusForbidden, CodeRequestHeaderRequired)
	if e.count(`SELECT count(*) FROM jobs WHERE id = $1 AND state = 'failed'`, other) != 1 {
		t.Fatal("a refused retry changed the job")
	}

	// Done and skipped: nothing to retry.
	done := e.importJob(batch, "in/C", "pending")
	e.exec(`UPDATE jobs SET state = 'done', result_album_id = $2 WHERE id = $1`, done, album)
	e.wantError(req{method: "POST", path: retryPath(done)}, nethttp.StatusConflict, jobs.CodeNotRetryable)
	// Overrides are the import's only (§7.3): refused for a render and a
	// scan, even empty ones.
	var render, scan uuid.UUID
	if err := e.db.QueryRow(t.Context(), `SELECT id FROM jobs WHERE kind = 'render' AND album_id = $1`, album).Scan(&render); err != nil {
		t.Fatal(err)
	}
	if err := e.db.QueryRow(t.Context(), `SELECT id FROM jobs WHERE kind = 'scan' AND batch_id = $1`, batch).Scan(&scan); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{render, scan} {
		e.wantError(req{method: "POST", path: retryPath(id), body: `{"artist": null, "title": null}`},
			nethttp.StatusUnprocessableEntity, jobs.CodeOverridesNotAllowed)
	}
	// A failed render: the single enqueue, pending with a new ticket; no
	// catalog change, no If-Match.
	rev := e.revision(album)
	e.exec(`UPDATE jobs SET state = 'failed', error_code = 'render_io', error_message = 'x' WHERE id = $1`, render)
	rr := e.must(req{method: "POST", path: retryPath(render)}, nethttp.StatusAccepted)
	if rr.body["state"] != "pending" || rr.body["error_code"] != nil || rr.body["album_id"] != album.String() || e.revision(album) != rev {
		t.Fatalf("retried render: %s", rr.raw)
	}
	e.wantError(req{method: "POST", path: retryPath(uuid.New())}, nethttp.StatusNotFound, catalog.CodeJobNotFound)
	e.wantError(req{method: "POST", path: "/api/jobs/x/retry"}, nethttp.StatusNotFound, catalog.CodeJobNotFound)
	e.wantError(req{method: "GET", path: retryPath(render)}, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed)
}

func TestRetryFailedAndRenderAllAPI(t *testing.T) {
	e := newEnv(t)
	a := e.seed("Miles Davis", "Kind of Blue")
	b := e.seed("Bill Evans", "Portrait in Jazz")
	gone := e.seed("Chet Baker", "Chet")
	_, tag := e.album(gone)
	e.must(req{method: "DELETE", path: "/api/albums/" + gone.String(), ifMatch: tag}, ok)
	e.exec(`DELETE FROM jobs WHERE kind = 'render'`)
	batch := e.newBatch()
	f1 := e.importJob(batch, "in/A", "failed")
	f2 := e.importJob(batch, "in/B", "failed")
	run := e.importJob(batch, "in/C", "running")
	runBefore := e.count(`SELECT requested FROM jobs WHERE id = $1`, run)

	e.wantError(req{method: "POST", path: "/api/jobs/retry-failed", body: `{}`}, nethttp.StatusBadRequest, CodeBodyNotAllowed)
	r := e.must(req{method: "POST", path: "/api/jobs/retry-failed"}, nethttp.StatusAccepted)
	if r.body["retried"] != float64(2) || len(r.body) != 1 {
		t.Fatalf("retry-failed: %s", r.raw)
	}
	if e.count(`SELECT count(*) FROM jobs WHERE id IN ($1, $2) AND state = 'pending'`, f1, f2) != 2 ||
		e.count(`SELECT count(*) FROM jobs WHERE id = $1 AND state = 'running' AND requested = $2`, run, runBefore) != 1 {
		t.Fatal("retry-failed did not retry exactly the failed jobs")
	}
	if r := e.must(req{method: "POST", path: "/api/jobs/retry-failed"}, nethttp.StatusAccepted); r.body["retried"] != float64(0) {
		t.Fatalf("second retry-failed: %s", r.raw)
	}

	// render-all: the two active albums; the trashed one was never
	// published, so it has no deletion to materialize.
	revs := []int64{e.revision(a), e.revision(b), e.revision(gone)}
	e.wantError(req{method: "POST", path: "/api/render-all", body: `x`}, nethttp.StatusBadRequest, CodeBodyNotAllowed)
	r = e.must(req{method: "POST", path: "/api/render-all"}, nethttp.StatusAccepted)
	if r.body["enqueued"] != float64(2) || e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`) != 2 {
		t.Fatalf("render-all: %s", r.raw)
	}
	// A trashed album still published is a deletion to materialize.
	e.exec(`UPDATE albums SET published_path = 'Chet Baker/Chet', published_revision = 1, published_renderer = 'x',
		published_build = $2, published_receipt_hash = $3 WHERE id = $1`, gone, store.NewID(), newHash())
	r = e.must(req{method: "POST", path: "/api/render-all"}, nethttp.StatusAccepted)
	if r.body["enqueued"] != float64(3) || e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`) != 3 {
		t.Fatalf("render-all with a deletion: %s", r.raw)
	}
	if got := []int64{e.revision(a), e.revision(b), e.revision(gone)}; got[0] != revs[0] || got[1] != revs[1] || got[2] != revs[2] {
		t.Fatalf("render-all changed a revision: %v, want %v", got, revs)
	}
	e.wantError(req{method: "GET", path: "/api/render-all"}, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed)
	e.wantError(req{method: "POST", path: "/api/render-all", headers: map[string][]string{RequestHeader: {""}}},
		nethttp.StatusForbidden, CodeRequestHeaderRequired)
}

// POST /api/jobs/{id}/dismiss (owner, NOTES.md N-285): a failed scan or
// import stops needing attention, and «Retry all» leaves it alone; a
// retry clears it. Nothing else of the job changes.
func TestDismissAPI(t *testing.T) {
	e := newEnv(t)
	album := e.seed("Miles Davis", "Kind of Blue")
	batch := e.newBatch()
	failed := e.importJob(batch, "in/A", "failed")
	other := e.importJob(batch, "in/B", "failed")
	pending := e.importJob(batch, "in/C", "pending")
	path := func(id uuid.UUID) string { return "/api/jobs/" + id.String() + "/dismiss" }
	before := e.must(req{method: "GET", path: "/api/jobs?state=failed"}, ok)

	e.wantError(req{method: "POST", path: path(failed), headers: map[string][]string{RequestHeader: {""}}},
		nethttp.StatusForbidden, CodeRequestHeaderRequired)
	e.wantError(req{method: "POST", path: path(failed), body: `{}`}, nethttp.StatusBadRequest, CodeBodyNotAllowed)
	e.wantError(req{method: "GET", path: path(failed)}, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed)
	r := e.must(req{method: "POST", path: path(failed)}, ok)
	if r.body["state"] != "failed" || r.body["dismissed_at"] == nil || r.body["needs_attention"] != false ||
		r.body["error_code"] != "mixed_album" || r.body["superseded"] != false {
		t.Fatalf("dismissed: %s", r.raw)
	}
	var was map[string]any
	for _, j := range before.body["jobs"].([]any) {
		if j.(map[string]any)["id"] == failed.String() {
			was = j.(map[string]any)
		}
	}
	for _, k := range []string{"ticket", "updated_at", "queued_at", "error_message", "overrides"} {
		if fmt.Sprint(r.body[k]) != fmt.Sprint(was[k]) {
			t.Errorf("the dismissal changed %s: %v, was %v", k, r.body[k], was[k])
		}
	}
	// Again: the same answer.
	if again := e.must(req{method: "POST", path: path(failed)}, ok); !bytes.Equal(again.raw, r.raw) {
		t.Fatalf("a second dismissal changed the job:\n%s\n%s", r.raw, again.raw)
	}
	// The report and the list say so; retry-failed retries only the other.
	rep := e.must(req{method: "GET", path: "/api/imports/" + batch.String()}, ok)
	for _, c := range rep.body["candidates"].([]any) {
		c := c.(map[string]any)
		if want := c["id"] != failed.String() && c["state"] == "failed"; c["needs_attention"] != want {
			t.Errorf("report: %v needs attention %v, want %v", c["source_rel"], c["needs_attention"], want)
		}
	}
	if r := e.must(req{method: "POST", path: "/api/jobs/retry-failed"}, nethttp.StatusAccepted); r.body["retried"] != float64(1) {
		t.Fatalf("retry-failed: %s", r.raw)
	}
	if e.count(`SELECT count(*) FROM jobs WHERE id = $1 AND state = 'failed'`, failed) != 1 ||
		e.count(`SELECT count(*) FROM jobs WHERE id = $1 AND state = 'pending'`, other) != 1 {
		t.Fatal("retry-failed retried the dismissed job or missed the other")
	}
	// Refusals: not failed, a failed render, an unknown job.
	var render uuid.UUID
	if err := e.db.QueryRow(t.Context(), `SELECT id FROM jobs WHERE kind = 'render' AND album_id = $1`, album).Scan(&render); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE jobs SET state = 'failed', error_code = 'render_io', error_message = 'disk' WHERE id = $1`, render)
	for _, id := range []uuid.UUID{pending, render} {
		e.wantError(req{method: "POST", path: path(id)}, nethttp.StatusConflict, jobs.CodeNotDismissable)
	}
	e.wantError(req{method: "POST", path: path(store.NewID())}, nethttp.StatusNotFound, catalog.CodeJobNotFound)
	// A retry is a new attempt: no longer dismissed.
	if r := e.must(req{method: "POST", path: retryPath(failed)}, nethttp.StatusAccepted); r.body["dismissed_at"] != nil || r.body["state"] != "pending" {
		t.Fatalf("retried: %s", r.raw)
	}
}

// §6.4 at the API: the answer to a retry's COMMIT is lost, so the process
// stops (503, Fatal). After the restart the same retry is safe: the job
// is pending with the ticket the lost commit gave it, and nothing more.
// The same for POST /api/imports: the repeat after the restart is the
// same batch, 200.
func TestRetryAnswerLost(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	direct := pgtest.Pool(t, dbURL)
	if err := store.Migrate(t.Context(), direct); err != nil {
		t.Fatal(err)
	}
	proxy := pgtest.NewProxy(t, dbURL)
	e := newEnvOn(t, pgtest.Pool(t, proxy.URL), true)
	e.db = direct
	batch := e.newBatch()
	job := e.importJob(batch, "in/A", "failed")
	before := e.count(`SELECT requested FROM jobs WHERE id = $1`, job)
	proxy.LoseNextCommitAck()
	e.wantError(req{method: "POST", path: retryPath(job), body: `{"artist": null, "title": "T"}`}, unavailable, store.CodeCommitUncertain)
	lost := e.count(`SELECT requested FROM jobs WHERE id = $1 AND state = 'pending'`, job)
	if lost <= before || e.fatalCount() != 1 {
		t.Fatalf("after the lost answer: ticket %d (was %d), fatal %d", lost, before, e.fatalCount())
	}
	importID := uuid.New()
	// Disabled until the restart: the API answers the fatal error.
	e.wantError(req{method: "POST", path: "/api/imports", body: importBody(importID, "x")}, unavailable, store.CodeCommitUncertain)

	// The restart: a new process on the same database.
	after := newEnvOn(t, direct, true)
	r := after.must(req{method: "POST", path: retryPath(job), body: `{"artist": null, "title": "T"}`}, nethttp.StatusAccepted)
	if int(r.body["ticket"].(float64)) != lost || r.body["state"] != "pending" {
		t.Fatalf("the retry after the restart: %s, want ticket %d", r.raw, lost)
	}
	if n := after.count(`SELECT count(*) FROM jobs WHERE batch_id = $1`, batch); n != 2 {
		t.Fatalf("%d jobs in the batch, want the scan and the import", n)
	}
	// An import whose answer was lost, repeated after the restart.
	p2 := pgtest.NewProxy(t, dbURL)
	lostEnv := newEnvOn(t, pgtest.Pool(t, p2.URL), true)
	p2.LoseNextCommitAck()
	lostEnv.wantError(req{method: "POST", path: "/api/imports", body: importBody(importID, "x")}, unavailable, store.CodeCommitUncertain)
	if after.count(`SELECT count(*) FROM import_batches WHERE id = $1`, importID) != 1 {
		t.Fatal("the batch of the lost answer is not durable")
	}
	rep := after.must(req{method: "POST", path: "/api/imports", body: importBody(importID, "x")}, ok)
	if rep.body["id"] != importID.String() || after.count(`SELECT count(*) FROM jobs WHERE batch_id = $1`, importID) != 1 {
		t.Fatalf("the repeat after the restart: %s", rep.raw)
	}
}
