#include "mp4.h"

#include <algorithm>
#include <array>
#include <limits>

#include "failure.h"
#include "text.h"

namespace mltags::mp4 {

namespace {

[[noreturn]] void corrupt(const std::string &msg) { throw Failure(code::kCorrupt, msg); }

}  // namespace

std::uint16_t be16(std::string_view b, std::size_t off) {
  return static_cast<std::uint16_t>((static_cast<unsigned>(static_cast<unsigned char>(b[off])) << 8) |
                                    static_cast<unsigned>(static_cast<unsigned char>(b[off + 1])));
}

std::uint32_t be32(std::string_view b, std::size_t off) {
  return (static_cast<std::uint32_t>(be16(b, off)) << 16) | be16(b, off + 2);
}

std::uint64_t be64(std::string_view b, std::size_t off) {
  return (static_cast<std::uint64_t>(be32(b, off)) << 32) | be32(b, off + 4);
}

std::string enc16(std::uint16_t n) {
  std::string s(2, '\0');
  s[0] = static_cast<char>(n >> 8);
  s[1] = static_cast<char>(n & 0xFF);
  return s;
}

std::string enc32(std::uint32_t n) {
  return enc16(static_cast<std::uint16_t>(n >> 16)) + enc16(static_cast<std::uint16_t>(n & 0xFFFF));
}

std::string enc64(std::uint64_t n) {
  return enc32(static_cast<std::uint32_t>(n >> 32)) + enc32(static_cast<std::uint32_t>(n & 0xFFFFFFFFU));
}

std::optional<Box> parseBoxHeader(std::string_view h, std::uint64_t offset, std::uint64_t limit, bool toEnd,
                                  const std::string &what) {
  if (h.size() < 8) return std::nullopt;
  if (offset > limit) corrupt(what + ": a box starts past its container");
  Box box;
  box.type.assign(h.substr(4, 4));
  box.offset = offset;
  const std::uint32_t size32 = be32(h, 0);
  if (size32 == 1) {
    if (h.size() < 16) return std::nullopt;
    box.header = 16;
    box.size = be64(h, 8);
  } else if (size32 == 0) {
    if (!toEnd) corrupt(what + ": the box \"" + box.type + "\" has size 0");
    box.size = limit - offset;
  } else {
    box.size = size32;
  }
  if (box.size < box.header) {
    corrupt(what + ": the box \"" + box.type + "\" declares " + std::to_string(box.size) + " bytes");
  }
  if (box.size > limit - offset) {
    corrupt(what + ": the box \"" + box.type + "\" extends past its container");
  }
  return box;
}

std::vector<Box> children(std::string_view b, std::uint64_t begin, std::uint64_t end, const std::string &what) {
  if (begin > end || end > b.size()) corrupt(what + ": the range is outside the data");
  std::vector<Box> out;
  std::uint64_t off = begin;
  while (off < end) {
    if (out.size() >= kMaxChildren) corrupt(what + ": more than " + std::to_string(kMaxChildren) + " boxes");
    const std::size_t avail = static_cast<std::size_t>(std::min<std::uint64_t>(16, end - off));
    const auto box = parseBoxHeader(b.substr(static_cast<std::size_t>(off), avail), off, end, false, what);
    if (!box) corrupt(what + ": a truncated box header");
    off = box->end();
    out.push_back(*box);
  }
  return out;
}

std::optional<std::vector<Box>> tryChildren(std::string_view b, std::uint64_t begin, std::uint64_t end) {
  try {
    return children(b, begin, end, "");
  } catch (const Failure &) {
    return std::nullopt;
  }
}

std::string renderBox(std::string_view type, std::string_view payload, bool large) {
  const std::uint64_t small = payload.size() + 8;
  if (!large && small <= std::numeric_limits<std::uint32_t>::max()) {
    return enc32(static_cast<std::uint32_t>(small)) + std::string(type) + std::string(payload);
  }
  return enc32(1) + std::string(type) + enc64(payload.size() + 16) + std::string(payload);
}

bool isFullAtomMeta(std::string_view payload) {
  static constexpr std::array<std::string_view, 5> kChildren = {"hdlr", "ilst", "mhdr", "ctry", "lang"};
  if (payload.size() < 8) return true;
  const std::string_view name = payload.substr(4, 4);
  for (const auto c : kChildren) {
    if (name == c) return false;
  }
  return true;
}

std::optional<Data> parseData(std::string_view box) {
  if (box.size() < 16 || be32(box, 0) != box.size() || box.substr(4, 4) != "data") return std::nullopt;
  return Data{be32(box, 8), be32(box, 12), box.substr(16)};
}

std::string renderData(std::uint32_t type, std::string_view value) {
  return renderBox("data", enc32(type) + enc32(0) + std::string(value));
}

std::optional<std::string_view> parseFullString(std::string_view box, std::string_view type) {
  if (box.size() < 12 || be32(box, 0) != box.size() || box.substr(4, 4) != type || be32(box, 8) != 0) {
    return std::nullopt;
  }
  return box.substr(12);
}

std::optional<std::string> utf16beToUTF8(std::string_view b) {
  if (b.size() % 2 != 0) return std::nullopt;
  std::string out;
  for (std::size_t i = 0; i < b.size(); i += 2) {
    const std::uint32_t u = be16(b, i);
    if (u >= 0xDC00 && u <= 0xDFFF) return std::nullopt;
    if (u >= 0xD800 && u <= 0xDBFF) {
      if (i + 4 > b.size()) return std::nullopt;
      const std::uint32_t low = be16(b, i + 2);
      if (low < 0xDC00 || low > 0xDFFF) return std::nullopt;
      appendUTF8(out, 0x10000 + ((u - 0xD800) << 10) + (low - 0xDC00));
      i += 2;
      continue;
    }
    appendUTF8(out, u);
  }
  return out;
}

std::vector<Chunk> chunks(std::string_view stsc, std::string_view stsz, std::string_view offsets, bool co64) {
  const std::string name = co64 ? "co64" : "stco";
  if (stsc.size() < 8 || stsz.size() < 12 || offsets.size() < 8) corrupt("a sample table box is too short");
  const std::uint64_t runs = be32(stsc, 4);
  if (runs > (stsc.size() - 8) / 12) corrupt("stsc declares more entries than it holds");
  const std::uint32_t uniform = be32(stsz, 4);
  const std::uint64_t samples = be32(stsz, 8);
  if (uniform == 0 && samples > (stsz.size() - 12) / 4) corrupt("stsz declares more samples than it holds");
  const std::uint64_t entry = co64 ? 8 : 4;
  const std::uint64_t count = be32(offsets, 4);
  if (count > (offsets.size() - 8) / entry) corrupt(name + " declares more chunks than it holds");
  if (runs == 0) {
    if (count != 0 || samples != 0) corrupt("stsc has no entry for the chunks of the track");
    return {};
  }

  std::vector<Chunk> out;
  out.reserve(static_cast<std::size_t>(count));
  std::uint64_t sample = 0;  // index of the next sample in stsz
  for (std::uint64_t r = 0; r < runs; ++r) {
    const std::size_t at = static_cast<std::size_t>(8 + 12 * r);
    const std::uint64_t first = be32(stsc, at);
    const std::uint64_t perChunk = be32(stsc, at + 4);
    const std::uint32_t description = be32(stsc, at + 8);
    const std::uint64_t next = r + 1 < runs ? be32(stsc, at + 12) : count + 1;
    if ((r == 0 && first != 1) || first > count || next <= first || next > count + 1) {
      corrupt("stsc entry " + std::to_string(r) + " does not describe the chunks in order");
    }
    if (perChunk == 0 || description != 1) {
      corrupt("stsc entry " + std::to_string(r) + " has " + std::to_string(perChunk) +
              " samples per chunk and sample description " + std::to_string(description));
    }
    for (std::uint64_t c = first; c < next; ++c) {
      if (perChunk > samples - sample) corrupt("the chunks hold more samples than stsz declares");
      std::uint64_t size = 0;
      if (uniform != 0) {
        size = perChunk * uniform;
      } else {
        for (std::uint64_t s = 0; s < perChunk; ++s) size += be32(stsz, static_cast<std::size_t>(12 + 4 * (sample + s)));
      }
      sample += perChunk;
      const std::size_t o = static_cast<std::size_t>(8 + entry * (c - 1));
      const std::uint64_t offset = co64 ? be64(offsets, o) : be32(offsets, o);
      if (size > std::numeric_limits<std::uint64_t>::max() - offset) corrupt(name + ": a chunk ends past 2^64");
      out.push_back(Chunk{offset, size});
    }
  }
  if (sample != samples) corrupt("stsz declares more samples than the chunks hold");
  return out;
}

std::string patchOffsets(std::string_view payload, bool co64, std::uint64_t from, std::int64_t delta) {
  std::string out(payload);
  if (payload.size() < 8) corrupt("a chunk offset box is too short");
  const std::uint64_t entry = co64 ? 8 : 4;
  const std::uint64_t count = be32(payload, 4);
  if (count > (payload.size() - 8) / entry) corrupt("a chunk offset box declares more chunks than it holds");
  for (std::uint64_t i = 0; i < count; ++i) {
    const std::size_t at = static_cast<std::size_t>(8 + entry * i);
    const std::uint64_t o = co64 ? be64(payload, at) : be32(payload, at);
    if (o < from) continue;
    std::uint64_t moved;
    if (delta >= 0) {
      const auto d = static_cast<std::uint64_t>(delta);
      if (d > std::numeric_limits<std::uint64_t>::max() - o) throw Failure(code::kTooLarge, "a chunk offset overflows");
      moved = o + d;
    } else {
      const auto d = static_cast<std::uint64_t>(-(delta + 1)) + 1;
      if (d > o) throw Failure(code::kTooLarge, "a chunk offset would become negative");
      moved = o - d;
    }
    if (co64) {
      out.replace(at, 8, enc64(moved));
    } else {
      if (moved > std::numeric_limits<std::uint32_t>::max()) {
        throw Failure(code::kTooLarge, "a chunk offset of stco would exceed 32 bits (" + std::to_string(moved) + ")");
      }
      out.replace(at, 4, enc32(static_cast<std::uint32_t>(moved)));
    }
  }
  return out;
}

}  // namespace mltags::mp4
