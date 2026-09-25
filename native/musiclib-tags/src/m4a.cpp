#include "m4a.h"

#include <sys/stat.h>

#include <algorithm>
#include <array>
#include <cerrno>
#include <cstring>
#include <limits>
#include <iterator>
#include <map>
#include <optional>
#include <string_view>

#include <taglib/id3v1genres.h>
#include <taglib/mp4atom.h>
#include <taglib/mp4coverart.h>
#include <taglib/mp4file.h>
#include <taglib/mp4item.h>
#include <taglib/mp4properties.h>
#include <taglib/mp4tag.h>
#include <taglib/tstring.h>
#include <taglib/tstringlist.h>

#include "failure.h"
#include "fdio.h"
#include "id3v2.h"
#include "sha256.h"
#include "text.h"

namespace mltags {

namespace {

using mp4::Box;

// The padding a write adds when the new metadata does not fit the old room:
// one "free" box of this size, right after the ilst. The old room is kept
// when the new metadata fits and leaves at most kMaxPadding: a second write
// of the output then gives the same bytes (the rule of the MP3 writer,
// NOTES.md N-152).
constexpr std::uint64_t kDefaultPadding = 1024;
constexpr std::uint64_t kMaxPadding = 1024 * 1024;
// The buffer of the samples hash.
constexpr std::uint64_t kReadChunk = 1 << 20;

[[noreturn]] void corrupt(const std::string &msg) { throw Failure(code::kCorrupt, msg); }
[[noreturn]] void internal(const std::string &msg) { throw Failure(code::kInternal, msg); }
[[noreturn]] void unsupported(const std::string &msg) { throw Failure(code::kUnsupportedFormat, msg); }

std::uint64_t fileSize(int fd) {
  struct stat st {};
  if (fstat(fd, &st) != 0) throw Failure(code::kIO, std::string("stat: ") + std::strerror(errno));
  return static_cast<std::uint64_t>(st.st_size);
}

std::string upperASCII(std::string_view s) {
  std::string out(s);
  for (char &c : out) {
    if (c >= 'a' && c <= 'z') c = static_cast<char>(c - 'a' + 'A');
  }
  return out;
}

// name is how an atom type is shown in keys and messages: its bytes as
// Latin-1 ("©nam" for 0xA9 "nam").
std::string name(std::string_view type) { return id3v2::latin1ToUTF8(type); }

std::string_view slice(std::string_view b, const Box &box) {
  return b.substr(static_cast<std::size_t>(box.offset), static_cast<std::size_t>(box.size));
}

std::string_view payloadOf(std::string_view b, const Box &box) {
  return b.substr(static_cast<std::size_t>(box.payload()), static_cast<std::size_t>(box.payloadSize()));
}

// hashRange is the SHA-256 of the bytes [start, end) of fd, read in chunks.
std::string hashRange(int fd, std::uint64_t start, std::uint64_t end) {
  SHA256 h;
  for (std::uint64_t off = start; off < end;) {
    const std::uint64_t n = std::min(kReadChunk, end - off);
    h.update(readAt(fd, off, static_cast<std::size_t>(n), "a box"));
    off += n;
  }
  return h.hex();
}

// only returns the one child of the type, nullopt when there is none;
// Failure(corrupt) when there are several.
std::optional<Box> only(const std::vector<Box> &boxes, std::string_view type, const std::string &where) {
  std::optional<Box> out;
  for (const Box &b : boxes) {
    if (b.type != type) continue;
    if (out) corrupt(where + " has two \"" + std::string(type) + "\" boxes");
    out = b;
  }
  return out;
}

Box required(const std::vector<Box> &boxes, std::string_view type, const std::string &where) {
  const auto b = only(boxes, type, where);
  if (!b) corrupt(where + " has no \"" + std::string(type) + "\" box");
  return *b;
}

// ---------------------------------------------------------------------------
// The raw file, read with pread only.

// The boxes a file may start with (FFmpeg's mov probe and TagLib accept
// these first): anything else is not an ISO base media file.
constexpr std::array<std::string_view, 8> kFirstBoxes = {"ftyp", "moov", "mdat", "free", "skip", "wide", "uuid", "pdin"};

// The one track: where its chunk offset table is, and its chunks.
struct Track {
  std::size_t index = 0;  // in moovChildren
  std::string sampleEntry;
  Box offsets;  // the stco or co64 box, offsets in the moov bytes
  bool co64 = false;
  std::vector<mp4::Chunk> chunks;
};

struct RawM4a {
  std::uint64_t size = 0;
  std::vector<Box> top;  // offsets in the file
  std::size_t moovIndex = 0;
  std::string moov;  // the whole moov box; the boxes below have offsets in it
  std::vector<Box> moovChildren;
  Track track;
  std::optional<std::size_t> udta;  // index in moovChildren
  std::vector<Box> udtaChildren;
  std::optional<std::size_t> meta;  // index in udtaChildren
  bool metaFull = true;
  std::vector<Box> metaChildren;
  std::optional<std::size_t> ilst;  // index in metaChildren
  std::vector<Box> items;
  // The metadata structure cannot be kept (blocking): the items are not read.
  std::vector<Opaque> opaque;
  AudioRange audio;
  std::string samplesSHA256;
  // The canonical forms of the boxes a write keeps byte for byte (NOTES.md
  // N-167): "mp4.box:<path>" -> their SHA-256 in file order, and
  // "mp4.trak" -> the SHA-256 of the track with its chunk offsets zeroed.
  std::map<std::string, std::vector<std::string>> boxes;

  const Box &moovBox() const { return top[moovIndex]; }
};

// readTop reads the top-level boxes of the file. The file must start with a
// box an ISO base media file starts with (Failure(format_mismatch)), and its
// boxes must tile it exactly: bytes after the last box are Failure(corrupt).
std::vector<Box> readTop(int fd, std::uint64_t size) {
  if (size < 8) throw Failure(code::kFormatMismatch, "not an ISO base media file: " + std::to_string(size) + " bytes");
  const std::string first = readAt(fd, 0, 8, "the file");
  if (std::find(kFirstBoxes.begin(), kFirstBoxes.end(), std::string_view(first).substr(4, 4)) == kFirstBoxes.end()) {
    throw Failure(code::kFormatMismatch, "not an ISO base media file: it does not start with a box of one");
  }
  std::vector<Box> top;
  std::uint64_t off = 0;
  while (off < size) {
    if (top.size() >= mp4::kMaxChildren) corrupt("the file has more than " + std::to_string(mp4::kMaxChildren) + " boxes");
    if (size - off < 8) corrupt("the file has " + std::to_string(size - off) + " bytes after its last box");
    const std::string h = readAt(fd, off, static_cast<std::size_t>(std::min<std::uint64_t>(16, size - off)), "a box header");
    const auto box = mp4::parseBoxHeader(h, off, size, true, "the file");
    if (!box) corrupt("the file has a truncated box header at its end");
    top.push_back(*box);
    off = box->end();
  }
  return top;
}

// sampleEntryChildren is where the child boxes of an audio sample entry
// start, after its fields: 28 bytes, plus 16 (version 1) or 36 (version 2)
// in the QuickTime sound description. A large box has a 16-byte header.
std::uint64_t sampleEntryChildren(std::string_view entry, std::uint64_t header) {
  if (entry.size() < header + 28) corrupt("the audio sample entry is too short");
  if (mp4::be16(entry, static_cast<std::size_t>(header + 6)) != 1) {
    unsupported("the audio sample entry does not use self-contained data reference 1");
  }
  const std::uint16_t version = mp4::be16(entry, static_cast<std::size_t>(header + 8));
  const std::uint64_t at = header + 28 + (version == 1 ? 16 : version == 2 ? 36 : 0);
  if (at > entry.size()) corrupt("the audio sample entry is truncated");
  return at;
}

// readTrack checks the one track of the file: an audio track ("soun") whose
// one sample description is the declared codec, without encryption or media
// data in another file, and computes its chunks.
Track readTrack(std::string_view m, const std::vector<Box> &moovChildren, std::size_t index, Format format) {
  Track t;
  t.index = index;
  const Box &trak = moovChildren[index];
  const auto trakKids = mp4::children(m, trak.payload(), trak.end(), "trak");
  const Box mdia = required(trakKids, "mdia", "trak");
  const auto mdiaKids = mp4::children(m, mdia.payload(), mdia.end(), "mdia");
  const Box hdlr = required(mdiaKids, "hdlr", "mdia");
  if (hdlr.payloadSize() < 12) corrupt("the track's hdlr box is too short");
  const std::string handler(m.substr(static_cast<std::size_t>(hdlr.payload() + 8), 4));
  if (handler != "soun") {
    unsupported("the track is a \"" + name(handler) + "\" track, not an audio track (" +
                (handler == "vide" ? "video" : "only one audio track is supported") + ")");
  }
  const Box minf = required(mdiaKids, "minf", "mdia");
  const auto minfKids = mp4::children(m, minf.payload(), minf.end(), "minf");
  if (const auto dinf = only(minfKids, "dinf", "minf")) {
    const auto dinfKids = mp4::children(m, dinf->payload(), dinf->end(), "dinf");
    if (const auto dref = only(dinfKids, "dref", "dinf")) {
      if (dref->payloadSize() < 8) corrupt("the dref box is too short");
      const auto refs = mp4::children(m, dref->payload() + 8, dref->end(), "dref");
      // Sample description 1 uses data reference 1: flag 1 is "the media
      // data is in this file".
      if (refs.empty() || refs[0].payloadSize() < 4 || (mp4::be32(m, static_cast<std::size_t>(refs[0].payload())) & 1U) == 0) {
        unsupported("the audio of the track is in another file");
      }
    }
  }
  const Box stbl = required(minfKids, "stbl", "minf");
  const auto stblKids = mp4::children(m, stbl.payload(), stbl.end(), "stbl");
  if (only(stblKids, "stz2", "stbl")) unsupported("compact sample sizes (stz2) are not supported");
  const Box stsd = required(stblKids, "stsd", "stbl");
  if (stsd.payloadSize() < 8) corrupt("the stsd box is too short");
  const std::uint32_t descriptions = mp4::be32(m, static_cast<std::size_t>(stsd.payload() + 4));
  const auto entries = mp4::children(m, stsd.payload() + 8, stsd.end(), "stsd");
  if (descriptions != 1 || entries.size() != 1) {
    unsupported("the track has " + std::to_string(entries.size()) + " sample descriptions; one is supported");
  }
  t.sampleEntry = entries[0].type;
  if (t.sampleEntry == "enca" || t.sampleEntry == "drms" || t.sampleEntry == "drmi") {
    unsupported("the audio is encrypted (DRM, sample description \"" + t.sampleEntry + "\")");
  }
  const std::string_view entry = slice(m, entries[0]);
  for (const Box &k : mp4::children(entry, sampleEntryChildren(entry, entries[0].header), entry.size(), "audio sample entry")) {
    if (k.type == "sinf") unsupported("the audio is encrypted (DRM, protection scheme information)");
  }
  const std::string_view want = format == Format::M4A_AAC ? "mp4a" : "alac";
  if (t.sampleEntry != want) {
    throw Failure(code::kFormatMismatch, "the audio track is \"" + name(t.sampleEntry) + "\", not " +
                                             std::string(formatName(format)) + " (\"" + std::string(want) + "\")");
  }
  const auto stco = only(stblKids, "stco", "stbl");
  const auto co64 = only(stblKids, "co64", "stbl");
  if (stco.has_value() == co64.has_value()) corrupt("the track must have exactly one stco or co64 box");
  t.co64 = co64.has_value();
  t.offsets = t.co64 ? *co64 : *stco;
  const Box stsc = required(stblKids, "stsc", "stbl");
  const Box stsz = required(stblKids, "stsz", "stbl");
  t.chunks = mp4::chunks(payloadOf(m, stsc), payloadOf(m, stsz), payloadOf(m, t.offsets), t.co64);
  if (t.chunks.empty()) corrupt("the audio track has no samples");
  return t;
}

// readMetadata locates moov/udta/meta/ilst. A structure a write could not
// keep is reported as a blocking opaque field, and then no item is read: two
// udta, meta or ilst boxes, or a meta that is not iTunes metadata.
void readMetadata(RawM4a &raw) {
  const std::string_view m = raw.moov;
  auto blocking = [&](std::string key, const char *why) { raw.opaque.push_back({std::move(key), why, false}); };
  std::vector<std::size_t> udtas;
  for (std::size_t i = 0; i < raw.moovChildren.size(); ++i) {
    if (raw.moovChildren[i].type == "udta") udtas.push_back(i);
  }
  if (udtas.size() > 1) return blocking("mp4:moov/udta", reason::kDuplicateBlock);
  if (udtas.empty()) return;
  raw.udta = udtas[0];
  const Box &udta = raw.moovChildren[udtas[0]];
  raw.udtaChildren = mp4::children(m, udta.payload(), udta.end(), "udta");
  std::vector<std::size_t> metas;
  for (std::size_t i = 0; i < raw.udtaChildren.size(); ++i) {
    if (raw.udtaChildren[i].type == "meta") metas.push_back(i);
  }
  if (metas.size() > 1) return blocking("mp4:moov/udta/meta", reason::kDuplicateBlock);
  if (metas.empty()) return;
  raw.meta = metas[0];
  const Box &meta = raw.udtaChildren[metas[0]];
  raw.metaFull = mp4::isFullAtomMeta(payloadOf(m, meta));
  const std::uint64_t first = meta.payload() + (raw.metaFull ? 4 : 0);
  if (first > meta.end()) corrupt("the meta box is too short");
  raw.metaChildren = mp4::children(m, first, meta.end(), "meta");
  std::vector<std::size_t> hdlrs, ilsts;
  for (std::size_t i = 0; i < raw.metaChildren.size(); ++i) {
    if (raw.metaChildren[i].type == "hdlr") hdlrs.push_back(i);
    if (raw.metaChildren[i].type == "ilst") ilsts.push_back(i);
  }
  if (hdlrs.size() != 1 || raw.metaChildren[hdlrs[0]].payloadSize() < 12 ||
      m.substr(static_cast<std::size_t>(raw.metaChildren[hdlrs[0]].payload() + 8), 4) != "mdir") {
    return blocking("mp4:moov/udta/meta", reason::kForeignMetadata);
  }
  if (ilsts.size() > 1) return blocking("mp4:moov/udta/meta/ilst", reason::kDuplicateBlock);
  if (ilsts.empty()) return;
  raw.ilst = ilsts[0];
  const Box &ilst = raw.metaChildren[ilsts[0]];
  raw.items = mp4::children(m, ilst.payload(), ilst.end(), "ilst");
}

// zeroOffsets is the track with the entries of its chunk offset table set to
// zero: the part of it a write keeps byte for byte.
std::string zeroOffsets(const RawM4a &raw) {
  const Box &trak = raw.moovChildren[raw.track.index];
  std::string t(slice(raw.moov, trak));
  const std::uint64_t at = raw.track.offsets.payload() + 8 - trak.offset;
  const std::uint64_t n = raw.track.chunks.size() * (raw.track.co64 ? 8 : 4);
  std::fill(t.begin() + static_cast<std::ptrdiff_t>(at), t.begin() + static_cast<std::ptrdiff_t>(at + n), '\0');
  return t;
}

// canonicalBoxes fills raw.boxes (see RawM4a).
void canonicalBoxes(int fd, RawM4a &raw) {
  const std::string_view m = raw.moov;
  for (std::size_t i = 0; i < raw.top.size(); ++i) {
    const Box &b = raw.top[i];
    if (i == raw.moovIndex) continue;
    // The media data is covered by mp4.samples; its size is its identity
    // here, so that no inspection reads the audio twice.
    std::string v = b.type == "mdat" ? "size=" + std::to_string(b.size) : "sha256:" + hashRange(fd, b.offset, b.end());
    raw.boxes["mp4.box:" + name(b.type)].push_back(std::move(v));
  }
  for (std::size_t i = 0; i < raw.moovChildren.size(); ++i) {
    const Box &b = raw.moovChildren[i];
    if (raw.udta && i == *raw.udta) continue;
    if (i == raw.track.index) {
      raw.boxes["mp4.trak"].push_back("sha256:" + sha256Hex(zeroOffsets(raw)));
      continue;
    }
    raw.boxes["mp4.box:moov/" + name(b.type)].push_back("sha256:" + sha256Hex(slice(m, b)));
  }
  for (std::size_t i = 0; i < raw.udtaChildren.size(); ++i) {
    if (raw.meta && i == *raw.meta) continue;
    const Box &b = raw.udtaChildren[i];
    raw.boxes["mp4.box:moov/udta/" + name(b.type)].push_back("sha256:" + sha256Hex(slice(m, b)));
  }
  for (const Box &b : raw.metaChildren) {
    // The handler is part of the metadata structure a write keeps or
    // creates; free and skip boxes are its padding.
    if (b.type == "hdlr" || b.type == "ilst" || b.type == "free" || b.type == "skip") continue;
    raw.boxes["mp4.box:moov/udta/meta/" + name(b.type)].push_back("sha256:" + sha256Hex(slice(m, b)));
  }
}

// hashSamples checks that every chunk lies in the payload of an mdat box and
// hashes the samples in their order, through a buffer: the chunks of a file
// are usually consecutive.
void hashSamples(int fd, RawM4a &raw) {
  std::vector<Box> mdats;
  for (const Box &b : raw.top) {
    if (b.type == "mdat") mdats.push_back(b);
  }
  SHA256 h;
  std::string buf;
  std::uint64_t bufStart = 0;
  raw.audio = AudioRange{std::numeric_limits<std::uint64_t>::max(), 0};
  for (const mp4::Chunk &c : raw.track.chunks) {
    const auto in = std::find_if(mdats.begin(), mdats.end(), [&](const Box &b) {
      return c.offset >= b.payload() && c.offset <= b.end() && c.size <= b.end() - c.offset;
    });
    if (in == mdats.end()) {
      corrupt("a chunk of the audio (" + std::to_string(c.size) + " bytes at " + std::to_string(c.offset) +
              ") is not inside an mdat box");
    }
    raw.audio.start = std::min(raw.audio.start, c.offset);
    raw.audio.end = std::max(raw.audio.end, c.offset + c.size);
    std::uint64_t off = c.offset, left = c.size;
    while (left > 0) {
      if (off < bufStart || off >= bufStart + buf.size()) {
        const std::uint64_t n = std::min(std::max(kReadChunk, left), in->end() - off);
        buf = readAt(fd, off, static_cast<std::size_t>(std::min<std::uint64_t>(n, kReadChunk)), "the audio");
        bufStart = off;
      }
      const std::uint64_t at = off - bufStart;
      const std::uint64_t k = std::min<std::uint64_t>(left, buf.size() - at);
      h.update(std::string_view(buf).substr(static_cast<std::size_t>(at), static_cast<std::size_t>(k)));
      off += k;
      left -= k;
    }
  }
  raw.samplesSHA256 = h.hex();
}

// readRawM4a reads the structure of the file (NOTES.md N-165): its
// top-level boxes; the one moov, in memory (at most mp4::kMaxMoov bytes); the
// one track; the metadata; the canonical forms of the kept boxes; and the
// hash of the audio samples through the sample table.
RawM4a readRawM4a(int fd, Format format) {
  RawM4a raw;
  raw.size = fileSize(fd);
  raw.top = readTop(fd, raw.size);
  std::optional<std::size_t> moov;
  for (std::size_t i = 0; i < raw.top.size(); ++i) {
    const std::string &t = raw.top[i].type;
    if (t == "moof" || t == "styp" || t == "sidx" || t == "mfra") unsupported("fragmented MP4 files are not supported");
    if (t != "moov") continue;
    if (moov) corrupt("the file has two moov boxes");
    moov = i;
  }
  if (!moov) corrupt("the file has no moov box");
  raw.moovIndex = *moov;
  const Box &mb = raw.moovBox();
  if (mb.size > mp4::kMaxMoov) {
    throw Failure(code::kTooLarge, "the moov box has " + std::to_string(mb.size) + " bytes, more than " +
                                       std::to_string(mp4::kMaxMoov));
  }
  raw.moov = readAt(fd, mb.offset, static_cast<std::size_t>(mb.size), "the moov box");
  const std::string_view m = raw.moov;
  raw.moovChildren = mp4::children(m, mb.header, mb.size, "moov");
  std::vector<std::size_t> tracks;
  for (std::size_t i = 0; i < raw.moovChildren.size(); ++i) {
    const std::string &t = raw.moovChildren[i].type;
    if (t == "mvex") unsupported("fragmented MP4 files are not supported");
    if (t == "pssh") unsupported("the audio is encrypted (DRM, protection system header)");
    if (t == "trak") tracks.push_back(i);
  }
  if (tracks.size() != 1) {
    unsupported("the file has " + std::to_string(tracks.size()) + " tracks; exactly one audio track is supported");
  }
  raw.track = readTrack(m, raw.moovChildren, tracks[0], format);
  readMetadata(raw);
  canonicalBoxes(fd, raw);
  hashSamples(fd, raw);
  return raw;
}

// ---------------------------------------------------------------------------
// From the raw file to an inspection.

// What decoding the values of a managed item gave: its values, or why they
// could not be read.
struct Decoded {
  std::vector<std::string> values;
  const char *reason = nullptr;
};

// itemChildren parses the children of an item; nullopt when they do not
// tile it.
std::optional<std::vector<Box>> itemChildren(std::string_view m, const Box &item) {
  return mp4::tryChildren(m, item.payload(), item.end());
}

// decodeText reads text data atoms: UTF-8 (type 1) or UTF-16BE (type 2),
// empty values dropped (an empty value is an absent one, as in the FLAC
// reader).
Decoded decodeText(std::string_view m, const std::vector<Box> &kids, std::size_t first = 0) {
  Decoded d;
  for (std::size_t i = first; i < kids.size(); ++i) {
    const auto data = mp4::parseData(slice(m, kids[i]));
    if (!data) return {{}, reason::kMalformedEntry};
    std::string v;
    if (data->type == mp4::kTypeUTF8) {
      if (!validUTF8(data->value)) return {{}, reason::kInvalidUTF8};
      v.assign(data->value);
    } else if (data->type == mp4::kTypeUTF16) {
      auto u = mp4::utf16beToUTF8(data->value);
      if (!u) return {{}, reason::kInvalidText};
      v = std::move(*u);
    } else {
      return {{}, reason::kUnsupportedData};
    }
    if (v.find('\0') != std::string::npos) return {{}, reason::kNulByte};
    if (!v.empty()) d.values.push_back(std::move(v));
  }
  return d;
}

// integerData reads the one data atom of an integer item: type 0 or 21,
// between min and max bytes. nullopt otherwise.
std::optional<std::string_view> integerData(std::string_view m, const std::vector<Box> &kids, std::size_t min,
                                            std::size_t max) {
  if (kids.size() != 1) return std::nullopt;
  const auto data = mp4::parseData(slice(m, kids[0]));
  if (!data || (data->type != mp4::kTypeImplicit && data->type != mp4::kTypeInteger) || data->value.size() < min ||
      data->value.size() > max) {
    return std::nullopt;
  }
  return data->value;
}

std::uint64_t bigEndian(std::string_view b) {
  std::uint64_t n = 0;
  for (const char c : b) n = (n << 8) | static_cast<unsigned char>(c);
  return n;
}

// childValue is the canonical value of one child of an unmanaged item: the
// text of a UTF-8 data atom with locale 0; for any other data atom its type,
// locale and the SHA-256 of its value; for any other box its type and the
// SHA-256 of its bytes.
std::string childValue(std::string_view box) {
  if (const auto d = mp4::parseData(box)) {
    if (d->type == mp4::kTypeUTF8 && d->locale == 0 && validUTF8(d->value)) return std::string(d->value);
    return "data type=" + std::to_string(d->type) + " locale=" + hexLower(mp4::enc32(d->locale)) +
           " sha256:" + sha256Hex(d->value);
  }
  return name(box.substr(4, 4)) + " sha256:" + sha256Hex(box);
}

// Freeform is a well-formed "----" item: mean, name, then its values.
struct Freeform {
  std::string mean, name;
  std::size_t firstValue = 2;
};

std::optional<Freeform> parseFreeform(std::string_view m, const std::vector<Box> &kids) {
  if (kids.size() < 2) return std::nullopt;
  const auto mean = mp4::parseFullString(slice(m, kids[0]), "mean");
  const auto nm = mp4::parseFullString(slice(m, kids[1]), "name");
  if (!mean || !nm || !validUTF8(*mean) || !validUTF8(*nm)) return std::nullopt;
  return Freeform{std::string(*mean), std::string(*nm), 2};
}

struct M4aAnalysis {
  // The sources of each managed field, in reading order (§8.1).
  std::array<std::vector<KeyValues>, kFieldCount> sources;
  std::vector<Picture> pictures;
  std::vector<Opaque> opaque;
  std::map<std::string, std::vector<std::string>> unmanaged;
  std::vector<std::size_t> keptItems;  // indexes into raw.items
  // Where a write puts the cover, relative to the kept items (N-168).
  std::optional<std::size_t> coverAt;
};

bool isFreeformAlias(std::string_view upper) {
  return std::any_of(kMp4Fields.begin(), kMp4Fields.end(), [&](const Mp4Field &f) {
    return std::any_of(f.freeformAliases.begin(), f.freeformAliases.end(),
                       [&](std::string_view a) { return !a.empty() && a == upper; });
  });
}

// removedItem reports whether a write removes the item as an alias, a sort
// atom or the numeric genre of the table (not as a managed atom or the
// cover, which it rewrites).
bool removedItem(std::string_view m, const Box &item) {
  if (item.type == kMp4GenreNumber) return true;
  if (std::find(kMp4SortAtoms.begin(), kMp4SortAtoms.end(), item.type) != kMp4SortAtoms.end()) return true;
  if (item.type != "----") return false;
  const auto kids = mp4::tryChildren(m, item.payload(), item.end());
  const auto ff = kids ? parseFreeform(m, *kids) : std::nullopt;
  if (!ff || ff->mean != kMp4FreeformMean) return false;
  const std::string upper = upperASCII(ff->name);
  return isFreeformAlias(upper) ||
         std::find(kMp4SortFreeform.begin(), kMp4SortFreeform.end(), upper) != kMp4SortFreeform.end();
}

bool isManagedAtom(std::string_view type) {
  return std::any_of(kMp4Fields.begin(), kMp4Fields.end(), [&](const Mp4Field &f) { return !f.atom.empty() && f.atom == type; });
}

// coverMime is the MIME type of a covr data type, "" when it names none.
std::string coverMime(std::uint32_t type) {
  switch (type) {
    case mp4::kTypeJPEG:
      return "image/jpeg";
    case mp4::kTypePNG:
      return "image/png";
    case mp4::kTypeBMP:
      return "image/bmp";
    case mp4::kTypeGIF:
      return "image/gif";
  }
  return "";
}

// analyzeItem reads one item of the ilst into the analysis.
void analyzeItem(const RawM4a &raw, std::size_t i, M4aAnalysis &a, std::map<std::string, std::vector<std::string>> &values) {
  const std::string_view m = raw.moov;
  const Box &item = raw.items[i];
  const std::string key = "ilst:" + name(item.type);
  const auto kids = itemChildren(m, item);
  auto removed = [&](const std::string &k, const char *why) { a.opaque.push_back({k, why, true}); };
  auto keep = [&](const std::string &k, std::vector<std::string> v) {
    a.keptItems.push_back(i);
    auto &dst = a.unmanaged[k];
    dst.insert(dst.end(), std::make_move_iterator(v.begin()), std::make_move_iterator(v.end()));
  };
  if (item.type == "free" || item.type == "skip") return;  // padding inside the list: nothing to keep
  if (item.type == kMp4PictureAtom) {
    if (!kids) return removed(key, reason::kInvalidPicture);
    for (const Box &k : *kids) {
      const auto d = mp4::parseData(slice(m, k));
      if (!d || d->value.empty()) {
        removed(key, reason::kInvalidPicture);
        continue;
      }
      // Every image of covr is cover art: the front cover of §8.2 (N-166).
      a.pictures.push_back(Picture{"covr", kFrontCover, coverMime(d->type), 0, 0, 0, 0, std::string(d->value)});
    }
    return;
  }
  if (std::find(kMp4SortAtoms.begin(), kMp4SortAtoms.end(), item.type) != kMp4SortAtoms.end()) return;
  if (item.type == kMp4GenreNumber) {
    const auto v = kids ? integerData(m, *kids, 2, 2) : std::nullopt;
    if (!v) return removed(key, reason::kMalformedEntry);
    const auto n = bigEndian(*v);
    const std::string g = n >= 1 && n <= 192 ? TagLib::ID3v1::genre(static_cast<int>(n - 1)).to8Bit(true) : "";
    if (!g.empty()) values[key].push_back(g);
    return;
  }
  if (isManagedAtom(item.type)) {
    if (!kids) return removed(key, reason::kMalformedEntry);
    if (item.type == "trkn" || item.type == "disk") {
      const auto v = integerData(m, *kids, 6, 8);
      if (!v || (v->size() != 6 && v->size() != 8)) return removed(key, reason::kMalformedEntry);
      if (const auto n = mp4::be16(*v, 2); n != 0) values[key].push_back(std::to_string(n));
      if (const auto t = mp4::be16(*v, 4); t != 0) values[key + "/total"].push_back(std::to_string(t));
      return;
    }
    if (item.type == "cpil") {
      const auto v = integerData(m, *kids, 1, 8);
      if (!v) return removed(key, reason::kMalformedEntry);
      values[key].push_back(std::to_string(bigEndian(*v)));
      return;
    }
    Decoded d = decodeText(m, *kids);
    if (d.reason != nullptr) return removed(key, d.reason);
    auto &dst = values[key];
    dst.insert(dst.end(), d.values.begin(), d.values.end());
    return;
  }
  if (item.type == "----" && kids) {
    if (const auto ff = parseFreeform(m, *kids)) {
      const std::string upper = upperASCII(ff->name);
      if (ff->mean == kMp4FreeformMean) {
        if (std::find(kMp4SortFreeform.begin(), kMp4SortFreeform.end(), upper) != kMp4SortFreeform.end()) return;
        if (isFreeformAlias(upper)) {
          const std::string source = "ilst:----:" + std::string(kMp4FreeformMean) + ":" + upper;
          Decoded d = decodeText(m, *kids, ff->firstValue);
          if (d.reason != nullptr) return removed(source, d.reason);
          auto &dst = values[source];
          dst.insert(dst.end(), d.values.begin(), d.values.end());
          return;
        }
      }
      std::vector<std::string> v;
      for (std::size_t k = ff->firstValue; k < kids->size(); ++k) v.push_back(childValue(slice(m, (*kids)[k])));
      return keep("ilst:----:" + ff->mean + ":" + ff->name, std::move(v));
    }
  }
  if (!kids) return keep(key, {"raw sha256:" + sha256Hex(payloadOf(m, item))});
  std::vector<std::string> v;
  for (const Box &k : *kids) v.push_back(childValue(slice(m, k)));
  keep(key, std::move(v));
}

void addSource(std::vector<KeyValues> &sources, const std::string &key,
               std::map<std::string, std::vector<std::string>> &values) {
  const auto it = values.find(key);
  if (it != values.end() && !it->second.empty()) sources.push_back(KeyValues{key, it->second});
}

M4aAnalysis analyze(const RawM4a &raw) {
  M4aAnalysis a;
  a.opaque = raw.opaque;
  std::map<std::string, std::vector<std::string>> values;  // source key -> values, in file order
  for (std::size_t i = 0; i < raw.items.size(); ++i) analyzeItem(raw, i, a, values);
  // FFmpeg creates an attached-picture stream only for JPEG, PNG and BMP
  // covr data. If such a stream preceded iTunSMPB, the replacement must
  // precede it too; otherwise it must follow iTunSMPB. Without iTunSMPB,
  // retain the first covr's position for deterministic item order.
  std::optional<std::size_t> firstCover, firstStream, firstStreamItem, firstSMPBItem;
  std::size_t afterSMPB = a.keptItems.size();
  std::size_t kept = 0;
  for (std::size_t i = 0; i < raw.items.size(); ++i) {
    const Box &item = raw.items[i];
    if (item.type == kMp4PictureAtom) {
      if (!firstCover) firstCover = kept;
      if (!firstStream) {
        if (const auto kids = itemChildren(raw.moov, item)) {
          for (const Box &k : *kids) {
            const auto d = mp4::parseData(slice(raw.moov, k));
            if (d && !d->value.empty() && (d->type == mp4::kTypeJPEG || d->type == mp4::kTypePNG || d->type == mp4::kTypeBMP)) {
              firstStream = kept;
              firstStreamItem = i;
              break;
            }
          }
        }
      }
    }
    if (a.keptItems.size() > kept && a.keptItems[kept] == i) ++kept;
    if (item.type == "----") {
      if (const auto kids = itemChildren(raw.moov, item)) {
        const auto ff = parseFreeform(raw.moov, *kids);
        if (ff && ff->mean == kMp4FreeformMean && ff->name == "iTunSMPB") {
          if (!firstSMPBItem) firstSMPBItem = i;
          afterSMPB = kept;
        }
      }
    }
  }
  a.coverAt = firstSMPBItem ? (firstStreamItem && *firstStreamItem < *firstSMPBItem
                                    ? firstStream : std::optional<std::size_t>(afterSMPB))
                            : firstCover;
  for (const Mp4Field &f : kMp4Fields) {
    auto &dst = a.sources[idx(f.field)];
    if (!f.atom.empty()) {
      const std::string key = "ilst:" + name(f.atom);
      addSource(dst, key, values);
      // The total of trkn and disk is the canonical source of the totals.
      if (f.field == Field::Track) addSource(a.sources[idx(Field::TrackTotal)], key + "/total", values);
      if (f.field == Field::Disc) addSource(a.sources[idx(Field::DiscTotal)], key + "/total", values);
      if (f.field == Field::Genre) addSource(dst, "ilst:" + std::string(kMp4GenreNumber), values);
    }
    for (const auto alias : f.freeformAliases) {
      if (!alias.empty()) addSource(dst, "ilst:----:" + std::string(kMp4FreeformMean) + ":" + std::string(alias), values);
    }
  }
  for (const auto &[k, v] : raw.boxes) a.unmanaged[k] = v;
  a.unmanaged["mp4.samples"] = {"sha256:" + raw.samplesSHA256};
  return a;
}

Inspection toInspection(const M4aAnalysis &a, const RawM4a &raw, Format format) {
  Inspection in;
  in.format = format;
  in.audio = raw.audio;
  for (std::size_t f = 0; f < kFieldCount; ++f) {
    const auto &sources = a.sources[f];
    if (sources.empty()) continue;
    in.managed[f] = sources.front().values;
    const bool disagree = std::any_of(sources.begin() + 1, sources.end(),
                                      [&](const KeyValues &s) { return s.values != sources.front().values; });
    if (disagree) in.conflicts.push_back(Conflict{static_cast<Field>(f), sources});
  }
  in.pictures = a.pictures;
  for (const auto &[key, values] : a.unmanaged) in.unmanaged.push_back(KeyValues{key, values});
  in.opaque = a.opaque;
  return in;
}

// ---------------------------------------------------------------------------
// TagLib, an independent parser.

TagLib::String itemKey(std::string_view atom) {
  return TagLib::String(TagLib::ByteVector(atom.data(), static_cast<unsigned int>(atom.size())), TagLib::String::Latin1);
}

std::vector<std::string> strings(const TagLib::StringList &l) {
  std::vector<std::string> out;
  for (const auto &s : l) out.push_back(s.to8Bit(true));
  return out;
}

// What TagLib 2.3.2 reads of one managed atom (MP4::ItemFactory), when the
// atom is one TagLib reads without loss: nullopt when the comparison does not
// apply (several atoms of the name, a data type TagLib drops, 16-bit numbers
// it reads as signed...). The expected value is in the helper's terms.
struct TagLibExpectation {
  std::string atom;
  std::vector<std::string> text;  // text atoms
  int number = 0, total = 0;      // trkn, disk
  std::optional<bool> flag;       // cpil
  std::vector<std::pair<std::uint32_t, std::string>> covers;
};

std::optional<TagLibExpectation> cleanAtom(const RawM4a &raw, std::string_view atom) {
  const std::string_view m = raw.moov;
  std::optional<Box> item;
  for (const Box &b : raw.items) {
    if (b.type != atom) continue;
    if (item) return std::nullopt;
    item = b;
  }
  TagLibExpectation e{std::string(atom), {}, 0, 0, std::nullopt, {}};
  if (!item) return e;
  const auto kids = itemChildren(m, *item);
  if (!kids || kids->empty()) return std::nullopt;
  if (atom == "trkn" || atom == "disk") {
    const auto v = integerData(m, *kids, 6, 8);
    if (!v || mp4::be16(*v, 2) > 0x7FFF || mp4::be16(*v, 4) > 0x7FFF) return std::nullopt;
    e.number = mp4::be16(*v, 2);
    e.total = mp4::be16(*v, 4);
    return e;
  }
  if (atom == "cpil") {
    const auto v = integerData(m, *kids, 1, 1);
    if (!v) return std::nullopt;
    e.flag = (*v)[0] != '\0';
    return e;
  }
  if (atom == kMp4PictureAtom) {
    for (const Box &k : *kids) {
      const auto d = mp4::parseData(slice(m, k));
      if (!d || (d->type != mp4::kTypeJPEG && d->type != mp4::kTypePNG && d->type != mp4::kTypeBMP &&
                 d->type != mp4::kTypeGIF && d->type != mp4::kTypeImplicit)) {
        return std::nullopt;
      }
      e.covers.emplace_back(d->type, std::string(d->value));
    }
    return e;
  }
  for (const Box &k : *kids) {
    const auto d = mp4::parseData(slice(m, k));
    if (!d || d->type != mp4::kTypeUTF8 || d->value.empty() || !validUTF8(d->value) ||
        d->value.find('\0') != std::string_view::npos) {
      return std::nullopt;
    }
    e.text.emplace_back(d->value);
  }
  return e;
}

// checkTagLibItems compares TagLib's reading of every managed atom with the
// helper's, where TagLib reads it without loss (cleanAtom). TagLib reads a
// numeric genre as the text genre, so "©gen" is compared only without one.
void checkTagLibItems(const RawM4a &raw, const TagLib::MP4::Tag &tag) {
  const bool numericGenre = std::any_of(raw.items.begin(), raw.items.end(),
                                        [](const Box &b) { return b.type == kMp4GenreNumber; });
  std::vector<std::string_view> atoms;
  for (const Mp4Field &f : kMp4Fields) {
    if (!f.atom.empty() && !(f.field == Field::Genre && numericGenre)) atoms.push_back(f.atom);
  }
  atoms.push_back(kMp4PictureAtom);
  const auto &map = tag.itemMap();
  for (const std::string_view atom : atoms) {
    const auto e = cleanAtom(raw, atom);
    if (!e) continue;
    const auto it = map.find(itemKey(atom));
    const bool present = !e->text.empty() || e->number != 0 || e->total != 0 || e->flag || !e->covers.empty() ||
                         std::any_of(raw.items.begin(), raw.items.end(), [&](const Box &b) { return b.type == atom; });
    bool same = (it != map.end()) == present;
    if (same && it != map.end()) {
      const TagLib::MP4::Item &item = it->second;
      if (atom == "trkn" || atom == "disk") {
        same = item.toIntPair().first == e->number && item.toIntPair().second == e->total;
      } else if (atom == "cpil") {
        same = item.toBool() == *e->flag;
      } else if (atom == kMp4PictureAtom) {
        const auto covers = item.toCoverArtList();
        same = covers.size() == e->covers.size();
        std::size_t i = 0;
        for (const auto &c : covers) {
          if (!same) break;
          same = c.data() == TagLib::ByteVector(e->covers[i].second.data(), static_cast<unsigned int>(e->covers[i].second.size())) &&
                 static_cast<std::uint32_t>(c.format()) == e->covers[i].first;
          ++i;
        }
      } else {
        same = strings(item.toStringList()) == e->text;
      }
    }
    if (!same) internal("TagLib and the helper read the " + name(atom) + " atom differently");
  }
}

// checkTagLibAgrees verifies that TagLib finds the same structure: a valid
// file, the moov box and the ilst where the reader found them, the same
// codec, no encryption, and the same managed atoms where it reads them
// without loss. Anything else is Failure(internal).
void checkTagLibAgrees(int fd, const RawM4a &raw) {
  FdStream stream(fd, false);
  TagLib::MP4::File file(&stream, true, TagLib::AudioProperties::Fast);
  stream.check();
  if (!file.isValid()) corrupt("TagLib cannot read the M4A file");
  const TagLib::MP4::Atoms atoms(&file);
  stream.check();
  const TagLib::MP4::Atom *moov = atoms.find("moov");
  const Box &mb = raw.moovBox();
  if (moov == nullptr || static_cast<std::uint64_t>(moov->offset()) != mb.offset ||
      static_cast<std::uint64_t>(moov->length()) != mb.size) {
    internal("TagLib and the helper disagree on the moov box");
  }
  const TagLib::MP4::Properties *p = file.audioProperties();
  if (p == nullptr || p->isEncrypted() || p->codecId().to8Bit(false) != raw.track.sampleEntry) {
    internal("TagLib and the helper disagree on the audio track");
  }
  if (!raw.opaque.empty()) return;  // the metadata structure is refused anyway
  const TagLib::MP4::Atom *ilst = atoms.find("moov", "udta", "meta", "ilst");
  if ((ilst != nullptr) != raw.ilst.has_value() ||
      (ilst != nullptr && static_cast<std::uint64_t>(ilst->offset()) != mb.offset + raw.metaChildren[*raw.ilst].offset)) {
    internal("TagLib and the helper disagree on the ilst box");
  }
  checkTagLibItems(raw, *file.tag());
}

// ---------------------------------------------------------------------------
// The write.

std::string item(std::string_view atom, std::string_view payload) { return mp4::renderBox(atom, payload); }

// kMaxNumber is the largest track or disc number or total an M4A write
// accepts: trkn and disk hold 16 bits, which TagLib reads as signed
// (MP4::ItemFactory::parseIntPair). The schema's limits are far below
// (999 tracks, 99 discs).
constexpr std::uint32_t kMaxNumber = 0x7FFF;

// managedItems are the managed atoms of a write, in table order (§8.2):
// UTF-8 text; trkn with 8 bytes and disk with 6, as iTunes writes them;
// cpil as one byte of type 21, only when true (N-089).
std::vector<std::string> managedItems(const ManagedValues &mv) {
  std::vector<std::string> out;
  auto number = [&](Field f) -> std::uint16_t {
    const auto v = mv.number[idx(f)];
    if (!v) return 0;
    if (*v > kMaxNumber) {
      throw Failure(code::kInvalidRequest, "tags." + std::string(kFieldNames[idx(f)]) + " " + std::to_string(*v) +
                                               " does not fit in an M4A file (at most " + std::to_string(kMaxNumber) + ")");
    }
    return static_cast<std::uint16_t>(*v);
  };
  for (const Mp4Field &f : kMp4Fields) {
    if (f.atom.empty()) continue;
    switch (f.field) {
      case Field::Track:
      case Field::Disc: {
        const std::uint16_t n = number(f.field);
        const std::uint16_t t = number(f.field == Field::Track ? Field::TrackTotal : Field::DiscTotal);
        if (n == 0 && t == 0) break;
        std::string v = mp4::enc16(0) + mp4::enc16(n) + mp4::enc16(t);
        if (f.field == Field::Track) v += mp4::enc16(0);
        out.push_back(item(f.atom, mp4::renderData(mp4::kTypeImplicit, v)));
        break;
      }
      case Field::Compilation:
        if (mv.compilation) out.push_back(item(f.atom, mp4::renderData(mp4::kTypeInteger, std::string(1, '\x01'))));
        break;
      default:
        if (const auto &t = mv.text[idx(f.field)]) out.push_back(item(f.atom, mp4::renderData(mp4::kTypeUTF8, *t)));
    }
  }
  return out;
}

// newItems is the item list of the written ilst: the managed atoms, then the
// kept items in their order with the cover placed to preserve FFmpeg's
// iTunSMPB stream ordering (NOTES.md N-168).
std::vector<std::string> newItems(const RawM4a &raw, const M4aAnalysis &a, const WriteRequest &req,
                                  const std::string &cover) {
  std::vector<std::string> out = managedItems(req.values);
  std::string covr;
  if (req.cover) {
    const std::string &mime = req.cover->mime;
    if (mime != "image/jpeg" && mime != "image/png") {
      throw Failure(code::kInvalidRequest, "cover.mime \"" + mime + "\" cannot be embedded in an M4A file (JPEG or PNG)");
    }
    covr = item(kMp4PictureAtom, mp4::renderData(mime == "image/jpeg" ? mp4::kTypeJPEG : mp4::kTypePNG, cover));
  }
  const std::size_t at = a.coverAt.value_or(a.keptItems.size());
  for (std::size_t k = 0; k <= a.keptItems.size(); ++k) {
    if (k == at && !covr.empty()) out.push_back(covr);
    if (k < a.keptItems.size()) out.emplace_back(slice(raw.moov, raw.items[a.keptItems[k]]));
  }
  return out;
}

// render is box with a new payload, in the header form it had.
std::string render(const Box &box, std::string_view payload) { return mp4::renderBox(box.type, payload, box.header == 16); }

// The hdlr of a new meta box: iTunes metadata ("mdir"), as iTunes, FFmpeg and
// TagLib write it.
std::string newHdlr() {
  return mp4::renderBox("hdlr", std::string(8, '\0') + "mdirappl" + std::string(9, '\0'));
}

std::string freeBox(std::uint64_t size) {
  return size == 0 ? std::string() : mp4::renderBox("free", std::string(static_cast<std::size_t>(size - 8), '\0'));
}

// newMoov is the moov of the write, with a padding box of pad bytes after
// the ilst (0: none), before the chunk offsets are fixed up.
std::string newMoov(const RawM4a &raw, const std::string &ilst, std::uint64_t pad) {
  const std::string_view m = raw.moov;
  const std::string padding = freeBox(pad);
  std::string meta;
  if (raw.meta) {
    const Box &mb = raw.udtaChildren[*raw.meta];
    std::string body = raw.metaFull ? std::string(m.substr(static_cast<std::size_t>(mb.payload()), 4)) : std::string();
    for (std::size_t i = 0; i < raw.metaChildren.size(); ++i) {
      const Box &b = raw.metaChildren[i];
      if (b.type == "free" || b.type == "skip") continue;
      body += raw.ilst && i == *raw.ilst ? ilst + padding : std::string(slice(m, b));
    }
    if (!raw.ilst) body += ilst + padding;
    meta = render(mb, body);
  } else {
    meta = mp4::renderBox("meta", std::string(4, '\0') + newHdlr() + ilst + padding);
  }
  std::string udta;
  if (raw.udta) {
    std::string body;
    for (std::size_t i = 0; i < raw.udtaChildren.size(); ++i) {
      body += raw.meta && i == *raw.meta ? meta : std::string(slice(m, raw.udtaChildren[i]));
    }
    if (!raw.meta) body += meta;
    udta = render(raw.moovChildren[*raw.udta], body);
  } else {
    udta = mp4::renderBox("udta", meta);
  }
  std::string body;
  for (std::size_t i = 0; i < raw.moovChildren.size(); ++i) {
    body += raw.udta && i == *raw.udta ? udta : std::string(slice(m, raw.moovChildren[i]));
  }
  if (!raw.udta) body += udta;
  return render(raw.moovBox(), body);
}

// fixOffsets adds delta to every chunk offset of the track that points past
// the old moov (NOTES.md N-165): the media data after it moves by delta.
std::string fixOffsets(const RawM4a &raw, std::string moov, std::int64_t delta) {
  if (delta == 0) return moov;
  // The track is copied unchanged, so its chunk offset table is at the same
  // place relative to the track: find the track in the new moov.
  const auto kids = mp4::children(moov, raw.moovBox().header, moov.size(), "the new moov");
  const Box &oldTrak = raw.moovChildren[raw.track.index];
  const auto it = std::find_if(kids.begin(), kids.end(), [&](const Box &b) {
    return b.type == "trak" && std::string_view(moov).substr(static_cast<std::size_t>(b.offset), static_cast<std::size_t>(b.size)) ==
                                   slice(raw.moov, oldTrak);
  });
  if (it == kids.end()) internal("the track is not in the new moov");
  const Box &o = raw.track.offsets;
  const std::uint64_t at = it->offset + (o.payload() - oldTrak.offset);
  const std::string patched = mp4::patchOffsets(payloadOf(raw.moov, o), raw.track.co64, raw.moovBox().end(), delta);
  moov.replace(static_cast<std::size_t>(at), patched.size(), patched);
  return moov;
}

// writtenMoov is the complete new moov: the padding keeps the old size when
// the new metadata fits the old room and leaves at most kMaxPadding, else it
// is kDefaultPadding (a room of 1 to 7 bytes cannot hold a free box). A file
// without metadata that gets nothing to write is left as it is.
std::string writtenMoov(const RawM4a &raw, const std::vector<std::string> &items) {
  if (!raw.meta && items.empty()) return raw.moov;
  std::string list;
  for (const auto &i : items) list += i;
  const std::string ilst = raw.ilst ? render(raw.metaChildren[*raw.ilst], list) : mp4::renderBox("ilst", list);
  const std::uint64_t bare = newMoov(raw, ilst, 0).size();
  const std::uint64_t old = raw.moovBox().size;
  std::uint64_t pad = kDefaultPadding;
  if (bare == old) {
    pad = 0;
  } else if (bare < old && old - bare >= 8 && old - bare <= kMaxPadding) {
    pad = old - bare;
  }
  std::string moov = newMoov(raw, ilst, pad);
  if (moov.size() > mp4::kMaxMoov) {
    throw Failure(code::kTooLarge, "the new moov box would have " + std::to_string(moov.size()) + " bytes, more than " +
                                       std::to_string(mp4::kMaxMoov));
  }
  const auto delta = static_cast<std::int64_t>(moov.size()) - static_cast<std::int64_t>(old);
  return fixOffsets(raw, std::move(moov), delta);
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
// with TagLib, and compares it with what was asked: the moov written, every
// other box where it was (moved by delta after the moov), the same samples
// through the fixed-up offsets, the managed values, the cover, the kept
// items and boxes. Any difference is Failure(internal).
void verifyWritten(int fd, Format format, const RawM4a &before, const M4aAnalysis &a, const std::string &moov,
                   const WriteRequest &req, const std::string &cover) {
  RawM4a raw;
  try {
    raw = readRawM4a(fd, format);
  } catch (const Failure &f) {
    if (std::string(f.code()) == code::kIO) throw;
    internal(std::string("the written file does not read back: ") + f.what());
  }
  const auto delta = static_cast<std::int64_t>(moov.size()) - static_cast<std::int64_t>(before.moovBox().size);
  bool same = raw.top.size() == before.top.size() && raw.moovIndex == before.moovIndex && raw.moov == moov;
  for (std::size_t i = 0; same && i < raw.top.size(); ++i) {
    const Box &b = before.top[i], &n = raw.top[i];
    const std::uint64_t at = i > before.moovIndex ? static_cast<std::uint64_t>(static_cast<std::int64_t>(b.offset) + delta) : b.offset;
    same = n.type == b.type && n.offset == at && (i == before.moovIndex || n.size == b.size);
  }
  if (!same) internal("the written file does not have the expected boxes");
  if (raw.samplesSHA256 != before.samplesSHA256 || raw.boxes != before.boxes) {
    internal("the audio samples or the kept boxes of the written file are not the original ones");
  }
  const M4aAnalysis after = analyze(raw);
  const Inspection in = toInspection(after, raw, format);
  if (!in.opaque.empty() || !in.conflicts.empty() || in.managed != expectedManaged(req.values)) {
    internal("the written managed fields do not read back as requested");
  }
  // Each field has at most its canonical source: an alias left with the
  // same value would be no conflict.
  for (const Mp4Field &f : kMp4Fields) {
    const auto &s = after.sources[idx(f.field)];
    const std::string canonical =
        f.field == Field::TrackTotal ? "ilst:trkn/total" : f.field == Field::DiscTotal ? "ilst:disk/total" : "ilst:" + name(f.atom);
    if (s.size() > 1 || (s.size() == 1 && s[0].key != canonical)) internal("an alias of " + canonical + " is left");
  }
  std::size_t removed = 0;
  for (const Box &b : raw.items) removed += removedItem(raw.moov, b) ? 1U : 0U;
  if (removed != 0 || raw.items.size() != managedItems(req.values).size() + a.keptItems.size() + (req.cover ? 1U : 0U)) {
    internal("the written ilst does not have exactly the managed, kept and cover items");
  }
  if (after.unmanaged != a.unmanaged || after.keptItems.size() != a.keptItems.size()) {
    internal("the written file does not keep the unmanaged items");
  }
  if (!req.cover) {
    if (!in.pictures.empty()) internal("the written file still has pictures");
  } else if (in.pictures.size() != 1 || in.pictures[0].mime != req.cover->mime || in.pictures[0].data != cover) {
    internal("the written picture is not the requested cover");
  }

  // TagLib, an independent parser, must read the same structure and the
  // managed atoms as written (checkTagLibAgrees). Every written managed atom
  // is one TagLib reads without loss, so none of them is skipped there.
  for (const Mp4Field &f : kMp4Fields) {
    if (!f.atom.empty() && !cleanAtom(raw, f.atom)) internal("TagLib cannot read the written " + name(f.atom) + " atom");
  }
  if (!cleanAtom(raw, kMp4PictureAtom)) internal("TagLib cannot read the written cover");
  checkTagLibAgrees(fd, raw);
}

}  // namespace

Inspection inspectM4a(int fd, Format format) {
  const RawM4a raw = readRawM4a(fd, format);
  const M4aAnalysis a = analyze(raw);
  checkTagLibAgrees(fd, raw);
  return toInspection(a, raw, format);
}

void writeM4a(int fd, const WriteRequest &req, const std::string &cover) {
  // 1. What is on disk; nothing is written if a field would be lost.
  const RawM4a raw = readRawM4a(fd, req.format);
  const M4aAnalysis a = analyze(raw);
  refuseBlocking(a.opaque);
  checkTagLibAgrees(fd, raw);

  // 2. The new moov, complete before anything is written.
  const std::string moov = writtenMoov(raw, newItems(raw, a, req, cover));

  // 3. Write: the new moov replaces the old one; what follows it moves when
  //    its size changes.
  if (moov != raw.moov) {
    FdStream stream(fd, true);
    stream.insert(TagLib::ByteVector(moov.data(), static_cast<unsigned int>(moov.size())),
                  static_cast<TagLib::offset_t>(raw.moovBox().offset), static_cast<std::size_t>(raw.moovBox().size));
    stream.check();
  }

  // 4. Read it back.
  verifyWritten(fd, req.format, raw, a, moov, req, cover);
}

}  // namespace mltags
