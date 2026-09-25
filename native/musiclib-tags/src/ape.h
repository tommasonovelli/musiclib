// APEv1/APEv2 tags at the end of an MP3: the helper's own reader and writer
// (DESIGN.md §8.3: "Gli eventuali APE vengono conservati per i campi non
// gestiti"; NOTES.md N-152). A write keeps every unmanaged item byte for
// byte, in order, and rewrites only the counts and sizes. Nothing here
// depends on TagLib.
#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

namespace mltags::ape {

inline constexpr std::size_t kFooterSize = 32;  // the footer, and the header
// TagLib's MAX_APE_ITEM_COUNT: the items beyond it are dropped by TagLib.
inline constexpr std::uint32_t kMaxItems = 50000;

// Footer is an APE footer (or header): "APETAGEX", version, size (the items
// and the footer, not the header), item count, flags, 8 reserved bytes.
struct Footer {
  std::uint32_t version = 0;
  std::uint32_t size = 0;
  std::uint32_t count = 0;
  std::uint32_t flags = 0;

  bool hasHeader() const { return (flags & 0x80000000U) != 0; }
  bool isHeader() const { return (flags & 0x20000000U) != 0; }
  // completeSize is the extent of the tag, its header included: TagLib's
  // Footer::completeTagSize.
  std::uint64_t completeSize() const { return std::uint64_t{size} + (hasHeader() ? kFooterSize : 0); }
};

// parseFooter reads 32 bytes; nullopt when they do not start with
// "APETAGEX".
std::optional<Footer> parseFooter(std::string_view b);

// Item is one item: its key as stored, its flags (bit 0 read only, bits 1-2
// the type: 0 UTF-8 text, 1 binary, 2 external locator), its value, and its
// bytes as stored (size, flags, key, NUL, value), which a write copies.
struct Item {
  std::string key;
  std::uint32_t flags = 0;
  std::string value;
  std::string raw;
  bool operator==(const Item &) const = default;
};

// parseItems reads count items from the start of area. An item that does not
// fit in the area is Failure(corrupt). Bytes after the last item are not an
// item and are ignored, as TagLib ignores them.
std::vector<Item> parseItems(std::string_view area, std::uint32_t count);

// render is a tag with the items, the version and flags of original, and a
// header when original had one. The reserved bytes are zero.
std::string render(const Footer &original, const std::vector<Item> &items);

}  // namespace mltags::ape
