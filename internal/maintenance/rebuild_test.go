package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
	"musiclib/internal/store"
	"musiclib/internal/volume"
)

func TestRebuildKeepsCatalogAndOriginals(t *testing.T) {
	old, db, dir, _ := doctorFixture(t)
	var originalHash string
	if err := db.QueryRow(t.Context(), `SELECT hash FROM blobs`).Scan(&originalHash); err != nil {
		t.Fatal(err)
	}
	id := old.StoreID()
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	v, err := volume.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := Rebuild(t.Context(), db, v, store.NewID(), nil); err == nil {
		t.Fatal("wrong store id accepted")
	}
	if _, err := v.Root().Stat(volume.MaintenanceMarker); fsops.Code(err) != fsops.CodeNotFound {
		t.Fatalf("wrong id wrote marker: %v", err)
	}
	if err := Rebuild(t.Context(), db, v, id, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{volume.Library, volume.Work} {
		e, err := v.Root().ReadDir(name)
		if err != nil || len(e) != 0 {
			t.Fatalf("%s not empty: %+v, %v", name, e, err)
		}
	}
	for _, q := range []struct {
		sql  string
		want int
	}{
		{`SELECT count(*) FROM albums WHERE published_path IS NULL AND published_revision=0`, 1},
		{`SELECT count(*) FROM publication`, 0},
		{`SELECT count(*) FROM jobs WHERE kind='render' AND state='pending'`, 1},
		{`SELECT count(*) FROM path_claims`, 1},
		{`SELECT count(*) FROM tracks`, 1},
		{`SELECT count(*) FROM blobs WHERE hash=$1`, 1},
	} {
		var n int
		args := []any{}
		if q.sql == `SELECT count(*) FROM blobs WHERE hash=$1` {
			args = append(args, originalHash)
		}
		if err := db.QueryRow(t.Context(), q.sql, args...).Scan(&n); err != nil || n != q.want {
			t.Fatalf("%s: count %d want %d: %v", q.sql, n, q.want, err)
		}
	}
	if _, err := v.Root().Stat(volume.MaintenanceMarker); fsops.Code(err) != fsops.CodeNotFound {
		t.Fatalf("marker not removed: %v", err)
	}
}

// A failure after a deletion leaves the marker in place. A second invocation
// replays all steps, even when some directories were already removed.
func TestRebuildLeavesTrashedAlbumsUnpublished(t *testing.T) {
	old, db, dir, _ := doctorFixture(t)
	id := old.StoreID()
	if _, err := db.Exec(t.Context(), `UPDATE albums SET deleted_at=now()`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	v, err := volume.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := Rebuild(t.Context(), db, v, id, nil); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`SELECT count(*) FROM path_claims`, `SELECT count(*) FROM jobs WHERE kind='render'`, `SELECT count(*) FROM albums WHERE deleted_at IS NOT NULL AND published_path IS NULL`} {
		want := 0
		if query == `SELECT count(*) FROM albums WHERE deleted_at IS NOT NULL AND published_path IS NULL` {
			want = 1
		}
		var got int
		if err := db.QueryRow(t.Context(), query).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: %d want %d, %v", query, got, want, err)
		}
	}
}

func TestRebuildDoesNotFollowLibrarySymlinks(t *testing.T) {
	old, db, dir, _ := doctorFixture(t)
	id := old.StoreID()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "library", "Artist", "Album", "outside")); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	v, err := volume.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := Rebuild(t.Context(), db, v, id, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(outside, "keep"))
	if err != nil || string(got) != "user data" {
		t.Fatalf("outside content changed: %q, %v", got, err)
	}
}

func TestRebuildRetryAfterPartialDeletion(t *testing.T) {
	old, db, dir, _ := doctorFixture(t)
	id := old.StoreID()
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	v, err := volume.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	hook := failpoint.Hook(func(p failpoint.Point) error {
		if p.Name == "rebuild_deleted_library" {
			return context.Canceled
		}
		return nil
	})
	if err := Rebuild(t.Context(), db, v, id, hook); err == nil {
		t.Fatal("interrupted rebuild succeeded")
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	v, err = volume.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := v.CheckMaintenance(); volume.Code(err) != volume.CodeMaintenance {
		t.Fatalf("boot did not refuse: %v", err)
	}
	if _, err := v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := Rebuild(t.Context(), db, v, id, nil); err != nil {
		t.Fatal(err)
	}
}
