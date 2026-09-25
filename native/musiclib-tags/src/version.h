// Version of the helper (DESIGN.md §2.1: an input of render_version).
#pragma once

namespace mltags {

// kHelperVersion identifies the behavior of this program: the managed-field
// tables, the reading rules and the bytes write-managed-tags produces. Bump it
// with any change that can change an inspection or a written file, together
// with media.PinnedTagsVersion in internal/media/tools.go.
//   1: the first release (round 3).
//   2: ID3v2 and ID3v1 tags in a FLAC file are stripped by a write instead
//      of refusing it (NOTES.md N-090).
//   3: MP3 (ID3v2, APE, ID3v1) in the three operations (N-152); the
//      inspection reports the audio range (N-154); ENOSPC and EDQUOT are
//      the failure code no_space (N-143).
inline constexpr const char *kHelperVersion = "3";

// MUSICLIB_TAGLIB_BUILD is the revision of the TagLib build configuration,
// passed by the Dockerfile (ARG TAGLIB_BUILD_REVISION). It is appended to the
// TagLib release read at run time, like FFmpeg's --extra-version.
#ifndef MUSICLIB_TAGLIB_BUILD
#error "MUSICLIB_TAGLIB_BUILD must be defined by the build"
#endif
inline constexpr const char *kTagLibBuild = MUSICLIB_TAGLIB_BUILD;

}  // namespace mltags
