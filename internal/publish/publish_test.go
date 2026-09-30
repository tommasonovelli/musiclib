package publish

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/render"
	"musiclib/internal/store"
)

// §9.3 on a new album: the staging moves to <artist>/<album> with
// RENAME_NOREPLACE, published_* are the journal's values, the job is
// deleted (the ticket is unchanged), the journal is gone, the claims are
// the desired path only, and work/ holds nothing of the build.
func TestPublishNewAlbum(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Miles Davis", "Kind of Blue")
	rep, res := e.mustPublish("v1")
	if rep.Job != jobs.RenderDeleted || res.Dir != "Miles Davis/Kind of Blue" {
		t.Fatalf("report %+v, dir %q", rep, res.Dir)
	}
	e.wantPublished(id, res)
	if _, ok := e.renderJob(id); ok {
		t.Fatal("the render job survived a publication of its current ticket")
	}
	if _, ok := e.journal(); ok {
		t.Fatal("the journal survived FINALIZE")
	}
	e.wantClaims(id, "Miles Davis/Kind of Blue")
	e.wantWorkClean()
	if fi, err := os.Stat(e.path("library/Miles Davis")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("artist directory: %v %v", fi, err)
	}
}

// §9.3 B, same path: RENAME_EXCHANGE, the old album goes into the staging
// and is removed after the release; the output is the new build.
func TestPublishSamePathExchange(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	_, first := e.mustPublish("v1")
	e.bump(id)
	var sawOld bool
	e.setFailpoint(func(point string) error {
		if point == "installed" {
			// Right after the exchange: the staging holds the old album.
			b, err := os.ReadFile(e.path("work/" + render.StagingDir(e.mustJournal().BuildID) + "/" + render.ReceiptName))
			sawOld = err == nil && strings.Contains(string(b), first.BuildID.String())
		}
		return nil
	})
	_, second := e.mustPublish("v2")
	if !sawOld {
		t.Fatal("the exchange did not put the old album into the staging")
	}
	e.wantPublished(id, second)
	e.wantClaims(id, "Artist/Album")
	e.wantWorkClean()
}

func (e *env) mustJournal() Journal {
	e.t.Helper()
	j, ok := e.journal()
	if !ok {
		e.t.Fatal("no journal")
	}
	return j
}

// §9.3 B, another path: the new album is installed with NOREPLACE, then
// the old directory is retired and removed, and its empty artist directory
// with rmdir; the old claim is released at FINALIZE (§5.3).
func TestPublishArtistRename(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Old Name", "Album")
	e.mustPublish("v1")
	e.renameArtist(id, "New Name")
	e.wantClaims(id, "New Name/Album", "Old Name/Album") // the old name stays reserved (§5.3)
	var during []string
	e.setFailpoint(func(point string) error {
		if point == "installed" {
			during = e.entries("library")
			e.wantClaims(id, "New Name/Album", "Old Name/Album")
		}
		return nil
	})
	_, res := e.mustPublish("v2")
	if strings.Join(during, "|") != "New Name|Old Name" {
		t.Fatalf("after the install, before the retirement: %q", during)
	}
	e.wantPublished(id, res)
	if got := e.entries("library"); strings.Join(got, "|") != "New Name" {
		t.Fatalf("library holds %q: the old artist directory must be removed when empty", got)
	}
	e.wantClaims(id, "New Name/Album")
	e.wantWorkClean()
}

// A title change under the same artist: the artist directory holds the new
// album and stays.
func TestPublishRetitle(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "First Title")
	e.mustPublish("v1")
	if err := e.retitle(id, "Second Title"); err != nil {
		t.Fatal(err)
	}
	_, res := e.mustPublish("v2")
	e.wantPublished(id, res)
	if got := e.entries("library/Artist"); strings.Join(got, "|") != "Second Title" {
		t.Fatalf("library/Artist holds %q", got)
	}
}

// §9.3: old_path == new_path is compared on the exact paths, never on the
// keys: "Abba/X" and "ABBA/X" are two directories on ext4, one claim key
// (§5.3). A case-only artist rename installs the new directory with
// NOREPLACE and retires the old one.
func TestPublishCaseOnlyRenameComparesExactPaths(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Abba", "X")
	e.mustPublish("v1")
	e.renameArtist(id, "ABBA")
	e.wantClaims(id, "ABBA/X") // one key
	_, res := e.mustPublish("v2")
	e.wantPublished(id, res)
	if got := e.entries("library"); strings.Join(got, "|") != "ABBA" {
		t.Fatalf("library holds %q, want only ABBA", got)
	}
	e.wantWorkClean()
}

// Trash publishes a removal: the directory is retired, the artist directory
// removed, published_path/build/receipt NULL and the revision the one built
// (§4.2); the old claim is released. Restore publishes it again.
func TestPublishRemovalAndRestore(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	e.mustPublish("v1")
	e.trash(id)
	e.wantClaims(id, "Artist/Album") // the published path, until removed
	rep, res := e.mustPublish("")
	if !res.Removal || rep.Job != jobs.RenderDeleted {
		t.Fatalf("%+v %+v", rep, res)
	}
	e.wantPublished(id, res)
	if got := e.entries("library"); len(got) != 0 {
		t.Fatalf("library holds %q after the removal", got)
	}
	e.wantClaims(id)
	e.wantWorkClean()

	e.restore(id)
	_, res = e.mustPublish("v3")
	e.wantPublished(id, res)
	e.wantClaims(id, "Artist/Album")
}

// A removal of an album that was never published has nothing to retire,
// and still records the revision published (§4.2).
func TestPublishRemovalNeverPublished(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	e.trash(id)
	_, res := e.mustPublish("")
	e.wantPublished(id, res)
	if got := e.entries("library"); len(got) != 0 {
		t.Fatalf("library holds %q", got)
	}
}

// §3.3, §9.3: a directory at the new path that the album has not published
// is a conflict of the preflight: the job fails with its code, before any
// journal; nothing in library/ is touched, the build is discarded.
func TestPublishForeignDirectoryRefused(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	e.write("library/Artist/Album/mine.txt", "the user's")
	before := e.tree("library")
	c := e.claim()
	res := e.stage(c.Render, "v1")
	_, err := e.p.Publish(context.Background(), c.Render, res)
	wantCode(t, err, CodeDestinationOccupied)
	if jobs.Stops(err) {
		t.Fatal("a preflight conflict stops the pool")
	}
	e.wantTree("library", before)
	if _, ok := e.journal(); ok {
		t.Fatal("a journal was written")
	}
	e.wantWorkClean()
	// The executor's completion: failed with the code (§6.4).
	if err := e.p.failBeforeJournal(context.Background(), c.Attempt, err); err != nil {
		t.Fatal(err)
	}
	if j, _ := e.renderJob(id); j.State != "failed" || deref(j.ErrorCode) != CodeDestinationOccupied {
		t.Fatalf("job %s %v", j.State, deref(j.ErrorCode))
	}
}

// Another album's receipt, at the new path or at the album's own published
// path, never authorizes a replacement (§9.3).
func TestPublishAnotherAlbumsReceiptRefused(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	e.mustPublish("v1")
	// Replace the album's receipt with one of another album.
	other := render.Receipt{AlbumID: uuid.New(), BuildID: uuid.New(), AlbumRevision: 1, RenderVersion: render.Version}
	b, err := other.Encode()
	if err != nil {
		t.Fatal(err)
	}
	e.write("library/Artist/Album/"+render.ReceiptName, string(b))
	e.bump(id)
	before := e.tree("library")
	_, _, err = e.render("v2")
	wantCode(t, err, CodeForeignOutput)
	e.wantTree("library", before)

	// The same at a new path: the rename target holds it.
	e.write("library/Artist/Renamed/"+render.ReceiptName, string(b))
	if err := e.retitle(id, "Renamed"); err != nil {
		t.Fatal(err)
	}
	before = e.tree("library")
	_, _, err = e.render("v3")
	wantCode(t, err, CodeForeignOutput)
	e.wantTree("library", before)
}

// At the album's own published path, a missing receipt or an altered file
// is output damage: the build replaces the directory (§9.3).
func TestPublishReplacesDamagedOwnOutput(t *testing.T) {
	for _, damage := range []string{"receipt removed", "file altered", "file added", "receipt garbage"} {
		t.Run(damage, func(t *testing.T) {
			e := newEnv(t)
			id := e.importAlbum("Artist", "Album")
			e.mustPublish("v1")
			switch damage {
			case "receipt removed":
				if err := os.Remove(e.path("library/Artist/Album/" + render.ReceiptName)); err != nil {
					t.Fatal(err)
				}
			case "file altered":
				e.write("library/Artist/Album/01 - One.flac", "tampered")
			case "file added":
				e.write("library/Artist/Album/extra.txt", "added by hand")
			case "receipt garbage":
				e.write("library/Artist/Album/"+render.ReceiptName, "{not json")
			}
			e.bump(id)
			_, res := e.mustPublish("v2")
			e.wantPublished(id, res)
			if e.exists("library/Artist/Album/extra.txt") {
				t.Fatal("a file added by hand survived the replacement (§3.3)")
			}
		})
	}
}

// A publication owns the album folder only. Files another program left in
// the artist folder or at the top of library/ survive a trash and an artist
// rename: the artist folder is removed only when empty, and it keeps them.
// A file inside the album folder leaves with the old folder on a path change.
func TestPublishKeepsForeignFilesOutsideTheAlbumFolder(t *testing.T) {
	wantFile := func(e *env, rel, content string) {
		e.t.Helper()
		b, err := os.ReadFile(e.path(rel))
		if err != nil || string(b) != content {
			e.t.Fatalf("%s: %q %v, want it kept with %q", rel, b, err, content)
		}
	}
	wantEntries := func(e *env, rel, want string) {
		e.t.Helper()
		if got := e.entries(rel); strings.Join(got, "|") != want {
			e.t.Fatalf("%s holds %q, want %q", rel, got, want)
		}
	}
	t.Run("trash and restore", func(t *testing.T) {
		e := newEnv(t)
		id := e.importAlbum("Artist", "Album")
		e.mustPublish("v1")
		e.write("library/Artist/folder.jpg", "an artist picture")
		e.write("library/Artist/artist.nfo", "an index")
		e.write("library/.DS_Store", "a desktop file")
		e.trash(id)
		_, res := e.mustPublish("")
		e.wantPublished(id, res)
		wantEntries(e, "library", ".DS_Store|Artist")
		wantEntries(e, "library/Artist", "artist.nfo|folder.jpg")
		wantFile(e, "library/Artist/folder.jpg", "an artist picture")
		wantFile(e, "library/Artist/artist.nfo", "an index")
		wantFile(e, "library/.DS_Store", "a desktop file")
		e.wantWorkClean()

		// The restored album is installed into the artist folder that stayed.
		e.restore(id)
		_, res = e.mustPublish("v2")
		e.wantPublished(id, res)
		wantEntries(e, "library/Artist", "Album|artist.nfo|folder.jpg")
		e.wantWorkClean()
	})
	t.Run("artist rename", func(t *testing.T) {
		e := newEnv(t)
		id := e.importAlbum("Old Name", "Album")
		e.mustPublish("v1")
		e.write("library/Old Name/folder.jpg", "an artist picture")
		e.renameArtist(id, "New Name")
		_, res := e.mustPublish("v2")
		e.wantPublished(id, res)
		wantEntries(e, "library", "New Name|Old Name")
		wantEntries(e, "library/Old Name", "folder.jpg")
		wantFile(e, "library/Old Name/folder.jpg", "an artist picture")
		e.wantWorkClean()
	})
	t.Run("retitle", func(t *testing.T) {
		e := newEnv(t)
		id := e.importAlbum("Artist", "First Title")
		e.mustPublish("v1")
		e.write("library/Artist/First Title/folder.jpg", "added by another program")
		if err := e.retitle(id, "Second Title"); err != nil {
			t.Fatal(err)
		}
		_, res := e.mustPublish("v2")
		e.wantPublished(id, res)
		wantEntries(e, "library/Artist", "Second Title")
		if e.exists("library/Artist/Second Title/folder.jpg") {
			t.Fatal("a foreign file followed the album to its new folder")
		}
		e.wantWorkClean()
	})
}

// Symlinks and special files are always refused (§9.3): at the new path,
// as the artist directory, and at the old path.
func TestPublishRefusesSymlinksAndSpecialFiles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *env)
	}{
		{"symlink at the new path", func(e *env) {
			if err := os.MkdirAll(e.path("library/Artist"), 0o755); err != nil {
				e.t.Fatal(err)
			}
			if err := os.Symlink(e.path("work"), e.path("library/Artist/Album")); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"symlink as the artist directory", func(e *env) {
			if err := os.MkdirAll(e.path("elsewhere"), 0o755); err != nil {
				e.t.Fatal(err)
			}
			if err := os.Symlink(e.path("elsewhere"), e.path("library/Artist")); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"FIFO at the new path", func(e *env) {
			if err := os.MkdirAll(e.path("library/Artist"), 0o755); err != nil {
				e.t.Fatal(err)
			}
			if err := unix.Mkfifo(e.path("library/Artist/Album"), 0o644); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"file at the new path", func(e *env) { e.write("library/Artist/Album", "a file") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.importAlbum("Artist", "Album")
			tc.setup(e)
			before := e.tree("library")
			_, _, err := e.render("v1")
			wantCode(t, err, CodeUnsafeEntry)
			e.wantTree("library", before)
			if e.exists("elsewhere/Album") {
				t.Fatal("the publication followed the symlink")
			}
		})
	}

	t.Run("symlink at the old path", func(t *testing.T) {
		e := newEnv(t)
		id := e.importAlbum("Artist", "Album")
		e.mustPublish("v1")
		if err := os.Rename(e.path("library/Artist/Album"), e.path("moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(e.path("moved"), e.path("library/Artist/Album")); err != nil {
			t.Fatal(err)
		}
		if err := e.retitle(id, "Other"); err != nil {
			t.Fatal(err)
		}
		before := e.tree("library")
		_, _, err := e.render("v2")
		wantCode(t, err, CodeUnsafeEntry)
		e.wantTree("library", before)
	})
}

// §9.5 "API modifica un album durante la copia": the build is superseded,
// PREPARE requeues the job without a journal and the staging is discarded;
// the next render publishes the new revision.
func TestPublishSupersededBuild(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	c := e.claim()
	res := e.stage(c.Render, "v1")
	e.bump(id) // during the build
	rep, err := e.p.Publish(context.Background(), c.Render, res)
	if err != nil || rep.Outcome != Superseded {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, ok := e.journal(); ok {
		t.Fatal("a superseded build wrote a journal")
	}
	j, _ := e.renderJob(id)
	if j.State != "pending" || j.Claimed != nil {
		t.Fatalf("job %s claimed %v, want pending for the new request", j.State, j.Claimed)
	}
	e.wantWorkClean()
	if len(e.entries("library")) != 0 {
		t.Fatal("a superseded build was published")
	}
	_, res = e.mustPublish("v2")
	if res.AlbumRevision != e.album(id).Revision {
		t.Fatal("the second render is not the new revision")
	}
	e.wantPublished(id, res)

	// A new renderer supersedes too (§6.3).
	e.bump(id)
	c = e.claim()
	c.Render.RenderVersion = "another renderer"
	res = e.stage(c.Render, "v3")
	rep, err = e.p.Publish(context.Background(), c.Render, res)
	if err != nil || rep.Outcome != Superseded {
		t.Fatalf("renderer: %+v %v", rep, err)
	}
}

// §9.5 "API modifica dopo PREPARE": the prepared publication completes on
// its own revision; published_revision is the one built, never the current
// one (§6.3), and the job goes back to pending for the new request.
func TestPublishChangeBetweenPrepareAndFinalize(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	e.setFailpoint(func(point string) error {
		if point == "prepared" {
			e.bump(id) // the API never waits for publishMu
		}
		return nil
	})
	rep, res := e.mustPublish("v1")
	if rep.Job != jobs.RenderRequeued {
		t.Fatalf("job outcome %q, want requeued", rep.Job)
	}
	a := e.album(id)
	if a.PublishedRevision != res.AlbumRevision || a.Revision != res.AlbumRevision+1 {
		t.Fatalf("published revision %d, revision %d; built %d", a.PublishedRevision, a.Revision, res.AlbumRevision)
	}
	j, _ := e.renderJob(id)
	if j.State != "pending" {
		t.Fatalf("job %s, want pending: a second render is still needed", j.State)
	}
	e.setFailpoint(nil)
	_, res = e.mustPublish("v2")
	e.wantPublished(id, res)
	if _, ok := e.renderJob(id); ok {
		t.Fatal("the job survived its current render")
	}
}

// N-112 as endorsed: stale claims are first repaired inside PREPARE; if
// another album owns a path, the render fails with path_reserved naming
// it, no journal is written, and the job is never requeued.
func TestPublishStaleClaims(t *testing.T) {
	t.Run("repaired", func(t *testing.T) {
		e := newEnv(t)
		id := e.importAlbum("Artist", "Album")
		e.exec(`DELETE FROM path_claims WHERE album_id = $1`, id) // outside the rules
		_, res := e.mustPublish("v1")
		e.wantPublished(id, res)
		e.wantClaims(id, "Artist/Album")
	})
	t.Run("owned by another album", func(t *testing.T) {
		e := newEnv(t)
		id := e.importAlbum("Artist", "Album")
		thief := e.importAlbum("Thief", "Other")
		e.exec(`UPDATE path_claims SET album_id = $2 WHERE album_id = $1`, id, thief)
		c := e.claim()
		if c.Render.Album.ID != id {
			c = e.claim()
		}
		res := e.stage(c.Render, "v1")
		rep, err := e.p.Publish(context.Background(), c.Render, res)
		if err != nil || rep.Outcome != Refused {
			t.Fatalf("%+v %v", rep, err)
		}
		j, _ := e.renderJob(id)
		if j.State != "failed" || deref(j.ErrorCode) != catalog.CodePathReserved || !strings.Contains(deref(j.ErrorMessage), thief.String()) {
			t.Fatalf("job %s %v %q: want failed with path_reserved naming %s", j.State, deref(j.ErrorCode), deref(j.ErrorMessage), thief)
		}
		if _, ok := e.journal(); ok {
			t.Fatal("a journal was written")
		}
		if e.exists("library/Artist") {
			t.Fatal("published over a stolen claim")
		}
		e.wantWorkClean()
		// Never requeued: nothing else is pending for the album.
		if n := e.count(`SELECT count(*) FROM jobs WHERE album_id = $1 AND state = 'pending'`, id); n != 0 {
			t.Fatal("the job was requeued")
		}
	})
}

// §9.5 "Un nome viene riusato durante una rinomina": the old name stays
// reserved by its album until the old directory is retired; another album
// asking for it gets path_reserved naming the owner, and gets it once the
// retirement is published. Nothing is stolen.
func TestPublishNameReuseDuringRename(t *testing.T) {
	e := newEnv(t)
	a := e.importAlbum("Artist", "X")
	b := e.importAlbum("Artist", "B")
	e.mustPublish("a1")
	e.mustPublish("b1")
	if err := e.retitle(a, "Y"); err != nil {
		t.Fatal(err)
	}
	err := e.retitle(b, "X")
	if catalog.Code(err) != catalog.CodePathReserved {
		t.Fatalf("reusing X during the rename: %v", err)
	}
	if ce, _ := catalog.AsError(err); ce.Details.AlbumID != a {
		t.Fatalf("the owner named is %s, want %s", ce.Details.AlbumID, a)
	}
	// Also while A's publication is prepared and not finished.
	e.setFailpoint(func(point string) error {
		if point == "installed" {
			if err := e.retitle(b, "X"); catalog.Code(err) != catalog.CodePathReserved {
				t.Errorf("reusing X between INSTALL and FINALIZE: %v", err)
			}
		}
		return nil
	})
	e.mustPublish("a2")
	e.setFailpoint(nil)
	if err := e.retitle(b, "X"); err != nil {
		t.Fatalf("X after its retirement: %v", err)
	}
	_, res := e.mustPublish("b2")
	e.wantPublished(b, res)
}

// §9.5 "Album cancellato e subito ripristinato": the newest ticket wins
// before PREPARE; a removal already prepared completes, and the restore
// renders after it.
func TestPublishTrashThenRestore(t *testing.T) {
	t.Run("before PREPARE", func(t *testing.T) {
		e := newEnv(t)
		id := e.importAlbum("Artist", "Album")
		e.mustPublish("v1")
		e.trash(id)
		c := e.claim()
		res := e.stage(c.Render, "")
		e.restore(id)
		rep, err := e.p.Publish(context.Background(), c.Render, res)
		if err != nil || rep.Outcome != Superseded {
			t.Fatalf("%+v %v", rep, err)
		}
		if !e.exists("library/Artist/Album") {
			t.Fatal("the superseded removal was published")
		}
		_, res = e.mustPublish("v2")
		e.wantPublished(id, res)
	})
	t.Run("after PREPARE", func(t *testing.T) {
		e := newEnv(t)
		id := e.importAlbum("Artist", "Album")
		e.mustPublish("v1")
		e.trash(id)
		e.setFailpoint(func(point string) error {
			if point == "prepared" {
				e.restore(id)
			}
			return nil
		})
		rep, res := e.mustPublish("")
		if !res.Removal || rep.Job != jobs.RenderRequeued || e.exists("library/Artist/Album") {
			t.Fatalf("the prepared removal: %+v", rep)
		}
		e.setFailpoint(nil)
		_, res = e.mustPublish("v2")
		e.wantPublished(id, res)
		e.wantClaims(id, "Artist/Album")
	})
}

// The lock order is publishMu, then the catalog transaction; API mutations
// never take publishMu (§9.3). While one publisher holds publishMu and
// another waits for it, a catalog mutation still commits: the waiter holds
// no catalog lock.
func TestPublishLockOrder(t *testing.T) {
	e := newEnv(t)
	a := e.importAlbum("A", "One")
	b := e.importAlbum("B", "Two")
	other := e.importAlbum("C", "Three")
	ca, cb := e.claim(), e.claim()
	ra, rb := e.stage(ca.Render, "a"), e.stage(cb.Render, "b")
	holding, release := make(chan struct{}), make(chan struct{})
	e.setFailpoint(func(point string) error {
		if point == "preflight" {
			select {
			case holding <- struct{}{}:
				<-release
			default:
			}
		}
		return nil
	})
	errs := make(chan error, 2)
	go func() { _, err := e.p.Publish(context.Background(), ca.Render, ra); errs <- err }()
	<-holding
	go func() { _, err := e.p.Publish(context.Background(), cb.Render, rb); errs <- err }()
	time.Sleep(200 * time.Millisecond) // the second publisher is waiting for publishMu
	done := make(chan error, 1)
	go func() { done <- e.retitle(other, "Renamed") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("a catalog mutation waited for a publisher that waits for publishMu")
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uuid.UUID{a, b} {
		if e.album(id).PublishedRevision == 0 {
			t.Fatalf("album %s not published", id)
		}
	}
}

// An error after PREPARE is not a failed job (§9.4): the journal stays
// pending, the error stops the pool, and nothing else is published by this
// process; a new process recovers the journal first.
func TestPublishFailureAfterPrepareSuspends(t *testing.T) {
	e := newEnv(t)
	a := e.importAlbum("A", "One")
	e.importAlbum("B", "Two")
	e.failAt("installed")
	_, resA, err := e.render("a")
	if !jobs.Stops(err) || Code(err) != CodeSuspended {
		t.Fatalf("err %v (code %q), want a stop with %s", err, Code(err), CodeSuspended)
	}
	j := e.mustJournal()
	if j.BuildID != resA.BuildID {
		t.Fatal("the journal is not the failed publication's")
	}
	if job, _ := e.renderJob(a); job.State != "running" {
		t.Fatalf("job %s: a failure after PREPARE must not complete the job", job.State)
	}
	// Another build is refused without touching anything.
	c := e.claim()
	resB := e.stage(c.Render, "b")
	before := e.tree("library")
	_, err = e.p.Publish(context.Background(), c.Render, resB)
	if !jobs.Stops(err) || Code(err) != CodeSuspended {
		t.Fatalf("second publication: %v", err)
	}
	e.wantTree("library", before)
	if e.mustJournal() != j {
		t.Fatal("the journal changed")
	}
	// A new process recovers it forward.
	got, err := e.publisher(e.db).Recover(context.Background())
	if err != nil || got == nil || *got != j {
		t.Fatalf("Recover: %v %v", got, err)
	}
	e.wantPublished(a, resA)
}

// A shutdown after PREPARE: the prepared publication still completes
// within the grace (§11.1).
func TestPublishCompletesAfterCancellationWithinGrace(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	c := e.claim()
	res := e.stage(c.Render, "v1")
	ctx, cancel := context.WithCancel(context.Background())
	e.setFailpoint(func(point string) error {
		if point == "prepared" {
			cancel()
		}
		return nil
	})
	rep, err := e.p.Publish(ctx, c.Render, res)
	if err != nil || rep.Outcome != Published {
		t.Fatalf("%+v %v", rep, err)
	}
	e.wantPublished(id, res)
}

// After the grace, the publication gives up and leaves the journal to the
// recovery.
func TestPublishGivesUpAfterTheGrace(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	old := shutdownGrace
	shutdownGrace = 50 * time.Millisecond
	t.Cleanup(func() { shutdownGrace = old })
	c := e.claim()
	res := e.stage(c.Render, "v1")
	ctx, cancel := context.WithCancel(context.Background())
	e.setFailpoint(func(point string) error {
		switch point {
		case "prepared":
			cancel()
		case "installed":
			time.Sleep(300 * time.Millisecond) // a slow INSTALL
		}
		return nil
	})
	_, err := e.p.Publish(ctx, c.Render, res)
	if !jobs.Stops(err) {
		t.Fatalf("err %v, want a stop", err)
	}
	if _, ok := e.journal(); !ok {
		t.Fatal("no journal left to the recovery")
	}
	if _, err := e.publisher(e.db).Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.wantPublished(id, res)
}

// Publish refuses a build that is not the snapshot's.
func TestPublishArguments(t *testing.T) {
	e := newEnv(t)
	e.importAlbum("Artist", "Album")
	c := e.claim()
	res := e.stage(c.Render, "v1")
	bad := res
	bad.AlbumRevision++
	if _, err := e.p.Publish(context.Background(), c.Render, bad); Code(err) != CodeInvalidArgument {
		t.Fatalf("another revision: %v", err)
	}
	bad = res
	bad.Staging = "render/x/album"
	if _, err := e.p.Publish(context.Background(), c.Render, bad); Code(err) != CodeInvalidArgument {
		t.Fatalf("another staging: %v", err)
	}
	if _, err := New(Config{}); Code(err) != CodeInvalidArgument {
		t.Fatalf("New without dependencies: %v", err)
	}
	// A staging whose receipt is not the build's: refused before PREPARE.
	e.write("work/"+res.Staging+"/"+render.ReceiptName, "{}")
	_, err := e.p.Publish(context.Background(), c.Render, res)
	wantCode(t, err, CodeStagingInvalid)
	if errors.Is(err, context.Canceled) || store.IsFatal(err) {
		t.Fatal(err)
	}
}

// N-150 (§10.1): a render that fails before the journal because of the
// database stores its code, and a message that carries no database text;
// a content failure keeps its own message.
func TestFailBeforeJournalMessages(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	_, pgErr := e.db.Exec(context.Background(), `SELECT * FROM no_such_table`)
	dbFailure := &catalog.Error{Code: catalog.CodeDB, Message: "reading the album", Err: pgErr}
	c := e.claim()
	if err := e.p.failBeforeJournal(context.Background(), c.Attempt, dbFailure); err != nil {
		t.Fatal(err)
	}
	j, _ := e.renderJob(id)
	if j.State != "failed" || deref(j.ErrorCode) != catalog.CodeDB || deref(j.ErrorMessage) != catalog.DatabaseJobMessage {
		t.Fatalf("job %s %q %q", j.State, deref(j.ErrorCode), deref(j.ErrorMessage))
	}
	e.bump(id)
	c = e.claim()
	content := &Error{Code: CodeDestinationOccupied, Message: "Artist/Album is occupied"}
	if err := e.p.failBeforeJournal(context.Background(), c.Attempt, content); err != nil {
		t.Fatal(err)
	}
	if j, _ = e.renderJob(id); deref(j.ErrorMessage) != content.Error() {
		t.Fatalf("a content failure's message %q", deref(j.ErrorMessage))
	}
}
