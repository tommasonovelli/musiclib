// FLAC: inspect and write-managed-tags (DESIGN.md §8.1-§8.3).
#pragma once

#include <string>

#include "inspection.h"
#include "request.h"

namespace mltags {

// inspectFlac reads the FLAC file on fd (readable, regular). The metadata is
// read by this helper's own strict parser, which also finds every field
// TagLib would drop or alter on a save (reported as Opaque); TagLib must
// accept the file as well, so that a later write cannot fail on it.
Inspection inspectFlac(int fd);

// writeFlac rewrites the managed fields and the cover of the FLAC file on fd
// (read-write, regular), in place, with TagLib. cover holds the image bytes
// when req.cover is set. Nothing is written unless every check passes; the
// result is read back and compared with what was asked before returning.
void writeFlac(int fd, const WriteRequest &req, const std::string &cover);

}  // namespace mltags
