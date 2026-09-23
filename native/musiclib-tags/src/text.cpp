#include "text.h"

#include <array>

namespace mltags {

bool validUTF8(std::string_view s) noexcept {
  const auto *p = reinterpret_cast<const unsigned char *>(s.data());
  const std::size_t n = s.size();
  std::size_t i = 0;
  while (i < n) {
    const unsigned char c = p[i];
    if (c < 0x80) {
      ++i;
      continue;
    }
    // Lead byte: length and the allowed range of the first continuation byte
    // (which is where overlong forms, surrogates and values above U+10FFFF
    // are excluded).
    std::size_t len = 0;
    unsigned char lo = 0x80, hi = 0xBF;
    if (c >= 0xC2 && c <= 0xDF) {
      len = 2;
    } else if (c == 0xE0) {
      len = 3, lo = 0xA0;
    } else if ((c >= 0xE1 && c <= 0xEC) || c == 0xEE || c == 0xEF) {
      len = 3;
    } else if (c == 0xED) {
      len = 3, hi = 0x9F;
    } else if (c == 0xF0) {
      len = 4, lo = 0x90;
    } else if (c >= 0xF1 && c <= 0xF3) {
      len = 4;
    } else if (c == 0xF4) {
      len = 4, hi = 0x8F;
    } else {
      return false;
    }
    if (n - i < len) return false;
    if (p[i + 1] < lo || p[i + 1] > hi) return false;
    for (std::size_t k = 2; k < len; ++k) {
      if (p[i + k] < 0x80 || p[i + k] > 0xBF) return false;
    }
    i += len;
  }
  return true;
}

void appendUTF8(std::string &out, std::uint32_t cp) {
  if (cp < 0x80) {
    out.push_back(static_cast<char>(cp));
  } else if (cp < 0x800) {
    out.push_back(static_cast<char>(0xC0 | (cp >> 6)));
    out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
  } else if (cp < 0x10000) {
    out.push_back(static_cast<char>(0xE0 | (cp >> 12)));
    out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
    out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
  } else {
    out.push_back(static_cast<char>(0xF0 | (cp >> 18)));
    out.push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3F)));
    out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
    out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
  }
}

namespace {

// base64Value returns the 6-bit value of a base64 character, or -1.
int base64Value(char c) noexcept {
  if (c >= 'A' && c <= 'Z') return c - 'A';
  if (c >= 'a' && c <= 'z') return c - 'a' + 26;
  if (c >= '0' && c <= '9') return c - '0' + 52;
  if (c == '+') return 62;
  if (c == '/') return 63;
  return -1;
}

}  // namespace

std::optional<std::string> decodeBase64(std::string_view s) {
  if (s.size() % 4 != 0) return std::nullopt;
  std::string out;
  out.reserve(s.size() / 4 * 3);
  for (std::size_t i = 0; i < s.size(); i += 4) {
    const bool last = i + 4 == s.size();
    std::array<int, 4> v{};
    int pad = 0;
    for (std::size_t k = 0; k < 4; ++k) {
      const char c = s[i + k];
      if (c == '=' && last && k >= 2) {
        v[k] = 0;
        ++pad;
        continue;
      }
      if (pad > 0) return std::nullopt;  // data after padding
      v[k] = base64Value(c);
      if (v[k] < 0) return std::nullopt;
    }
    const std::uint32_t q = (static_cast<std::uint32_t>(v[0]) << 18) | (static_cast<std::uint32_t>(v[1]) << 12) |
                            (static_cast<std::uint32_t>(v[2]) << 6) | static_cast<std::uint32_t>(v[3]);
    out.push_back(static_cast<char>((q >> 16) & 0xFF));
    if (pad == 2) {
      if ((q & 0xFFFF) != 0) return std::nullopt;  // non-canonical trailing bits
      break;
    }
    out.push_back(static_cast<char>((q >> 8) & 0xFF));
    if (pad == 1) {
      if ((q & 0xFF) != 0) return std::nullopt;
      break;
    }
    out.push_back(static_cast<char>(q & 0xFF));
  }
  return out;
}

std::string hexLower(std::string_view bytes) {
  static constexpr char kDigits[] = "0123456789abcdef";
  std::string out;
  out.reserve(bytes.size() * 2);
  for (const char ch : bytes) {
    const auto b = static_cast<unsigned char>(ch);
    out.push_back(kDigits[b >> 4]);
    out.push_back(kDigits[b & 0x0F]);
  }
  return out;
}

bool isPrintableASCII(std::string_view s) noexcept {
  if (s.empty()) return false;
  for (const char ch : s) {
    const auto b = static_cast<unsigned char>(ch);
    if (b < 0x20 || b > 0x7E) return false;
  }
  return true;
}

}  // namespace mltags
