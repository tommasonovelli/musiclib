#include "request.h"

#include <algorithm>
#include <initializer_list>
#include <limits>

#include "failure.h"
#include "json.h"
#include "text.h"

namespace mltags {

std::string_view formatName(Format f) noexcept {
  switch (f) {
    case Format::FLAC:
      return "flac";
    case Format::MP3:
      return "mp3";
    case Format::M4A_AAC:
      return "m4a-aac";
    case Format::M4A_ALAC:
      return "m4a-alac";
  }
  return "?";
}

namespace {

[[noreturn]] void invalid(const std::string &msg) { throw Failure(code::kInvalidRequest, msg); }

// requireKeys checks that v is an object with exactly the given keys.
void requireKeys(const json::Value &v, const std::string &what, std::initializer_list<std::string_view> keys) {
  if (v.kind != json::Value::Kind::Object) invalid(what + " must be an object");
  for (const auto &k : v.keys) {
    if (std::find(keys.begin(), keys.end(), k) == keys.end()) invalid(what + ": unknown key \"" + k + "\"");
  }
  for (const auto k : keys) {
    if (v.find(k) == nullptr) invalid(what + ": missing key \"" + std::string(k) + "\"");
  }
}

Format parseFormat(const json::Value &v) {
  if (v.kind != json::Value::Kind::String) invalid("format must be a string");
  for (const Format f : {Format::FLAC, Format::MP3, Format::M4A_AAC, Format::M4A_ALAC}) {
    if (v.string == formatName(f)) return f;
  }
  // Any well-formed name is a format this helper does not know.
  throw Failure(code::kUnsupportedFormat, "unsupported format \"" + v.string + "\"");
}

std::uint32_t parseUint(const json::Value &v, const std::string &what, std::int64_t min, std::int64_t max) {
  if (v.kind != json::Value::Kind::Int || v.integer < min || v.integer > max) {
    invalid(what + " must be an integer in " + std::to_string(min) + ".." + std::to_string(max));
  }
  return static_cast<std::uint32_t>(v.integer);
}

// A text value to write: absent (null) or a non-empty string without NUL
// bytes (TagLib's String stops at the first NUL).
std::optional<std::string> parseText(const json::Value &v, const std::string &what) {
  if (v.kind == json::Value::Kind::Null) return std::nullopt;
  if (v.kind != json::Value::Kind::String) invalid(what + " must be a string or null");
  if (v.string.empty()) invalid(what + " must not be empty (null removes the field)");
  if (v.string.find('\0') != std::string::npos) invalid(what + " must not contain NUL");
  return v.string;
}

}  // namespace

InspectRequest parseInspectRequest(std::string_view text) {
  const json::Value v = json::parse(text);
  requireKeys(v, "the request", {"format"});
  return InspectRequest{parseFormat(*v.find("format"))};
}

ExtractRequest parseExtractRequest(std::string_view text) {
  const json::Value v = json::parse(text);
  requireKeys(v, "the request", {"format", "pictures"});
  ExtractRequest r{parseFormat(*v.find("format")), {}};
  const json::Value &pics = *v.find("pictures");
  if (pics.kind != json::Value::Kind::Array) invalid("pictures must be an array");
  if (pics.items.empty() || pics.items.size() > kMaxPictures) {
    invalid("pictures must name 1.." + std::to_string(kMaxPictures) + " pictures");
  }
  for (const auto &item : pics.items) {
    const std::uint32_t i = parseUint(item, "a picture index", 0, std::numeric_limits<std::uint32_t>::max());
    if (std::find(r.pictures.begin(), r.pictures.end(), i) != r.pictures.end()) {
      invalid("picture " + std::to_string(i) + " is named twice");
    }
    r.pictures.push_back(i);
  }
  return r;
}

WriteRequest parseWriteRequest(std::string_view text) {
  const json::Value v = json::parse(text);
  requireKeys(v, "the request", {"format", "tags", "cover"});
  WriteRequest r{parseFormat(*v.find("format")), {}, std::nullopt};

  const json::Value &tags = *v.find("tags");
  requireKeys(tags, "tags",
              {kFieldNames[0], kFieldNames[1], kFieldNames[2], kFieldNames[3], kFieldNames[4], kFieldNames[5],
               kFieldNames[6], kFieldNames[7], kFieldNames[8], kFieldNames[9], kFieldNames[10]});
  for (std::size_t i = 0; i < kFieldCount; ++i) {
    const auto f = static_cast<Field>(i);
    const json::Value &val = *tags.find(kFieldNames[i]);
    const std::string what = "tags." + std::string(kFieldNames[i]);
    if (f == Field::Compilation) {
      if (val.kind != json::Value::Kind::Bool) invalid(what + " must be true or false");
      r.values.compilation = val.boolean;
    } else if (isNumberField(f)) {
      if (val.kind != json::Value::Kind::Null) {
        r.values.number[i] = parseUint(val, what, 1, std::numeric_limits<std::int32_t>::max());
      }
    } else {
      r.values.text[i] = parseText(val, what);
    }
  }

  const json::Value &cover = *v.find("cover");
  if (cover.kind != json::Value::Kind::Null) {
    requireKeys(cover, "cover", {"mime", "width", "height", "depth", "colors"});
    const json::Value &mime = *cover.find("mime");
    if (mime.kind != json::Value::Kind::String || !isPrintableASCII(mime.string) || mime.string.size() > 255) {
      invalid("cover.mime must be 1..255 printable ASCII characters");
    }
    constexpr std::int64_t kMax = std::numeric_limits<std::uint32_t>::max();
    CoverSpec c;
    c.mime = mime.string;
    c.width = parseUint(*cover.find("width"), "cover.width", 0, kMax);
    c.height = parseUint(*cover.find("height"), "cover.height", 0, kMax);
    c.depth = parseUint(*cover.find("depth"), "cover.depth", 0, kMax);
    c.colors = parseUint(*cover.find("colors"), "cover.colors", 0, kMax);
    r.cover = c;
  }
  return r;
}

}  // namespace mltags
