package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/store"
)

// Stale failures stop needing attention (owner, NOTES.md N-285): a failed
// scan or import can be dismissed, and one is superseded by a later
// successful import of its source. Neither touches the job's outcome.

func (f *fixture) dismiss(id uuid.UUID) (bool, error) {
	f.t.Helper()
	var changed bool
	err := store.InCatalogTx(context.Background(), f.db, func(tx *store.CatalogTx) error {
		var err error
		changed, err = Dismiss(context.Background(), tx, id)
		return err
	})
	return changed, err
}

// flag evaluates one of migration 00002's functions on a job.
func (f *fixture) flag(fn string, id uuid.UUID) bool {
	f.t.Helper()
	var b bool
	if err := f.db.QueryRow(context.Background(), `SELECT `+fn+`($1)`, id).Scan(&b); err != nil {
		f.t.Fatal(err)
	}
	return b
}

// failWith makes a job failed with code, as an executor would have.
func (f *fixture) failWith(id uuid.UUID, code string) {
	f.t.Helper()
	f.exec(`UPDATE jobs SET state = 'failed', claimed = NULL, error_code = $2, error_message = 'x' WHERE id = $1`, id, code)
}

func TestDismiss(t *testing.T) {
	f := newFixture(t)
	batch := f.batch()
	id := f.importJob(batch, "incoming/A", time.Now().Add(-time.Hour))
	f.fail(id)
	before := f.job(id)
	if changed, err := f.dismiss(id); err != nil || !changed {
		t.Fatalf("Dismiss = %v, %v", changed, err)
	}
	j := f.job(id)
	if j.DismissedAt == nil || j.State != before.State || j.Requested != before.Requested || !j.UpdatedAt.Equal(before.UpdatedAt) ||
		*j.ErrorCode != *before.ErrorCode || string(j.Warnings) != string(before.Warnings) {
		t.Fatalf("after the dismissal: %+v (before %+v)", j, before)
	}
	if f.flag("job_needs_attention", id) {
		t.Fatal("a dismissed failure still needs attention")
	}
	// Again: nothing changes, the first time is kept.
	if changed, err := f.dismiss(id); err != nil || changed {
		t.Fatalf("second Dismiss = %v, %v", changed, err)
	}
	if again := f.job(id); !again.DismissedAt.Equal(*j.DismissedAt) {
		t.Fatalf("the second dismissal moved the time: %v, want %v", again.DismissedAt, j.DismissedAt)
	}
	// A retry is a new attempt: no longer dismissed.
	f.mustRetry(id, nil, true)
	if j := f.job(id); j.DismissedAt != nil || j.State != string(StatePending) {
		t.Fatalf("a retried job kept its dismissal: %+v", j)
	}
	// The schema refuses a dismissed job that is not a failed scan or import.
	if _, err := f.db.Exec(context.Background(), `UPDATE jobs SET dismissed_at = now() WHERE id = $1`, id); err == nil {
		t.Fatal("a pending job was dismissed by SQL")
	}

	// Refusals: every other state, a failed render, an unknown job.
	album := f.album("A")
	render := f.enqueue(album).JobID
	f.fail(render)
	scan := f.scanJob(batch, time.Now())
	running := f.importJob(batch, "incoming/B", time.Now())
	f.run(running)
	done := f.importJob(batch, "incoming/C", time.Now())
	f.exec(`UPDATE jobs SET state = 'done', result_album_id = $2 WHERE id = $1`, done, album)
	for _, id := range []uuid.UUID{render, scan, running, done} {
		before := f.job(id)
		if _, err := f.dismiss(id); Code(err) != CodeNotDismissable {
			t.Errorf("Dismiss of a %s %s: %v, want %s", before.State, before.Kind, err, CodeNotDismissable)
		}
		if f.job(id).DismissedAt != nil {
			t.Errorf("a refused dismissal changed the %s %s", before.State, before.Kind)
		}
	}
	if !f.flag("job_needs_attention", render) {
		t.Fatal("a failed render must always need attention")
	}
	if _, err := f.dismiss(store.NewID()); Code(err) != CodeNotFound {
		t.Fatalf("unknown job: %v, want %s", err, CodeNotFound)
	}
	// A failed scan can be dismissed too.
	f.failWith(scan, "source_not_found")
	if changed, err := f.dismiss(scan); err != nil || !changed || f.flag("job_needs_attention", scan) {
		t.Fatalf("Dismiss of a failed scan = %v, %v", changed, err)
	}
}

// The supersede rule of N-285, case by case: the same source, or a path
// under it for a scan and the structural errors; only a success requested
// after the failure's last attempt.
func TestSuperseded(t *testing.T) {
	f := newFixture(t)
	album := f.album("A")
	failed := func(source, code string) uuid.UUID {
		t.Helper()
		id := f.importJob(f.batch(), source, time.Now())
		f.failWith(id, code)
		return id
	}
	succeed := func(source, state string) uuid.UUID {
		t.Helper()
		id := f.importJob(f.batch(), source, time.Now())
		f.exec(`UPDATE jobs SET state = $2, result_album_id = $3 WHERE id = $1`, id, state, album)
		return id
	}
	superseded := func(id uuid.UUID, want bool, why string) {
		t.Helper()
		if got := f.flag("job_superseded", id); got != want {
			t.Errorf("%s: superseded %v, want %v", why, got, want)
		}
		if got := f.flag("job_needs_attention", id); got == want {
			t.Errorf("%s: needs attention %v, want %v", why, got, !want)
		}
	}

	// An earlier success does not supersede a later failure.
	succeed("early/X", "done")
	early := failed("early/X", "mixed_album")
	superseded(early, false, "a success requested before the failure")

	// The same source, done or skipped; a later failure of it does not.
	mixed := failed("Álvaro Soler/A Contracorriente", "mixed_album")
	sameFailed := failed("Álvaro Soler/A Contracorriente", "source_changed")
	superseded(mixed, false, "a later failure of the same source")
	succeed("Álvaro Soler/A Contracorriente", "done")
	superseded(mixed, true, "a later import of the same source, done")
	superseded(sameFailed, true, "a later import of the same source, done (another code)")
	present := failed("B/Y", "corrupt_audio")
	succeed("B/Y", "skipped")
	superseded(present, true, "a later import of the same source, already there")

	// Under the source: only the structural errors, and never a sibling
	// sharing a prefix of the name.
	under := failed("C", "mixed_album")
	succeed("C/Z", "done")
	superseded(under, false, "mixed_album with a success under it")
	ambiguous := failed("Álvaro Soler", "ambiguous_candidate")
	succeed("Álvaro Soler Live/Z", "done")
	superseded(ambiguous, false, "a success in a sibling with the same prefix")
	succeed("Álvaro Soler/Ferrari", "done")
	superseded(ambiguous, true, "ambiguous_candidate with a success under it")
	root := failed("", "not_a_candidate")
	succeed("James Hype/Ferrari (Remix)", "done")
	superseded(root, true, "not_a_candidate at the root with a success anywhere")

	// A scan: its batch root, and anything under it.
	batch := f.batch()
	f.exec(`UPDATE import_batches SET root_rel = 'D' WHERE id = $1`, batch)
	scan := f.scanJob(batch, time.Now())
	f.failWith(scan, "no_valid_candidate")
	superseded(scan, false, "a failed scan, nothing later")
	succeed("DD/X", "done")
	superseded(scan, false, "a scan with a success in a sibling")
	succeed("D/X", "done")
	superseded(scan, true, "a scan with a success under its root")

	// A pending or done job is never superseded, a render never.
	pending := f.importJob(f.batch(), "E", time.Now())
	succeed("E", "done")
	if f.flag("job_superseded", pending) || f.flag("job_needs_attention", pending) {
		t.Error("a pending job is superseded or needs attention")
	}
	render := f.enqueue(album).JobID
	f.fail(render)
	if f.flag("job_superseded", render) || !f.flag("job_needs_attention", render) {
		t.Error("a failed render is superseded or needs no attention")
	}
}

// A scan that found no valid candidate is not listed while its batch has
// import jobs, whose own rows say what went wrong.
func TestNoValidCandidateAttention(t *testing.T) {
	f := newFixture(t)
	batch := f.batch()
	scan := f.scanJob(batch, time.Now())
	f.failWith(scan, "no_valid_candidate")
	if !f.flag("job_needs_attention", scan) {
		t.Fatal("a scan that found nothing needs attention")
	}
	branch := f.importJob(batch, "incoming/A", time.Now())
	f.failWith(branch, "ambiguous_candidate")
	if f.flag("job_needs_attention", scan) || !f.flag("job_needs_attention", branch) {
		t.Fatal("the scan and its failed branch are two rows for one problem")
	}
}

// «Retry all» leaves the dismissed and the superseded failures alone.
func TestRetryFailedSkipsSettled(t *testing.T) {
	f := newFixture(t)
	album := f.album("A")
	open := f.importJob(f.batch(), "incoming/open", time.Now())
	f.fail(open)
	dismissed := f.importJob(f.batch(), "incoming/dismissed", time.Now())
	f.fail(dismissed)
	if _, err := f.dismiss(dismissed); err != nil {
		t.Fatal(err)
	}
	stale := f.importJob(f.batch(), "incoming/stale", time.Now())
	f.fail(stale)
	later := f.importJob(f.batch(), "incoming/stale", time.Now())
	f.exec(`UPDATE jobs SET state = 'done', result_album_id = $2 WHERE id = $1`, later, album)
	settled := map[uuid.UUID]store.Job{dismissed: f.job(dismissed), stale: f.job(stale)}
	var n int
	f.inTx(func(tx *store.CatalogTx) error {
		var err error
		n, err = RetryFailed(context.Background(), tx)
		return err
	})
	if n != 1 || f.job(open).State != string(StatePending) {
		t.Fatalf("RetryFailed retried %d, the open failure is %s", n, f.job(open).State)
	}
	for id, before := range settled {
		j := f.job(id)
		if j.State != string(StateFailed) || j.Requested != before.Requested || !j.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("RetryFailed changed a settled failure: %+v", j)
		}
	}
}
