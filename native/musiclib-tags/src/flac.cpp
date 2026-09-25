#include "flac.h"

#include <sys/stat.h>

#include <algorithm>
#include <cerrno>
#include <cstring>
#include <memory>
#include <optional>
#include <string_view>

#include <taglib/flacfile.h>
#include <taglib/flacpicture.h>
#include <taglib/id3v2header.h>
#include <taglib/id3v2tag.h>
#include <taglib/tstring.h>
#include <taglib/tstringlist.h>
#include <taglib/xiphcomment.h>

#include "failure.h"
#include "fdio.h"
#include "sha256.h"
#include "text.h"

namespace mltags {

namespace {

// Limits of the format (RFC 9639) and of TagLib 2.3.2, which the reader
// enforces so that it never accepts what TagLib would truncate.
constexpr std::uint32_t kMaxBlockLength = 0xFFFFFF;  // 24-bit block length
constexpr std::size_t kMaxBlocks = 50000;            // MAX_FLAC_METADATA_BLOCK_COUNT
constexpr std::uint32_t kMaxVorbisFields = 50000;    // MAX_XIPH_COMMENT_FIELD_COUNT

enum BlockType : std::uint8_t {
  kStreamInfo = 0,
  kPadding = 1,
  kApplication = 2,
  kSeekTable = 3,
  kVorbisComment = 4,
  kPictureBlock = 6,
  kForbiddenType = 127,
};

[[noreturn]] void corrupt(const std::string &msg) { throw Failure(code::kCorrupt, msg); }
[[noreturn]] void internal(const std::string &msg) { throw Failure(code::kInternal, msg); }

std::uint32_t be32(std::string_view d, std::size_t off) {
  return (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off])) << 24) |
         (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 1])) << 16) |
         (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 2])) << 8) |
         static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 3]));
}

std::uint32_t le32(std::string_view d, std::size_t off) {
  return (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 3])) << 24) |
         (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 2])) << 16) |
         (static_cast<std::uint32_t>(static_cast<unsigned char>(d[off + 1])) << 8) |
         static_cast<std::uint32_t>(static_cast<unsigned char>(d[off]));
}

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

// ---------------------------------------------------------------------------
// The raw metadata, read with pread only.

struct Block {
  std::size_t index = 0;  // position among all blocks, padding included
  std::uint8_t type = 0;
  std::string data;
};

struct RawFlac {
  bool id3v2 = false;
  // The complete size of the ID3v2 tag: header, body and footer.
  std::uint64_t id3v2Size = 0;
  bool id3v1 = false;
  std::vector<Block> blocks;  // every block but PADDING, in file order
  // The audio frames: after the metadata blocks, before an ID3v1 tag.
  AudioRange audio;
};

// readRawFlac reads the metadata blocks. The stream must start with "fLaC",
// after an ID3v2 tag at most. The ID3v2 tag is located, not parsed: a write
// strips it whole (NOTES.md N-090). Its extent must be exactly the one
// TagLib's ID3v2::Header computes, which is the extent TagLib removes:
// a header TagLib reads as a 10-byte tag of size 0 (a size byte of 0x80 or
// more, a version or revision of 0xFF) is Failure(corrupt).
RawFlac readRawFlac(int fd) {
  const std::uint64_t size = fileSize(fd);
  RawFlac raw;
  std::uint64_t pos = 0;
  if (size >= 10) {
    const std::string h = readAt(fd, 0, 10, "the file");
    if (h.compare(0, 3, "ID3") == 0) {
      raw.id3v2 = true;
      if (static_cast<unsigned char>(h[3]) == 0xFF || static_cast<unsigned char>(h[4]) == 0xFF) {
        corrupt("the version of the ID3v2 tag is 0xFF");
      }
      std::uint64_t tagSize = 0;
      for (std::size_t i = 6; i < 10; ++i) {
        const auto b = static_cast<unsigned char>(h[i]);
        if ((b & 0x80) != 0) corrupt("the size of the ID3v2 tag is not synchsafe");
        tagSize = (tagSize << 7) | b;
      }
      const bool footer = (static_cast<unsigned char>(h[5]) & 0x10) != 0;
      raw.id3v2Size = 10 + tagSize + (footer ? 10 : 0);
      if (raw.id3v2Size > size) corrupt("the ID3v2 tag extends past the end of the file");
      pos = raw.id3v2Size;
    }
  }
  if (size < pos + 4 || readAt(fd, pos, 4, "the file") != "fLaC") {
    throw Failure(code::kFormatMismatch, "not a FLAC stream: no fLaC marker at the start");
  }
  pos += 4;
  for (std::size_t i = 0;; ++i) {
    if (i >= kMaxBlocks) corrupt("more than " + std::to_string(kMaxBlocks) + " metadata blocks");
    if (size - pos < 4) corrupt("metadata block " + std::to_string(i) + ": truncated header");
    const std::string hdr = readAt(fd, pos, 4, "a metadata block header");
    const auto h0 = static_cast<unsigned char>(hdr[0]);
    const auto type = static_cast<std::uint8_t>(h0 & 0x7F);
    const bool last = (h0 & 0x80) != 0;
    const std::uint32_t len = be32(hdr, 0) & 0xFFFFFF;
    if (i == 0 && type != kStreamInfo) corrupt("the first metadata block is not STREAMINFO");
    if (type == kForbiddenType) corrupt("metadata block " + std::to_string(i) + " has the forbidden type 127");
    // TagLib's FLAC::File::scan refuses these too.
    if (len == 0 && type != kPadding && type != kSeekTable) {
      corrupt("metadata block " + std::to_string(i) + " is empty");
    }
    pos += 4;
    if (size - pos < len) corrupt("metadata block " + std::to_string(i) + " extends past the end of the file");
    if (type != kPadding) raw.blocks.push_back(Block{i, type, readAt(fd, pos, len, "a metadata block")});
    pos += len;
    if (last) break;
  }
  // ID3v1, found exactly as TagLib's Utils::findID3v1 finds it: "TAG" 128
  // bytes before the end, unless that is the end of an APEv2 footer.
  if (size >= 131) {
    const std::string t = readAt(fd, size - 131, 8, "the end of the file");
    raw.id3v1 = t.compare(3, 3, "TAG") == 0 && t != "APETAGEX";
  } else if (size >= 128) {
    raw.id3v1 = readAt(fd, size - 128, 3, "the end of the file") == "TAG";
  }
  // A write truncates the file where the ID3v1 tag starts: it must start
  // after the metadata blocks.
  if (raw.id3v1 && size - 128 < pos) corrupt("an ID3v1 tag overlaps the metadata blocks");
  raw.audio = AudioRange{pos, raw.id3v1 ? size - 128 : size};
  return raw;
}

// parsePicture decodes a FLAC picture structure (RFC 9639 §8.8). Every
// length is checked against what is left, without overflow; nullopt when
// the structure is invalid (TagLib then drops the picture).
std::optional<Picture> parsePicture(std::string_view d, const char *location) {
  std::size_t pos = 0;
  auto u32 = [&](std::uint32_t &out) {
    if (d.size() - pos < 4) return false;
    out = be32(d, pos);
    pos += 4;
    return true;
  };
  Picture p;
  p.location = location;
  std::uint32_t mimeLen = 0, descLen = 0, dataLen = 0;
  if (d.size() < 32 || !u32(p.type) || !u32(mimeLen) || mimeLen > d.size() - pos) return std::nullopt;
  p.mime.assign(d.substr(pos, mimeLen));
  pos += mimeLen;
  if (!u32(descLen) || descLen > d.size() - pos) return std::nullopt;
  pos += descLen;
  if (!u32(p.width) || !u32(p.height) || !u32(p.depth) || !u32(p.colors) || !u32(dataLen) ||
      dataLen > d.size() - pos) {
    return std::nullopt;
  }
  // RFC 9639: the media type is printable ASCII (possibly empty).
  if (!p.mime.empty() && !isPrintableASCII(p.mime)) return std::nullopt;
  p.data.assign(d.substr(pos, dataLen));
  return p;
}

// ---------------------------------------------------------------------------
// Vorbis comments.

struct Vorbis {
  std::optional<std::string> vendor;  // nullopt when it cannot be saved back
  FieldMap fields;                    // valid, non-empty values only
  std::vector<Picture> pictures;      // METADATA_BLOCK_PICTURE and COVERART
  std::vector<Opaque> opaque;
};

// addEntry classifies one "KEY=value" entry exactly as TagLib's
// XiphComment::parse would treat it, and reports as Opaque every entry that
// TagLib would drop or alter.
void addEntry(Vorbis &v, std::size_t n, std::string_view entry) {
  const std::size_t sep = entry.find('=');
  const std::string where = "vorbis#" + std::to_string(n);
  if (sep == std::string_view::npos || sep == 0) {
    v.opaque.push_back({where, reason::kMalformedEntry, false});
    return;
  }
  const std::string_view rawKey = entry.substr(0, sep);
  for (const char c : rawKey) {
    const auto b = static_cast<unsigned char>(c);
    if (b < 0x20 || b > 0x7D) {
      v.opaque.push_back({where, reason::kInvalidKey, false});
      return;
    }
  }
  const std::string key = upperASCII(rawKey);
  const std::string_view value = entry.substr(sep + 1);
  const bool removed = isVorbisManagedKey(key);

  if (key == "METADATA_BLOCK_PICTURE" || key == "COVERART") {
    const auto decoded = decodeBase64(value);
    std::optional<Picture> p;
    if (decoded && !decoded->empty()) {
      if (key == "COVERART") {
        // The legacy form: the image file itself, type and MIME unknown.
        p = Picture{"comment", 0, "", 0, 0, 0, 0, *decoded};
      } else {
        p = parsePicture(*decoded, "comment");
      }
    }
    if (p) {
      v.pictures.push_back(std::move(*p));
    } else {
      v.opaque.push_back({"vorbis:" + key, reason::kInvalidPicture, true});
    }
    return;
  }
  if (value.find('\0') != std::string_view::npos) {
    v.opaque.push_back({"vorbis:" + key, reason::kNulByte, removed});  // TagLib truncates at NUL
    return;
  }
  if (!validUTF8(value)) {
    v.opaque.push_back({"vorbis:" + key, reason::kInvalidUTF8, removed});  // TagLib empties it
    return;
  }
  // TagLib drops empty values: an empty value is no value (NOTES.md N-086).
  if (value.empty()) return;
  v.fields[key].emplace_back(value);
}

// parseVorbis decodes a VORBIS_COMMENT block. A damaged structure is
// Failure(corrupt): TagLib would silently drop the rest of the block.
// Bytes after the last entry are not a field and are ignored, as TagLib
// ignores them.
Vorbis parseVorbis(std::string_view d) {
  Vorbis v;
  if (d.size() < 8) corrupt("the Vorbis comment block is shorter than 8 bytes");
  std::size_t pos = 0;
  const std::uint32_t vendorLen = le32(d, pos);
  pos += 4;
  if (vendorLen > d.size() - pos) corrupt("the Vorbis vendor string extends past its block");
  const std::string_view vendor = d.substr(pos, vendorLen);
  pos += vendorLen;
  if (vendor.find('\0') != std::string_view::npos) {
    v.opaque.push_back({"vorbis.vendor", reason::kNulByte, false});
  } else if (!validUTF8(vendor)) {
    v.opaque.push_back({"vorbis.vendor", reason::kInvalidUTF8, false});
  } else {
    v.vendor = std::string(vendor);
  }
  if (d.size() - pos < 4) corrupt("the Vorbis comment block has no field count");
  const std::uint32_t count = le32(d, pos);
  pos += 4;
  if (count > kMaxVorbisFields || count > (d.size() - 8) / 4) {
    corrupt("the Vorbis comment block declares " + std::to_string(count) + " fields");
  }
  for (std::uint32_t i = 0; i < count; ++i) {
    if (d.size() - pos < 4) corrupt("Vorbis comment field " + std::to_string(i) + ": truncated length");
    const std::uint32_t len = le32(d, pos);
    pos += 4;
    if (len > d.size() - pos) corrupt("Vorbis comment field " + std::to_string(i) + " extends past its block");
    addEntry(v, i, d.substr(pos, len));
    pos += len;
  }
  return v;
}

// isForeignMetadata reports whether an APPLICATION block carries RIFF iXML
// or bext data, which TagLib 2.3 extracts on read and re-renders in another
// form and position on save (FLAC::File::scan and save).
bool isForeignMetadata(std::string_view d) {
  if (d.size() < 4) return false;
  const std::string_view app = d.substr(0, 4);
  std::string_view inner;
  if (app == "riff" && d.size() >= 12) {
    inner = d.substr(4, 4);
  } else if (app == "iXML" || app == "bext") {
    inner = app;
  }
  return inner == "iXML" || inner == "bext";
}

// ---------------------------------------------------------------------------
// From the raw metadata to an inspection.

struct Analysis {
  bool hasComment = false;
  Vorbis comment;
  std::vector<Picture> pictures;
  std::vector<Opaque> opaque;
  // "type:sha256" of every block a write keeps byte for byte (STREAMINFO,
  // SEEKTABLE, APPLICATION, CUESHEET, reserved types), in file order.
  std::vector<std::string> blocks;
};

Analysis analyze(const RawFlac &raw) {
  Analysis a;
  // ID3 tags are not part of FLAC: a write strips them (NOTES.md N-090).
  if (raw.id3v2) a.opaque.push_back({"id3v2", reason::kForeignTag, true});
  for (const Block &b : raw.blocks) {
    const std::string where = "flac.block#" + std::to_string(b.index);
    if (b.type == kVorbisComment) {
      if (a.hasComment) {
        // TagLib keeps only the first one.
        a.opaque.push_back({where, reason::kDuplicateBlock, false});
        continue;
      }
      a.hasComment = true;
      a.comment = parseVorbis(b.data);
      a.pictures.insert(a.pictures.end(), a.comment.pictures.begin(), a.comment.pictures.end());
      a.opaque.insert(a.opaque.end(), a.comment.opaque.begin(), a.comment.opaque.end());
    } else if (b.type == kPictureBlock) {
      if (auto p = parsePicture(b.data, "block")) {
        a.pictures.push_back(std::move(*p));
      } else {
        a.opaque.push_back({where, reason::kInvalidPicture, true});
      }
    } else if (b.type == kApplication && isForeignMetadata(b.data)) {
      a.opaque.push_back({where, reason::kForeignMetadata, false});
    } else {
      a.blocks.push_back(std::to_string(b.type) + ":" + sha256Hex(b.data));
    }
  }
  if (raw.id3v1) a.opaque.push_back({"id3v1", reason::kForeignTag, true});
  return a;
}

// splitNumbers splits "N/M" values into the parts before the first '/' and
// the parts after it (for the values that have one). Empty parts are
// dropped.
void splitNumbers(const std::vector<std::string> &values, std::vector<std::string> &numbers,
                  std::vector<std::string> &totals) {
  for (const auto &v : values) {
    const std::size_t slash = v.find('/');
    std::string n = v.substr(0, slash);
    if (!n.empty()) numbers.push_back(std::move(n));
    if (slash != std::string::npos && slash + 1 < v.size()) totals.push_back(v.substr(slash + 1));
  }
}

// resolveManaged applies DESIGN.md §8.1 to the Vorbis fields: for each
// managed field the canonical key wins, then its aliases in table order;
// the total of a track or disc number written as "N/M" comes last. Sources
// that disagree with the chosen one are reported as a conflict.
void resolveManaged(const FieldMap &fields, Inspection &in) {
  auto lookup = [&](std::string_view key) {
    const auto it = fields.find(std::string(key));
    return it == fields.end() ? std::vector<std::string>{} : it->second;
  };
  // The "/total" source of the track and disc fields.
  KeyValues embeddedTotal[kFieldCount];

  for (const VorbisField &vf : kVorbisFields) {
    std::vector<KeyValues> sources;
    std::vector<std::string_view> keys{vf.canonical};
    for (const auto a : vf.aliases) {
      if (!a.empty()) keys.push_back(a);
    }
    const bool numbered = vf.field == Field::Track || vf.field == Field::Disc;
    bool haveEmbedded = false;
    for (const auto key : keys) {
      const std::vector<std::string> values = lookup(key);
      if (values.empty()) continue;
      if (numbered) {
        std::vector<std::string> numbers, totals;
        splitNumbers(values, numbers, totals);
        if (!haveEmbedded) {
          // The totals come from the first key that is present.
          haveEmbedded = true;
          const Field total = vf.field == Field::Track ? Field::TrackTotal : Field::DiscTotal;
          embeddedTotal[idx(total)] = KeyValues{std::string(key) + "/total", std::move(totals)};
        }
        if (!numbers.empty()) sources.push_back(KeyValues{std::string(key), std::move(numbers)});
      } else {
        sources.push_back(KeyValues{std::string(key), values});
      }
    }
    if (!embeddedTotal[idx(vf.field)].values.empty()) sources.push_back(embeddedTotal[idx(vf.field)]);

    if (sources.empty()) continue;
    in.managed[idx(vf.field)] = sources.front().values;
    const bool disagree = std::any_of(sources.begin() + 1, sources.end(),
                                      [&](const KeyValues &s) { return s.values != sources.front().values; });
    if (disagree) in.conflicts.push_back(Conflict{vf.field, std::move(sources)});
  }
}

Inspection toInspection(const Analysis &a, const AudioRange &audio) {
  Inspection in;
  in.format = Format::FLAC;
  in.audio = audio;
  resolveManaged(a.comment.fields, in);
  in.pictures = a.pictures;
  for (const auto &[key, values] : a.comment.fields) {
    if (!isVorbisManagedKey(key)) in.unmanaged.push_back(KeyValues{"vorbis:" + key, values});
  }
  // An empty vendor is no vendor: TagLib writes an empty one into a block it
  // creates.
  if (a.comment.vendor && !a.comment.vendor->empty()) {
    in.unmanaged.push_back(KeyValues{"vorbis.vendor", {*a.comment.vendor}});
  }
  if (!a.blocks.empty()) in.unmanaged.push_back(KeyValues{"flac.blocks", a.blocks});
  std::sort(in.unmanaged.begin(), in.unmanaged.end(),
            [](const KeyValues &x, const KeyValues &y) { return x.key < y.key; });
  in.opaque = a.opaque;
  return in;
}

// ---------------------------------------------------------------------------
// TagLib.

// fieldMapOf is TagLib's view of a Vorbis comment, in the reader's form.
FieldMap fieldMapOf(const TagLib::Ogg::XiphComment &x) {
  FieldMap m;
  for (const auto &[key, values] : x.fieldListMap()) {
    auto &out = m[key.to8Bit(true)];
    for (const auto &v : values) out.push_back(v.to8Bit(true));
  }
  return m;
}

// describeDifference names the first key where two maps differ.
std::string describeDifference(const FieldMap &got, const FieldMap &want) {
  for (const auto &[k, v] : want) {
    const auto it = got.find(k);
    if (it == got.end()) return "missing " + k;
    if (it->second != v) return "different values of " + k;
  }
  for (const auto &[k, v] : got) {
    if (want.find(k) == want.end()) return "unexpected " + k;
  }
  return "no difference";
}

// checkTagLibAgrees verifies that TagLib reads the same file the reader
// read: the same foreign tags, an ID3v2 tag of the same extent (the bytes a
// write strips, N-090) and, when the comment has nothing opaque, the same
// fields and vendor.
void checkTagLibAgrees(TagLib::FLAC::File &file, const RawFlac &raw, const Analysis &a) {
  if (file.hasID3v2Tag() != raw.id3v2 || file.hasID3v1Tag() != raw.id3v1) {
    internal("TagLib and the helper disagree on the ID3 tags of the file");
  }
  if (raw.id3v2) {
    const TagLib::ID3v2::Tag *id3 = file.ID3v2Tag(false);
    if (id3 == nullptr || id3->header()->completeTagSize() != raw.id3v2Size) {
      internal("TagLib and the helper disagree on the size of the ID3v2 tag");
    }
  }
  const bool foreign = file.hasiXMLData() || file.hasBEXTData();
  const bool reported = std::any_of(a.opaque.begin(), a.opaque.end(),
                                    [](const Opaque &o) { return o.reason == std::string(reason::kForeignMetadata); });
  if (foreign && !reported) internal("TagLib found iXML or bext data the helper did not report");
  if (!a.comment.opaque.empty()) return;
  const TagLib::Ogg::XiphComment *x = file.xiphComment(false);
  const FieldMap taglib = x != nullptr ? fieldMapOf(*x) : FieldMap{};
  if (taglib != a.comment.fields) {
    internal("TagLib and the helper read the Vorbis comment differently: " +
             describeDifference(taglib, a.comment.fields));
  }
  const std::string vendor = x != nullptr ? x->vendorID().to8Bit(true) : std::string();
  if (a.comment.vendor.value_or("") != vendor) internal("TagLib and the helper read a different Vorbis vendor");
}

std::string managedValue(const ManagedValues &mv, Field f) {
  if (f == Field::Compilation) return mv.compilation ? "1" : "";
  if (isNumberField(f)) return mv.number[idx(f)] ? std::to_string(*mv.number[idx(f)]) : "";
  return mv.text[idx(f)].value_or("");
}

// applyManaged removes every key the write manages from TagLib's comment
// (canonical keys, aliases, sort keys, picture keys, whatever their
// values) and its pictures, then sets the requested values. Only explicit
// keys are used: no TagLib property map and no Tag::set* helper, whose
// aliases and conversions are TagLib's.
void applyManaged(TagLib::Ogg::XiphComment &x, const ManagedValues &mv) {
  std::vector<TagLib::String> managed;
  for (const auto &entry : x.fieldListMap()) {
    if (isVorbisManagedKey(entry.first.to8Bit(true))) managed.push_back(entry.first);
  }
  for (const auto &k : managed) x.removeFields(k);
  x.removeAllPictures();
  for (const VorbisField &vf : kVorbisFields) {
    const std::string v = managedValue(mv, vf.field);
    if (!v.empty()) {
      x.addField(TagLib::String(std::string(vf.canonical), TagLib::String::UTF8), TagLib::String(v, TagLib::String::UTF8),
                 true);
    }
  }
}

// expectedFields is the comment a write must produce: every unmanaged field
// as it was, plus the requested managed values under their canonical keys.
FieldMap expectedFields(const Analysis &before, const ManagedValues &mv) {
  FieldMap want;
  for (const auto &[key, values] : before.comment.fields) {
    if (!isVorbisManagedKey(key)) want[key] = values;
  }
  for (const VorbisField &vf : kVorbisFields) {
    const std::string v = managedValue(mv, vf.field);
    if (!v.empty()) want[std::string(vf.canonical)] = {v};
  }
  return want;
}

void checkTagLibWillWrite(const TagLib::Ogg::XiphComment &x, const FieldMap &want, const std::string &vendor) {
  const FieldMap got = fieldMapOf(x);
  if (got != want) internal("TagLib's comment is not the expected one: " + describeDifference(got, want));
  if (x.vendorID().to8Bit(true) != vendor) internal("TagLib's Vorbis vendor is not the original one");
}

void save(TagLib::FLAC::File &file, FdStream &stream) {
  const bool ok = file.save();
  stream.check();
  if (!ok) internal("TagLib refused to save the file");
}

// verifyWritten reads the written file back with the reader and compares it
// with what was asked: no ID3 tag, the fields, the vendor, the kept blocks
// and the cover. Any difference is Failure(internal); the caller discards
// the file.
void verifyWritten(int fd, const Analysis &before, const FieldMap &want, const std::string &vendor,
                   const WriteRequest &req, const std::string &cover) {
  const RawFlac raw = readRawFlac(fd);
  if (raw.id3v2 || raw.id3v1) internal("the written file still has an ID3 tag");
  const Analysis after = analyze(raw);
  if (!after.opaque.empty()) internal("the written file has a field the helper cannot save back");
  if (!after.hasComment) internal("the written file has no Vorbis comment");
  if (after.comment.fields != want) {
    internal("the written comment is not the expected one: " + describeDifference(after.comment.fields, want));
  }
  if (after.comment.vendor.value_or("") != vendor) internal("the written Vorbis vendor changed");
  if (after.blocks != before.blocks) internal("a metadata block that is not a tag changed");
  if (!req.cover) {
    if (!after.pictures.empty()) internal("the written file still has pictures");
    return;
  }
  const CoverSpec &c = *req.cover;
  if (after.pictures.size() != 1) internal("the written file does not have exactly one picture");
  const Picture &p = after.pictures.front();
  if (p.location != "block" || p.type != kFrontCover || p.mime != c.mime || p.width != c.width ||
      p.height != c.height || p.depth != c.depth || p.colors != c.colors || p.data != cover) {
    internal("the written picture is not the requested cover");
  }
}

}  // namespace

Inspection inspectFlac(int fd) {
  const RawFlac raw = readRawFlac(fd);
  const Analysis a = analyze(raw);
  // TagLib must accept the file too: otherwise a write would fail on it.
  FdStream stream(fd, false);
  TagLib::FLAC::File file(&stream, false);
  stream.check();
  if (!file.isValid()) corrupt("TagLib cannot read the FLAC metadata");
  checkTagLibAgrees(file, raw, a);
  return toInspection(a, raw.audio);
}

void writeFlac(int fd, const WriteRequest &req, const std::string &cover) {
  // 1. What is on disk, read independently of TagLib; nothing is written
  //    if a field would be lost.
  const RawFlac raw = readRawFlac(fd);
  const Analysis before = analyze(raw);
  refuseBlocking(before.opaque);
  const FieldMap want = expectedFields(before, req.values);
  const std::string vendor = before.comment.vendor.value_or("");
  // TagLib keeps no field of a comment that has more than kMaxVorbisFields:
  // a file at the limit plus new managed fields could not be read back.
  std::size_t fieldCount = 0;
  for (const auto &kv : want) fieldCount += kv.second.size();
  if (fieldCount > kMaxVorbisFields) {
    throw Failure(code::kTooLarge, "the Vorbis comment would have " + std::to_string(fieldCount) +
                                       " fields, more than the " + std::to_string(kMaxVorbisFields) +
                                       " a reader keeps");
  }

  // 2. TagLib reads the same file.
  FdStream stream(fd, true);
  TagLib::FLAC::File file(&stream, false);
  stream.check();
  if (!file.isValid()) corrupt("TagLib cannot read the FLAC metadata");
  checkTagLibAgrees(file, raw, before);

  // 3. The new tags, checked before anything is written.
  TagLib::Ogg::XiphComment *x = file.xiphComment(true);
  applyManaged(*x, req.values);
  file.removePictures();
  checkTagLibWillWrite(*x, want, vendor);
  if (x->render(false).size() > kMaxBlockLength) {
    throw Failure(code::kTooLarge, "the Vorbis comment would exceed the 16 MiB limit of a FLAC metadata block");
  }
  if (req.cover) {
    const CoverSpec &c = *req.cover;
    // type, MIME length, MIME, description length, width, height, depth,
    // colors, data length: 32 bytes of fixed fields.
    if (32 + c.mime.size() + cover.size() > kMaxBlockLength) {
      throw Failure(code::kTooLarge, "the cover does not fit in a FLAC metadata block (16 MiB)");
    }
    auto pic = std::make_unique<TagLib::FLAC::Picture>();
    pic->setType(TagLib::FLAC::Picture::FrontCover);
    pic->setMimeType(TagLib::String(c.mime, TagLib::String::UTF8));
    // TagLib takes int and renders it as a 32-bit unsigned field: the
    // conversion is modular (C++20), and verifyWritten checks the result.
    pic->setWidth(static_cast<int>(c.width));
    pic->setHeight(static_cast<int>(c.height));
    pic->setColorDepth(static_cast<int>(c.depth));
    pic->setNumColors(static_cast<int>(c.colors));
    pic->setData(TagLib::ByteVector(cover.data(), static_cast<unsigned int>(cover.size())));
    file.addPicture(pic.release());  // from here on the file owns it
  }

  // 4. ID3 tags are not part of FLAC: the output never carries them
  //    (DESIGN.md §8.3, declared rule of NOTES.md N-090). Stripped from
  //    TagLib's view before the first save, so that Tag::duplicate below
  //    cannot copy an ID3 value into a new comment block either; the save
  //    then removes the leading ID3v2 and truncates the trailing ID3v1.
  file.strip(TagLib::FLAC::File::ID3v1 | TagLib::FLAC::File::ID3v2);

  // 5. Save. When the file had no comment block, FLAC::File::save first
  //    copies the other tags into the new one with Tag::duplicate, which
  //    removes a DATE that is not a number; the second save, on a file that
  //    now has a comment block, writes the requested values again.
  //    When an ID3 tag was stripped, the first save chose the padding
  //    against the length of the file with the tag (it removes the tag
  //    after writing the metadata); the second save chooses it against the
  //    final length, as a later write of the output would, so that writing
  //    the output again gives the same bytes.
  save(file, stream);
  if (!before.hasComment || raw.id3v2 || raw.id3v1) {
    applyManaged(*x, req.values);
    checkTagLibWillWrite(*x, want, vendor);
    save(file, stream);
  }

  // 6. Read it back.
  verifyWritten(fd, before, want, vendor, req, cover);
}

}  // namespace mltags
