#include "fdio.h"

#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <climits>
#include <cstring>
#include <vector>

#include "failure.h"

namespace mltags {

namespace {

// kMoveChunk is the buffer of insert/removeBlock.
constexpr std::size_t kMoveChunk = 1 << 20;

std::string errnoText(int err) { return std::string(std::strerror(err)); }

}  // namespace

std::uint64_t checkDescriptor(int fd, Access access, const std::string &what) {
  struct stat st {};
  if (fstat(fd, &st) != 0) {
    throw Failure(code::kBadDescriptor, what + " (descriptor " + std::to_string(fd) + "): " + errnoText(errno));
  }
  if (!S_ISREG(st.st_mode)) {
    throw Failure(code::kBadDescriptor, what + " (descriptor " + std::to_string(fd) + ") is not a regular file");
  }
  const int flags = fcntl(fd, F_GETFL);
  if (flags < 0) {
    throw Failure(code::kBadDescriptor, what + " (descriptor " + std::to_string(fd) + "): " + errnoText(errno));
  }
  const int mode = flags & O_ACCMODE;
  const bool canRead = mode == O_RDONLY || mode == O_RDWR;
  const bool canWrite = mode == O_WRONLY || mode == O_RDWR;
  const bool ok = (access == Access::Read && canRead) || (access == Access::Write && canWrite) ||
                  (access == Access::ReadWrite && mode == O_RDWR);
  if (!ok) {
    throw Failure(code::kBadDescriptor, what + " (descriptor " + std::to_string(fd) + ") has the wrong access mode");
  }
  if (access != Access::Read && (flags & O_APPEND) != 0) {
    throw Failure(code::kBadDescriptor, what + " (descriptor " + std::to_string(fd) + ") is in append mode");
  }
  return static_cast<std::uint64_t>(st.st_size);
}

std::string readAt(int fd, std::uint64_t off, std::size_t n, const std::string &what) {
  std::string buf(n, '\0');
  std::size_t done = 0;
  while (done < n) {
    const ssize_t r = pread(fd, buf.data() + done, n - done, static_cast<off_t>(off + done));
    if (r < 0) {
      if (errno == EINTR) continue;
      throw Failure(code::kIO, "reading " + what + ": " + errnoText(errno));
    }
    if (r == 0) throw Failure(code::kIO, "reading " + what + ": unexpected end of file");
    done += static_cast<std::size_t>(r);
  }
  return buf;
}

void writeAt(int fd, std::uint64_t off, std::string_view data, const std::string &what) {
  std::size_t done = 0;
  while (done < data.size()) {
    const ssize_t w = pwrite(fd, data.data() + done, data.size() - done, static_cast<off_t>(off + done));
    if (w < 0) {
      if (errno == EINTR) continue;
      throw Failure(code::kIO, "writing " + what + ": " + errnoText(errno));
    }
    done += static_cast<std::size_t>(w);
  }
}

// ---------------------------------------------------------------------------

void FdStream::fail(const std::string &op, int err) {
  if (error_.empty()) error_ = op + ": " + errnoText(err);
}

void FdStream::check() const {
  if (!error_.empty()) throw Failure(code::kIO, error_);
}

bool FdStream::preadFull(char *p, std::size_t n, std::uint64_t off) {
  std::size_t done = 0;
  while (done < n) {
    const ssize_t r = pread(fd_, p + done, n - done, static_cast<off_t>(off + done));
    if (r < 0) {
      if (errno == EINTR) continue;
      fail("read", errno);
      return false;
    }
    if (r == 0) {
      fail("read", EIO);  // the file shrank underneath
      return false;
    }
    done += static_cast<std::size_t>(r);
  }
  return true;
}

bool FdStream::pwriteFull(const char *p, std::size_t n, std::uint64_t off) {
  if (!writable_) {
    fail("write", EBADF);
    return false;
  }
  std::size_t done = 0;
  while (done < n) {
    const ssize_t w = pwrite(fd_, p + done, n - done, static_cast<off_t>(off + done));
    if (w < 0) {
      if (errno == EINTR) continue;
      fail("write", errno);
      return false;
    }
    done += static_cast<std::size_t>(w);
  }
  return true;
}

TagLib::offset_t FdStream::length() {
  if (!error_.empty()) return 0;
  struct stat st {};
  if (fstat(fd_, &st) != 0) {
    fail("stat", errno);
    return 0;
  }
  return st.st_size;
}

TagLib::ByteVector FdStream::readBlock(size_t length) {
  if (!error_.empty() || length == 0) return {};
  const TagLib::offset_t size = this->length();
  if (!error_.empty() || pos_ >= size) return {};
  // Never allocate more than the file holds, whatever a header declares;
  // TagLib's ByteVector sizes are unsigned int.
  std::size_t n = std::min<std::uint64_t>(length, static_cast<std::uint64_t>(size - pos_));
  n = std::min<std::size_t>(n, UINT_MAX);
  TagLib::ByteVector buf(static_cast<unsigned int>(n), 0);
  if (!preadFull(buf.data(), n, static_cast<std::uint64_t>(pos_))) return {};
  pos_ += static_cast<TagLib::offset_t>(n);
  return buf;
}

void FdStream::writeBlock(const TagLib::ByteVector &data) {
  if (!error_.empty()) return;
  if (pwriteFull(data.data(), data.size(), static_cast<std::uint64_t>(pos_))) {
    pos_ += static_cast<TagLib::offset_t>(data.size());
  }
}

void FdStream::seek(TagLib::offset_t offset, Position p) {
  if (!error_.empty()) return;
  TagLib::offset_t base = 0;
  switch (p) {
    case Beginning:
      base = 0;
      break;
    case Current:
      base = pos_;
      break;
    case End:
      base = length();
      break;
  }
  const TagLib::offset_t target = base + offset;
  // Like fseek, a seek before the start fails and leaves the position
  // alone. TagLib relies on this (Utils::findID3v1 on a short file) and
  // checks what it reads afterwards.
  if (target >= 0) pos_ = target;
}

void FdStream::truncate(TagLib::offset_t length) {
  if (!error_.empty()) return;
  if (!writable_) {
    fail("truncate", EBADF);
    return;
  }
  while (ftruncate(fd_, length) != 0) {
    if (errno == EINTR) continue;
    fail("truncate", errno);
    return;
  }
}

bool FdStream::moveRange(std::uint64_t src, std::uint64_t dst, std::uint64_t n) {
  std::vector<char> buf(static_cast<std::size_t>(std::min<std::uint64_t>(n, kMoveChunk)));
  if (dst > src) {
    // Back to front, so that nothing is overwritten before it is read.
    std::uint64_t left = n;
    while (left > 0) {
      const std::uint64_t k = std::min<std::uint64_t>(left, buf.size());
      left -= k;
      if (!preadFull(buf.data(), static_cast<std::size_t>(k), src + left)) return false;
      if (!pwriteFull(buf.data(), static_cast<std::size_t>(k), dst + left)) return false;
    }
  } else {
    std::uint64_t done = 0;
    while (done < n) {
      const std::uint64_t k = std::min<std::uint64_t>(n - done, buf.size());
      if (!preadFull(buf.data(), static_cast<std::size_t>(k), src + done)) return false;
      if (!pwriteFull(buf.data(), static_cast<std::size_t>(k), dst + done)) return false;
      done += k;
    }
  }
  return true;
}

void FdStream::insert(const TagLib::ByteVector &data, TagLib::offset_t start, size_t replace) {
  if (!error_.empty()) return;
  if (start < 0) {
    fail("insert", EINVAL);
    return;
  }
  const auto s = static_cast<std::uint64_t>(start);
  const std::uint64_t size = data.size();
  if (size < replace) {
    if (!pwriteFull(data.data(), data.size(), s)) return;
    removeBlock(static_cast<TagLib::offset_t>(s + size), replace - size);
    pos_ = static_cast<TagLib::offset_t>(s + size);
    return;
  }
  if (size > replace) {
    const TagLib::offset_t len = length();
    if (!error_.empty()) return;
    const std::uint64_t tail = s + replace;
    if (tail < static_cast<std::uint64_t>(len)) {
      if (!moveRange(tail, s + size, static_cast<std::uint64_t>(len) - tail)) return;
    }
  }
  if (pwriteFull(data.data(), data.size(), s)) pos_ = static_cast<TagLib::offset_t>(s + size);
}

void FdStream::removeBlock(TagLib::offset_t start, size_t length) {
  if (!error_.empty()) return;
  if (start < 0) {
    fail("remove", EINVAL);
    return;
  }
  const TagLib::offset_t len = this->length();
  if (!error_.empty()) return;
  const auto size = static_cast<std::uint64_t>(len);
  const auto s = static_cast<std::uint64_t>(start);
  if (s >= size || length == 0) return;
  const std::uint64_t end = std::min<std::uint64_t>(s + length, size);
  if (!moveRange(end, s, size - end)) return;
  truncate(static_cast<TagLib::offset_t>(size - (end - s)));
  pos_ = start;
}

}  // namespace mltags
