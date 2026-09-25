#include "ape.h"

#include "failure.h"

namespace mltags::ape {

namespace {

std::uint32_t le32(std::string_view d, std::size_t off) {
  return static_cast<std::uint32_t>(static_cast<unsigned char>(d[off])) |
         (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 1])) << 8) |
         (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 2])) << 16) |
         (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 3])) << 24);
}

void putLE32(std::string &out, std::uint32_t v) {
  for (int i = 0; i < 4; ++i) {
    out.push_back(static_cast<char>(v & 0xFF));
    v >>= 8;
  }
}

std::string footerBytes(const Footer &f, bool header) {
  std::string out("APETAGEX");
  putLE32(out, f.version);
  putLE32(out, f.size);
  putLE32(out, f.count);
  putLE32(out, header ? (f.flags | 0x20000000U) : (f.flags & ~0x20000000U));
  out.append(8, '\0');
  return out;
}

}  // namespace

std::optional<Footer> parseFooter(std::string_view b) {
  if (b.size() < kFooterSize || b.substr(0, 8) != "APETAGEX") return std::nullopt;
  return Footer{le32(b, 8), le32(b, 12), le32(b, 16), le32(b, 20)};
}

std::vector<Item> parseItems(std::string_view area, std::uint32_t count) {
  std::vector<Item> items;
  std::size_t pos = 0;
  for (std::uint32_t i = 0; i < count; ++i) {
    const std::string what = "APE item " + std::to_string(i);
    if (area.size() - pos < 9) throw Failure(code::kCorrupt, what + " does not fit in the tag");
    const std::uint32_t len = le32(area, pos);
    Item it;
    it.flags = le32(area, pos + 4);
    const std::size_t nul = area.find('\0', pos + 8);
    if (nul == std::string_view::npos) throw Failure(code::kCorrupt, what + " has no end of key");
    it.key.assign(area.substr(pos + 8, nul - pos - 8));
    if (len > area.size() - nul - 1) throw Failure(code::kCorrupt, what + " extends past the tag");
    it.value.assign(area.substr(nul + 1, len));
    const std::size_t end = nul + 1 + len;
    it.raw.assign(area.substr(pos, end - pos));
    pos = end;
    items.push_back(std::move(it));
  }
  return items;
}

std::string render(const Footer &original, const std::vector<Item> &items) {
  std::string body;
  for (const Item &it : items) body += it.raw;
  const std::uint64_t size = body.size() + kFooterSize;
  if (size > 0xFFFFFFFFU) throw Failure(code::kTooLarge, "the APE tag would be larger than 4 GiB");
  Footer f = original;
  f.size = static_cast<std::uint32_t>(size);
  f.count = static_cast<std::uint32_t>(items.size());
  std::string out;
  if (original.hasHeader()) out += footerBytes(f, true);
  out += body;
  out += footerBytes(f, false);
  return out;
}

}  // namespace mltags::ape
