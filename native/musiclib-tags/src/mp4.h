// ISO base media file format (ISO/IEC 14496-12) primitives for the M4A
// reader and writer (m4a.cpp; DESIGN.md §8.1-§8.3; NOTES.md N-165, N-166):
// box headers and children, the "data" atoms of iTunes metadata items, the
// sample table (which bytes of the file are the audio samples), and the
// chunk-offset fix-up of a write that changes the size of "moov".
//
// Nothing here depends on TagLib and nothing does I/O: the functions take and
// return bytes, and are unit-tested under ASan/UBSan (tests/unit_tests.cpp).
// Every length comes from hostile input: each one is checked against the
// bytes that are there before it is used.
#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

namespace mltags::mp4 {

// kMaxMoov bounds the "moov" box the reader keeps in memory and the one a
// write produces: the bound of an ID3v2 tag (NOTES.md N-166). A larger one is
// Failure(too_large).
inline constexpr std::uint64_t kMaxMoov = 256 * 1024 * 1024;

// kMaxChildren bounds the boxes of one container, TagLib's
// MAX_MP4_ATOM_COUNT_PER_LEVEL: a container with more is Failure(corrupt).
inline constexpr std::size_t kMaxChildren = 50000;

// Box is one box: its four-byte type as stored, the offset of its first
// header byte, its size (header included) and the size of its header: 8, or
// 16 when the size is stored in 64 bits (a 32-bit size of 1).
struct Box {
  std::string type;
  std::uint64_t offset = 0;
  std::uint64_t size = 0;
  std::uint32_t header = 8;

  std::uint64_t payload() const { return offset + header; }
  std::uint64_t end() const { return offset + size; }
  std::uint64_t payloadSize() const { return size - header; }
};

std::uint16_t be16(std::string_view b, std::size_t off);
std::uint32_t be32(std::string_view b, std::size_t off);
std::uint64_t be64(std::string_view b, std::size_t off);
std::string enc16(std::uint16_t n);
std::string enc32(std::uint32_t n);
std::string enc64(std::uint64_t n);

// parseBoxHeader decodes the header of a box at offset whose bytes start at
// h (h holds up to 16 bytes, fewer at the end of the data) and that must end
// by limit. A 32-bit size of 0, "to the end", is accepted only when toEnd is
// true, and then ends at limit. nullopt when h holds fewer than 8 bytes (or
// fewer than 16 for a 64-bit size). A size smaller than its header or past
// limit is Failure(corrupt), naming what.
std::optional<Box> parseBoxHeader(std::string_view h, std::uint64_t offset, std::uint64_t limit, bool toEnd,
                                  const std::string &what);

// children parses the boxes that tile b[begin, end) exactly, their offsets
// in b. Anything else (a truncated header, a size past end, more than
// kMaxChildren boxes) is Failure(corrupt), naming what. Sizes of 0 are not
// accepted inside a container.
std::vector<Box> children(std::string_view b, std::uint64_t begin, std::uint64_t end, const std::string &what);

// tryChildren is children without the failure: nullopt when b[begin, end)
// is not a sequence of boxes.
std::optional<std::vector<Box>> tryChildren(std::string_view b, std::uint64_t begin, std::uint64_t end);

// renderBox is a box of the type around payload: an 8-byte header, or a
// 16-byte one (a 32-bit size of 1 and a 64-bit size) when large is true or
// the box does not fit in 32 bits.
std::string renderBox(std::string_view type, std::string_view payload, bool large = false);

// isFullAtomMeta reports whether a "meta" box whose payload starts at
// payload[0] is a full box (4 bytes of version and flags before its
// children), by TagLib 2.3.2's rule (MP4::Atom::read): it is not when the
// four bytes at payload[4] name one of its usual children (hdlr, ilst, mhdr,
// ctry, lang), that is when payload[0] is already a child's size.
bool isFullAtomMeta(std::string_view payload);

// ---------------------------------------------------------------------------
// iTunes metadata items (the children of "ilst").

// Well-known data types of a "data" atom.
inline constexpr std::uint32_t kTypeImplicit = 0;
inline constexpr std::uint32_t kTypeUTF8 = 1;
inline constexpr std::uint32_t kTypeUTF16 = 2;
inline constexpr std::uint32_t kTypeGIF = 12;
inline constexpr std::uint32_t kTypeJPEG = 13;
inline constexpr std::uint32_t kTypePNG = 14;
inline constexpr std::uint32_t kTypeInteger = 21;
inline constexpr std::uint32_t kTypeBMP = 27;

// Data is a "data" atom: its type (the 32-bit field, whose first byte is the
// type set, 0 for the well-known types), its locale, and its value.
struct Data {
  std::uint32_t type = 0;
  std::uint32_t locale = 0;
  std::string_view value;
};

// parseData decodes a "data" atom from the whole box bytes (header
// included); nullopt when it is not one: another type, fewer than 16 bytes,
// or a 64-bit header.
std::optional<Data> parseData(std::string_view box);

// renderData is a "data" atom of the type with locale 0.
std::string renderData(std::uint32_t type, std::string_view value);

// parseFullString decodes the "mean" or "name" atom of a freeform item from
// the whole box bytes: 4 bytes of version and flags, which must be zero,
// then the string. nullopt otherwise, or when the box is not of type.
std::optional<std::string_view> parseFullString(std::string_view box, std::string_view type);

// utf16beToUTF8 decodes UTF-16BE (data type 2) into UTF-8; nullopt for an
// odd length or an unpaired surrogate.
std::optional<std::string> utf16beToUTF8(std::string_view b);

// ---------------------------------------------------------------------------
// The sample table.

// Chunk is a run of samples stored together: its offset in the file and the
// sum of its samples' sizes.
struct Chunk {
  std::uint64_t offset = 0;
  std::uint64_t size = 0;
};

// chunks computes the chunks of a track from the payloads (after the box
// header) of its "stsc", "stsz" and "stco" or "co64" boxes. The tables must
// be consistent: stsc's first entry is chunk 1, its first chunks increase
// and stay within the chunk count, every run has at least one sample and
// sample description 1, and the samples of all chunks are exactly the
// samples of stsz. Anything else is Failure(corrupt). Bytes after the
// declared entries of a table are ignored, as FFmpeg and TagLib ignore them.
std::vector<Chunk> chunks(std::string_view stsc, std::string_view stsz, std::string_view offsets, bool co64);

// patchOffsets returns the payload of a "stco" or "co64" box (after the box
// header) with delta added to every chunk offset that is at least from: the
// fix-up of a write that changes the size of a "moov" placed before the
// media data. An offset that would leave the 32 bits of "stco" (or go
// negative) is Failure(too_large). Bytes after the entries are kept.
std::string patchOffsets(std::string_view payload, bool co64, std::uint64_t from, std::int64_t delta);

}  // namespace mltags::mp4
