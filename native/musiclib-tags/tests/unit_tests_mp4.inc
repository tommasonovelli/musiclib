// Unit tests of the ISO base media primitives (src/mp4.cpp), included by
// unit_tests.cpp inside its anonymous namespace, after unit_tests_mp3.inc
// (bytes, failsWith, be32s). The contract on real M4A files is tested from
// Go (internal/media).

std::string box(const std::string &type, const std::string &payload) { return be32s(static_cast<std::uint32_t>(payload.size() + 8)) + type + payload; }

// stscOf, stszOf and stcoOf are sample table payloads (after the box header).
std::string stscOf(std::initializer_list<std::array<std::uint32_t, 3>> runs) {
  std::string s = be32s(0) + be32s(static_cast<std::uint32_t>(runs.size()));
  for (const auto &r : runs) s += be32s(r[0]) + be32s(r[1]) + be32s(r[2]);
  return s;
}

std::string stszOf(std::uint32_t uniform, std::initializer_list<std::uint32_t> sizes, std::uint32_t count) {
  std::string s = be32s(0) + be32s(uniform) + be32s(count);
  for (const auto n : sizes) s += be32s(n);
  return s;
}

std::string stcoOf(std::initializer_list<std::uint64_t> offsets, bool co64) {
  std::string s = be32s(0) + be32s(static_cast<std::uint32_t>(offsets.size()));
  for (const auto o : offsets) s += co64 ? mp4::enc64(o) : be32s(static_cast<std::uint32_t>(o));
  return s;
}

void testMP4Boxes() {
  expect(mp4::be16(bytes({0x12, 0x34}), 0) == 0x1234 && mp4::enc16(0xBEEF) == bytes({0xBE, 0xEF}), "be16/enc16");
  expect(mp4::be64(mp4::enc64(0x0102030405060708ULL), 0) == 0x0102030405060708ULL, "be64/enc64");

  const std::string b = box("abcd", "xyz");
  const auto h = mp4::parseBoxHeader(b, 0, b.size(), false, "t");
  expect(h && h->type == "abcd" && h->size == 11 && h->header == 8 && h->payload() == 8 && h->end() == 11, "a small box");
  expect(!mp4::parseBoxHeader(b.substr(0, 7), 0, 100, false, "t"), "fewer than 8 bytes");
  const std::string large = be32s(1) + "mdat" + mp4::enc64(20) + "wxyz";
  const auto l = mp4::parseBoxHeader(large, 0, large.size(), false, "t");
  expect(l && l->header == 16 && l->size == 20 && l->payloadSize() == 4, "a 64-bit size");
  expect(!mp4::parseBoxHeader(large.substr(0, 12), 0, 100, false, "t"), "a 64-bit size needs 16 bytes");
  const std::string zero = be32s(0) + "mdat";
  const auto z = mp4::parseBoxHeader(zero, 4, 100, true, "t");
  expect(z && z->size == 96, "size 0 runs to the limit");
  expect(failsWith([&] { mp4::parseBoxHeader(zero, 4, 100, false, "t"); }, code::kCorrupt), "size 0 inside a container");
  expect(failsWith([&] { mp4::parseBoxHeader(be32s(7) + "abcd", 0, 100, false, "t"); }, code::kCorrupt), "size below the header");
  expect(failsWith([&] { mp4::parseBoxHeader(be32s(1) + "abcd" + mp4::enc64(15), 0, 100, false, "t"); }, code::kCorrupt),
         "a 64-bit size below its header");
  expect(failsWith([&] { mp4::parseBoxHeader(b, 0, 10, false, "t"); }, code::kCorrupt), "a box past its limit");
  expect(failsWith([&] { mp4::parseBoxHeader(be32s(1) + "abcd" + mp4::enc64(~0ULL), 8, 100, false, "t"); }, code::kCorrupt),
         "a 64-bit size that overflows");

  const std::string seq = box("aaaa", "1") + box("bbbb", "") + large;
  const auto kids = mp4::children(seq, 0, seq.size(), "t");
  expect(kids.size() == 3 && kids[1].offset == 9 && kids[2].type == "mdat" && kids[2].offset == 17, "children tile");
  expect(failsWith([&] { mp4::children(seq + "x", 0, seq.size() + 1, "t"); }, code::kCorrupt), "a byte after the children");
  expect(failsWith([&] { mp4::children(seq, 0, seq.size() - 1, "t"); }, code::kCorrupt), "a child past the end");
  expect(failsWith([&] { mp4::children(seq, 0, seq.size() + 1, "t"); }, code::kCorrupt), "a range past the data");
  expect(!mp4::tryChildren(seq, 0, 5) && mp4::tryChildren(seq, 0, 9), "tryChildren");
  expect(mp4::children("", 0, 0, "t").empty(), "no children");
  std::string many;
  for (std::size_t i = 0; i <= mp4::kMaxChildren; ++i) many += box("free", "");
  expect(failsWith([&] { mp4::children(many, 0, many.size(), "t"); }, code::kCorrupt), "more than kMaxChildren boxes");
  expect(mp4::children(many, 8, many.size(), "t").size() == mp4::kMaxChildren, "exactly kMaxChildren boxes");

  expect(mp4::renderBox("free", "ab") == box("free", "ab"), "renderBox");
  expect(mp4::renderBox("free", "ab", true) == be32s(1) + "free" + mp4::enc64(18) + "ab", "renderBox large");

  expect(mp4::isFullAtomMeta(be32s(0) + box("hdlr", std::string(25, '\0'))), "a full meta");
  expect(!mp4::isFullAtomMeta(box("hdlr", std::string(25, '\0'))), "a QuickTime meta starts with hdlr");
  expect(!mp4::isFullAtomMeta(box("ilst", "")), "a QuickTime meta starts with ilst");
  expect(mp4::isFullAtomMeta("abc"), "a short meta counts as full");
}

void testMP4Data() {
  const std::string d = mp4::renderData(mp4::kTypeUTF8, "Hi");
  expect(d == box("data", be32s(1) + be32s(0) + "Hi"), "renderData");
  const auto p = mp4::parseData(d);
  expect(p && p->type == 1 && p->locale == 0 && p->value == "Hi", "parseData");
  const std::string withLocale = box("data", be32s(21) + be32s(0x656E0000) + "\x01");
  const auto loc = mp4::parseData(withLocale);
  expect(loc && loc->type == 21 && loc->locale == 0x656E0000 && loc->value == "\x01", "a locale");
  expect(!mp4::parseData(box("data", be32s(1) + "abc")), "a data atom shorter than 16 bytes");
  expect(!mp4::parseData(box("date", be32s(1) + be32s(0))), "another type");
  expect(!mp4::parseData(d + "x"), "a size that is not the box's");

  expect(mp4::parseFullString(box("mean", be32s(0) + "com.apple.iTunes"), "mean") == "com.apple.iTunes", "mean");
  expect(!mp4::parseFullString(box("mean", be32s(1) + "x"), "mean"), "mean with flags");
  expect(!mp4::parseFullString(box("name", be32s(0) + "x"), "mean"), "name is not mean");
  expect(!mp4::parseFullString(box("mean", "abc"), "mean"), "short mean");

  expect(mp4::utf16beToUTF8(bytes({0x00, 0x41, 0x00, 0xE9})) == "A\xC3\xA9", "UTF-16BE");
  expect(mp4::utf16beToUTF8(bytes({0xD8, 0x3D, 0xDE, 0x00})) == "\xF0\x9F\x98\x80", "a surrogate pair");
  expect(!mp4::utf16beToUTF8(bytes({0x00})), "odd length");
  expect(!mp4::utf16beToUTF8(bytes({0xD8, 0x3D})), "a lone high surrogate");
  expect(!mp4::utf16beToUTF8(bytes({0xDE, 0x00, 0x00, 0x41})), "a lone low surrogate");
  expect(!mp4::utf16beToUTF8(bytes({0xD8, 0x3D, 0x00, 0x41})), "a high surrogate before a letter");
}

void testMP4SampleTable() {
  // 5 samples in 3 chunks: 2, 2, 1.
  const auto c = mp4::chunks(stscOf({{1, 2, 1}, {3, 1, 1}}), stszOf(0, {10, 20, 30, 40, 50}, 5), stcoOf({100, 200, 300}, false), false);
  expect(c.size() == 3 && c[0].offset == 100 && c[0].size == 30 && c[1].size == 70 && c[2].offset == 300 && c[2].size == 50,
         "chunks of a table");
  const auto u = mp4::chunks(stscOf({{1, 3, 1}}), stszOf(4, {}, 6), stcoOf({1ULL << 33, (1ULL << 33) + 12}, true), true);
  expect(u.size() == 2 && u[0].offset == 1ULL << 33 && u[0].size == 12 && u[1].size == 12, "uniform sizes and co64");
  expect(mp4::chunks(stscOf({}), stszOf(0, {}, 0), stcoOf({}, false), false).empty(), "an empty track");
  // Trailing bytes after the entries are ignored.
  expect(mp4::chunks(stscOf({{1, 1, 1}}) + "xx", stszOf(0, {7}, 1) + "y", stcoOf({9}, false) + "z", false).size() == 1,
         "bytes after the entries");

  const auto bad = [](const std::string &stsc, const std::string &stsz, const std::string &stco, const std::string &what) {
    expect(failsWith([&] { mp4::chunks(stsc, stsz, stco, false); }, code::kCorrupt), what);
  };
  bad(stscOf({{2, 1, 1}}), stszOf(0, {1}, 1), stcoOf({0}, false), "stsc does not start at chunk 1");
  bad(stscOf({{1, 1, 1}, {1, 1, 1}}), stszOf(0, {1, 1}, 2), stcoOf({0, 1}, false), "stsc runs not increasing");
  bad(stscOf({{1, 1, 1}, {3, 1, 1}}), stszOf(0, {1, 1}, 2), stcoOf({0, 1}, false), "stsc past the chunks");
  bad(stscOf({{1, 0, 1}}), stszOf(0, {}, 0), stcoOf({0}, false), "zero samples per chunk");
  bad(stscOf({{1, 1, 2}}), stszOf(0, {1}, 1), stcoOf({0}, false), "sample description 2");
  bad(stscOf({{1, 2, 1}}), stszOf(0, {1, 1, 1}, 3), stcoOf({0}, false), "stsz declares more samples");
  bad(stscOf({{1, 2, 1}}), stszOf(0, {1}, 1), stcoOf({0}, false), "the chunks hold more samples");
  bad(stscOf({{1, 1, 1}}), stszOf(0, {1}, 2), stcoOf({0, 1}, false), "stsz shorter than its count");
  bad(stscOf({{1, 1, 1}}).substr(0, 12), stszOf(0, {1}, 1), stcoOf({0}, false), "stsc shorter than its count");
  bad(stscOf({{1, 1, 1}}), stszOf(0, {1}, 1), stcoOf({0}, false).substr(0, 10), "stco shorter than its count");
  bad(stscOf({}), stszOf(0, {1}, 1), stcoOf({0}, false), "no stsc entry for a chunk");
  bad("abc", stszOf(0, {}, 0), stcoOf({}, false), "a short stsc");
  expect(failsWith([] { mp4::chunks(stscOf({{1, 1, 1}}), stszOf(2, {}, 1), stcoOf({~0ULL - 1}, true), true); }, code::kCorrupt),
         "a chunk past 2^64");
  // A huge uniform count cannot make the reader loop over samples.
  const auto big = mp4::chunks(stscOf({{1, 0x80000000U, 1}}), stszOf(1, {}, 0x80000000U), stcoOf({0}, false), false);
  expect(big.size() == 1 && big[0].size == 0x80000000ULL, "a large uniform chunk");
}

void testMP4PatchOffsets() {
  const std::string stco = stcoOf({50, 100, 150}, false);
  const std::string moved = mp4::patchOffsets(stco, false, 100, 24);
  expect(moved == stcoOf({50, 124, 174}, false), "offsets from 100 move by 24");
  expect(mp4::patchOffsets(stco, false, 100, -20) == stcoOf({50, 80, 130}, false), "a negative delta");
  expect(mp4::patchOffsets(stco + "tail", false, 0, 1) == stcoOf({51, 101, 151}, false) + "tail", "bytes after the entries kept");
  expect(mp4::patchOffsets(stcoOf({(1ULL << 32) + 5}, true), true, 0, 7) == stcoOf({(1ULL << 32) + 12}, true), "co64");
  expect(failsWith([] { mp4::patchOffsets(stcoOf({0xFFFFFFF0U}, false), false, 0, 16); }, code::kTooLarge),
         "a stco offset past 32 bits");
  expect(mp4::patchOffsets(stcoOf({0xFFFFFFF0U}, false), false, 0, 15) == stcoOf({0xFFFFFFFFU}, false), "the last 32-bit offset");
  expect(failsWith([] { mp4::patchOffsets(stcoOf({10}, false), false, 0, -11); }, code::kTooLarge), "a negative offset");
  expect(failsWith([] { mp4::patchOffsets(be32s(0) + be32s(2) + be32s(1), false, 0, 1); }, code::kCorrupt), "a short table");
}
