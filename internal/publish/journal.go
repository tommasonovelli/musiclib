package publish

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/render"
	"musiclib/internal/store"
)

// Journal is the publication row (§4.2, §9.3): the one publication in
// progress, durable from PREPARE's commit to FINALIZE's. Paths are relative
// to library/ and exact (§5.3: on ext4 "Abba/X" and "ABBA/X" are two
// paths); a zero value is NULL.
type Journal struct {
	AlbumID uuid.UUID
	// Ticket is the claimed ticket of the attempt that built the output:
	// FINALIZE completes the album's render job by it (§6.4).
	Ticket int64
	// Revision and Renderer are the revision built and its render_version,
	// never the album's current ones (§6.3).
	Revision int64
	Renderer string
	// BuildID names the staging work/render/<build_id>/album and the
	// retired directory work/retired/<build_id> (§3.1, §9.3).
	BuildID uuid.UUID
	// ReceiptHash is the SHA-256 of the new output's receipt; "" for a
	// removal.
	ReceiptHash string
	// OldPath and OldBuild are the album's published path and build when
	// the journal was written; "" and uuid.Nil if it had no output.
	OldPath  string
	OldBuild uuid.UUID
	// NewPath is the album's directory after the publication; "" removes
	// the album from the library.
	NewPath string
}

// Removal reports whether the journal takes the album out of the library.
func (j Journal) Removal() bool { return j.NewPath == "" }

// staging is the build's album directory, relative to work/.
func (j Journal) staging() string { return render.StagingDir(j.BuildID) }

// retired is where the old directory goes, relative to work/.
func (j Journal) retired() string { return retiredDir + "/" + j.BuildID.String() }

// renames reports whether the old output must be retired: it exists and is
// not at the new path. The comparison is on the exact paths, never on the
// keys (§9.3).
func (j Journal) renames() bool { return j.OldPath != "" && j.OldPath != j.NewPath }

func (j Journal) String() string {
	return fmt.Sprintf("publication of album %s revision %d build %s (%q -> %q)", j.AlbumID, j.Revision, j.BuildID, j.OldPath, j.NewPath)
}

// validate checks the shape the protocol relies on: an album directory is
// <artist>/<album>, two valid segments (§5.1, §5.2).
func (j Journal) validate() error {
	if j.AlbumID == uuid.Nil || j.BuildID == uuid.Nil || j.Revision <= 0 || j.Renderer == "" || j.Ticket <= 0 {
		return errorf(CodeInvalidArgument, "%s: an id, the revision, the renderer or the ticket is missing", j)
	}
	if (j.NewPath == "") != (j.ReceiptHash == "") {
		return errorf(CodeInvalidArgument, "%s: a new path needs a receipt hash, and a removal has none", j)
	}
	if (j.OldPath == "") != (j.OldBuild == uuid.Nil) {
		return errorf(CodeInvalidArgument, "%s: the old path and the old build go together", j)
	}
	for _, p := range []string{j.OldPath, j.NewPath} {
		if p == "" {
			continue
		}
		segs, err := names.SplitRelPath(p)
		if err != nil || len(segs) != 2 {
			return wrap(CodeInvalidArgument, err, "%s: %q is not an <artist>/<album> path", j, p)
		}
	}
	return nil
}

// artistDir is the artist directory of an album path.
func artistDir(albumPath string) string {
	dir, _, _ := strings.Cut(albumPath, "/")
	return dir
}

// journalFor is the journal that publishing res, built from snap, would
// write (§9.3 A).
func journalFor(snap *jobs.RenderSnapshot, res render.Result) (Journal, error) {
	if snap == nil {
		return Journal{}, errorf(CodeInvalidArgument, "no snapshot")
	}
	if res.AlbumID != snap.Album.ID || res.AlbumRevision != snap.Album.Revision || res.RenderVersion != snap.RenderVersion {
		return Journal{}, errorf(CodeInvalidArgument, "the build (album %s revision %d, %s) is not the snapshot's (album %s revision %d, %s)",
			res.AlbumID, res.AlbumRevision, res.RenderVersion, snap.Album.ID, snap.Album.Revision, snap.RenderVersion)
	}
	j := Journal{
		AlbumID: res.AlbumID, Ticket: snap.Attempt.Ticket, Revision: res.AlbumRevision, Renderer: res.RenderVersion,
		BuildID: res.BuildID, OldPath: snap.Album.PublishedPath, OldBuild: snap.Album.PublishedBuild,
	}
	if res.Removal != (res.Dir == "") || res.Removal != (res.Staging == "") {
		return Journal{}, errorf(CodeInvalidArgument, "build %s: a removal has no directory and no staging, an album both", res.BuildID)
	}
	if !res.Removal {
		if res.Staging != render.StagingDir(res.BuildID) {
			return Journal{}, errorf(CodeInvalidArgument, "build %s: staging %q", res.BuildID, res.Staging)
		}
		j.NewPath, j.ReceiptHash = res.Dir, res.ReceiptHash
	}
	return j, j.validate()
}

// fromRow is the journal of a publication row.
func fromRow(r store.Publication) Journal {
	return Journal{
		AlbumID: r.AlbumID, Ticket: r.Ticket, Revision: r.Revision, Renderer: r.Renderer, BuildID: r.BuildID,
		ReceiptHash: deref(r.ReceiptHash), OldPath: deref(r.OldPath), OldBuild: deref(r.OldBuild), NewPath: deref(r.NewPath),
	}
}

// Outcome is how PREPARE, and so a call to Publish, ended.
type Outcome string

const (
	// Published: FINALIZE committed; the output is the build's.
	Published Outcome = "published"
	// Superseded: the ticket, the revision or the renderer changed since
	// the claim (§6.3); the job is pending again and the build discarded.
	// No journal was written.
	Superseded Outcome = "superseded"
	// Refused: the album's claims were stale and another album owns one of
	// its paths (N-112); the render failed with path_reserved, the build
	// was discarded, and no journal was written. Never requeued.
	Refused Outcome = "refused"
	// prepared: the journal is committed (internal).
	prepared Outcome = "prepared"
)

// prepare is §9.3 A, one short transaction under the catalog lock:
// CheckFresh (§6.3); a superseded build is requeued; stale claims are
// repaired or the render fails (N-112); otherwise the journal row is
// inserted with the published output of the album as the old one, and the
// claims are reconciled so that the journal's paths are the album's (§5.3).
func (p *Publisher) prepare(ctx context.Context, snap *jobs.RenderSnapshot, j Journal) (Outcome, error) {
	var out Outcome
	err := store.InCatalogTx(ctx, p.db, func(tx *store.CatalogTx) error {
		out = ""
		fresh, err := catalog.CheckFresh(ctx, tx, snap, p.renderVersion)
		if err != nil {
			return err
		}
		switch fresh {
		case catalog.Fresh:
		case catalog.StaleTicket, catalog.StaleRevision, catalog.StaleRenderer:
			if err := jobs.RequeueRender(ctx, tx, snap.Attempt); err != nil {
				return err
			}
			out = Superseded
			return nil
		case catalog.StaleClaims:
			// N-112: nothing in the queue changes the claims, so a requeue
			// would rebuild and be refused forever. Repair them if the paths
			// are free; otherwise fail the render, naming the owner.
			if err := catalog.ReconcileClaims(ctx, tx, snap.Album.ID); err != nil {
				if catalog.Code(err) != catalog.CodePathReserved {
					return err
				}
				e, _ := catalog.AsError(err)
				if _, err := jobs.FailRender(ctx, tx, snap.Attempt, catalog.CodePathReserved, e.Message); err != nil {
					return err
				}
				out = Refused
				return nil
			}
		default:
			return errorf(CodeInvalidArgument, "unknown staleness %q", fresh)
		}
		if _, err := tx.GetJournalForUpdate(ctx); err == nil {
			return errorf(CodeJournalPending, "a publication journal is already pending: the boot must recover it first (§9.4)")
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return dbErr("reading the publication journal", err)
		}
		al, err := tx.GetAlbum(ctx, j.AlbumID)
		if err != nil {
			return dbErr("reading album "+j.AlbumID.String(), err)
		}
		if deref(al.PublishedPath) != j.OldPath || deref(al.PublishedBuild) != j.OldBuild {
			return errorf(CodeStateChanged, "album %s is published at %q (build %s), the preflight checked %q (build %s)",
				j.AlbumID, deref(al.PublishedPath), deref(al.PublishedBuild), j.OldPath, j.OldBuild)
		}
		if err := tx.InsertJournal(ctx, store.InsertJournalParams{
			AlbumID: j.AlbumID, Ticket: j.Ticket, Revision: j.Revision, Renderer: j.Renderer, BuildID: j.BuildID,
			ReceiptHash: ptr(j.ReceiptHash), OldPath: ptr(j.OldPath), OldBuild: uuidPtr(j.OldBuild), NewPath: ptr(j.NewPath),
		}); err != nil {
			return dbErr("inserting the publication journal", err)
		}
		// §5.3: the union now includes old_path and new_path.
		if err := catalog.ReconcileClaims(ctx, tx, j.AlbumID); err != nil {
			return err
		}
		out = prepared
		return nil
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// finalize is §9.3 C, one short transaction under the catalog lock: the
// published state written from the journal's values (never the album's
// current revision, §6.3), the job completed by the ticket comparison
// (§6.4), the journal deleted and the claims reconciled to the current
// union (§5.3). It returns what happened to the job; "" when the album's
// render row is not running with the journal's ticket, which the protocol
// never leaves (only the attempt holding the journal completes it), and
// which must not keep a completed publication pending: it is logged.
func (p *Publisher) finalize(ctx context.Context, j Journal) (jobs.RenderOutcome, error) {
	var jo jobs.RenderOutcome
	var jobState string
	err := store.InCatalogTx(ctx, p.db, func(tx *store.CatalogTx) error {
		jo, jobState = "", ""
		row, err := tx.GetJournalForUpdate(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(CodeIllegalState, "%s: the journal disappeared before FINALIZE", j)
		}
		if err != nil {
			return dbErr("reading the publication journal", err)
		}
		if got := fromRow(row); got != j {
			return errorf(CodeIllegalState, "the journal is %s, not %s", got, j)
		}
		pub := store.SetAlbumPublishedParams{ID: j.AlbumID, Revision: j.Revision, Renderer: &j.Renderer}
		if !j.Removal() {
			pub.Path, pub.Build, pub.ReceiptHash = &j.NewPath, &j.BuildID, &j.ReceiptHash
		}
		if n, err := tx.SetAlbumPublished(ctx, pub); err != nil || n != 1 {
			return dbErr(fmt.Sprintf("recording the publication of album %s (%d rows)", j.AlbumID, n), err)
		}
		job, err := tx.GetRenderJobForUpdate(ctx, &j.AlbumID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			jobState = "none"
		case err != nil:
			return dbErr("reading the render job of album "+j.AlbumID.String(), err)
		case job.State == string(jobs.StateRunning) && deref(job.Claimed) == j.Ticket:
			if jo, err = jobs.FinishRender(ctx, tx, jobs.Attempt{JobID: job.ID, Ticket: j.Ticket}); err != nil {
				return err
			}
		default:
			jobState = fmt.Sprintf("%s with claim %d", job.State, deref(job.Claimed))
		}
		if n, err := tx.DeleteJournal(ctx, store.DeleteJournalParams{AlbumID: j.AlbumID, BuildID: j.BuildID}); err != nil || n != 1 {
			return dbErr(fmt.Sprintf("deleting the publication journal (%d rows)", n), err)
		}
		return catalog.ReconcileClaims(ctx, tx, j.AlbumID)
	})
	if err != nil {
		return "", err
	}
	if jobState != "" {
		p.log.Warn("FINALIZE found no render attempt with the journal's ticket; the job is left as it is",
			"album_id", j.AlbumID, "build_id", j.BuildID, "ticket", j.Ticket, "job", jobState)
	}
	return jo, nil
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// ptr is a nullable text column: "" is NULL.
func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func uuidPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
