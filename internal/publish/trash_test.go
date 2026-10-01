package publish

import (
	"context"
	"sync"
	"testing"

	"musiclib/internal/jobs"
)

// Emptying the trash during the publication of an album's removal: at
// every step before FINALIZE the album is waiting (its render row, then
// also its journal, still exist) and nothing of it is deleted; once
// FINALIZE has committed, the next call deletes it, and the publication
// completes as usual. The originals' rows stay.
func TestEmptyTrashDuringRemoval(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Artist", "Album")
	e.mustPublish("v1")
	e.trash(id)
	blobs := e.count(`SELECT count(*) FROM blobs`)
	if d, w, err := e.cat.EmptyTrash(ctx); err != nil || d != 0 || w != 1 {
		t.Fatalf("removal pending: %d deleted, %d waiting, %v", d, w, err)
	}

	type call struct {
		point            string
		deleted, waiting int
		err              error
	}
	var calls []call
	e.setFailpoint(func(point string) error {
		d, w, err := e.cat.EmptyTrash(ctx)
		calls = append(calls, call{point, d, w, err})
		return nil
	})
	rep, res := e.mustPublish("")
	e.setFailpoint(nil)
	if !res.Removal || rep.Job != jobs.RenderDeleted {
		t.Fatalf("%+v %+v", rep, res)
	}
	steps := map[string]bool{}
	for i, c := range calls {
		want := call{c.point, 0, 1, nil}
		if i == len(calls)-1 {
			want = call{"finalized", 1, 0, nil}
		}
		if c != want {
			t.Fatalf("EmptyTrash at step %d: %+v, want %+v (all: %+v)", i, c, want, calls)
		}
		steps[c.point] = true
	}
	for _, p := range []string{"preflight", "prepared", "retired", "synced", "finalized"} {
		if !steps[p] {
			t.Fatalf("the trash was not emptied at %s: %+v", p, calls)
		}
	}
	if e.count(`SELECT count(*) FROM albums`) != 0 || e.count(`SELECT count(*) FROM artists`) != 0 ||
		e.count(`SELECT count(*) FROM path_claims`) != 0 || e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`) != 0 {
		t.Fatal("the purged album left rows behind")
	}
	if got := e.count(`SELECT count(*) FROM blobs`); got != blobs {
		t.Fatalf("blobs %d, want %d", got, blobs)
	}
	if got := e.entries("library"); len(got) != 0 {
		t.Fatalf("library holds %q", got)
	}
	e.wantWorkClean()
}

// The same, with the trash emptied over and over while the removal is
// published: no call fails, the album is never deleted before FINALIZE,
// and it is deleted exactly once.
func TestEmptyTrashRacesRemoval(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Artist", "Album")
	e.mustPublish("v1")
	e.trash(id)

	var early []string
	e.setFailpoint(func(point string) error {
		if point != "finalized" && e.count(`SELECT count(*) FROM albums WHERE id = $1`, id) != 1 {
			early = append(early, point)
		}
		return nil
	})
	stop := make(chan struct{})
	var (
		wg      sync.WaitGroup
		deleted int
		errs    []error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			d, _, err := e.cat.EmptyTrash(ctx)
			deleted += d
			if err != nil {
				errs = append(errs, err)
			}
		}
	}()
	rep, res := e.mustPublish("")
	close(stop)
	wg.Wait()
	e.setFailpoint(nil)
	if !res.Removal || rep.Job != jobs.RenderDeleted || len(errs) != 0 || len(early) != 0 {
		t.Fatalf("%+v %+v; errors %v; deleted before FINALIZE at %q", rep, res, errs, early)
	}
	d, w, err := e.cat.EmptyTrash(ctx)
	if err != nil || w != 0 || deleted+d != 1 {
		t.Fatalf("deleted %d during the publication and %d after it (waiting %d, %v): want 1 in all", deleted, d, w, err)
	}
	if e.count(`SELECT count(*) FROM albums`) != 0 {
		t.Fatal("the album is still there")
	}
	e.wantWorkClean()
}
