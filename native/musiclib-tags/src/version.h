// Version of the helper (DESIGN.md §2.1: an input of render_version).
#pragma once

namespace mltags {

// kHelperVersion identifies the behavior of this program: the managed-field
// tables, the reading rules and the bytes write-managed-tags produces. Bump it
// with any change that can change an inspection or a written file, together
// with media.PinnedTagsVersion in internal/media/tools.go.
inline constexpr const char *kHelperVersion = "1";

// MUSICLIB_TAGLIB_BUILD is the revision of the TagLib build configuration,
// passed by the Dockerfile (ARG TAGLIB_BUILD_REVISION). It is appended to the
// TagLib release read at run time, like FFmpeg's --extra-version.
#ifndef MUSICLIB_TAGLIB_BUILD
#error "MUSICLIB_TAGLIB_BUILD must be defined by the build"
#endif
inline constexpr const char *kTagLibBuild = MUSICLIB_TAGLIB_BUILD;

}  // namespace mltags
