// Typed failures of the helper. Every failure the helper reports is one of
// these codes, written as {"code": ..., "message": ...} on stderr with exit
// status 3 (see main.cpp). The codes are stable: the Go adapter
// (internal/media/tags.go) maps each one to its own media_tags_* code.
#pragma once

#include <stdexcept>
#include <string>

namespace mltags {

namespace code {
// The command line or the JSON request is malformed or violates the schema.
inline constexpr const char *kInvalidRequest = "invalid_request";
// A descriptor is missing, is not a regular file, or has the wrong access
// mode for the operation.
inline constexpr const char *kBadDescriptor = "bad_descriptor";
// The format is not one the helper handles for this operation.
inline constexpr const char *kUnsupportedFormat = "unsupported_format";
// The content is not the format the request declares.
inline constexpr const char *kFormatMismatch = "format_mismatch";
// The metadata structure of the file is damaged.
inline constexpr const char *kCorrupt = "corrupt";
// write-managed-tags refused: a field could not be saved back without loss
// (DESIGN.md §8.3).
inline constexpr const char *kOpaqueField = "opaque_field";
// A value does not fit the format (a FLAC metadata block is at most
// 16 MiB - 1), or memory ran out.
inline constexpr const char *kTooLarge = "too_large";
// extract-images named a picture the file does not have.
inline constexpr const char *kPictureNotFound = "picture_not_found";
// A read or write on a descriptor failed.
inline constexpr const char *kIO = "io";
// A write on a descriptor failed because the filesystem is full (ENOSPC) or
// the quota is exhausted (EDQUOT): the caller reports it as a lack of space
// (DESIGN.md §11.2), not as a generic I/O error (NOTES.md N-143). Since
// helper version 3.
inline constexpr const char *kNoSpace = "no_space";
// An invariant of the helper broke: TagLib and the helper disagree, or the
// written file does not read back as requested. Never expected.
inline constexpr const char *kInternal = "internal";
}  // namespace code

class Failure : public std::runtime_error {
 public:
  Failure(const char *code, const std::string &message) : std::runtime_error(message), code_(code) {}
  const char *code() const noexcept { return code_; }

 private:
  const char *code_;
};

}  // namespace mltags
