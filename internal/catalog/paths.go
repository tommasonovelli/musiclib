package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/names"
	"musiclib/internal/store"
)

// Path is a relative library path with its claim key (§5.2, §5.3): Path is
// the exact name on disk, Key the comparison key that path_claims is unique
// on.
type Path struct {
	Path string
	Key  string
}

// AlbumPath is the desired path of an album, <artist>/<album> (§5.1), from
// the artist's name and the album's title. Its key is FolderKey(artist) +
// "/" + FolderKey(title), the two folder_key columns joined: the reservation
// of a folder and the uniqueness of its name use the same algorithm (§5.2).
// The planner builds the output under this path.
func AlbumPath(artistName, title string) Path {
	segs := []string{names.Segment(artistName), names.Segment(title)}
	return Path{Path: strings.Join(segs, "/"), Key: names.PathKey(segs)}
}

// exactPath is the claim of a path that already exists as such: the
// published path or a path of the journal, made of final segments. Its key
// is the key of each segment (§5.2).
func exactPath(p string) Path {
	return Path{Path: p, Key: names.PathKey(strings.Split(p, "/"))}
}

// claimSources are the inputs of an album's claims (§5.3): its desired path
// if active, the paths of an in-progress publication of the album, and its
// published path.
type claimSources struct {
	desired    *Path
	journalNew string
	journalOld string
	published  string
}

// union is §5.3's union: the desired path if the album is active, the
// published path, and old_path and new_path of an in-progress publication,
// on normalized keys. When two of them share a key, as case variants do,
// the path kept is the desired one, then the journal's, then the published
// one. The result is ordered by key.
func (c claimSources) union() []Path {
	var candidates []Path
	if c.desired != nil {
		candidates = append(candidates, *c.desired)
	}
	for _, p := range []string{c.journalNew, c.journalOld, c.published} {
		if p != "" {
			candidates = append(candidates, exactPath(p))
		}
	}
	seen := map[string]bool{}
	var out []Path
	for _, p := range candidates {
		if !seen[p.Key] {
			seen[p.Key] = true
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Path) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// ExpectedClaims exposes the same pure §5.3 union used by ReconcileClaims
// to read-only integrity checks; no SQL or filesystem access is involved.
func ExpectedClaims(desired *Path, published, journalNew, journalOld string) []Path {
	return (claimSources{desired: desired, published: published, journalNew: journalNew, journalOld: journalOld}).union()
}

// albumClaims derives the claims an album must hold from its current state
// in tx: the album row, its artist and the publication journal.
func albumClaims(ctx context.Context, tx *store.CatalogTx, albumID uuid.UUID) ([]Path, error) {
	st, err := tx.GetAlbumPathState(ctx, albumID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errorf(CodeAlbumNotFound, "album %s does not exist", albumID)
	}
	if err != nil {
		return nil, dbErr("reading the paths of album "+albumID.String(), err)
	}
	var src claimSources
	if st.Active {
		p := AlbumPath(st.ArtistName, st.Title)
		src.desired = &p
	}
	if st.PublishedPath != nil {
		src.published = *st.PublishedPath
	}
	pub, err := tx.GetPublication(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, dbErr("reading the publication journal", err)
	case pub.AlbumID == albumID:
		src.journalNew, src.journalOld = deref(pub.NewPath), deref(pub.OldPath)
	}
	return src.union(), nil
}

// ReconcileClaims makes the album's rows of path_claims exactly the union
// of §5.3 for its current state: it reserves the paths it lacks, updates
// the displayed path of the ones it keeps, and releases only those that are
// no longer in the union. A key owned by another album is CodePathReserved,
// naming the owner: nothing is stolen, and the caller's transaction rolls
// back (§5.3).
//
// It is the one implementation of the claims rule: every catalog mutation
// uses it, and so will PREPARE, FINALIZE and rebuild.
func ReconcileClaims(ctx context.Context, tx *store.CatalogTx, albumID uuid.UUID) error {
	want, err := albumClaims(ctx, tx, albumID)
	if err != nil {
		return err
	}
	if err := checkAvailable(ctx, tx, albumID, want); err != nil {
		return err
	}
	have, err := tx.ListAlbumClaims(ctx, albumID)
	if err != nil {
		return dbErr("listing the claims of album "+albumID.String(), err)
	}
	current := make(map[string]string, len(have))
	for _, c := range have {
		current[c.PathKey] = c.Path
	}
	for _, p := range want {
		if path, ok := current[p.Key]; ok && path == p.Path {
			continue
		}
		n, err := tx.UpsertClaim(ctx, store.UpsertClaimParams{PathKey: p.Key, Path: p.Path, AlbumID: albumID})
		if err != nil {
			return dbErr("reserving "+p.Path, err)
		}
		if n != 1 {
			// checkAvailable saw the key free or ours under the lock.
			return errorf(CodeDB, "the claim of %s changed under the catalog lock", p.Path)
		}
	}
	keep := make(map[string]bool, len(want))
	for _, p := range want {
		keep[p.Key] = true
	}
	for _, c := range have {
		if keep[c.PathKey] {
			continue
		}
		if _, err := tx.DeleteClaim(ctx, store.DeleteClaimParams{PathKey: c.PathKey, AlbumID: albumID}); err != nil {
			return dbErr("releasing "+c.Path, err)
		}
	}
	return nil
}

// checkAvailable returns CodePathReserved for the first of paths whose key
// another album owns.
func checkAvailable(ctx context.Context, tx *store.CatalogTx, albumID uuid.UUID, paths []Path) error {
	keys := make([]string, len(paths))
	for i, p := range paths {
		keys[i] = p.Key
	}
	owners, err := tx.GetClaims(ctx, keys)
	if err != nil {
		return dbErr("reading path claims", err)
	}
	for _, o := range owners {
		if o.AlbumID != albumID {
			return &Error{
				Code:    CodePathReserved,
				Message: fmt.Sprintf("the path %s is reserved by album %s", o.Path, o.AlbumID),
				Details: Details{AlbumID: o.AlbumID, Path: o.Path},
			}
		}
	}
	return nil
}

// claimsHeld reports whether the album owns every key of its union: the
// "validità delle prenotazioni" that PREPARE rechecks (§6.3).
func claimsHeld(ctx context.Context, tx *store.CatalogTx, albumID uuid.UUID) (bool, error) {
	want, err := albumClaims(ctx, tx, albumID)
	if err != nil {
		return false, err
	}
	have, err := tx.ListAlbumClaims(ctx, albumID)
	if err != nil {
		return false, dbErr("listing the claims of album "+albumID.String(), err)
	}
	owned := make(map[string]bool, len(have))
	for _, c := range have {
		owned[c.PathKey] = true
	}
	for _, p := range want {
		if !owned[p.Key] {
			return false, nil
		}
	}
	return true, nil
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
