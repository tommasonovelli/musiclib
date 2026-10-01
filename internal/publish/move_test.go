package publish

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// Moving every track out of a published album at each step of the
// publication of its next render: the publication completes, or is
// superseded, as for any other change of the album; the next renders
// publish the source's removal and the destination with the moved track,
// both at their current revision; then the emptied album leaves the trash.
func TestMoveTracksDuringPublication(t *testing.T) {
	for _, at := range []string{"preflight", "prepared", "installed", "synced", "finalized"} {
		t.Run(at, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			dst := e.importAlbum("B", "Two")
			e.mustPublish("b1")
			src := e.importAlbum("A", "One")
			e.mustPublish("a1")
			e.bump(src) // a render that replaces a published folder
			var (
				moved   bool
				moveErr error
			)
			e.setFailpoint(func(point string) error {
				if point == at && !moved {
					moved = true
					var id uuid.UUID
					if err := e.db.QueryRow(ctx, `SELECT id FROM tracks WHERE album_id = $1`, src).Scan(&id); err != nil {
						return err
					}
					_, moveErr = e.cat.MoveTracks(ctx, src, e.album(src).Revision, []uuid.UUID{id}, dst)
				}
				return nil
			})
			rep, _, err := e.render("a2")
			e.setFailpoint(nil)
			if err != nil || !moved || moveErr != nil || (rep.Outcome != Published && rep.Outcome != Superseded) {
				t.Fatalf("publication %+v, %v; moved %v, %v", rep, err, moved, moveErr)
			}
			for i := 0; e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`) > 0; i++ {
				if i == 4 {
					t.Fatal("the queue does not drain")
				}
				if rep, _, err := e.render(fmt.Sprintf("v%d", i)); err != nil || rep.Outcome != Published {
					t.Fatalf("render %d: %+v %v", i, rep, err)
				}
			}
			a, b := e.album(src), e.album(dst)
			if a.DeletedAt == nil || a.PublishedPath != nil || a.PublishedRevision != a.Revision {
				t.Fatalf("source: trashed %v, published %v at %d of %d", a.DeletedAt != nil, a.PublishedPath, a.PublishedRevision, a.Revision)
			}
			if b.PublishedRevision != b.Revision || e.count(`SELECT count(*) FROM tracks WHERE album_id = $1`, dst) != 2 {
				t.Fatalf("destination published at %d of %d", b.PublishedRevision, b.Revision)
			}
			if e.exists("library/A/One") || e.exists("library/A") || !e.exists("library/B/Two") {
				t.Fatalf("library holds %q", e.entries("library"))
			}
			if _, ok := e.journal(); ok {
				t.Fatal("a journal is left")
			}
			e.wantClaims(src)
			e.wantClaims(dst, "B/Two")
			e.wantWorkClean()
			if d, w, err := e.cat.EmptyTrash(ctx); err != nil || d != 1 || w != 0 {
				t.Fatalf("EmptyTrash: %d deleted, %d waiting, %v", d, w, err)
			}
		})
	}
}
