package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/names"
	"musiclib/internal/store"
)

// artistRef is the outcome of resolveArtist: an existing artist, or the
// row to create.
type artistRef struct {
	ID        uuid.UUID
	Name      string
	FolderKey string
	Exists    bool
}

// SameArtistName is §7.6's identity of artist names: equal after NFC, trim
// and casefold. name and other are already normalized texts (NFC, trimmed),
// so what is left is the comparison key. The importer uses it for the "one
// artist of the tracks" of §7.3 (NOTES.md N-121).
func SameArtistName(name, other string) bool {
	return names.Key(name) == names.Key(other)
}

// resolveArtist is §7.6's rule for a normalized artist name. The folder_key
// is the artists' unique identity on disk, so the lookup is by folder:
//   - no artist has the folder: a new artist;
//   - the artist of the folder has the same name after NFC, trim and
//     casefold: that artist, keeping its own spelling;
//   - it has a different name, which collides only through the path
//     sanitization (for example "AC/DC" and "AC_DC"):
//     CodeArtistFolderConflict with both names; artists are never merged
//     implicitly.
//
// It only reads; the caller creates the new artist.
func resolveArtist(ctx context.Context, tx *store.CatalogTx, name string) (artistRef, error) {
	key := names.FolderKey(name)
	a, err := tx.GetArtistByFolderKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return artistRef{ID: store.NewID(), Name: name, FolderKey: key}, nil
	}
	if err != nil {
		return artistRef{}, dbErr("looking up the artist folder "+key, err)
	}
	if !SameArtistName(name, a.Name) {
		return artistRef{}, folderConflict(name, a)
	}
	return artistRef{ID: a.ID, Name: a.Name, FolderKey: a.FolderKey, Exists: true}, nil
}

func folderConflict(name string, existing store.Artist) *Error {
	return &Error{
		Code: CodeArtistFolderConflict,
		Message: fmt.Sprintf("the artist %q would share the folder of the existing artist %q: use a different name",
			name, existing.Name),
		Details: Details{ArtistID: existing.ID, Names: []string{name, existing.Name}},
	}
}

// CreateArtist creates an artist at revision 1 (§10.2 POST /api/artists),
// in a catalog transaction. The name is a required metadata text (§5.2).
// Artists are identified as at import (§7.6): if an artist with the same
// name after NFC, trim and casefold exists, the result is
// CodeArtistExists; if a different name would share its folder through the
// path sanitization only, CodeArtistFolderConflict. Nothing is merged or
// renamed. In both conflicts the existing artist is returned together with
// the error, read in the same transaction, so that the caller can offer it
// (§10.2: "conflitto restituisce anche l'artista esistente"). A new artist
// has no album and changes no output: nothing is enqueued. It is an API
// client's explicit request, so it stays without albums until one arrives
// (NOTES.md N-299); the UI never calls it, it names a new artist in the
// album update instead (N-298).
func (s *Service) CreateArtist(ctx context.Context, name string) (Artist, error) {
	name, err := names.NormalizeRequiredText(name)
	if err != nil {
		return Artist{}, textError("artist name", err)
	}
	var out Artist
	err = store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		var err error
		out, err = insertArtist(ctx, tx, name)
		return err
	})
	if err != nil {
		if c := Code(err); c == CodeArtistExists || c == CodeArtistFolderConflict {
			return out, err
		}
		return Artist{}, err
	}
	return out, nil
}

// insertArtist creates the artist name, already normalized, at revision 1
// in the caller's transaction: the rule of CreateArtist, shared with the
// album update that names a new artist (NOTES.md N-298). On a conflict it
// returns the existing artist together with the error.
func insertArtist(ctx context.Context, tx *store.CatalogTx, name string) (Artist, error) {
	key := names.FolderKey(name)
	existing, err := tx.GetArtistByFolderKey(ctx, key)
	if err == nil {
		out := Artist{ID: existing.ID, Name: existing.Name, Revision: existing.Revision}
		if !SameArtistName(name, existing.Name) {
			return out, folderConflict(name, existing)
		}
		return out, &Error{
			Code:    CodeArtistExists,
			Message: fmt.Sprintf("the artist %q already exists as %q", name, existing.Name),
			Details: Details{ArtistID: existing.ID, Names: []string{name, existing.Name}},
		}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Artist{}, dbErr("looking up the artist folder "+key, err)
	}
	id := store.NewID()
	if err := tx.InsertArtist(ctx, store.InsertArtistParams{ID: id, Name: name, FolderKey: key}); err != nil {
		return Artist{}, dbErr("creating artist "+id.String(), err)
	}
	return Artist{ID: id, Name: name, Revision: 1}, nil
}

// leaveArtist is the owner's rule N-297 (2026-09-28), applied by the one
// change that takes an album away from an artist (the album update, §10.2):
// an artist left without any album, active or trashed, is deleted in the
// same transaction. It runs under the catalog lock like every catalog
// mutation (§5.3), so an album arriving at the artist in another
// transaction is either already committed (the artist stays) or waits for
// this one (and then finds no artist: CodeArtistNotFound). An artist that
// still has an album, in the trash included, is kept. The artist's folder
// in library/ follows its albums' renders: the publisher removes it only
// when it is empty (§9.3).
func leaveArtist(ctx context.Context, tx *store.CatalogTx, artistID uuid.UUID) error {
	if _, err := tx.DeleteOrphanArtist(ctx, artistID); err != nil {
		return dbErr("deleting artist "+artistID.String()+" left without albums", err)
	}
	return nil
}

// RenameArtist renames an artist (§4.3, §10.2 PUT /api/artists/{id}):
// ifMatch is the artist revision seen by the client (0: none, §10.1).
// Saving the current name is a no-op without a new revision. Otherwise the
// artist's revision and the revision of every one of its albums, trashed
// ones included, are bumped, every album's claims are re-derived and its
// render enqueued, in one transaction: a path reserved by another album
// refuses the whole rename (§5.3). A name whose folder belongs to another
// artist is CodeArtistFolderConflict: merges are never implicit (§5.3).
//
// It returns the artist's revision and whether anything changed.
func (s *Service) RenameArtist(ctx context.Context, artistID uuid.UUID, ifMatch int64, name string) (int64, bool, error) {
	if ifMatch == 0 {
		return 0, false, checkRevision("artist", artistID, 0, 0)
	}
	name, err := names.NormalizeRequiredText(name)
	if err != nil {
		return 0, false, textError("artist name", err)
	}
	var (
		revision int64
		changed  bool
	)
	err = store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		revision, changed = 0, false
		a, err := tx.GetArtist(ctx, artistID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(CodeArtistNotFound, "artist %s does not exist", artistID)
		}
		if err != nil {
			return dbErr("reading artist "+artistID.String(), err)
		}
		if err := checkRevision("artist", artistID, ifMatch, a.Revision); err != nil {
			return err
		}
		if name == a.Name {
			revision = a.Revision
			return nil
		}
		key := names.FolderKey(name)
		if key != a.FolderKey {
			other, err := tx.GetArtistByFolderKey(ctx, key)
			if err == nil {
				return folderConflict(name, other)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return dbErr("looking up the artist folder "+key, err)
			}
		}
		revision, err = tx.RenameArtist(ctx, store.RenameArtistParams{ID: artistID, Name: name, FolderKey: key})
		if err != nil {
			return dbErr("renaming artist "+artistID.String(), err)
		}
		albums, err := tx.ListArtistAlbumIDs(ctx, artistID)
		if err != nil {
			return dbErr("listing the albums of artist "+artistID.String(), err)
		}
		for _, id := range albums {
			if _, err := outputChanged(ctx, tx, id, false); err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	if changed {
		s.notify()
	}
	return revision, changed, nil
}
