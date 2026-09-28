package catalog_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// The reads of the Import and Activity views (NOTES.md N-286 to N-289) and
// the dismissal of N-285, on the real catalog.

func jobIDsOf(g catalog.ActivityGroup) []uuid.UUID {
	var out []uuid.UUID
	for _, j := range g.Jobs {
		out = append(out, j.ID)
	}
	return out
}

func TestListActivity(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Three albums: three pending renders, in queue order.
	a := e.importAlbum("Miles Davis", "Kind of Blue")
	b := e.importAlbum("Bill Evans", "Portrait in Jazz")
	c := e.importAlbum("Chet Baker", "Chet")
	e.exec(`UPDATE jobs SET queued_at = now() - interval '3 minutes' WHERE kind = 'render' AND album_id = $1`, a)
	e.exec(`UPDATE jobs SET queued_at = now() - interval '2 minutes' WHERE kind = 'render' AND album_id = $1`, b)
	e.exec(`UPDATE albums SET cover_hash = NULL WHERE id = $1`, c)
	render := func(album uuid.UUID) uuid.UUID {
		t.Helper()
		j, ok := e.renderJob(album)
		if !ok {
			t.Fatalf("no render job for %s", album)
		}
		return j.ID
	}
	// One of them running, one failed.
	e.exec(`UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1`, render(c))
	newBatch := func(root string) uuid.UUID {
		t.Helper()
		id := store.NewID()
		e.exec(`INSERT INTO import_batches (id, root_rel, created_at) VALUES ($1, $2, now())`, id, root)
		return id
	}
	batch := newBatch("Rock")
	scan := store.NewID()
	e.exec(`INSERT INTO jobs (id, kind, batch_id, state, queued_at, updated_at, error_code, error_message)
		VALUES ($1, 'scan', $2, 'failed', now(), now() - interval '5 minutes', 'source_not_found', 'gone')`, scan, batch)
	failed := e.importJob(batch, "Rock/Mixed", "failed")
	e.exec(`UPDATE jobs SET updated_at = now() - interval '1 minute' WHERE id = $1`, failed)
	dismissed := e.importJob(batch, "Rock/Dismissed", "dismissed")
	stale := e.importJob(batch, "Rock/Fixed", "failed")
	e.exec(`UPDATE jobs SET updated_at = now() - interval '3 minutes' WHERE id = $1`, stale)
	waiting := e.importJob(batch, "Rock/Waiting", "pending")
	e.importJob(batch, "Rock/Waiting 2", "pending")
	e.exec(`UPDATE jobs SET error_code = 'render_io', error_message = 'disk', state = 'failed', claimed = NULL,
		updated_at = now() - interval '10 minutes' WHERE id = $1`, render(b))

	got, err := e.svc.ListActivity(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ids := jobIDsOf(got.Running); !slices.Equal(ids, []uuid.UUID{render(c)}) || got.Running.Total != 1 {
		t.Fatalf("running %v (%d)", ids, got.Running.Total)
	}
	if r := got.Running.Jobs[0]; r.Album == nil || r.Album.Title != "Chet" || r.Album.ArtistName != "Chet Baker" || r.Album.Cover || r.Kind != jobs.KindRender {
		t.Fatalf("running render %+v %+v", r, r.Album)
	}
	// Pending in queue order, two of three: the render queued first.
	if ids := jobIDsOf(got.Pending); !slices.Equal(ids, []uuid.UUID{render(a), waiting}) || got.Pending.Total != 3 || !got.Pending.Jobs[0].Album.Cover {
		t.Fatalf("pending %v (%d)", ids, got.Pending.Total)
	}
	// Needing attention, newest first, two of four; never the dismissed one.
	if ids := jobIDsOf(got.Failed); !slices.Equal(ids, []uuid.UUID{failed, stale}) || got.Failed.Total != 4 {
		t.Fatalf("failed %v (%d)", ids, got.Failed.Total)
	}
	if f := got.Failed.Jobs[0]; f.Folder != "Rock/Mixed" || f.Album != nil || f.ErrorCode == nil || *f.ErrorCode != "mixed_album" {
		t.Fatalf("failed row %+v", f)
	}
	all, err := e.svc.ListActivity(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if ids := jobIDsOf(all.Failed); !slices.Equal(ids, []uuid.UUID{failed, stale, scan, render(b)}) || slices.Contains(ids, dismissed) {
		t.Fatalf("all failed %v", ids)
	}
	if f := all.Failed.Jobs[2]; f.Folder != "Rock" || f.Kind != jobs.KindScan {
		t.Fatalf("the scan names its batch root: %+v", f)
	}
	if f := all.Failed.Jobs[3]; f.Album == nil || f.Album.Title != "Portrait in Jazz" || f.Folder != "" {
		t.Fatalf("the failed render names its album: %+v", f)
	}
	// A later import of the same folder succeeds: the stale failure goes.
	e.importJob(newBatch("Rock/Fixed"), "Rock/Fixed", "done")
	all, err = e.svc.ListActivity(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	// ...and so does the failed scan of the batch root above it.
	if ids := jobIDsOf(all.Failed); !slices.Equal(ids, []uuid.UUID{failed, render(b)}) || all.Failed.Total != 2 {
		t.Fatalf("a superseded failure is listed: %v (%d)", ids, all.Failed.Total)
	}
	if _, err := e.svc.ListActivity(ctx, 0); err == nil {
		t.Fatal("a limit of 0 was accepted")
	}
}

func TestRecentImportsCardsAndRenderAllCount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	old := store.NewID()
	e.exec(`INSERT INTO import_batches (id, root_rel, created_at) VALUES ($1, '', now() - interval '2 days')`, old)
	for _, st := range []string{"done", "done", "skipped", "failed", "dismissed"} {
		e.importJob(old, "In/"+store.NewID().String(), st)
	}
	recent := store.NewID()
	e.exec(`INSERT INTO import_batches (id, root_rel, created_at) VALUES ($1, 'Jazz', now())`, recent)
	e.exec(`INSERT INTO jobs (id, kind, batch_id, state, queued_at, updated_at) VALUES ($1, 'scan', $2, 'pending', now(), now())`, store.NewID(), recent)
	got, err := e.svc.ListRecentImports(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's own batch (e.batch) has the imports of importAlbum.
	var ids []uuid.UUID
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if len(got) < 2 || got[0].ID != recent || got[0].RootRel != "Jazz" || got[0].Active != 1 || got[0].Imported != 0 {
		t.Fatalf("recent imports %+v", got)
	}
	var o catalog.RecentImport
	for _, r := range got {
		if r.ID == old {
			o = r
		}
	}
	if o.Imported != 2 || o.Present != 1 || o.Attention != 1 || o.Active != 0 || o.RootRel != "" {
		t.Fatalf("old batch %+v", o)
	}

	a := e.importAlbum("Miles Davis", "Kind of Blue")
	gone := e.importAlbum("Chet Baker", "Chet")
	if _, _, err := e.svc.TrashAlbum(ctx, gone, e.album(gone).Revision); err != nil {
		t.Fatal(err)
	}
	cards, err := e.svc.AlbumCards(ctx, []uuid.UUID{a, gone, store.NewID()})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 || cards[a].Title != "Kind of Blue" || cards[a].ArtistName != "Miles Davis" || !cards[a].Cover ||
		cards[a].Trashed || !cards[gone].Trashed {
		t.Fatalf("cards %+v", cards)
	}
	// Every active album, and the trashed ones still published: the albums
	// of the done/skipped fixtures count too, as RenderAll would enqueue.
	n, err := e.svc.CountRenderAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := e.count(`SELECT count(*) FROM albums WHERE deleted_at IS NULL`)
	if int(n) != want {
		t.Fatalf("CountRenderAll %d, want %d", n, want)
	}
	e.published(gone, "Chet Baker/Chet")
	if n, _ := e.svc.CountRenderAll(ctx); int(n) != want+1 {
		t.Fatalf("CountRenderAll with a removal to materialize %d, want %d", n, want+1)
	}
	enq, err := e.svc.RenderAll(ctx)
	if err != nil || enq != want+1 {
		t.Fatalf("RenderAll %d, %v; the count said %d", enq, err, want+1)
	}
}

func TestDismissJob(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b, err := e.svc.CreateImportBatch(ctx, uuid.New(), "Rock")
	if err != nil {
		t.Fatal(err)
	}
	batch := b.ID
	id := e.importJob(batch, "Rock/A", "failed")
	v, err := e.svc.DismissJob(ctx, id)
	if err != nil || v.DismissedAt == nil || v.Attention || v.State != jobs.StateFailed {
		t.Fatalf("DismissJob = %+v, %v", v, err)
	}
	r, err := e.svc.GetImportReport(ctx, batch)
	if err != nil || len(r.Imports) != 1 || r.Imports[0].DismissedAt == nil || r.Imports[0].Attention {
		t.Fatalf("report %+v, %v", r.Imports, err)
	}
	pending := e.importJob(batch, "Rock/B", "pending")
	if _, err := e.svc.DismissJob(ctx, pending); catalog.Code(err) != catalog.CodeJobNotDismissable {
		t.Fatalf("dismissing a pending job: %v", err)
	}
	if _, err := e.svc.DismissJob(ctx, store.NewID()); catalog.Code(err) != catalog.CodeJobNotFound {
		t.Fatalf("dismissing an unknown job: %v", err)
	}
}
