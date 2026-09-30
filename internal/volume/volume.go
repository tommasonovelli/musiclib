// Package volume owns the data volume /data as a whole (DESIGN.md §2.2,
// §3.1, §11.1, §11.3): the exclusive lock that makes the application a
// single instance per volume, the maintenance marker that blocks the boot,
// the volume identity paired with settings.store_id, the media layout
// (originals/, library/, work/) and the boot checks of the filesystem.
//
// It is shared by the server (cmd/musiclibd) and, from Phase 6, by the
// maintenance subcommands, which take the same lock and read the same
// markers (NOTES.md N-060). Every disk access goes through internal/fsops
// and every query through internal/store.
//
// The intended order, which the server's boot follows (§11.1):
//
//	v, err := volume.Acquire("/data")      // step 1: flock /data/.lock
//	v.CheckMaintenance()                   // before any write, DB included
//	v.Identify(ctx, pool)                  // step 3: store_id <-> marker
//	v.OpenLayout()                         // originals/, library/, work/
//	v.CheckFilesystem()                    // st_dev, mounts, access, probe
//	...
//	v.Close()                              // roots, then the lock, last
package volume

import (
	"errors"
	"os"
	"strconv"

	"github.com/google/uuid"

	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
)

// Names at the top of /data (DESIGN.md §3.1, §11.3).
const (
	LockFile          = ".lock"
	StoreMarker       = ".musiclib-store"
	MaintenanceMarker = ".maintenance"

	Originals = "originals"
	Library   = "library"
	Work      = "work"

	// storeMarkerTemp is where the store marker is written before its
	// no-replace rename; only the first initialization creates or removes it.
	storeMarkerTemp = StoreMarker + ".tmp"
	dirPerm         = 0o755
	// markerPerm: the store marker is never rewritten by the application.
	markerPerm = 0o444
)

// mediaDirs are the media storage directories, in creation order.
var mediaDirs = [...]string{Originals, Library, Work}

// Volume is /data, locked by this process. It is not safe for concurrent
// use: the boot runs its steps in sequence, and afterwards only the roots it
// hands out ([Volume.Originals], ...) are shared, which are.
type Volume struct {
	root *fsops.Root
	lock *fsops.Lock

	storeID                  uuid.UUID   // set by Identify
	originals, library, work *fsops.Root // set by OpenLayout

	// failpoints is nil in production. The package's tests set it to kill
	// the process (a real crash) or return an error at the named points of
	// the first initialization (NOTES.md N-061, N-142).
	failpoints failpoint.Hook
}

// Acquire opens the data volume at path and takes the exclusive,
// non-blocking flock on its .lock file (§2.2, §11.1 step 1). The lock is
// held until [Volume.Close], which releases it after everything else.
//
// A lock held by another process yields [CodeLocked]: there is no waiting,
// because a second instance must not start (§2.2) and a maintenance command
// needs the application stopped (§11.3).
func Acquire(path string) (*Volume, error) {
	root, err := fsops.OpenRoot(path)
	if err != nil {
		return nil, acquireErr("cannot open the data volume", err)
	}
	lock, err := root.Lock(LockFile)
	if err != nil {
		return nil, errors.Join(acquireErr("cannot open the volume lock "+LockFile, err), root.Close())
	}
	return &Volume{root: root, lock: lock}, nil
}

// acquireErr maps a failure to open /data or its lock. A busy lock is
// [CodeLocked]; a permission problem or a read-only mount is
// [CodePermission], with what to check; anything else (a missing /data, a
// .lock that is not a regular file, an I/O error) is [CodeUnavailable].
func acquireErr(msg string, err error) *Error {
	switch fsops.Code(err) {
	case fsops.CodeLockBusy:
		return newErr(CodeLocked, "another musiclib process holds "+LockFile+
			": stop it first (one instance per volume)", err)
	case fsops.CodePermission, fsops.CodeReadOnly:
		return permissionErr(msg, err)
	default:
		return newErr(CodeUnavailable, msg, err)
	}
}

// permissionErr builds every [CodePermission] error of the package. The
// process cannot know the host folder behind /data, so the message names the
// uid and gid it runs as (the effective ids, which the access checks use) and
// the command that gives a host folder back to them.
func permissionErr(msg string, err error) *Error {
	uid, gid := strconv.Itoa(os.Geteuid()), strconv.Itoa(os.Getegid())
	return newErr(CodePermission, msg+": this process runs as uid "+uid+" and gid "+gid+
		" (MUSICLIB_UID, MUSICLIB_GID) and must be able to read and write all of /data, on a read-write mount;"+
		" if MUSICLIB_DATA is a host folder, run `sudo chown -R "+uid+":"+gid+" <that folder>` on the host", err)
}

// Root is the /data root itself.
func (v *Volume) Root() *fsops.Root { return v.root }

// StoreID is the identity established by [Volume.Identify]; uuid.Nil before.
func (v *Volume) StoreID() uuid.UUID { return v.storeID }

// Originals, Library and Work are the media roots opened by
// [Volume.OpenLayout]; nil before. They are closed by [Volume.Close].
func (v *Volume) Originals() *fsops.Root { return v.originals }
func (v *Volume) Library() *fsops.Root   { return v.library }
func (v *Volume) Work() *fsops.Root      { return v.work }

// CheckMaintenance fails if /data/.maintenance exists (§11.3): a rebuild or
// restore did not finish, and until it is repeated to completion nothing
// else may run on the volume. A parseable marker yields [CodeMaintenance]
// with the operation and store id; anything else at the name yields
// [CodeMaintenanceMalformed]. Both block the boot.
//
// It only reads. Creating and removing the marker belongs to the rebuild
// and restore subcommands.
func (v *Volume) CheckMaintenance() error {
	b, found, err := readMarker(v.root, MaintenanceMarker, CodeMaintenanceMalformed)
	if err != nil || !found {
		return err
	}
	m, err := ParseMaintenance(b)
	if err != nil {
		return newErr(CodeMaintenanceMalformed, MaintenanceMarker+
			" exists but cannot be parsed; a maintenance operation is incomplete: inspect it, "+
			"then repeat the rebuild or restore (DESIGN.md §11.3)", err)
	}
	return newErr(CodeMaintenance, "the "+m.Operation+" of store "+m.StoreID.String()+
		" is incomplete ("+MaintenanceMarker+" exists): repeat `musiclibd "+m.Operation+
		"` until it completes before starting the server (DESIGN.md §11.3)", nil)
}

// OpenLayout creates originals/, library/ and work/ if missing, makes them
// durable with an fsync of /data itself (N-037), and opens them as roots.
// It must follow a successful [Volume.Identify]: media directories are only
// created on a volume whose identity is established.
//
// The fsync of /data runs on every call, not only when a directory was
// created: after a crash between a mkdir and its fsync the directory exists
// but may not be durable yet.
func (v *Volume) OpenLayout() error {
	if v.storeID == uuid.Nil {
		return newErr(CodeIO, "OpenLayout called before Identify", nil)
	}
	if v.originals != nil {
		return newErr(CodeIO, "OpenLayout called twice", nil)
	}
	for _, d := range mediaDirs {
		if _, err := v.root.MkdirAll(d, dirPerm); err != nil {
			return fsErr("cannot create "+d, err)
		}
	}
	if err := v.failpoints.Hit("layout_created"); err != nil {
		return err
	}
	if err := v.root.SyncDir(""); err != nil {
		return fsErr("cannot make the media directories durable", err)
	}
	roots := make([]*fsops.Root, 0, len(mediaDirs))
	for _, d := range mediaDirs {
		r, err := v.root.SubRoot(d)
		if err != nil {
			for _, o := range roots {
				err = errors.Join(err, o.Close())
			}
			return fsErr("cannot open "+d, err)
		}
		roots = append(roots, r)
	}
	v.originals, v.library, v.work = roots[0], roots[1], roots[2]
	return nil
}

// Close closes the media roots and /data, then releases the lock, last:
// nothing opened through this volume may outlive the lock (§11.1). Every
// error is returned; calling it again is not an error.
//
// Closing a root does not stop an operation that already resolved its own
// descriptors (N-031): callers cancel their contexts and wait for their
// goroutines before calling Close.
func (v *Volume) Close() error {
	var errs []error
	for _, r := range []*fsops.Root{v.work, v.library, v.originals, v.root} {
		if r != nil {
			errs = append(errs, r.Close())
		}
	}
	errs = append(errs, v.lock.Close())
	return errors.Join(errs...)
}

// fsErr wraps an fsops failure, keeping permission problems recognizable.
func fsErr(msg string, err error) *Error {
	switch fsops.Code(err) {
	case fsops.CodePermission, fsops.CodeReadOnly:
		return permissionErr(msg, err)
	default:
		return newErr(CodeIO, msg, err)
	}
}
