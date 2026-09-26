package catalog_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

// The queue through the catalog (§10.2, round 16): the jobs and the import
// report, the retries, render-all and the retention of §6.4.

// importJob inserts an import job of batch in state, by SQL, as the scan
// and the importer would have left it.
func (e *env) importJob(batch uuid.UUID, source, state string) uuid.UUID {
	e.t.Helper()
	id := store.NewID()
	e.exec(`INSERT INTO jobs (id, kind, batch_id, source_rel, state, queued_at, updated_at)
		VALUES ($1, 'import', $2, $3, 'pending', now(), now())`, id, batch, source)
	e.setState(id, state)
	return id
}

func (e *env) setState(id uuid.UUID, state string) {
	e.t.Helper()
	switch state {
	case "pending":
	case "running":
		e.exec(`UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1`, id)
	case "failed":
		e.exec(`UPDATE jobs SET state = 'failed', error_code = 'mixed_album', error_message = 'two album tags' WHERE id = $1`, id)
	case "done":
		e.exec(`UPDATE jobs SET state = 'done', result_album_id = $2 WHERE id = $1`, id, e.importAlbum("R", "done "+id.String()))
	case "skipped":
		e.exec(`UPDATE jobs SET state = 'skipped', result_album_id = $2, error_code = 'duplicate_import', error_message = 'x'
			WHERE id = $1`, id, e.importAlbum("R", "skipped "+id.String()))
	default:
		e.t.Fatalf("state %q", state)
	}
}

func (e *env) newBatch(root string) uuid.UUID {
	e.t.Helper()
	b, err := e.svc.CreateImportBatch(context.Background(), store.NewID(), root)
	if err != nil || !b.Created {
		e.t.Fatalf("CreateImportBatch: %+v %v", b, err)
	}
	return b.ID
}

func TestImportReport(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	batch := e.newBatch("in")
	r, err := e.svc.GetImportReport(ctx, batch)
	if err != nil || r.State() != catalog.BatchScanning || r.Scan.Kind != jobs.KindScan || len(r.Imports) != 0 ||
		r.Batch.RootRel != "in" || r.Batch.ScanJobID != r.Scan.ID {
		t.Fatalf("a new batch: %+v %v", r, err)
	}
	// The scan's outcome and candidates, as CommitScan writes them: the
	// candidates by the bytes of their paths (Z before a).
	e.exec(`UPDATE jobs SET state = 'done', warnings = '[{"code":"unassigned_file","message":"m","path":"in/x.txt"}]'
		WHERE id = $1`, r.Scan.ID)
	b := e.importJob(batch, "in/b", "pending")
	z := e.importJob(batch, "in/Z", "failed")
	a := e.importJob(batch, "in/a", "done")
	if r, _ = e.svc.GetImportReport(ctx, batch); r.State() != catalog.BatchImporting {
		t.Fatalf("state %s, want %s", r.State(), catalog.BatchImporting)
	}
	var ids []uuid.UUID
	for _, j := range r.Imports {
		ids = append(ids, j.ID)
	}
	if !slices.Equal(ids, []uuid.UUID{z, a, b}) {
		t.Fatalf("candidates %v, want by path bytes", ids)
	}
	if w := r.Scan.Warnings; len(w) != 1 || w[0].Code != jobs.WarnUnassignedFile || w[0].Path != "in/x.txt" {
		t.Fatalf("scan warnings %+v", w)
	}
	if j := r.Imports[1]; j.ResultAlbumID == nil || j.SourceRel == nil || *j.SourceRel != "in/a" {
		t.Fatalf("done candidate %+v", j)
	}
	e.setState(b, "running")
	if r, _ = e.svc.GetImportReport(ctx, batch); r.State() != catalog.BatchImporting {
		t.Fatalf("state %s with a running import", r.State())
	}
	e.exec(`UPDATE jobs SET state = 'skipped', claimed = NULL, result_album_id = $2, error_code = 'duplicate_import',
		error_message = 'x' WHERE id = $1`, b, *r.Imports[1].ResultAlbumID)
	if r, _ = e.svc.GetImportReport(ctx, batch); r.State() != catalog.BatchCompleted {
		t.Fatalf("state %s, want %s", r.State(), catalog.BatchCompleted)
	}
	// No valid candidate: the scan failed with its explanation, and the
	// batch is completed, not an empty success (§7.2).
	empty := e.newBatch("nothing")
	r, _ = e.svc.GetImportReport(ctx, empty)
	e.exec(`UPDATE jobs SET state = 'failed', error_code = 'no_valid_candidate', error_message = 'no audio' WHERE id = $1`, r.Scan.ID)
	if r, _ = e.svc.GetImportReport(ctx, empty); r.State() != catalog.BatchCompleted || *r.Scan.ErrorCode != "no_valid_candidate" {
		t.Fatalf("no valid candidate: %s %+v", r.State(), r.Scan)
	}
	wantCode(t, func() error { _, err := e.svc.GetImportReport(ctx, store.NewID()); return err }(), catalog.CodeImportBatchNotFound)
	wantCode(t, func() error { _, err := e.svc.GetJob(ctx, store.NewID()); return err }(), catalog.CodeJobNotFound)
}

func TestListJobs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	batch := e.newBatch("in")
	var want []uuid.UUID
	for _, st := range []string{"pending", "running", "failed", "done", "skipped", "failed"} {
		id := e.importJob(batch, "in/"+st+store.NewID().String(), st)
		if st == "pending" || st == "running" || st == "failed" {
			want = append(want, id)
		}
	}
	// Every pending, running or failed job, by id (UUIDv7: creation order):
	// the batch's scan, the imports, the renders of the albums created.
	rows, err := e.db.Query(ctx, `SELECT id FROM jobs WHERE state IN ('pending', 'running', 'failed') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var all []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		all = append(all, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	page := func(f catalog.JobFilter) catalog.JobPage {
		t.Helper()
		p, err := e.svc.ListJobs(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	var got []uuid.UUID
	for after := uuid.Nil; ; {
		p := page(catalog.JobFilter{Limit: 2, After: after})
		for _, j := range p.Jobs {
			got = append(got, j.ID)
		}
		if p.Next == uuid.Nil {
			break
		}
		after = p.Next
	}
	if !slices.Equal(got, all) {
		t.Fatalf("pages %v, want %v", got, all)
	}
	var imports []uuid.UUID
	for _, j := range page(catalog.JobFilter{Limit: 200, Kinds: []jobs.Kind{jobs.KindImport}}).Jobs {
		imports = append(imports, j.ID)
	}
	if !slices.Equal(imports, want) {
		t.Fatalf("imports %v, want %v (no done or skipped)", imports, want)
	}
	failed := page(catalog.JobFilter{Limit: 200, States: []jobs.State{jobs.StateFailed}, Kinds: []jobs.Kind{jobs.KindImport}})
	if len(failed.Jobs) != 2 || *failed.Jobs[0].ErrorCode != "mixed_album" {
		t.Fatalf("failed imports %+v", failed.Jobs)
	}
	if _, err := e.svc.ListJobs(ctx, catalog.JobFilter{Limit: 201}); catalog.Code(err) != catalog.CodeInvalidArgument {
		t.Fatalf("limit 201: %v", err)
	}
}

func TestRetryJob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	batch := e.newBatch("in")
	id := e.importJob(batch, "in/a", "failed")
	e.wakes.Store(0)
	// The overrides are normalized as texts (§5.2) before they are stored.
	v, changed, err := e.svc.RetryJob(ctx, id, &jobs.Overrides{Artist: ptr("  Quéen "), Title: nil})
	if err != nil || !changed || v.State != jobs.StatePending || v.Overrides.Artist == nil || *v.Overrides.Artist != "Quéen" ||
		v.Overrides.Title != nil || e.wakes.Load() != 1 {
		t.Fatalf("RetryJob: %+v %v %v, wakes %d", v, changed, err, e.wakes.Load())
	}
	// Pending: the same answer, nothing changed, no wake-up.
	again, changed, err := e.svc.RetryJob(ctx, id, nil)
	if err != nil || changed || again.Ticket != v.Ticket || e.wakes.Load() != 1 {
		t.Fatalf("second RetryJob: %+v %v %v", again, changed, err)
	}
	e.setState(id, "failed")
	for _, bad := range []string{"", "   ", "a\x00b"} {
		_, _, err := e.svc.RetryJob(ctx, id, &jobs.Overrides{Title: ptr(bad)})
		if code := catalog.Code(err); code != names.CodeTextEmpty && code != names.CodeTextControlChar {
			t.Errorf("override %q: %v, want a names text code", bad, err)
		}
	}
	if j := e.job(id); j.State != "failed" {
		t.Fatalf("a refused retry changed the job: %s", j.State)
	}
	_, _, err = e.svc.RetryJob(ctx, store.NewID(), nil)
	if catalog.Code(err) != jobs.CodeNotFound {
		t.Fatalf("unknown job: %v", err)
	}
	// retry-failed: every failed job of the catalog.
	e.importJob(batch, "in/b", "failed")
	e.importJob(batch, "in/c", "running")
	if n, err := e.svc.RetryFailed(ctx); err != nil || n != 2 {
		t.Fatalf("RetryFailed = %d, %v; want 2", n, err)
	}
	if n, err := e.svc.RetryFailed(ctx); err != nil || n != 0 {
		t.Fatalf("second RetryFailed = %d, %v; want 0", n, err)
	}
}

// render-all (§10.2): every active album and every deletion still to
// materialize, through the single enqueue: one row per album, a running
// render keeps running with a newer ticket, no revision changes.
func TestRenderAll(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	active := e.importAlbum("A", "Active")
	running := e.importAlbum("A", "Running")
	failed := e.importAlbum("A", "Failed")
	gone := e.importAlbum("A", "Trashed and removed")
	pendingRemoval := e.importAlbum("A", "Trashed, still published")
	neverPublished := e.importAlbum("A", "Trashed, never published")
	e.published(gone, "A/Trashed and removed")
	e.published(pendingRemoval, "A/Trashed, still published")
	for _, id := range []uuid.UUID{gone, pendingRemoval, neverPublished} {
		if _, _, err := e.svc.TrashAlbum(ctx, id, e.album(id).Revision); err != nil {
			t.Fatal(err)
		}
	}
	e.removalPublished(gone)
	e.clearRenders()
	enqueue := func(id uuid.UUID) uuid.UUID {
		var j jobs.Enqueued
		if err := store.InCatalogTx(ctx, e.db, func(tx *store.CatalogTx) error {
			var err error
			j, err = jobs.EnqueueRender(ctx, tx, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return j.JobID
	}
	runningJob := enqueue(running)
	e.exec(`UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1`, runningJob)
	failedJob := enqueue(failed)
	e.exec(`UPDATE jobs SET state = 'failed', error_code = 'render_io', error_message = 'x' WHERE id = $1`, failedJob)
	before := map[uuid.UUID]store.Job{runningJob: e.job(runningJob), failedJob: e.job(failedJob)}
	revisions := map[uuid.UUID]int64{}
	for _, id := range []uuid.UUID{active, running, failed, gone, pendingRemoval, neverPublished} {
		revisions[id] = e.album(id).Revision
	}
	e.wakes.Store(0)
	n, err := e.svc.RenderAll(ctx)
	if err != nil || n != 4 || e.wakes.Load() != 1 {
		t.Fatalf("RenderAll = %d, %v (wakes %d); want 4", n, err, e.wakes.Load())
	}
	for id, want := range map[uuid.UUID]bool{active: true, running: true, failed: true, pendingRemoval: true,
		gone: false, neverPublished: false} {
		_, has := e.renderJob(id)
		if has != want {
			t.Errorf("album %q: render job %v, want %v", e.album(id).Title, has, want)
		}
		if e.album(id).Revision != revisions[id] {
			t.Errorf("album %q: revision changed", e.album(id).Title)
		}
	}
	r := e.job(runningJob)
	if r.State != "running" || *r.Claimed != *before[runningJob].Claimed || r.Requested <= before[runningJob].Requested {
		t.Fatalf("the running render: %+v, want running, its claim kept, a newer ticket", r)
	}
	if f := e.job(failedJob); f.State != "pending" || f.ErrorCode != nil {
		t.Fatalf("the failed render: %+v, want pending without error", f)
	}
	// Again: still one row per album (coalescing on the single row).
	if n, err := e.svc.RenderAll(ctx); err != nil || n != 4 {
		t.Fatalf("second RenderAll = %d, %v", n, err)
	}
	if c := e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`); c != 4 {
		t.Fatalf("%d render rows, want 4", c)
	}
}

// §6.4 retention with fabricated timestamps: 90 days, terminal batches
// only, the albums untouched.
func TestPurgeImportReports(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	type batch struct {
		name    string
		created int // days ago
		jobs    []string
		updated []int // days ago, per job
		purged  bool
	}
	cases := []batch{
		{"old, all terminal", 100, []string{"done", "skipped", "failed"}, []int{95, 91, 100}, true},
		{"old, failed only", 200, []string{"failed"}, []int{150}, true},
		{"old, a pending job", 100, []string{"done", "pending"}, []int{100, 100}, false},
		{"old, a running job", 100, []string{"done", "running"}, []int{100, 100}, false},
		{"old, a recent outcome", 100, []string{"done", "failed"}, []int{100, 89}, false},
		{"recent", 89, []string{"done"}, []int{89}, false},
		// The batch's own age alone keeps it: outcomes older than the
		// batch cannot happen, but the two conditions are independent.
		{"recent batch, old outcomes", 89, []string{"done", "failed"}, []int{100, 100}, false},
	}
	ids := map[string]uuid.UUID{}
	jobIDs := map[string][]uuid.UUID{}
	var results []uuid.UUID
	for _, c := range cases {
		b := store.NewID()
		ids[c.name] = b
		e.exec(`INSERT INTO import_batches (id, root_rel, created_at) VALUES ($1, 'in', now() - make_interval(days => $2))`, b, c.created)
		for i, st := range c.jobs {
			j := e.importJob(b, "in/"+store.NewID().String(), st)
			e.exec(`UPDATE jobs SET updated_at = now() - make_interval(days => $2) WHERE id = $1`, j, c.updated[i])
			jobIDs[c.name] = append(jobIDs[c.name], j)
			if r := e.job(j).ResultAlbumID; r != nil {
				results = append(results, *r)
			}
		}
	}
	albums := e.count(`SELECT count(*) FROM albums`)
	n, err := e.svc.PurgeImportReports(ctx)
	if err != nil || n != 2 {
		t.Fatalf("PurgeImportReports = %d, %v; want 2", n, err)
	}
	for _, c := range cases {
		kept := e.count(`SELECT count(*) FROM import_batches WHERE id = $1`, ids[c.name]) == 1
		jobsKept := e.count(`SELECT count(*) FROM jobs WHERE batch_id = $1`, ids[c.name])
		if kept == c.purged || (c.purged && jobsKept != 0) || (!c.purged && jobsKept != len(c.jobs)) {
			t.Errorf("%s: batch kept %v with %d jobs, want purged %v", c.name, kept, jobsKept, c.purged)
		}
	}
	// A job references its album, never the reverse: every album stays.
	if got := e.count(`SELECT count(*) FROM albums`); got != albums || len(results) == 0 {
		t.Fatalf("%d albums after the purge, want %d", got, albums)
	}
	if n, err := e.svc.PurgeImportReports(ctx); err != nil || n != 0 {
		t.Fatalf("second purge = %d, %v", n, err)
	}
}
