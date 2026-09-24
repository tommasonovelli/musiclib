package publish

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"musiclib/internal/failpoint"
	"musiclib/internal/jobs"
)

// §9.3 B: "Sincronizzare tutte le directory coinvolte nei rename/rmdir,
// incluse quelle nuove e i relativi parent". The fsyncs of INSTALL, traced
// at the fsync_dir failpoint, for a rename to another artist and for an
// exchange: the moved album directory and work/retired/<build_id> are
// fsynced (their ".." changed), every directory before its parent.
func TestPublishFsyncOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(e *env, id [16]byte)
		want   func(j Journal) []string
		// preRename: fsynced before the rename (§9.3 B "creare/sincronizzare
		// i parent e spostare lo staging"): a new artist directory, library/.
		preRename []string
	}{
		{"rename", func(e *env, id [16]byte) { e.renameArtist(id, "New Name") }, func(j Journal) []string {
			return []string{"library:New Name/Album", "library:New Name", "work:retired/" + j.BuildID.String(),
				"work:retired", "library:Old Name", "library:", "work:"}
		}, []string{"library:New Name", "library:"}},
		{"exchange", func(e *env, id [16]byte) { e.bump(id) }, func(j Journal) []string {
			return []string{"library:Old Name/Album", "library:Old Name", "library:"}
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.importAlbum("Old Name", "Album")
			e.mustPublish("v1")
			tc.change(e, id)
			var (
				mu     sync.Mutex
				events []string
				pre    []string // the fsyncs before the point installed
				after  bool
			)
			e.fp.Set(func(p failpoint.Point) error {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case p.Name == "installed":
					after = true
				case p.Name == "fsync_dir" && !after:
					pre = append(pre, p.Path)
				case p.Name == "fsync_dir":
					events = append(events, p.Path)
				}
				return nil
			})
			rep, _ := e.mustPublish("v2")
			if !slices.Equal(pre, tc.preRename) {
				t.Fatalf("fsynced before the rename: %q, want %q", pre, tc.preRename)
			}
			for _, w := range tc.want(rep.Journal) {
				if !slices.Contains(events, w) {
					t.Errorf("%s not fsynced: %q", w, events)
				}
			}
			// Every directory before its parent on the same root.
			for i, a := range events {
				for _, b := range events[:i] {
					ra, pa, _ := strings.Cut(a, ":")
					rb, pb, _ := strings.Cut(b, ":")
					if ra == rb && (pa == "" || strings.HasPrefix(pb, pa+"/")) {
						continue // b is below a: right order
					}
					if ra == rb && (pb == "" || strings.HasPrefix(pa, pb+"/")) {
						t.Fatalf("%s fsynced after its parent %s: %q", a, b, events)
					}
				}
			}
		})
	}
}

// §9.3 B for a new artist: the artist directory is created and made
// durable before the rename. If the installation fails after that, the
// artist directory it created is removed again (it is empty); the journal
// stays and the recovery installs the album.
func TestPublishRemovesTheArtistDirectoryOfAFailedInstall(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("New Artist", "Album")
	var sawDir bool
	e.setFailpoint(func(point string) error {
		if point == "parents_synced" {
			_, err := os.Stat(e.path("library/New Artist"))
			sawDir = err == nil && !e.exists("library/New Artist/Album")
			return errors.New("injected after the parents")
		}
		return nil
	})
	_, _, err := e.render("v1")
	if !jobs.Stops(err) || Code(err) != CodeSuspended {
		t.Fatalf("err = %v, want a suspension", err)
	}
	if !sawDir {
		t.Fatal("the artist directory did not exist before the rename")
	}
	if got := e.entries("library"); len(got) != 0 {
		t.Fatalf("library holds %q after the failed install", got)
	}
	j := e.mustJournal()
	e.setFailpoint(nil)
	if got, err := e.publisher(e.db).Recover(context.Background()); err != nil || got == nil || got.BuildID != j.BuildID {
		t.Fatalf("Recover: %v %v", got, err)
	}
	e.wantPublished(id, resultOf(j))
}
