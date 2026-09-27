package volume

import (
	"os"
	"testing"

	"musiclib/internal/store"
)

func TestMaintenanceMarkerCreationAndRefusals(t *testing.T) {
	f := newFixture(t)
	id := f.mustBoot(t)
	v := f.acquire(t)
	if err := v.BeginMaintenance(OpRebuild, id); err != nil {
		t.Fatal(err)
	}
	before := f.state(t, MaintenanceMarker)
	if string(before.content) != string((Maintenance{OpRebuild, id}).Encode()) {
		t.Fatalf("marker %q", before.content)
	}
	if info, err := os.Stat(f.path(MaintenanceMarker)); err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("marker permissions %v, %v", info, err)
	}
	if err := v.BeginMaintenance(OpRebuild, id); err != nil {
		t.Fatal(err)
	}
	f.assertUnchanged(t, MaintenanceMarker, before)
	wantCode(t, v.BeginMaintenance(OpRestore, id), CodeMaintenance)
	wantCode(t, v.BeginMaintenance(OpRebuild, store.NewID()), CodeMaintenance)
	wantCode(t, v.EndMaintenance(OpRestore, id), CodeMaintenance)
	f.assertUnchanged(t, MaintenanceMarker, before)
	wantCode(t, v.CheckMaintenance(), CodeMaintenance)
	if _, err := f.boot(t); Code(err) != CodeLocked {
		// The still-open lock refuses a second process before the marker read.
		t.Fatalf("boot while maintenance holds lock: %v", err)
	}
	if err := v.EndMaintenance(OpRebuild, id); err != nil {
		t.Fatal(err)
	}
	if f.exists(t, MaintenanceMarker) {
		t.Fatal("marker remains after durable removal")
	}
	wantCode(t, v.EndMaintenance(OpRebuild, id), CodeMaintenance)
}

func TestMaintenanceMarkerCrashWindows(t *testing.T) {
	for _, point := range []string{"maintenance_temp_synced", "maintenance_renamed", "maintenance_synced", "maintenance_removed"} {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t)
			id := f.mustBoot(t)
			if point == "maintenance_removed" {
				v := f.acquire(t)
				if err := v.BeginMaintenance(OpRebuild, id); err != nil {
					t.Fatal(err)
				}
				if err := v.Close(); err != nil {
					t.Fatal(err)
				}
			}
			env := []string{"VOLUME_DIR=" + f.dir, "STORE_ID=" + id.String(), "CRASH_AT=" + point}
			if point == "maintenance_removed" {
				env = append(env, "MAINTENANCE_ACTION=end")
			}
			if got := runHelper(t, "crash-maintenance", env...); got != "killed" {
				t.Fatalf("child: %s", got)
			}
			if point == "maintenance_temp_synced" || point == "maintenance_removed" {
				if f.exists(t, MaintenanceMarker) {
					t.Fatal("uninstalled or removed marker still present")
				}
			} else {
				if !f.exists(t, MaintenanceMarker) {
					t.Fatal("installed marker lost")
				}
				_, err := f.boot(t)
				wantCode(t, err, CodeMaintenance)
			}
			v := f.acquire(t)
			if err := v.BeginMaintenance(OpRebuild, id); err != nil {
				t.Fatal(err)
			}
			if err := v.EndMaintenance(OpRebuild, id); err != nil {
				t.Fatal(err)
			}
			if f.exists(t, MaintenanceMarker) {
				t.Fatal("marker remains after repeat")
			}
		})
	}
}
