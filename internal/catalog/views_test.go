package catalog_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

// §10.2 POST /api/artists: a new artist at revision 1; §7.6's identity
// gives artist_exists, a sanitization-only collision
// artist_folder_conflict, both with the existing artist; nothing merged,
// nothing enqueued.
func TestCreateArtist(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, err := e.svc.CreateArtist(ctx, " Stráuß ") // NFC and trim
	if err != nil || a.Name != "Stráuß" || a.Revision != 1 {
		t.Fatalf("CreateArtist = %+v, %v", a, err)
	}
	if got := e.artist(a.ID); got.Name != "Stráuß" || got.FolderKey != names.FolderKey("Stráuß") || got.Revision != 1 {
		t.Fatalf("row %+v", got)
	}
	for _, name := range []string{"STRÁUSS", "stráuß", "Stráuss"} {
		got, err := e.svc.CreateArtist(ctx, name)
		ce := wantCode(t, err, catalog.CodeArtistExists)
		if got != a || ce.Details.ArtistID != a.ID || len(ce.Details.Names) != 2 || ce.Details.Names[1] != "Stráuß" {
			t.Fatalf("%q: %+v, %+v", name, got, ce)
		}
	}
	acdc, err := e.svc.CreateArtist(ctx, "AC/DC")
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.CreateArtist(ctx, "AC_DC")
	ce := wantCode(t, err, catalog.CodeArtistFolderConflict)
	if got != acdc || ce.Details.ArtistID != acdc.ID {
		t.Fatalf("AC_DC: %+v, %+v", got, ce)
	}
	for name, code := range map[string]string{"": names.CodeTextEmpty, " \t": names.CodeTextEmpty, "a\x00b": names.CodeTextControlChar} {
		if got, err := e.svc.CreateArtist(ctx, name); catalog.Code(err) != code || got.ID.String() != "00000000-0000-0000-0000-000000000000" {
			t.Errorf("%q: %+v, %v; want %s", name, got, err, code)
		}
	}
	if n := e.count(`SELECT count(*) FROM artists`); n != 2 {
		t.Fatalf("%d artists", n)
	}
	if n := e.count(`SELECT count(*) FROM jobs`); n != 0 || e.wakes.Load() != 0 {
		t.Fatalf("a creation enqueued %d jobs or woke the pool", n)
	}
}

// Eight concurrent creations of one name: one artist, seven
// artist_exists naming it; never a database error (the catalog lock).
func TestCreateArtistConcurrent(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	results := make([]catalog.Artist, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = e.svc.CreateArtist(context.Background(), "Nina Simone")
		}()
	}
	wg.Wait()
	created := 0
	for i, err := range errs {
		switch catalog.Code(err) {
		case "":
			created++
		case catalog.CodeArtistExists:
		default:
			t.Fatalf("creation %d: %v", i, err)
		}
	}
	for i := range results {
		if results[i] != results[0] {
			t.Fatalf("different artists %+v, %+v", results[i], results[0])
		}
	}
	if created != 1 || e.count(`SELECT count(*) FROM artists`) != 1 {
		t.Fatalf("%d created", created)
	}
}

// Owner decision N-146: every artist of the catalog, one just created
// without albums and one whose only album is in the trash included; order
// by folder key, byte-wise.
func TestListArtists(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.importAlbum("zappa", "Hot Rats")
	e.importAlbum("Abba", "Arrival")
	trashed := e.importAlbum("Émile", "Gone")
	e.importAlbum("abba", "Voulez-Vous") // the same artist
	if _, _, err := e.svc.TrashAlbum(ctx, trashed, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateArtist(ctx, "Nobody"); err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.ListArtists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range list {
		got = append(got, fmt.Sprintf("%s@%d", a.Name, a.Revision))
	}
	if fmt.Sprint(got) != "[Abba@1 Nobody@1 zappa@1 Émile@1]" {
		t.Fatalf("list %v", got)
	}
	if _, err := e.svc.GetArtist(ctx, store.NewID()); catalog.Code(err) != catalog.CodeArtistNotFound {
		t.Fatalf("unknown artist: %v", err)
	}
}

// The album's desired aggregate and its status, each from one snapshot.
func TestGetAlbumView(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Miles Davis", "Kind of Blue")
	v, err := e.svc.GetAlbum(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	al := e.album(id)
	if v.ID != id || v.Revision != 1 || v.ArtistID != al.ArtistID || v.ArtistName != "Miles Davis" || v.Title != "Kind of Blue" ||
		*v.Year != 1959 || *v.Genre != "Jazz" || v.Compilation || v.Trashed || v.Cover == nil || v.Cover.Hash != *al.CoverHash ||
		v.Cover.Format != catalog.FormatJPEG || v.Cover.Size != 500_000 {
		t.Fatalf("view %+v", v)
	}
	if len(v.Tracks) != 2 || v.Tracks[0].Title != "So What" || v.Tracks[0].LyricsHash == nil || v.Tracks[0].Blob.Format != catalog.FormatFLAC ||
		v.Tracks[0].SourcePath != "CD1/01 So What.flac" || *v.Tracks[1].Artist != "Miles & Cannonball" || *v.Tracks[1].Genre != "" {
		t.Fatalf("tracks %+v", v.Tracks)
	}
	if len(v.Attachments) != 2 || v.Attachments[0].RelPath != "cover.jpg" || v.Attachments[1].RelPath != "Scans/Booklet.pdf" ||
		v.Attachments[1].Blob.Format != "" {
		t.Fatalf("attachments %+v", v.Attachments)
	}
	if _, err := e.svc.GetAlbum(ctx, store.NewID()); catalog.Code(err) != catalog.CodeAlbumNotFound {
		t.Fatalf("unknown album: %v", err)
	}

	st, err := e.svc.GetAlbumStatus(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Revision != 1 || st.PublishedRevision != 0 || st.PublishedPath != nil || st.Job == nil || st.Job.State != "pending" {
		t.Fatalf("status %+v", st)
	}
	e.clearRenders()
	e.published(id, "Miles Davis/Kind of Blue")
	if st, err = e.svc.GetAlbumStatus(ctx, id); err != nil || st.Job != nil || *st.PublishedPath != "Miles Davis/Kind of Blue" ||
		st.PublishedRevision != 1 || *st.PublishedRenderer != "rv-test" {
		t.Fatalf("status %+v, %v", st, err)
	}
	if _, err := e.svc.GetAlbumStatus(ctx, store.NewID()); catalog.Code(err) != catalog.CodeAlbumNotFound {
		t.Fatalf("unknown album: %v", err)
	}
}

// §10.2 POST /api/albums/{id}/render: the seen revision is required and
// compared in the transaction; no bump; the single enqueue, coalescing
// with a running attempt (§6.3).
func TestRequestRender(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Artist", "Album")
	e.clearRenders()
	wakes := e.wakes.Load()
	if _, _, err := e.svc.RequestRender(ctx, id, 0); catalog.Code(err) != catalog.CodePreconditionRequired {
		t.Fatalf("no revision: %v", err)
	}
	ce := wantCode(t, func() error { _, _, err := e.svc.RequestRender(ctx, id, 2); return err }(), catalog.CodePreconditionFailed)
	if ce.Details.Revision != 1 {
		t.Fatalf("412 details %+v", ce.Details)
	}
	if _, _, err := e.svc.RequestRender(ctx, store.NewID(), 1); catalog.Code(err) != catalog.CodeAlbumNotFound {
		t.Fatalf("unknown album: %v", err)
	}
	if _, ok := e.renderJob(id); ok || e.wakes.Load() != wakes {
		t.Fatal("a refused render enqueued a job or woke the pool")
	}
	rev, enq, err := e.svc.RequestRender(ctx, id, 1)
	if err != nil || rev != 1 || enq.State != jobs.StatePending || e.album(id).Revision != 1 || e.wakes.Load() != wakes+1 {
		t.Fatalf("RequestRender = %d, %+v, %v", rev, enq, err)
	}
	// A running attempt stays running with its claim; the new ticket is
	// more work after it.
	a := e.claimJob(enq.JobID)
	_, enq2, err := e.svc.RequestRender(ctx, id, 1)
	if err != nil || enq2.JobID != enq.JobID || enq2.State != jobs.StateRunning || enq2.Claimed != a.Ticket || enq2.Requested <= a.Ticket {
		t.Fatalf("coalesced %+v, %v (attempt %+v)", enq2, err, a)
	}
	// A trashed album is enqueued too: its render is the removal.
	if _, _, err := e.svc.TrashAlbum(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.RequestRender(ctx, id, 2); err != nil {
		t.Fatal(err)
	}
}

// N-150: a failed job's stored message never carries database text.
func TestJobMessage(t *testing.T) {
	e := newEnv(t)
	// A real PostgreSQL error, from a real statement.
	_, pgErr := e.db.Exec(context.Background(), `INSERT INTO artists (id, name, folder_key, revision) VALUES ($1, 'x', 'x', 0)`, store.NewID())
	var pg *pgconn.PgError
	if !errors.As(pgErr, &pg) {
		t.Fatalf("no PgError: %v", pgErr)
	}
	for _, tc := range []struct {
		name string
		err  error
		safe bool
	}{
		{"a content error", &catalog.Error{Code: catalog.CodeInvalidYear, Message: "year"}, true},
		{"a plain error", errors.New("decoder refused the stream"), true},
		{"a raw PgError", pgErr, false},
		{"a PgError wrapped", fmt.Errorf("render: %w", pgErr), false},
		{"catalog_db without a PgError", &catalog.Error{Code: catalog.CodeDB, Message: "reading", Err: errors.New("conn closed")}, false},
		{"job_db", &jobs.Error{Code: jobs.CodeDB, Msg: "claim", Err: errors.New("dial tcp 10.0.0.5:5432")}, false},
		{"a store error", &store.Error{Code: store.CodeConnectionLost, Msg: "x"}, false},
		{"catalog_db under another typed error", &catalog.Error{Code: catalog.CodeInvalidCover, Message: "cover",
			Err: &catalog.Error{Code: catalog.CodeDB, Message: "reading blob", Err: errors.New("x")}}, false},
		{"one of a joined pair", errors.Join(errors.New("fine"), &jobs.Error{Code: jobs.CodeDB, Msg: "x"}), false},
		{"nil", nil, true},
	} {
		got := catalog.JobMessage(tc.err, "the original message")
		if want := map[bool]string{true: "the original message", false: catalog.DatabaseJobMessage}[tc.safe]; got != want {
			t.Errorf("%s: %q, want %q", tc.name, got, want)
		}
	}
}
