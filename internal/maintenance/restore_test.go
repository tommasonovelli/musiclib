package maintenance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/blobstore"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

func TestBackupLossRestoreOnFreshDestinations(t *testing.T) {
	original, db, _, _ := doctorFixture(t)
	archive := filepath.Join(t.TempDir(), "snapshot")
	if err := Backup(t.Context(), db, original, archive, db.Config().ConnConfig.ConnString(), nil); err != nil {
		t.Fatal(err)
	}
	url := pgtest.EmptyDB(t)
	fresh := pgtest.Pool(t, url)
	data := t.TempDir()
	v, err := volume.Acquire(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore(t.Context(), fresh, v, archive, url, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	v, err = volume.Acquire(data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := v.IdentifyExisting(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	if err := v.OpenExistingLayout(); err != nil {
		t.Fatal(err)
	}
	if v.StoreID() != original.StoreID() {
		t.Fatal("store id changed")
	}
	if n, err := store.New(fresh).RestoreDatabaseObjects(t.Context()); err != nil || n == 0 {
		t.Fatalf("catalog absent: %d %v", n, err)
	}
	r, err := Doctor(t.Context(), fresh, v, true)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(r, "doctor_pending_work") || r.HasErrors() {
		t.Fatalf("restore doctor: %+v", r)
	}
	var hash string
	if err := fresh.QueryRow(t.Context(), `SELECT hash FROM blobs`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	bs := blobstore.OpenReadOnly(v.Originals())
	size, err := bs.Verify(t.Context(), hash)
	if err != nil || size != 8 {
		t.Fatalf("restored blob %s %d %v", hash, size, err)
	}
	var state string
	if err := fresh.QueryRow(t.Context(), `SELECT state FROM jobs WHERE kind='render'`).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("render job %s %v", state, err)
	}
}

func TestRestoreAllowsOnlyEmptyLostAndFound(t *testing.T) {
	old, db, _, _ := doctorFixture(t)
	archive := filepath.Join(t.TempDir(), "archive")
	if err := Backup(t.Context(), db, old, archive, db.Config().ConnString(), nil); err != nil {
		t.Fatal(err)
	}
	url := pgtest.EmptyDB(t)
	fresh := pgtest.Pool(t, url)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "lost+found"), 0700); err != nil {
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
	if err := Restore(t.Context(), fresh, v, archive, url, nil); err != nil {
		t.Fatal(err)
	}
	if entries, err := v.Root().ReadDir("lost+found"); err != nil || len(entries) != 0 {
		t.Fatalf("lost+found changed: %+v %v", entries, err)
	}
}

func TestRestoreRefusesNonemptyDestinations(t *testing.T) {
	v, db, _, _ := doctorFixture(t)
	archive := filepath.Join(t.TempDir(), "archive")
	if err := Backup(t.Context(), db, v, archive, db.Config().ConnConfig.ConnString(), nil); err != nil {
		t.Fatal(err)
	}
	if err := Restore(t.Context(), db, v, archive, db.Config().ConnConfig.ConnString(), nil); err == nil || !strings.Contains(err.Error(), "restore_volume_not_empty") {
		t.Fatalf("existing volume: %v", err)
	}
	url := pgtest.EmptyDB(t)
	fresh := pgtest.Pool(t, url)
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "stranger"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	empty, err := volume.Acquire(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore(t.Context(), fresh, empty, archive, url, nil); err == nil {
		t.Fatal("existing file overwritten")
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(data, "stranger"))
	if err != nil || string(content) != "keep" {
		t.Fatalf("existing bytes changed: %q %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(data, volume.MaintenanceMarker)); !os.IsNotExist(err) {
		t.Fatal("refused restore wrote a marker")
	}
}

func TestRestoreRefusesNonemptyDatabaseBeforeVolumeWrite(t *testing.T) {
	old, db, _, _ := doctorFixture(t)
	archive := filepath.Join(t.TempDir(), "archive")
	if err := Backup(t.Context(), db, old, archive, db.Config().ConnString(), nil); err != nil {
		t.Fatal(err)
	}
	url := pgtest.EmptyDB(t)
	fresh := pgtest.Pool(t, url)
	if _, err := fresh.Exec(t.Context(), `CREATE TABLE user_data (id integer)`); err != nil {
		t.Fatal(err)
	}
	v, err := volume.Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := Restore(t.Context(), fresh, v, archive, url, nil); err == nil || !strings.Contains(err.Error(), "restore_database_not_empty") {
		t.Fatalf("nonempty database accepted: %v", err)
	}
	entries, err := v.Root().ReadDir("")
	if err != nil || len(entries) != 1 || entries[0].Name != volume.LockFile {
		t.Fatalf("volume modified: %+v %v", entries, err)
	}
	if n := queryDatabaseObjects(t, fresh); n != 1 {
		t.Fatalf("database modified: %d objects", n)
	}
}

func queryDatabaseObjects(t *testing.T, db *pgxpool.Pool) int64 {
	t.Helper()
	n, err := store.New(db).RestoreDatabaseObjects(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRestoreCorruptBackupRefusedBeforeMarker(t *testing.T) {
	old, db, _, _ := doctorFixture(t)
	archive := filepath.Join(t.TempDir(), "archive")
	if err := Backup(t.Context(), db, old, archive, db.Config().ConnConfig.ConnString(), nil); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(archive, backupDump))
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)/2] ^= 1
	if err := os.WriteFile(filepath.Join(archive, backupDump), body, 0600); err != nil {
		t.Fatal(err)
	}
	url := pgtest.EmptyDB(t)
	db2 := pgtest.Pool(t, url)
	data := t.TempDir()
	fresh, err := volume.Acquire(data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := fresh.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := Restore(t.Context(), db2, fresh, archive, url, nil); err == nil || !strings.Contains(err.Error(), "restore_dump_hash") {
		t.Fatalf("corrupt dump: %v", err)
	}
	entries, err := fresh.Root().ReadDir("")
	if err != nil || len(entries) != 1 || entries[0].Name != volume.LockFile {
		t.Fatalf("restore wrote before checking: %+v %v", entries, err)
	}
}

func TestRestoreDamagedOriginalAndManifestRefused(t *testing.T) {
	for _, name := range []string{"manifest", "original"} {
		t.Run(name, func(t *testing.T) {
			old, db, _, _ := doctorFixture(t)
			archive := filepath.Join(t.TempDir(), "complete")
			if err := Backup(t.Context(), db, old, archive, db.Config().ConnString(), nil); err != nil {
				t.Fatal(err)
			}
			var path string
			if name == "manifest" {
				path = filepath.Join(archive, backupManifest)
			} else {
				var hash string
				if err := db.QueryRow(t.Context(), `SELECT hash FROM blobs`).Scan(&hash); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(archive, backupBlobs, hash[:2], hash[2:4], hash)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if name == "manifest" {
				contents[0] = '!' // invalid JSON
			} else {
				contents[len(contents)/2] ^= 1 // same size, wrong SHA-256
			}
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			url := pgtest.EmptyDB(t)
			freshDB := pgtest.Pool(t, url)
			v, err := volume.Acquire(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := v.Close(); err != nil {
					t.Error(err)
				}
			}()
			if err := Restore(t.Context(), freshDB, v, archive, url, nil); err == nil {
				t.Fatal("corrupt backup accepted")
			}
			entries, err := v.Root().ReadDir("")
			if err != nil || len(entries) != 1 || entries[0].Name != volume.LockFile {
				t.Fatalf("restore modified volume before verification: %+v %v", entries, err)
			}
		})
	}
}

func TestRestorePendingImportFailed(t *testing.T) {
	old, db, _, _ := doctorFixture(t)
	batch, job := store.NewID(), store.NewID()
	if _, err := db.Exec(t.Context(), `INSERT INTO import_batches(id,root_rel,created_at) VALUES($1,'old source',now())`, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `INSERT INTO jobs(id,kind,batch_id,source_rel,state,queued_at,updated_at) VALUES($1,'import',$2,'not mounted','pending',now(),now())`, job, batch); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "archive")
	if err := Backup(t.Context(), db, old, archive, db.Config().ConnConfig.ConnString(), nil); err != nil {
		t.Fatal(err)
	}
	url := pgtest.EmptyDB(t)
	db2 := pgtest.Pool(t, url)
	v, err := volume.Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := Restore(t.Context(), db2, v, archive, url, nil); err != nil {
		t.Fatal(err)
	}
	var state, code, message string
	if err := db2.QueryRow(t.Context(), `SELECT state,error_code,error_message FROM jobs WHERE id=$1`, job).Scan(&state, &code, &message); err != nil || state != "failed" || code != "source_needs_verification" || !strings.Contains(message, "remount") {
		t.Fatalf("job: %s %s %s %v", state, code, message, err)
	}
}
