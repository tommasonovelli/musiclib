// Package maintenance implements offline checks and maintenance of the
// musiclib store (DESIGN.md §11.3–§11.4). Its caller holds the volume flock.
package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/names"
	"musiclib/internal/render"
	"musiclib/internal/store"
	"musiclib/internal/volume"
)

// Finding is a read-only diagnostic; codes and advice are stable for scripts
// and operators. Entity is an album id, a blob hash or a relative path.
type Finding struct {
	Severity string
	Code     string
	Entity   string
	Advice   string
}

type Report struct{ Findings []Finding }

func (r *Report) add(severity, code, entity, advice string) {
	r.Findings = append(r.Findings, Finding{severity, code, entity, advice})
}

func (r Report) HasErrors() bool {
	for _, f := range r.Findings {
		if f.Severity == "error" {
			return true
		}
	}
	return false
}

// Doctor reads the catalog and the two derived filesystem inventories. It
// never creates directories, removes the journal or repairs an entry. The
// caller acquired the flock and verified the store identity before calling.
func Doctor(ctx context.Context, db *pgxpool.Pool, v *volume.Volume, deep bool) (Report, error) {
	var r Report
	q := store.New(db)
	issues, err := q.DoctorStructuralIssues(ctx)
	if err != nil {
		return r, fmt.Errorf("read structural inventory: %w", err)
	}
	for _, issue := range issues {
		r.add("error", issue.Code, issue.Entity, "Inspect the catalog and recover from a verified backup if needed.")
	}
	blobs, err := q.DoctorBlobs(ctx)
	if err != nil {
		return r, fmt.Errorf("read blob inventory: %w", err)
	}
	albums, err := q.DoctorAlbums(ctx)
	if err != nil {
		return r, fmt.Errorf("read album inventory: %w", err)
	}
	claims, err := q.DoctorClaims(ctx)
	if err != nil {
		return r, fmt.Errorf("read reservations: %w", err)
	}
	journal, err := q.DoctorPublication(ctx)
	if err != nil {
		return r, fmt.Errorf("read publication journal: %w", err)
	}
	if len(journal) != 0 {
		r.add("warning", "doctor_journal_pending", journal[0].AlbumID.String(), "Start the app to recover publication, or explicitly rebuild; doctor does not touch the journal.")
	}
	expectedClaims := make(map[string]uuid.UUID)
	known := make(map[uuid.UUID]bool, len(albums))
	output := make(map[string]bool)
	outputDirs := make(map[string]bool)
	transient := make(map[string]bool)
	for _, j := range journal {
		for _, p := range []*string{j.OldPath, j.NewPath} {
			if p != nil {
				transient[*p] = true
				markParents(outputDirs, *p+"/placeholder")
			}
		}
	}
	for _, a := range albums {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		known[a.ID] = true
		entity := a.ID.String()
		var desired *catalog.Path
		if a.Active {
			p := catalog.AlbumPath(a.ArtistName, a.Title)
			desired = &p
		}
		var published, newPath, oldPath string
		if a.PublishedPath != nil {
			published = *a.PublishedPath
		}
		for _, j := range journal {
			if j.AlbumID == a.ID {
				if j.OldPath != nil {
					oldPath = *j.OldPath
				}
				if j.NewPath != nil {
					newPath = *j.NewPath
				}
			}
		}
		for _, p := range catalog.ExpectedClaims(desired, published, newPath, oldPath) {
			claim(&r, expectedClaims, p.Key, a.ID)
		}
		if a.JobState != nil || a.Active && (a.PublishedRenderer == nil || *a.PublishedRenderer != render.Version || a.PublishedRevision != a.Revision) {
			r.add("info", "doctor_pending_work", entity, "Start the app to process pending render work; this is not damage.")
		}
		if a.PublishedPath == nil {
			continue
		}
		path := *a.PublishedPath
		if oldPath != "" || newPath != "" {
			// INSTALL may have moved the old output and installed the new one.
			// Neither is stable until recovery finalizes the journal.
			continue
		}
		segments, pathErr := names.SplitRelPath(path)
		if pathErr != nil || len(segments) != 2 {
			// A published path has the fixed artist/album shape. Renames
			// may legitimately leave an older path for an active album.

			r.add("error", "doctor_invalid_path", entity, "The catalog has an invalid published path; investigate before rebuilding.")
			continue
		}
		if a.PublishedBuild == nil || a.PublishedReceiptHash == nil {
			r.add("error", "doctor_published_state", entity, "Published columns are inconsistent; inspect the catalog.")
			continue
		}
		receiptPath := path + "/" + render.ReceiptName
		output[receiptPath] = true
		markParents(outputDirs, receiptPath)
		data, err := readReceipt(v.Library(), receiptPath)
		if err != nil {
			r.add("error", "doctor_receipt_unreadable", entity, "Restore the output by rendering or rebuilding: "+err.Error())
			continue
		}
		if render.ReceiptHash(data) != *a.PublishedReceiptHash {
			r.add("error", "doctor_receipt_hash", entity, "The receipt differs from the DB hash; render or rebuild the output.")
			continue
		}
		rec, err := render.ParseReceipt(data)
		if err != nil || rec.AlbumID != a.ID || rec.BuildID != *a.PublishedBuild || rec.AlbumRevision != a.PublishedRevision || (a.PublishedRenderer != nil && rec.RenderVersion != *a.PublishedRenderer) {
			r.add("error", "doctor_receipt_invalid", entity, "The receipt does not match the published album; render or rebuild the output.")
			continue
		}
		for _, file := range rec.Files {
			rel := path + "/" + file.RelativePath
			output[rel] = true
			markParents(outputDirs, rel)
			fi, err := v.Library().Stat(rel)
			switch {
			case err != nil && fsops.Code(err) != fsops.CodeNotFound:
				r.add("error", "doctor_unreadable", rel, "Inspect permissions and unsafe path components; doctor did not follow them.")
			case err != nil:
				r.add("error", "doctor_output_missing", rel, "Render or rebuild this album.")
			case fi.Type != fsops.TypeRegular:
				r.add("error", "doctor_output_type", rel, "Remove the unsafe entry manually, then render or rebuild; never follow symlinks.")
			case fi.Size != file.Size:
				r.add("error", "doctor_output_size", rel, "Render or rebuild this album.")
			case deep:
				sum, size, err := hashFile(ctx, v.Library(), rel)
				if err != nil || size != file.Size || sum != file.SHA256 {
					r.add("error", "doctor_output_hash", rel, "Render or rebuild this album.")
				}
			}
		}
	}
	for _, c := range claims {
		if !known[c.AlbumID] || pathKey(c.Path) != c.PathKey || expectedClaims[c.PathKey] != c.AlbumID {
			r.add("error", "doctor_claim_invalid", c.Path, "Reservations must match desired and published album paths; inspect the catalog.")
		}
		delete(expectedClaims, c.PathKey)
	}
	for key, id := range expectedClaims {
		r.add("error", "doctor_claim_missing", id.String()+":"+key, "Rebuild reservations with an explicit rebuild.")
	}
	bs := blobstore.OpenReadOnly(v.Originals())
	knownBlobs := make(map[string]bool, len(blobs)) // hash -> referenced
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		knownBlobs[b.Hash] = knownBlobReferenced(b.Referenced)
		if b.Referenced == nil || !*b.Referenced {
			r.add("info", "doctor_unreferenced_blob", b.Hash, "Keep this original; no automatic garbage collection is performed.")
		}
		if !knownBlobReferenced(b.Referenced) {
			continue // absence of an unreferenced blob is not catalog damage
		}
		if err := bs.Check(blobstore.Blob{SHA256: b.Hash, Size: b.Size}); err != nil {
			r.add("error", "doctor_blob_damaged", b.Hash, "Recover a good original from backup; rendering cannot repair an original.")
		} else if deep {
			n, err := bs.Verify(ctx, b.Hash)
			if err != nil || n != b.Size {
				r.add("error", "doctor_blob_hash", b.Hash, "Recover a good original from backup; rendering cannot repair an original.")
			}
		}
	}
	if err := walk(ctx, v.Originals(), "", func(rel string, t fsops.FileType) {
		parts := strings.Split(rel, "/")
		if t == fsops.TypeDir {
			return
		}
		if len(parts) != 3 || len(parts[0]) != 2 || len(parts[1]) != 2 || !strings.HasPrefix(parts[2], parts[0]+parts[1]) || blobstore.ValidateSHA(parts[2]) != nil || t != fsops.TypeRegular {
			r.add("warning", "doctor_originals_extra", rel, "Inspect this unexpected originals entry; doctor never deletes originals.")
		} else if _, inCatalog := knownBlobs[parts[2]]; !inCatalog {
			r.add("info", "doctor_unreferenced_blob", parts[2], "Keep this unreferenced original; no automatic garbage collection is performed.")
			if deep {
				if _, err := bs.Verify(ctx, parts[2]); err != nil {
					r.add("error", "doctor_blob_hash", parts[2], "Recover a good original from backup.")
				}
			}
		} else if deep && !knownBlobs[parts[2]] {
			if _, err := bs.Verify(ctx, parts[2]); err != nil {
				r.add("error", "doctor_blob_hash", parts[2], "Recover a good original from backup.")
			}
		}
	}, func(path string, err error) {
		r.add("error", "doctor_unreadable", "originals/"+path, "Inspect inaccessible entry: "+err.Error())
	}); err != nil {
		return r, err
	}
	if err := walk(ctx, v.Library(), "", func(rel string, t fsops.FileType) {
		for path := range transient {
			if rel == path || strings.HasPrefix(rel, path+"/") {
				return
			}
		}
		if t == fsops.TypeDir {
			if !outputDirs[rel] {
				r.add("warning", "doctor_output_extra", rel, "This output directory is not part of a published album; inspect it before rebuilding.")
			}
			return
		}
		if t.IsSpecial() {
			r.add("error", "doctor_output_type", rel, "Remove unsafe symlinks or special files manually; doctor never follows them.")
		} else if !output[rel] {
			r.add("warning", "doctor_output_extra", rel, "This output file is not in a DB-anchored receipt; inspect it before rebuilding.")
		}
	}, func(path string, err error) {
		r.add("error", "doctor_unreadable", "library/"+path, "Inspect inaccessible entry: "+err.Error())
	}); err != nil {
		return r, err
	}
	return r, ctx.Err()
}

func knownBlobReferenced(b *bool) bool { return b != nil && *b }

func markParents(dirs map[string]bool, path string) {
	for index := strings.LastIndexByte(path, '/'); index > 0; index = strings.LastIndexByte(path[:index], '/') {
		dirs[path[:index]] = true
	}
}

func claim(r *Report, want map[string]uuid.UUID, key string, id uuid.UUID) {
	if other, ok := want[key]; ok && other != id {
		r.add("error", "doctor_claim_collision", key, "Two albums desire this path; resolve the conflict before rebuild.")
	}
	want[key] = id
}

func pathKey(path string) string { return names.PathKey(strings.Split(path, "/")) }

func readReceipt(root *fsops.Root, rel string) (data []byte, err error) {
	fi, err := root.Stat(rel)
	if err != nil {
		return nil, err
	}
	if fi.Type != fsops.TypeRegular || fi.Size > render.MaxReceiptBytes {
		return nil, fmt.Errorf("not a bounded regular receipt")
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return io.ReadAll(io.LimitReader(f, render.MaxReceiptBytes+1))
}

func hashFile(ctx context.Context, root *fsops.Root, rel string) (sum string, size int64, err error) {
	f, err := root.Open(rel)
	if err != nil {
		return "", 0, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	h := sha256.New()
	buf := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", size, err
		}
		n, e := f.Read(buf)
		if n > 0 {
			if _, err := h.Write(buf[:n]); err != nil {
				return "", size, err
			}
			size += int64(n)
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", size, e
		}
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// walk never traverses a symlink and never opens a special file. Every
// descent is confined through fsops, including a planted intermediate link.
func walk(ctx context.Context, root *fsops.Root, dir string, visit func(string, fsops.FileType), unreadable func(string, error)) error {
	entries, err := root.ReadDir(dir)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		unreadable(dir, err)
		return nil
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := e.Name
		if dir != "" {
			rel = dir + "/" + e.Name
		}
		visit(rel, e.Type)
		if e.Type == fsops.TypeDir {
			if err := walk(ctx, root, rel, visit, unreadable); err != nil {
				return err
			}
		}
	}
	return nil
}
