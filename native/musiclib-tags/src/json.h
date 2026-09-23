// A small, strict JSON reader for the requests and a writer for the
// responses (DESIGN.md §8.1: "Input JSON limitato").
//
// The reader accepts RFC 8259 JSON with these restrictions, all of which the
// Go adapter satisfies by construction:
//   - the input must be valid UTF-8 (see validUTF8);
//   - numbers are integers only (no fraction, no exponent), within int64;
//   - object keys are unique;
//   - nesting is at most kMaxDepth levels;
//   - \u escapes must form valid scalar values (no lone surrogates).
// Any violation is Failure(invalid_request).
#pragma once

#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace mltags::json {

inline constexpr int kMaxDepth = 8;

struct Value {
  enum class Kind { Null, Bool, Int, String, Array, Object };
  Kind kind = Kind::Null;
  bool boolean = false;
  std::int64_t integer = 0;
  std::string string;
  std::vector<Value> items;       // Array
  std::vector<std::string> keys;  // Object, in input order, unique
  std::vector<Value> values;      // Object, parallel to keys

  // find returns the member called key of an object, or nullptr.
  const Value *find(std::string_view key) const;
};

// parse parses a complete JSON text: one value, surrounded only by
// whitespace.
Value parse(std::string_view text);

// Writer produces compact JSON. Strings must be valid UTF-8: the writer
// throws Failure(internal) otherwise, so the helper can never print output
// the Go side would decode lossily.
class Writer {
 public:
  void beginObject();
  void endObject();
  void beginArray();
  void endArray();
  void key(std::string_view k);
  void string(std::string_view s);
  void integer(std::int64_t v);
  void uinteger(std::uint64_t v);
  void boolean(bool v);
  void null();
  // take returns the text; the writer must be balanced.
  std::string take();

 private:
  void beforeValue();
  void quoted(std::string_view s);

  std::string out_;
  // One entry per open container: true while it has no element yet.
  std::vector<bool> empty_;
  bool afterKey_ = false;
};

}  // namespace mltags::json
