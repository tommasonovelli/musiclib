package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// §7.6 steps 1-5: blobs, artist, album at revision 1, tracks, attachments,
// the path reserved, the render enqueued, the job done, in one commit.
func TestCommitImport(t *testing.T) {
	e := newEnv(t)
	a := e.runningImport()
	c := candidate(a, "  Miles Davis ", "Kind of Blue")
	out := e.commit(c)
	if out.State != jobs.StateDone || out.AlbumID == uuid.Nil || out.AlreadyCompleted || out.ErrorCode != "" {
		t.Fatalf("outcome %+v", out)
	}

	al := e.album(out.AlbumID)
	if al.Title != "Kind of Blue" || al.FolderKey != "kind of blue" || *al.Year != 1959 || *al.Genre != "Jazz" ||
		al.Compilation || *al.CoverHash != *c.CoverHash || al.Revision != 1 || al.DeletedAt != nil ||
		*al.ImportFingerprint != c.Fingerprint || al.PublishedPath != nil || al.PublishedRevision != 0 {
		t.Errorf("album row %+v", al)
	}
	ar := e.artist(al.ArtistID)
	if ar.Name != "Miles Davis" || ar.FolderKey != "miles davis" || ar.Revision != 1 {
		t.Errorf("artist row %+v", ar)
	}

	ts := e.tracks(out.AlbumID)
	if len(ts) != 2 || ts[0].Title != "So What" || ts[0].Artist != nil || ts[0].Genre != nil ||
		ts[1].Title != "Freddie Freeloader" || *ts[1].Artist != "Miles & Cannonball" || *ts[1].Genre != "" {
		t.Errorf("tracks %+v", ts)
	}
	var lyrics, source string
	if err := e.db.QueryRow(context.Background(), `SELECT lyrics_hash, source_path FROM tracks WHERE album_id = $1 AND no = 1`,
		out.AlbumID).Scan(&lyrics, &source); err != nil {
		t.Fatal(err)
	}
	if lyrics != c.Tracks[0].Lyrics.BlobHash || source != "CD1/01 So What.flac" {
		t.Errorf("track 1 lyrics %s source %q", lyrics, source)
	}
	booklet, err := names.SanitizeRelFilePath("Scans/Booklet.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM attachments WHERE album_id = $1 AND
		((rel_path = 'Scans/Booklet.pdf' AND path_key = $2) OR (rel_path = 'cover.jpg' AND path_key = 'cover.jpg'))`,
		out.AlbumID, booklet.Key); n != 2 {
		t.Errorf("%d attachments as expected, want 2", n)
	}
	for _, b := range c.Blobs {
		var size int64
		var format *string
		if err := e.db.QueryRow(context.Background(), `SELECT size, format FROM blobs WHERE hash = $1`, b.Hash).Scan(&size, &format); err != nil {
			t.Fatalf("blob %s: %v", b.Hash, err)
		}
		if size != b.Size || (format == nil) != (b.Format == "") || (format != nil && *format != b.Format) {
			t.Errorf("blob %s: size %d format %v, want %d %q", b.Hash, size, format, b.Size, b.Format)
		}
	}

	if got := e.claims(out.AlbumID); !reflect.DeepEqual(got, map[string]string{"miles davis/kind of blue": "Miles Davis/Kind of Blue"}) {
		t.Errorf("claims %v", got)
	}
	if r, ok := e.renderJob(out.AlbumID); !ok || r.State != "pending" {
		t.Errorf("render job %+v (exists %v), want pending", r, ok)
	}
	j := e.job(a.JobID)
	var ws []jobs.Warning
	if err := json.Unmarshal(j.Warnings, &ws); err != nil {
		t.Fatal(err)
	}
	if j.State != "done" || j.Claimed != nil || *j.ResultAlbumID != out.AlbumID || j.ErrorCode != nil ||
		!reflect.DeepEqual(ws, c.Warnings) {
		t.Errorf("import job %+v", j)
	}
	if n := e.wakes.Load(); n != 1 {
		t.Errorf("%d wake-ups, want 1", n)
	}
}

// An album genre that is empty is no genre (NOTES N-100).
func TestCommitImportEmptyAlbumGenre(t *testing.T) {
	e := newEnv(t)
	c := candidate(e.runningImport(), "A", "B")
	c.Genre, c.Year = ptr(""), nil
	out := e.commit(c)
	if al := e.album(out.AlbumID); al.Genre != nil || al.Year != nil {
		t.Errorf("genre %v year %v, want NULL", al.Genre, al.Year)
	}
}

// The same commit twice, in sequence and concurrently: one album, one
// durable outcome (§7.6, §12.2).
func TestCommitImportIdempotent(t *testing.T) {
	e := newEnv(t)
	c := candidate(e.runningImport(), "Artist", "Album")
	first := e.commit(c)
	again := e.commit(c)
	if !again.AlreadyCompleted || again.State != jobs.StateDone || again.AlbumID != first.AlbumID {
		t.Errorf("second commit %+v, want the first outcome, already completed", again)
	}

	c2 := candidate(e.runningImport(), "Artist", "Other")
	const n = 8
	outs := make([]catalog.ImportOutcome, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			var err error
			outs[i], err = e.svc.CommitImport(context.Background(), c2)
			if err != nil {
				t.Errorf("concurrent commit: %v", err)
			}
		})
	}
	wg.Wait()
	fresh := 0
	for _, o := range outs {
		if o.State != jobs.StateDone || o.AlbumID != outs[0].AlbumID {
			t.Errorf("outcome %+v differs from %+v", o, outs[0])
		}
		if !o.AlreadyCompleted {
			fresh++
		}
	}
	if fresh != 1 {
		t.Errorf("%d commits wrote the album, want 1", fresh)
	}
	if n := e.count(`SELECT count(*) FROM albums`); n != 2 {
		t.Errorf("%d albums, want 2", n)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`); n != 2 {
		t.Errorf("%d render jobs, want 2", n)
	}
}

// Two imports of the same candidate in two jobs, concurrently: one album,
// the other job skipped with a reference to it (§7.6, §12.2).
func TestCommitImportConcurrentSameFingerprint(t *testing.T) {
	e := newEnv(t)
	c1 := candidate(e.runningImport(), "Artist", "Album")
	c2 := c1
	c2.Attempt = e.runningImport()
	outs := make([]catalog.ImportOutcome, 2)
	var wg sync.WaitGroup
	for i, c := range []catalog.ImportCandidate{c1, c2} {
		wg.Go(func() {
			var err error
			if outs[i], err = e.svc.CommitImport(context.Background(), c); err != nil {
				t.Errorf("commit: %v", err)
			}
		})
	}
	wg.Wait()
	done, skipped := outs[0], outs[1]
	if done.State != jobs.StateDone {
		done, skipped = skipped, done
	}
	if done.State != jobs.StateDone || skipped.State != jobs.StateSkipped || skipped.AlbumID != done.AlbumID ||
		skipped.ErrorCode != catalog.CodeDuplicateImport {
		t.Fatalf("outcomes %+v and %+v, want one done and one skipped on it", done, skipped)
	}
	if n := e.count(`SELECT count(*) FROM albums`); n != 1 {
		t.Errorf("%d albums, want 1", n)
	}
}

// The mutation test of the catalog lock: concurrent imports of different
// candidates with the same new artist and title. Exactly one album is
// created; every other job fails with album_folder_conflict, never with a
// database error.
func TestCommitImportConcurrentSameFolder(t *testing.T) {
	e := newEnv(t)
	const n = 8
	cs := make([]catalog.ImportCandidate, n)
	for i := range cs {
		cs[i] = candidate(e.runningImport(), "Brand New Artist", "Same Title")
	}
	outs := make([]catalog.ImportOutcome, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			var err error
			if outs[i], err = e.svc.CommitImport(context.Background(), cs[i]); err != nil {
				t.Errorf("commit %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	var done, conflict int
	for _, o := range outs {
		switch {
		case o.State == jobs.StateDone:
			done++
		case o.State == jobs.StateFailed && o.ErrorCode == catalog.CodeAlbumFolderConflict:
			conflict++
		default:
			t.Errorf("outcome %+v", o)
		}
	}
	if done != 1 || conflict != n-1 {
		t.Errorf("%d done, %d folder conflicts; want 1 and %d", done, conflict, n-1)
	}
	if a, b := e.count(`SELECT count(*) FROM albums`), e.count(`SELECT count(*) FROM artists`); a != 1 || b != 1 {
		t.Errorf("%d albums and %d artists, want 1 and 1", a, b)
	}
}

// A lost commit acknowledgement (§6.4, §7.6: "una conferma del commit persa
// non causa un secondo album"): the commit is fatal and uncertain; the
// retry finds the job completed and writes nothing. A cut before the
// COMMIT leaves nothing; after the boot's recovery a new attempt imports.
func TestCommitImportLostAck(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	direct := pgtest.Pool(t, dbURL)
	if err := store.Migrate(t.Context(), direct); err != nil {
		t.Fatal(err)
	}
	e := newEnvOn(t, direct)
	proxy := pgtest.NewProxy(t, dbURL)
	viaProxy := e.service(pgtest.Pool(t, proxy.URL))

	t.Run("ack lost", func(t *testing.T) {
		c := candidate(e.runningImport(), "Lost", "Ack")
		proxy.LoseNextCommitAck()
		_, err := viaProxy.CommitImport(context.Background(), c)
		if !store.IsFatal(err) || catalog.Code(err) != store.CodeCommitUncertain {
			t.Fatalf("err = %v (code %q), want %s", err, catalog.Code(err), store.CodeCommitUncertain)
		}
		retry, err := e.svc.CommitImport(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if !retry.AlreadyCompleted || retry.State != jobs.StateDone {
			t.Fatalf("retry %+v, want the durable outcome", retry)
		}
		if n := e.count(`SELECT count(*) FROM albums WHERE import_fingerprint = $1`, c.Fingerprint); n != 1 {
			t.Errorf("%d albums, want 1", n)
		}
		if n := e.count(`SELECT count(*) FROM jobs WHERE kind = 'render' AND album_id = $1`, retry.AlbumID); n != 1 {
			t.Errorf("%d render jobs, want 1", n)
		}
	})

	t.Run("cut before the commit", func(t *testing.T) {
		old := e.runningImport()
		c := candidate(old, "Cut", "Before")
		proxy.CutBeforeNextCommit()
		_, err := viaProxy.CommitImport(context.Background(), c)
		if !store.IsFatal(err) || catalog.Code(err) != store.CodeCommitUncertain {
			t.Fatalf("err = %v, want %s", err, store.CodeCommitUncertain)
		}
		// The restarted process recovers the running jobs (§11.1 step 5)
		// and claims the import again, with a new ticket.
		waitIdle(t, e)
		if _, err := jobs.RecoverRunning(context.Background(), e.db); err != nil {
			t.Fatal(err)
		}
		if n := e.count(`SELECT count(*) FROM albums WHERE import_fingerprint = $1`, c.Fingerprint); n != 0 {
			t.Fatalf("%d albums after a commit that never arrived", n)
		}
		_, err = e.svc.CommitImport(context.Background(), c)
		if jobs.Code(err) != jobs.CodeAttemptStale {
			t.Errorf("commit of the recovered attempt: %v, want %s", err, jobs.CodeAttemptStale)
		}
		c.Attempt = e.claimJob(old.JobID)
		if out := e.commit(c); out.State != jobs.StateDone || out.AlreadyCompleted {
			t.Errorf("new attempt %+v", out)
		}
		if n := e.count(`SELECT count(*) FROM albums WHERE import_fingerprint = $1`, c.Fingerprint); n != 1 {
			t.Errorf("%d albums, want 1", n)
		}
	})
}

// waitIdle waits until no other session of the database is inside a
// transaction: the server has noticed the proxy's cut.
func waitIdle(t *testing.T, e *env) {
	t.Helper()
	for range 1000 {
		time.Sleep(5 * time.Millisecond)
		if e.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
			AND pid <> pg_backend_pid() AND state LIKE 'idle in transaction%'`) == 0 {
			return
		}
	}
	t.Fatal("a proxied session stayed in its transaction")
}

// §7.6: an existing fingerprint makes the job skipped with a reference to
// the album, in the trash or not; the same bytes under another fingerprint
// are deduplicated silently.
func TestCommitImportDuplicate(t *testing.T) {
	e := newEnv(t)
	c := candidate(e.runningImport(), "Artist", "Album")
	first := e.commit(c)
	blobs := e.count(`SELECT count(*) FROM blobs`)

	dup := c
	dup.Attempt = e.runningImport()
	dup.Title = "Another title: fingerprints, not metadata, identify an import"
	out := e.commit(dup)
	if out.State != jobs.StateSkipped || out.AlbumID != first.AlbumID || out.ErrorCode != catalog.CodeDuplicateImport ||
		strings.Contains(out.ErrorMessage, "trash") {
		t.Errorf("duplicate: %+v", out)
	}
	j := e.job(dup.Attempt.JobID)
	if j.State != "skipped" || *j.ResultAlbumID != first.AlbumID || *j.ErrorCode != catalog.CodeDuplicateImport {
		t.Errorf("duplicate job row %+v", j)
	}

	if _, _, err := e.svc.TrashAlbum(context.Background(), first.AlbumID, 1); err != nil {
		t.Fatal(err)
	}
	dup.Attempt = e.runningImport()
	out = e.commit(dup)
	if out.State != jobs.StateSkipped || out.AlbumID != first.AlbumID || !strings.Contains(out.ErrorMessage, "restore") {
		t.Errorf("duplicate of a trashed album: %+v, want skipped with a restore hint", out)
	}

	same := c
	same.Attempt = e.runningImport()
	same.Fingerprint = newHash()
	same.Title = "Same bytes, other candidate"
	if out := e.commit(same); out.State != jobs.StateDone {
		t.Fatalf("same bytes: %+v", out)
	}
	if n := e.count(`SELECT count(*) FROM blobs`); n != blobs {
		t.Errorf("%d blobs after importing the same bytes, want %d", n, blobs)
	}
	if n := e.count(`SELECT count(*) FROM albums`); n != 2 {
		t.Errorf("%d albums, want 2", n)
	}
}

// §7.6: a folder of another album of the artist fails the import, casefold
// included; nothing is written.
func TestCommitImportFolderConflict(t *testing.T) {
	e := newEnv(t)
	first := e.importAlbum("Artist", "Title")
	before := snapshotCounts(e)
	c := candidate(e.runningImport(), "ARTIST", "TITLE")
	out := e.commit(c)
	if out.State != jobs.StateFailed || out.ErrorCode != catalog.CodeAlbumFolderConflict ||
		!strings.Contains(out.ErrorMessage, first.String()) {
		t.Fatalf("outcome %+v, want failed %s naming %s", out, catalog.CodeAlbumFolderConflict, first)
	}
	if after := snapshotCounts(e); after != before {
		t.Errorf("rows %v -> %v: a failed import wrote something", before, after)
	}
	if j := e.job(c.Attempt.JobID); j.State != "failed" || *j.ErrorCode != catalog.CodeAlbumFolderConflict || j.ResultAlbumID != nil {
		t.Errorf("job %+v", j)
	}
	// A retry of the job with another title (§7.6: "richiesta di titolo
	// diverso") is a new attempt that succeeds.
	e.exec(`UPDATE jobs SET state = 'pending', error_code = NULL, error_message = NULL WHERE id = $1`, c.Attempt.JobID)
	c.Attempt = e.claimJob(c.Attempt.JobID)
	c.Title = "Title (Mono)"
	if out := e.commit(c); out.State != jobs.StateDone {
		t.Errorf("retry: %+v", out)
	}
}

// counts of the rows an import writes, to prove a failure wrote nothing.
type counts struct{ blobs, artists, albums, tracks, attachments, claims, renders int }

func snapshotCounts(e *env) counts {
	return counts{
		e.count(`SELECT count(*) FROM blobs`), e.count(`SELECT count(*) FROM artists`), e.count(`SELECT count(*) FROM albums`),
		e.count(`SELECT count(*) FROM tracks`), e.count(`SELECT count(*) FROM attachments`),
		e.count(`SELECT count(*) FROM path_claims`), e.count(`SELECT count(*) FROM jobs WHERE kind = 'render'`),
	}
}

// §7.6: an existing artist is reused only if the name is the same after
// NFC, trim and casefold, keeping its spelling; names that collide only
// through the path sanitization fail with both names.
func TestCommitImportArtistRules(t *testing.T) {
	e := newEnv(t)
	abba := e.album(e.importAlbum("ABBA", "Arrival")).ArtistID
	if got := e.album(e.importAlbum("abba", "Waterloo")).ArtistID; got != abba {
		t.Errorf("'abba' made a new artist")
	}
	if e.artist(abba).Name != "ABBA" {
		t.Errorf("the artist was renamed to %q", e.artist(abba).Name)
	}
	nfd := e.album(e.importAlbum("Beyoncé", "Lemonade")).ArtistID
	if got := e.album(e.importAlbum("BEYONCÉ", "Renaissance")).ArtistID; got != nfd {
		t.Error("the NFC, casefolded name made a new artist")
	}
	if e.artist(nfd).Name != "Beyoncé" {
		t.Errorf("stored name %q, want NFC", e.artist(nfd).Name)
	}

	e.importAlbum("AC/DC", "Back in Black")
	before := snapshotCounts(e)
	out := e.commit(candidate(e.runningImport(), "AC_DC", "Highway to Hell"))
	if out.State != jobs.StateFailed || out.ErrorCode != catalog.CodeArtistFolderConflict ||
		!strings.Contains(out.ErrorMessage, `"AC_DC"`) || !strings.Contains(out.ErrorMessage, `"AC/DC"`) {
		t.Errorf("outcome %+v, want %s naming both artists", out, catalog.CodeArtistFolderConflict)
	}
	if after := snapshotCounts(e); after != before {
		t.Errorf("rows %v -> %v", before, after)
	}
}

// Every domain rule of the closed input (§4, §5.2, §7.2, §7.4, §8.5): the
// job fails with the rule's code and nothing else is written.
func TestCommitImportValidation(t *testing.T) {
	e := newEnv(t)
	lrcOf := func(c *catalog.ImportCandidate) *catalog.ImportLyrics { return c.Tracks[0].Lyrics }
	addTracks := func(c *catalog.ImportCandidate, n int) {
		for i := range n {
			h := newHash()
			c.Blobs = append(c.Blobs, catalog.Blob{Hash: h, Size: 1, Format: catalog.FormatFLAC})
			c.Tracks = append(c.Tracks, catalog.ImportTrack{SourcePath: fmt.Sprintf("x/%04d.flac", i), Disc: 2, No: i%999 + 1,
				Title: "t", BlobHash: h})
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(c *catalog.ImportCandidate)
		code   string
	}{
		{"fingerprint", func(c *catalog.ImportCandidate) { c.Fingerprint = "ABC" }, catalog.CodeInvalidFingerprint},
		{"empty artist", func(c *catalog.ImportCandidate) { c.Artist = " " }, names.CodeTextEmpty},
		{"control title", func(c *catalog.ImportCandidate) { c.Title = "a\tb" }, names.CodeTextControlChar},
		{"long title", func(c *catalog.ImportCandidate) { c.Title = strings.Repeat("x", 1025) }, names.CodeTextTooLong},
		{"year 0", func(c *catalog.ImportCandidate) { c.Year = ptr(0) }, catalog.CodeInvalidYear},
		{"year 10000", func(c *catalog.ImportCandidate) { c.Year = ptr(10000) }, catalog.CodeInvalidYear},
		{"genre", func(c *catalog.ImportCandidate) { c.Genre = ptr("\x00") }, names.CodeTextControlChar},
		{"disc 0", func(c *catalog.ImportCandidate) { c.Tracks[0].Disc = 0 }, catalog.CodeInvalidDisc},
		{"disc 100", func(c *catalog.ImportCandidate) { c.Tracks[0].Disc = 100 }, catalog.CodeInvalidDisc},
		{"number 0", func(c *catalog.ImportCandidate) { c.Tracks[0].No = 0 }, catalog.CodeInvalidTrackNumber},
		{"number 1000", func(c *catalog.ImportCandidate) { c.Tracks[0].No = 1000 }, catalog.CodeInvalidTrackNumber},
		{"same number", func(c *catalog.ImportCandidate) { c.Tracks[1].No = 1 }, catalog.CodeDuplicateTrackNumber},
		{"empty track title", func(c *catalog.ImportCandidate) { c.Tracks[0].Title = "" }, names.CodeTextEmpty},
		{"empty track artist", func(c *catalog.ImportCandidate) { c.Tracks[0].Artist = ptr("") }, names.CodeTextEmpty},
		{"no tracks", func(c *catalog.ImportCandidate) {
			c.Tracks, c.CoverHash = nil, nil
			c.Blobs, c.Attachments = c.Blobs[4:], c.Attachments[:1]
		}, catalog.CodeNoTracks},
		{"1001 tracks", func(c *catalog.ImportCandidate) { addTracks(c, 999) }, catalog.CodeTooManyFiles},
		{"10001 files", func(c *catalog.ImportCandidate) {
			// 2 tracks, 1 LRC, 2 attachments: 9,996 more make 10,001 files.
			for i := range 9996 {
				c.Attachments = append(c.Attachments, catalog.ImportAttachment{RelPath: fmt.Sprintf("many/%05d.txt", i), BlobHash: c.Blobs[4].Hash})
			}
		}, catalog.CodeTooManyFiles},
		{"blob not listed", func(c *catalog.ImportCandidate) { c.Tracks[0].BlobHash = newHash() }, catalog.CodeInvalidBlob},
		{"blob not used", func(c *catalog.ImportCandidate) {
			c.Blobs = append(c.Blobs, catalog.Blob{Hash: newHash(), Size: 1})
		}, catalog.CodeInvalidBlob},
		{"blob twice", func(c *catalog.ImportCandidate) { c.Blobs = append(c.Blobs, c.Blobs[0]) }, catalog.CodeInvalidBlob},
		{"bad hash", func(c *catalog.ImportCandidate) {
			c.Blobs[4].Hash = strings.ToUpper(c.Blobs[4].Hash)
		}, catalog.CodeInvalidBlob},
		{"negative size", func(c *catalog.ImportCandidate) { c.Blobs[0].Size = -1 }, catalog.CodeInvalidBlob},
		{"unknown format", func(c *catalog.ImportCandidate) { c.Blobs[4].Format = "pdf" }, catalog.CodeInvalidBlobFormat},
		{"track not audio", func(c *catalog.ImportCandidate) { c.Blobs[0].Format = catalog.FormatJPEG }, catalog.CodeInvalidBlobFormat},
		{"track of unknown content", func(c *catalog.ImportCandidate) { c.Blobs[0].Format = "" }, catalog.CodeInvalidBlobFormat},
		{"cover not an image", func(c *catalog.ImportCandidate) { c.CoverHash = &c.Blobs[1].Hash }, catalog.CodeInvalidCover},
		{"cover of unknown content", func(c *catalog.ImportCandidate) { c.Blobs[3].Format = "" }, catalog.CodeInvalidCover},
		{"cover over 20 MiB", func(c *catalog.ImportCandidate) { c.Blobs[3].Size = catalog.MaxCoverBytes + 1 }, catalog.CodeInvalidCover},
		{"LRC with a format", func(c *catalog.ImportCandidate) { c.Blobs[2].Format = catalog.FormatPNG }, catalog.CodeInvalidBlobFormat},
		{"source twice", func(c *catalog.ImportCandidate) { c.Tracks[1].SourcePath = c.Tracks[0].SourcePath }, catalog.CodeDuplicateSource},
		{"attachment is a track", func(c *catalog.ImportCandidate) { c.Attachments[0].RelPath = c.Tracks[1].SourcePath }, catalog.CodeDuplicateSource},
		{"absolute source", func(c *catalog.ImportCandidate) { c.Tracks[0].SourcePath = "/etc/passwd" }, names.CodePathAbsolute},
		{"dot-dot source", func(c *catalog.ImportCandidate) { c.Attachments[0].RelPath = "a/../b" }, names.CodePathDotSegment},
		{"LRC not .lrc", func(c *catalog.ImportCandidate) { lrcOf(c).SourcePath = "CD1/01 So What.txt" }, catalog.CodeLyricsAssociation},
		{"LRC in another directory", func(c *catalog.ImportCandidate) { lrcOf(c).SourcePath = "01 So What.lrc" }, catalog.CodeLyricsAssociation},
		{"LRC of another stem", func(c *catalog.ImportCandidate) { lrcOf(c).SourcePath = "CD1/02 Freddie.lrc" }, catalog.CodeLyricsAssociation},
		{"LRC ambiguous", func(c *catalog.ImportCandidate) {
			h := newHash()
			c.Blobs = append(c.Blobs, catalog.Blob{Hash: h, Size: 1, Format: catalog.FormatMP3})
			c.Tracks = append(c.Tracks, catalog.ImportTrack{SourcePath: "CD1/01 SO WHAT.mp3", Disc: 1, No: 3, Title: "x", BlobHash: h})
		}, catalog.CodeLyricsAssociation},
		{"LRC is a track", func(c *catalog.ImportCandidate) { lrcOf(c).SourcePath = c.Tracks[0].SourcePath }, catalog.CodeDuplicateSource},
		{"attachments collide", func(c *catalog.ImportCandidate) {
			c.Attachments = append(c.Attachments, catalog.ImportAttachment{RelPath: "scans/booklet.PDF", BlobHash: c.Blobs[4].Hash})
		}, catalog.CodeAttachmentCollision},
		{"attachments collide by sanitization", func(c *catalog.ImportCandidate) {
			c.Attachments = append(c.Attachments,
				catalog.ImportAttachment{RelPath: "a:b.txt", BlobHash: c.Blobs[4].Hash},
				catalog.ImportAttachment{RelPath: "a_b.txt", BlobHash: c.Blobs[4].Hash})
		}, catalog.CodeAttachmentCollision},
		{"file and directory", func(c *catalog.ImportCandidate) {
			c.Attachments = append(c.Attachments, catalog.ImportAttachment{RelPath: "SCANS", BlobHash: c.Blobs[4].Hash})
		}, catalog.CodeAttachmentCollision},
		{"warning", func(c *catalog.ImportCandidate) { c.Warnings = []jobs.Warning{{Code: "made_up", Message: "m"}} },
			catalog.CodeInvalidImportWarnings},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := candidate(e.runningImport(), "Validation", "Album "+tc.name)
			tc.mutate(&c)
			before := snapshotCounts(e)
			out := e.commit(c)
			if out.State != jobs.StateFailed || out.ErrorCode != tc.code || out.AlbumID != uuid.Nil || out.ErrorMessage == "" {
				t.Fatalf("outcome %+v, want failed %s", out, tc.code)
			}
			if after := snapshotCounts(e); after != before {
				t.Errorf("rows %v -> %v", before, after)
			}
			if j := e.job(c.Attempt.JobID); j.State != "failed" || *j.ErrorCode != tc.code {
				t.Errorf("job %s %v", j.State, j.ErrorCode)
			}
		})
	}
	// The limits themselves are accepted.
	t.Run("1000 tracks", func(t *testing.T) {
		c := candidate(e.runningImport(), "Validation", "Exactly 1000")
		addTracks(&c, 998)
		if out := e.commit(c); out.State != jobs.StateDone {
			t.Fatalf("outcome %+v", out)
		}
	})
}

// The blob rows already in the database are part of the check (N-102).
func TestCommitImportRecordedBlobs(t *testing.T) {
	e := newEnv(t)
	record := func(hash string, size int64, format *string) {
		e.exec(`INSERT INTO blobs (hash, size, format, created_at) VALUES ($1, $2, $3, now())`, hash, size, format)
	}
	t.Run("size differs", func(t *testing.T) {
		c := candidate(e.runningImport(), "Blobs", "Size")
		record(c.Blobs[4].Hash, c.Blobs[4].Size+1, nil)
		if out := e.commit(c); out.ErrorCode != catalog.CodeBlobMismatch {
			t.Errorf("outcome %+v, want %s", out, catalog.CodeBlobMismatch)
		}
	})
	t.Run("format differs", func(t *testing.T) {
		c := candidate(e.runningImport(), "Blobs", "Format")
		record(c.Blobs[0].Hash, c.Blobs[0].Size, ptr(catalog.FormatMP3))
		if out := e.commit(c); out.ErrorCode != catalog.CodeInvalidBlobFormat {
			t.Errorf("outcome %+v, want %s", out, catalog.CodeInvalidBlobFormat)
		}
	})
	t.Run("recorded format breaks a role", func(t *testing.T) {
		c := candidate(e.runningImport(), "Blobs", "Role")
		record(c.Blobs[2].Hash, c.Blobs[2].Size, ptr(catalog.FormatPNG)) // the LRC
		if out := e.commit(c); out.ErrorCode != catalog.CodeInvalidBlobFormat {
			t.Errorf("outcome %+v, want %s", out, catalog.CodeInvalidBlobFormat)
		}
	})
	t.Run("unknown format learned", func(t *testing.T) {
		c := candidate(e.runningImport(), "Blobs", "Learned")
		record(c.Blobs[3].Hash, c.Blobs[3].Size, nil) // the cover, uploaded as an attachment before
		record(c.Blobs[4].Hash, c.Blobs[4].Size, ptr(catalog.FormatPNG))
		if out := e.commit(c); out.State != jobs.StateDone {
			t.Fatalf("outcome %+v", out)
		}
		var cover, booklet *string
		if err := e.db.QueryRow(context.Background(), `SELECT (SELECT format FROM blobs WHERE hash = $1), (SELECT format FROM blobs WHERE hash = $2)`,
			c.Blobs[3].Hash, c.Blobs[4].Hash).Scan(&cover, &booklet); err != nil {
			t.Fatal(err)
		}
		if cover == nil || *cover != catalog.FormatJPEG || booklet == nil || *booklet != catalog.FormatPNG {
			t.Errorf("formats %v %v, want jpeg learned and png kept", cover, booklet)
		}
	})
}

// A commit for an attempt that is not the running one changes nothing.
func TestCommitImportStaleAttempt(t *testing.T) {
	e := newEnv(t)
	a := e.runningImport()
	before := snapshotCounts(e)
	for _, tc := range []struct {
		name    string
		attempt jobs.Attempt
		code    string
	}{
		{"wrong ticket", jobs.Attempt{JobID: a.JobID, Ticket: a.Ticket + 1}, jobs.CodeAttemptStale},
		{"unknown job", jobs.Attempt{JobID: store.NewID(), Ticket: 1}, jobs.CodeNotFound},
	} {
		_, err := e.svc.CommitImport(context.Background(), candidate(tc.attempt, "Stale", "Attempt"))
		if catalog.Code(err) != tc.code {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.code)
		}
	}
	scan := store.NewID()
	e.exec(`INSERT INTO jobs (id, kind, batch_id, state, queued_at, updated_at) VALUES ($1, 'scan', $2, 'pending', now(), now())`,
		scan, e.batch)
	_, err := e.svc.CommitImport(context.Background(), candidate(e.claimJob(scan), "Scan", "Job"))
	if catalog.Code(err) != jobs.CodeAttemptStale {
		t.Errorf("a scan job: %v, want %s", err, jobs.CodeAttemptStale)
	}
	if after := snapshotCounts(e); after != before {
		t.Errorf("rows %v -> %v", before, after)
	}
	if j := e.job(a.JobID); j.State != "running" {
		t.Errorf("the running job became %s", j.State)
	}
}

// N-091: the cover is checked against every audio format of the album,
// each once, in order; a refusal fails the import with invalid_cover.
func TestCommitImportCoverFits(t *testing.T) {
	e := newEnv(t)
	var asked []string
	e.fits = func(cover catalog.Blob, format string) error {
		asked = append(asked, format)
		if cover.Format != catalog.FormatJPEG || cover.Size != 500_000 {
			t.Errorf("asked about %+v", cover)
		}
		if format == catalog.FormatMP3 {
			return errors.New("too large for an ID3 frame")
		}
		return nil
	}
	c := candidate(e.runningImport(), "Cover", "Fits")
	h := newHash()
	c.Blobs = append(c.Blobs, catalog.Blob{Hash: h, Size: 1, Format: catalog.FormatMP3})
	c.Tracks = append(c.Tracks, catalog.ImportTrack{SourcePath: "CD1/03.mp3", Disc: 1, No: 3, Title: "x", BlobHash: h})
	out := e.commit(c)
	if !reflect.DeepEqual(asked, []string{catalog.FormatFLAC, catalog.FormatMP3}) {
		t.Errorf("asked %v, want [flac mp3]", asked)
	}
	if out.State != jobs.StateFailed || out.ErrorCode != catalog.CodeInvalidCover || !strings.Contains(out.ErrorMessage, "mp3") {
		t.Errorf("outcome %+v", out)
	}

	asked = nil
	c = candidate(e.runningImport(), "Cover", "None")
	c.CoverHash = nil
	if out := e.commit(c); out.State != jobs.StateDone || asked != nil {
		t.Errorf("without a cover: %+v, asked %v", out, asked)
	}

	if _, err := catalog.New(e.db, nil, nil); catalog.Code(err) != catalog.CodeInvalidArgument {
		t.Errorf("New without a cover check: %v", err)
	}
}
