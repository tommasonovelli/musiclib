// Package media is the adapter of the native media tools (DESIGN.md §2.3):
// ffprobe and ffmpeg, and the TagLib helper native/musiclib-tags.
//
// Every tool runs through [Runner], which enforces the rules of §8.5 and the
// global semaphore of §6.1. The input file is never named on a command line:
// the caller opens it through internal/fsops and the tool reads that
// descriptor only (the ffmpeg "fd" protocol, with every other protocol
// refused; the helper takes descriptors only), so a tool can neither resolve
// a path nor follow a playlist or reference out of the file it was given
// (NOTES.md N-075).
//
// The package knows formats and tools, not the domain: deciding that a
// failed probe of a file with a known audio extension is an album error
// belongs to the importer (§7.2).
package media

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"time"
)

// Fixed locations and the pinned versions of the tools (§2.1). The images
// install the same static binaries at these paths (Dockerfile, stages
// build-ffmpeg and build-tags); tests run the real ones.
const (
	FFmpegPath  = "/usr/local/bin/ffmpeg"
	FFprobePath = "/usr/local/bin/ffprobe"
	TagsPath    = "/usr/local/bin/musiclib-tags"

	// PinnedVersion is the version both FFmpeg tools must report: FFmpeg
	// 8.1.3 built with the configuration revision "musiclib1" of the
	// Dockerfile (--extra-version). A change of either the release or the
	// configure line changes it, and the boot refuses a mismatch.
	PinnedVersion = "8.1.3-musiclib1"

	// PinnedTagsVersion is the version of native/musiclib-tags
	// (kHelperVersion in src/version.h): it changes whenever an inspection
	// or a written file can change.
	PinnedTagsVersion = "2"
	// PinnedTagLibVersion is the TagLib the helper is linked with, as the
	// library reports it at run time, plus the revision of its build
	// configuration (Dockerfile, TAGLIB_BUILD_REVISION).
	PinnedTagLibVersion = "2.3.2-musiclib1"
)

// Versions are the exact version strings the tools report, read at startup
// and never assumed. They are an input of render_version (§2.1, N-010).
type Versions struct {
	FFmpeg  string
	FFprobe string
	Tags    string // native/musiclib-tags
	TagLib  string // the TagLib linked into it
}

// Tools runs ffprobe, ffmpeg and the TagLib helper. It is safe for
// concurrent use; every call takes a slot of the Runner.
type Tools struct {
	run      *Runner
	ffmpeg   string
	ffprobe  string
	tags     string
	versions Versions
	// tagsTimeout replaces InspectTimeout for the helper when positive:
	// only tests set it.
	tagsTimeout time.Duration
}

// NewTools reads the version of the three tools and checks it against the
// pinned ones. It fails with CodeToolUnavailable when a tool cannot run or
// does not print a version, and with CodeToolVersion on a mismatch (§11.1
// step 3: the boot verifies the primitives it needs).
func NewTools(ctx context.Context, run *Runner, ffmpegPath, ffprobePath, tagsPath string) (*Tools, error) {
	t := &Tools{run: run, ffmpeg: ffmpegPath, ffprobe: ffprobePath, tags: tagsPath}
	var err error
	if t.versions.FFmpeg, err = readVersion(ctx, run, ffmpegPath, "ffmpeg"); err != nil {
		return nil, err
	}
	if t.versions.FFprobe, err = readVersion(ctx, run, ffprobePath, "ffprobe"); err != nil {
		return nil, err
	}
	if t.versions.Tags, t.versions.TagLib, err = readTagsVersion(ctx, run, tagsPath); err != nil {
		return nil, err
	}
	for _, v := range []struct{ tool, got, want string }{
		{"ffmpeg", t.versions.FFmpeg, PinnedVersion},
		{"ffprobe", t.versions.FFprobe, PinnedVersion},
		{"musiclib-tags", t.versions.Tags, PinnedTagsVersion},
		{"musiclib-tags (TagLib)", t.versions.TagLib, PinnedTagLibVersion},
	} {
		if v.got != v.want {
			return nil, newErr(CodeToolVersion, v.tool, "version "+v.got+" is not the pinned "+v.want, nil)
		}
	}
	return t, nil
}

// Versions returns the versions read by NewTools.
func (t *Tools) Versions() Versions { return t.versions }

// versionLine matches the first line of `ffmpeg -version` and
// `ffprobe -version`: "<tool> version <version> Copyright ...".
var versionLine = regexp.MustCompile(`^(ffmpeg|ffprobe) version (\S+)`)

// readVersion runs `<path> -version` and returns the version token of its
// first line, which must name the expected tool.
func readVersion(ctx context.Context, run *Runner, path, tool string) (string, error) {
	var out bytes.Buffer
	res, err := run.Run(ctx, Command{
		Path:        path,
		Args:        []string{"-hide_banner", "-version"},
		Stdout:      &out,
		StdoutLimit: 1 << 20,
		Timeout:     InspectTimeout,
	})
	if err != nil {
		return "", unavailable(err, res)
	}
	first, _, _ := bytes.Cut(out.Bytes(), []byte("\n"))
	m := versionLine.FindSubmatch(first)
	if m == nil || string(m[1]) != tool {
		return "", newErr(CodeToolUnavailable, tool, "unexpected -version output: "+quoteShort(first), nil)
	}
	return string(m[2]), nil
}

// unavailable turns the failure of a version command into
// CodeToolUnavailable: a tool that runs but cannot print its version is as
// good as absent.
func unavailable(err error, res Result) error {
	var e *Error
	if errors.As(err, &e) && (e.Code == CodeToolFailed || e.Code == CodeOutputTooLarge) {
		e.Code = CodeToolUnavailable
		e.Stderr = res.Stderr
	}
	return err
}

// quoteShort quotes at most 80 bytes of b, for error messages.
func quoteShort(b []byte) string {
	if len(b) > 80 {
		b = b[:80]
	}
	return `"` + string(bytes.ToValidUTF8(b, []byte("?"))) + `"`
}
