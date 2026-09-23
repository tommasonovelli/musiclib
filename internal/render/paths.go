package render

import (
	"fmt"
	"strings"

	"musiclib/internal/names"
)

// checkOutputPath is the one rule for the paths of an album directory,
// relative to it: the planner applies it to every file it plans, and the
// receipt to every file it lists (Encode and ParseReceipt), so that a path
// the planner produces is always a path the receipt accepts (NOTES.md
// N-131, N-133).
//
// The limits of §5.2 (names.SplitRelPath: no empty, "." or ".." segment,
// valid UTF-8, no NUL, at most names.MaxPathDepth levels; at most
// names.MaxPathBytes bytes; each segment at most names.MaxSegmentBytes
// bytes) are measured on the path itself, except for an attachment: below
// Extras/, as the catalog measured its rel_path at the import commit
// (names.SanitizeRelFilePath). An attachment's album-relative path can
// therefore have one level and len("Extras/") bytes more.
func checkOutputPath(path string) error {
	measured := path
	if rest, ok := strings.CutPrefix(path, extrasDir+"/"); ok {
		measured = rest
	}
	segs, err := names.SplitRelPath(measured)
	if err != nil {
		return err
	}
	if len(measured) > names.MaxPathBytes {
		return &names.Error{Code: names.CodePathTooLong,
			Message: fmt.Sprintf("the path takes %d bytes, the maximum is %d", len(measured), names.MaxPathBytes)}
	}
	for _, s := range segs {
		if len(s) > names.MaxSegmentBytes {
			return &names.Error{Code: names.CodePathTooLong,
				Message: fmt.Sprintf("a segment takes %d bytes, the maximum is %d", len(s), names.MaxSegmentBytes)}
		}
	}
	return nil
}
