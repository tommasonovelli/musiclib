// M4A: inspect and write-managed-tags (DESIGN.md §8.1-§8.3; NOTES.md N-094,
// N-165 to N-168).
#pragma once

#include <cstdint>
#include <string>

#include "inspection.h"
#include "mp4.h"
#include "request.h"

namespace mltags {

// kM4aMaxCover is the largest cover a write of an M4A reads, refused before
// reading: the bound of the whole "moov" (mp4::kMaxMoov) minus the headers of
// the "covr" item and of its "data" atom. A moov that already holds other
// large items can still refuse a smaller cover (too_large, NOTES.md N-166).
inline constexpr std::uint64_t kM4aMaxCover = mp4::kMaxMoov - 24;

// inspectM4a reads the M4A file on fd (readable, regular) as format
// (m4a-aac or m4a-alac): its structure with the helper's own box walker,
// which must find exactly one track, an audio track whose one sample
// description is "mp4a" (AAC) or "alac" (ALAC) as declared, without
// encryption, fragments or external media data (Failure(unsupported_format)
// otherwise), and the iTunes metadata of moov/udta/meta/ilst. The managed
// fields follow §8.1: the canonical atom of the §8.2 table wins over its
// aliases, with the disagreeing sources as conflicts. TagLib must see the
// same structure and read the managed atoms it can read the same way.
Inspection inspectM4a(int fd, Format format);

// writeM4a rewrites the metadata of the M4A file on fd (read-write, regular)
// in place (§8.2, §8.3):
//   - the ilst holds the managed atoms as requested (absent = none), then
//     every other item kept byte for byte in its order, with the cover (one
//     "covr" image, or none) where the first "covr" was, else at the end;
//   - the aliases, sort atoms and numeric genre of the table are gone;
//   - "moov" keeps its size when the free padding next to the ilst allows it,
//     so that nothing else moves; otherwise the media data after it moves and
//     every chunk offset that points past it is fixed up;
//   - every other box and the audio samples are unchanged.
// A field that cannot be kept refuses the file before anything is written
// (Failure(opaque_field)). The result is read back by the helper's reader and
// by TagLib and compared with what was asked: Failure(internal) otherwise.
void writeM4a(int fd, const WriteRequest &req, const std::string &cover);

}  // namespace mltags
