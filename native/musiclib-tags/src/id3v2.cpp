#include "id3v2.h"

#include <array>
#include <utility>

#include "failure.h"
#include "text.h"

namespace mltags::id3v2 {

namespace {

[[noreturn]] void corrupt(const std::string &msg) { throw Failure(code::kCorrupt, msg); }

std::uint8_t u8(std::string_view d, std::size_t off) { return static_cast<std::uint8_t>(d[off]); }

std::uint32_t be24(std::string_view d, std::size_t off) {
  return (static_cast<std::uint32_t>(u8(d, off)) << 16) | (static_cast<std::uint32_t>(u8(d, off + 1)) << 8) |
         u8(d, off + 2);
}

std::uint32_t be32(std::string_view d, std::size_t off) {
  return (static_cast<std::uint32_t>(u8(d, off)) << 24) | be24(d, off + 1);
}

// validID reports whether id is a frame identifier: upper-case letters and
// digits only (TagLib's isValidFrameID for 4 characters).
bool validID(std::string_view id) {
  if (id.empty()) return false;
  for (const char c : id) {
    if (!((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'))) return false;
  }
  return true;
}

// validIDAt reports whether a valid 4-character identifier is at off.
bool validIDAt(std::string_view d, std::uint64_t off) { return off <= d.size() && d.size() - off >= 4 && validID(d.substr(off, 4)); }

}  // namespace

std::optional<std::uint32_t> synchsafe(std::string_view b) {
  std::uint32_t n = 0;
  for (std::size_t i = 0; i < 4; ++i) {
    const auto c = u8(b, i);
    if ((c & 0x80) != 0) return std::nullopt;
    n = (n << 7) | c;
  }
  return n;
}

std::string encodeSynchsafe(std::uint32_t n) {
  std::string out(4, '\0');
  for (int i = 3; i >= 0; --i) {
    out[static_cast<std::size_t>(i)] = static_cast<char>(n & 0x7F);
    n >>= 7;
  }
  return out;
}

std::string deunsynchronise(std::string_view b) {
  std::string out;
  out.reserve(b.size());
  for (std::size_t i = 0; i < b.size(); ++i) {
    out.push_back(b[i]);
    if (u8(b, i) == 0xFF && i + 1 < b.size() && b[i + 1] == '\0') ++i;
  }
  return out;
}

std::optional<Header> parseHeader(std::string_view b) {
  if (b.size() < kHeaderSize || b.substr(0, 3) != "ID3") return std::nullopt;
  Header h;
  h.major = u8(b, 3);
  h.revision = u8(b, 4);
  h.flags = u8(b, 5);
  if (h.major == 0xFF || h.revision == 0xFF) corrupt("the version of the ID3v2 tag is 0xFF");
  const auto size = synchsafe(b.substr(6, 4));
  if (!size) corrupt("the size of the ID3v2 tag is not synchsafe");
  h.size = *size;
  if (h.major < 2 || h.major > 4) corrupt("the ID3v2 tag has version 2." + std::to_string(h.major));
  if (h.major == 2 && (h.flags & 0x40) != 0) corrupt("the ID3v2.2 tag is compressed, which no reader can read");
  const std::uint8_t undefined = h.major == 2 ? 0x3F : h.major == 3 ? 0x1F : 0x0F;
  if ((h.flags & undefined) != 0) {
    corrupt("the ID3v2." + std::to_string(h.major) + " tag has flags its version does not define");
  }
  return h;
}

Tag parseTag(std::string_view tag, const Header &h) {
  Tag t;
  t.header = h;
  const std::string_view stored = tag.substr(kHeaderSize, h.size);
  std::string owned;
  std::string_view data = stored;
  // ID3v2.2 and ID3v2.3 unsynchronise the whole body; ID3v2.4 each frame.
  if (h.unsynchronised() && h.major <= 3) {
    owned = deunsynchronise(stored);
    data = owned;
  }
  std::size_t pos = 0;
  if (h.extended()) {
    if (data.size() < 4) corrupt("the extended header of the ID3v2 tag does not fit in it");
    if (h.major == 3) {
      const std::uint32_t n = be32(data, 0);
      if (n != 6 && n != 10) corrupt("the extended header of the ID3v2.3 tag has size " + std::to_string(n));
      pos = 4 + n;
    } else {
      const auto n = synchsafe(data.substr(0, 4));
      if (!n || *n < 6) corrupt("the extended header of the ID3v2.4 tag has an invalid size");
      pos = *n;
    }
    if (pos > data.size()) corrupt("the extended header of the ID3v2 tag does not fit in it");
  }

  const std::size_t hs = h.major == 2 ? 6 : 10;
  const bool unsync4 = h.major == 4 && h.unsynchronised();
  while (pos < data.size()) {
    // The padding: the rest is not a field (TagLib stops here too).
    if (data[pos] == '\0') break;
    const std::string where = "id3v2#" + std::to_string(pos);
    auto malformed = [&](const std::string &key) { t.opaque.push_back({key, reason::kMalformedFrame, false}); };
    if (t.frames.size() + t.opaque.size() >= kMaxFrames || data.size() - pos < hs) {
      malformed(where);
      break;
    }
    std::string id(data.substr(pos, hs == 6 ? 3 : 4));
    bool v22 = h.major == 2;
    std::uint32_t size = 0;
    std::uint8_t f1 = 0, f2 = 0;
    if (h.major == 2) {
      size = be24(data, pos + 3);
    } else {
      f1 = u8(data, pos + 8);
      f2 = u8(data, pos + 9);
      if (h.major == 3) {
        size = be32(data, pos + 4);
      } else {
        // TagLib's SynchData::toUInt: a size that is not synchsafe is read as
        // a plain integer (written by buggy software).
        const auto s = synchsafe(data.substr(pos + 4, 4));
        size = s ? *s : be32(data, pos + 4);
        // TagLib's iTunes hack: ID3v2.4 frames written with ID3v2.3 sizes.
        // The same rule, so that both read the same frames.
        if (size > 127 && !validIDAt(data, pos + 10 + std::uint64_t{size})) {
          const std::uint32_t plain = be32(data, pos + 4);
          if (validIDAt(data, pos + 10 + std::uint64_t{plain})) size = plain;
        }
      }
      // TagLib's other iTunes hack: ID3v2.2 identifiers in ID3v2.3 tags.
      if (h.major == 3 && (id[3] == '\0' || id[3] == ' ')) {
        id.resize(3);
        v22 = true;
      }
    }
    if (!validID(id) || size > data.size() - pos - hs) {
      malformed(where);
      break;
    }
    const std::string_view raw = data.substr(pos + hs, size);
    Frame fr;
    fr.sourceID = id;
    fr.offset = pos;
    pos += hs + size;

    if (v22) {
      const std::string_view mapped = id3v2Of22(id);
      if (mapped.empty()) {
        t.opaque.push_back({"id3v2:" + id, reason::kUnsupportedFrame, false});
        continue;
      }
      fr.id = std::string(mapped);
    } else {
      fr.id = id;
    }
    const std::string key = "id3v2:" + fr.id;

    std::string unsynced;
    std::string_view d = raw;
    std::size_t p = 0;
    auto need = [&](std::size_t n) { return d.size() - p >= n; };
    if (h.major == 3) {
      if ((f1 & 0x1F) != 0 || (f2 & 0x1F) != 0) {
        t.opaque.push_back({key, reason::kUnknownFlags, false});
        continue;
      }
      fr.status = static_cast<std::uint8_t>(((f1 & 0x80) ? 0x40 : 0) | ((f1 & 0x40) ? 0x20 : 0) | ((f1 & 0x20) ? 0x10 : 0));
      // ID3v2.3 structure 3.3.1: decompressed size, encryption method,
      // group identifier, in this order.
      if ((f2 & 0x80) != 0) {
        if (!need(4)) {
          malformed(key);
          continue;
        }
        const std::uint32_t n = be32(d, p);
        if (n > kMaxSynchsafe) {
          t.opaque.push_back({key, reason::kUnsupportedFrame, false});
          continue;
        }
        fr.compressed = fr.hasLength = true;
        fr.length = n;
        p += 4;
      }
      if ((f2 & 0x40) != 0) {
        if (!need(1)) {
          malformed(key);
          continue;
        }
        fr.encrypted = true;
        fr.method = u8(d, p++);
      }
      if ((f2 & 0x20) != 0) {
        if (!need(1)) {
          malformed(key);
          continue;
        }
        fr.grouping = true;
        fr.group = u8(d, p++);
      }
    } else if (h.major == 4) {
      if ((f1 & 0x8F) != 0 || (f2 & 0xB0) != 0) {
        t.opaque.push_back({key, reason::kUnknownFlags, false});
        continue;
      }
      fr.status = static_cast<std::uint8_t>(f1 & 0x70);
      // Structure 4.1.2 n: everything after the frame header is
      // unsynchronised, the added bytes included.
      if (unsync4 || (f2 & 0x02) != 0) {
        unsynced = deunsynchronise(raw);
        d = unsynced;
      }
      // Grouping identity, encryption method, data length indicator, in
      // this order.
      if ((f2 & 0x40) != 0) {
        if (!need(1)) {
          malformed(key);
          continue;
        }
        fr.grouping = true;
        fr.group = u8(d, p++);
      }
      if ((f2 & 0x04) != 0) {
        if (!need(1)) {
          malformed(key);
          continue;
        }
        fr.encrypted = true;
        fr.method = u8(d, p++);
      }
      if ((f2 & 0x01) != 0) {
        const auto n = need(4) ? synchsafe(d.substr(p, 4)) : std::nullopt;
        if (!n) {
          malformed(key);
          continue;
        }
        fr.hasLength = true;
        fr.length = *n;
        p += 4;
      }
      fr.compressed = (f2 & 0x08) != 0;
      if (fr.compressed && !fr.hasLength) {
        malformed(key);  // structure 4.1.2 k: compression needs the length
        continue;
      }
    }
    fr.body.assign(d.substr(p));
    if (fr.hasLength && !fr.compressed && !fr.encrypted) {
      // Only the length of the data itself: TagLib reads the data by it, so
      // any other value would read differently.
      if (fr.length != fr.body.size()) {
        malformed(key);
        continue;
      }
      fr.hasLength = false;
      fr.length = 0;
    }
    if (fr.body.empty()) {
      t.opaque.push_back({key, reason::kEmptyFrame, true});
      continue;
    }
    t.frames.push_back(std::move(fr));
  }
  return t;
}

std::string renderFrame(const Frame &f) {
  if (f.id.size() != 4) throw Failure(code::kInternal, "frame identifier \"" + f.id + "\" is not ID3v2.4");
  std::string extra;
  if (f.grouping) extra.push_back(static_cast<char>(f.group));
  if (f.encrypted) extra.push_back(static_cast<char>(f.method));
  if (f.hasLength) extra += encodeSynchsafe(f.length);
  const std::uint64_t size = extra.size() + std::uint64_t{f.body.size()};
  if (size > kMaxSynchsafe) throw Failure(code::kTooLarge, "the ID3v2 frame " + f.id + " is larger than 256 MiB");
  const auto format = static_cast<std::uint8_t>((f.grouping ? 0x40 : 0) | (f.compressed ? 0x08 : 0) |
                                                (f.encrypted ? 0x04 : 0) | (f.hasLength ? 0x01 : 0));
  std::string out = f.id + encodeSynchsafe(static_cast<std::uint32_t>(size));
  out.push_back(static_cast<char>(f.status));
  out.push_back(static_cast<char>(format));
  out += extra;
  out += f.body;
  return out;
}

std::string renderTag(const std::vector<Frame> &frames, std::uint64_t padding) {
  std::string body;
  for (const Frame &f : frames) body += renderFrame(f);
  const std::uint64_t size = body.size() + padding;
  if (size > kMaxSynchsafe) throw Failure(code::kTooLarge, "the ID3v2 tag would be larger than 256 MiB");
  std::string out("ID3\x04\x00\x00", 6);
  out += encodeSynchsafe(static_cast<std::uint32_t>(size));
  out += body;
  out.append(static_cast<std::size_t>(padding), '\0');
  return out;
}

// ---------------------------------------------------------------------------
// Text.

std::string latin1ToUTF8(std::string_view b) {
  std::string out;
  out.reserve(b.size());
  for (const char c : b) appendUTF8(out, static_cast<unsigned char>(c));
  return out;
}

namespace {

// decodeUTF16 decodes code units in the given order; nullopt on a lone
// surrogate or an odd length.
std::optional<std::string> decodeUTF16(std::string_view b, bool bigEndian) {
  if (b.size() % 2 != 0) return std::nullopt;
  std::string out;
  out.reserve(b.size());
  auto unit = [&](std::size_t i) -> std::uint32_t {
    const std::uint32_t x = u8(b, i), y = u8(b, i + 1);
    return bigEndian ? (x << 8) | y : (y << 8) | x;
  };
  for (std::size_t i = 0; i < b.size(); i += 2) {
    const std::uint32_t u = unit(i);
    if (u >= 0xD800 && u <= 0xDBFF) {
      if (i + 2 >= b.size()) return std::nullopt;
      const std::uint32_t lo = unit(i + 2);
      if (lo < 0xDC00 || lo > 0xDFFF) return std::nullopt;
      appendUTF8(out, 0x10000 + ((u - 0xD800) << 10) + (lo - 0xDC00));
      i += 2;
    } else if (u >= 0xDC00 && u <= 0xDFFF) {
      return std::nullopt;
    } else {
      appendUTF8(out, u);
    }
  }
  return out;
}

bool wide(std::uint8_t enc) { return enc == kUTF16 || enc == kUTF16BE; }

// UTF16Order tracks the byte order of the strings of one UTF-16 field.
struct UTF16Order {
  std::optional<bool> first;  // big endian?
};

// decodeOne decodes one string (no terminator inside) in enc. For UTF-16
// (1) the string must start with a byte order mark, or take the first
// string's order; an empty string needs neither.
std::optional<std::string> decodeOne(std::uint8_t enc, std::string_view b, UTF16Order &order) {
  switch (enc) {
    case kLatin1:
      return latin1ToUTF8(b);
    case kUTF8:
      if (!validUTF8(b)) return std::nullopt;
      return std::string(b);
    case kUTF16BE:
      return decodeUTF16(b, true);
    case kUTF16: {
      if (b.empty()) return std::string();
      bool big;
      if (b.size() >= 2 && u8(b, 0) == 0xFE && u8(b, 1) == 0xFF) {
        big = true;
        b.remove_prefix(2);
      } else if (b.size() >= 2 && u8(b, 0) == 0xFF && u8(b, 1) == 0xFE) {
        big = false;
        b.remove_prefix(2);
      } else if (order.first) {
        big = *order.first;
      } else {
        return std::nullopt;
      }
      if (!order.first) order.first = big;
      return decodeUTF16(b, big);
    }
  }
  return std::nullopt;
}

// terminatorAt returns the offset of the first terminator of enc in b (a
// zero byte, or an aligned zero code unit), or npos.
std::size_t terminatorAt(std::uint8_t enc, std::string_view b) {
  if (!wide(enc)) return b.find('\0');
  for (std::size_t i = 0; i + 1 < b.size(); i += 2) {
    if (b[i] == '\0' && b[i + 1] == '\0') return i;
  }
  return std::string_view::npos;
}

// trimmed removes the trailing terminators of a field (and, for UTF-16, a
// stray odd zero byte after the last code unit, as TagLib tolerates it).
std::string_view trimmed(std::uint8_t enc, std::string_view b) {
  if (!wide(enc)) {
    while (!b.empty() && b.back() == '\0') b.remove_suffix(1);
    return b;
  }
  if (b.size() % 2 != 0 && b.back() == '\0') b.remove_suffix(1);
  while (b.size() >= 2 && b.size() % 2 == 0 && b[b.size() - 1] == '\0' && b[b.size() - 2] == '\0') {
    b.remove_suffix(2);
  }
  return b;
}

}  // namespace

std::optional<std::vector<std::string>> decodeStrings(std::uint8_t enc, std::string_view b) {
  if (enc > kUTF8) return std::nullopt;
  b = trimmed(enc, b);
  if (wide(enc) && b.size() % 2 != 0) return std::nullopt;
  std::vector<std::string> out;
  UTF16Order order;
  const std::size_t step = wide(enc) ? 2 : 1;
  while (true) {
    const std::size_t end = terminatorAt(enc, b);
    const std::string_view piece = b.substr(0, end);
    auto s = decodeOne(enc, piece, order);
    if (!s) return std::nullopt;
    if (!s->empty()) out.push_back(std::move(*s));
    if (end == std::string_view::npos) break;
    b.remove_prefix(end + step);
  }
  return out;
}

std::optional<TextField> decodeTerminated(std::uint8_t enc, std::string_view b) {
  if (enc > kUTF8) return std::nullopt;
  const std::size_t end = terminatorAt(enc, b);
  if (end == std::string_view::npos) return std::nullopt;
  UTF16Order order;
  auto s = decodeOne(enc, b.substr(0, end), order);
  if (!s) return std::nullopt;
  return TextField{std::move(*s), b.substr(end + (wide(enc) ? 2 : 1))};
}

std::optional<std::string> decodeWhole(std::uint8_t enc, std::string_view b) {
  if (enc > kUTF8) return std::nullopt;
  b = trimmed(enc, b);
  UTF16Order order;
  return decodeOne(enc, b, order);
}

std::string_view id3v2Of22(std::string_view id) {
  // ID3v2.2 specification, section 4, and the iTunes frames (TCP, TST,
  // TSP, TSA, TS2, TSC). Frames ID3v2.4 dropped map to their ID3v2.3 form.
  static constexpr std::array<std::pair<std::string_view, std::string_view>, 67> kMap = {{
      {"BUF", "RBUF"}, {"CNT", "PCNT"}, {"COM", "COMM"}, {"CRA", "AENC"}, {"ETC", "ETCO"}, {"EQU", "EQUA"},
      {"GEO", "GEOB"}, {"IPL", "IPLS"}, {"MCI", "MCDI"}, {"MLL", "MLLT"}, {"PIC", "APIC"}, {"POP", "POPM"},
      {"REV", "RVRB"}, {"RVA", "RVAD"}, {"SLT", "SYLT"}, {"STC", "SYTC"}, {"TAL", "TALB"}, {"TBP", "TBPM"},
      {"TCM", "TCOM"}, {"TCO", "TCON"}, {"TCP", "TCMP"}, {"TCR", "TCOP"}, {"TDA", "TDAT"}, {"TDY", "TDLY"},
      {"TEN", "TENC"}, {"TFT", "TFLT"}, {"TIM", "TIME"}, {"TKE", "TKEY"}, {"TLA", "TLAN"}, {"TLE", "TLEN"},
      {"TMT", "TMED"}, {"TOA", "TOPE"}, {"TOF", "TOFN"}, {"TOL", "TOLY"}, {"TOR", "TORY"}, {"TOT", "TOAL"},
      {"TP1", "TPE1"}, {"TP2", "TPE2"}, {"TP3", "TPE3"}, {"TP4", "TPE4"}, {"TPA", "TPOS"}, {"TPB", "TPUB"},
      {"TRC", "TSRC"}, {"TRD", "TRDA"}, {"TRK", "TRCK"}, {"TS2", "TSO2"}, {"TSA", "TSOA"}, {"TSC", "TSOC"},
      {"TSI", "TSIZ"}, {"TSP", "TSOP"}, {"TSS", "TSSE"}, {"TST", "TSOT"}, {"TT1", "TIT1"}, {"TT2", "TIT2"},
      {"TT3", "TIT3"}, {"TXT", "TEXT"}, {"TXX", "TXXX"}, {"TYE", "TYER"}, {"UFI", "UFID"}, {"ULT", "USLT"},
      {"WAF", "WOAF"}, {"WAR", "WOAR"}, {"WAS", "WOAS"}, {"WCM", "WCOM"}, {"WCP", "WCOP"}, {"WPB", "WPUB"},
      {"WXX", "WXXX"},
  }};
  if (id.size() != 3) return {};
  for (const auto &[from, to] : kMap) {
    if (from == id) return to;
  }
  return {};
}

}  // namespace mltags::id3v2
