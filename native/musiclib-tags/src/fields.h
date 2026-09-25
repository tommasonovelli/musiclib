// The managed fields and their per-format keys (DESIGN.md §8.2).
//
// "La lista precisa per formato è una tabella costante dell'adapter, coperta
// da fixture; non è configurazione dell'utente." This file is that table:
// the canonical key of each managed field, the known aliases (read only as a
// fallback, never preferred over the canonical key, always removed by a
// write), the sort keys of the four managed names (removed by a write), and
// the keys that carry pictures (managed as the cover).
//
// FLAC (Vorbis comments) and MP3 (ID3v2, APE, ID3v1) are implemented and
// covered by the fixtures of internal/media. Besides this table, a FLAC
// write has one declared removal (NOTES.md N-090): ID3v2 and ID3v1 tags,
// which are not part of the format, are stripped whole (writeFlac in
// flac.cpp); an MP3 write has the declared rules of §8.3 (mp3.cpp): the ID3v1
// tag is removed and its comment migrated. The M4A table comes with its
// reader and writer (PROGRESS.md).
#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string_view>

namespace mltags {

// Field is a managed field of §8.2, in the order of the inspection output.
enum class Field : std::size_t {
  Title,
  Artist,
  AlbumArtist,
  Album,
  Track,
  TrackTotal,
  Disc,
  DiscTotal,
  Date,
  Genre,
  Compilation,
};
inline constexpr std::size_t kFieldCount = 11;

// kFieldNames are the JSON names of the fields, in Field order.
inline constexpr std::array<std::string_view, kFieldCount> kFieldNames = {
    "title", "artist", "album_artist", "album", "track", "track_total",
    "disc",  "disc_total", "date", "genre", "compilation",
};

constexpr std::size_t idx(Field f) noexcept { return static_cast<std::size_t>(f); }

// isNumberField reports whether a field is written as a decimal number.
constexpr bool isNumberField(Field f) noexcept {
  return f == Field::Track || f == Field::TrackTotal || f == Field::Disc || f == Field::DiscTotal;
}

// ---------------------------------------------------------------------------
// FLAC: Vorbis comments. Keys are compared in upper case, as Vorbis field
// names are case-insensitive ASCII (and TagLib upper-cases them).

struct VorbisField {
  Field field;
  std::string_view canonical;
  // Aliases in reading order; empty entries are unused.
  std::array<std::string_view, 2> aliases;
};

inline constexpr std::array<VorbisField, kFieldCount> kVorbisFields = {{
    {Field::Title, "TITLE", {}},
    {Field::Artist, "ARTIST", {}},
    // §8.2 names "ALBUM ARTIST"; "ALBUM_ARTIST" is the other spelling in use.
    {Field::AlbumArtist, "ALBUMARTIST", {"ALBUM ARTIST", "ALBUM_ARTIST"}},
    {Field::Album, "ALBUM", {}},
    // TRACKNUM: TagLib's own XiphComment::track() reads it as an alias.
    {Field::Track, "TRACKNUMBER", {"TRACKNUM"}},
    {Field::TrackTotal, "TRACKTOTAL", {"TOTALTRACKS"}},
    {Field::Disc, "DISCNUMBER", {}},
    {Field::DiscTotal, "DISCTOTAL", {"TOTALDISCS"}},
    // YEAR: TagLib's XiphComment::year() reads it as an alias of DATE.
    {Field::Date, "DATE", {"YEAR"}},
    {Field::Genre, "GENRE", {}},
    {Field::Compilation, "COMPILATION", {}},
}};

// Sort keys of title, artist, album artist and album (§8.2).
inline constexpr std::array<std::string_view, 4> kVorbisSortKeys = {
    "TITLESORT",
    "ARTISTSORT",
    "ALBUMARTISTSORT",
    "ALBUMSORT",
};

// Keys that carry pictures in a Vorbis comment: the base64 FLAC picture of
// Ogg files, the legacy base64 image and its MIME type. They are part of the
// managed cover: a write removes all of them.
inline constexpr std::array<std::string_view, 3> kVorbisPictureKeys = {
    "METADATA_BLOCK_PICTURE",
    "COVERART",
    "COVERARTMIME",
};

// isVorbisManagedKey reports whether a write removes the (upper-case) key:
// a canonical key, an alias, a sort key or a picture key.
constexpr bool isVorbisManagedKey(std::string_view key) noexcept {
  for (const auto &f : kVorbisFields) {
    if (key == f.canonical) return true;
    for (const auto a : f.aliases) {
      if (!a.empty() && key == a) return true;
    }
  }
  for (const auto k : kVorbisSortKeys) {
    if (key == k) return true;
  }
  for (const auto k : kVorbisPictureKeys) {
    if (key == k) return true;
  }
  return false;
}

// FLAC picture type of a front cover (FLAC PICTURE / ID3v2 APIC type 3).
inline constexpr std::uint32_t kFrontCover = 3;

// ---------------------------------------------------------------------------
// MP3: ID3v2 frames (ID3v2.4 identifiers: an ID3v2.2 or ID3v2.3 frame is
// looked up by its ID3v2.4 identifier, or by its ID3v2.3 one when ID3v2.4
// has none, see id3v2.cpp), APE items and ID3v1 fields. Reading follows
// DESIGN.md §8.1: the first non-empty value in ID3v2 (the frame of the §8.2
// table, then its aliases in order), then APE (its keys in order), then
// ID3v1; every other non-empty source that disagrees is a conflict. A write
// removes every frame and item of these tables and writes only the ID3v2.4
// frame of the §8.2 table.

// Id3Field is a managed field in an ID3v2 tag. frame is the §8.2 frame (empty
// for the totals, whose canonical source is the "/M" of TRCK and TPOS);
// aliasFrames are old frames read as a fallback; txxxAliases are the
// descriptions of TXXX frames read as a fallback (compared in ASCII upper
// case), the names the same field has as a Vorbis comment.
struct Id3Field {
  Field field;
  std::string_view frame;
  std::array<std::string_view, 1> aliasFrames;
  std::array<std::string_view, 3> txxxAliases;
};

inline constexpr std::array<Id3Field, kFieldCount> kId3Fields = {{
    {Field::Title, "TIT2", {}, {}},
    {Field::Artist, "TPE1", {}, {}},
    {Field::AlbumArtist, "TPE2", {}, {"ALBUM ARTIST", "ALBUMARTIST", "ALBUM_ARTIST"}},
    {Field::Album, "TALB", {}, {}},
    {Field::Track, "TRCK", {}, {}},
    {Field::TrackTotal, "", {}, {"TRACKTOTAL", "TOTALTRACKS"}},
    {Field::Disc, "TPOS", {}, {}},
    {Field::DiscTotal, "", {}, {"DISCTOTAL", "TOTALDISCS"}},
    // TYER: the year of ID3v2.3, which has no TDRC.
    {Field::Date, "TDRC", {"TYER"}, {}},
    {Field::Genre, "TCON", {}, {}},
    {Field::Compilation, "TCMP", {}, {}},
}};

// The old date frames of ID3v2.3 (§8.2: "vecchi campi ID3 anno/data") that
// complete the recording date, not read, always removed: date, time,
// recording dates. TORY, the original release year (the ID3v2.3 form of
// TDOR), is not the recording date: it is unmanaged and kept (owner
// decision 2026-09-25, NOTES.md N-161).
inline constexpr std::array<std::string_view, 3> kId3OldDateFrames = {"TDAT", "TIME", "TRDA"};

// Sort frames of title, artist, album artist and album (§8.2): the ID3v2.4
// ones, the ones MusicBrainz used in ID3v2.3, and TXXX frames named like the
// Vorbis sort keys. Always removed. TSOC (composer sort) is unmanaged.
inline constexpr std::array<std::string_view, 7> kId3SortFrames = {"TSOT", "TSOP", "TSO2", "TSOA", "XSOT", "XSOP", "XSOA"};
inline constexpr std::array<std::string_view, 4> kId3SortTXXX = {"TITLESORT", "ARTISTSORT", "ALBUMARTISTSORT",
                                                                 "ALBUMSORT"};

// The frame of pictures, managed as the cover: a write removes all of them.
inline constexpr std::string_view kId3PictureFrame = "APIC";

// The description of the COMM frame that receives the comment of an ID3v1
// tag (§8.3), and its language: "XXX", unknown (ID3v2.4 frames §4.10).
inline constexpr std::string_view kLegacyCommentDescription = "legacy-id3v1";
inline constexpr std::string_view kLegacyCommentLanguage = "XXX";

// ApeField is a managed field in an APE tag: its keys in reading order, in
// upper case (APE keys compare case-insensitively). For the totals the "/M"
// of the number key comes first (TRACK "N/M" is the usual form).
struct ApeField {
  Field field;
  std::array<std::string_view, 3> keys;
};

inline constexpr std::array<ApeField, kFieldCount> kApeFields = {{
    {Field::Title, {"TITLE"}},
    {Field::Artist, {"ARTIST"}},
    {Field::AlbumArtist, {"ALBUM ARTIST", "ALBUMARTIST", "ALBUM_ARTIST"}},
    {Field::Album, {"ALBUM"}},
    {Field::Track, {"TRACK", "TRACKNUMBER"}},
    {Field::TrackTotal, {"TRACKTOTAL", "TOTALTRACKS"}},
    {Field::Disc, {"DISC", "DISCNUMBER"}},
    {Field::DiscTotal, {"DISCTOTAL", "TOTALDISCS"}},
    {Field::Date, {"YEAR", "DATE"}},
    {Field::Genre, {"GENRE"}},
    {Field::Compilation, {"COMPILATION"}},
}};

inline constexpr std::array<std::string_view, 4> kApeSortKeys = {"TITLESORT", "ARTISTSORT", "ALBUMARTISTSORT",
                                                                 "ALBUMSORT"};

// APE items whose (upper-case) key starts with this carry pictures, managed
// as the cover: "Cover Art (Front)", "Cover Art (Back)", ...
inline constexpr std::string_view kApePicturePrefix = "COVER ART (";

// isApeManagedKey reports whether a write removes the (upper-case) APE key:
// a key of kApeFields, a sort key or a picture key.
constexpr bool isApeManagedKey(std::string_view key) noexcept {
  for (const auto &f : kApeFields) {
    for (const auto k : f.keys) {
      if (!k.empty() && key == k) return true;
    }
  }
  for (const auto k : kApeSortKeys) {
    if (key == k) return true;
  }
  return key.starts_with(kApePicturePrefix);
}

}  // namespace mltags
