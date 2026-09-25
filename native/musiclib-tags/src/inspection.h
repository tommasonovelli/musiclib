// What inspect reports about one file (DESIGN.md §8.1, §8.2, §8.3).
#pragma once

#include <array>
#include <cstdint>
#include <map>
#include <string>
#include <vector>

#include "fields.h"
#include "request.h"

namespace mltags {

// A key and its values in order. Keys are namespaced by container:
// "vorbis:TITLE", "vorbis.vendor", "flac.blocks", ...
struct KeyValues {
  std::string key;
  std::vector<std::string> values;
  bool operator==(const KeyValues &) const = default;
};

// Several sources of one managed field disagree. sources are in reading
// order: the first is the one the inspection reports (DESIGN.md §8.1: the
// canonical field wins over its aliases).
struct Conflict {
  Field field;
  std::vector<KeyValues> sources;
};

// An embedded picture. data holds its bytes, for extract-images; the JSON
// reports only its size and SHA-256.
struct Picture {
  // "block" (a FLAC PICTURE block), "comment" (a picture in a Vorbis
  // comment), "id3v2" (an APIC frame, or PIC in ID3v2.2) or "ape" (a "Cover
  // Art (...)" item). An APIC or APE picture has no width, height, depth or
  // colors: they are 0.
  std::string location;
  std::uint32_t type = 0;
  std::string mime;
  std::uint32_t width = 0, height = 0, depth = 0, colors = 0;
  std::string data;
};

// A field that cannot be saved back without loss (DESIGN.md §8.3). removed
// is true when a write removes it anyway (a managed key, an alias, a sort
// key, a picture, an ID3 tag in a FLAC file): only the others make
// write-managed-tags refuse the file.
struct Opaque {
  std::string key;
  std::string reason;
  bool removed = false;
};

// Reasons of Opaque; stable, part of the output.
namespace reason {
inline constexpr const char *kMalformedEntry = "malformed_entry";  // no '=' or an empty name
inline constexpr const char *kInvalidKey = "invalid_key";          // a name outside ASCII 0x20..0x7D
inline constexpr const char *kInvalidUTF8 = "invalid_utf8";
inline constexpr const char *kNulByte = "nul_byte";
inline constexpr const char *kDuplicateBlock = "duplicate_block";    // a second Vorbis comment block
inline constexpr const char *kForeignMetadata = "foreign_metadata";  // iXML / bext APPLICATION blocks
inline constexpr const char *kForeignTag = "foreign_tag";            // ID3v2 or ID3v1 in a FLAC file, stripped by a write (N-090)
inline constexpr const char *kInvalidPicture = "invalid_picture";
// MP3 (since helper version 3, NOTES.md N-153):
inline constexpr const char *kInvalidText = "invalid_text";            // text that does not decode in its encoding
inline constexpr const char *kMalformedFrame = "malformed_frame";      // an ID3v2 frame header or length that does not parse
inline constexpr const char *kUnknownFlags = "unknown_flags";          // ID3v2 frame flags undefined for the version
inline constexpr const char *kUnsupportedFrame = "unsupported_frame";  // an ID3v2.2 frame with no ID3v2.4 form
inline constexpr const char *kEmptyFrame = "empty_frame";              // an ID3v2 frame of size 0: it holds nothing
inline constexpr const char *kCompressedFrame = "compressed_frame";    // a managed frame the reader cannot read
inline constexpr const char *kEncryptedFrame = "encrypted_frame";      // a managed frame the reader cannot read
inline constexpr const char *kDuplicateTag = "duplicate_tag";          // a second ID3v2 tag after the first
inline constexpr const char *kMigrationConflict = "migration_conflict";  // the ID3v1 comment's COMM is taken
// M4A (since helper version 4, NOTES.md N-167):
inline constexpr const char *kUnsupportedData = "unsupported_data";  // a managed item of a data type the reader does not read
}  // namespace reason

// AudioRange is where the reader finds the audio in the file: from the end
// of the leading metadata (the FLAC metadata blocks, the ID3v2 tag of an
// MP3) to the start of the trailing tags (an ID3v1 tag; for MP3 also an APE
// tag before it). The digest of internal/media decodes exactly the bytes
// before end (NOTES.md N-128, N-154); a test pins the agreement.
struct AudioRange {
  std::uint64_t start = 0;
  std::uint64_t end = 0;
};

// A text field map: upper-case key -> values in file order, keys in byte
// order (std::string compares bytes as unsigned char).
using FieldMap = std::map<std::string, std::vector<std::string>>;

struct Inspection {
  Format format = Format::FLAC;
  // The managed fields (§8.2), each an ordered list of values (empty =
  // absent). Multi-valued text stays a list; the Go side joins it (§7.3).
  std::array<std::vector<std::string>, kFieldCount> managed;
  std::vector<Conflict> conflicts;
  std::vector<Picture> pictures;
  // Every unmanaged field in a canonical, comparable form, sorted by key.
  std::vector<KeyValues> unmanaged;
  std::vector<Opaque> opaque;
  AudioRange audio;
};

// renderInspection returns the JSON of an inspection.
std::string renderInspection(const Inspection &in);

// refuseBlocking throws Failure(opaque_field), naming them, when opaque has
// fields a write would not remove (DESIGN.md §8.3: "provocano un errore di
// render, non una perdita silenziosa").
void refuseBlocking(const std::vector<Opaque> &opaque);

}  // namespace mltags
