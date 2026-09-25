package render

import (
	"runtime"

	"musiclib/internal/media"
	"musiclib/internal/names"
)

// RendererRevision is the revision of this package's own contribution to
// the output: the layout and names of the plan (§5.1), the mapping of the
// catalog to the managed tags (§8.2, TagValues), the build (§9.1) and the
// receipt (§9.2). Bump it with any change that can change a byte of an
// album directory; TestRendererRevisionPinned fails until it is bumped.
//
// The helper's own field table and the bytes it writes are covered by its
// version (media.PinnedTagsVersion, N-088); the names of the output by
// names.AlgorithmVersion.
//
// Revisions: 1, the first (round 8); 2, MP3 tracks planned and built
// (".mp3", round 12, NOTES.md N-158); 3, M4A tracks planned and built
// (".m4a", round 13, N-169).
const RendererRevision = "3"

// GoVersion is the Go toolchain the renderer is built with (the Dockerfile's
// GO_IMAGE). The renderer's Go code, image/jpeg and image/png (the cover
// attributes, N-087) and the Unicode tables of internal/names come with it.
// TestGoVersionPinned fails on another toolchain, and CheckTools refuses
// one at boot.
const GoVersion = "go1.25.14"

// Version is render_version (§2.1, NOTES.md N-010): the build constant that
// identifies the renderer code, the naming rules, the tag mapping and the
// tool versions. It is derived at compile time from the pinned constants,
// so a change of any of them changes it:
//   - RendererRevision: the planner, the builder and the receipt;
//   - names.AlgorithmVersion: the naming rules (§5.2);
//   - GoVersion: the toolchain;
//   - media.PinnedVersion: ffmpeg and ffprobe (one release, one version);
//   - media.PinnedTagsVersion: the TagLib helper, whose version also covers
//     its tag mapping (N-088);
//   - media.PinnedTagLibVersion: the TagLib linked into it.
//
// Each input is a token of [0-9A-Za-z.+-] (TestVersionInputs), so the
// string is unambiguous. It is stored as albums.published_renderer and
// compared at the boot's step 6 and at PREPARE (§6.3, §11.1).
const Version = "musiclib-render/" + RendererRevision +
	" names/" + names.AlgorithmVersion +
	" " + GoVersion +
	" ffmpeg/" + media.PinnedVersion +
	" musiclib-tags/" + media.PinnedTagsVersion +
	" taglib/" + media.PinnedTagLibVersion

// CheckTools verifies that the running process is the one Version names:
// the tools verified at boot (media.NewTools, §11.1 step 3) have the
// versions Version was derived from, and the binary was built with
// GoVersion. media.NewTools already refuses other tool versions; this check
// ties the two together, so that a later change of either cannot make the
// process label its output with a render_version it does not implement.
func CheckTools(v media.Versions) error {
	for _, c := range []struct{ what, got, want string }{
		{"ffmpeg", v.FFmpeg, media.PinnedVersion},
		{"ffprobe", v.FFprobe, media.PinnedVersion},
		{"musiclib-tags", v.Tags, media.PinnedTagsVersion},
		{"TagLib", v.TagLib, media.PinnedTagLibVersion},
		{"the Go toolchain", runtime.Version(), GoVersion},
	} {
		if c.got != c.want {
			return errorf(CodeVersionMismatch, "%s is version %q, render_version %q needs %q", c.what, c.got, Version, c.want)
		}
	}
	return nil
}
