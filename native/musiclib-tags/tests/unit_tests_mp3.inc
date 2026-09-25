// Unit tests of the ID3v2 and APE parsers and writers (src/id3v2.cpp,
// src/ape.cpp), included by unit_tests.cpp inside its anonymous namespace.
// The contract on real MP3 files is tested from Go (internal/media).

std::string bytes(std::initializer_list<int> b) {
  std::string s;
  for (const int c : b) s.push_back(static_cast<char>(c));
  return s;
}

// failsWith reports whether f throws a Failure with the code.
template <typename F>
bool failsWith(F f, const char *code) {
  try {
    f();
  } catch (const Failure &e) {
    return std::string(e.code()) == code;
  }
  return false;
}

std::string be32s(std::uint32_t n) {
  return bytes({static_cast<int>(n >> 24), static_cast<int>((n >> 16) & 0xFF), static_cast<int>((n >> 8) & 0xFF),
                static_cast<int>(n & 0xFF)});
}

// tagOf is a tag of the given version and flags around body.
std::string tagOf(int major, int flags, const std::string &body) {
  return "ID3" + bytes({major, 0, flags}) + id3v2::encodeSynchsafe(static_cast<std::uint32_t>(body.size())) + body;
}

id3v2::Tag parsed(const std::string &tag) {
  const auto h = id3v2::parseHeader(tag);
  if (!h) {
    fail("no ID3v2 header");
    return {};
  }
  return id3v2::parseTag(tag, *h);
}

void testID3v2Primitives() {
  for (const std::uint32_t n : {0U, 1U, 127U, 128U, 0x3FFFU, 0x4000U, 123456U, id3v2::kMaxSynchsafe}) {
    const auto back = id3v2::synchsafe(id3v2::encodeSynchsafe(n));
    expect(back && *back == n, "synchsafe round trip " + std::to_string(n));
  }
  expect(!id3v2::synchsafe(bytes({0, 0, 0x80, 0})), "a size byte of 0x80 is not synchsafe");
  expect(id3v2::deunsynchronise(bytes({0xFF, 0x00, 0xE0, 0xFF, 0x00, 0x00, 0xFF})) == bytes({0xFF, 0xE0, 0xFF, 0x00, 0xFF}),
         "deunsynchronise removes one zero after each 0xFF");
  expect(id3v2::deunsynchronise("abc") == "abc", "deunsynchronise leaves other bytes");

  expect(!id3v2::parseHeader("ID2" + std::string(7, '\0')), "not ID3");
  expect(!id3v2::parseHeader("ID3"), "short");
  const auto h = id3v2::parseHeader(tagOf(4, 0x10, "x"));
  expect(h && h->footer() && h->completeSize() == 21, "ID3v2.4 footer adds 10 bytes");
  const auto h3 = id3v2::parseHeader(tagOf(3, 0x80, "x"));
  expect(h3 && h3->unsynchronised() && !h3->footer() && h3->completeSize() == 11, "ID3v2.3 flags");
  for (const auto &[tag, what] : std::vector<std::pair<std::string, std::string>>{
           {tagOf(3, 0x10, ""), "footer flag in ID3v2.3"},
           {tagOf(2, 0x40, ""), "compressed ID3v2.2"},
           {tagOf(2, 0x20, ""), "ID3v2.2 flag 0x20"},
           {tagOf(4, 0x08, ""), "ID3v2.4 flag 0x08"},
           {tagOf(5, 0, ""), "version 5"},
           {tagOf(1, 0, ""), "version 1"},
           {"ID3" + bytes({0xFF, 0, 0, 0, 0, 0, 0}), "version 0xFF"},
           {"ID3" + bytes({4, 0xFF, 0, 0, 0, 0, 0}), "revision 0xFF"},
           {"ID3" + bytes({4, 0, 0, 0, 0, 0x80, 0}), "size not synchsafe"},
       }) {
    const std::string t = tag;
    expect(failsWith([&] { id3v2::parseHeader(t); }, code::kCorrupt), "must be corrupt: " + what);
  }

  expect(id3v2::id3v2Of22("TT2") == "TIT2" && id3v2::id3v2Of22("TOA") == "TOPE" && id3v2::id3v2Of22("TOT") == "TOAL" &&
             id3v2::id3v2Of22("TYE") == "TYER" && id3v2::id3v2Of22("PIC") == "APIC" && id3v2::id3v2Of22("TXT") == "TEXT",
         "ID3v2.2 mapping from the specification");
  expect(id3v2::id3v2Of22("CRM").empty() && id3v2::id3v2Of22("LNK").empty() && id3v2::id3v2Of22("XYZ").empty() &&
             id3v2::id3v2Of22("TIT2").empty(),
         "ID3v2.2 frames without an ID3v2.4 form");
}

void testID3v2Text() {
  using V = std::vector<std::string>;
  auto strings = [](std::uint8_t enc, const std::string &b) { return id3v2::decodeStrings(enc, b); };
  expect(strings(0, bytes({'a', 0xE9, 0, 'b', 0, 0})) == V{"a\xC3\xA9", "b"}, "Latin-1 list");
  expect(strings(3, "caf\xC3\xA9\0x" + std::string(1, '\0')) == V{"caf\xC3\xA9"}, "UTF-8 until the NUL, then x");
  expect(!strings(3, "\xC3"), "invalid UTF-8");
  expect(!strings(4, "a"), "unknown encoding");
  // UTF-16 with byte order marks, little and big endian, and a second string
  // without one (the first string's order, TagLib's rule).
  expect(strings(1, bytes({0xFF, 0xFE, 'a', 0, 0, 0, 0xFE, 0xFF, 0, 'b', 0, 0, 'c', 0})) == V{"a", "b", "c"},
         "UTF-16 with and without BOM");
  expect(!strings(1, bytes({'a', 0})), "UTF-16 first string without a BOM");
  expect(strings(1, bytes({0xFF, 0xFE, 0x3D, 0xD8, 0x00, 0xDE})) == V{"\xF0\x9F\x98\x80"}, "surrogate pair");
  expect(!strings(1, bytes({0xFF, 0xFE, 0x3D, 0xD8})), "lone high surrogate");
  expect(!strings(1, bytes({0xFF, 0xFE, 0x00, 0xDE, 'a', 0})), "lone low surrogate");
  expect(!strings(2, bytes({0, 'a', 'b'})), "odd UTF-16BE length");
  expect(strings(2, bytes({0, 'a', 0, 0, 0})) == V{"a"}, "UTF-16BE with a stray zero byte at the end");
  expect(strings(1, bytes({0xFF, 0xFE})) == V{}, "a BOM alone is an empty string");

  const std::string field = bytes({0xFF, 0xFE, 'd', 0, 0, 0, 'x', 'y'});
  const auto t = id3v2::decodeTerminated(1, field);
  expect(t && t->text == "d" && t->rest == "xy", "terminated UTF-16");
  expect(!id3v2::decodeTerminated(0, "abc"), "no terminator");
  expect(id3v2::decodeTerminated(0, std::string("\0rest", 5))->text.empty(), "empty terminated string");
  expect(id3v2::decodeWhole(0, std::string("a\0b\0\0", 5)) == std::string("a\0b", 3), "whole string keeps inner NUL");
}

void testID3v2Tags() {
  // ID3v2.3: a TIT2, a compressed frame, an encrypted and grouped frame,
  // padding with garbage after its first zero byte.
  const std::string tit2 = "TIT2" + be32s(4) + bytes({0, 0}) + std::string("\0abc", 4);
  const std::string zlib = "COMM" + be32s(4 + 3) + bytes({0x20, 0x80}) + be32s(99) + "zzz";
  const std::string enc = "PRIV" + be32s(1 + 1 + 2) + bytes({0, 0x60}) + bytes({7, 9}) + "pp";
  const std::string body3 = tit2 + zlib + enc + std::string(20, '\0') + "garbage";
  const id3v2::Tag t3 = parsed(tagOf(3, 0, body3));
  expect(t3.opaque.empty() && t3.frames.size() == 3, "ID3v2.3 frames");
  if (t3.frames.size() == 3) {
    const auto &c = t3.frames[1];
    expect(c.id == "COMM" && c.compressed && c.hasLength && c.length == 99 && c.body == "zzz" && c.status == 0x10,
           "ID3v2.3 compression becomes an ID3v2.4 data length, read-only becomes 0x10");
    const auto &e = t3.frames[2];
    expect(e.encrypted && e.method == 7 && e.grouping && e.group == 9 && e.body == "pp",
           "ID3v2.3 encryption then grouping");
    // The ID3v2.4 form of these frames reads back identically.
    const id3v2::Tag back = parsed(id3v2::renderTag(t3.frames, 5));
    bool same = back.opaque.empty() && back.frames.size() == 3;
    for (std::size_t i = 0; same && i < 3; ++i) {
      const auto &x = back.frames[i], &y = t3.frames[i];
      same = x.id == y.id && x.status == y.status && x.grouping == y.grouping && x.group == y.group &&
             x.compressed == y.compressed && x.encrypted == y.encrypted && x.method == y.method &&
             x.hasLength == y.hasLength && x.length == y.length && x.body == y.body;
    }
    expect(same, "ID3v2.4 rendering round trip");
  }

  // ID3v2.3 whole-tag unsynchronisation.
  // The frame size counts the bytes after the unsynchronisation is undone.
  const id3v2::Tag u3 = parsed(tagOf(3, 0x80, "TIT2" + be32s(3) + bytes({0, 0, 0, 0xFF, 0x00, 0xE0})));
  expect(u3.frames.size() == 1 && u3.frames[0].body == bytes({0, 0xFF, 0xE0}), "ID3v2.3 unsynchronised tag");

  // ID3v2.4 frame unsynchronisation and a data length indicator.
  const std::string f4 = "TPE1" + id3v2::encodeSynchsafe(4 + 4) + bytes({0, 0x03}) + id3v2::encodeSynchsafe(3) +
                         bytes({0, 0xFF, 0x00, 0xE0});
  const id3v2::Tag u4 = parsed(tagOf(4, 0, f4));
  expect(u4.opaque.empty() && u4.frames.size() == 1 && u4.frames[0].body == bytes({0, 0xFF, 0xE0}) &&
             !u4.frames[0].hasLength,
         "ID3v2.4 unsynchronised frame with its data length");
  const std::string badLength = "TPE1" + id3v2::encodeSynchsafe(4 + 2) + bytes({0, 0x01}) + id3v2::encodeSynchsafe(5) + "ab";
  expect(parsed(tagOf(4, 0, badLength)).opaque.size() == 1, "a data length that is not the data's");
  const std::string noLength = "TPE1" + id3v2::encodeSynchsafe(2) + bytes({0, 0x08}) + "ab";
  expect(parsed(tagOf(4, 0, noLength)).opaque.at(0).reason == std::string(reason::kMalformedFrame),
         "compression without a data length");

  // Unknown flags, empty frames, ID3v2.2 mapping, an unknown identifier.
  expect(parsed(tagOf(3, 0, "TIT2" + be32s(1) + bytes({0x01, 0}) + "x")).opaque.at(0).reason ==
             std::string(reason::kUnknownFlags),
         "ID3v2.3 unknown flag");
  expect(parsed(tagOf(4, 0, "TIT2" + id3v2::encodeSynchsafe(1) + bytes({0x80, 0}) + "x")).opaque.at(0).reason ==
             std::string(reason::kUnknownFlags),
         "ID3v2.4 unknown flag");
  const id3v2::Tag empty = parsed(tagOf(4, 0, "TIT2" + id3v2::encodeSynchsafe(0) + bytes({0, 0}) + "TPE1" +
                                                    id3v2::encodeSynchsafe(1) + bytes({0, 0}) + "x"));
  expect(empty.frames.size() == 1 && empty.opaque.size() == 1 && empty.opaque[0].removed &&
             empty.opaque[0].reason == std::string(reason::kEmptyFrame),
         "an empty frame is removed, the next one read");
  const id3v2::Tag v22 = parsed(tagOf(2, 0, "TT2" + bytes({0, 0, 2}) + std::string("\0a", 2) + "CRM" + bytes({0, 0, 1}) + "x"));
  expect(v22.frames.size() == 1 && v22.frames[0].id == "TIT2" && v22.frames[0].sourceID == "TT2" &&
             v22.opaque.size() == 1 && v22.opaque[0].key == "id3v2:CRM" && !v22.opaque[0].removed,
         "ID3v2.2 frames mapped, CRM unsupported");
  // A frame larger than the tag, and lower-case identifiers, stop the
  // reading: blocking.
  expect(parsed(tagOf(4, 0, "TIT2" + id3v2::encodeSynchsafe(100) + bytes({0, 0}) + "x")).opaque.at(0).reason ==
             std::string(reason::kMalformedFrame),
         "frame past the tag");
  expect(parsed(tagOf(4, 0, "tit2" + id3v2::encodeSynchsafe(1) + bytes({0, 0}) + "x")).opaque.size() == 1,
         "lower-case identifier");
  // TagLib's iTunes rule: an ID3v2.4 frame whose size is only right when
  // read as a plain integer.
  const std::string itunes = "TIT2" + be32s(130) + bytes({0, 0}) + std::string(130, 'a') + "TPE1" +
                             id3v2::encodeSynchsafe(1) + bytes({0, 0}) + "b";
  const id3v2::Tag it = parsed(tagOf(4, 0, itunes));
  expect(it.opaque.empty() && it.frames.size() == 2 && it.frames[0].body.size() == 130, "iTunes frame sizes");
  // Extended headers are skipped.
  const id3v2::Tag ext3 = parsed(tagOf(3, 0x40, be32s(6) + std::string(6, '\0') + tit2));
  expect(ext3.frames.size() == 1, "ID3v2.3 extended header");
  const id3v2::Tag ext4 = parsed(tagOf(4, 0x40, id3v2::encodeSynchsafe(6) + bytes({1, 0}) + "TPE1" +
                                                   id3v2::encodeSynchsafe(1) + bytes({0, 0}) + "b"));
  expect(ext4.frames.size() == 1, "ID3v2.4 extended header");
  expect(failsWith([] { parsed(tagOf(3, 0x40, be32s(7) + std::string(7, '\0'))); }, code::kCorrupt),
         "ID3v2.3 extended header of size 7");
  expect(failsWith([] { parsed(tagOf(4, 0x40, id3v2::encodeSynchsafe(90))); }, code::kCorrupt),
         "ID3v2.4 extended header past the tag");
  // Rendering refuses what an ID3v2.4 size cannot hold.
  expect(failsWith([] { id3v2::renderTag({}, std::uint64_t{id3v2::kMaxSynchsafe} + 1); }, code::kTooLarge),
         "a tag of 256 MiB");
  expect(id3v2::renderTag({}, 3) == "ID3" + bytes({4, 0, 0, 0, 0, 0, 3, 0, 0, 0}), "an empty tag with padding");
}

void testAPE() {
  auto le = [](std::uint32_t n) {
    return bytes({static_cast<int>(n & 0xFF), static_cast<int>((n >> 8) & 0xFF), static_cast<int>((n >> 16) & 0xFF),
                  static_cast<int>(n >> 24)});
  };
  const std::string item1 = le(3) + le(0) + "Title" + std::string(1, '\0') + "abc";
  const std::string item2 = le(2) + le(2) + "Cover Art (Front)" + std::string(1, '\0') + "xy";
  const auto items = ape::parseItems(item1 + item2 + "junk", 2);
  expect(items.size() == 2 && items[0].key == "Title" && items[0].value == "abc" && items[0].raw == item1 &&
             items[1].flags == 2 && items[1].raw == item2,
         "APE items, bytes after them ignored");
  expect(failsWith([&] { ape::parseItems(item1, 2); }, code::kCorrupt), "a missing item");
  expect(failsWith([&] { ape::parseItems(le(9) + le(0) + "K" + std::string(1, '\0') + "a", 1); }, code::kCorrupt),
         "an item past the tag");
  expect(failsWith([&] { ape::parseItems(le(0) + le(0) + "Key", 1); }, code::kCorrupt), "a key without its end");

  const ape::Footer withHeader{2000, 0, 5, 0x80000000U};
  const std::string tag = ape::render(withHeader, items);
  expect(tag.size() == 32 + item1.size() + item2.size() + 32, "rendered size");
  const auto head = ape::parseFooter(tag), foot = ape::parseFooter(tag.substr(tag.size() - 32));
  expect(head && foot && head->isHeader() && !foot->isHeader() && foot->hasHeader() && foot->count == 2 &&
             foot->size == item1.size() + item2.size() + 32 && foot->completeSize() == tag.size() &&
             head->size == foot->size && foot->version == 2000,
         "rendered header and footer");
  const std::string noHeader = ape::render(ape::Footer{1000, 0, 0, 0}, {items[0]});
  expect(noHeader.size() == item1.size() + 32 && ape::parseFooter(noHeader.substr(item1.size()))->version == 1000,
         "APEv1 tag without header");
  expect(!ape::parseFooter("APETAGEY" + std::string(24, '\0')), "not a footer");
}
