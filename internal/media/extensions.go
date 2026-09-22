package media

import (
	"path"
	"slices"
	"strings"
)

// knownAudioExtensions is the fixed list of §7.2, lowercase, without the
// dot. A file with one of these extensions that the probe cannot classify as
// audio is an album error, not a silent attachment (the importer applies the
// rule). The list is part of the specification: TestKnownAudioExtensions pins
// it, and it is not configurable.
var knownAudioExtensions = [...]string{
	"flac", "mp3", "m4a", "mp4", "aac", "wav", "aif", "aiff",
	"ogg", "opus", "wma", "ape", "wv", "dsf", "dff",
}

// KnownAudioExtensions returns a copy of the §7.2 list, in its order.
func KnownAudioExtensions() []string {
	return slices.Clone(knownAudioExtensions[:])
}

// HasKnownAudioExtension reports whether the last element of name has one of
// the extensions of §7.2, compared ASCII case-insensitively ("Track.FLAC" is
// known). It looks at the name only: content decides what a file is (§7.2);
// the extension only decides how a failed probe is reported.
func HasKnownAudioExtension(name string) bool {
	ext := path.Ext(path.Base(name))
	if ext == "" {
		return false
	}
	return slices.Contains(knownAudioExtensions[:], asciiLower(ext[1:]))
}

// asciiLower lowercases A-Z only: Unicode case mapping would make, for
// example, a Kelvin sign equal to "k".
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}
