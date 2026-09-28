package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/store"
)

// §10.2 retries (NOTES.md N-195): a failed job gets a new ticket; pending
// and running jobs are left alone; done and skipped are not retryable;
// overrides only for an import.

// fail makes a job failed by SQL, as an executor would have.
func (f *fixture) fail(id uuid.UUID) {
	f.t.Helper()
	f.exec(`UPDATE jobs SET state = 'failed', claimed = NULL, error_code = 'mixed_album', error_message = 'two albums',
		warnings = '[{"code":"year_discordant","message":"years differ"}]' WHERE id = $1`, id)
}

// run makes a job running by SQL, with its claim.
func (f *fixture) run(id uuid.UUID) {
	f.t.Helper()
	f.exec(`UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1`, id)
}

func (f *fixture) retry(id uuid.UUID, ov *Overrides) (bool, error) {
	f.t.Helper()
	var changed bool
	err := store.InCatalogTx(context.Background(), f.db, func(tx *store.CatalogTx) error {
		var err error
		changed, err = Retry(context.Background(), tx, id, ov)
		return err
	})
	return changed, err
}

func (f *fixture) mustRetry(id uuid.UUID, ov *Overrides, changed bool) {
	f.t.Helper()
	got, err := f.retry(id, ov)
	if err != nil || got != changed {
		f.t.Fatalf("Retry(%s, %+v) = %v, %v; want %v", id, ov, got, err, changed)
	}
}

func overridesOf(t *testing.T, j store.Job) Overrides {
	t.Helper()
	o, err := DecodeOverrides(j.Overrides)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestRetryImport(t *testing.T) {
	f := newFixture(t)
	batch := f.batch()
	id := f.importJob(batch, "incoming/A", time.Now())
	f.exec(`UPDATE jobs SET overrides = '{"artist": "Queen"}' WHERE id = $1`, id)
	f.fail(id)
	before := f.job(id)

	// Failed: a new ticket, pending, the outcome cleared, the overrides kept.
	f.mustRetry(id, nil, true)
	j := f.job(id)
	if j.State != string(StatePending) || j.Requested <= before.Requested || j.Claimed != nil ||
		j.ErrorCode != nil || j.ErrorMessage != nil || string(j.Warnings) != "[]" || j.ResultAlbumID != nil ||
		!j.QueuedAt.After(before.QueuedAt) {
		t.Fatalf("after the retry: %+v", j)
	}
	if o := overridesOf(t, j); o.Artist == nil || *o.Artist != "Queen" || o.Title != nil {
		t.Fatalf("overrides %+v, want the stored ones kept", o)
	}

	// Pending: idempotent, nothing moves; with the same overrides too.
	ticket := j.Requested
	f.mustRetry(id, nil, false)
	f.mustRetry(id, &Overrides{Artist: ptr("Queen")}, false)
	if j := f.job(id); j.Requested != ticket || j.State != string(StatePending) {
		t.Fatalf("a retry of a pending job changed it: %+v", j)
	}
	// Other overrides while pending: refused, nothing changed.
	if _, err := f.retry(id, &Overrides{Title: ptr("Singles")}); Code(err) != CodeInProgress {
		t.Fatalf("other overrides on a pending job: %v, want %s", err, CodeInProgress)
	}
	// Running: idempotent, the claim kept.
	f.run(id)
	running := f.job(id)
	f.mustRetry(id, nil, false)
	if _, err := f.retry(id, &Overrides{}); Code(err) != CodeInProgress {
		t.Fatalf("other overrides on a running job: %v, want %s", err, CodeInProgress)
	}
	if j := f.job(id); j.Requested != running.Requested || *j.Claimed != *running.Claimed || j.State != string(StateRunning) {
		t.Fatalf("a retry of a running job changed it: %+v", j)
	}

	// Failed again: new overrides replace the stored ones; {} clears them.
	f.fail(id)
	f.mustRetry(id, &Overrides{Title: ptr("Singles")}, true)
	if o := overridesOf(t, f.job(id)); o.Artist != nil || o.Title == nil || *o.Title != "Singles" {
		t.Fatalf("overrides %+v, want exactly the new title", o)
	}
	f.fail(id)
	f.mustRetry(id, &Overrides{}, true)
	if raw := string(f.job(id).Overrides); raw != "{}" {
		t.Fatalf("overrides %s, want {}", raw)
	}
	// A value that is not normalized is refused by Encode, nothing changed.
	f.fail(id)
	failed := f.job(id)
	if _, err := f.retry(id, &Overrides{Title: ptr(" padded ")}); Code(err) != CodeInvalidOverrides {
		t.Fatalf("non-normalized override: %v, want %s", err, CodeInvalidOverrides)
	}
	if j := f.job(id); j.State != string(StateFailed) || j.Requested != failed.Requested {
		t.Fatalf("a refused retry changed the job: %+v", j)
	}
}

func TestRetryRefusals(t *testing.T) {
	f := newFixture(t)
	batch := f.batch()
	album := f.album("A")
	scan := f.scanJob(batch, time.Now())
	done := f.importJob(batch, "incoming/done", time.Now())
	skipped := f.importJob(batch, "incoming/skipped", time.Now())
	f.exec(`UPDATE jobs SET state = 'done', result_album_id = $2 WHERE id = $1`, done, album)
	f.exec(`UPDATE jobs SET state = 'skipped', result_album_id = $2, error_code = 'duplicate_import', error_message = 'x'
		WHERE id = $1`, skipped, album)
	for _, id := range []uuid.UUID{done, skipped} {
		before := f.job(id)
		if _, err := f.retry(id, nil); Code(err) != CodeNotRetryable {
			t.Errorf("retry of a %s job: %v, want %s", before.State, err, CodeNotRetryable)
		}
		if j := f.job(id); j.State != before.State || j.Requested != before.Requested {
			t.Errorf("a refused retry changed the %s job", before.State)
		}
	}
	// Overrides are for imports only (§7.3), whatever the state.
	f.fail(scan)
	render := f.enqueue(album).JobID
	f.fail(render)
	for _, id := range []uuid.UUID{scan, render} {
		if _, err := f.retry(id, &Overrides{Artist: ptr("X")}); Code(err) != CodeOverridesNotAllowed {
			t.Errorf("overrides on a %s job: %v, want %s", f.job(id).Kind, err, CodeOverridesNotAllowed)
		}
		if f.job(id).State != string(StateFailed) {
			t.Errorf("a refused retry changed the %s job", f.job(id).Kind)
		}
	}
	if _, err := f.retry(store.NewID(), nil); Code(err) != CodeNotFound {
		t.Fatalf("unknown job: %v, want %s", err, CodeNotFound)
	}
	// A failed scan is retried with no overrides.
	f.mustRetry(scan, nil, true)
	if j := f.job(scan); j.State != string(StatePending) || string(j.Overrides) != "{}" {
		t.Fatalf("retried scan %+v", j)
	}
}

// A render is retried through the single enqueue (§6.3): the album's one
// row, pending with a new ticket; a running render is left alone.
func TestRetryRender(t *testing.T) {
	f := newFixture(t)
	album := f.album("A")
	id := f.enqueue(album).JobID
	f.fail(id)
	before := f.job(id)
	f.mustRetry(id, nil, true)
	j := f.job(id)
	if j.ID != id || j.State != string(StatePending) || j.Requested <= before.Requested || j.ErrorCode != nil {
		t.Fatalf("retried render %+v", j)
	}
	f.run(id)
	running := f.job(id)
	f.mustRetry(id, nil, false)
	if j := f.job(id); j.Requested != running.Requested || *j.Claimed != *running.Claimed || j.State != string(StateRunning) {
		t.Fatalf("a retry of a running render changed it: %+v", j)
	}
	var n int
	if err := f.db.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE album_id = $1`, album).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d render rows for the album (%v), want 1", n, err)
	}
}

// retry-failed (§10.2): every failed job, and nothing else; a running job
// is never duplicated nor given a new ticket.
func TestRetryFailed(t *testing.T) {
	f := newFixture(t)
	batch := f.batch()
	failedImport := f.importJob(batch, "incoming/A", time.Now())
	f.exec(`UPDATE jobs SET overrides = '{"title": "T"}' WHERE id = $1`, failedImport)
	f.fail(failedImport)
	failedScan := f.scanJob(batch, time.Now())
	f.fail(failedScan)
	failedRender := f.enqueue(f.album("A")).JobID
	f.fail(failedRender)
	runningImport := f.importJob(batch, "incoming/B", time.Now())
	f.run(runningImport)
	runningRender := f.enqueue(f.album("B")).JobID
	f.run(runningRender)
	pending := f.importJob(batch, "incoming/C", time.Now())
	// Outside the batch root: a success under it would supersede the
	// failed scan (NOTES.md N-285).
	done := f.importJob(batch, "elsewhere/D", time.Now())
	f.exec(`UPDATE jobs SET state = 'done', result_album_id = $2 WHERE id = $1`, done, f.album("D"))

	untouched := map[uuid.UUID]store.Job{}
	for _, id := range []uuid.UUID{runningImport, runningRender, pending, done} {
		untouched[id] = f.job(id)
	}
	failed := map[uuid.UUID]store.Job{}
	for _, id := range []uuid.UUID{failedImport, failedScan, failedRender} {
		failed[id] = f.job(id)
	}
	var total int
	count := func() int {
		if err := f.db.QueryRow(context.Background(), `SELECT count(*) FROM jobs`).Scan(&total); err != nil {
			t.Fatal(err)
		}
		return total
	}
	rows := count()

	retryAll := func() int {
		var n int
		f.inTx(func(tx *store.CatalogTx) error {
			var err error
			n, err = RetryFailed(context.Background(), tx)
			return err
		})
		return n
	}
	if n := retryAll(); n != 3 {
		t.Fatalf("RetryFailed retried %d jobs, want 3", n)
	}
	for id, before := range failed {
		j := f.job(id)
		if j.State != string(StatePending) || j.Requested <= before.Requested || j.ErrorCode != nil || j.Claimed != nil {
			t.Errorf("%s job after retry-failed: %+v", j.Kind, j)
		}
	}
	if o := overridesOf(t, f.job(failedImport)); o.Title == nil || *o.Title != "T" {
		t.Errorf("the failed import lost its overrides: %+v", o)
	}
	for id, before := range untouched {
		j := f.job(id)
		if j.State != before.State || j.Requested != before.Requested || (before.Claimed != nil && *j.Claimed != *before.Claimed) ||
			!j.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("retry-failed changed a %s %s job: %+v", before.State, before.Kind, j)
		}
	}
	if count() != rows {
		t.Fatalf("retry-failed changed the number of jobs: %d, want %d", total, rows)
	}
	if n := retryAll(); n != 0 {
		t.Fatalf("a second retry-failed retried %d jobs, want 0", n)
	}
}
