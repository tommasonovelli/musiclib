package render

import (
	"errors"
	"fmt"

	"musiclib/internal/blobstore"
	"musiclib/internal/fsops"
	"musiclib/internal/media"
	"musiclib/internal/names"
)

// Error codes of the renderer. They are stable: a failed render job stores
// them as error_code, and the API shows them (§10.1). The codes of the
// packages below pass through unchanged when the failure is theirs: the
// media adapter's (a tool failure, media_tags_verification of §9.1 step 6),
// the blob store's (corrupt_blob, blob_not_found), the filesystem's.
const (
	// CodeInvalidSnapshot: the snapshot breaks an invariant the catalog
	// guarantees (an invalid hash, a disc or number out of range, a cover
	// that is not JPEG or PNG, no track). Never expected; nothing is built.
	CodeInvalidSnapshot = "render_invalid_snapshot"
	// CodeFormatNotSupportedYet: a track whose blob is M4A, which the
	// renderer does not build until its tag writer exists.
	CodeFormatNotSupportedYet = "render_format_not_supported_yet"
	// CodePathCollision: two entries of the album have the same path after
	// normalization, or a file has the path of a directory of another
	// (§5.2). The message and Names give both; the user corrects them.
	CodePathCollision = "render_path_collision"
	// CodePathInvalid: an output path breaks a limit of §5.2 (Err has the
	// names code, such as path_too_long).
	CodePathInvalid = "render_path_invalid"
	// CodeVersionMismatch: a plan or snapshot made for another
	// render_version, or tools that are not the ones render_version names.
	CodeVersionMismatch = "render_version_mismatch"
	// CodeCopyMismatch: a staged copy does not read back as the blob it was
	// copied from (§9.1 steps 5 and 7).
	CodeCopyMismatch = "render_copy_mismatch"
	// CodeAudioChanged: the audio digest of a track after the tag write
	// differs from the one before (§8.4, §9.1 step 6, §12.2).
	CodeAudioChanged = "render_audio_changed"
	// CodeInsufficientSpace: the estimate plus the 1 GiB margin exceeds the
	// free space of /data, or a write met ENOSPC (§11.2). The code is the
	// importer's: the same condition for the user.
	CodeInsufficientSpace = "insufficient_space"
	// CodeReceiptInvalid: a .musiclib.json that ParseReceipt refuses.
	CodeReceiptInvalid = "render_receipt_invalid"
	// CodeInvalidArgument: a call that cannot be served as made.
	CodeInvalidArgument = "render_invalid_argument"
)

// Error is the renderer's typed error: a stable code, a message for the
// user, and for a collision the two entries concerned. Paths are relative
// to the album directory, never absolute (§10.1). Err keeps the cause.
type Error struct {
	Code    string
	Message string
	// Names are the entries of a collision, as the user knows them: a
	// track's title, an attachment's rel_path, "cover", the receipt.
	Names []string
	Err   error
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

// Code returns the stable code of err: the renderer's, then the media
// adapter's, the blob store's, the filesystem's or the names code.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	for _, code := range []string{media.Code(err), blobstore.Code(err), fsops.Code(err), names.Code(err)} {
		if code != "" {
			return code
		}
	}
	return ""
}
