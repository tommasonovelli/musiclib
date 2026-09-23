#include "sha256.h"

#include <algorithm>
#include <cstring>

#include "text.h"

namespace mltags {

namespace {

constexpr std::array<std::uint32_t, 64> kK = {
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
};

constexpr std::uint32_t rotr(std::uint32_t x, int n) noexcept { return (x >> n) | (x << (32 - n)); }

}  // namespace

SHA256::SHA256() noexcept
    : h_{0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19} {}

void SHA256::block(const unsigned char *p) noexcept {
  std::array<std::uint32_t, 64> w{};
  for (std::size_t i = 0; i < 16; ++i) {
    w[i] = (static_cast<std::uint32_t>(p[4 * i]) << 24) | (static_cast<std::uint32_t>(p[4 * i + 1]) << 16) |
           (static_cast<std::uint32_t>(p[4 * i + 2]) << 8) | static_cast<std::uint32_t>(p[4 * i + 3]);
  }
  for (std::size_t i = 16; i < 64; ++i) {
    const std::uint32_t s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >> 3);
    const std::uint32_t s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >> 10);
    w[i] = w[i - 16] + s0 + w[i - 7] + s1;
  }
  std::uint32_t a = h_[0], b = h_[1], c = h_[2], d = h_[3], e = h_[4], f = h_[5], g = h_[6], h = h_[7];
  for (std::size_t i = 0; i < 64; ++i) {
    const std::uint32_t s1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
    const std::uint32_t ch = (e & f) ^ (~e & g);
    const std::uint32_t t1 = h + s1 + ch + kK[i] + w[i];
    const std::uint32_t s0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
    const std::uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
    const std::uint32_t t2 = s0 + maj;
    h = g;
    g = f;
    f = e;
    e = d + t1;
    d = c;
    c = b;
    b = a;
    a = t1 + t2;
  }
  h_[0] += a;
  h_[1] += b;
  h_[2] += c;
  h_[3] += d;
  h_[4] += e;
  h_[5] += f;
  h_[6] += g;
  h_[7] += h;
}

void SHA256::update(std::string_view data) noexcept {
  const auto *p = reinterpret_cast<const unsigned char *>(data.data());
  std::size_t n = data.size();
  bytes_ += n;
  if (used_ > 0) {
    const std::size_t take = std::min(n, buf_.size() - used_);
    std::memcpy(buf_.data() + used_, p, take);
    used_ += take;
    p += take;
    n -= take;
    if (used_ < buf_.size()) return;
    block(buf_.data());
    used_ = 0;
  }
  while (n >= 64) {
    block(p);
    p += 64;
    n -= 64;
  }
  if (n > 0) {
    std::memcpy(buf_.data(), p, n);
    used_ = n;
  }
}

std::string SHA256::hex() {
  const std::uint64_t bits = bytes_ * 8;
  const unsigned char pad = 0x80;
  update(std::string_view(reinterpret_cast<const char *>(&pad), 1));
  static constexpr unsigned char kZeros[64] = {};
  const std::size_t zeros = used_ <= 56 ? 56 - used_ : 64 + 56 - used_;
  update(std::string_view(reinterpret_cast<const char *>(kZeros), zeros));
  unsigned char len[8];
  for (int i = 0; i < 8; ++i) len[i] = static_cast<unsigned char>(bits >> (56 - 8 * i));
  update(std::string_view(reinterpret_cast<const char *>(len), 8));
  std::string digest;
  for (const std::uint32_t v : h_) {
    digest.push_back(static_cast<char>(v >> 24));
    digest.push_back(static_cast<char>(v >> 16));
    digest.push_back(static_cast<char>(v >> 8));
    digest.push_back(static_cast<char>(v));
  }
  return hexLower(digest);
}

std::string sha256Hex(std::string_view data) {
  SHA256 h;
  h.update(data);
  return h.hex();
}

}  // namespace mltags
