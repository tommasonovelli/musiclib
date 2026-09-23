// Unit tests of the helper's own parsers: UTF-8 validation, base64, the
// JSON reader and writer, SHA-256. They run at image build time
// (`make check`, under ASan/UBSan); the helper's contract on real files is
// tested from Go, in internal/media.
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <string>
#include <string_view>
#include <vector>

#include "../src/failure.h"
#include "../src/json.h"
#include "../src/sha256.h"
#include "../src/text.h"

using namespace mltags;

namespace {

int failures = 0;

void fail(const std::string &what) {
  std::fprintf(stderr, "FAIL: %s\n", what.c_str());
  ++failures;
}

void expect(bool ok, const std::string &what) {
  if (!ok) fail(what);
}

// refUTF8 is a reference decoder written from the definition: a lead byte
// with n leading ones, n-1 continuation bytes, the shortest form, a scalar
// value (not a surrogate, at most U+10FFFF).
bool refUTF8(const std::vector<unsigned char> &b) {
  std::size_t i = 0;
  while (i < b.size()) {
    const unsigned char c = b[i];
    std::size_t n;
    std::uint32_t cp;
    if (c < 0x80) {
      ++i;
      continue;
    } else if ((c & 0xE0) == 0xC0) {
      n = 2, cp = c & 0x1Fu;
    } else if ((c & 0xF0) == 0xE0) {
      n = 3, cp = c & 0x0Fu;
    } else if ((c & 0xF8) == 0xF0) {
      n = 4, cp = c & 0x07u;
    } else {
      return false;
    }
    if (b.size() - i < n) return false;
    for (std::size_t k = 1; k < n; ++k) {
      if ((b[i + k] & 0xC0) != 0x80) return false;
      cp = (cp << 6) | (b[i + k] & 0x3Fu);
    }
    const std::uint32_t min = n == 2 ? 0x80 : n == 3 ? 0x800 : 0x10000;
    if (cp < min || cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF)) return false;
    i += n;
  }
  return true;
}

bool valid(const std::vector<unsigned char> &b) {
  return validUTF8(std::string_view(reinterpret_cast<const char *>(b.data()), b.size()));
}

void testUTF8() {
  // Every scalar value encodes to valid UTF-8, and appendUTF8 agrees with
  // the reference.
  for (std::uint32_t cp = 0; cp <= 0x10FFFF; ++cp) {
    if (cp >= 0xD800 && cp <= 0xDFFF) continue;
    std::string s;
    appendUTF8(s, cp);
    if (!validUTF8(s)) {
      fail("appendUTF8(" + std::to_string(cp) + ") is not valid");
      return;
    }
  }
  // Every 1-, 2- and 3-byte sequence, exhaustively.
  for (unsigned a = 0; a < 256; ++a) {
    if (valid({static_cast<unsigned char>(a)}) != refUTF8({static_cast<unsigned char>(a)})) fail("1-byte " + std::to_string(a));
    for (unsigned b = 0; b < 256; ++b) {
      const std::vector<unsigned char> two{static_cast<unsigned char>(a), static_cast<unsigned char>(b)};
      if (valid(two) != refUTF8(two)) fail("2-byte " + std::to_string(a) + "," + std::to_string(b));
      if (a < 0xE0 || a > 0xEF) continue;
      for (unsigned c = 0; c < 256; ++c) {
        const std::vector<unsigned char> three{static_cast<unsigned char>(a), static_cast<unsigned char>(b),
                                               static_cast<unsigned char>(c)};
        if (valid(three) != refUTF8(three)) {
          fail("3-byte " + std::to_string(a) + "," + std::to_string(b) + "," + std::to_string(c));
          return;
        }
      }
    }
  }
  // 4-byte sequences: every lead and second byte, the boundary values of the
  // last two.
  const unsigned edges[] = {0x00, 0x7F, 0x80, 0x8F, 0x90, 0xBF, 0xC0, 0xFF};
  for (unsigned a = 0xF0; a < 0x100; ++a) {
    for (unsigned b = 0; b < 256; ++b) {
      for (const unsigned c : edges) {
        for (const unsigned d : edges) {
          const std::vector<unsigned char> four{static_cast<unsigned char>(a), static_cast<unsigned char>(b),
                                                static_cast<unsigned char>(c), static_cast<unsigned char>(d)};
          if (valid(four) != refUTF8(four)) fail("4-byte sequence");
        }
      }
    }
  }
  expect(validUTF8(""), "empty string");
  expect(validUTF8(std::string("a\0b", 3)), "NUL is valid UTF-8 (the callers refuse it separately)");
  expect(!validUTF8("\xC3"), "truncated sequence");
  expect(!validUTF8("\xED\xA0\x80"), "surrogate U+D800");
  expect(!validUTF8("\xF4\x90\x80\x80"), "U+110000");
  expect(!validUTF8("\xC0\xAF"), "overlong /");
  expect(validUTF8("Cafe\xCC\x81"), "NFD e + combining acute");
}

void testBase64() {
  const struct {
    const char *in;
    const char *out;
  } ok[] = {
      {"", ""}, {"Zg==", "f"}, {"Zm8=", "fo"}, {"Zm9v", "foo"}, {"Zm9vYg==", "foob"}, {"Zm9vYmE=", "fooba"},
      {"Zm9vYmFy", "foobar"}, {"+/+/", "\xfb\xff\xbf"},
  };
  for (const auto &c : ok) {
    const auto got = decodeBase64(c.in);
    expect(got && *got == c.out, std::string("base64 ") + c.in);
  }
  for (const char *bad : {"Zg", "Zg=", "Zg===", "Z===", "Zm9v\n", " Zm9v", "Zh==", "Zm9=", "Zg==Zg==", "Zm9v*A==", "=Zg="}) {
    expect(!decodeBase64(bad), std::string("base64 must refuse ") + bad);
  }
}

bool parses(std::string_view s) {
  try {
    json::parse(s);
    return true;
  } catch (const Failure &f) {
    if (std::string(f.code()) != code::kInvalidRequest) fail("wrong failure code for " + std::string(s));
    return false;
  }
}

void testJSON() {
  for (const char *ok : {"{}", " {\"a\" : [1, -2, 0, true, false, null, \"x\"]} ", "[]", "\"\"", "0", "-0",
                         "9223372036854775807", "-9223372036854775808", "{\"a\":{\"b\":{}}}",
                         "\"\\u00e9\\ud83d\\ude00\\n\\/\"", "\"caf\xC3\xA9\""}) {
    expect(parses(ok), std::string("must parse: ") + ok);
  }
  for (const char *bad : {"", " ", "{", "}", "{\"a\":1,}", "[1,]", "{\"a\":1 \"b\":2}", "{\"a\":1,\"a\":2}", "01",
                          "1.5", "1e3", "-", "+1", "9223372036854775808", "-9223372036854775809", "tru", "nul",
                          "\"\\ud800\"", "\"\\udc00\"", "\"\\ud800\\u0041\"", "\"\\x\"", "\"a\tb\"", "\"abc",
                          "{} {}", "{\"a\":1}x", "'a'", "\"\xC3\"", "[[[[[[[[[]]]]]]]]]", "{1:2}", "NaN"}) {
    expect(!parses(bad), std::string("must refuse: ") + bad);
  }
  // Depth: kMaxDepth levels are fine, one more is not.
  std::string deep(json::kMaxDepth, '[');
  deep += std::string(json::kMaxDepth, ']');
  expect(parses(deep), "kMaxDepth levels");
  expect(!parses("[" + deep + "]"), "kMaxDepth+1 levels");

  const json::Value v = json::parse("{\"s\":\"a\\u0000b\\\"\",\"n\":-7,\"l\":[1,2]}");
  expect(v.find("s")->string == std::string("a\0b\"", 4), "escapes decode, NUL included");
  expect(v.find("n")->integer == -7, "negative integer");
  expect(v.find("l")->items.size() == 2, "array");
  expect(v.find("zz") == nullptr, "missing key");

  json::Writer w;
  w.beginObject();
  w.key("a");
  w.string(std::string("q\"\\\n\x01\x7f\xC3\xA9", 8));
  w.key("b");
  w.beginArray();
  w.integer(-1);
  w.uinteger(18446744073709551615ULL);
  w.boolean(true);
  w.null();
  w.endArray();
  w.endObject();
  expect(w.take() == "{\"a\":\"q\\\"\\\\\\n\\u0001\x7f\xC3\xA9\",\"b\":[-1,18446744073709551615,true,null]}",
         "writer output");
  json::Writer bad;
  try {
    bad.string("\xFF");
    fail("the writer printed invalid UTF-8");
  } catch (const Failure &f) {
    expect(std::string(f.code()) == code::kInternal, "invalid UTF-8 is an internal failure");
  }
}

void testSHA256() {
  expect(sha256Hex("") == "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "sha256 empty");
  expect(sha256Hex("abc") == "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "sha256 abc");
  expect(sha256Hex("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq") ==
             "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1",
         "sha256 448 bits");
  const std::string million(1000000, 'a');
  expect(sha256Hex(million) == "cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0", "sha256 1M a");
  // Any split of the input gives the same hash.
  std::string data;
  for (int i = 0; i < 300; ++i) data.push_back(static_cast<char>(i * 7));
  const std::string whole = sha256Hex(data);
  for (std::size_t cut = 0; cut <= data.size(); ++cut) {
    SHA256 h;
    h.update(std::string_view(data).substr(0, cut));
    h.update(std::string_view(data).substr(cut));
    if (h.hex() != whole) {
      fail("sha256 split at " + std::to_string(cut));
      return;
    }
  }
}

}  // namespace

int main() {
  testUTF8();
  testBase64();
  testJSON();
  testSHA256();
  if (failures > 0) {
    std::fprintf(stderr, "%d failure(s)\n", failures);
    return 1;
  }
  std::printf("unit tests passed\n");
  return 0;
}
