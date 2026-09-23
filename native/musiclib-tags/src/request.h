// The typed requests of the three operations (DESIGN.md §8.1), decoded from
// the JSON on standard input. Every object has a closed set of keys, all of
// them required: a missing or unknown key is Failure(invalid_request).
#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

#include "fields.h"

namespace mltags {

// kMaxRequestBytes bounds the JSON request. The largest legitimate request
// is a write with ten text fields of 1,024 characters each (DESIGN.md §5.2),
// at most about 120 KiB even with every character escaped.
inline constexpr std::size_t kMaxRequestBytes = 256 * 1024;

// kMaxPictures bounds the pictures of one extract-images request.
inline constexpr std::size_t kMaxPictures = 256;

// Format is the blobs.format of the file (DESIGN.md §4.2), decided by the
// caller from the content (ffprobe), never from a name.
enum class Format { FLAC, MP3, M4A_AAC, M4A_ALAC };

std::string_view formatName(Format f) noexcept;

struct InspectRequest {
  Format format;
};

struct ExtractRequest {
  Format format;
  // Indexes of Inspection pictures; picture i is written to descriptor 4+i.
  std::vector<std::uint32_t> pictures;
};

// ManagedValues are the managed fields to write (DESIGN.md §8.2). An empty
// optional means the field is removed; compilation false means the field is
// removed, true is written as "1".
struct ManagedValues {
  std::optional<std::string> text[kFieldCount];      // text fields
  std::optional<std::uint32_t> number[kFieldCount];  // track, totals, disc
  bool compilation = false;
};

// CoverSpec describes the image on descriptor 4: it becomes the one embedded
// picture, a front cover, with these attributes and the file's bytes.
struct CoverSpec {
  std::string mime;
  std::uint32_t width = 0, height = 0, depth = 0, colors = 0;
};

struct WriteRequest {
  Format format;
  ManagedValues values;
  std::optional<CoverSpec> cover;
};

InspectRequest parseInspectRequest(std::string_view json);
ExtractRequest parseExtractRequest(std::string_view json);
WriteRequest parseWriteRequest(std::string_view json);

}  // namespace mltags
