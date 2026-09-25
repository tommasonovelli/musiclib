// ID3v2: the helper's own reader of ID3v2.2, ID3v2.3 and ID3v2.4 tags and
// its ID3v2.4 writer (DESIGN.md §8.1, §8.3; NOTES.md N-094, N-152, N-153).
//
// TagLib is not used to read or write these tags: its reading upgrades or
// drops ID3v2.2 and ID3v2.3 frames (TYER+TDAT+TIME into TDRC, EQUA, RVAD,
// TSIZ discarded, v2.2 frames without a v2.4 form discarded), treats
// compressed frames as unknown without zlib, and its writing drops every
// frame flag. This reader keeps every frame as it is stored, in its ID3v2.4
// form, and the writer renders it back byte for byte.
//
// Nothing here depends on TagLib: the parsers are unit-tested under
// ASan/UBSan (tests/unit_tests.cpp).
#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

#include "inspection.h"

namespace mltags::id3v2 {

inline constexpr std::size_t kHeaderSize = 10;
// The largest 28-bit synchsafe integer: the largest tag or frame body.
inline constexpr std::uint32_t kMaxSynchsafe = 0x0FFFFFFF;
// TagLib's MAX_ID3V2_FRAME_COUNT: the frames beyond it are dropped by TagLib.
inline constexpr std::size_t kMaxFrames = 50000;

// Header is the 10-byte header of a tag.
struct Header {
  int major = 0;
  int revision = 0;
  std::uint8_t flags = 0;
  std::uint32_t size = 0;  // the body: extended header, frames, padding

  bool unsynchronised() const { return (flags & 0x80) != 0; }
  bool extended() const { return major >= 3 && (flags & 0x40) != 0; }
  bool footer() const { return major == 4 && (flags & 0x10) != 0; }
  // completeSize is the extent of the tag in the file: header, body and
  // footer; the extent TagLib's Header::completeTagSize and FFmpeg's reader
  // give to a valid header.
  std::uint64_t completeSize() const { return kHeaderSize + size + (footer() ? kHeaderSize : 0); }
};

// parseHeader reads the header of a tag from its first 10 bytes. It returns
// nullopt when they do not start with "ID3". A header that TagLib and FFmpeg
// would read differently, or that does not describe a readable tag, is
// Failure(corrupt): a version other than 2, 3 or 4, a revision of 0xFF, a
// size that is not synchsafe, a flag undefined for the version (the footer
// flag of ID3v2.3 is read by TagLib and not by FFmpeg), the compression of
// ID3v2.2 (no scheme was ever defined).
std::optional<Header> parseHeader(std::string_view b);

// Frame is one frame in its ID3v2.4 form.
struct Frame {
  std::string id;        // the ID3v2.4 identifier (an ID3v2.3 one for the frames v2.4 dropped)
  std::string sourceID;  // as stored: 3 characters in ID3v2.2
  std::size_t offset = 0;  // in the (de-unsynchronised) tag body, for messages
  // The flags a write keeps, in their ID3v2.4 form (structure 4.1):
  // status is 0abc0000 (tag alter, file alter, read only).
  std::uint8_t status = 0;
  bool grouping = false;
  std::uint8_t group = 0;
  bool compressed = false;
  bool encrypted = false;
  std::uint8_t method = 0;
  // The data length indicator, kept only for compressed or encrypted
  // frames, where it is the size of the frame once decoded.
  bool hasLength = false;
  std::uint32_t length = 0;
  // The frame's data: de-unsynchronised; for a compressed or encrypted
  // frame, the stored (compressed or encrypted) bytes.
  std::string body;

  // plain reports whether the frame has no flag: its body is its content.
  bool plain() const { return status == 0 && !grouping && !compressed && !encrypted; }
  bool operator==(const Frame &) const = default;
};

// Tag is a parsed tag: its frames in file order, and the frames the reader
// could not take as they are (Opaque keys "id3v2:<ID>" or "id3v2#<offset>").
struct Tag {
  Header header;
  std::vector<Frame> frames;
  std::vector<Opaque> opaque;
};

// parseTag parses a complete tag: tag holds the header and the whole body
// (header.completeSize() bytes, the footer included). The structure of the
// tag itself (an extended header that does not fit) is Failure(corrupt); a
// frame that cannot be kept is reported in Tag::opaque:
//   - malformed_frame (blocking): a header or length that does not parse,
//     or more than kMaxFrames frames; the frames after it are not read, as
//     TagLib does not read them either;
//   - unknown_flags (blocking): flags undefined for the version;
//   - unsupported_frame (blocking): an ID3v2.2 frame with no ID3v2.4 form
//     (CRM, LNK, an unknown identifier), an ID3v2.3 compressed frame too large
//     for an ID3v2.4 length;
//   - empty_frame (removed): a frame of size 0, which holds nothing and which
//     TagLib drops too.
// Bytes after the frames that start with a zero byte are padding, not a
// field: ignored, as TagLib ignores them.
Tag parseTag(std::string_view tag, const Header &header);

// renderFrame is the ID3v2.4 form of a frame. Failure(too_large) when its
// size exceeds kMaxSynchsafe.
std::string renderFrame(const Frame &f);

// renderTag is an ID3v2.4 tag: no flag, no extended header, the frames in
// order, then padding zero bytes. Failure(too_large) when the body exceeds
// kMaxSynchsafe.
std::string renderTag(const std::vector<Frame> &frames, std::uint64_t padding);

// deunsynchronise undoes unsynchronisation: every 0xFF 0x00 becomes 0xFF.
std::string deunsynchronise(std::string_view b);

// synchsafe decodes 4 bytes of 7 bits each; nullopt when a byte has its high
// bit set.
std::optional<std::uint32_t> synchsafe(std::string_view b);

// encodeSynchsafe is the 4-byte synchsafe form of n <= kMaxSynchsafe.
std::string encodeSynchsafe(std::uint32_t n);

// ---------------------------------------------------------------------------
// Text (structure 4, frames 4.2).

// Text encodings of a text frame.
enum Encoding : std::uint8_t { kLatin1 = 0, kUTF16 = 1, kUTF16BE = 2, kUTF8 = 3 };

// decodeStrings decodes the strings of a text field in encoding enc into
// UTF-8: the field is split at the encoding's terminator (one zero byte, or
// two aligned ones); empty strings are dropped, as TagLib drops them.
//   - Latin-1: every byte is its code point;
//   - UTF-16 (1): each string starts with its byte order mark; a string
//     without one takes the order of the first string (TagLib's rule);
//   - UTF-16BE (2);
//   - UTF-8 (3): must be valid.
// Encodings 2 and 3 are accepted in ID3v2.3 tags too, as TagLib accepts
// them. nullopt when the bytes are not valid text in the encoding: an
// unknown encoding, an odd UTF-16 length, a lone surrogate, a first UTF-16
// string without a byte order mark, invalid UTF-8.
std::optional<std::vector<std::string>> decodeStrings(std::uint8_t enc, std::string_view b);

// TextField is one terminated string of a frame and what follows it.
struct TextField {
  std::string text;  // UTF-8
  std::string_view rest;
};

// decodeTerminated decodes the string at the start of b up to its
// terminator (which must be there); nullopt when there is none or the
// string is not valid text.
std::optional<TextField> decodeTerminated(std::uint8_t enc, std::string_view b);

// decodeWhole decodes all of b as one string, without its trailing
// terminators; nullopt when it is not valid text.
std::optional<std::string> decodeWhole(std::uint8_t enc, std::string_view b);

// latin1ToUTF8 is the UTF-8 form of Latin-1 bytes.
std::string latin1ToUTF8(std::string_view b);

// id3v2Of22 maps an ID3v2.2 frame identifier to its ID3v2.4 one (to its
// ID3v2.3 one for the frames ID3v2.4 dropped: TDA, TIM, TRD, TOR, TSI, IPL,
// EQU, RVA), from the ID3v2.2 specification; "APIC" for PIC, whose body
// keeps the ID3v2.2 form. Empty when there is none (CRM, LNK, unknown).
std::string_view id3v2Of22(std::string_view id);

}  // namespace mltags::id3v2
