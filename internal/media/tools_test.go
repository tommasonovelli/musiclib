package media

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The installed tools are the pinned ones, and their versions are read, not
// assumed (§2.1, N-010). A Dockerfile bump without a matching PinnedVersion
// fails here, in the gate, before it can fail a boot.
func TestPinnedToolsInstalled(t *testing.T) {
	tools := newTools(t)
	want := Versions{FFmpeg: PinnedVersion, FFprobe: PinnedVersion}
	if got := tools.Versions(); got != want {
		t.Fatalf("Versions() = %+v, want %+v", got, want)
	}
	if PinnedVersion != "8.1.3-musiclib1" {
		t.Fatalf("PinnedVersion changed to %q: update NOTES.md N-073, docs/docker.md and the Dockerfile together", PinnedVersion)
	}
}

// §8.5: 30 s for inspection, 30 min for the decode of one track.
func TestTimeouts(t *testing.T) {
	if InspectTimeout != 30*time.Second || DecodeTimeout != 30*time.Minute {
		t.Fatalf("timeouts %v / %v, §8.5 says 30s / 30m", InspectTimeout, DecodeTimeout)
	}
}

// A missing tool, a tool of another version, a tool that is not what its
// path says and a tool that fails are all refused, each with its code.
func TestNewToolsRefusesWrongTools(t *testing.T) {
	dir := t.TempDir()
	fake := func(name, script string) string {
		p := filepath.Join(dir, name)
		writeFile(t, p, []byte("#!/bin/sh\n"+script+"\n"))
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldFFmpeg := fake("ffmpeg-old", `echo "ffmpeg version 7.1.2 Copyright (c) 2000-2025 the FFmpeg developers"`)
	oldFFprobe := fake("ffprobe-old", `echo "ffprobe version 8.1.3 Copyright (c) 2007-2026 the FFmpeg developers"`)
	garbled := fake("garbled", `echo "hello"`)
	failing := fake("failing", `echo "ffmpeg version `+PinnedVersion+`"; exit 1`)

	for _, tc := range []struct {
		name            string
		ffmpeg, ffprobe string
		code            string
	}{
		{"ffmpeg missing", filepath.Join(dir, "none"), FFprobePath, CodeToolUnavailable},
		{"ffprobe missing", FFmpegPath, filepath.Join(dir, "none"), CodeToolUnavailable},
		{"ffmpeg of another version", oldFFmpeg, FFprobePath, CodeToolVersion},
		{"ffprobe without the configuration suffix", FFmpegPath, oldFFprobe, CodeToolVersion},
		{"not a version line", garbled, FFprobePath, CodeToolUnavailable},
		{"ffprobe at the ffmpeg path", FFprobePath, FFprobePath, CodeToolUnavailable},
		{"ffmpeg at the ffprobe path", FFmpegPath, FFmpegPath, CodeToolUnavailable},
		{"version printed, exit 1", failing, FFprobePath, CodeToolUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewTools(t.Context(), NewRunner(1), tc.ffmpeg, tc.ffprobe)
			wantCode(t, err, tc.code)
		})
	}
}

func TestVersionLine(t *testing.T) {
	for line, want := range map[string]string{
		"ffmpeg version 8.1.3-musiclib1 Copyright (c) 2000-2026 the FFmpeg developers": "8.1.3-musiclib1",
		"ffprobe version n8.1 Copyright":                                               "n8.1",
		"ffmpeg version 8.1.3":                                                         "8.1.3",
		" ffmpeg version 8.1.3":                                                        "",
		"ffplay version 8.1.3 Copyright":                                               "",
		"ffmpeg  version 8.1.3":                                                        "",
		"libavutil      60.  8.100 / 60.  8.100":                                       "",
	} {
		m := versionLine.FindStringSubmatch(line)
		got := ""
		if m != nil {
			got = m[2]
		}
		if got != want {
			t.Errorf("%q: version %q, want %q", line, got, want)
		}
	}
}

// §7.2 fixes the list; the test pins it, order included.
func TestKnownAudioExtensions(t *testing.T) {
	want := []string{"flac", "mp3", "m4a", "mp4", "aac", "wav", "aif", "aiff", "ogg", "opus", "wma", "ape", "wv", "dsf", "dff"}
	got := KnownAudioExtensions()
	if !slices.Equal(got, want) {
		t.Fatalf("KnownAudioExtensions() = %q, want %q", got, want)
	}
	got[0] = "changed"
	if KnownAudioExtensions()[0] != "flac" {
		t.Fatal("KnownAudioExtensions returns the package's own array")
	}

	for name, known := range map[string]bool{
		"01 Track.flac":      true,
		"01 Track.FLAC":      true,
		"a/b/c.Mp3":          true,
		"x.m4a":              true,
		"x.MP4":              true,
		"x.aiff":             true,
		"x.dff":              true,
		"x.tar.flac":         true,
		".flac":              true,
		"flac":               false,
		"x.flac.txt":         false,
		"x.jpg":              false,
		"x.cue":              false,
		"x.log":              false,
		"x.m3u":              false,
		"x.":                 false,
		"x.fla":              false,
		"x.flacc":            false,
		"dir.flac/cover.jpg": false,
		"x.ＦＬＡC":             false, // fullwidth letters are not ASCII
		"x.FLAС":             false, // Cyrillic Es, not C
	} {
		if got := HasKnownAudioExtension(name); got != known {
			t.Errorf("HasKnownAudioExtension(%q) = %v, want %v", name, got, known)
		}
	}
}
