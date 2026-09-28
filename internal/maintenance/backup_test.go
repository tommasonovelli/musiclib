package maintenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

func TestPGConnectionDoesNotExposeCredentialsOrPGXOptions(t *testing.T) {
	for _, input := range []string{
		"postgres://alice:uri-secret@db:5432/catalog?sslmode=disable&sslpassword=tls-secret&pool_max_conns=7",
		"host=db port=5432 user=alice password=uri-secret dbname=catalog sslmode=disable pool_max_conns=7",
		"postgres://db/catalog?user=alice&password=uri-secret&sslmode=disable",
	} {
		uri, password, err := pgConnection(input)
		if err != nil {
			t.Fatal(err)
		}
		if password != "uri-secret" || strings.Contains(uri, "secret") || strings.Contains(uri, "pool_max_conns") || !strings.Contains(uri, "catalog") {
			t.Fatalf("unsafe libpq URI %q (password set %v)", uri, password == "uri-secret")
		}
	}
	if stderr := safeToolStderr([]byte("connect postgres://alice:uri-secret@db/catalog password=uri-secret sslpassword=tls-secret"), "uri-secret"); strings.Contains(stderr, "secret") {
		t.Fatalf("secret in stderr: %s", stderr)
	}
}

func TestBackupAndRestoreEmptyOriginals(t *testing.T) {
	db := pgtest.New(t)
	v, err := volume.Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Identify(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := v.OpenLayout(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "empty")
	if err := Backup(t.Context(), db, v, archive, db.Config().ConnString(), nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	url := pgtest.EmptyDB(t)
	fresh := pgtest.Pool(t, url)
	newVolume, err := volume.Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := newVolume.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := Restore(t.Context(), fresh, newVolume, archive, url, nil); err != nil {
		t.Fatal(err)
	}
}

func TestBackupManifestAndRefusals(t *testing.T) {
	v, db, dir, _ := doctorFixture(t)
	url := db.Config().ConnConfig.ConnString()
	dest := filepath.Join(t.TempDir(), "backup with spaces")
	if err := Backup(t.Context(), db, v, dest, url, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, backupManifest))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	latest, err := store.LatestSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	// The schema of this binary: 2 since the dismissal of failed jobs (N-285).
	if m.StoreID != v.StoreID() || m.SchemaVersion != latest || latest != 3 || m.AppVersion == "" || len(m.Blobs) != 1 {
		t.Fatalf("bad manifest: %+v", m)
	}
	dump, err := os.ReadFile(filepath.Join(dest, backupDump))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(dump)
	if hex.EncodeToString(sum[:]) != m.DumpSHA256 {
		t.Fatal("dump hash mismatch")
	}
	b := m.Blobs[0]
	if b.Size != 8 {
		t.Fatalf("blob size: %d", b.Size)
	}
	copy, err := os.ReadFile(filepath.Join(dest, "originals", b.SHA256[:2], b.SHA256[2:4], b.SHA256))
	if err != nil || string(copy) != "original" {
		t.Fatalf("copied blob: %q %v", copy, err)
	}
	if err := Backup(t.Context(), db, v, dest, url, nil); err == nil || !strings.Contains(err.Error(), "backup_exists") {
		t.Fatalf("overwrite: %v", err)
	}
	// An alias of the data directory is rejected by identity, not by its
	// spelling. A direct path inside the data root is rejected too.
	// Rebuild or the next boot would delete a backup inside library/ or
	// work/; one inside originals/ would break doctor (N-228).
	for _, inside := range []string{"", "library", "work", "originals", "work/deep"} {
		parent := filepath.Join(dir, inside)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		err := Backup(t.Context(), db, v, filepath.Join(parent, "backup"), url, nil)
		var me *Error
		if !errors.As(err, &me) || me.Code != "backup_destination" || !me.Refusal {
			t.Fatalf("backup inside %q: %v", inside, err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "work", "deep")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if err := Backup(t.Context(), db, v, filepath.Join(alias, "backup"), url, nil); err == nil {
		t.Fatal("symlink into /data accepted")
	}
	for _, media := range []string{"work", "library", "originals"} {
		link := filepath.Join(t.TempDir(), "aliased-"+media)
		if err := os.Symlink(filepath.Join(dir, media), link); err != nil {
			t.Fatal(err)
		}
		if err := Backup(t.Context(), db, v, filepath.Join(link, "backup"), url, nil); err == nil {
			t.Fatalf("alias of %s accepted", media)
		}
	}
}

func TestBackupCorruptBlobNeverPublishesFinalName(t *testing.T) {
	v, db, dir, _ := doctorFixture(t)
	entries, err := v.Originals().ReadDir("")
	if err != nil {
		t.Fatal(err)
	}
	shard1 := entries[0].Name
	entries, err = v.Originals().ReadDir(shard1)
	if err != nil {
		t.Fatal(err)
	}
	shard2 := entries[0].Name
	entries, err = v.Originals().ReadDir(shard1 + "/" + shard2)
	if err != nil {
		t.Fatal(err)
	}
	rel := shard1 + "/" + shard2 + "/" + entries[0].Name
	if err := os.Chmod(filepath.Join(dir, "originals", filepath.FromSlash(rel)), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := v.Originals().OpenFile(rel, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(t.TempDir(), "bad")
	if err := Backup(t.Context(), db, v, final, db.Config().ConnConfig.ConnString(), nil); err == nil || !strings.Contains(err.Error(), "backup_corrupt_blob") {
		t.Fatalf("corrupt blob: %v", err)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("final exists: %v", err)
	}
}

// A referenced zero-size blob that is absent from originals/ must be
// reported missing: reading the inventory map without a presence check
// would see the zero value and accept it.
func TestBackupMissingZeroSizeReferencedBlob(t *testing.T) {
	v, db, _, _ := doctorFixture(t)
	empty := sha256.Sum256(nil)
	hash := hex.EncodeToString(empty[:])
	if _, err := db.Exec(t.Context(), `INSERT INTO blobs(hash,size,created_at) VALUES($1,0,now())`, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE tracks SET lyrics_hash=$1`, hash); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(t.TempDir(), "missing")
	err := Backup(t.Context(), db, v, final, db.Config().ConnConfig.ConnString(), nil)
	var me *Error
	if !errors.As(err, &me) || me.Code != "backup_blob_missing" {
		t.Fatalf("missing zero-size blob: %v", err)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("final exists: %v", err)
	}
}

// N-228: the host-path check compares filesystem locations from
// /proc/self/mountinfo. A directory opened through its own path (as a
// separate /backup bind mount would be) inside the data directory is
// refused; a sibling directory is accepted.
func TestBackupDestinationOutsideDataSource(t *testing.T) {
	dataDir, sibling := t.TempDir(), t.TempDir()
	nested := filepath.Join(dataDir, "host-backups")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	open := func(p string) *fsops.Root {
		r, err := fsops.OpenRoot(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := r.Close(); err != nil {
				t.Error(err)
			}
		})
		return r
	}
	data := open(dataDir)
	err := outsideDataSource(open(nested), data)
	var me *Error
	if !errors.As(err, &me) || me.Code != "backup_destination" || !me.Refusal {
		t.Fatalf("nested destination: %v", err)
	}
	if err := outsideDataSource(data, data); !errors.As(err, &me) || me.Code != "backup_destination" {
		t.Fatalf("data itself: %v", err)
	}
	if err := outsideDataSource(open(sibling), data); err != nil {
		t.Fatalf("sibling destination: %v", err)
	}
}

func TestBackupFailedPointLeavesOnlyTemporary(t *testing.T) {
	v, db, _, _ := doctorFixture(t)
	parent := t.TempDir()
	final := filepath.Join(parent, "finished")
	hook := failpoint.Hook(func(p failpoint.Point) error {
		if p.Name == "backup_before_rename" {
			return os.ErrClosed
		}
		return nil
	})
	if err := Backup(t.Context(), db, v, final, db.Config().ConnConfig.ConnString(), hook); err == nil {
		t.Fatal("failpoint ignored")
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("final exists: %v", err)
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
		t.Fatalf("leftovers: %+v", entries)
	}
	if _, err := store.New(db).GetStoreID(t.Context()); err != nil {
		t.Fatal(err)
	}
}
