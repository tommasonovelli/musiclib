package volume

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/fsops"
	"musiclib/internal/store"
)

// §11.1 first initialization on an empty volume and an empty database, then
// a repeated boot on the same pair.
func TestFirstInitThenRepeatBoot(t *testing.T) {
	f := newFixture(t)
	id := f.mustBoot(t)
	if id == uuid.Nil || f.dbStoreID(t) != id {
		t.Fatalf("store id %s, database %s", id, f.dbStoreID(t))
	}
	marker := f.state(t, StoreMarker)
	if want := "store_id=" + id.String() + "\n"; string(marker.content) != want {
		t.Fatalf("marker %q, want %q", marker.content, want)
	}
	info, err := os.Stat(f.path(StoreMarker))
	if err != nil || info.Mode().Perm() != markerPerm {
		t.Fatalf("marker mode %v, %v; want %v", info.Mode(), err, os.FileMode(markerPerm))
	}
	f.assertLayout(t, true)
	if f.exists(t, storeMarkerTemp) {
		t.Fatal("the marker's temporary was left behind")
	}

	for range 3 {
		if again := f.mustBoot(t); again != id {
			t.Fatalf("repeat boot: store id %s, want %s", again, id)
		}
	}
	f.assertUnchanged(t, StoreMarker, marker)
}

// §11.1: "la procedura è ripetibile se il primo avvio si interrompe". A child
// process is killed with SIGKILL at each point of the first initialization;
// the disk and the database must be in the expected intermediate state, and
// a new boot must complete with the database's store id.
func TestFirstInitInterruptedAtEveryStep(t *testing.T) {
	for _, tc := range []struct {
		at        string
		dbID      bool // settings.store_id committed at the crash
		temp      bool // marker temporary on disk, marker absent
		marker    bool // marker on disk
		mediaDirs bool
	}{
		{at: "media_checked"},
		{at: "db_inserted"}, // inside the transaction: rolled back
		{at: "db_committed", dbID: true},
		{at: "marker_temp_synced", dbID: true, temp: true},
		{at: "marker_renamed", dbID: true, marker: true},
		{at: "marker_synced", dbID: true, marker: true},
		{at: "layout_created", dbID: true, marker: true, mediaDirs: true},
	} {
		t.Run(tc.at, func(t *testing.T) {
			f := newFixture(t)
			got := runHelper(t, "crash-first-init",
				"VOLUME_DIR="+f.dir, "VOLUME_DB="+f.dbURL, "CRASH_AT="+tc.at)
			if got != "killed" {
				t.Fatalf("child: %s, want killed at %s", got, tc.at)
			}
			dbID := f.dbStoreID(t)
			if (dbID != uuid.Nil) != tc.dbID {
				t.Fatalf("after the crash: database store id %s, want present=%v", dbID, tc.dbID)
			}
			if got := f.exists(t, storeMarkerTemp); got != tc.temp {
				t.Fatalf("after the crash: temporary exists = %v, want %v", got, tc.temp)
			}
			if tc.temp {
				if b, _ := os.ReadFile(f.path(storeMarkerTemp)); string(b) != string(EncodeStoreMarker(dbID)) {
					t.Fatalf("temporary %q, want the database's id %s", b, dbID)
				}
			}
			if got := f.exists(t, StoreMarker); got != tc.marker {
				t.Fatalf("after the crash: marker exists = %v, want %v", got, tc.marker)
			}
			f.assertLayout(t, tc.mediaDirs)

			id := f.mustBoot(t)
			if tc.dbID && id != dbID {
				t.Fatalf("retry adopted %s, want the committed %s", id, dbID)
			}
			if f.dbStoreID(t) != id {
				t.Fatalf("database %s, boot %s", f.dbStoreID(t), id)
			}
			if b, _ := os.ReadFile(f.path(StoreMarker)); string(b) != string(EncodeStoreMarker(id)) {
				t.Fatalf("marker %q after the retry", b)
			}
			if f.exists(t, storeMarkerTemp) {
				t.Fatal("the temporary survived the retry")
			}
			f.assertLayout(t, true)
		})
	}
}

// A temporary left with arbitrary content (a crash in the middle of the
// write) is replaced, not trusted.
func TestFirstInitReplacesPartialTemporary(t *testing.T) {
	f := newFixture(t)
	f.write(t, storeMarkerTemp, "store_id=0192")
	if err := os.Chmod(f.path(storeMarkerTemp), 0o444); err != nil {
		t.Fatal(err)
	}
	id := f.mustBoot(t)
	if b, _ := os.ReadFile(f.path(StoreMarker)); string(b) != string(EncodeStoreMarker(id)) {
		t.Fatalf("marker %q", b)
	}
}

// The database was initialized, the marker is missing and the media storage
// is empty: the only reachable cause is an interrupted first init, so the
// marker is completed with the database's id.
func TestStoreIDWithoutMarkerOnEmptyMedia(t *testing.T) {
	f := newFixture(t)
	want := store.NewID()
	f.setDBStoreID(t, want)
	// Empty media directories count as empty.
	for _, d := range mediaDirs {
		if err := os.Mkdir(f.path(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.mustBoot(t); got != want {
		t.Fatalf("store id %s, want the database's %s", got, want)
	}
}

// The reverse: a marker but no store id in the database. The database is new
// or was reset; adopting the volume would pair it with a foreign catalog.
func TestMarkerWithoutStoreID(t *testing.T) {
	f := newFixture(t)
	f.write(t, StoreMarker, string(EncodeStoreMarker(store.NewID())))
	before := f.state(t, StoreMarker)
	_, err := f.boot(t)
	wantCode(t, err, CodeDBUninitialized)
	f.assertUnchanged(t, StoreMarker, before)
	if id := f.dbStoreID(t); id != uuid.Nil {
		t.Fatalf("the refused boot wrote store id %s to the database", id)
	}
	f.assertLayout(t, false)
}

// A marker that names another store is refused and never rewritten, and
// the database is not touched either.
func TestStoreMismatchRefusedAndNeverRewritten(t *testing.T) {
	f := newFixture(t)
	dbID := f.mustBoot(t)
	other := store.NewID()
	if err := os.Chmod(f.path(StoreMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	f.write(t, StoreMarker, string(EncodeStoreMarker(other)))
	before := f.state(t, StoreMarker)
	for range 2 {
		_, err := f.boot(t)
		wantCode(t, err, CodeStoreMismatch)
		for _, id := range []string{dbID.String(), other.String()} {
			if !strings.Contains(err.Error(), id) {
				t.Fatalf("the error does not name store %s: %v", id, err)
			}
		}
		f.assertUnchanged(t, StoreMarker, before)
		if got := f.dbStoreID(t); got != dbID {
			t.Fatalf("database store id changed to %s", got)
		}
	}
}

// No marker and media that is not empty: never initialized or completed,
// whether or not the database has a store id; nothing is written.
func TestMissingMarkerWithMediaRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *fixture)
	}{
		{"blob in originals", func(t *testing.T, f *fixture) { f.write(t, "originals/ab/cd/x", "x") }},
		{"album in library", func(t *testing.T, f *fixture) { f.write(t, "library/A/B/01.flac", "x") }},
		{"empty dir in library", func(t *testing.T, f *fixture) {
			if err := os.MkdirAll(f.path("library/Artist"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"blobs dir in work", func(t *testing.T, f *fixture) {
			if err := os.MkdirAll(f.path("work/blobs"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"originals is a file", func(t *testing.T, f *fixture) { f.write(t, "originals", "x") }},
		{"work is a symlink", func(t *testing.T, f *fixture) {
			if err := os.Mkdir(f.path("elsewhere"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("elsewhere", f.path("work")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, withDB := range []bool{false, true} {
			name := tc.name + "/empty database"
			want := CodeNotEmpty
			if withDB {
				name, want = tc.name+"/database initialized", CodeMarkerMissing
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				var dbID uuid.UUID
				if withDB {
					dbID = store.NewID()
					f.setDBStoreID(t, dbID)
				}
				tc.setup(t, f)
				_, err := f.boot(t)
				wantCode(t, err, want)
				if got := f.dbStoreID(t); got != dbID {
					t.Fatalf("database store id %s, want %s", got, dbID)
				}
				if f.exists(t, StoreMarker) || f.exists(t, storeMarkerTemp) {
					t.Fatal("a refused boot wrote the marker")
				}
			})
		}
	}
}

// Entries at the top of /data other than the media directories do not make
// the storage non-empty: lost+found exists on a dedicated ext4 filesystem.
func TestOtherTopLevelEntriesAreNotMedia(t *testing.T) {
	f := newFixture(t)
	if err := os.Mkdir(f.path("lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.mustBoot(t)
}

func TestMalformedStoreMarker(t *testing.T) {
	id := store.NewID().String()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *fixture)
	}{
		{"empty", content("")},
		{"no newline", content("store_id=" + id)},
		{"bare uuid", content(id + "\n")},
		{"uppercase", content("store_id=" + strings.ToUpper(id) + "\n")},
		{"braces", content("store_id={" + id + "}\n")},
		{"urn", content("store_id=urn:uuid:" + id + "\n")},
		{"nil uuid", content("store_id=" + uuid.Nil.String() + "\n")},
		{"crlf", content("store_id=" + id + "\r\n")},
		{"trailing space", content("store_id=" + id + " \n")},
		{"leading space", content(" store_id=" + id + "\n")},
		{"extra line", content("store_id=" + id + "\nx=y\n")},
		{"blank line", content("store_id=" + id + "\n\n")},
		{"wrong key", content("store=" + id + "\n")},
		{"huge", content("store_id=" + id + "\n" + strings.Repeat("#", 4096))},
		{"directory", func(t *testing.T, f *fixture) {
			if err := os.Mkdir(f.path(StoreMarker), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink to a valid marker", func(t *testing.T, f *fixture) {
			f.write(t, "real", "store_id="+id+"\n")
			if err := os.Symlink("real", f.path(StoreMarker)); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", func(t *testing.T, f *fixture) {
			if err := unixMkfifo(f.path(StoreMarker)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.setup(t, f)
			before := f.state(t, StoreMarker)
			_, err := f.boot(t)
			wantCode(t, err, CodeMarkerMalformed)
			f.assertUnchanged(t, StoreMarker, before)
			if got := f.dbStoreID(t); got != uuid.Nil {
				t.Fatalf("a malformed marker let the boot write store id %s", got)
			}
		})
	}
}

func content(s string) func(t *testing.T, f *fixture) {
	return func(t *testing.T, f *fixture) { f.write(t, StoreMarker, s) }
}

// §11.3: while /data/.maintenance exists the boot does not proceed, whatever
// the marker says, and nothing is written.
func TestMaintenanceMarkerBlocksBoot(t *testing.T) {
	id := store.NewID()
	for _, tc := range []struct {
		name    string
		content string
		code    string
	}{
		{"rebuild", "operation=rebuild\nstore_id=" + id.String() + "\n", CodeMaintenance},
		{"restore", "operation=restore\nstore_id=" + id.String() + "\n", CodeMaintenance},
		{"unknown operation", "operation=repair\nstore_id=" + id.String() + "\n", CodeMaintenanceMalformed},
		{"swapped lines", "store_id=" + id.String() + "\noperation=rebuild\n", CodeMaintenanceMalformed},
		{"missing store id", "operation=rebuild\n", CodeMaintenanceMalformed},
		{"empty", "", CodeMaintenanceMalformed},
		{"uppercase uuid", "operation=rebuild\nstore_id=" + strings.ToUpper(id.String()) + "\n", CodeMaintenanceMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.write(t, MaintenanceMarker, tc.content)
			before := f.state(t, MaintenanceMarker)
			_, err := f.boot(t)
			wantCode(t, err, tc.code)
			if tc.code == CodeMaintenance {
				op := strings.SplitN(strings.TrimPrefix(tc.content, "operation="), "\n", 2)[0]
				for _, s := range []string{op, id.String()} {
					if !strings.Contains(err.Error(), s) {
						t.Fatalf("the error does not name %q: %v", s, err)
					}
				}
			}
			f.assertUnchanged(t, MaintenanceMarker, before)
			if f.exists(t, StoreMarker) || f.dbStoreID(t) != uuid.Nil {
				t.Fatal("the boot initialized a volume under maintenance")
			}
			f.assertLayout(t, false)
		})
	}
	t.Run("directory", func(t *testing.T) {
		f := newFixture(t)
		if err := os.Mkdir(f.path(MaintenanceMarker), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := f.boot(t)
		wantCode(t, err, CodeMaintenanceMalformed)
	})
}

func TestMaintenanceFormatRoundTrip(t *testing.T) {
	for _, op := range []string{OpRebuild, OpRestore} {
		m := Maintenance{Operation: op, StoreID: store.NewID()}
		b := m.Encode()
		if want := "operation=" + op + "\nstore_id=" + m.StoreID.String() + "\n"; string(b) != want {
			t.Fatalf("Encode = %q, want %q", b, want)
		}
		got, err := ParseMaintenance(b)
		if err != nil || got != m {
			t.Fatalf("ParseMaintenance(Encode(m)) = %+v, %v; want %+v", got, err, m)
		}
	}
	id := store.NewID()
	got, err := ParseStoreMarker(EncodeStoreMarker(id))
	if err != nil || got != id {
		t.Fatalf("ParseStoreMarker(EncodeStoreMarker(id)) = %s, %v", got, err)
	}
}

// §2.2: one instance per volume. A real second process cannot take the lock
// while it is held, and can once it is released.
func TestLockExcludesOtherProcesses(t *testing.T) {
	f := &fixture{dir: t.TempDir()}
	v, err := Acquire(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := runHelper(t, "try-acquire", "VOLUME_DIR="+f.dir); got != "error "+CodeLocked {
		t.Fatalf("second process with the lock held: %s", got)
	}
	// Also a second Acquire in the same process.
	_, err = Acquire(f.dir)
	wantCode(t, err, CodeLocked)
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runHelper(t, "try-acquire", "VOLUME_DIR="+f.dir); got != "acquired" {
		t.Fatalf("second process after release: %s", got)
	}
}

func TestAcquireUnavailable(t *testing.T) {
	dir := t.TempDir()
	_, err := Acquire(filepath.Join(dir, "missing"))
	wantCode(t, err, CodeUnavailable)
	if os.Geteuid() != 0 {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Error(err)
			}
		})
		_, err = Acquire(dir) // cannot create .lock
		wantCode(t, err, CodePermission)
		if err := os.Chmod(dir, 0o311); err != nil {
			t.Fatal(err)
		}
		_, err = Acquire(dir) // cannot open /data itself
		wantCode(t, err, CodePermission)
	}
	// A .lock that is not a regular file is not a permission problem.
	dir = t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, LockFile), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = Acquire(dir)
	wantCode(t, err, CodeUnavailable)
}

// Close closes every root and releases the lock; the roots handed out are
// unusable afterwards.
func TestCloseReleasesEverything(t *testing.T) {
	f := newFixture(t)
	v, err := Acquire(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootSteps(t.Context(), v, f.pool); err != nil {
		t.Fatal(err)
	}
	roots := []*fsops.Root{v.Root(), v.Originals(), v.Library(), v.Work()}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	for _, r := range roots {
		if _, err := r.Stat(""); fsops.Code(err) != fsops.CodeRootClosed {
			t.Fatalf("root %s still usable after Close: %v", r.Name(), err)
		}
	}
	if err := v.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	f.acquire(t)
}

func TestStepOrderEnforced(t *testing.T) {
	f := newFixture(t)
	v := f.acquire(t)
	if err := v.OpenLayout(); err == nil {
		t.Fatal("OpenLayout before Identify succeeded")
	}
	if err := v.CheckFilesystem(); err == nil {
		t.Fatal("CheckFilesystem before OpenLayout succeeded")
	}
	f.assertLayout(t, false)
}

func TestIdentifyDatabaseError(t *testing.T) {
	f := newFixture(t)
	v := f.acquire(t)
	f.pool.Close()
	_, err := v.Identify(t.Context(), f.pool)
	wantCode(t, err, CodeDB)
	if f.exists(t, StoreMarker) {
		t.Fatal("marker written without a database")
	}
}

func TestErrorCodes(t *testing.T) {
	cause := &fsops.Error{Code: fsops.CodeIO}
	err := error(newErr(CodeIO, "x", cause))
	if Code(err) != CodeIO || !errors.Is(err, cause) {
		t.Fatalf("Code/Unwrap broken: %v", err)
	}
	if Code(cause) != fsops.CodeIO || Code(errors.New("x")) != "" {
		t.Fatal("Code fallback broken")
	}
}

// N-069: completing a missing marker is only for an interrupted first boot.
// A database with catalog content belongs to another volume: an empty volume
// (for example a mistyped /data mount) is refused, and nothing is written to
// either side, whether or not the database has a store id.
func TestEmptyVolumeRefusedWithCatalogContent(t *testing.T) {
	blob := strings.Repeat("ab", 32)
	artist, album, batch, job := store.NewID(), store.NewID(), store.NewID(), store.NewID()
	for _, tc := range []struct {
		name string
		sql  []string
	}{
		{"blob", []string{
			"INSERT INTO blobs VALUES ('" + blob + "', 1, 'flac', now())"}},
		{"artist", []string{
			"INSERT INTO artists VALUES ('" + artist.String() + "', 'A', 'a', 1)"}},
		{"album", []string{
			"INSERT INTO artists VALUES ('" + artist.String() + "', 'A', 'a', 1)",
			"INSERT INTO albums (id, artist_id, title, folder_key, revision) VALUES ('" +
				album.String() + "', '" + artist.String() + "', 'B', 'b', 1)"}},
		{"import batch", []string{
			"INSERT INTO import_batches VALUES ('" + batch.String() + "', '', now())"}},
		{"scan job", []string{
			"INSERT INTO import_batches VALUES ('" + batch.String() + "', '', now())",
			"INSERT INTO jobs (id, kind, batch_id, state, queued_at, updated_at) VALUES ('" +
				job.String() + "', 'scan', '" + batch.String() + "', 'pending', now(), now())"}},
	} {
		for _, withID := range []bool{true, false} {
			name := tc.name + "/store id"
			if !withID {
				name = tc.name + "/no store id"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				var dbID uuid.UUID
				if withID {
					dbID = store.NewID()
					f.setDBStoreID(t, dbID)
				}
				for _, s := range tc.sql {
					if _, err := f.pool.Exec(t.Context(), s); err != nil {
						t.Fatalf("%s: %v", s, err)
					}
				}
				_, err := f.boot(t)
				wantCode(t, err, CodeMarkerMissing)
				if got := f.dbStoreID(t); got != dbID {
					t.Fatalf("database store id %s, want %s", got, dbID)
				}
				// Only the lock file: no marker, no temporary, no layout.
				entries, err := os.ReadDir(f.dir)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 || entries[0].Name() != LockFile {
					t.Fatalf("the refused boot wrote to the volume: %v", entries)
				}
			})
		}
	}
}
