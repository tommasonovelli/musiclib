package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

// Codes of the import batches (§7.1).
const (
	// CodeImportBatchConflict: the request id is already a batch of another
	// root (§7.1: "con parametri diversi restituisce 409").
	CodeImportBatchConflict = "import_batch_conflict"
	// CodeImportBatchNotFound: no batch has that id.
	CodeImportBatchNotFound = "import_batch_not_found"
)

// ImportBatch is an import_batches row with its scan job.
type ImportBatch struct {
	ID uuid.UUID
	// RootRel is relative to /import, exactly as on disk; "" is /import
	// itself (§5.2).
	RootRel   string
	CreatedAt time.Time
	ScanJobID uuid.UUID
	// Created is true when this call created the batch, false when the same
	// request was repeated (§7.1).
	Created bool
}

// CreateImportBatch is the catalog transaction behind POST /api/imports
// (§7.1): id is the client's request UUID, rootRel the directory to import,
// relative to /import and kept exactly as on disk ("" is /import itself,
// §5.2). It creates the batch and its scan job. The same id with the same
// root returns the same batch (Created false); with another root it is
// CodeImportBatchConflict, naming the root already recorded. The root is
// validated as a path (§5.2), not looked up on disk: the scan does that.
func (s *Service) CreateImportBatch(ctx context.Context, id uuid.UUID, rootRel string) (ImportBatch, error) {
	if id == uuid.Nil {
		return ImportBatch{}, errorf(CodeInvalidArgument, "an import batch needs a request id")
	}
	if _, err := names.SplitRelPathOrRoot(rootRel); err != nil {
		return ImportBatch{}, &Error{Code: names.Code(err), Message: fmt.Sprintf("import root %q", rootRel), Err: err}
	}
	var b ImportBatch
	err := store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		b = ImportBatch{}
		if err := tx.InsertImportBatch(ctx, store.InsertImportBatchParams{ID: id, RootRel: rootRel}); err != nil {
			return dbErr("creating import batch "+id.String(), err)
		}
		row, err := tx.GetImportBatch(ctx, id)
		if err != nil {
			return dbErr("reading import batch "+id.String(), err)
		}
		if row.RootRel != rootRel {
			return &Error{
				Code:    CodeImportBatchConflict,
				Message: fmt.Sprintf("request %s is already the import of %q, not %q", id, row.RootRel, rootRel),
				Details: Details{Path: row.RootRel},
			}
		}
		scan, created, err := jobs.EnqueueScan(ctx, tx, id)
		if err != nil {
			return err
		}
		b = ImportBatch{ID: row.ID, RootRel: row.RootRel, CreatedAt: row.CreatedAt, ScanJobID: scan, Created: created}
		return nil
	})
	if err != nil {
		return ImportBatch{}, err
	}
	if b.Created {
		s.notify()
	}
	return b, nil
}

// GetImportBatch reads a batch. CodeImportBatchNotFound if there is none.
func (s *Service) GetImportBatch(ctx context.Context, id uuid.UUID) (ImportBatch, error) {
	q := store.New(s.db)
	row, err := q.GetImportBatch(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ImportBatch{}, errorf(CodeImportBatchNotFound, "import batch %s does not exist", id)
	}
	if err != nil {
		return ImportBatch{}, dbErr("reading import batch "+id.String(), err)
	}
	scan, err := q.GetScanJob(ctx, &id)
	if err != nil {
		return ImportBatch{}, dbErr("reading the scan job of batch "+id.String(), err)
	}
	return ImportBatch{ID: row.ID, RootRel: row.RootRel, CreatedAt: row.CreatedAt, ScanJobID: scan.ID}, nil
}

// ScanOutcome is what a scan found (§7.2): the import jobs to insert and the
// scan's own outcome.
type ScanOutcome struct {
	// Attempt is the running scan job's attempt.
	Attempt jobs.Attempt
	BatchID uuid.UUID
	// Imports are the candidates, pending, and the ambiguous branches,
	// already failed with their path and reason.
	Imports []jobs.ImportJob
	// Result is the scan's outcome: done with its warnings (the unassigned
	// files), or failed with the explanation of why no candidate is valid.
	Result jobs.Result
}

// CommitScan records a scan in one transaction under the catalog lock
// (§7.2): every import job, then the scan's outcome, conditioned on its
// attempt. An import job already present for the same (batch, source) is
// kept, so repeating a scan after a crash is safe. An attempt that is no
// longer the running one is jobs.CodeAttemptStale and nothing is written.
// It returns how many import jobs were inserted now.
func (s *Service) CommitScan(ctx context.Context, o ScanOutcome) (int, error) {
	var inserted, pending int
	err := store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		inserted, pending = 0, 0
		for _, imp := range o.Imports {
			ok, err := jobs.EnqueueImport(ctx, tx, o.BatchID, imp)
			if err != nil {
				return err
			}
			if ok {
				inserted++
				if imp.ErrorCode == "" {
					pending++
				}
			}
		}
		return jobs.Finish(ctx, tx, jobs.KindScan, o.Attempt, o.Result)
	})
	if err != nil {
		return 0, err
	}
	if pending > 0 {
		s.notify()
	}
	return inserted, nil
}

// FailJob records the failure of a scan or import attempt that did not
// reach its commit (§6.4: content, space or permission errors, visible,
// with no automatic retry). CodeAttemptStale if the attempt is no longer
// the running one.
func (s *Service) FailJob(ctx context.Context, kind jobs.Kind, a jobs.Attempt, code, message string, warnings []jobs.Warning) error {
	return store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		return jobs.Finish(ctx, tx, kind, a, jobs.Result{
			State: jobs.StateFailed, ErrorCode: code, ErrorMessage: message, Warnings: warnings,
		})
	})
}
