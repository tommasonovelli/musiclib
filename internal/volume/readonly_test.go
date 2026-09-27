package volume

import (
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/fsops"
	"musiclib/internal/store/pgtest"
)

func TestReadOnlyVolumeDoesNotInitialize(t *testing.T) {
	db := pgtest.New(t)
	path := t.TempDir()
	v, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.IdentifyExisting(t.Context(), db); Code(err) != CodeMarkerMissing {
		t.Fatalf("new volume identified: %v", err)
	}
	if err := v.OpenExistingLayout(); err == nil {
		t.Fatal("opened uninitialized layout")
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := fsops.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := r.ReadDir("")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != LockFile {
		t.Fatalf("inspection created media or identity: %+v", entries)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyVolumeIdentifiesExistingWithoutChangingMarkers(t *testing.T) {
	db := pgtest.New(t)
	path := t.TempDir()
	v, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.Identify(t.Context(), db)
	if err != nil || id == uuid.Nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := v.OpenLayout(); err != nil {
		t.Fatal(err)
	}
	before, err := v.Root().Stat(StoreMarker)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	v, err = Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	got, err := v.IdentifyExisting(t.Context(), db)
	if err != nil || got != id {
		t.Fatalf("existing: %s, %v", got, err)
	}
	if err := v.OpenExistingLayout(); err != nil {
		t.Fatal(err)
	}
	after, err := v.Root().Stat(StoreMarker)
	if err != nil {
		t.Fatal(err)
	}
	if before.Ino != after.Ino || before.MTime != after.MTime {
		t.Fatalf("marker changed: %+v => %+v", before, after)
	}
}
