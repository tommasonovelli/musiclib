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
  std::string location;  // "block" (a FLAC PICTURE block) or "comment"
  std::uint32_t type = 0;
  std::string mime;
  std::uint32_t width = 0, height = 0, depth = 0, colors = 0;
  std::string data;
};

// A field that cannot be saved back without loss (DESIGN.md §8.3). removed
// is true when a write removes it anyway (a managed key, an alias, a sort
// key, a picture): only the others make write-managed-tags refuse the file.
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
inline constexpr const char *kForeignTag = "foreign_tag";            // ID3v2 or ID3v1 in a FLAC file
inline constexpr const char *kInvalidPicture = "invalid_picture";
}  // namespace reason

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
};

// renderInspection returns the JSON of an inspection.
std::string renderInspection(const Inspection &in);

}  // namespace mltags
