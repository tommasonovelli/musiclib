// Descriptors: the only way files reach the helper (NOTES.md N-075: names
// and paths never do). The Go Runner passes them as 3, 4, ...
#pragma once

#include <sys/types.h>

#include <cstdint>
#include <string>
#include <string_view>

#include <taglib/tiostream.h>

namespace mltags {

// Access a descriptor must allow.
enum class Access { Read, Write, ReadWrite };

// checkDescriptor verifies that fd is open, is a regular file (never a
// directory, FIFO, socket or device: nothing can block or be traversed) and
// allows the access; a descriptor that must be written must not be in
// O_APPEND mode, where Linux ignores the offset of pwrite. It returns the
// file size. Failure(bad_descriptor) otherwise; what names the descriptor in
// the message.
std::uint64_t checkDescriptor(int fd, Access access, const std::string &what);

// readAt reads exactly n bytes at offset off; Failure(io) on an error or a
// short read (the file changed underneath).
std::string readAt(int fd, std::uint64_t off, std::size_t n, const std::string &what);

// writeAt writes all of data at offset off (pwrite: the descriptor's own
// offset is not used); Failure(io) on error.
void writeAt(int fd, std::uint64_t off, std::string_view data, const std::string &what);

// FdStream is TagLib's IOStream over a descriptor, with positioned reads and
// writes (pread/pwrite: the descriptor's own offset is never used) and
// explicit errors.
//
// TagLib's own FileStream ignores write errors (fwrite, ftruncate). Here the
// first failure is kept, every later operation becomes a no-op, isOpen()
// turns false, and check() throws it: a write that fails is never reported
// as success. On such a failure the file may be partially rewritten; the
// helper only ever writes a staging copy that the caller discards on any
// error (DESIGN.md §9.1).
class FdStream final : public TagLib::IOStream {
 public:
  FdStream(int fd, bool writable) noexcept : fd_(fd), writable_(writable) {}

  TagLib::FileName name() const override { return "descriptor"; }
  TagLib::ByteVector readBlock(size_t length) override;
  void writeBlock(const TagLib::ByteVector &data) override;
  void insert(const TagLib::ByteVector &data, TagLib::offset_t start, size_t replace) override;
  void removeBlock(TagLib::offset_t start, size_t length) override;
  bool readOnly() const override { return !writable_; }
  bool isOpen() const override { return error_.empty(); }
  void seek(TagLib::offset_t offset, Position p) override;
  void clear() override {}
  TagLib::offset_t tell() const override { return pos_; }
  TagLib::offset_t length() override;
  void truncate(TagLib::offset_t length) override;

  // check throws Failure(io) if any operation failed.
  void check() const;

 private:
  void fail(const std::string &op, int err);
  bool preadFull(char *p, std::size_t n, std::uint64_t off);
  bool pwriteFull(const char *p, std::size_t n, std::uint64_t off);
  // moveRange copies n bytes from src to dst (possibly overlapping), in
  // chunks, back to front when dst > src.
  bool moveRange(std::uint64_t src, std::uint64_t dst, std::uint64_t n);

  int fd_;
  bool writable_;
  TagLib::offset_t pos_ = 0;
  std::string error_;
};

}  // namespace mltags
