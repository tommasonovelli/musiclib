package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"musiclib/internal/faulttest"
	"musiclib/internal/fsops"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

func backupRestoreChild(t *testing.T, mode string) {
	dbURL := os.Getenv("MAINTENANCE_DB")
	db, err := store.NewPool(context.Background(), dbURL, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v, err := volume.Acquire(os.Getenv("MAINTENANCE_VOLUME"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	archive := os.Getenv("MAINTENANCE_ARCHIVE")
	point := faulttest.Crash(os.Getenv("MAINTENANCE_POINT"))
	if mode == "backup" {
		if _, err := v.IdentifyExisting(t.Context(), db); err != nil {
			t.Fatal(err)
		}
		if err := v.OpenExistingLayout(); err != nil {
			t.Fatal(err)
		}
		err = Backup(t.Context(), db, v, archive, dbURL, point)
	} else {
		err = Restore(t.Context(), db, v, archive, dbURL, point)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal("child did not crash at failpoint")
}

func TestBackupRealCrashWindows(t *testing.T) {
	for _, point := range []string{"backup_temporary", "backup_dump", "backup_blob", "backup_copied", "backup_before_rename"} {
		t.Run(point, func(t *testing.T) {
			old, db, dir, _ := doctorFixture(t)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			final := filepath.Join(parent, "completed")
			if result := faulttest.RunChild(t, 40*time.Second, "backup", "MAINTENANCE_DB="+db.Config().ConnString(), "MAINTENANCE_VOLUME="+dir, "MAINTENANCE_ARCHIVE="+final, "MAINTENANCE_POINT="+point); result != faulttest.Killed {
				t.Fatalf("child: %q", result)
			}
			root, err := fsops.OpenRoot(parent)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := root.ReadDir("")
			if err != nil {
				t.Fatal(err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || !strings.HasSuffix(entries[0].Name, ".tmp") {
				t.Fatalf("interrupted backup published final: %+v", entries)
			}
		})
	}
}

func TestRestoreRealCrashWindows(t *testing.T) {
	for _, point := range []string{"restore_marker", "restore_dump", "restore_blob", "restore_originals", "restore_before_reset", "restore_after_reset"} {
		t.Run(point, func(t *testing.T) {
			old, db, _, _ := doctorFixture(t)
			archive := filepath.Join(t.TempDir(), "archive")
			if err := Backup(t.Context(), db, old, archive, db.Config().ConnConfig.ConnString(), nil); err != nil {
				t.Fatal(err)
			}
			url := pgtest.EmptyDB(t)
			dir := t.TempDir()
			if result := faulttest.RunChild(t, 45*time.Second, "restore", "MAINTENANCE_DB="+url, "MAINTENANCE_VOLUME="+dir, "MAINTENANCE_ARCHIVE="+archive, "MAINTENANCE_POINT="+point); result != faulttest.Killed {
				t.Fatalf("child: %q", result)
			}
			v, err := volume.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.CheckMaintenance(); volume.Code(err) != volume.CodeMaintenance {
				t.Fatalf("boot was not blocked: %v", err)
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			// Restore has no resume protocol: new destinations, never the partial
			// database or volume that the killed process left behind.
			replacementURL := pgtest.EmptyDB(t)
			replacement := pgtest.Pool(t, replacementURL)
			fresh, err := volume.Acquire(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := Restore(t.Context(), replacement, fresh, archive, replacementURL, nil); err != nil {
				t.Fatal(err)
			}
			if err := fresh.CheckMaintenance(); err != nil {
				t.Fatal(err)
			}
			for name, root := range map[string]*fsops.Root{"library": fresh.Library(), "work": fresh.Work()} {
				entries, err := root.ReadDir("")
				if err != nil || len(entries) != 0 {
					t.Fatalf("%s after repeat: %+v %v", name, entries, err)
				}
			}
			var pending, claims, journal, bad int
			for _, q := range []struct {
				statement string
				target    *int
			}{
				{`SELECT count(*) FROM jobs WHERE kind='render' AND state='pending'`, &pending},
				{`SELECT count(*) FROM path_claims`, &claims},
				{`SELECT count(*) FROM publication`, &journal},
				{`SELECT count(*) FROM albums WHERE published_path IS NOT NULL OR published_revision<>0`, &bad},
			} {
				if err := replacement.QueryRow(t.Context(), q.statement).Scan(q.target); err != nil {
					t.Fatal(err)
				}
			}
			if pending != 1 || claims != 1 || journal != 0 || bad != 0 {
				t.Fatalf("restore not converged: pending=%d claims=%d journal=%d bad=%d", pending, claims, journal, bad)
			}
			if err := fresh.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
