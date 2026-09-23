// SHA-256 (FIPS 180-4), for the identity of embedded pictures and metadata
// blocks in an inspection. The tests compare it with Go's crypto/sha256.
#pragma once

#include <array>
#include <cstdint>
#include <string>
#include <string_view>

namespace mltags {

class SHA256 {
 public:
  SHA256() noexcept;
  void update(std::string_view data) noexcept;
  // hex finishes the hash and returns it as 64 lowercase hex digits. The
  // object must not be used afterwards.
  std::string hex();

 private:
  void block(const unsigned char *p) noexcept;

  std::array<std::uint32_t, 8> h_;
  std::array<unsigned char, 64> buf_{};
  std::size_t used_ = 0;
  std::uint64_t bytes_ = 0;
};

// sha256Hex is the SHA-256 of data, as 64 lowercase hex digits.
std::string sha256Hex(std::string_view data);

}  // namespace mltags
