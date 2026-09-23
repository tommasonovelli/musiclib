package importer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path"
	"slices"
	"unicode/utf8"

	"musiclib/internal/catalog"
)

// isLRC reports whether a name has the .lrc extension, ASCII
// case-insensitively.
func isLRC(name string) bool {
	return asciiLower(path.Ext(name)) == ".lrc"
}

// associateLyrics is §7.4's rule for LRC files: an .lrc file is associated
// with a track only if exactly one track of its directory has the same stem,
// compared with NFC and casefold (catalog.StemKey, the rule the commit
// checks). Several such tracks, or several LRC files for one track, are an
// explicit error (catalog.CodeLyricsAssociation) naming the files. An LRC
// file without a match stays an attachment. It returns track path -> LRC
// path. files must be sorted; it does no I/O.
func associateLyrics(tracks, files []string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range files {
		dir, base := path.Split(f)
		if !isLRC(base) {
			continue
		}
		key := catalog.StemKey(base)
		var match []string
		for _, t := range tracks {
			if td, tb := path.Split(t); td == dir && catalog.StemKey(tb) == key {
				match = append(match, t)
			}
		}
		switch {
		case len(match) > 1:
			slices.Sort(match)
			return nil, &Error{Code: catalog.CodeLyricsAssociation, Path: f,
				Message: fmt.Sprintf("%q matches %d tracks (%s): rename the files so that it matches one", f, len(match), quoteList(match))}
		case len(match) == 1:
			if other, ok := out[match[0]]; ok {
				return nil, &Error{Code: catalog.CodeLyricsAssociation, Path: f,
					Message: fmt.Sprintf("%q and %q both match the track %q: keep one", other, f, match[0])}
			}
			out[match[0]] = f
		}
	}
	return out, nil
}

// validUTF8 reads r to its end and reports whether it is valid UTF-8, in
// constant memory (§10.2 requires UTF-8 text for LRC files; an imported one
// that is not stays an attachment, NOTES.md N-117).
func validUTF8(ctx context.Context, r io.Reader) (bool, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	for n := 0; ; n++ {
		if n%(1<<16) == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		c, size, err := br.ReadRune()
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if c == utf8.RuneError && size == 1 {
			return false, nil
		}
	}
}
