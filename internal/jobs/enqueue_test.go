package jobs

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/store"
)

// The scan job of a batch is created once (§7.1, jobs_scan_batch_key).
func TestEnqueueScan(t *testing.T) {
	f := newFixture(t)
	b := f.batch()
	var first, second struct {
		id      uuid.UUID
		created bool
	}
	f.inTx(func(tx *store.CatalogTx) error {
		id, created, err := EnqueueScan(context.Background(), tx, b)
		first.id, first.created = id, created
		return err
	})
	f.inTx(func(tx *store.CatalogTx) error {
		id, created, err := EnqueueScan(context.Background(), tx, b)
		second.id, second.created = id, created
		return err
	})
	if !first.created || second.created || first.id != second.id {
		t.Errorf("first %v, second %v", first, second)
	}
	j := f.job(first.id)
	if j.Kind != "scan" || j.State != "pending" || *j.BatchID != b {
		t.Errorf("scan job %+v", j)
	}
}

// Import jobs (§7.2): pending, or failed with their reason; the same
// (batch, source) is never inserted twice, and an existing job is kept as
// it is; invalid sources and failures without a reason are refused.
func TestEnqueueImport(t *testing.T) {
	f := newFixture(t)
	b := f.batch()
	enqueue := func(j ImportJob) (bool, error) {
		var ok bool
		err := store.InCatalogTx(context.Background(), f.db, func(tx *store.CatalogTx) error {
			var err error
			ok, err = EnqueueImport(context.Background(), tx, b, j)
			return err
		})
		return ok, err
	}
	for _, tc := range []struct {
		job  ImportJob
		ok   bool
		code string
	}{
		{ImportJob{SourceRel: "A"}, true, ""},
		{ImportJob{SourceRel: ""}, true, ""},
		{ImportJob{SourceRel: "Bad", ErrorCode: "ambiguous_candidate", ErrorMessage: "why"}, true, ""},
		{ImportJob{SourceRel: "A", ErrorCode: "x", ErrorMessage: "the existing job is kept"}, false, ""},
		{ImportJob{SourceRel: "/abs"}, false, CodeInvalidArgument},
		{ImportJob{SourceRel: "a/../b"}, false, CodeInvalidArgument},
		{ImportJob{SourceRel: "C", ErrorCode: "Bad Code", ErrorMessage: "m"}, false, CodeInvalidResult},
		{ImportJob{SourceRel: "C", ErrorCode: "code"}, false, CodeInvalidResult},
		{ImportJob{SourceRel: "C", ErrorMessage: "m"}, false, CodeInvalidResult},
	} {
		ok, err := enqueue(tc.job)
		if ok != tc.ok || Code(err) != tc.code {
			t.Errorf("%+v: %v %v, want %v %q", tc.job, ok, err, tc.ok, tc.code)
		}
	}
	var state, code string
	if err := f.db.QueryRow(context.Background(), `SELECT state, coalesce(error_code, '') FROM jobs WHERE batch_id = $1 AND source_rel = 'A'`, b).
		Scan(&state, &code); err != nil || state != "pending" || code != "" {
		t.Errorf("A: %s %s %v", state, code, err)
	}
	if err := f.db.QueryRow(context.Background(), `SELECT state, error_code FROM jobs WHERE batch_id = $1 AND source_rel = 'Bad'`, b).
		Scan(&state, &code); err != nil || state != "failed" || code != "ambiguous_candidate" {
		t.Errorf("Bad: %s %s %v", state, code, err)
	}
}
