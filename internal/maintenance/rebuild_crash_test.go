package maintenance

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/faulttest"
	"musiclib/internal/store"
	"musiclib/internal/volume"
)

func TestHelperProcess(t *testing.T) {
	mode := faulttest.Mode()
	if mode != "rebuild" && mode != "backup" && mode != "restore" {
		return
	}
	if mode == "backup" || mode == "restore" {
		backupRestoreChild(t, mode)
		return
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx, os.Getenv("REBUILD_DB"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	v, err := volume.Acquire(os.Getenv("REBUILD_VOLUME"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := v.IdentifyExisting(ctx, pool); err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(os.Getenv("REBUILD_ID"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Rebuild(ctx, pool, v, id, faulttest.Crash(os.Getenv("REBUILD_POINT"))); err != nil {
		t.Fatal(err)
	}
	t.Fatal("rebuild did not hit its crash point")
}

func TestRebuildRealCrashWindows(t *testing.T) {
	for _, point := range []string{"rebuild_marker", "rebuild_deleted_library", "rebuild_deleted_work", "rebuild_before_transaction", "rebuild_after_transaction", "rebuild_before_marker_removal"} {
		t.Run(point, func(t *testing.T) {
			old, db, dir, _ := doctorFixture(t)
			id := old.StoreID()
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			if result := faulttest.RunChild(t, 30*time.Second, "rebuild", "REBUILD_DB="+db.Config().ConnString(), "REBUILD_VOLUME="+dir, "REBUILD_ID="+id.String(), "REBUILD_POINT="+point); result != faulttest.Killed {
				t.Fatalf("unexpected child result %q", result)
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
			if err := v.CheckMaintenance(); volume.Code(err) != volume.CodeMaintenance {
				t.Fatalf("server boot not blocked after crash: %v", err)
			}
			if _, err := v.IdentifyExisting(t.Context(), db); err != nil {
				t.Fatal(err)
			}
			if err := Rebuild(t.Context(), db, v, id, nil); err != nil {
				t.Fatalf("rebuild repeat: %v", err)
			}
			if err := v.CheckMaintenance(); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []struct {
				name    string
				entries func() (int, error)
			}{
				{"library", func() (int, error) { e, err := v.Library().ReadDir(""); return len(e), err }},
				{"work", func() (int, error) { e, err := v.Work().ReadDir(""); return len(e), err }},
			} {
				count, err := dir.entries()
				if err != nil || count != 0 {
					t.Fatalf("%s not empty after retry: %d %v", dir.name, count, err)
				}
			}
			var pending, claims, publication, badState int
			if err := db.QueryRow(t.Context(), `SELECT count(*) FROM jobs WHERE kind='render' AND state='pending'`).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(t.Context(), `SELECT count(*) FROM path_claims WHERE path='Artist/Album' AND album_id=(SELECT id FROM albums)`).Scan(&claims); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(t.Context(), `SELECT count(*) FROM publication`).Scan(&publication); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(t.Context(), `SELECT count(*) FROM albums WHERE published_path IS NOT NULL OR published_revision<>0`).Scan(&badState); err != nil {
				t.Fatal(err)
			}
			if pending != 1 || claims != 1 || publication != 0 || badState != 0 {
				t.Fatalf("nonconverged after retry: pending=%d claims=%d journal=%d bad=%d", pending, claims, publication, badState)
			}
		})
	}
}
