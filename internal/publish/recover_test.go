package publish

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/jobs"
	"musiclib/internal/render"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// scenario sets up an album whose next render is one kind of publication
// and returns it with the paths the library must hold afterwards.
type scenario struct {
	name   string
	points []string // the failpoints this publication passes
	setup  func(e *env) uuid.UUID
	// after lists the entries of library/ after the publication.
	after []string
}

var scenarios = []scenario{
	{
		name: "new", points: []string{"prepared", "installed", "synced", "finalized"},
		setup: func(e *env) uuid.UUID { return e.importAlbum("Artist", "Album") },
		after: []string{"Artist"},
	},
	{
		name: "exchange", points: []string{"prepared", "installed", "synced", "finalized"},
		setup: func(e *env) uuid.UUID {
			id := e.importAlbum("Artist", "Album")
			e.mustPublish("old")
			e.bump(id)
			return id
		},
		after: []string{"Artist"},
	},
	{
		name: "rename", points: []string{"prepared", "installed", "retired", "synced", "finalized"},
		setup: func(e *env) uuid.UUID {
			id := e.importAlbum("Old", "Album")
			e.mustPublish("old")
			e.renameArtist(id, "New")
			return id
		},
		after: []string{"New"},
	},
	{
		name: "removal", points: []string{"prepared", "retired", "synced", "finalized"},
		setup: func(e *env) uuid.UUID {
			id := e.importAlbum("Artist", "Album")
			e.mustPublish("old")
			e.trash(id)
			return id
		},
	},
}

// §9.4 and §12.2 "Crash prima/dopo PREPARE", "subito dopo exchange, prima
// degli fsync, prima/dopo FINALIZE", "tra installazione del nuovo path e
// ritiro del vecchio", in process: the publication fails at every named
// point after PREPARE; a new publisher (a new process, with no memory)
// completes it forward, never with a reverse exchange; a second recovery
// finds nothing to do; the boot's cleanup leaves work/ empty.
func TestRecoverAfterEveryPoint(t *testing.T) {
	for _, sc := range scenarios {
		for _, at := range sc.points {
			t.Run(sc.name+"/"+at, func(t *testing.T) {
				e := newEnv(t)
				id := sc.setup(e)
				e.failAt(at)
				_, res, err := e.render("new")
				if !jobs.Stops(err) {
					t.Fatalf("err %v, want a stop", err)
				}
				e.setFailpoint(nil)
				j, pending := e.journal()
				if pending != (at != "finalized") {
					t.Fatalf("journal pending = %v after a failure at %s", pending, at)
				}
				p2 := e.publisher(e.db)
				got, err := p2.Recover(context.Background())
				if err != nil {
					t.Fatalf("Recover: %v", err)
				}
				if pending && (got == nil || *got != j) {
					t.Fatalf("Recover completed %v, want %v", got, j)
				}
				e.wantPublished(id, res)
				if got := e.entries("library"); !slices.Equal(got, sc.after) {
					t.Fatalf("library holds %q, want %q", got, sc.after)
				}
				if _, ok := e.renderJob(id); ok {
					t.Fatal("the job survived FINALIZE")
				}
				// Idempotent.
				if again, err := e.publisher(e.db).Recover(context.Background()); err != nil || again != nil {
					t.Fatalf("a second Recover: %v %v", again, err)
				}
				if _, err := p2.CleanWork(context.Background()); err != nil {
					t.Fatal(err)
				}
				e.wantWorkClean()
				e.wantPublished(id, res)
				if res.Removal {
					e.wantClaims(id)
				} else {
					e.wantClaims(id, res.Dir)
				}
			})
		}
	}
}

// §12.2 "Crash prima di PREPARE": nothing was prepared; the old output is
// valid, there is no journal, and the boot's cleanup and RecoverRunning
// make the render run again.
func TestRecoverBeforePrepare(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	_, first := e.mustPublish("old")
	e.bump(id)
	c := e.claim()
	res := e.stage(c.Render, "new") // a crash here: the staging is left
	_ = res
	before := e.tree("library")
	if got, err := e.publisher(e.db).Recover(context.Background()); err != nil || got != nil {
		t.Fatalf("Recover: %v %v", got, err)
	}
	removed, err := e.p.CleanWork(context.Background())
	if err != nil || len(removed) != 1 {
		t.Fatalf("CleanWork removed %q: %v", removed, err)
	}
	e.wantTree("library", before)
	e.wantInstalled(first)
	if n, err := jobs.RecoverRunning(context.Background(), e.db); err != nil || n != 1 {
		t.Fatalf("RecoverRunning = %d, %v", n, err)
	}
	_, res = e.mustPublish("new")
	e.wantPublished(id, res)
}

// §9.4: a state that matches no legal transition suspends publishing and
// exposes the error; nothing is deleted or moved, and the journal stays.
func TestRecoverIllegalStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sc     int    // index in scenarios
		failAt string // where the publication stopped
		tamper func(e *env, j Journal)
	}{
		{"staging missing", 0, "prepared", func(e *env, j Journal) {
			if err := os.RemoveAll(e.path("work/" + j.staging())); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"foreign directory at the new path", 0, "prepared", func(e *env, j Journal) {
			e.write("library/"+j.NewPath+"/mine.txt", "the user's")
		}},
		{"another album at the published path", 1, "prepared", func(e *env, j Journal) {
			r := render.Receipt{AlbumID: uuid.New(), BuildID: uuid.New(), AlbumRevision: 1, RenderVersion: render.Version}
			b, err := r.Encode()
			if err != nil {
				e.t.Fatal(err)
			}
			e.write("library/"+j.NewPath+"/"+render.ReceiptName, string(b))
		}},
		{"exchanged output damaged: never a reverse exchange", 1, "installed", func(e *env, j Journal) {
			// The new output lost its receipt after the exchange: the
			// staging now holds the old album, not the expected build.
			if err := os.Remove(e.path("library/" + j.NewPath + "/" + render.ReceiptName)); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"retired directory already present", 2, "installed", func(e *env, j Journal) {
			e.write("work/"+j.retired()+"/other.txt", "something")
		}},
		{"symlink at the old path", 2, "prepared", func(e *env, j Journal) {
			if err := os.Rename(e.path("library/"+j.OldPath), e.path("moved")); err != nil {
				e.t.Fatal(err)
			}
			if err := os.Symlink(e.path("moved"), e.path("library/"+j.OldPath)); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"removal: another album at the old path", 3, "prepared", func(e *env, j Journal) {
			r := render.Receipt{AlbumID: uuid.New(), BuildID: uuid.New(), AlbumRevision: 1, RenderVersion: render.Version}
			b, err := r.Encode()
			if err != nil {
				e.t.Fatal(err)
			}
			e.write("library/"+j.OldPath+"/"+render.ReceiptName, string(b))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			scenarios[tc.sc].setup(e)
			e.failAt(tc.failAt)
			if _, _, err := e.render("new"); !jobs.Stops(err) {
				t.Fatalf("err %v, want a stop", err)
			}
			e.setFailpoint(nil)
			j := e.mustJournal()
			tc.tamper(e, j)
			lib, work := e.tree("library"), e.tree("work")
			for range 2 {
				got, err := e.publisher(e.db).Recover(context.Background())
				wantCode(t, err, CodeIllegalState)
				if got == nil || *got != j {
					t.Fatalf("the refusal names %v, want the pending journal %v", got, j)
				}
				e.wantTree("library", lib)
				e.wantTree("work", work)
				if e.mustJournal() != j {
					t.Fatal("the journal changed")
				}
			}
		})
	}
}

// §12.2 "Errore DB dopo rename": the database is lost at FINALIZE, after
// the rename. The publisher stops with a fatal error; the journal is
// completed at the restart. A FINALIZE that committed but whose answer was
// lost leaves nothing to recover.
func TestRecoverDatabaseErrorAfterRename(t *testing.T) {
	for _, mode := range []string{"commit cut", "ack lost"} {
		t.Run(mode, func(t *testing.T) {
			dbURL := pgtest.EmptyDB(t)
			direct := pgtest.Pool(t, dbURL)
			if err := store.Migrate(t.Context(), direct); err != nil {
				t.Fatal(err)
			}
			e := newEnvOn(t, dbURL, direct, t.TempDir()+"/data")
			proxy := pgtest.NewProxy(t, dbURL)
			viaProxy := e.publisher(pgtest.Pool(t, proxy.URL))
			id := e.importAlbum("Artist", "Album")
			e.setFailpoint(func(point string) error {
				if point == "synced" {
					if mode == "commit cut" {
						proxy.CutBeforeNextCommit()
					} else {
						proxy.LoseNextCommitAck()
					}
				}
				return nil
			})
			c := e.claim()
			res := e.stage(c.Render, "v1")
			_, err := viaProxy.Publish(context.Background(), c.Render, res)
			if !store.IsFatal(err) || !jobs.Stops(err) {
				t.Fatalf("err %v, want a fatal store error", err)
			}
			e.setFailpoint(nil)
			_, pending := e.journal()
			if pending != (mode == "commit cut") {
				t.Fatalf("journal pending = %v", pending)
			}
			if _, err := e.publisher(direct).Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			e.wantPublished(id, res)
			if _, err := e.p.CleanWork(context.Background()); err != nil {
				t.Fatal(err)
			}
			e.wantWorkClean()
		})
	}
}

// CleanWork keeps the build a pending journal references, and removes
// every other build and retired directory.
func TestCleanWorkKeepsTheJournalsBuild(t *testing.T) {
	e := newEnv(t)
	e.importAlbum("Artist", "Album")
	e.failAt("prepared")
	if _, _, err := e.render("v1"); !jobs.Stops(err) {
		t.Fatal(err)
	}
	j := e.mustJournal()
	e.write("work/render/leftover/album/x", "x")
	e.write("work/retired/"+uuid.NewString()+"/y", "y")
	e.write("work/retired/"+j.BuildID.String()+"/z", "z")
	removed, err := e.p.CleanWork(context.Background())
	if err != nil || len(removed) != 2 {
		t.Fatalf("removed %q: %v", removed, err)
	}
	if got := e.entries("work/render"); !slices.Equal(got, []string{j.BuildID.String()}) {
		t.Fatalf("work/render holds %q", got)
	}
	if got := e.entries("work/retired"); !slices.Equal(got, []string{j.BuildID.String()}) {
		t.Fatalf("work/retired holds %q", got)
	}
	if !strings.HasPrefix(removed[0], "render/") {
		t.Fatalf("removed %q", removed)
	}
}
