package maintenance

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/catalog"
	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
	"musiclib/internal/volume"
)

// Rebuild regenerates only derived output. The caller has taken the flock and
// identified both sides of the existing store. Explicit confirmation must
// match both identities before calling. Hook is nil in production.
func Rebuild(ctx context.Context, db *pgxpool.Pool, v *volume.Volume, confirmed uuid.UUID, hook failpoint.Hook) error {
	if confirmed == uuid.Nil || confirmed != v.StoreID() {
		return refuse("rebuild_store_id", "the confirmed store id does not match the database and volume")
	}
	if err := v.BeginMaintenance(volume.OpRebuild, confirmed); err != nil {
		return err
	}
	if err := hook.Hit("rebuild_marker"); err != nil {
		return err
	}
	for _, dir := range []string{volume.Library, volume.Work} {
		if err := checkDeletionTree(ctx, v.Root(), dir); err != nil {
			return fail("rebuild_unsafe_tree", "unsafe directory "+dir, err)
		}
		if err := v.Root().RemoveAll(ctx, dir); err != nil {
			return fail("rebuild_delete", "cannot remove "+dir, err)
		}
		if err := v.Root().SyncDir(""); err != nil {
			return err
		}
		if err := hook.Hit("rebuild_deleted_" + dir); err != nil {
			return err
		}
	}
	if err := hook.Hit("rebuild_before_transaction"); err != nil {
		return err
	}
	if err := catalog.ResetDerivedForRebuild(ctx, db); err != nil {
		return err
	}
	if err := hook.Hit("rebuild_after_transaction"); err != nil {
		return err
	}
	if err := v.OpenLayout(); err != nil {
		return err
	}
	if err := v.CheckFilesystem(); err != nil {
		return err
	}
	if err := hook.Hit("rebuild_before_marker_removal"); err != nil {
		return err
	}
	return v.EndMaintenance(volume.OpRebuild, confirmed)
}

// Do not enter an unexpected mount or a symlink at the top level. Nested
// mounts are disallowed by §3.1, including bind mounts with the same st_dev.
// Symlinks inside an ordinary directory are not followed by RemoveAll.
func checkDeletionTree(ctx context.Context, root *fsops.Root, rel string) (result error) {
	info, err := root.Stat(rel)
	if fsops.Code(err) == fsops.CodeNotFound {
		return nil // repeat after a partial deletion
	}
	if err != nil {
		return err
	}
	if info.Type != fsops.TypeDir {
		return fmt.Errorf("%s is not a directory", rel)
	}
	child, err := root.SubRoot(rel)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, child.Close()) }()
	same, err := fsops.SameMount(root, child)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("%s is on another mount", rel)
	}
	entries, err := child.ReadDir("")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Type == fsops.TypeDir {
			if err := checkDeletionTree(ctx, root, rel+"/"+e.Name); err != nil {
				return err
			}
		}
	}
	return nil
}
