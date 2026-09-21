package names

import (
	"strings"
	"unicode/utf8"
)

// SanitizedPath is the result of sanitizing a relative path intended for
// the output.
type SanitizedPath struct {
	// Segments are the sanitized segments, in their original order.
	Segments []string
	// Path is the segments joined by "/".
	Path string
	// Key is the comparison key of the path: the keys of the segments
	// joined by "/". It is the value of the attachments.path_key column.
	Key string
}

// SplitRelPath validates a relative path and returns its segments exactly as
// they are on disk, without sanitizing them (DESIGN.md §5.2: source_rel,
// source_path and root_rel are validated but opened with their original
// name).
//
// It rejects absolute paths, ".", "..", empty segments, invalid UTF-8, NUL
// bytes and a depth beyond MaxPathDepth. The empty string is an error: the
// root of /import uses SplitRelPathOrRoot.
func SplitRelPath(p string) ([]string, error) {
	if p == "" {
		return nil, errf(CodePathEmpty, "the relative path is empty")
	}
	return splitRelPath(p)
}

// SplitRelPathOrRoot is SplitRelPath but accepts the empty string, which
// denotes the root of /import and yields zero segments.
func SplitRelPathOrRoot(p string) ([]string, error) {
	if p == "" {
		return []string{}, nil
	}
	return splitRelPath(p)
}

func splitRelPath(p string) ([]string, error) {
	if !utf8.ValidString(p) {
		return nil, errf(CodeInvalidUTF8, "the path is not valid UTF-8")
	}
	if strings.IndexByte(p, 0) >= 0 {
		return nil, errf(CodePathNulByte, "the path contains a NUL byte")
	}
	if strings.HasPrefix(p, "/") {
		return nil, errf(CodePathAbsolute, "the path is absolute")
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		switch s {
		case "":
			return nil, errf(CodePathEmptySegment, "the path contains an empty segment")
		case ".", "..":
			return nil, errf(CodePathDotSegment, "the path contains the segment %q", s)
		}
	}
	if len(segs) > MaxPathDepth {
		return nil, errf(CodePathTooDeep,
			"the path has %d levels, the maximum is %d", len(segs), MaxPathDepth)
	}
	return segs, nil
}

// SanitizeRelFilePath validates a relative path and sanitizes it segment by
// segment: the last segment is treated as a file name (extension preserved),
// the preceding ones as directories.
//
// It is the path of attachments: rel_path keeps the original or
// user-chosen value, the render plan uses this result.
func SanitizeRelFilePath(p string) (SanitizedPath, error) {
	segs, err := SplitRelPath(p)
	if err != nil {
		return SanitizedPath{}, err
	}
	out := make([]string, len(segs))
	last := len(segs) - 1
	for i, s := range segs {
		if i == last {
			out[i] = FileSegment(s)
		} else {
			out[i] = Segment(s)
		}
	}
	joined := strings.Join(out, "/")
	if len(joined) > MaxPathBytes {
		return SanitizedPath{}, errf(CodePathTooLong,
			"the sanitized path takes %d bytes, the maximum is %d", len(joined), MaxPathBytes)
	}
	return SanitizedPath{Segments: out, Path: joined, Key: PathKey(out)}, nil
}
