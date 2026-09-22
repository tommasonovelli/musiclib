// Package media is the adapter of the native media tools (DESIGN.md §2.3):
// ffprobe and ffmpeg now, the TagLib helper (native/musiclib-tags) later.
//
// Every tool runs through [Runner], which enforces the rules of §8.5 and the
// global semaphore of §6.1. The input file is never named on a command line:
// the caller opens it through internal/fsops and the tool reads that
// descriptor only (the ffmpeg "fd" protocol, with every other protocol
// refused), so a tool can neither resolve a path nor follow a playlist or
// reference out of the file it was given (NOTES.md N-075).
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
)

// Fixed locations and the pinned version of the tools (§2.1). The images
// install the same static binaries at these paths (Dockerfile, stage
// build-ffmpeg); tests run the real ones.
const (
	FFmpegPath  = "/usr/local/bin/ffmpeg"
	FFprobePath = "/usr/local/bin/ffprobe"

	// PinnedVersion is the version both tools must report: FFmpeg 8.1.3
	// built with the configuration revision "musiclib1" of the Dockerfile
	// (--extra-version). A change of either the release or the configure
	// line changes it, and the boot refuses a mismatch.
	PinnedVersion = "8.1.3-musiclib1"
)

// Versions are the exact version strings the tools report, read at startup
// and never assumed. They are an input of render_version (§2.1, N-010).
type Versions struct {
	FFmpeg  string
	FFprobe string
}

// Tools runs ffprobe and ffmpeg. It is safe for concurrent use; every call
// takes a slot of the Runner.
type Tools struct {
	run      *Runner
	ffmpeg   string
	ffprobe  string
	versions Versions
}

// NewTools reads the version of both tools and checks it against
// PinnedVersion. It fails with CodeToolUnavailable when a tool cannot run or
// does not print a version line, and with CodeToolVersion on a mismatch
// (§11.1 step 3: the boot verifies the primitives it needs).
func NewTools(ctx context.Context, run *Runner, ffmpegPath, ffprobePath string) (*Tools, error) {
	t := &Tools{run: run, ffmpeg: ffmpegPath, ffprobe: ffprobePath}
	var err error
	if t.versions.FFmpeg, err = readVersion(ctx, run, ffmpegPath, "ffmpeg"); err != nil {
		return nil, err
	}
	if t.versions.FFprobe, err = readVersion(ctx, run, ffprobePath, "ffprobe"); err != nil {
		return nil, err
	}
	for _, v := range []struct{ tool, got string }{{"ffmpeg", t.versions.FFmpeg}, {"ffprobe", t.versions.FFprobe}} {
		if v.got != PinnedVersion {
			return nil, newErr(CodeToolVersion, v.tool,
				"version "+v.got+" is not the pinned "+PinnedVersion, nil)
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
		// A tool that runs but cannot print its version is as good as absent.
		var e *Error
		if errors.As(err, &e) && (e.Code == CodeToolFailed || e.Code == CodeOutputTooLarge) {
			e.Code = CodeToolUnavailable
			e.Stderr = res.Stderr
		}
		return "", err
	}
	first, _, _ := bytes.Cut(out.Bytes(), []byte("\n"))
	m := versionLine.FindSubmatch(first)
	if m == nil || string(m[1]) != tool {
		return "", newErr(CodeToolUnavailable, tool, "unexpected -version output: "+quoteShort(first), nil)
	}
	return string(m[2]), nil
}

// quoteShort quotes at most 80 bytes of b, for error messages.
func quoteShort(b []byte) string {
	if len(b) > 80 {
		b = b[:80]
	}
	return `"` + string(bytes.ToValidUTF8(b, []byte("?"))) + `"`
}
