// musiclib-tags: the TagLib helper of musiclib (DESIGN.md §2.1, §8.1).
//
// Usage: musiclib-tags <operation>
//
//   version             print {"helper": ..., "taglib": ...}
//   inspect             descriptor 3: the audio file (readable)
//   extract-images      descriptor 3: the audio file (readable);
//                       descriptors 4, 5, ...: one empty, writable regular
//                       file per requested picture, in request order
//   write-managed-tags  descriptor 3: the staging copy (read-write);
//                       descriptor 4: the cover image, when one is requested
//
// The request is JSON on standard input (at most kMaxRequestBytes); the
// result is JSON on standard output, exit status 0. A failure prints
// {"code": ..., "message": ...} on standard error and exits with status 3;
// any other status (a crash, a signal) is a failure of the tool. The helper
// never receives a path, never runs a shell and knows nothing of the domain:
// it reads and writes tags of the descriptors it is given.
#include <unistd.h>

#include <cerrno>
#include <cstring>
#include <new>
#include <string>
#include <string_view>

#include <taglib/tstring.h>
#include <taglib/tversionnumber.h>

#include "failure.h"
#include "fdio.h"
#include "flac.h"
#include "inspection.h"
#include "json.h"
#include "request.h"
#include "sha256.h"
#include "version.h"

namespace mltags {
namespace {

constexpr int kExitFailure = 3;
constexpr int kAudioFD = 3;
constexpr int kFirstExtraFD = 4;

// readRequest reads standard input to the end, refusing more than
// kMaxRequestBytes.
std::string readRequest() {
  std::string in;
  char buf[16384];
  while (true) {
    const ssize_t n = read(STDIN_FILENO, buf, sizeof buf);
    if (n < 0) {
      if (errno == EINTR) continue;
      throw Failure(code::kIO, std::string("reading the request: ") + std::strerror(errno));
    }
    if (n == 0) return in;
    in.append(buf, static_cast<std::size_t>(n));
    if (in.size() > kMaxRequestBytes) {
      throw Failure(code::kInvalidRequest, "the request is larger than " + std::to_string(kMaxRequestBytes) + " bytes");
    }
  }
}

// writeOut writes text and a newline to a descriptor that is a pipe.
void writeOut(int fd, const std::string &text) {
  const std::string line = text + "\n";
  std::size_t done = 0;
  while (done < line.size()) {
    const ssize_t w = write(fd, line.data() + done, line.size() - done);
    if (w < 0) {
      if (errno == EINTR) continue;
      throw Failure(code::kIO, std::string("writing the result: ") + std::strerror(errno));
    }
    done += static_cast<std::size_t>(w);
  }
}

// requireFormat refuses the formats an operation does not handle yet.
void requireFLAC(Format f, std::string_view op) {
  if (f != Format::FLAC) {
    throw Failure(code::kUnsupportedFormat,
                  std::string(op) + " of " + std::string(formatName(f)) + " is not implemented (Phase 4)");
  }
}

std::string version() {
  json::Writer w;
  w.beginObject();
  w.key("helper");
  w.string(kHelperVersion);
  w.key("taglib");
  w.string(TagLib::runtimeVersion().toString().to8Bit(false) + "-" + kTagLibBuild);
  w.endObject();
  return w.take();
}

std::string inspect(std::string_view request) {
  const InspectRequest req = parseInspectRequest(request);
  requireFLAC(req.format, "inspect");
  checkDescriptor(kAudioFD, Access::Read, "the audio file");
  return renderInspection(inspectFlac(kAudioFD));
}

std::string extractImages(std::string_view request) {
  const ExtractRequest req = parseExtractRequest(request);
  requireFLAC(req.format, "extract-images");
  checkDescriptor(kAudioFD, Access::Read, "the audio file");
  // Every destination is checked before anything is read or written.
  for (std::size_t i = 0; i < req.pictures.size(); ++i) {
    const int fd = kFirstExtraFD + static_cast<int>(i);
    if (checkDescriptor(fd, Access::Write, "output " + std::to_string(i)) != 0) {
      throw Failure(code::kBadDescriptor, "output " + std::to_string(i) + " is not empty");
    }
  }
  const Inspection in = inspectFlac(kAudioFD);
  for (const std::uint32_t p : req.pictures) {
    if (p >= in.pictures.size()) {
      throw Failure(code::kPictureNotFound,
                    "picture " + std::to_string(p) + " does not exist (the file has " +
                        std::to_string(in.pictures.size()) + ")");
    }
  }
  json::Writer w;
  w.beginObject();
  w.key("images");
  w.beginArray();
  for (std::size_t i = 0; i < req.pictures.size(); ++i) {
    const Picture &pic = in.pictures[req.pictures[i]];
    const int fd = kFirstExtraFD + static_cast<int>(i);
    writeAt(fd, 0, pic.data, "output " + std::to_string(i));
    w.beginObject();
    w.key("index");
    w.uinteger(req.pictures[i]);
    w.key("size");
    w.uinteger(pic.data.size());
    w.key("sha256");
    w.string(sha256Hex(pic.data));
    w.endObject();
  }
  w.endArray();
  w.endObject();
  return w.take();
}

std::string writeManagedTags(std::string_view request) {
  const WriteRequest req = parseWriteRequest(request);
  requireFLAC(req.format, "write-managed-tags");
  checkDescriptor(kAudioFD, Access::ReadWrite, "the audio file");
  std::string cover;
  if (req.cover) {
    const std::uint64_t size = checkDescriptor(kFirstExtraFD, Access::Read, "the cover");
    // No format embeds a picture of 16 MiB or more in this helper (a FLAC
    // metadata block is at most 16 MiB - 1): refuse before reading it.
    if (size >= (1U << 24)) throw Failure(code::kTooLarge, "the cover is larger than 16 MiB");
    cover = readAt(kFirstExtraFD, 0, static_cast<std::size_t>(size), "the cover");
  }
  writeFlac(kAudioFD, req, cover);
  return "{}";
}

std::string run(std::string_view op) {
  if (op == "version") return version();
  if (op != "inspect" && op != "extract-images" && op != "write-managed-tags") {
    throw Failure(code::kInvalidRequest, "unknown operation \"" + std::string(op) + "\"");
  }
  const std::string request = readRequest();
  if (op == "inspect") return inspect(request);
  if (op == "extract-images") return extractImages(request);
  return writeManagedTags(request);
}

// kMaxMessageBytes bounds the message of a failure, so that the JSON always
// fits in the 64 KiB of standard error the Go Runner keeps (DESIGN.md §8.5):
// a message can quote a request string of up to kMaxRequestBytes.
constexpr std::size_t kMaxMessageBytes = 4096;

// shortened cuts a valid UTF-8 message to at most kMaxMessageBytes plus an
// ellipsis, on a character boundary.
std::string shortened(const std::string &message) {
  if (message.size() <= kMaxMessageBytes) return message;
  std::size_t cut = kMaxMessageBytes;
  while (cut > 0 && (static_cast<unsigned char>(message[cut]) & 0xC0) == 0x80) --cut;
  return message.substr(0, cut) + "...";
}

// report prints a failure on stderr; the message falls back to a fixed text
// if it cannot be printed as JSON.
void report(const char *code, const std::string &message) {
  std::string text;
  try {
    json::Writer w;
    w.beginObject();
    w.key("code");
    w.string(code);
    w.key("message");
    w.string(shortened(message));
    w.endObject();
    text = w.take();
  } catch (...) {
    text = std::string("{\"code\":\"") + code + "\",\"message\":\"(unprintable message)\"}";
  }
  try {
    writeOut(STDERR_FILENO, text);
  } catch (...) {
    // Nothing else can be reported; the exit status still says failure.
  }
}

}  // namespace
}  // namespace mltags

int main(int argc, char **argv) {
  using namespace mltags;
  try {
    if (argc != 2) throw Failure(code::kInvalidRequest, "usage: musiclib-tags <operation>");
    writeOut(STDOUT_FILENO, run(argv[1]));
    return 0;
  } catch (const Failure &f) {
    report(f.code(), f.what());
  } catch (const std::bad_alloc &) {
    report(code::kTooLarge, "out of memory");
  } catch (const std::exception &e) {
    report(code::kInternal, std::string("unexpected error: ") + e.what());
  } catch (...) {
    report(code::kInternal, "unexpected error");
  }
  return kExitFailure;
}
