package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/store"
)

// Two workers on one job (§9.5, §12.2): among many concurrent claimers
// exactly one gets it, round after round.
func TestOneClaimPerJob(t *testing.T) {
	f := newFixture(t)
	const rounds, claimers = 25, 8
	for round := range rounds {
		album := f.album("round")
		e := f.enqueue(album)
		var (
			wg     sync.WaitGroup
			mu     sync.Mutex
			claims []*Claim
		)
		for range claimers {
			wg.Go(func() {
				c, err := claimTolerant(f)
				if err != nil {
					t.Errorf("round %d: ClaimNext: %v", round, err)
					return
				}
				if c != nil {
					mu.Lock()
					claims = append(claims, c)
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		if len(claims) != 1 {
			t.Fatalf("round %d: %d claims of one job, want exactly 1", round, len(claims))
		}
		if claims[0].Attempt != (Attempt{JobID: e.JobID, Ticket: e.Requested}) {
			t.Fatalf("round %d: claimed %v, want %s ticket %d", round, claims[0].Attempt, e.JobID, e.Requested)
		}
		f.inTx(func(tx *store.CatalogTx) error {
			_, err := FinishRender(context.Background(), tx, claims[0].Attempt)
			return err
		})
	}
}

// claimTolerant claims like an unserialized claimer: a claim that lost
// every race (store_retries_exhausted, N-110) claimed nothing and changed
// nothing, so it simply tries again.
func claimTolerant(f *fixture) (*Claim, error) {
	for {
		c, err := ClaimNext(context.Background(), f.db, testRenderer)
		if store.Code(err) == store.CodeRetriesExhausted {
			continue
		}
		return c, err
	}
}

// Many jobs and many concurrent claimers: every job is claimed exactly once.
func TestEveryJobClaimedOnce(t *testing.T) {
	f := newFixture(t)
	const n, claimers = 60, 8
	want := map[uuid.UUID]bool{}
	for range n / 2 {
		want[f.enqueue(f.album("a")).JobID] = true
	}
	batch := f.batch()
	for i := range n / 2 {
		want[f.importJob(batch, fmt.Sprintf("dir%02d", i), time.Now())] = true
	}
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		got = map[uuid.UUID]int{}
	)
	for range claimers {
		wg.Go(func() {
			for {
				c, err := claimTolerant(f)
				if err != nil {
					t.Errorf("ClaimNext: %v", err)
					return
				}
				if c == nil {
					return
				}
				mu.Lock()
				got[c.Attempt.JobID]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(got) != n {
		t.Errorf("%d jobs claimed, want %d", len(got), n)
	}
	for id, k := range got {
		if !want[id] || k != 1 {
			t.Errorf("job %s claimed %d times (known %v)", id, k, want[id])
		}
	}
}

// §6.1: renders first, then the scan, then the imports; within a kind by
// queued_at, then id.
func TestClaimPriority(t *testing.T) {
	f := newFixture(t)
	base := time.Now().Add(-time.Hour)
	batch := f.batch()
	impA := f.importJob(batch, "a", base)
	impB := f.importJob(batch, "b", base) // same queued_at: the id orders
	scan := f.scanJob(f.batch(), base.Add(time.Minute))
	r1 := f.enqueue(f.album("r1")).JobID
	r2 := f.enqueue(f.album("r2")).JobID
	first, second := impA, impB
	if second.String() < first.String() {
		first, second = second, first
	}
	want := []uuid.UUID{r1, r2, scan, first, second}
	for i, w := range want {
		c := f.claim()
		if c == nil || c.Attempt.JobID != w {
			t.Fatalf("claim %d = %v, want job %s", i, c, w)
		}
	}
	if c := f.claim(); c != nil {
		t.Fatalf("claimed %v with nothing pending", c.Attempt)
	}
}

// §6.3: the upsert on the album's one row. A running job stays running and
// keeps its claim while requested moves on; completions follow the ticket.
func TestCoalescing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	album := f.album("coalesce")

	e1 := f.enqueue(album)
	if e1.State != StatePending || e1.Claimed != 0 {
		t.Fatalf("first enqueue = %+v, want pending without claim", e1)
	}
	if e2 := f.enqueue(album); e2.JobID != e1.JobID || e2.Requested <= e1.Requested || e2.State != StatePending {
		t.Fatalf("second enqueue = %+v, want the same pending row with a newer ticket than %d", e2, e1.Requested)
	}

	c1 := f.claim()
	j := f.job(e1.JobID)
	if j.State != "running" || *j.Claimed != j.Requested || c1.Attempt.Ticket != j.Requested {
		t.Fatalf("after the claim: state %s claimed %v requested %d, attempt %v", j.State, j.Claimed, j.Requested, c1.Attempt)
	}

	e3 := f.enqueue(album)
	j = f.job(e1.JobID)
	if e3.State != StateRunning || e3.Claimed != c1.Attempt.Ticket || e3.Requested <= c1.Attempt.Ticket ||
		j.State != "running" || *j.Claimed != c1.Attempt.Ticket {
		t.Fatalf("enqueue while running = %+v (row %s, claimed %v), want running, claim %d kept, newer request",
			e3, j.State, j.Claimed, c1.Attempt.Ticket)
	}
	if got := f.claim(); got != nil {
		t.Fatalf("a running job was claimed again: %v", got.Attempt)
	}

	// The old attempt completes: a newer request exists, so the row goes
	// back to pending without claim.
	var out RenderOutcome
	f.inTx(func(tx *store.CatalogTx) error {
		var err error
		out, err = FinishRender(ctx, tx, c1.Attempt)
		return err
	})
	j = f.job(e1.JobID)
	if out != RenderRequeued || j.State != "pending" || j.Claimed != nil || j.Requested != e3.Requested {
		t.Fatalf("finish of the old attempt: %s, row %s claimed %v requested %d", out, j.State, j.Claimed, j.Requested)
	}
	// The old attempt can no longer complete anything.
	for name, fn := range staleCompletions(c1.Attempt) {
		var err error
		f.inTx(func(tx *store.CatalogTx) error { err = fn(ctx, tx); return nil })
		if Code(err) != CodeAttemptStale {
			t.Errorf("%s of the old attempt: %v, want %s", name, err, CodeAttemptStale)
		}
	}
	if j2 := f.job(e1.JobID); !reflect.DeepEqual(j2, j) {
		t.Errorf("a stale completion changed the row: %+v -> %+v", j, j2)
	}

	c2 := f.claim()
	if c2.Attempt.Ticket != e3.Requested {
		t.Fatalf("second claim ticket %d, want %d", c2.Attempt.Ticket, e3.Requested)
	}
	f.inTx(func(tx *store.CatalogTx) error {
		var err error
		out, err = FinishRender(ctx, tx, c2.Attempt)
		return err
	})
	if out != RenderDeleted || f.jobExists(e1.JobID) {
		t.Fatalf("finish of the current attempt: %s, row exists %v; want deleted", out, f.jobExists(e1.JobID))
	}
}

// staleCompletions are every render completion, for an attempt that must
// be refused.
func staleCompletions(a Attempt) map[string]func(context.Context, *store.CatalogTx) error {
	return map[string]func(context.Context, *store.CatalogTx) error{
		"FinishRender": func(ctx context.Context, tx *store.CatalogTx) error {
			_, err := FinishRender(ctx, tx, a)
			return err
		},
		"RequeueRender": func(ctx context.Context, tx *store.CatalogTx) error { return RequeueRender(ctx, tx, a) },
		"FailRender": func(ctx context.Context, tx *store.CatalogTx) error {
			_, err := FailRender(ctx, tx, a, "render_failed", "boom")
			return err
		},
	}
}

// §6.4: the render completions, each with the current ticket and with a
// newer request, plus attempts that are not the running one.
func TestRenderCompletions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	type result struct {
		out   RenderOutcome
		state string // "" = deleted
		code  string
	}
	for _, tc := range []struct {
		name       string
		newer      bool
		complete   func(tx *store.CatalogTx, a Attempt) (RenderOutcome, error)
		want       result
		wantErrMsg string
	}{
		{"finish current", false, func(tx *store.CatalogTx, a Attempt) (RenderOutcome, error) { return FinishRender(ctx, tx, a) },
			result{RenderDeleted, "", ""}, ""},
		{"finish newer", true, func(tx *store.CatalogTx, a Attempt) (RenderOutcome, error) { return FinishRender(ctx, tx, a) },
			result{RenderRequeued, "pending", ""}, ""},
		{"fail current", false, func(tx *store.CatalogTx, a Attempt) (RenderOutcome, error) {
			return FailRender(ctx, tx, a, "media_tags_opaque_field", "an opaque field")
		}, result{RenderFailed, "failed", "media_tags_opaque_field"}, "an opaque field"},
		{"fail newer", true, func(tx *store.CatalogTx, a Attempt) (RenderOutcome, error) {
			return FailRender(ctx, tx, a, "media_tags_opaque_field", "an opaque field")
		}, result{RenderRequeued, "pending", ""}, ""},
		{"requeue", false, func(tx *store.CatalogTx, a Attempt) (RenderOutcome, error) {
			return RenderRequeued, RequeueRender(ctx, tx, a)
		}, result{RenderRequeued, "pending", ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			album := f.album(tc.name)
			e := f.enqueue(album)
			c := f.claim()
			if c.Attempt.JobID != e.JobID {
				t.Fatalf("claimed %v, want %s", c.Attempt, e.JobID)
			}
			// Another attempt of the same row: wrong ticket.
			wrong := Attempt{JobID: c.Attempt.JobID, Ticket: c.Attempt.Ticket - 1}
			for name, fn := range staleCompletions(wrong) {
				var err error
				f.inTx(func(tx *store.CatalogTx) error { err = fn(ctx, tx); return nil })
				if Code(err) != CodeAttemptStale {
					t.Errorf("%s with a wrong ticket: %v, want %s", name, err, CodeAttemptStale)
				}
			}
			if tc.newer {
				f.enqueue(album)
			}
			before := f.job(e.JobID)
			var out RenderOutcome
			f.inTx(func(tx *store.CatalogTx) error {
				var err error
				out, err = tc.complete(tx, c.Attempt)
				return err
			})
			if out != tc.want.out {
				t.Errorf("outcome %s, want %s", out, tc.want.out)
			}
			if tc.want.state == "" {
				if f.jobExists(e.JobID) {
					t.Error("the job still exists")
				}
				return
			}
			j := f.job(e.JobID)
			if j.State != tc.want.state || j.Claimed != nil || j.Requested != before.Requested ||
				deref(j.ErrorCode) != tc.want.code || deref(j.ErrorMessage) != tc.wantErrMsg {
				t.Errorf("row %s claimed %v requested %d error %q %q; want %s, no claim, requested %d, error %q %q",
					j.State, j.Claimed, j.Requested, deref(j.ErrorCode), deref(j.ErrorMessage),
					tc.want.state, before.Requested, tc.want.code, tc.wantErrMsg)
			}
			// Once out of running, the attempt is stale.
			for name, fn := range staleCompletions(c.Attempt) {
				var err error
				f.inTx(func(tx *store.CatalogTx) error { err = fn(ctx, tx); return nil })
				if Code(err) != CodeAttemptStale {
					t.Errorf("%s after completion: %v, want %s", name, err, CodeAttemptStale)
				}
			}
			if tc.want.state == "failed" {
				// A later change of the album reactivates it (§6.4).
				e2 := f.enqueue(album)
				j := f.job(e.JobID)
				if e2.State != StatePending || j.ErrorCode != nil || j.ErrorMessage != nil {
					t.Errorf("enqueue of a failed render: %+v, error %v %v; want pending without error", e2, j.ErrorCode, j.ErrorMessage)
				}
			}
			f.exec(`DELETE FROM jobs WHERE id = $1`, e.JobID)
		})
	}

	t.Run("invalid failure", func(t *testing.T) {
		var err error
		f.inTx(func(tx *store.CatalogTx) error {
			_, err = FailRender(ctx, tx, Attempt{JobID: store.NewID(), Ticket: 1}, "Not A Code", "x")
			return nil
		})
		if Code(err) != CodeInvalidResult {
			t.Errorf("err = %v, want %s", err, CodeInvalidResult)
		}
	})
	t.Run("message clipped", func(t *testing.T) {
		album := f.album("clip")
		f.enqueue(album)
		c := f.claim()
		long := strings.Repeat("é", 3000) + "\xff"
		f.inTx(func(tx *store.CatalogTx) error {
			_, err := FailRender(ctx, tx, c.Attempt, "render_failed", long)
			return err
		})
		msg := deref(f.job(c.Attempt.JobID).ErrorMessage)
		if len(msg) > maxErrorMessage || !strings.HasPrefix(long, msg) || len(msg) < maxErrorMessage-1 {
			t.Errorf("stored message of %d bytes, want a prefix of at most %d", len(msg), maxErrorMessage)
		}
	})
}

// §6.4: the outcomes of scan and import attempts, with their validation.
func TestFinish(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	album := f.album("result")
	for _, tc := range []struct {
		name string
		kind Kind
		r    Result
		ok   bool
	}{
		{"import done", KindImport, Result{State: StateDone, AlbumID: album,
			Warnings: []Warning{{Code: WarnYearDiscordant, Message: "1999 and 2001"}}}, true},
		{"import skipped", KindImport, Result{State: StateSkipped, AlbumID: album, ErrorCode: "duplicate_import", ErrorMessage: "same"}, true},
		{"import failed", KindImport, Result{State: StateFailed, ErrorCode: "mixed_album", ErrorMessage: "two albums"}, true},
		{"scan done", KindScan, Result{State: StateDone,
			Warnings: []Warning{{Code: WarnUnassignedFile, Message: "outside every candidate", Path: "loose/file.txt"}}}, true},
		{"scan failed", KindScan, Result{State: StateFailed, ErrorCode: "import_unavailable", ErrorMessage: "gone"}, true},
		{"done with error", KindImport, Result{State: StateDone, AlbumID: album, ErrorCode: "x", ErrorMessage: "y"}, false},
		{"done import without album", KindImport, Result{State: StateDone}, false},
		{"skipped without code", KindImport, Result{State: StateSkipped, AlbumID: album, ErrorMessage: "y"}, false},
		{"failed without message", KindImport, Result{State: StateFailed, ErrorCode: "x"}, false},
		{"failed with bad code", KindImport, Result{State: StateFailed, ErrorCode: "Bad Code", ErrorMessage: "y"}, false},
		{"failed import with album", KindImport, Result{State: StateFailed, AlbumID: album, ErrorCode: "x", ErrorMessage: "y"}, false},
		{"scan with album", KindScan, Result{State: StateDone, AlbumID: album}, false},
		{"pending", KindImport, Result{State: StatePending}, false},
		{"running", KindScan, Result{State: StateRunning}, false},
		{"render", KindRender, Result{State: StateFailed, ErrorCode: "x", ErrorMessage: "y"}, false},
		{"unknown warning", KindScan, Result{State: StateDone, Warnings: []Warning{{Code: "whatever", Message: "m"}}}, false},
		{"warning with absolute path", KindScan, Result{State: StateDone, Warnings: []Warning{{Code: WarnUnassignedFile, Message: "m", Path: "/etc"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch := f.batch()
			var id uuid.UUID
			if tc.kind == KindScan {
				id = f.scanJob(batch, time.Now())
			} else {
				id = f.importJob(batch, "src", time.Now())
			}
			f.exec(`UPDATE jobs SET state = 'running', claimed = requested WHERE id = $1`, id)
			j := f.job(id)
			a := Attempt{JobID: id, Ticket: *j.Claimed}
			kind := tc.kind // KindRender: the row is an import, the call is refused
			var err error
			// Another ticket of the running job is refused and changes nothing.
			f.inTx(func(tx *store.CatalogTx) error {
				err = Finish(ctx, tx, kind, Attempt{JobID: id, Ticket: a.Ticket + 1}, tc.r)
				return nil
			})
			if want := map[bool]string{true: CodeAttemptStale, false: CodeInvalidResult}[tc.ok]; Code(err) != want {
				t.Fatalf("Finish with another ticket: %v, want %s", err, want)
			}
			if !reflect.DeepEqual(f.job(id), j) {
				t.Fatal("Finish with another ticket changed the row")
			}
			f.inTx(func(tx *store.CatalogTx) error { err = Finish(ctx, tx, kind, a, tc.r); return nil })
			after := f.job(id)
			if !tc.ok {
				if Code(err) != CodeInvalidResult || !reflect.DeepEqual(after, j) {
					t.Fatalf("err = %v, row changed %v; want %s and no change", err, !reflect.DeepEqual(after, j), CodeInvalidResult)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var ws []Warning
			if err := json.Unmarshal(after.Warnings, &ws); err != nil {
				t.Fatal(err)
			}
			if after.State != string(tc.r.State) || after.Claimed != nil || deref(after.ResultAlbumID) != tc.r.AlbumID ||
				deref(after.ErrorCode) != tc.r.ErrorCode || deref(after.ErrorMessage) != tc.r.ErrorMessage ||
				len(ws) != len(tc.r.Warnings) || (len(ws) > 0 && ws[0] != tc.r.Warnings[0]) {
				t.Errorf("row %+v does not record %+v", after, tc.r)
			}
			// A second completion of the same attempt is stale.
			f.inTx(func(tx *store.CatalogTx) error { err = Finish(ctx, tx, kind, a, tc.r); return nil })
			if Code(err) != CodeAttemptStale {
				t.Errorf("second Finish: %v, want %s", err, CodeAttemptStale)
			}
		})
	}
}

// LockStatus reads the row the completions and the import commit decide on.
func TestLockStatus(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.enqueue(f.album("status"))
	c := f.claim()
	var st Status
	f.inTx(func(tx *store.CatalogTx) error {
		var err error
		st, err = LockStatus(ctx, tx, e.JobID)
		return err
	})
	if !st.Runs(c.Attempt) || st.Kind != KindRender || st.Requested != c.Attempt.Ticket || st.AlbumID == uuid.Nil {
		t.Errorf("status %+v does not describe %v", st, c.Attempt)
	}
	if st.Runs(Attempt{JobID: e.JobID, Ticket: c.Attempt.Ticket + 1}) {
		t.Error("Runs accepted another ticket")
	}
	var err error
	f.inTx(func(tx *store.CatalogTx) error { _, err = LockStatus(ctx, tx, store.NewID()); return nil })
	if Code(err) != CodeNotFound {
		t.Errorf("unknown job: %v, want %s", err, CodeNotFound)
	}
}

// §6.2: the snapshot is the album of one committed state. A change
// committed between the album read and the tracks read is invisible.
func TestSnapshotCoherence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	artist, album := store.NewID(), store.NewID()
	cover, audio1, audio2, lrc, pdf := f.blob("jpeg"), f.blob("flac"), f.blob("flac"), f.blob(""), f.blob("")
	build := store.NewID()
	receipt := newHash()
	f.exec(`INSERT INTO artists (id, name, folder_key, revision) VALUES ($1, 'Miles Davis', 'miles davis', 3)`, artist)
	f.exec(`INSERT INTO albums (id, artist_id, title, folder_key, year, genre, compilation, cover_hash, revision,
			published_path, published_revision, published_renderer, published_build, published_receipt_hash)
		VALUES ($1, $2, 'Kind of Blue', 'kind of blue', 1959, 'Jazz', true, $3, 4,
			'Miles Davis/Kind of Blue', 2, 'rv-old', $4, $5)`, album, artist, cover, build, receipt)
	t1, t2 := store.NewID(), store.NewID()
	f.exec(`INSERT INTO tracks (id, album_id, disc, no, title, artist, genre, blob_hash, source_path, lyrics_hash)
		VALUES ($1, $3, 1, 2, 'Freddie Freeloader', NULL, '', $4, 'b.flac', NULL),
		       ($2, $3, 1, 1, 'So What', 'Miles & Coltrane', NULL, $5, 'a.flac', $6)`, t2, t1, album, audio2, audio1, lrc)
	a1, a2 := store.NewID(), store.NewID()
	f.exec(`INSERT INTO attachments (id, album_id, rel_path, path_key, blob_hash) VALUES
		($1, $3, 'Scans/front.pdf', 'scans/front.pdf', $4), ($2, $3, 'booklet.pdf', 'booklet.pdf', $4)`, a1, a2, album, pdf)
	e := f.enqueue(album)

	var once sync.Once
	f.setHook(func(point string) {
		if point != "claim_snapshot_album" {
			return
		}
		once.Do(func() {
			// A catalog change committed after the snapshot was taken.
			f.exec(`UPDATE tracks SET title = 'changed', disc = 2 WHERE album_id = $1`, album)
			f.exec(`UPDATE attachments SET rel_path = 'changed.pdf' WHERE album_id = $1`, album)
			f.exec(`UPDATE albums SET title = 'changed' WHERE id = $1`, album)
		})
	})
	c, err := claimNext(ctx, f.db, testRenderer, f.fp.Hook())
	if err != nil {
		t.Fatal(err)
	}
	want := &RenderSnapshot{
		Attempt:       Attempt{JobID: e.JobID, Ticket: e.Requested},
		RenderVersion: testRenderer,
		Artist:        SnapshotArtist{ID: artist, Name: "Miles Davis", Revision: 3},
		Album: SnapshotAlbum{ID: album, Title: "Kind of Blue", Year: 1959, Genre: Text{"Jazz", true}, Compilation: true,
			Revision: 4, PublishedPath: "Miles Davis/Kind of Blue", PublishedRevision: 2, PublishedRenderer: "rv-old",
			PublishedBuild: build, PublishedReceiptHash: receipt},
		Cover: SnapshotBlob{Hash: cover, Size: 1000, Format: "jpeg"},
		Tracks: []SnapshotTrack{
			{ID: t1, Disc: 1, No: 1, Title: "So What", Artist: Text{"Miles & Coltrane", true}, SourcePath: "a.flac",
				Blob: SnapshotBlob{audio1, 1000, "flac"}, Lyrics: SnapshotBlob{Hash: lrc, Size: 1000}},
			{ID: t2, Disc: 1, No: 2, Title: "Freddie Freeloader", Genre: Text{"", true}, SourcePath: "b.flac",
				Blob: SnapshotBlob{audio2, 1000, "flac"}},
		},
		Attachments: []SnapshotAttachment{
			{ID: a2, RelPath: "booklet.pdf", PathKey: "booklet.pdf", Blob: SnapshotBlob{pdf, 1000, ""}},
			{ID: a1, RelPath: "Scans/front.pdf", PathKey: "scans/front.pdf", Blob: SnapshotBlob{pdf, 1000, ""}},
		},
	}
	if c == nil || c.Kind != KindRender || !reflect.DeepEqual(c.Render, want) {
		t.Fatalf("snapshot\n got %+v\nwant %+v", c.Render, want)
	}
	if c.Render.String() == "" {
		t.Error("empty String")
	}
}

// A serialization failure inside the claim reruns the claim transaction
// only (§6.2): a pending import touched by a commit after the snapshot
// makes FOR UPDATE fail with 40001, and the rerun claims it.
func TestClaimRetriesSerializationFailure(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.importJob(f.batch(), "album", time.Now())
	f.exec(`UPDATE jobs SET overrides = '{"artist": "Someone"}' WHERE id = $1`, id)
	var runs atomic.Int32
	f.setHook(func(point string) {
		if point == "claim_selected_render" && runs.Add(1) == 1 {
			f.exec(`UPDATE jobs SET updated_at = now() WHERE id = $1`, id)
		}
	})
	c, err := claimNext(ctx, f.db, testRenderer, f.fp.Hook())
	if err != nil {
		t.Fatal(err)
	}
	if n := runs.Load(); n != 2 {
		t.Errorf("the claim transaction ran %d times, want 2", n)
	}
	j := f.job(id)
	if c == nil || c.Attempt.JobID != id || c.Kind != KindImport || j.State != "running" || *j.Claimed != c.Attempt.Ticket ||
		c.SourceRel != "album" || c.Overrides.Artist == nil || *c.Overrides.Artist != "Someone" || c.Overrides.Title != nil {
		t.Fatalf("claim %+v, row %s claimed %v", c, j.State, j.Claimed)
	}
}

// §11.1 step 5: every running job becomes pending without its claim; the
// others are untouched.
func TestRecoverRunning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.enqueue(f.album("running render"))
	batch := f.batch()
	imp := f.importJob(batch, "running import", time.Now())
	running := []uuid.UUID{f.claim().Attempt.JobID, f.claim().Attempt.JobID}
	if running[1] != imp {
		t.Fatalf("claimed %s, want the import %s", running[1], imp)
	}
	pending := f.enqueue(f.album("pending")).JobID
	done := f.importJob(batch, "done", time.Now())
	f.exec(`UPDATE jobs SET state = 'done' WHERE id = $1`, done)
	beforePending, beforeDone := f.job(pending), f.job(done)

	n, err := RecoverRunning(ctx, f.db)
	if err != nil || n != 2 {
		t.Fatalf("RecoverRunning = %d, %v; want 2", n, err)
	}
	for _, id := range running {
		if j := f.job(id); j.State != "pending" || j.Claimed != nil {
			t.Errorf("job %s: %s claimed %v, want pending without claim", id, j.State, j.Claimed)
		}
	}
	if j := f.job(pending); !reflect.DeepEqual(j, beforePending) {
		t.Errorf("pending job changed: %+v", j)
	}
	if j := f.job(done); !reflect.DeepEqual(j, beforeDone) {
		t.Errorf("done job changed: %+v", j)
	}
	if n, err := RecoverRunning(ctx, f.db); err != nil || n != 0 {
		t.Errorf("second RecoverRunning = %d, %v; want 0", n, err)
	}
}

// §11.1 step 6: active albums with another (or no) published renderer and
// no render job at all are enqueued; everything else is left alone.
func TestEnqueueStaleRenders(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	publish := func(album uuid.UUID, renderer string) {
		f.exec(`UPDATE albums SET published_path = 'a/' || id::text, published_revision = 1, published_renderer = $2,
			published_build = $3, published_receipt_hash = $4 WHERE id = $1`, album, renderer, store.NewID(), newHash())
	}
	stale := f.album("stale renderer")
	publish(stale, "rv-old")
	never := f.album("never published")
	current := f.album("current renderer")
	publish(current, testRenderer)
	failed := f.album("stale but failed")
	publish(failed, "rv-old")
	fe := f.enqueue(failed)
	f.claim()
	f.inTx(func(tx *store.CatalogTx) error {
		_, err := FailRender(ctx, tx, Attempt{JobID: fe.JobID, Ticket: fe.Requested}, "render_failed", "no")
		return err
	})
	queued := f.album("stale but queued")
	qe := f.enqueue(queued)
	trashed := f.album("trashed")
	publish(trashed, "rv-old")
	f.exec(`UPDATE albums SET deleted_at = now() WHERE id = $1`, trashed)
	beforeFailed, beforeQueued := f.job(fe.JobID), f.job(qe.JobID)

	if _, err := EnqueueStaleRenders(ctx, f.db, ""); Code(err) != CodeInvalidArgument {
		t.Fatalf("empty renderer: %v, want %s", err, CodeInvalidArgument)
	}
	n, err := EnqueueStaleRenders(ctx, f.db, testRenderer)
	if err != nil || n != 2 {
		t.Fatalf("EnqueueStaleRenders = %d, %v; want 2", n, err)
	}
	var got []uuid.UUID
	rows, err := f.db.Query(ctx, `SELECT album_id FROM jobs WHERE kind = 'render' AND state = 'pending' AND album_id <> $1 ORDER BY album_id`, queued)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	want := []uuid.UUID{stale, never}
	if want[1].String() < want[0].String() {
		want[0], want[1] = want[1], want[0]
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pending renders %v, want %v", got, want)
	}
	if j := f.job(fe.JobID); !reflect.DeepEqual(j, beforeFailed) {
		t.Errorf("the failed render changed: %+v", j)
	}
	if j := f.job(qe.JobID); !reflect.DeepEqual(j, beforeQueued) {
		t.Errorf("the queued render changed: %+v", j)
	}
	if n, err := EnqueueStaleRenders(ctx, f.db, testRenderer); err != nil || n != 0 {
		t.Errorf("second run = %d, %v; want 0", n, err)
	}
}
