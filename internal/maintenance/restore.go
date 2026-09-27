package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
	"musiclib/internal/media"
	"musiclib/internal/store"
	"musiclib/internal/volume"
)

// Restore refuses an existing installation, verifies the complete archive,
// then creates the marker before any catalog or media write. On interruption
// never resume on the partially restored destinations: replace both with new
// empty ones, and run the command again (DESIGN.md §11.4).
func Restore(ctx context.Context, db *pgxpool.Pool, v *volume.Volume, from, dbURL string, hook failpoint.Hook) (result error) {
	entries, err := v.Root().ReadDir("")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name == volume.LockFile {
			continue
		}
		if entry.Name == "lost+found" && entry.Type == fsops.TypeDir {
			contents, err := v.Root().ReadDir("lost+found")
			if err != nil {
				return err
			}
			if len(contents) == 0 {
				continue
			}
		}
		return refuse("restore_volume_not_empty", "replace the data volume and database with new empty destinations before retrying")
	}
	n, err := store.New(db).RestoreDatabaseObjects(ctx)
	if err != nil {
		return fail("restore_database_check", "cannot inspect the empty database", err)
	}
	if n != 0 {
		return refuse("restore_database_not_empty", "replace the data volume and database with new empty destinations before retrying")
	}
	parent, name, err := backupDestination(from, v)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, parent.Close()) }()
	archive, err := parent.SubRoot(name)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, archive.Close()) }()
	m, err := verifyBackup(ctx, archive)
	if err != nil {
		var coded *Error
		if errors.As(err, &coded) {
			return err
		}
		return &Error{Code: "restore_archive_invalid", Message: "backup verification failed; nothing was written", Refusal: true, Err: err}
	}
	latest, err := store.LatestSchemaVersion()
	if err != nil {
		return fail("restore_schema", "cannot read embedded schema", err)
	}
	if m.SchemaVersion > latest {
		return refuse("restore_schema_too_new", "this binary does not support the archive schema")
	}
	if m.SchemaVersion < 1 {
		return refuse("restore_schema_unsupported", "this binary cannot restore this archive")
	}
	uri, password, err := pgConnection(dbURL)
	if err != nil {
		return err
	}
	if err := hook.Hit("restore_verified"); err != nil {
		return err
	}
	if err := v.BeginMaintenance(volume.OpRestore, m.StoreID); err != nil {
		return err
	}
	if err := hook.Hit("restore_marker"); err != nil {
		return err
	}
	dump, err := archive.Open(backupDump)
	if err != nil {
		return err
	}
	runner := media.NewRunner(1)
	runResult, runErr := runner.Run(ctx, media.Command{Path: "/usr/lib/postgresql/17/bin/pg_restore", Args: []string{"--exit-on-error", "--single-transaction", "--no-owner", "--no-acl", "--dbname=" + uri, "/proc/self/fd/3"}, Env: []string{"PGPASSWORD=" + password}, Files: []*os.File{dump}, Timeout: 2 * time.Hour})
	closeErr := dump.Close()
	if runErr != nil {
		return errors.Join(fail("restore_dump_failed", "pg_restore failed: "+safeToolStderr(runResult.Stderr, password), runErr), closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if err := hook.Hit("restore_dump"); err != nil {
		return err
	}
	// Supported forward-only migrations run before any sqlc query reads the
	// restored catalog, so every query sees the schema it was generated for
	// (NOTES.md N-225, N-230 D4). store.Migrate refuses a newer schema.
	if err := store.Migrate(ctx, db); err != nil {
		return fail("restore_migrate", "cannot apply forward migrations to the restored catalog", err)
	}
	id, err := store.New(db).GetStoreID(ctx)
	if err != nil || id != m.StoreID {
		return fail("restore_store_id", "restored settings.store_id differs from manifest", err)
	}
	// A canonical manifest can still omit a catalog-referenced original if
	// the archive was assembled incorrectly. Refuse before installing media
	// and never clear the marker on a damaged catalog/manifest pair.
	catalogBlobs, err := store.New(db).DoctorBlobs(ctx)
	if err != nil {
		return err
	}
	inventory := make(map[string]int64, len(m.Blobs))
	for _, b := range m.Blobs {
		inventory[b.SHA256] = b.Size
	}
	for _, b := range catalogBlobs {
		if size, ok := inventory[b.Hash]; ok && size != b.Size {
			return fail("restore_catalog_blob_size", "catalog size differs for "+b.Hash, nil)
		} else if b.Referenced != nil && *b.Referenced && !ok {
			return fail("restore_catalog_blob_missing", "referenced original is absent: "+b.Hash, nil)
		}
	}
	if err := v.PrepareRestoreLayout(id); err != nil {
		return err
	}
	if err := v.CheckFilesystem(); err != nil {
		return err
	}
	bs, err := blobstore.New(v.Originals(), v.Work())
	if err != nil {
		return err
	}
	for _, b := range m.Blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := archive.Open(backupBlobs + "/" + b.SHA256[:2] + "/" + b.SHA256[2:4] + "/" + b.SHA256)
		if err != nil {
			return err
		}
		put, putErr := bs.Put(ctx, f)
		closeErr := f.Close()
		if err := errors.Join(putErr, closeErr); err != nil {
			return err
		}
		if put.SHA256 != b.SHA256 || put.Size != b.Size {
			return fail("restore_blob_mismatch", "backup blob changed while restoring", nil)
		}
		if err := hook.Hit("restore_blob"); err != nil {
			return err
		}
	}
	if err := hook.Hit("restore_originals"); err != nil {
		return err
	}
	if err := v.Work().RemoveAll(ctx, "blobs"); err != nil {
		return err
	}
	if err := v.Work().SyncDir(""); err != nil {
		return err
	}
	if err := v.CompleteRestoreIdentity(id); err != nil {
		return err
	}
	if err := hook.Hit("restore_before_reset"); err != nil {
		return err
	}
	if err := catalog.ResetDerivedForRestore(ctx, db); err != nil {
		return err
	}
	if err := hook.Hit("restore_after_reset"); err != nil {
		return err
	}
	return v.EndMaintenance(volume.OpRestore, id)
}

// verifyBackup checks the dump, the complete inventory, and every hash before
// any destination changes. Strict canonical JSON rejects duplicate fields,
// unsorted/duplicate hashes and unknown fields without a second parser.
func verifyBackup(ctx context.Context, archive *fsops.Root) (Manifest, error) {
	var m Manifest
	fi, err := archive.Stat(backupManifest)
	if err != nil {
		return m, err
	}
	if fi.Type != fsops.TypeRegular || fi.Size > 16<<20 {
		return m, refuse("restore_manifest_invalid", "not a bounded regular manifest; nothing was written")
	}
	f, err := archive.Open(backupManifest)
	if err != nil {
		return m, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, refuse("restore_manifest_invalid", "invalid backup manifest; nothing was written")
	}
	canonical, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, err
	}
	if !bytes.Equal(raw, append(canonical, '\n')) || m.StoreID == uuid.Nil || m.AppVersion == "" || m.RenderVersion == "" || blobstore.ValidateSHA(m.DumpSHA256) != nil || m.Blobs == nil {
		return m, refuse("restore_manifest_invalid", "invalid or noncanonical manifest; nothing was written")
	}
	for i, b := range m.Blobs {
		if blobstore.ValidateSHA(b.SHA256) != nil || b.Size < 0 || i > 0 && m.Blobs[i-1].SHA256 >= b.SHA256 {
			return m, refuse("restore_manifest_invalid", "invalid blob inventory; nothing was written")
		}
	}
	sum, _, err := hashFile(ctx, archive, backupDump)
	if err != nil || sum != m.DumpSHA256 {
		return m, refuse("restore_dump_hash", "dump does not match manifest; nothing was written")
	}
	want := make(map[string]int64, len(m.Blobs))
	for _, b := range m.Blobs {
		want[b.SHA256] = b.Size
	}
	if err := verifyBackupTree(ctx, archive, backupBlobs, want); err != nil {
		return m, err
	}
	if len(want) != 0 {
		return m, refuse("restore_blob_missing", "manifest lists absent blobs; nothing was written")
	}
	top, err := archive.ReadDir("")
	if err != nil {
		return m, err
	}
	if len(top) != 3 || top[0].Name != backupDump || top[1].Name != backupManifest || top[2].Name != backupBlobs {
		return m, refuse("restore_archive_extra", "unexpected archive entry; nothing was written")
	}
	return m, nil
}

func verifyBackupTree(ctx context.Context, root *fsops.Root, dir string, want map[string]int64) error {
	entries, err := root.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := dir + "/" + e.Name
		depth := strings.Count(rel, "/") - 1
		if depth < 2 && e.Type == fsops.TypeDir {
			if len(e.Name) != 2 || !isHex(e.Name) {
				return refuse("restore_archive_extra", "invalid shard "+rel+"; nothing was written")
			}
			if err := verifyBackupTree(ctx, root, rel, want); err != nil {
				return err
			}
			continue
		}
		if depth != 2 || e.Type != fsops.TypeRegular || blobstore.ValidateSHA(e.Name) != nil || !strings.HasPrefix(e.Name, strings.ReplaceAll(strings.TrimPrefix(dir, backupBlobs+"/"), "/", "")) {
			return refuse("restore_archive_extra", "unsafe entry "+rel+"; nothing was written")
		}
		size, ok := want[e.Name]
		if !ok {
			return refuse("restore_archive_extra", "unlisted original "+e.Name+"; nothing was written")
		}
		sum, n, err := hashFile(ctx, root, rel)
		if err != nil || sum != e.Name || n != size {
			return refuse("restore_blob_hash", "damaged original "+e.Name+"; nothing was written")
		}
		delete(want, e.Name)
	}
	return nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
