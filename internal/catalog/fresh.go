package catalog

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// Staleness is why a built render may no longer be published (§6.3). The
// zero value means the build is still current.
type Staleness string

// The four conditions that PREPARE rechecks under the catalog lock (§6.3,
// §9.3 A), in this order.
const (
	Fresh Staleness = ""
	// StaleTicket: the job is not running with the snapshot's ticket, or
	// a newer request exists (requested != claimed).
	StaleTicket Staleness = "ticket"
	// StaleRevision: the album's revision is not the snapshot's.
	StaleRevision Staleness = "revision"
	// StaleRenderer: the build's renderer is not the current one.
	StaleRenderer Staleness = "renderer"
	// StaleClaims: the album does not own every path of its §5.3 union.
	StaleClaims Staleness = "claims"
)

// CheckFresh is the recheck before PREPARE (§6.3): in the publisher's
// PREPARE transaction, under the catalog lock, it tells whether the render
// built from snap may still be published:
//
//   - requested == claimed == the snapshot's ticket, the job running;
//   - the album's revision equal to the snapshot's;
//   - the snapshot's renderer equal to currentRenderer;
//   - the album owning every path of its current claims union (§5.3).
//
// It only reads. A stale ticket, revision or renderer means the work was
// superseded: the caller discards the build and calls jobs.RequeueRender
// in the same transaction (§6.3, §9.3 A). StaleClaims cannot come from a
// normal change, since every change re-derives the claims in its own
// transaction; what to do about it is the publisher's decision.
//
// It lives here and not in package jobs because the claims union is the
// catalog's (ReconcileClaims), and the catalog depends on the queue, not
// the reverse.
func CheckFresh(ctx context.Context, tx *store.CatalogTx, snap *jobs.RenderSnapshot, currentRenderer string) (Staleness, error) {
	st, err := jobs.LockStatus(ctx, tx, snap.Attempt.JobID)
	if jobs.Code(err) == jobs.CodeNotFound {
		return StaleTicket, nil
	}
	if err != nil {
		return "", err
	}
	if !st.Runs(snap.Attempt) || st.Requested != st.Claimed {
		return StaleTicket, nil
	}
	al, err := tx.GetAlbum(ctx, snap.Album.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errorf(CodeAlbumNotFound, "album %s does not exist", snap.Album.ID)
	}
	if err != nil {
		return "", dbErr("reading album "+snap.Album.ID.String(), err)
	}
	if al.Revision != snap.Album.Revision {
		return StaleRevision, nil
	}
	if snap.RenderVersion != currentRenderer {
		return StaleRenderer, nil
	}
	held, err := claimsHeld(ctx, tx, snap.Album.ID)
	if err != nil {
		return "", err
	}
	if !held {
		return StaleClaims, nil
	}
	return Fresh, nil
}
