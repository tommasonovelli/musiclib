package main

import (
	"bytes"
	"strings"
	"testing"

	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

func TestRebuildCommandRefusesWrongStoreIDWithoutWriting(t *testing.T) {
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
	var out, logs bytes.Buffer
	if code := runRebuild(doctorEnv(db.Config().ConnString()), p, store.NewID(), &out, newLogger(&logs)); code != exitUsage || !strings.Contains(logs.String(), "rebuild_store_id") || out.Len() != 0 {
		t.Fatalf("wrong-id refusal: %d %q %q", code, out.String(), logs.String())
	}
	v, err = volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := v.CheckMaintenance(); err != nil {
		t.Fatalf("refusal wrote marker: %v", err)
	}
	for _, dir := range []string{volume.Library, volume.Work} {
		if _, err := v.Root().Stat(dir); err != nil {
			t.Fatalf("refusal deleted %s: %v", dir, err)
		}
	}
}
