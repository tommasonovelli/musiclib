package importer

import (
	"errors"
	"fmt"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/media"
	"musiclib/internal/names"
	"musiclib/internal/store"
)

// Error codes of the importer. They are stable: a failed scan or import job
// stores them as error_code, and the API shows them (§10.1). Codes shared
// with the catalog (too_many_files, invalid_disc, invalid_track_number,
// lyrics_association) and the names codes of an invalid text are used
// as they are; the media, fsops and blobstore codes of a failure that is
// not the file's fault (a timeout, an I/O error, a corrupt blob) pass
// through unchanged.
const (
	// CodeSourceNotFound: the batch root or the candidate does not exist
	// (any more) under /import.
	CodeSourceNotFound = "source_not_found"
	// CodeSourceNotDirectory: the batch root or the candidate is not a
	// directory.
	CodeSourceNotDirectory = "source_not_directory"
	// CodeSourceRejected: a symlink, a special file (FIFO, socket, device)
	// or a name that is not a valid relative path (§5.2) inside the
	// candidate. It is never followed nor opened.
	CodeSourceRejected = "source_rejected_entry"
	// CodeSourceChanged: the source changed during the import: a file or
	// directory added or removed, or a different identity, size or mtime
	// (§7.1).
	CodeSourceChanged = "source_changed"
	// CodeAmbiguousCandidate: direct audio together with audio further
	// down, or overlapping candidates (§7.2 rule 4); for a multi-disc
	// candidate, audio outside its disc directories or below one of them
	// (NOTES.md N-184).
	CodeAmbiguousCandidate = "ambiguous_candidate"
	// CodeDuplicateDisc: two disc directories of one multi-disc candidate
	// with the same number, such as CD1 and CD01, or CD1 and Disc 1 (§7.2
	// rule 3). The branch fails as a whole.
	CodeDuplicateDisc = "duplicate_disc"
	// CodeNotACandidate: the directory of an import job has no direct audio
	// (any more).
	CodeNotACandidate = "not_a_candidate"
	// CodeNoValidCandidate: a scan that found no candidate to import: the
	// batch is complete, with this explanation (§7.2).
	CodeNoValidCandidate = "no_valid_candidate"
	// CodeInsufficientSpace: the estimate plus the 1 GiB margin exceeds the
	// free space of /data (§11.2).
	CodeInsufficientSpace = "insufficient_space"
	// CodeCorruptAudio: a file with a known audio extension that has no
	// decodable audio, or supported audio that does not decode completely
	// (§7.2, §7.6).
	CodeCorruptAudio = "corrupt_audio"
	// CodeUnsupportedAudio: audio the application does not support (§8.1):
	// refused by the probe, or an M4A the tag reader refuses (fragmented,
	// encrypted, more than one track; N-165).
	CodeUnsupportedAudio = "unsupported_audio"
	// CodeUnrenderableTag: a field that the tag writer cannot save back
	// without loss, such as invalid UTF-8 in an unmanaged field (N-092).
	CodeUnrenderableTag = "unrenderable_tag"
	// CodeMixedAlbum: the tracks have different non-empty album tags
	// (§7.2); an explicit title resolves it.
	CodeMixedAlbum = "mixed_album"
	// CodeAmbiguousAlbumArtist: the tracks have different non-empty album
	// artist tags (§7.3); an explicit artist resolves it.
	CodeAmbiguousAlbumArtist = "ambiguous_album_artist"
	// CodeAlbumTitleMissing: no album tag and no directory name to fall
	// back on (the candidate is /import itself); an explicit title
	// resolves it.
	CodeAlbumTitleMissing = "album_title_missing"
	// CodeInvalidTag: a tag value that is not a valid metadata text (§5.2);
	// Err has the names code.
	CodeInvalidTag = "invalid_tag"
	// CodeInvalidArgument: a call that cannot be served as made.
	CodeInvalidArgument = "importer_invalid_argument"
)

// Error is the importer's typed error: a stable code, a message for the
// user, and the source path concerned, relative to the candidate (or to
// /import for the scan) and never absolute (§10.1). Err keeps the cause.
type Error struct {
	Code    string
	Message string
	Path    string
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

// Code returns the stable code of err: a fatal store code first (§6.4),
// then the importer's, the catalog's, the media adapter's, the blob
// store's, the filesystem's or the names code.
func Code(err error) string {
	if store.IsFatal(err) {
		return store.Code(err)
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	for _, code := range []string{catalog.Code(err), media.Code(err), blobstore.Code(err), fsops.Code(err), names.Code(err)} {
		if code != "" {
			return code
		}
	}
	return ""
}

// failure turns an error of a job's work into the code and message stored
// with the failed job. The message never holds an absolute path: every
// package below speaks in root labels and relative paths (§10.1). A tool's
// stderr is never included (N-082), nor a database error's text (N-150).
func failure(err error) (code, message string) {
	code = Code(err)
	if code == "" {
		code = "import_failed"
	}
	message = err.Error()
	var e *Error
	if errors.As(err, &e) {
		message = e.Message
		if e.Err != nil {
			message += ": " + e.Err.Error()
		}
	}
	// No database text in a message the user sees (§10.1, N-150).
	return code, catalog.JobMessage(err, message)
}
