package catalog_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
)

// Moving tracks to another album (POST /api/albums/{id}/move-tracks): one
// catalog transaction that changes both albums, or neither.

// trackState is everything of a track a move must carry unchanged, and
// where it is.
type trackState struct {
	Album         uuid.UUID
	Disc, No      int32
	Title, Source string
	Artist, Genre *string
	Blob          string
	Lyrics        *string
}

func (e *env) trackState(id uuid.UUID) trackState {
	e.t.Helper()
	var s trackState
	if err := e.db.QueryRow(context.Background(),
		`SELECT album_id, disc, no, title, source_path, artist, genre, blob_hash, lyrics_hash FROM tracks WHERE id = $1`, id).
		Scan(&s.Album, &s.Disc, &s.No, &s.Title, &s.Source, &s.Artist, &s.Genre, &s.Blob, &s.Lyrics); err != nil {
		e.t.Fatalf("track %s: %v", id, err)
	}
	return s
}

// albumState is what a refused move must leave as it was.
type albumState struct {
	Revision int64
	Trashed  bool
	Tracks   string
	Claims   string
}

func (e *env) albumState(id uuid.UUID) albumState {
	e.t.Helper()
	a := e.album(id)
	var tracks string
	for _, t := range e.tracks(id) {
		tracks += fmt.Sprintf("%s %d %d %q %v %q %v %q %s;", t.ID, t.Disc, t.No, t.Title, t.Artist != nil, deref(t.Artist), t.Genre != nil, deref(t.Genre), t.BlobHash)
	}
	return albumState{a.Revision, a.DeletedAt != nil, tracks, fmt.Sprint(e.claims(id))}
}

func (e *env) newTrack(album uuid.UUID, title, format string, disc, no int) uuid.UUID {
	e.t.Helper()
	_, id, err := e.svc.AddTrack(context.Background(), album, e.album(album).Revision, catalog.NewTrack{
		Blob:       catalog.Blob{Hash: newHash(), Size: 4000, Format: format},
		SourcePath: title + ".x", Title: title, Disc: disc, No: no,
	})
	if err != nil {
		e.t.Fatalf("AddTrack %q: %v", title, err)
	}
	return id
}

func (e *env) moveAll(from, to uuid.UUID) (catalog.MoveResult, error) {
	e.t.Helper()
	var ids []uuid.UUID
	for _, t := range e.tracks(from) {
		ids = append(ids, t.ID)
	}
	return e.svc.MoveTracks(context.Background(), from, e.album(from).Revision, ids, to)
}

// The tracks land one after the other, in their order in the source: at
// their own place when it is free, otherwise after the destination's last
// track; everything else goes with them. Both albums get a new revision
// and a render. The emptied source goes to the trash, cannot be restored,
// and is emptied from the trash with its artist; no blob goes.
func TestMoveTracks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dst := e.importAlbum("Miles Davis", "Kind of Blue") // tracks 1 and 2
	src := e.importAlbum("Mile Davis", "Kind of Blue")  // tracks 1 (with lyrics) and 2
	e.newTrack(src, "Bonus", catalog.FormatFLAC, 1, 5)
	last := e.newTrack(src, "Disc Two", catalog.FormatFLAC, 2, 1)
	e.clearRenders()
	ts := e.tracks(src)
	before := map[uuid.UUID]trackState{}
	for _, tr := range ts {
		before[tr.ID] = e.trackState(tr.ID)
	}
	blobs := e.count(`SELECT count(*) FROM blobs`)
	srcRev, dstRev, w0 := e.album(src).Revision, e.album(dst).Revision, e.wakes.Load()

	// The request's order does not matter: 1/1, 1/2, 1/5 in the source.
	res, err := e.svc.MoveTracks(ctx, src, srcRev, []uuid.UUID{ts[2].ID, ts[0].ID, ts[1].ID}, dst)
	if err != nil {
		t.Fatal(err)
	}
	if res != (catalog.MoveResult{From: srcRev + 1, To: dstRev + 1}) || e.wakes.Load() == w0 {
		t.Fatalf("result %+v", res)
	}
	for i, want := range [][2]int32{{1, 3}, {1, 4}, {1, 5}} {
		got := e.trackState(ts[i].ID)
		b := before[ts[i].ID]
		b.Album, b.Disc, b.No = dst, want[0], want[1]
		if !reflect.DeepEqual(got, b) {
			t.Errorf("track %d: %+v, want %+v", i, got, b)
		}
	}
	if e.trackState(ts[0].ID).Lyrics == nil {
		t.Fatal("the lyrics stayed behind")
	}
	if n := len(e.tracks(src)); n != 1 || e.album(src).DeletedAt != nil {
		t.Fatalf("the source has %d tracks, trashed %v", n, e.album(src).DeletedAt != nil)
	}
	for _, id := range []uuid.UUID{src, dst} {
		if j, ok := e.renderJob(id); !ok || j.State != "pending" {
			t.Fatalf("album %s: no pending render", id)
		}
	}
	if e.album(src).Revision != res.From || e.album(dst).Revision != res.To {
		t.Fatal("revisions")
	}
	e.clearRenders()

	// The last track: kept at disc 2, track 1. The source goes to the
	// trash; its render is the removal.
	srcRev, dstRev = res.From, res.To
	res, err = e.svc.MoveTracks(ctx, src, srcRev, []uuid.UUID{last}, dst)
	if err != nil {
		t.Fatal(err)
	}
	if res != (catalog.MoveResult{From: srcRev + 1, To: dstRev + 1, Trashed: true}) {
		t.Fatalf("result %+v", res)
	}
	if s := e.trackState(last); s.Album != dst || s.Disc != 2 || s.No != 1 {
		t.Fatalf("the last track %+v", s)
	}
	for _, id := range []uuid.UUID{src, dst} {
		if j, ok := e.renderJob(id); !ok || j.State != "pending" {
			t.Fatalf("album %s: no pending render", id)
		}
	}
	a := e.album(src)
	if a.DeletedAt == nil || len(e.tracks(src)) != 0 || len(e.tracks(dst)) != 6 || len(e.claims(src)) != 0 {
		t.Fatalf("source trashed %v with %d tracks, claims %v; destination %d tracks", a.DeletedAt != nil,
			len(e.tracks(src)), e.claims(src), len(e.tracks(dst)))
	}
	if e.album(dst).Revision != dstRev+1 || e.album(src).Revision != srcRev+1 {
		t.Fatal("revisions")
	}
	if e.count(`SELECT count(*) FROM attachments WHERE album_id = $1`, src) != 2 || e.album(src).CoverHash == nil {
		t.Fatal("the cover and the attachments of the source stay with it")
	}
	e.clearRenders()
	w0 = e.wakes.Load()

	// An album without tracks stays in the trash.
	_, _, err = e.svc.RestoreAlbum(ctx, src, res.From)
	wantCode(t, err, catalog.CodeNoTracks)
	e.unchanged(src, res.From, w0)

	// Its artist keeps it until the trash is emptied.
	artist := a.ArtistID
	if !e.artistExists(artist) {
		t.Fatal("the source's artist went with its tracks")
	}
	e.removalPublished(src)
	e.emptyTrash(1, 0)
	if e.artistExists(artist) || e.orphans() != 0 || e.count(`SELECT count(*) FROM blobs`) != blobs {
		t.Fatal("after emptying the trash")
	}
	if len(e.tracks(dst)) != 6 {
		t.Fatal("the moved tracks went with the emptied album")
	}
}

// Every refusal leaves both albums exactly as they were: no revision, no
// render, no track moved, the source not trashed.
func TestMoveTracksRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := e.importAlbum("Mile Davis", "Kind of Blue")
	dst := e.importAlbum("Miles Davis", "Kind of Blue")
	other := e.importAlbum("Bill Evans", "Portrait in Jazz")
	trashed := e.importAlbum("Chet Baker", "Chet")
	e.trash(trashed)
	e.clearRenders()
	ts := e.tracks(src)
	all := []uuid.UUID{ts[0].ID, ts[1].ID}
	w0 := e.wakes.Load()
	check := func(what string) {
		t.Helper()
		for _, id := range []uuid.UUID{src, dst} {
			e.unchanged(id, e.album(id).Revision, w0)
		}
		if len(e.tracks(src)) != 2 || len(e.tracks(dst)) != 2 || e.album(src).DeletedAt != nil {
			t.Fatalf("%s: tracks moved or source trashed", what)
		}
	}
	srcState, dstState := e.albumState(src), e.albumState(dst)
	rev := srcState.Revision
	move := func(rev int64, ids []uuid.UUID, to uuid.UUID) error {
		_, err := e.svc.MoveTracks(ctx, src, rev, ids, to)
		return err
	}

	wantCode(t, move(0, all, dst), catalog.CodePreconditionRequired)
	wantCode(t, move(rev+1, all, dst), catalog.CodePreconditionFailed)
	_, err := e.svc.MoveTracks(ctx, uuid.New(), 1, all, dst)
	wantCode(t, err, catalog.CodeAlbumNotFound)
	missing := uuid.New()
	if ce := wantCode(t, move(rev, all, missing), catalog.CodeAlbumNotFound); ce.Details.AlbumID != missing {
		t.Fatalf("details %+v", ce.Details)
	}
	if ce := wantCode(t, move(rev, all, trashed), catalog.CodeAlbumTrashed); ce.Details.AlbumID != trashed {
		t.Fatalf("details %+v", ce.Details)
	}
	wantCode(t, move(rev, all, src), catalog.CodeSameAlbum)
	wantCode(t, move(rev, nil, dst), catalog.CodeTrackListMismatch)
	wantCode(t, move(rev, []uuid.UUID{ts[0].ID, ts[0].ID}, dst), catalog.CodeTrackListMismatch)
	wantCode(t, move(rev, []uuid.UUID{ts[0].ID, e.tracks(other)[0].ID}, dst), catalog.CodeTrackNotFound)
	wantCode(t, move(rev, []uuid.UUID{e.tracks(dst)[0].ID}, other), catalog.CodeTrackNotFound)
	check("the argument refusals")

	// A path conflict of the destination rolls back the source's trash,
	// revision and render too.
	claimed := keys(e.claims(dst))
	e.exec(`UPDATE path_claims SET album_id = $2 WHERE album_id = $1`, dst, other)
	wantCode(t, move(rev, all, dst), catalog.CodePathReserved)
	e.exec(`UPDATE path_claims SET album_id = $1 WHERE path_key = ANY($2)`, dst, claimed)
	check("the path conflict")
	if e.albumState(src) != srcState || e.albumState(dst) != dstState {
		t.Fatalf("after the path conflict:\n%+v\n%+v", e.albumState(src), e.albumState(dst))
	}

	// The genre rule with the destination's formats: a FLAC track whose
	// genre an MP3 of the destination cannot hold.
	e.newTrack(dst, "MP3", catalog.FormatMP3, 0, 0)
	u := e.update(src)
	u.Tracks[0].Genre = ptr("(Rock)")
	if _, _, err := e.svc.UpdateAlbum(ctx, src, rev, u); err != nil {
		t.Fatal(err)
	}
	e.clearRenders()
	srcState, dstState, rev = e.albumState(src), e.albumState(dst), rev+1
	w0 = e.wakes.Load()
	wantCode(t, move(rev, all, dst), catalog.CodeGenreNotWritable)
	if e.albumState(src) != srcState || e.albumState(dst) != dstState {
		t.Fatal("after the genre refusal")
	}
	u.Tracks[0].Genre = nil
	if _, _, err := e.svc.UpdateAlbum(ctx, src, rev, u); err != nil {
		t.Fatal(err)
	}
	rev++

	// The cover rule: the cover of a destination with only FLAC tracks
	// cannot be embedded in an MP3 of the source.
	mp3 := e.newTrack(src, "Source MP3", catalog.FormatMP3, 0, 0)
	rev++
	e.mu.Lock()
	e.fits = func(c catalog.Blob, format string) error {
		if format == catalog.FormatMP3 {
			return errors.New("too large for mp3")
		}
		return nil
	}
	e.mu.Unlock()
	e.clearRenders()
	srcState, otherState, w0 := e.albumState(src), e.albumState(other), e.wakes.Load()
	_, err = e.svc.MoveTracks(ctx, src, rev, []uuid.UUID{mp3}, other)
	wantCode(t, err, catalog.CodeCoverNotEmbeddable)
	if e.albumState(src) != srcState || e.albumState(other) != otherState {
		t.Fatal("after the cover refusal")
	}
	e.unchanged(src, rev, w0)
	e.mu.Lock()
	e.fits = nil
	e.mu.Unlock()

	// The same audio already in the destination: never two tracks of it.
	var hash string
	if err := e.db.QueryRow(ctx, `SELECT blob_hash FROM tracks WHERE id = $1`, ts[1].ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.AddTrack(ctx, dst, e.album(dst).Revision, catalog.NewTrack{
		Blob: catalog.Blob{Hash: hash, Size: 25_000_000, Format: catalog.FormatFLAC}, SourcePath: "copy.flac", Title: "Copy"}); err != nil {
		t.Fatal(err)
	}
	e.clearRenders()
	srcState, dstState = e.albumState(src), e.albumState(dst)
	if ce := wantCode(t, move(rev, all, dst), catalog.CodeTrackExists); ce.Details.TrackID == uuid.Nil || ce.Details.Names[0] != "Copy" {
		t.Fatalf("details %+v", ce.Details)
	}
	if e.albumState(src) != srcState || e.albumState(dst) != dstState {
		t.Fatal("after the duplicate refusal")
	}
}

// The destination's revision is not asked: its editor, still on the old
// revision, gets 412 on its next save.
func TestMoveTracksDestinationRevision(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := e.importAlbum("Mile Davis", "Kind of Blue")
	dst := e.importAlbum("Miles Davis", "Kind of Blue")
	open := e.update(dst)
	rev := e.album(dst).Revision
	if _, err := e.svc.MoveTracks(ctx, src, e.album(src).Revision, []uuid.UUID{e.tracks(src)[0].ID}, dst); err != nil {
		t.Fatal(err)
	}
	open.Title = "Kind of Blue (Legacy)"
	_, _, err := e.svc.UpdateAlbum(ctx, dst, rev, open)
	wantCode(t, err, catalog.CodePreconditionFailed)
}

// A trashed album's tracks can be moved out of it to an active album; it
// stays in the trash, without tracks once the last one goes.
func TestMoveTracksOutOfTrash(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := e.importAlbum("Mile Davis", "Kind of Blue")
	dst := e.importAlbum("Miles Davis", "Kind of Blue")
	e.trash(src)
	e.clearRenders()
	ts := e.tracks(src)
	for i, tr := range ts {
		rev, drev := e.album(src).Revision, e.album(dst).Revision
		res, err := e.svc.MoveTracks(ctx, src, rev, []uuid.UUID{tr.ID}, dst)
		if err != nil {
			t.Fatal(err)
		}
		if res != (catalog.MoveResult{From: rev + 1, To: drev + 1}) {
			t.Fatalf("move %d: %+v", i, res)
		}
		if e.album(src).DeletedAt == nil || len(e.tracks(src)) != len(ts)-i-1 {
			t.Fatalf("move %d: the source", i)
		}
	}
	if len(e.tracks(dst)) != 4 {
		t.Fatal("the destination")
	}
}

// A move races a save of the destination and the claim of the source's
// render. The save either comes first, or meets 412 (the move bumped the
// destination); the move always succeeds. The claim's snapshot is the
// source before the move, with its tracks, or after it, trashed and empty:
// never half moved.
func TestMoveTracksRaces(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for round := range 10 {
		src := e.importAlbum("Mile Davis", fmt.Sprintf("Album %d", round))
		dst := e.importAlbum("Miles Davis", fmt.Sprintf("Album %d", round))
		e.clearRenders()
		srcRev, dstRev := e.album(src).Revision, e.album(dst).Revision
		if _, _, err := e.svc.RequestRender(ctx, src, srcRev); err != nil {
			t.Fatal(err)
		}
		u := e.update(dst)
		u.Year = ptr(1960)
		var (
			wg             sync.WaitGroup
			moveErr, upErr error
			claim          *jobs.Claim
			claimErr       error
		)
		wg.Add(3)
		go func() { defer wg.Done(); _, moveErr = e.moveAll(src, dst) }()
		go func() { defer wg.Done(); _, _, upErr = e.svc.UpdateAlbum(ctx, dst, dstRev, u) }()
		go func() { defer wg.Done(); claim, claimErr = jobs.ClaimNext(ctx, e.db, "rv-test") }()
		wg.Wait()
		if moveErr != nil || claimErr != nil {
			t.Fatalf("round %d: move %v, claim %v", round, moveErr, claimErr)
		}
		want := dstRev + 1
		switch catalog.Code(upErr) {
		case "":
			want++
		case catalog.CodePreconditionFailed:
		default:
			t.Fatalf("round %d: save %v", round, upErr)
		}
		if got := e.album(dst).Revision; got != want || len(e.tracks(dst)) != 4 {
			t.Fatalf("round %d: destination at %d with %d tracks, want %d", round, got, len(e.tracks(dst)), want)
		}
		if a := e.album(src); a.DeletedAt == nil || a.Revision != srcRev+1 || len(e.tracks(src)) != 0 {
			t.Fatalf("round %d: source %+v", round, a)
		}
		if claim == nil {
			// The claim skipped the render row the move held: nothing to
			// check of it.
			continue
		}
		snap := claim.Render
		switch {
		case snap.Album.ID == src && snap.Album.Revision == srcRev && !snap.Album.Deleted && len(snap.Tracks) == 2:
		case snap.Album.ID == src && snap.Album.Revision == srcRev+1 && snap.Album.Deleted && len(snap.Tracks) == 0:
		case snap.Album.ID == dst && snap.Album.Revision > dstRev:
			// The move came first, and the destination's render was older.
		default:
			t.Fatalf("round %d: snapshot of %s at %d, deleted %v, %d tracks", round, snap.Album.ID,
				snap.Album.Revision, snap.Album.Deleted, len(snap.Tracks))
		}
	}
}

// Two moves that cross, each with its source's revision: the first bumps
// the other's source, so exactly one succeeds and the other is 412.
func TestMoveTracksCrossing(t *testing.T) {
	e := newEnv(t)
	for round := range 10 {
		a := e.importAlbum("A", fmt.Sprintf("Album %d", round))
		b := e.importAlbum("B", fmt.Sprintf("Album %d", round))
		ids := func(album uuid.UUID) []uuid.UUID {
			var out []uuid.UUID
			for _, t := range e.tracks(album) {
				out = append(out, t.ID)
			}
			return out
		}
		ra, rb, ta, tb := e.album(a).Revision, e.album(b).Revision, ids(a), ids(b)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, errs[0] = e.svc.MoveTracks(context.Background(), a, ra, ta, b) }()
		go func() { defer wg.Done(); _, errs[1] = e.svc.MoveTracks(context.Background(), b, rb, tb, a) }()
		wg.Wait()
		c0, c1 := catalog.Code(errs[0]), catalog.Code(errs[1])
		if !(c0 == "" && c1 == catalog.CodePreconditionFailed) && !(c1 == "" && c0 == catalog.CodePreconditionFailed) {
			t.Fatalf("round %d: %q %q", round, c0, c1)
		}
		na, nb := len(e.tracks(a)), len(e.tracks(b))
		if na*nb != 0 || na+nb != 4 {
			t.Fatalf("round %d: %d and %d tracks", round, na, nb)
		}
		if e.count(`SELECT count(*) FROM albums WHERE deleted_at IS NULL AND id IN ($1, $2)
			AND NOT EXISTS (SELECT 1 FROM tracks t WHERE t.album_id = albums.id)`, a, b) != 0 {
			t.Fatalf("round %d: an active album without tracks", round)
		}
	}
}
