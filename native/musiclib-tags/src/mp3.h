// MP3: inspect and write-managed-tags (DESIGN.md §8.1-§8.3; NOTES.md N-094,
// N-152, N-153).
#pragma once

#include <string>

#include "inspection.h"
#include "request.h"

namespace mltags {

// inspectMp3 reads the MP3 file on fd (readable, regular): its ID3v2 tag
// (versions 2.2 to 2.4), its APE tag and its ID3v1 tag, each with the
// helper's own parser. The managed fields follow §8.1: the first non-empty
// value in ID3v2, then APE, then ID3v1, with the disagreeing sources as
// conflicts. TagLib must see the same tags in the same places.
Inspection inspectMp3(int fd);

// writeMp3 rewrites the tags of the MP3 file on fd (read-write, regular), in
// place (§8.2, §8.3):
//   - an ID3v2.4 tag: the managed frames as requested (absent = none), the
//     cover as the one APIC front cover, every other frame kept as it was,
//     byte for byte, in its ID3v2.4 form, then the ID3v1 comment migrated to
//     a COMM frame when no COMM already holds it (description
//     "legacy-id3v1", language "XXX");
//   - the APE tag without its managed, sort and cover items, every other
//     item kept byte for byte; removed when no item is left;
//   - no ID3v1 tag.
// The audio bytes are not changed. A field that cannot be kept refuses the
// file before anything is written (Failure(opaque_field)). The result is
// read back by the helper's reader and by TagLib and compared with what was
// asked: Failure(internal) otherwise.
void writeMp3(int fd, const WriteRequest &req, const std::string &cover);

}  // namespace mltags
