#include "json.h"

#include <limits>

#include "failure.h"
#include "text.h"

namespace mltags::json {

const Value *Value::find(std::string_view key) const {
  for (std::size_t i = 0; i < keys.size(); ++i) {
    if (keys[i] == key) return &values[i];
  }
  return nullptr;
}

namespace {

[[noreturn]] void bad(const std::string &what, std::size_t pos) {
  throw Failure(code::kInvalidRequest, "malformed JSON at byte " + std::to_string(pos) + ": " + what);
}

class Parser {
 public:
  explicit Parser(std::string_view in) : in_(in) {}

  Value document() {
    if (!validUTF8(in_)) bad("the request is not valid UTF-8", 0);
    skipSpace();
    Value v = value(1);
    skipSpace();
    if (pos_ != in_.size()) bad("unexpected data after the value", pos_);
    return v;
  }

 private:
  bool atEnd() const { return pos_ >= in_.size(); }
  char peek() const { return atEnd() ? '\0' : in_[pos_]; }

  void skipSpace() {
    while (!atEnd()) {
      const char c = in_[pos_];
      if (c != ' ' && c != '\t' && c != '\n' && c != '\r') return;
      ++pos_;
    }
  }

  void expect(char c) {
    if (peek() != c || atEnd()) bad(std::string("expected '") + c + "'", pos_);
    ++pos_;
  }

  Value value(int depth) {
    if (depth > kMaxDepth) bad("nesting deeper than " + std::to_string(kMaxDepth), pos_);
    if (atEnd()) bad("unexpected end of input", pos_);
    switch (peek()) {
      case '{':
        return object(depth);
      case '[':
        return array(depth);
      case '"': {
        Value v;
        v.kind = Value::Kind::String;
        v.string = string();
        return v;
      }
      case 't':
        literal("true");
        return boolean(true);
      case 'f':
        literal("false");
        return boolean(false);
      case 'n':
        literal("null");
        return Value{};
      default:
        return number();
    }
  }

  static Value boolean(bool b) {
    Value v;
    v.kind = Value::Kind::Bool;
    v.boolean = b;
    return v;
  }

  void literal(std::string_view word) {
    if (in_.substr(pos_, word.size()) != word) bad("invalid literal", pos_);
    pos_ += word.size();
  }

  Value number() {
    const std::size_t start = pos_;
    bool negative = false;
    if (peek() == '-') {
      negative = true;
      ++pos_;
    }
    if (atEnd() || peek() < '0' || peek() > '9') bad("invalid value", start);
    if (peek() == '0' && pos_ + 1 < in_.size() && in_[pos_ + 1] >= '0' && in_[pos_ + 1] <= '9') {
      bad("leading zeros are not allowed", start);
    }
    // Accumulate as a negative number, whose range includes INT64_MIN.
    std::int64_t acc = 0;
    constexpr std::int64_t kMin = std::numeric_limits<std::int64_t>::min();
    while (!atEnd() && peek() >= '0' && peek() <= '9') {
      const int digit = peek() - '0';
      if (acc < (kMin + digit) / 10) bad("integer out of range", start);
      acc = acc * 10 - digit;
      ++pos_;
    }
    if (!atEnd() && (peek() == '.' || peek() == 'e' || peek() == 'E')) bad("only integers are allowed", start);
    Value v;
    v.kind = Value::Kind::Int;
    if (negative) {
      v.integer = acc;
    } else {
      if (acc == kMin) bad("integer out of range", start);
      v.integer = -acc;
    }
    return v;
  }

  unsigned hex4() {
    if (in_.size() - pos_ < 4) bad("truncated \\u escape", pos_);
    unsigned v = 0;
    for (int i = 0; i < 4; ++i) {
      const char c = in_[pos_++];
      v <<= 4;
      if (c >= '0' && c <= '9') {
        v |= static_cast<unsigned>(c - '0');
      } else if (c >= 'a' && c <= 'f') {
        v |= static_cast<unsigned>(c - 'a' + 10);
      } else if (c >= 'A' && c <= 'F') {
        v |= static_cast<unsigned>(c - 'A' + 10);
      } else {
        bad("invalid \\u escape", pos_ - 1);
      }
    }
    return v;
  }

  std::string string() {
    expect('"');
    std::string out;
    while (true) {
      if (atEnd()) bad("unterminated string", pos_);
      const char c = in_[pos_];
      if (c == '"') {
        ++pos_;
        return out;
      }
      if (static_cast<unsigned char>(c) < 0x20) bad("control character in a string", pos_);
      if (c != '\\') {
        out.push_back(c);  // the input is valid UTF-8, so bytes can be copied
        ++pos_;
        continue;
      }
      ++pos_;
      if (atEnd()) bad("unterminated escape", pos_);
      const char e = in_[pos_++];
      switch (e) {
        case '"':
          out.push_back('"');
          break;
        case '\\':
          out.push_back('\\');
          break;
        case '/':
          out.push_back('/');
          break;
        case 'b':
          out.push_back('\b');
          break;
        case 'f':
          out.push_back('\f');
          break;
        case 'n':
          out.push_back('\n');
          break;
        case 'r':
          out.push_back('\r');
          break;
        case 't':
          out.push_back('\t');
          break;
        case 'u': {
          std::uint32_t cp = hex4();
          if (cp >= 0xDC00 && cp <= 0xDFFF) bad("lone low surrogate", pos_);
          if (cp >= 0xD800 && cp <= 0xDBFF) {
            if (in_.substr(pos_, 2) != "\\u") bad("lone high surrogate", pos_);
            pos_ += 2;
            const std::uint32_t low = hex4();
            if (low < 0xDC00 || low > 0xDFFF) bad("invalid surrogate pair", pos_);
            cp = 0x10000 + ((cp - 0xD800) << 10) + (low - 0xDC00);
          }
          appendUTF8(out, cp);
          break;
        }
        default:
          bad("invalid escape", pos_ - 1);
      }
    }
  }

  Value array(int depth) {
    expect('[');
    Value v;
    v.kind = Value::Kind::Array;
    skipSpace();
    if (peek() == ']' && !atEnd()) {
      ++pos_;
      return v;
    }
    while (true) {
      skipSpace();
      v.items.push_back(value(depth + 1));
      skipSpace();
      if (peek() == ',' && !atEnd()) {
        ++pos_;
        continue;
      }
      expect(']');
      return v;
    }
  }

  Value object(int depth) {
    expect('{');
    Value v;
    v.kind = Value::Kind::Object;
    skipSpace();
    if (peek() == '}' && !atEnd()) {
      ++pos_;
      return v;
    }
    while (true) {
      skipSpace();
      const std::size_t at = pos_;
      std::string k = string();
      if (v.find(k) != nullptr) bad("duplicate key \"" + k + "\"", at);
      skipSpace();
      expect(':');
      skipSpace();
      Value member = value(depth + 1);
      v.keys.push_back(std::move(k));
      v.values.push_back(std::move(member));
      skipSpace();
      if (peek() == ',' && !atEnd()) {
        ++pos_;
        continue;
      }
      expect('}');
      return v;
    }
  }

  std::string_view in_;
  std::size_t pos_ = 0;
};

}  // namespace

Value parse(std::string_view text) { return Parser(text).document(); }

// ---------------------------------------------------------------------------

void Writer::beforeValue() {
  if (afterKey_) {
    afterKey_ = false;
    return;
  }
  if (!empty_.empty()) {
    if (!empty_.back()) out_.push_back(',');
    empty_.back() = false;
  }
}

void Writer::beginObject() {
  beforeValue();
  out_.push_back('{');
  empty_.push_back(true);
}

void Writer::endObject() {
  out_.push_back('}');
  empty_.pop_back();
}

void Writer::beginArray() {
  beforeValue();
  out_.push_back('[');
  empty_.push_back(true);
}

void Writer::endArray() {
  out_.push_back(']');
  empty_.pop_back();
}

void Writer::key(std::string_view k) {
  beforeValue();
  quoted(k);
  out_.push_back(':');
  afterKey_ = true;
}

void Writer::string(std::string_view s) {
  beforeValue();
  quoted(s);
}

void Writer::integer(std::int64_t v) {
  beforeValue();
  out_ += std::to_string(v);
}

void Writer::uinteger(std::uint64_t v) {
  beforeValue();
  out_ += std::to_string(v);
}

void Writer::boolean(bool v) {
  beforeValue();
  out_ += v ? "true" : "false";
}

void Writer::null() {
  beforeValue();
  out_ += "null";
}

void Writer::quoted(std::string_view s) {
  if (!validUTF8(s)) throw Failure(code::kInternal, "refusing to print a string that is not valid UTF-8");
  static constexpr char kDigits[] = "0123456789abcdef";
  out_.push_back('"');
  for (const char c : s) {
    const auto b = static_cast<unsigned char>(c);
    switch (c) {
      case '"':
        out_ += "\\\"";
        break;
      case '\\':
        out_ += "\\\\";
        break;
      case '\n':
        out_ += "\\n";
        break;
      case '\r':
        out_ += "\\r";
        break;
      case '\t':
        out_ += "\\t";
        break;
      default:
        if (b < 0x20) {
          out_ += "\\u00";
          out_.push_back(kDigits[b >> 4]);
          out_.push_back(kDigits[b & 0x0F]);
        } else {
          out_.push_back(c);
        }
    }
  }
  out_.push_back('"');
}

std::string Writer::take() {
  if (!empty_.empty() || afterKey_) throw Failure(code::kInternal, "unbalanced JSON writer");
  return std::move(out_);
}

}  // namespace mltags::json
