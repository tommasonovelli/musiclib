package catalog_test

import (
	"context"
	"testing"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// The four conditions PREPARE rechecks under the catalog lock (§6.3,
// §9.3 A), each on its own.
func TestCheckFresh(t *testing.T) {
	const renderer = "rv-fresh"
	for _, tc := range []struct {
		name    string
		spoil   func(e *env, snap *jobs.RenderSnapshot)
		current string
		want    catalog.Staleness
	}{
		{"fresh", func(*env, *jobs.RenderSnapshot) {}, renderer, catalog.Fresh},
		{"newer request", func(e *env, s *jobs.RenderSnapshot) {
			if err := store.InCatalogTx(context.Background(), e.db, func(tx *store.CatalogTx) error {
				_, err := jobs.EnqueueRender(context.Background(), tx, s.Album.ID)
				return err
			}); err != nil {
				e.t.Fatal(err)
			}
		}, renderer, catalog.StaleTicket},
		{"attempt gone", func(e *env, s *jobs.RenderSnapshot) {
			e.exec(`UPDATE jobs SET state = 'pending', claimed = NULL WHERE id = $1`, s.Attempt.JobID)
		}, renderer, catalog.StaleTicket},
		{"job deleted", func(e *env, s *jobs.RenderSnapshot) {
			e.exec(`DELETE FROM jobs WHERE id = $1`, s.Attempt.JobID)
		}, renderer, catalog.StaleTicket},
		{"revision", func(e *env, s *jobs.RenderSnapshot) {
			e.exec(`UPDATE albums SET revision = revision + 1 WHERE id = $1`, s.Album.ID)
		}, renderer, catalog.StaleRevision},
		{"renderer", func(*env, *jobs.RenderSnapshot) {}, "rv-next", catalog.StaleRenderer},
		{"claims", func(e *env, s *jobs.RenderSnapshot) {
			e.exec(`DELETE FROM path_claims WHERE album_id = $1`, s.Album.ID)
		}, renderer, catalog.StaleClaims},
		{"claim of another album", func(e *env, s *jobs.RenderSnapshot) {
			other := e.importAlbum("Other", "Album")
			e.exec(`UPDATE path_claims SET album_id = $2 WHERE album_id = $1`, s.Album.ID, other)
		}, renderer, catalog.StaleClaims},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			album := e.importAlbum("Fresh", "Album")
			c, err := jobs.ClaimNext(context.Background(), e.db, renderer)
			if err != nil || c == nil || c.Render == nil || c.Render.Album.ID != album {
				t.Fatalf("claim %+v, %v", c, err)
			}
			tc.spoil(e, c.Render)
			var got catalog.Staleness
			if err := store.InCatalogTx(context.Background(), e.db, func(tx *store.CatalogTx) error {
				var err error
				got, err = catalog.CheckFresh(context.Background(), tx, c.Render, tc.current)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("CheckFresh = %q, want %q", got, tc.want)
			}
		})
	}
}

// A fatal store error (§6.4) wraps whatever the transaction's function
// returned; its code wins, so no caller reads it as a domain refusal.
func TestCodeFatalWins(t *testing.T) {
	domain := &catalog.Error{Code: catalog.CodePathReserved, Message: "reserved"}
	for _, code := range []string{store.CodeConnectionLost, store.CodeCommitUncertain} {
		err := &store.Error{Code: code, Msg: "lost", Err: domain}
		if got := catalog.Code(err); got != code {
			t.Errorf("catalog.Code = %s, want %s", got, code)
		}
		if got := jobs.Code(err); got != code {
			t.Errorf("jobs.Code = %s, want %s", got, code)
		}
	}
	if got := catalog.Code(&store.Error{Code: store.CodeCanceled, Err: domain}); got != catalog.CodePathReserved {
		t.Errorf("a non-fatal store error hides the domain code: %s", got)
	}
}
