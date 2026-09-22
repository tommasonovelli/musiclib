package volume

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/fsops"
	"musiclib/internal/store"
)

// Identify pairs the volume with the database (DESIGN.md §2.2, §11.1) and
// returns the store id. The volume marker /data/.musiclib-store and
// settings.store_id must name the same store:
//
//	marker   store_id   media      catalog     outcome
//	id A     A          any        any         paired: return A
//	id A     B          any        any         CodeStoreMismatch
//	id A     none       any        any         CodeDBUninitialized
//	none     any/none   empty      empty       first initialization (or its completion)
//	none     any/none   empty      not empty   CodeMarkerMissing
//	none     A          not empty  any         CodeMarkerMissing
//	none     none       not empty  any         CodeNotEmpty
//	malformed                                  CodeMarkerMalformed
//
// "catalog" is store.CatalogIsEmpty: no blob, artist, album, import batch or
// job. Completing a missing marker is meant only for an interrupted first
// boot (§11.1), and after one the catalog is still empty; a database with
// content belongs to another volume (§2.2, N-069).
//
// A refused case never writes anything, to the volume or to the database: a
// marker that disagrees is never rewritten, a database is never paired with a
// volume that holds media, and an empty volume is never paired with a
// database that has catalog content.
//
// First initialization, in this order, is repeatable after an interruption
// at any point (N-061 lists the crash windows):
//
//  1. check that the media storage is empty ([Volume.mediaContent]) and
//     that the database has no catalog content (store.CatalogIsEmpty);
//  2. in one transaction, insert a new settings.store_id unless one exists,
//     and read back the one that is there (store.InsertStoreID is
//     idempotent);
//  3. write the marker durably and without replacing anything: a temporary
//     file, fsync, renameat2(RENAME_NOREPLACE) to the marker's name, fsync
//     of /data. The rename is atomic, so the marker is either absent or
//     complete.
//
// The database comes first, so an interruption leaves at worst a store_id
// with no marker on an empty volume, which step 1 then allows to complete.
// The opposite order could leave a marker with no store_id, which is
// indistinguishable from a foreign volume and must be refused.
func (v *Volume) Identify(ctx context.Context, pool *pgxpool.Pool) (uuid.UUID, error) {
	markerID, markerFound, err := v.readStoreMarker()
	if err != nil {
		return uuid.Nil, err
	}
	dbID, dbFound, err := getStoreID(ctx, pool)
	if err != nil {
		return uuid.Nil, err
	}
	switch {
	case markerFound && dbFound && markerID == dbID:
		v.storeID = dbID
		return dbID, nil
	case markerFound && dbFound:
		return uuid.Nil, newErr(CodeStoreMismatch, "the volume belongs to store "+markerID.String()+
			" but the database to store "+dbID.String()+
			": wrong volume or wrong database; nothing was changed (DESIGN.md §2.2)", nil)
	case markerFound:
		return uuid.Nil, newErr(CodeDBUninitialized, "the volume belongs to store "+markerID.String()+
			" but the database has no store id: a new or reset database is never paired with an existing volume;"+
			" restore the database from the backup (DESIGN.md §11.4)", nil)
	}

	// No marker. Completing it is allowed only for an interrupted first
	// boot: empty media storage *and* a database with no catalog content
	// (N-062, N-069). Anything else is a wrong pairing (§2.2).
	media, err := v.mediaContent()
	if err != nil {
		return uuid.Nil, err
	}
	if media != "" && !dbFound {
		return uuid.Nil, newErr(CodeNotEmpty, "neither the volume nor the database has a store id, but the "+
			"media storage is not empty ("+media+"): this is not a new installation (DESIGN.md §11.1)", nil)
	}
	if media != "" {
		return uuid.Nil, newErr(CodeMarkerMissing, "the database belongs to store "+dbID.String()+
			" but the volume has no "+StoreMarker+" and its media storage is not empty ("+media+
			"); the marker is completed automatically only on empty media storage (DESIGN.md §11.1)", nil)
	}
	empty, err := store.New(pool).CatalogIsEmpty(ctx)
	if err != nil {
		return uuid.Nil, newErr(CodeDB, "cannot check whether the catalog is empty", err)
	}
	if !empty {
		return uuid.Nil, newErr(CodeMarkerMissing, "the volume has no "+StoreMarker+
			" but the database already has catalog content: it belongs to another volume, "+
			"and an empty volume is never paired with it; mount the right /data (DESIGN.md §2.2, §11.1)", nil)
	}
	if err := v.fail("media_checked"); err != nil {
		return uuid.Nil, err
	}
	id, err := v.initStoreID(ctx, pool)
	if err != nil {
		return uuid.Nil, err
	}
	if err := v.fail("db_committed"); err != nil {
		return uuid.Nil, err
	}
	if err := v.writeStoreMarker(id); err != nil {
		return uuid.Nil, err
	}
	v.storeID = id
	return id, nil
}

// readStoreMarker reads and parses /data/.musiclib-store.
func (v *Volume) readStoreMarker() (uuid.UUID, bool, error) {
	b, found, err := readMarker(v.root, StoreMarker, CodeMarkerMalformed)
	if err != nil || !found {
		return uuid.Nil, found, err
	}
	id, err := ParseStoreMarker(b)
	if err != nil {
		return uuid.Nil, true, newErr(CodeMarkerMalformed, StoreMarker+
			" cannot be parsed; it is never rewritten automatically: inspect it", err)
	}
	return id, true, nil
}

// getStoreID reads settings.store_id; found is false when there is no row.
func getStoreID(ctx context.Context, pool *pgxpool.Pool) (uuid.UUID, bool, error) {
	id, err := store.New(pool).GetStoreID(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, false, nil
	case err != nil:
		return uuid.Nil, false, newErr(CodeDB, "cannot read settings.store_id", err)
	}
	return id, true, nil
}

// initStoreID creates settings.store_id if it is missing and returns the
// one in the database, in one transaction (§11.1). A concurrent or earlier
// initialization wins: its id is returned.
func (v *Volume) initStoreID(ctx context.Context, pool *pgxpool.Pool) (_ uuid.UUID, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, newErr(CodeDB, "cannot begin the store id transaction", err)
	}
	defer func() {
		if err == nil {
			return
		}
		// The rollback of a transaction that failed or was never committed;
		// after a successful Commit it reports ErrTxClosed, which is fine.
		if rerr := tx.Rollback(context.WithoutCancel(ctx)); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
			err = errors.Join(err, newErr(CodeDB, "cannot roll back the store id transaction", rerr))
		}
	}()
	q := store.New(tx)
	if _, err := q.InsertStoreID(ctx, store.NewID()); err != nil {
		return uuid.Nil, newErr(CodeDB, "cannot insert settings.store_id", err)
	}
	if err := v.fail("db_inserted"); err != nil {
		return uuid.Nil, err
	}
	id, err := q.GetStoreID(ctx)
	if err != nil {
		return uuid.Nil, newErr(CodeDB, "cannot read back settings.store_id", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, newErr(CodeDB, "cannot commit settings.store_id", err)
	}
	return id, nil
}

// mediaContent describes the first media found, or returns "" when each of
// originals/, library/ and work/ is either absent or an empty directory
// (N-062). Anything else at those names, a symlink or a file included,
// counts as media. Other entries at the top of /data (lost+found on a
// dedicated filesystem, the lock file) are not media and are not looked at.
func (v *Volume) mediaContent() (string, error) {
	for _, d := range mediaDirs {
		info, err := v.root.Stat(d)
		switch {
		case fsops.Code(err) == fsops.CodeNotFound:
			continue
		case err != nil:
			return "", fsErr("cannot inspect "+d, err)
		case info.Type != fsops.TypeDir:
			return d + " is a " + info.Type.String() + ", not a directory", nil
		}
		entries, err := v.root.ReadDir(d)
		if err != nil {
			return "", fsErr("cannot list "+d, err)
		}
		if len(entries) > 0 {
			return d + " contains " + entries[0].Name, nil
		}
	}
	return "", nil
}

// writeStoreMarker writes /data/.musiclib-store durably without replacing
// anything (§11.1). A temporary left by an interrupted attempt is removed
// first: nothing else creates that name. On failure the temporary is left
// for the next attempt to remove, exactly as after a crash.
func (v *Volume) writeStoreMarker(id uuid.UUID) error {
	if err := v.root.Remove(storeMarkerTemp); err != nil && fsops.Code(err) != fsops.CodeNotFound {
		return fsErr("cannot remove the leftover "+storeMarkerTemp, err)
	}
	f, err := v.root.CreateExclusive(storeMarkerTemp, markerPerm)
	if err != nil {
		return fsErr("cannot create "+storeMarkerTemp, err)
	}
	// (*os.File).Write returns an error whenever it writes less than asked.
	if _, err := f.Write(EncodeStoreMarker(id)); err != nil {
		return fsErr("cannot write "+storeMarkerTemp, errors.Join(err, f.Close()))
	}
	if err := fsops.SyncAndClose(f); err != nil {
		return fsErr("cannot make "+storeMarkerTemp+" durable", err)
	}
	if err := v.fail("marker_temp_synced"); err != nil {
		return err
	}
	if err := fsops.RenameNoReplace(v.root, storeMarkerTemp, v.root, StoreMarker); err != nil {
		return fsErr("cannot install "+StoreMarker, err)
	}
	if err := v.fail("marker_renamed"); err != nil {
		return err
	}
	if err := v.root.SyncDir(""); err != nil {
		return fsErr("cannot make "+StoreMarker+" durable", err)
	}
	return v.fail("marker_synced")
}
