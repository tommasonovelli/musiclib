package publish

import (
	"context"
	"errors"
	"fmt"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/render"
	"musiclib/internal/store"
)

// ExecuteRender runs a claimed render job, in the form jobs.Pool expects:
// the claim's snapshot through the pure plan (render.NewPlan) and the build
// (render.Builder.Build, §9.1), then Publish (§9.3). Every path that is not
// a shutdown or a stop ends in a completion (N-107):
//
//   - published: FINALIZE deleted the job, or put it back to pending for a
//     newer request (§6.4);
//   - superseded before PREPARE: pending again (§6.3);
//   - stale claims owned by another album: failed with path_reserved, never
//     requeued (N-112);
//   - any other failure before the journal (a plan, a build, a preflight
//     conflict): jobs.FailRender, which records failed only if the ticket is
//     still the requested one and otherwise leaves the newer request
//     pending (§6.4). The build is discarded.
//
// A shutdown (ctx ended before PREPARE) completes nothing: the job stays
// running for the next boot (§11.1 step 5). A fatal store error, or any
// failure after PREPARE, is returned marked with jobs.Stop: the pool stops
// and the journal is recovered at the next boot (§9.4).
func (p *Publisher) ExecuteRender(ctx context.Context, c *jobs.Claim) error {
	if c == nil || c.Kind != jobs.KindRender || c.Render == nil {
		return errorf(CodeInvalidArgument, "ExecuteRender needs a render claim")
	}
	snap := c.Render
	plan, err := render.NewPlan(snap, p.renderVersion)
	if err != nil {
		return p.failBeforeJournal(ctx, c.Attempt, err)
	}
	res, err := p.builder.Build(ctx, plan)
	if err != nil {
		return p.failBeforeJournal(ctx, c.Attempt, err)
	}
	// The staging holds its space until it is installed or discarded
	// (§11.2, N-114); both are over when Publish returns.
	defer res.Space.Release()
	rep, err := p.Publish(ctx, snap, res)
	if err != nil {
		if jobs.Stops(err) {
			return err
		}
		return p.failBeforeJournal(ctx, c.Attempt, err)
	}
	p.log.Info("render finished", "job_id", c.Attempt.JobID, "album_id", snap.Album.ID, "revision", snap.Album.Revision,
		"build_id", res.BuildID, "outcome", rep.Outcome, "job", rep.Job)
	return nil
}

// failBeforeJournal completes a render that failed before PREPARE (§6.4):
// failed with err's code and message if its ticket is still the requested
// one, pending for the newer request otherwise. A shutdown or a fatal
// store error is returned as it is and completes nothing.
func (p *Publisher) failBeforeJournal(ctx context.Context, a jobs.Attempt, err error) error {
	if jobs.Stops(err) || ctx.Err() != nil {
		return err
	}
	code := Code(err)
	if code == "" {
		code = "render_failed"
	}
	var out jobs.RenderOutcome
	ferr := store.InCatalogTx(ctx, p.db, func(tx *store.CatalogTx) error {
		var err2 error
		// No database text in a message the user sees (§10.1, N-150).
		out, err2 = jobs.FailRender(ctx, tx, a, code, catalog.JobMessage(err, err.Error()))
		return err2
	})
	if ferr != nil {
		return errors.Join(fmt.Errorf("%s: %w", a, err), ferr)
	}
	p.log.Info("render failed before the journal", "job_id", a.JobID, "code", code, "job", out, "error", err.Error())
	return nil
}
