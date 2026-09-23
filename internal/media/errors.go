package media

import (
	"errors"
	"strconv"
)

// Error codes of the media adapter. They are stable: callers branch on them,
// jobs store them as error_code and the API exposes them (DESIGN.md §10.1).
const (
	// CodeToolUnavailable: a native tool cannot be started, or its -version
	// output is not the expected one. Fatal at boot (§11.1 step 3).
	CodeToolUnavailable = "media_tool_unavailable"
	// CodeToolVersion: the tool runs but is not the pinned version (§2.1).
	// Fatal at boot.
	CodeToolVersion = "media_tool_version"

	// CodeTimeout: the tool ran longer than its per-call timeout and was
	// killed, with its whole process group (§8.5: "un timeout è un errore
	// esplicito del job").
	CodeTimeout = "media_timeout"
	// CodeCanceled: the caller's context was cancelled; the tool and its
	// process group were killed and reaped.
	CodeCanceled = "media_canceled"
	// CodeToolFailed: the tool exited with a non-zero status or was killed by
	// a signal. A non-zero exit is always a failure, whatever the tool
	// printed (§8.5).
	CodeToolFailed = "media_tool_failed"
	// CodeOutputTooLarge: the tool wrote more to stdout than the call allows.
	CodeOutputTooLarge = "media_output_too_large"
	// CodeOutputInvalid: the tool succeeded but its output is not what the
	// adapter expects (malformed JSON, missing sections).
	CodeOutputInvalid = "media_output_invalid"

	// CodeNotSupported: AudioDigest was asked to decode a file that the
	// probe does not classify as supported audio (§8.1).
	CodeNotSupported = "media_not_supported"
	// CodeDecode: the full decode failed: a decoder or demuxer error, empty
	// output, PCM that is not a whole number of frames, or fewer or more
	// frames than the container declares (§7.6, §8.1, §8.4).
	CodeDecode = "media_decode"

	// CodeIO: an error of the adapter itself around the tool: pipes, the
	// input descriptor, the process group.
	CodeIO = "media_io"
	// CodeInvalidArgument: a programming error in a call (relative tool path,
	// missing timeout, an invalid tag value or cover).
	CodeInvalidArgument = "media_invalid_argument"

	// Failures of the TagLib helper (native/musiclib-tags), one per code of
	// its error protocol (src/failure.h), plus the verification of §9.1
	// step 6.

	// CodeTagsInvalidRequest: the helper refused the request; a bug of the
	// adapter, never of the file.
	CodeTagsInvalidRequest = "media_tags_invalid_request"
	// CodeTagsBadDescriptor: a descriptor is not a regular file with the
	// access the operation needs (a directory, a FIFO, read-only for a
	// write, a destination that is not empty).
	CodeTagsBadDescriptor = "media_tags_bad_descriptor"
	// CodeTagsUnsupported: the helper does not handle this format for this
	// operation (MP3 and M4A: Phase 4).
	CodeTagsUnsupported = "media_tags_unsupported_format"
	// CodeTagsFormatMismatch: the content is not the declared format.
	CodeTagsFormatMismatch = "media_tags_format_mismatch"
	// CodeTagsCorrupt: the metadata structure of the file is damaged.
	CodeTagsCorrupt = "media_tags_corrupt"
	// CodeTagsOpaque: the write refused the file because a field could not
	// be saved back without loss (§8.3: "provocano un errore di render").
	CodeTagsOpaque = "media_tags_opaque_field"
	// CodeTagsTooLarge: a value does not fit the format (a FLAC metadata
	// block, a cover of 16 MiB or more), or the helper ran out of memory.
	CodeTagsTooLarge = "media_tags_too_large"
	// CodeTagsNoPicture: ExtractImages named a picture the file does not
	// have.
	CodeTagsNoPicture = "media_tags_picture_not_found"
	// CodeTagsIO: a read or write of the helper on a descriptor failed.
	CodeTagsIO = "media_tags_io"
	// CodeTagsInternal: an invariant of the helper broke (TagLib and the
	// helper's reader disagree, or the written file does not read back as
	// asked). Never expected; the file must not be used.
	CodeTagsInternal = "media_tags_internal"
	// CodeTagsVerification: VerifyTags found that a written file does not
	// hold the expected managed fields and cover, or lost or changed an
	// unmanaged field (§9.1 step 6, §12.2).
	CodeTagsVerification = "media_tags_verification"
)

// Error is the package's typed error.
//
// Stderr holds at most the first 64 KiB of the tool's standard error. It is
// kept apart from Error() on purpose: a decoder's messages can quote tag
// contents, which the logs must not carry by default (§11.1). A caller that
// wants it for a job's error message reads the field explicitly.
type Error struct {
	Code string
	Op   string // "ffprobe", "ffmpeg decode", "run", ...
	Msg  string
	// ExitCode is the tool's exit status, or -1 if it did not exit normally
	// (killed by a signal, never started).
	ExitCode int
	Stderr   []byte
	Err      error
}

func (e *Error) Error() string {
	msg := e.Code + " (" + e.Op + ")"
	if e.Msg != "" {
		msg += ": " + e.Msg
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Code returns the code of the first *Error in err's tree, otherwise "".
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func newErr(code, op, msg string, err error) *Error {
	return &Error{Code: code, Op: op, Msg: msg, ExitCode: -1, Err: err}
}

// exitMsg describes how a tool ended, for messages.
func exitMsg(exitCode int, signal string) string {
	if signal != "" {
		return "killed by signal " + signal
	}
	return "exit status " + strconv.Itoa(exitCode)
}
