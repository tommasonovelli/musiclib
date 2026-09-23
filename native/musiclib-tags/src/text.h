// Byte-level text helpers: strict UTF-8 validation, base64 and hex.
#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

namespace mltags {

// validUTF8 reports whether s is well-formed UTF-8 (Unicode §3.9, table 3-7):
// no overlong forms, no surrogates (U+D800..U+DFFF), nothing above U+10FFFF,
// no truncated sequence. These are exactly the inputs utfcpp, and therefore
// TagLib's String, refuses; TagLib replaces such a value with an empty string.
bool validUTF8(std::string_view s) noexcept;

// appendUTF8 appends the UTF-8 encoding of the scalar value cp, which must be
// at most U+10FFFF and not a surrogate.
void appendUTF8(std::string &out, std::uint32_t cp);

// decodeBase64 decodes standard base64 (RFC 4648 §4) with mandatory padding
// and nothing else in the input (no whitespace, no line breaks). It returns
// nullopt for anything else, including non-zero bits in the final quantum.
std::optional<std::string> decodeBase64(std::string_view s);

// hexLower returns the lowercase hexadecimal form of the bytes.
std::string hexLower(std::string_view bytes);

// isPrintableASCII reports whether s is non-empty and made only of the bytes
// 0x20..0x7E.
bool isPrintableASCII(std::string_view s) noexcept;

}  // namespace mltags
