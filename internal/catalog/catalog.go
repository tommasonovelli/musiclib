// Package catalog holds the domain transactions of DESIGN.md §4 and §5.3:
// the import commit (§7.6), the metadata changes with their revisions
// (§4.3), trash and restore, the artist rename, and the path reservations
// (§5.3). Every mutation runs in store.InCatalogTx, under the single catalog
// lock, and every output-changing mutation bumps the album's revision and
// enqueues its render (jobs.EnqueueRender) in the same transaction.
//
// The package knows no absolute path and touches no media: its inputs are
// already durable blobs and already read metadata (§7.5, §13.2). The HTTP
// layer, the importer and the publisher call it; they never write the
// catalog with SQL of their own (§13.2).
package catalog

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

// Service runs the catalog's transactions on one database.
type Service struct {
	db *pgxpool.Pool
	// wake, if not nil, is called after a commit that enqueued a render:
	// the pool's in-memory signal (§6.4), never needed for correctness.
	wake func()
	// coverFits is the owner's N-091 rule, asked wherever a cover is
	// chosen.
	coverFits CoverFits
}

// CoverFits reports whether cover, a JPEG or PNG blob within §8.5's limits,
// can be embedded as the single front cover (§8.2, §8.5) of an audio file
// of format audioFormat (flac, mp3, m4a-aac or m4a-alac): nil if it can, an
// error saying why otherwise.
//
// The owner's decision N-091: a cover must be embeddable in every audio
// format of its album, and that is checked when the cover is chosen (the
// import commit, and the cover endpoints of §10.2), never discovered by a
// render. The per-format limits belong to the tag adapter (a FLAC metadata
// block holds less than 16 MiB, N-091), not to the catalog: the catalog
// only asks the question, once per audio format of the album. A cover
// refused here stays an attachment (§7.4).
type CoverFits func(cover Blob, audioFormat string) error

// New returns the catalog service over db. wake is the worker pool's
// Pool.Wake, or nil where no pool runs (maintenance, tests). coverFits is
// required: without it no cover could be accepted safely.
func New(db *pgxpool.Pool, wake func(), coverFits CoverFits) (*Service, error) {
	if db == nil || coverFits == nil {
		return nil, errorf(CodeInvalidArgument, "the catalog needs a database and a cover check")
	}
	return &Service{db: db, wake: wake, coverFits: coverFits}, nil
}

func (s *Service) notify() {
	if s.wake != nil {
		s.wake()
	}
}

// Error codes of the catalog. They are stable and domain-level: the HTTP
// layer maps them to statuses (§10.1) and puts them in {code, message,
// details}; the importer stores them as a failed job's error_code.
const (
	// CodePreconditionRequired: a change of an existing resource without
	// the revision seen by the client (§10.1: 428).
	CodePreconditionRequired = "precondition_required"
	// CodePreconditionFailed: the revision seen is not the current one
	// (§10.1: 412). Details.Revision is the current revision.
	CodePreconditionFailed = "precondition_failed"

	CodeAlbumNotFound  = "album_not_found"
	CodeArtistNotFound = "artist_not_found"

	// CodePathReserved: the album's path is claimed by another album
	// (§5.3: 409). Details.AlbumID is the owner, Details.Path the path.
	CodePathReserved = "path_reserved"
	// CodeAlbumFolderConflict: another active album of the same artist has
	// the same folder (§4.2, §7.6: 409). Details.AlbumID is that album.
	CodeAlbumFolderConflict = "album_folder_conflict"
	// CodeArtistFolderConflict: a different artist name has the same
	// folder, through path sanitization only (§7.6: 409). Details.Names
	// has both names, Details.ArtistID the existing artist.
	CodeArtistFolderConflict = "artist_folder_conflict"
	// CodeArtistExists: an artist with the same name after NFC, trim and
	// casefold already exists (§7.6's identity; §10.2 POST /api/artists:
	// 409 with the existing artist). Details.ArtistID is that artist,
	// Details.Names the name asked for and the existing spelling.
	CodeArtistExists = "artist_exists"

	// The content errors (§10.1: 422).
	CodeInvalidFingerprint    = "invalid_fingerprint"
	CodeInvalidBlob           = "invalid_blob"
	CodeBlobMismatch          = "blob_mismatch"
	CodeInvalidBlobFormat     = "invalid_blob_format"
	CodeInvalidYear           = "invalid_year"
	CodeInvalidDisc           = "invalid_disc"
	CodeInvalidTrackNumber    = "invalid_track_number"
	CodeDuplicateTrackNumber  = "duplicate_track_number"
	CodeTrackListMismatch     = "track_list_mismatch"
	CodeNoTracks              = "no_tracks"
	CodeTooManyFiles          = "too_many_files"
	CodeDuplicateSource       = "duplicate_source_path"
	CodeAttachmentCollision   = "attachment_path_collision"
	CodeLyricsAssociation     = "lyrics_association"
	CodeInvalidCover          = "invalid_cover"
	CodeInvalidImportWarnings = "invalid_import_warnings"

	// CodeDB is an unexpected database failure; store.IsFatal tells
	// whether it is one of §6.4's.
	CodeDB = "catalog_db"
	// CodeInvalidArgument is a call that cannot be served as made, such as
	// a service built without its cover check.
	CodeInvalidArgument = "catalog_invalid_argument"
)

// Details are the structured parts of an error, for the API's details.
type Details struct {
	AlbumID  uuid.UUID
	ArtistID uuid.UUID
	Path     string
	Names    []string
	Revision int64
}

// Error is the package's typed error. Err keeps the cause: a names error
// for an invalid text or path, a store or pgx error for CodeDB.
type Error struct {
	Code    string
	Message string
	Details Details
	Err     error
}

func (e *Error) Error() string {
	msg := e.Code + ": " + e.Message
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// textError types an invalid metadata text or path: the names code is the
// catalog code (text_empty, text_too_long, path_absolute, ...), so that the
// API and the job report say exactly what is wrong.
func textError(field string, err error) *Error {
	return &Error{Code: names.Code(err), Message: field, Err: err}
}

func dbErr(op string, err error) error {
	return &Error{Code: CodeDB, Message: op, Err: err}
}

// Code returns the code of the first catalog *Error in err's tree,
// otherwise the jobs or store code, otherwise "". A fatal store error
// (store.IsFatal, §6.4) wins over the domain error it may wrap.
func Code(err error) string {
	if store.IsFatal(err) {
		return store.Code(err)
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return jobs.Code(err)
}

// AsError returns the catalog *Error in err's tree, if any. It does not
// look at fatality: test store.IsFatal first (or use Code).
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// checkRevision is the If-Match rule of §10.1, applied inside the
// transaction of the change: 0 is a missing precondition.
func checkRevision(kind string, id uuid.UUID, ifMatch, current int64) error {
	if ifMatch == 0 {
		return errorf(CodePreconditionRequired, "a change of %s %s requires the revision seen", kind, id)
	}
	if ifMatch < 0 {
		// The If-Match named no revision of this resource (the API's
		// noMatch, N-147): no number to quote.
		return &Error{
			Code:    CodePreconditionFailed,
			Message: fmt.Sprintf("%s %s has changed or does not match the ETag sent: reload it", kind, id),
			Details: Details{Revision: current},
		}
	}
	if ifMatch != current {
		return &Error{
			Code:    CodePreconditionFailed,
			Message: fmt.Sprintf("%s %s is at revision %d, not %d: reload it", kind, id, current, ifMatch),
			Details: Details{Revision: current},
		}
	}
	return nil
}
