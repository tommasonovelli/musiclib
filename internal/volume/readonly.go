package volume

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/fsops"
)

// IdentifyExisting pairs an already initialized database and volume without
// creating either identity. Doctor and backup must never initialize a store.
func (v *Volume) IdentifyExisting(ctx context.Context, db *pgxpool.Pool) (uuid.UUID, error) {
	marker, found, err := v.readStoreMarker()
	if err != nil {
		return uuid.Nil, err
	}
	if !found {
		return uuid.Nil, newErr(CodeMarkerMissing, "the volume has no store identity; nothing was initialized", nil)
	}
	id, found, err := getStoreID(ctx, db)
	if err != nil {
		return uuid.Nil, err
	}
	if !found {
		return uuid.Nil, newErr(CodeDBUninitialized, "the database has no store identity; nothing was initialized", nil)
	}
	if id != marker {
		return uuid.Nil, newErr(CodeStoreMismatch, "the volume and database store identities differ; nothing was changed", nil)
	}
	v.storeID = id
	return id, nil
}

// OpenExistingLayout opens the three media roots without creating directories
// or probing the filesystem. It must follow IdentifyExisting.
func (v *Volume) OpenExistingLayout() error {
	if v.storeID == uuid.Nil || v.originals != nil {
		return newErr(CodeIO, "OpenExistingLayout requires an identified, unopened volume", nil)
	}
	roots := make([]*fsops.Root, 0, len(mediaDirs))
	for _, name := range mediaDirs {
		r, err := v.root.SubRoot(name)
		if err != nil {
			for _, opened := range roots {
				err = errors.Join(err, opened.Close())
			}
			return fsErr("cannot open existing "+name, err)
		}
		roots = append(roots, r)
	}
	v.originals, v.library, v.work = roots[0], roots[1], roots[2]
	return nil
}
