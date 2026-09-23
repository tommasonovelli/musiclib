// The managed fields and their per-format keys (DESIGN.md §8.2).
//
// "La lista precisa per formato è una tabella costante dell'adapter, coperta
// da fixture; non è configurazione dell'utente." This file is that table:
// the canonical key of each managed field, the known aliases (read only as a
// fallback, never preferred over the canonical key, always removed by a
// write), the sort keys of the four managed names (removed by a write), and
// the keys that carry pictures (managed as the cover).
//
// FLAC (Vorbis comments) is implemented and covered by the fixtures of
// internal/media. The MP3 (ID3v2.4 / APE / ID3v1) and M4A tables are Phase 4
// (PROGRESS.md), together with their readers and writers.
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

}  // namespace mltags
