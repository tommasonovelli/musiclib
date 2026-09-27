package volume

import (
	"bytes"
	"errors"

	"github.com/google/uuid"

	"musiclib/internal/fsops"
)

const maintenanceTemp = MaintenanceMarker + ".tmp"

// PrepareRestoreLayout sets an in-memory identity so the ordinary OpenLayout
// can create the media directories behind a durable restore marker. It does
// not create .musiclib-store: that is installed only after the originals are
// verified and the dump's settings.store_id has been checked (§11.4).
func (v *Volume) PrepareRestoreLayout(id uuid.UUID) error {
	if id == uuid.Nil || v.storeID != uuid.Nil {
		return newErr(CodeIO, "invalid restore identity state", nil)
	}
	v.storeID = id
	return v.OpenLayout()
}

// CompleteRestoreIdentity installs the no-replace identity marker after the
// restored catalog and originals agree with the manifest. The restore marker
// prevents server boot throughout this interval.
func (v *Volume) CompleteRestoreIdentity(id uuid.UUID) error {
	if v.storeID != id || id == uuid.Nil {
		return newErr(CodeStoreMismatch, "restored identity does not match", nil)
	}
	return v.writeStoreMarker(id)
}

// BeginMaintenance durably creates the marker before any destructive step of
// rebuild or restore (DESIGN.md §11.3–§11.4). Repeating the *same* operation
// after an interruption is allowed; another operation or store is refused.
// The caller must hold the volume lock and validate the database store id.
func (v *Volume) BeginMaintenance(op string, id uuid.UUID) error {
	m := Maintenance{Operation: op, StoreID: id}
	if _, err := ParseMaintenance(m.Encode()); err != nil {
		return newErr(CodeMaintenanceMalformed, "invalid maintenance operation or store id", err)
	}
	b, found, err := readMarker(v.root, MaintenanceMarker, CodeMaintenanceMalformed)
	if err != nil {
		return err
	}
	if found {
		if !bytes.Equal(b, m.Encode()) {
			return newErr(CodeMaintenance, "a different maintenance operation is incomplete; inspect "+MaintenanceMarker+" before continuing", nil)
		}
		if err := v.root.SyncDir(""); err != nil {
			return fsErr("cannot sync the existing maintenance marker", err)
		}
		return nil
	}
	// Only our own temporary name is removed. A crash while writing the
	// temporary never makes it a valid marker and cannot authorize boot.
	if err := v.root.Remove(maintenanceTemp); err != nil && fsops.Code(err) != fsops.CodeNotFound {
		return fsErr("cannot remove leftover maintenance temporary", err)
	}
	f, err := v.root.CreateExclusive(maintenanceTemp, 0o444)
	if err != nil {
		return fsErr("cannot create maintenance temporary", err)
	}
	if _, err := f.Write(m.Encode()); err != nil {
		return fsErr("cannot write maintenance temporary", errors.Join(err, f.Close()))
	}
	if err := fsops.SyncAndClose(f); err != nil {
		return fsErr("cannot sync maintenance temporary", err)
	}
	if err := v.failpoints.Hit("maintenance_temp_synced"); err != nil {
		return err
	}
	if err := fsops.RenameNoReplace(v.root, maintenanceTemp, v.root, MaintenanceMarker); err != nil {
		return fsErr("cannot install maintenance marker", err)
	}

	if err := v.failpoints.Hit("maintenance_renamed"); err != nil {
		return err
	}
	if err := v.root.SyncDir(""); err != nil {
		return fsErr("cannot sync maintenance marker", err)
	}
	return v.failpoints.Hit("maintenance_synced")
}

// EndMaintenance removes only the marker for this operation and store and
// syncs its parent. Never clear a malformed or foreign marker automatically.
func (v *Volume) EndMaintenance(op string, id uuid.UUID) error {
	b, found, err := readMarker(v.root, MaintenanceMarker, CodeMaintenanceMalformed)
	if err != nil {
		return err
	}
	if !found || !bytes.Equal(b, (Maintenance{Operation: op, StoreID: id}).Encode()) {
		return newErr(CodeMaintenance, "maintenance marker missing or belongs to another operation; it was not removed", nil)
	}
	if err := v.root.Remove(MaintenanceMarker); err != nil {
		return fsErr("cannot remove maintenance marker", err)
	}
	if err := v.failpoints.Hit("maintenance_removed"); err != nil {
		return err
	}
	if err := v.root.SyncDir(""); err != nil {
		return fsErr("cannot sync maintenance marker removal", err)
	}
	return nil
}
