package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"musiclib/internal/blobstore"
	"musiclib/internal/maintenance"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

func TestBackupRestoreRefuseHeldLockBeforeDatabase(t *testing.T) {
	p := testPaths(t)
	v, err := volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	noDB := doctorEnv("postgres://invalid:secret@127.0.0.1:1/db")
	for _, tc := range []struct {
		name string
		run  func(*bytes.Buffer, *bytes.Buffer) int
	}{
		{"backup", func(out, log *bytes.Buffer) int { return runBackup(noDB, p, "/backup/safe", out, newLogger(log)) }},
		{"restore", func(out, log *bytes.Buffer) int { return runRestore(noDB, p, "/backup/safe", out, newLogger(log)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, logs bytes.Buffer
			if n := tc.run(&out, &logs); n != exitUsage || !strings.Contains(logs.String(), "volume_locked") || strings.Contains(logs.String(), "secret") || out.Len() != 0 {
				t.Fatalf("refusal %d %q %q", n, out.String(), logs.String())
			}
		})
	}
}

// A destination outside /backup is inside the one-off container, lost when
// it exits although the backup reports success: the command refuses it with
// its own code before the volume lock, the database or the destination is
// touched. Without a database or a /data, any later refusal would log another
// code.
func TestBackupRefusesDestinationOutsideBackup(t *testing.T) {
	noDB := doctorEnv("postgres://invalid:secret@127.0.0.1:1/db")
	for _, dest := range []string{
		"/tmp/x", "/", "", "/data/backup", "/backupx", "/backupx/2026",
		"/backup", "/backup/", "/backup/.", "/backup/..", "/backup/x/",
		"/backup//x", "/backup/./x", "/backup/../tmp/x", "/backup/x/../../tmp",
		"backup/x", "./backup/x", "../backup/x",
	} {
		var logs syncBuffer
		got := musiclibd([]string{"backup", "--to", dest}, noDB, &logs)
		if got != exitUsage || !strings.Contains(logs.String(), `"code":"backup_outside_backup"`) || strings.Contains(logs.String(), "secret") {
			t.Fatalf("backup --to %q = %d, logged %s", dest, got, &logs)
		}
	}
	// What backup.sh and the documented commands pass, and a nested folder.
	for _, dest := range []string{"/backup/2026-09-29-2130", "/backup/before-update-2026-09-29-2130", "/backup/2026-09-29 full", "/backup/old/2026-09-29-2130"} {
		if err := checkBackupDestination(dest); err != nil {
			t.Fatalf("backup --to %q refused: %v", dest, err)
		}
	}
}

func TestBackupRestoreRefuseUnavailableDatabase(t *testing.T) {
	p := testPaths(t)
	noDB := doctorEnv("postgres://invalid:secret@127.0.0.1:1/db")
	for _, tc := range []struct {
		name string
		run  func(*bytes.Buffer, *bytes.Buffer) int
	}{
		{"backup", func(out, log *bytes.Buffer) int { return runBackup(noDB, p, "/backup/safe", out, newLogger(log)) }},
		{"restore", func(out, log *bytes.Buffer) int { return runRestore(noDB, p, "/backup/safe", out, newLogger(log)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, logs bytes.Buffer
			if n := tc.run(&out, &logs); n != exitUsage || !strings.Contains(logs.String(), "maintenance_database") || strings.Contains(logs.String(), "secret") || out.Len() != 0 {
				t.Fatalf("database refusal %d %q %q", n, out.String(), logs.String())
			}
		})
	}
}

func TestBackupRestoreLogStableCorruptionCodes(t *testing.T) {
	db := pgtest.New(t)
	p := testPaths(t)
	v, err := volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Identify(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := v.OpenLayout(); err != nil {
		t.Fatal(err)
	}
	bs, err := blobstore.New(v.Originals(), v.Work())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bs.Put(t.Context(), strings.NewReader("original")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "complete")
	if err := maintenance.Backup(t.Context(), db, v, archive, db.Config().ConnString(), nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(archive, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m maintenance.Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	// The archive stays correct; corrupt only the source original for backup.
	b := m.Blobs[0]
	path := filepath.Join(p.data, "originals", b.SHA256[:2], b.SHA256[2:4], b.SHA256)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("bad"), 0644); err != nil {
		t.Fatal(err)
	}
	var out, logs bytes.Buffer
	code := runBackup(doctorEnv(db.Config().ConnString()), p, filepath.Join(t.TempDir(), "new"), &out, newLogger(&logs))
	if code != exitFailure || !strings.Contains(logs.String(), `"code":"backup_corrupt_blob"`) {
		t.Fatalf("backup code %d: %s", code, &logs)
	}
	// The fresh restore must refuse a corrupt archive before a marker exists.
	dump := filepath.Join(archive, "catalog.dump")
	f, err := os.OpenFile(dump, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("!"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fresh := testPaths(t)
	url := pgtest.EmptyDB(t)
	out.Reset()
	logs.Reset()
	code = runRestore(doctorEnv(url), fresh, archive, &out, newLogger(&logs))
	if code != exitUsage || !strings.Contains(logs.String(), `"code":"restore_dump_hash"`) || strings.Contains(logs.String(), "recreate BOTH") {
		t.Fatalf("restore code %d: %s", code, &logs)
	}
}

func TestRealServerRefusesIncompleteMaintenanceMarker(t *testing.T) {
	for _, operation := range []string{volume.OpRebuild, volume.OpRestore} {
		t.Run(string(operation), func(t *testing.T) {
			db := pgtest.New(t)
			p := testPaths(t)
			v, err := volume.Acquire(p.data)
			if err != nil {
				t.Fatal(err)
			}
			id, err := v.Identify(t.Context(), db)
			if err != nil {
				t.Fatal(err)
			}
			if err := v.OpenLayout(); err != nil {
				t.Fatal(err)
			}
			if err := v.BeginMaintenance(operation, id); err != nil {
				t.Fatal(err)
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			s := startServerProcess(t, db.Config().ConnString(), p, 022)
			if code := s.wait(t); code != exitFailure || !strings.Contains(s.stderr.String(), "volume_maintenance_pending") {
				t.Fatalf("boot %d: %s", code, s.stderr)
			}
		})
	}
}

func TestBackupRestoreCommandsDoNotStartServer(t *testing.T) {
	db := pgtest.New(t)
	p := testPaths(t)
	v, err := volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Identify(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := v.OpenLayout(); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	// Restore must refuse the existing volume without touching it.
	var out, logs bytes.Buffer
	if code := runRestore(doctorEnv(db.Config().ConnString()), p, "/backup/absent", &out, newLogger(&logs)); code != exitUsage || !strings.Contains(logs.String(), "restore_volume_not_empty") {
		t.Fatalf("restore %d %q", code, logs.String())
	}
}
