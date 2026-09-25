#include "mp3.h"

#include <sys/stat.h>

#include <algorithm>
#include <array>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <map>
#include <optional>
#include <string_view>

#include <taglib/apefooter.h>
#include <taglib/apetag.h>
#include <taglib/attachedpictureframe.h>
#include <taglib/id3v1genres.h>
#include <taglib/id3v2frame.h>
#include <taglib/id3v2header.h>
#include <taglib/id3v2tag.h>
#include <taglib/mpegfile.h>
#include <taglib/tstring.h>

#include "ape.h"
#include "failure.h"
#include "fdio.h"
#include "id3v2.h"
#include "sha256.h"
#include "text.h"

namespace mltags {

namespace {

constexpr std::uint64_t kID3v1Size = 128;
// How far FFmpeg's mp3 demuxer looks for the first two frames after the
// ID3v2 tag (mp3_read_header): the reader looks as far.
constexpr std::uint64_t kSyncScan = 64 * 1024;
// A layer III frame is at most 2,881 bytes (MPEG-1, 320 kbit/s, 32 kHz).
constexpr std::uint64_t kMaxFrameBytes = 2881;
// TagLib's default ID3v2 padding, and the most padding a write keeps from
// the original tag (its MaxPaddingSize): beyond it the tag is rebuilt with
// the default.
constexpr std::uint64_t kDefaultPadding = 1024;
constexpr std::uint64_t kMaxPadding = 1024 * 1024;
// The largest APE tag the reader reads, its items and footer: the bound of an
// ID3v2 tag (28-bit sizes). A footer declaring more is Failure(corrupt),
// before anything is read (NOTES.md N-163); internal/media applies the same
// bound (apeMaxTag).
constexpr std::uint64_t kMaxApeTag = 256 * 1024 * 1024;

[[noreturn]] void corrupt(const std::string &msg) { throw Failure(code::kCorrupt, msg); }
[[noreturn]] void internal(const std::string &msg) { throw Failure(code::kInternal, msg); }

std::uint64_t fileSize(int fd) {
  struct stat st {};
  if (fstat(fd, &st) != 0) throw Failure(code::kIO, std::string("stat: ") + std::strerror(errno));
  return static_cast<std::uint64_t>(st.st_size);
}

std::uint32_t be32(std::string_view d, std::size_t off) {
  return (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off])) << 24) |
         (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 1])) << 16) |
         (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 2])) << 8) |
         static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 3]));
}

std::string upperASCII(std::string_view s) {
  std::string out(s);
  for (char &c : out) {
    if (c >= 'a' && c <= 'z') c = static_cast<char>(c - 'a' + 'A');
  }
  return out;
}

// ---------------------------------------------------------------------------
// MPEG audio frames: only as much as telling an MP3 stream from something
// else, as FFmpeg's demuxer does (two consecutive layer III frames).

// layer3FrameBytes is the length of the MPEG layer III frame whose header is
// h, or 0 when h is not a valid layer III header.
std::uint64_t layer3FrameBytes(std::uint32_t h) {
  static constexpr std::array<std::uint32_t, 15> kRateV1 = {0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320};
  static constexpr std::array<std::uint32_t, 15> kRateV2 = {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160};
  static constexpr std::array<std::array<std::uint32_t, 3>, 4> kSampleRate = {
      {{11025, 12000, 8000}, {0, 0, 0}, {22050, 24000, 16000}, {44100, 48000, 32000}}};
  if ((h & 0xFFE00000U) != 0xFFE00000U) return 0;
  const std::uint32_t version = (h >> 19) & 3, layer = (h >> 17) & 3, rate = (h >> 12) & 0xF, sr = (h >> 10) & 3;
  if (version == 1 || layer != 1 || rate == 0 || rate == 15 || sr == 3) return 0;
  const std::uint64_t bitrate = std::uint64_t{(version == 3 ? kRateV1 : kRateV2)[rate]} * 1000;
  const std::uint64_t sampleRate = kSampleRate[version][sr];
  return (version == 3 ? 144 : 72) * bitrate / sampleRate + ((h >> 9) & 1);
}

// isMpegAudio reports whether two consecutive layer III frames of the same
// version and sample rate start within kSyncScan bytes of start.
bool isMpegAudio(int fd, std::uint64_t start, std::uint64_t end) {
  const std::uint64_t n = std::min(end - start, kSyncScan + 2 * kMaxFrameBytes + 4);
  const std::string d = readAt(fd, start, static_cast<std::size_t>(n), "the audio");
  for (std::uint64_t i = 0; i < kSyncScan && i + 4 <= d.size(); ++i) {
    const std::uint32_t h = be32(d, static_cast<std::size_t>(i));
    const std::uint64_t len = layer3FrameBytes(h);
    if (len == 0) continue;
    const std::uint64_t next = i + len;
    if (next == end - start) return true;  // a single frame, the whole stream
    if (next + 4 > d.size()) continue;
    const std::uint32_t h2 = be32(d, static_cast<std::size_t>(next));
    if (layer3FrameBytes(h2) != 0 && (h & 0xFFFE0C00U) == (h2 & 0xFFFE0C00U)) return true;
  }
  return false;
}

std::string hashRange(int fd, std::uint64_t start, std::uint64_t end) {
  SHA256 h;
  constexpr std::uint64_t kChunk = 1 << 20;
  for (std::uint64_t off = start; off < end;) {
    const std::uint64_t n = std::min(kChunk, end - off);
    h.update(readAt(fd, off, static_cast<std::size_t>(n), "the audio"));
    off += n;
  }
  return h.hex();
}

// ---------------------------------------------------------------------------
// The raw tags, read with pread only.

struct RawMp3 {
  std::uint64_t size = 0;
  std::optional<id3v2::Header> id3v2;
  std::string id3v2Bytes;  // the complete first tag
  bool stacked = false;    // another ID3v2 tag follows the first
  std::optional<std::string> id3v1;  // the 128 bytes
  std::optional<ape::Footer> ape;
  std::uint64_t apeStart = 0;
  std::vector<ape::Item> apeItems;
  bool lyrics3 = false;
  bool appendedID3v2 = false;
  AudioRange audio;
  std::string audioSHA256;
};

// readRawMp3 locates the tags, exactly where TagLib's MPEG::File finds them
// (NOTES.md N-154):
//   - an ID3v2 tag at the start of the file (TagLib with ReadStyle::Fast
//     looks nowhere else);
//   - an ID3v1 tag: "TAG" 128 bytes before the end, unless it is the "TAG"
//     of an "APETAGEX" that starts 131 bytes before the end
//     (Utils::findID3v1, the rule of the FLAC reader too);
//   - an APE tag: its footer ("APETAGEX") in the 32 bytes before the ID3v1
//     tag, or before the end (Utils::findAPE), its extent from the footer.
// The audio is what lies between; internal/media decodes exactly the bytes
// before its end (N-154). A structure that does not describe a readable tag
// is Failure(corrupt); a file without MPEG layer III frames after the tags
// is Failure(format_mismatch).
RawMp3 readRawMp3(int fd) {
  RawMp3 raw;
  raw.size = fileSize(fd);
  const std::uint64_t size = raw.size;
  std::uint64_t start = 0;
  if (size >= id3v2::kHeaderSize) {
    if (auto h = id3v2::parseHeader(readAt(fd, 0, id3v2::kHeaderSize, "the file"))) {
      const std::uint64_t extent = h->completeSize();
      if (extent > size) corrupt("the ID3v2 tag extends past the end of the file");
      raw.id3v2 = h;
      raw.id3v2Bytes = readAt(fd, 0, static_cast<std::size_t>(extent), "the ID3v2 tag");
      start = extent;
      if (size - start >= 3 && readAt(fd, start, 3, "the file") == "ID3") raw.stacked = true;
    }
  }
  std::uint64_t end = size;
  if (size >= kID3v1Size + 3) {
    const std::string t = readAt(fd, size - kID3v1Size - 3, 8, "the end of the file");
    if (t.compare(3, 3, "TAG") == 0 && t != "APETAGEX") raw.id3v1 = "";
  } else if (size >= kID3v1Size && readAt(fd, size - kID3v1Size, 3, "the end of the file") == "TAG") {
    raw.id3v1 = "";
  }
  if (raw.id3v1) {
    if (size - kID3v1Size < start) corrupt("an ID3v1 tag overlaps the ID3v2 tag");
    raw.id3v1 = readAt(fd, size - kID3v1Size, kID3v1Size, "the ID3v1 tag");
    end = size - kID3v1Size;
  }
  if (end >= ape::kFooterSize && readAt(fd, end - ape::kFooterSize, 8, "the end of the file") == "APETAGEX") {
    if (end - ape::kFooterSize < start) corrupt("an APE tag overlaps the ID3v2 tag");
    const ape::Footer f = *ape::parseFooter(readAt(fd, end - ape::kFooterSize, ape::kFooterSize, "the APE footer"));
    if (f.version != 1000 && f.version != 2000) corrupt("the APE tag has version " + std::to_string(f.version));
    if (f.isHeader()) corrupt("the APE footer is marked as a header");
    if (f.size < ape::kFooterSize || f.size > kMaxApeTag) corrupt("the APE tag has size " + std::to_string(f.size));
    if (f.count > ape::kMaxItems) corrupt("the APE tag declares " + std::to_string(f.count) + " items");
    if (f.completeSize() > end - start) corrupt("the APE tag extends before the start of the audio");
    raw.ape = f;
    raw.apeStart = end - f.completeSize();
    std::uint64_t items = raw.apeStart;
    if (f.hasHeader()) {
      const auto h = ape::parseFooter(readAt(fd, raw.apeStart, ape::kFooterSize, "the APE header"));
      if (!h || !h->isHeader() || h->version != f.version || h->size != f.size || h->count != f.count) {
        corrupt("the APE header does not match its footer");
      }
      items += ape::kFooterSize;
    }
    const std::string area = readAt(fd, items, f.size - ape::kFooterSize, "the APE items");
    raw.apeItems = ape::parseItems(area, f.count);
    end = raw.apeStart;
  }
  if (end <= start) throw Failure(code::kFormatMismatch, "not an MPEG audio stream: no audio between the tags");
  if (end - start >= 9) {
    const std::string t = readAt(fd, end - 9, 9, "the end of the audio");
    raw.lyrics3 = t == "LYRICS200" || t == "LYRICSEND";
  }
  if (end - start >= id3v2::kHeaderSize) raw.appendedID3v2 = readAt(fd, end - 10, 3, "the end of the audio") == "3DI";
  raw.audio = AudioRange{start, end};
  if (!isMpegAudio(fd, start, end)) {
    throw Failure(code::kFormatMismatch, "not an MPEG audio stream: no two consecutive layer III frames after the tags");
  }
  raw.audioSHA256 = hashRange(fd, start, end);
  return raw;
}

// ---------------------------------------------------------------------------
// Genres (TCON, ID3v1).

// genreName is the ID3v1 genre n (TagLib's list, 0..191), or "".
std::string genreName(unsigned n) { return TagLib::ID3v1::genre(static_cast<int>(n)).to8Bit(true); }

std::optional<unsigned> smallNumber(std::string_view s) {
  if (s.empty() || s.size() > 3) return std::nullopt;
  unsigned n = 0;
  for (const char c : s) {
    if (c < '0' || c > '9') return std::nullopt;
    n = n * 10 + static_cast<unsigned>(c - '0');
  }
  return n;
}

// decodeGenre is the reading of one TCON string: leading references "(n)"
// (ID3v1 genre n), "(RX)" (Remix) and "(CR)" (Cover), as ID3v2.3 writes
// them, then a refinement text; "((" escapes a literal "("; a string of digits
// alone is ID3v1 genre n, as ID3v2.4 writes it. A reference whose name is the
// refinement itself is not repeated. Anything else is the text itself.
std::vector<std::string> decodeGenre(std::string_view s) {
  std::vector<std::string> refs;
  while (s.size() >= 2 && s[0] == '(' && s[1] != '(') {
    const std::size_t close = s.find(')');
    if (close == std::string_view::npos) break;
    const std::string_view c = s.substr(1, close - 1);
    std::string name;
    if (c == "RX") {
      name = "Remix";
    } else if (c == "CR") {
      name = "Cover";
    } else if (const auto n = smallNumber(c)) {
      name = genreName(*n);
    }
    if (name.empty()) break;
    refs.push_back(std::move(name));
    s.remove_prefix(close + 1);
  }
  if (s.starts_with("((")) s.remove_prefix(1);
  std::string text(s);
  if (const auto n = smallNumber(text); n && !genreName(*n).empty()) text = genreName(*n);
  std::vector<std::string> out;
  for (auto &r : refs) {
    if (r != text) out.push_back(std::move(r));
  }
  if (!text.empty()) out.push_back(std::move(text));
  return out;
}

// ---------------------------------------------------------------------------
// From the raw tags to an inspection.

bool isTextFrame(std::string_view id) { return id.size() == 4 && id[0] == 'T' && id != "TXXX"; }

// frameValues decodes a plain text frame: its strings (TXXX: its values,
// after the description returned in desc). nullopt when it is not valid
// text.
std::optional<std::vector<std::string>> frameValues(const id3v2::Frame &f, std::string *desc) {
  const auto enc = static_cast<std::uint8_t>(f.body[0]);
  const std::string_view rest = std::string_view(f.body).substr(1);
  if (f.id != "TXXX") return id3v2::decodeStrings(enc, rest);
  const auto d = id3v2::decodeTerminated(enc, rest);
  if (!d) return std::nullopt;
  if (desc != nullptr) *desc = d->text;
  return id3v2::decodeStrings(enc, d->rest);
}

// txxxDescription is the description of a plain TXXX frame, or nullopt.
std::optional<std::string> txxxDescription(const id3v2::Frame &f) {
  const auto d = id3v2::decodeTerminated(static_cast<std::uint8_t>(f.body[0]), std::string_view(f.body).substr(1));
  if (!d) return std::nullopt;
  return d->text;
}

// flagsForm is the canonical value of a frame with flags: its flags and the
// hash of its stored data. Such a frame is kept byte for byte and compared
// as bytes.
std::string flagsForm(const id3v2::Frame &f) {
  const auto hex = [](std::uint8_t b) { return hexLower(std::string(1, static_cast<char>(b))); };
  const auto format = static_cast<std::uint8_t>((f.grouping ? 0x40 : 0) | (f.compressed ? 0x08 : 0) |
                                                (f.encrypted ? 0x04 : 0) | (f.hasLength ? 0x01 : 0));
  std::string out = "flags=" + hex(f.status) + hex(format);
  if (f.grouping) out += " group=" + hex(f.group);
  if (f.encrypted) out += " method=" + hex(f.method);
  if (f.hasLength) out += " length=" + std::to_string(f.length);
  return out + " sha256:" + sha256Hex(f.body);
}

// canonicalFrame is the canonical form of an unmanaged frame (NOTES.md
// N-153): a key naming the frame and what tells it apart from the frames of
// the same kind (TXXX: its description; COMM, USLT: language and
// description; WXXX: description; UFID, PRIV: owner), and its decoded
// values. nullopt when its text does not decode.
std::optional<KeyValues> canonicalFrame(const id3v2::Frame &f) {
  const std::string key = "id3v2:" + f.id;
  if (!f.plain()) return KeyValues{key, {flagsForm(f)}};
  const std::string_view b = f.body;
  const auto enc = static_cast<std::uint8_t>(b[0]);
  if (f.id == "TXXX" || isTextFrame(f.id)) {
    std::string desc;
    auto v = frameValues(f, &desc);
    if (!v) return std::nullopt;
    return KeyValues{f.id == "TXXX" ? key + ":" + desc : key, std::move(*v)};
  }
  if (f.id == "COMM" || f.id == "USLT") {
    if (b.size() < 4) return std::nullopt;
    const auto d = id3v2::decodeTerminated(enc, b.substr(4));
    if (!d) return std::nullopt;
    auto text = id3v2::decodeWhole(enc, d->rest);
    if (!text) return std::nullopt;
    return KeyValues{key + ":" + id3v2::latin1ToUTF8(b.substr(1, 3)) + ":" + d->text, {std::move(*text)}};
  }
  const auto latin1Trimmed = [](std::string_view s) {
    while (!s.empty() && s.back() == '\0') s.remove_suffix(1);
    return id3v2::latin1ToUTF8(s);
  };
  if (f.id == "WXXX") {
    const auto d = id3v2::decodeTerminated(enc, b.substr(1));
    if (!d) return std::nullopt;
    return KeyValues{key + ":" + d->text, {latin1Trimmed(d->rest)}};
  }
  if (f.id[0] == 'W') return KeyValues{key, {latin1Trimmed(b)}};
  if (f.id == "UFID" || f.id == "PRIV") {
    const std::size_t nul = b.find('\0');
    if (nul == std::string_view::npos) return std::nullopt;
    const std::string owner = key + ":" + id3v2::latin1ToUTF8(b.substr(0, nul));
    const std::string_view data = b.substr(nul + 1);
    return KeyValues{owner, {f.id == "UFID" ? "hex:" + hexLower(data) : "sha256:" + sha256Hex(data)}};
  }
  return KeyValues{key, {"sha256:" + sha256Hex(b)}};
}

// parsePicture decodes an APIC frame (or an ID3v2.2 PIC); nullopt when
// its structure is invalid.
std::optional<Picture> parseAPIC(const id3v2::Frame &f) {
  std::string_view b = f.body;
  const auto enc = static_cast<std::uint8_t>(b[0]);
  b.remove_prefix(1);
  Picture p;
  p.location = "id3v2";
  if (f.sourceID == "PIC") {
    if (b.size() < 4) return std::nullopt;
    const std::string format = upperASCII(b.substr(0, 3));
    p.mime = format == "JPG" ? "image/jpeg" : format == "PNG" ? "image/png" : "";
    b.remove_prefix(3);
  } else {
    const std::size_t nul = b.find('\0');
    if (nul == std::string_view::npos) return std::nullopt;
    p.mime = id3v2::latin1ToUTF8(b.substr(0, nul));
    b.remove_prefix(nul + 1);
  }
  if (b.empty()) return std::nullopt;
  p.type = static_cast<unsigned char>(b[0]);
  const auto d = id3v2::decodeTerminated(enc, b.substr(1));
  if (!d || d->rest.empty()) return std::nullopt;
  p.data.assign(d->rest);
  return p;
}

// validApeKey is the APEv2 rule for keys, which TagLib applies: 2 to 255
// characters of 0x20..0x7E, not ID3, TAG, OggS or MP+.
bool validApeKey(std::string_view k) {
  if (k.size() < 2 || k.size() > 255) return false;
  for (const char c : k) {
    if (c < 0x20 || c > 0x7E) return false;
  }
  const std::string u = upperASCII(k);
  return u != "ID3" && u != "TAG" && u != "OGGS" && u != "MP+";
}

// apeText returns the values of a text item (NUL-separated, empty ones
// dropped), or nullopt when the item is not UTF-8 text.
std::optional<std::vector<std::string>> apeText(const ape::Item &it) {
  if ((it.flags & 6U) != 0 || !validUTF8(it.value)) return std::nullopt;
  std::vector<std::string> out;
  std::string_view v = it.value;
  while (true) {
    const std::size_t nul = v.find('\0');
    if (nul != 0 && !v.empty()) out.emplace_back(v.substr(0, nul));
    if (nul == std::string_view::npos) break;
    v.remove_prefix(nul + 1);
  }
  return out;
}

std::uint32_t apePictureType(std::string_view upperKey) {
  if (upperKey == "COVER ART (FRONT)") return kFrontCover;
  if (upperKey == "COVER ART (BACK)") return 4;
  return 0;
}

// kWhiteSpace is the white space TagLib's String::stripWhiteSpace removes.
constexpr std::string_view kWhiteSpace = "\t\n\f\r ";

// ID3v1 text: Latin-1 up to the first NUL, without surrounding white space
// (TagLib's StringHandler).
std::string id3v1Text(std::string_view b) {
  b = b.substr(0, b.find('\0'));
  const std::size_t first = b.find_first_not_of(kWhiteSpace);
  if (first == std::string_view::npos) return {};
  return id3v2::latin1ToUTF8(b.substr(first, b.find_last_not_of(kWhiteSpace) - first + 1));
}

struct Mp3Analysis {
  std::optional<id3v2::Tag> tag;
  // The sources of each managed field, in reading order (§8.1).
  std::array<std::vector<KeyValues>, kFieldCount> sources;
  std::vector<Picture> pictures;
  std::vector<Opaque> opaque;
  std::map<std::string, std::vector<std::string>> unmanaged;
  std::vector<std::size_t> keptFrames;  // indexes into tag->frames
  std::vector<std::size_t> keptItems;   // indexes into the APE items
  std::optional<std::string> comment;   // the non-empty ID3v1 comment
  bool commentKept = false;             // an unmanaged COMM already holds it
};

void addSource(std::vector<KeyValues> &sources, std::string key, std::vector<std::string> values) {
  if (!values.empty()) sources.push_back(KeyValues{std::move(key), std::move(values)});
}

// splitNumbers splits "N/M" values as the FLAC reader does.
void splitNumbers(const std::vector<std::string> &values, std::vector<std::string> &numbers,
                  std::vector<std::string> &totals) {
  for (const auto &v : values) {
    const std::size_t slash = v.find('/');
    std::string n = v.substr(0, slash);
    if (!n.empty()) numbers.push_back(std::move(n));
    if (slash != std::string::npos && slash + 1 < v.size()) totals.push_back(v.substr(slash + 1));
  }
}

// ID3v2: the managed frames as sources, the pictures, the kept frames.
void analyzeID3v2(const id3v2::Tag &tag, Mp3Analysis &a, std::array<std::vector<KeyValues>, kFieldCount> &out) {
  std::map<std::string, std::vector<std::string>> values;  // source key -> values, in file order
  for (std::size_t i = 0; i < tag.frames.size(); ++i) {
    const id3v2::Frame &f = tag.frames[i];
    const std::string key = "id3v2:" + f.id;
    auto unreadable = [&](const std::string &k) {
      if (f.compressed || f.encrypted) {
        a.opaque.push_back({k, f.compressed ? reason::kCompressedFrame : reason::kEncryptedFrame, true});
        return true;
      }
      return false;
    };
    if (f.id == kId3PictureFrame) {
      if (unreadable(key)) continue;
      if (auto p = parseAPIC(f)) {
        a.pictures.push_back(std::move(*p));
      } else {
        a.opaque.push_back({key, reason::kInvalidPicture, true});
      }
      continue;
    }
    if (std::find(kId3OldDateFrames.begin(), kId3OldDateFrames.end(), f.id) != kId3OldDateFrames.end() ||
        std::find(kId3SortFrames.begin(), kId3SortFrames.end(), f.id) != kId3SortFrames.end()) {
      continue;  // removed, not read
    }
    // Which source of a managed field the frame is, if any.
    std::string source;
    for (const Id3Field &m : kId3Fields) {
      if (!m.frame.empty() && f.id == m.frame) source = key;
      if (!m.aliasFrames[0].empty() && f.id == m.aliasFrames[0]) source = key;
    }
    if (f.id == "TXXX" && f.plain()) {
      const auto desc = txxxDescription(f);
      if (!desc) {
        a.opaque.push_back({key, reason::kInvalidText, false});
        a.keptFrames.push_back(i);
        continue;
      }
      const std::string upper = upperASCII(*desc);
      if (std::find(kId3SortTXXX.begin(), kId3SortTXXX.end(), upper) != kId3SortTXXX.end()) continue;
      for (const Id3Field &m : kId3Fields) {
        for (const auto alias : m.txxxAliases) {
          if (!alias.empty() && upper == alias) source = key + ":" + upper;
        }
      }
    }
    if (source.empty()) {
      a.keptFrames.push_back(i);
      continue;
    }
    if (unreadable(source)) continue;
    auto v = frameValues(f, nullptr);
    if (!v) {
      a.opaque.push_back({source, reason::kInvalidText, true});
      continue;
    }
    if (f.id == "TCON") {
      std::vector<std::string> genres;
      for (const auto &s : *v) {
        for (auto &g : decodeGenre(s)) genres.push_back(std::move(g));
      }
      v = std::move(genres);
    }
    auto &dst = values[source];
    dst.insert(dst.end(), v->begin(), v->end());
  }
  for (const Id3Field &m : kId3Fields) {
    auto &dst = out[idx(m.field)];
    if (!m.frame.empty()) {
      const std::string key = "id3v2:" + std::string(m.frame);
      const auto &v = values[key];
      if (m.field == Field::Track || m.field == Field::Disc) {
        std::vector<std::string> numbers, totals;
        splitNumbers(v, numbers, totals);
        addSource(dst, key, std::move(numbers));
        // The "/M" is the canonical source of the total (fields.h).
        addSource(out[idx(m.field == Field::Track ? Field::TrackTotal : Field::DiscTotal)], key + "/total",
                  std::move(totals));
      } else {
        addSource(dst, key, v);
      }
    }
    if (!m.aliasFrames[0].empty()) {
      const std::string key = "id3v2:" + std::string(m.aliasFrames[0]);
      addSource(dst, key, values[key]);
    }
    for (const auto alias : m.txxxAliases) {
      if (alias.empty()) continue;
      const std::string key = "id3v2:TXXX:" + std::string(alias);
      addSource(dst, key, values[key]);
    }
  }
}

// APE: the managed items as sources, the pictures, the kept items.
void analyzeAPE(const std::vector<ape::Item> &items, Mp3Analysis &a,
                std::array<std::vector<KeyValues>, kFieldCount> &out) {
  std::map<std::string, std::vector<std::string>> values;
  for (std::size_t i = 0; i < items.size(); ++i) {
    const ape::Item &it = items[i];
    if (!validApeKey(it.key)) {
      a.opaque.push_back({"ape#" + std::to_string(i), reason::kInvalidKey, false});
      a.keptItems.push_back(i);
      continue;
    }
    const std::string upper = upperASCII(it.key);
    const std::string key = "ape:" + upper;
    if (upper.starts_with(kApePicturePrefix)) {
      const std::size_t nul = it.value.find('\0');
      if ((it.flags & 6U) != 2 || nul == std::string::npos || nul + 1 >= it.value.size()) {
        a.opaque.push_back({key, reason::kInvalidPicture, true});
      } else {
        a.pictures.push_back(Picture{"ape", apePictureType(upper), "", 0, 0, 0, 0, it.value.substr(nul + 1)});
      }
      continue;
    }
    if (!isApeManagedKey(upper)) {
      a.keptItems.push_back(i);
      continue;
    }
    if (std::find(kApeSortKeys.begin(), kApeSortKeys.end(), upper) != kApeSortKeys.end()) continue;
    auto v = apeText(it);
    if (!v) {
      a.opaque.push_back({key, reason::kInvalidUTF8, true});
      continue;
    }
    auto &dst = values[upper];
    dst.insert(dst.end(), v->begin(), v->end());
  }
  for (const ApeField &m : kApeFields) {
    auto &dst = out[idx(m.field)];
    const bool numbered = m.field == Field::Track || m.field == Field::Disc;
    bool haveTotal = false;
    for (const auto k : m.keys) {
      if (k.empty()) continue;
      const std::string key(k);
      const auto &v = values[key];
      if (!numbered) {
        addSource(dst, "ape:" + key, v);
        continue;
      }
      std::vector<std::string> numbers, totals;
      splitNumbers(v, numbers, totals);
      addSource(dst, "ape:" + key, std::move(numbers));
      if (!haveTotal && !v.empty()) {
        // The totals come from the first number key that is present.
        haveTotal = true;
        addSource(out[idx(m.field == Field::Track ? Field::TrackTotal : Field::DiscTotal)], "ape:" + key + "/total",
                  std::move(totals));
      }
    }
  }
}

// ID3v1 (ID3v1.1 when byte 125 is 0 and byte 126 is not: the track).
void analyzeID3v1(const std::string &t, Mp3Analysis &a, std::array<std::vector<KeyValues>, kFieldCount> &out) {
  const std::string_view b = t;
  auto text = [&](Field f, const char *key, std::size_t off, std::size_t len) {
    std::string s = id3v1Text(b.substr(off, len));
    if (!s.empty()) out[idx(f)].push_back(KeyValues{key, {std::move(s)}});
  };
  text(Field::Title, "id3v1:title", 3, 30);
  text(Field::Artist, "id3v1:artist", 33, 30);
  text(Field::Album, "id3v1:album", 63, 30);
  text(Field::Date, "id3v1:year", 93, 4);
  const bool v11 = b[125] == '\0' && b[126] != '\0';
  std::string comment = id3v1Text(b.substr(97, v11 ? 28 : 30));
  if (v11) {
    out[idx(Field::Track)].push_back(KeyValues{"id3v1:track", {std::to_string(static_cast<unsigned char>(b[126]))}});
  }
  if (const std::string g = genreName(static_cast<unsigned char>(b[127])); !g.empty()) {
    out[idx(Field::Genre)].push_back(KeyValues{"id3v1:genre", {g}});
  }
  if (!comment.empty()) a.comment = std::move(comment);
}

Mp3Analysis analyze(const RawMp3 &raw) {
  Mp3Analysis a;
  std::array<std::vector<KeyValues>, kFieldCount> id3, apeSources, v1;
  if (raw.id3v2) {
    a.tag = id3v2::parseTag(raw.id3v2Bytes, *raw.id3v2);
    a.opaque.insert(a.opaque.end(), a.tag->opaque.begin(), a.tag->opaque.end());
    analyzeID3v2(*a.tag, a, id3);
  }
  if (raw.stacked) a.opaque.push_back({"id3v2", reason::kDuplicateTag, false});
  if (raw.ape) analyzeAPE(raw.apeItems, a, apeSources);
  if (raw.id3v1) analyzeID3v1(*raw.id3v1, a, v1);
  if (raw.lyrics3) a.opaque.push_back({"lyrics3", reason::kForeignTag, false});
  if (raw.appendedID3v2) a.opaque.push_back({"id3v2.appended", reason::kForeignTag, false});
  for (std::size_t f = 0; f < kFieldCount; ++f) {
    for (auto *s : {&id3[f], &apeSources[f], &v1[f]}) {
      a.sources[f].insert(a.sources[f].end(), s->begin(), s->end());
    }
  }

  auto blocking = [&](std::string key, const char *why) { a.opaque.push_back({std::move(key), why, false}); };
  for (const std::size_t i : a.keptFrames) {
    const id3v2::Frame &f = a.tag->frames[i];
    if (f.id == "TXXX" && f.plain() && !txxxDescription(f)) continue;  // reported above
    auto kv = canonicalFrame(f);
    if (!kv) {
      blocking("id3v2:" + f.id, reason::kInvalidText);
      continue;
    }
    auto &dst = a.unmanaged[kv->key];
    dst.insert(dst.end(), kv->values.begin(), kv->values.end());
  }
  for (const std::size_t i : a.keptItems) {
    const ape::Item &it = raw.apeItems[i];
    if (!validApeKey(it.key)) continue;  // reported above
    const std::string key = "ape:" + upperASCII(it.key);
    std::vector<std::string> v;
    if (it.flags == 0) {
      auto t = apeText(it);
      if (!t) {
        blocking(key, reason::kInvalidUTF8);
        continue;
      }
      v = std::move(*t);
    } else {
      char flags[9];
      std::snprintf(flags, sizeof flags, "%08x", it.flags);
      v = {std::string("flags=") + flags + " sha256:" + sha256Hex(it.value)};
    }
    auto &dst = a.unmanaged[key];
    dst.insert(dst.end(), v.begin(), v.end());
  }
  if (a.comment) {
    a.unmanaged["id3v1:comment"] = {*a.comment};
    const std::string legacy = "id3v2:COMM:" + std::string(kLegacyCommentLanguage) + ":" +
                               std::string(kLegacyCommentDescription);
    for (const auto &[key, values] : a.unmanaged) {
      if (key.starts_with("id3v2:COMM:") && std::find(values.begin(), values.end(), *a.comment) != values.end()) {
        a.commentKept = true;
      }
    }
    // A second "legacy-id3v1" comment with another text would break the
    // uniqueness of COMM frames (ID3v2.4 frames §4.10).
    if (!a.commentKept && a.unmanaged.contains(legacy)) blocking("id3v1:comment", reason::kMigrationConflict);
  }
  a.unmanaged["mpeg.audio"] = {raw.audioSHA256};
  return a;
}

// id3v1Truncation reports whether v1, the value of a 30-byte ID3v1 text
// field, is full cut to what that field can hold: its first 30 characters,
// all of them Latin-1, without surrounding white space (id3v1Text). Such a
// value agrees with full, not a conflict (NOTES.md N-164).
bool id3v1Truncation(const std::string &full, const std::string &v1) {
  std::string prefix;
  std::size_t chars = 0;
  for (std::size_t i = 0; i < full.size() && chars < 30; ++chars) {
    const auto c = static_cast<unsigned char>(full[i]);
    const std::size_t len = c < 0x80 ? 1 : c < 0xE0 ? 2 : c < 0xF0 ? 3 : 4;
    // Latin-1 is U+0000..U+00FF: one byte, or two starting with 0xC2, 0xC3.
    if (len > 2 || (len == 2 && c > 0xC3)) return false;
    prefix.append(full, i, len);
    i += len;
  }
  const std::size_t first = prefix.find_first_not_of(kWhiteSpace);
  if (first == std::string::npos) return false;
  return prefix.substr(first, prefix.find_last_not_of(kWhiteSpace) - first + 1) == v1;
}

// agrees reports whether a source agrees with the one that wins: the same
// values, or an ID3v1 title, artist or album that is its truncation.
bool agrees(const KeyValues &s, const KeyValues &winner) {
  if (s.values == winner.values) return true;
  const bool text30 = s.key == "id3v1:title" || s.key == "id3v1:artist" || s.key == "id3v1:album";
  return text30 && s.values.size() == 1 && winner.values.size() == 1 && id3v1Truncation(winner.values[0], s.values[0]);
}

Inspection toInspection(const Mp3Analysis &a, const RawMp3 &raw) {
  Inspection in;
  in.format = Format::MP3;
  in.audio = raw.audio;
  for (std::size_t f = 0; f < kFieldCount; ++f) {
    const auto &sources = a.sources[f];
    if (sources.empty()) continue;
    in.managed[f] = sources.front().values;
    const bool disagree = std::any_of(sources.begin() + 1, sources.end(),
                                      [&](const KeyValues &s) { return !agrees(s, sources.front()); });
    if (disagree) in.conflicts.push_back(Conflict{static_cast<Field>(f), sources});
  }
  in.pictures = a.pictures;
  for (const auto &[key, values] : a.unmanaged) in.unmanaged.push_back(KeyValues{key, values});
  in.opaque = a.opaque;
  return in;
}

// ---------------------------------------------------------------------------
// TagLib.

// checkTagLibAgrees verifies that TagLib finds the same tags in the same
// places: the ID3v2 tag at the start, the ID3v1 tag, the APE tag and its
// extent. Anything else is Failure(internal).
void checkTagLibAgrees(int fd, const RawMp3 &raw) {
  FdStream stream(fd, false);
  TagLib::MPEG::File file(&stream, false, TagLib::MPEG::Properties::Fast);
  stream.check();
  if (!file.isValid()) corrupt("TagLib cannot read the MP3 file");
  if (file.hasID3v2Tag() != raw.id3v2.has_value() || file.hasID3v1Tag() != raw.id3v1.has_value() ||
      file.hasAPETag() != raw.ape.has_value()) {
    internal("TagLib and the helper disagree on the tags of the MP3 file");
  }
  // TagLib counts a second ID3v2 tag into the first; such a file is refused
  // anyway (duplicate_tag).
  if (raw.id3v2 && !raw.stacked && file.ID3v2Tag()->header()->completeTagSize() != raw.audio.start) {
    internal("TagLib and the helper disagree on the size of the ID3v2 tag");
  }
  if (raw.ape && file.APETag()->footer()->completeTagSize() != raw.ape->completeSize()) {
    internal("TagLib and the helper disagree on the size of the APE tag");
  }
}

// ---------------------------------------------------------------------------
// The write.

// managedText is the text a write puts in the frame of a managed field, ""
// for none (TRCK and TPOS carry the totals).
std::string managedText(const ManagedValues &mv, Field f) {
  auto number = [&](Field n, Field total) {
    if (!mv.number[idx(n)]) {
      if (mv.number[idx(total)]) {
        throw Failure(code::kInvalidRequest, "an MP3 " + std::string(kFieldNames[idx(total)]) + " needs its number: TRCK and TPOS hold both");
      }
      return std::string();
    }
    std::string s = std::to_string(*mv.number[idx(n)]);
    if (mv.number[idx(total)]) s += "/" + std::to_string(*mv.number[idx(total)]);
    return s;
  };
  switch (f) {
    case Field::Track:
      return number(Field::Track, Field::TrackTotal);
    case Field::Disc:
      return number(Field::Disc, Field::DiscTotal);
    case Field::Compilation:
      return mv.compilation ? "1" : "";
    default:
      return mv.text[idx(f)].value_or("");
  }
}

id3v2::Frame textFrame(std::string_view id, std::string_view text) {
  id3v2::Frame f;
  f.id = f.sourceID = std::string(id);
  f.body = "\x03" + std::string(text);  // UTF-8
  return f;
}

// newFrames is the frame list of the written tag: the managed frames, the
// kept frames in their order, the migrated ID3v1 comment, the cover.
std::vector<id3v2::Frame> newFrames(const Mp3Analysis &a, const WriteRequest &req, const std::string &cover) {
  std::vector<id3v2::Frame> frames;
  for (const Id3Field &m : kId3Fields) {
    if (m.frame.empty()) continue;
    const std::string text = managedText(req.values, m.field);
    if (text.empty()) continue;
    // A genre that a reader would take for ID3v1 genre references: this
    // reader (decodeGenre), or TagLib, which consumes every leading "(...)"
    // (FrameFactory::updateGenre) and keeps nothing of "(Rock)".
    if (m.field == Field::Genre &&
        (decodeGenre(text) != std::vector<std::string>{text} || (text.starts_with("(") && text.find(')') != std::string::npos))) {
      throw Failure(code::kInvalidRequest, "tags.genre \"" + text +
                                               "\" cannot be written to an MP3: it reads back as an ID3v1 genre reference");
    }
    frames.push_back(textFrame(m.frame, text));
  }
  for (const std::size_t i : a.keptFrames) frames.push_back(a.tag->frames[i]);
  if (a.comment && !a.commentKept) {
    id3v2::Frame f;
    f.id = f.sourceID = "COMM";
    f.body = "\x03" + std::string(kLegacyCommentLanguage) + std::string(kLegacyCommentDescription) + '\0' + *a.comment;
    frames.push_back(std::move(f));
  }
  if (req.cover) {
    id3v2::Frame f;
    f.id = f.sourceID = std::string(kId3PictureFrame);
    // Latin-1 (the empty description needs nothing else), MIME, type,
    // empty description, data.
    f.body = std::string("\x00", 1) + req.cover->mime + '\0' + static_cast<char>(kFrontCover) + '\0' + cover;
    frames.push_back(std::move(f));
  }
  return frames;
}

bool sameFrame(const id3v2::Frame &x, const id3v2::Frame &y) {
  return x.id == y.id && x.status == y.status && x.grouping == y.grouping && x.group == y.group &&
         x.compressed == y.compressed && x.encrypted == y.encrypted && x.method == y.method &&
         x.hasLength == y.hasLength && x.length == y.length && x.body == y.body;
}

// expectedManaged is how the reader must report the managed fields after
// the write.
std::array<std::vector<std::string>, kFieldCount> expectedManaged(const ManagedValues &mv) {
  std::array<std::vector<std::string>, kFieldCount> out;
  for (std::size_t i = 0; i < kFieldCount; ++i) {
    const auto f = static_cast<Field>(i);
    std::string v;
    if (f == Field::Compilation) {
      v = mv.compilation ? "1" : "";
    } else if (isNumberField(f)) {
      v = mv.number[i] ? std::to_string(*mv.number[i]) : "";
    } else {
      v = mv.text[i].value_or("");
    }
    if (!v.empty()) out[i] = {v};
  }
  return out;
}

// verifyWritten reads the written file back, with the helper's reader and
// with TagLib, and compares it with what was asked: exactly the frames
// written, the kept APE items, no ID3v1, the same audio bytes, the managed
// values and the cover. Any difference is Failure(internal).
void verifyWritten(int fd, const RawMp3 &before, const std::vector<id3v2::Frame> &frames, std::uint64_t tagSize,
                   const std::vector<ape::Item> &items, const WriteRequest &req, const std::string &cover) {
  RawMp3 raw;
  try {
    raw = readRawMp3(fd);
  } catch (const Failure &f) {
    if (std::string(f.code()) == code::kIO) throw;
    internal(std::string("the written file does not read back: ") + f.what());
  }
  if (!raw.id3v2 || raw.id3v2->major != 4 || raw.stacked || raw.id3v1 || raw.lyrics3 || raw.appendedID3v2) {
    internal("the written file does not have exactly one ID3v2.4 tag and no ID3v1 tag");
  }
  if (raw.audio.start != tagSize || raw.audioSHA256 != before.audioSHA256 ||
      raw.audio.end - raw.audio.start != before.audio.end - before.audio.start) {
    internal("the audio of the written file is not the original audio");
  }
  const id3v2::Tag tag = id3v2::parseTag(raw.id3v2Bytes, *raw.id3v2);
  if (!tag.opaque.empty() || tag.frames.size() != frames.size() ||
      !std::equal(frames.begin(), frames.end(), tag.frames.begin(), sameFrame)) {
    internal("the written ID3v2 tag does not read back as written");
  }
  if (items.empty() != !raw.ape.has_value()) internal("the written APE tag is not the expected one");
  if (raw.ape) {
    if (raw.ape->version != before.ape->version || raw.ape->hasHeader() != before.ape->hasHeader() ||
        raw.apeItems.size() != items.size() ||
        !std::equal(items.begin(), items.end(), raw.apeItems.begin(),
                    [](const ape::Item &x, const ape::Item &y) { return x.raw == y.raw; })) {
      internal("the written APE tag does not keep the unmanaged items");
    }
  }
  const Inspection in = toInspection(analyze(raw), raw);
  if (!in.opaque.empty() || !in.conflicts.empty() || in.managed != expectedManaged(req.values)) {
    internal("the written managed fields do not read back as requested");
  }
  if (!req.cover) {
    if (!in.pictures.empty()) internal("the written file still has pictures");
  } else if (in.pictures.size() != 1 || in.pictures[0].type != kFrontCover || in.pictures[0].mime != req.cover->mime ||
             in.pictures[0].data != cover) {
    internal("the written picture is not the requested cover");
  }

  // TagLib, an independent parser, must read the same tags.
  FdStream stream(fd, false);
  TagLib::MPEG::File file(&stream, false, TagLib::MPEG::Properties::Fast);
  stream.check();
  const TagLib::ID3v2::Tag *id3 = file.hasID3v2Tag() ? file.ID3v2Tag() : nullptr;
  if (id3 == nullptr || id3->header()->majorVersion() != 4 || id3->header()->completeTagSize() != tagSize ||
      id3->frameList().size() != frames.size() || file.hasID3v1Tag() || file.hasAPETag() != raw.ape.has_value() ||
      (raw.ape && file.APETag()->footer()->completeTagSize() != raw.ape->completeSize())) {
    internal("TagLib does not read the written tags as written");
  }
  for (const Id3Field &m : kId3Fields) {
    if (m.frame.empty()) continue;
    const std::string want = managedText(req.values, m.field);
    const TagLib::ByteVector id(m.frame.data(), static_cast<unsigned int>(m.frame.size()));
    const auto it = id3->frameListMap().find(id);
    const bool present = it != id3->frameListMap().end() && !it->second.isEmpty();
    if (present != !want.empty() ||
        (present && (it->second.size() != 1 || it->second.front()->toString().to8Bit(true) != want))) {
      internal("TagLib does not read the written " + std::string(m.frame) + " frame as written");
    }
  }
  if (req.cover) {
    const auto it = id3->frameListMap().find("APIC");
    const auto *pic = it == id3->frameListMap().end() || it->second.size() != 1
                          ? nullptr
                          : dynamic_cast<const TagLib::ID3v2::AttachedPictureFrame *>(it->second.front());
    if (pic == nullptr || pic->type() != TagLib::ID3v2::AttachedPictureFrame::FrontCover ||
        pic->mimeType().to8Bit(true) != req.cover->mime ||
        pic->picture() != TagLib::ByteVector(cover.data(), static_cast<unsigned int>(cover.size()))) {
      internal("TagLib does not read the written cover as written");
    }
  }
}

}  // namespace

Inspection inspectMp3(int fd) {
  const RawMp3 raw = readRawMp3(fd);
  const Mp3Analysis a = analyze(raw);
  checkTagLibAgrees(fd, raw);
  return toInspection(a, raw);
}

void writeMp3(int fd, const WriteRequest &req, const std::string &cover) {
  // 1. What is on disk; nothing is written if a field would be lost.
  const RawMp3 raw = readRawMp3(fd);
  const Mp3Analysis a = analyze(raw);
  refuseBlocking(a.opaque);
  checkTagLibAgrees(fd, raw);

  // 2. The new tags, complete before anything is written.
  const std::vector<id3v2::Frame> frames = newFrames(a, req, cover);
  std::uint64_t framesSize = 0;
  for (const auto &f : frames) framesSize += id3v2::renderFrame(f).size();
  // The padding: the original tag's room when the frames fit in it and
  // leave at most kMaxPadding (the audio does not move), else the default.
  // A second write of the output therefore gives the same bytes.
  std::uint64_t padding = kDefaultPadding;
  if (raw.id3v2) {
    const std::uint64_t room = raw.audio.start - id3v2::kHeaderSize;
    if (framesSize <= room && room - framesSize <= kMaxPadding) padding = room - framesSize;
  }
  const std::string tag = id3v2::renderTag(frames, padding);
  std::vector<ape::Item> items;
  for (const std::size_t i : a.keptItems) items.push_back(raw.apeItems[i]);
  // An APE tag left without items holds nothing: it is removed.
  const std::string apeTag = items.empty() ? std::string() : ape::render(*raw.ape, items);

  // 3. Write: the tag replaces the old one (the audio moves when the size
  //    changes), the APE tag follows the audio, and the file ends there: no
  //    ID3v1 tag (§8.3).
  FdStream stream(fd, true);
  stream.insert(TagLib::ByteVector(tag.data(), static_cast<unsigned int>(tag.size())), 0,
                static_cast<std::size_t>(raw.audio.start));
  const std::uint64_t audioEnd = tag.size() + (raw.audio.end - raw.audio.start);
  stream.seek(static_cast<TagLib::offset_t>(audioEnd), TagLib::IOStream::Beginning);
  stream.writeBlock(TagLib::ByteVector(apeTag.data(), static_cast<unsigned int>(apeTag.size())));
  stream.truncate(static_cast<TagLib::offset_t>(audioEnd + apeTag.size()));
  stream.check();

  // 4. Read it back.
  verifyWritten(fd, raw, frames, tag.size(), items, req, cover);
}

}  // namespace mltags
