package importer

import (
	"fmt"
	"slices"
	"strings"

	"musiclib/internal/catalog"
)

// The limits of one candidate (§7.2), checked before the import: no file
// beyond them is ever ignored. They are the catalog's, which checks them
// again at the commit.
const (
	MaxTracks = catalog.MaxTracks
	MaxFiles  = catalog.MaxFiles
)

// branch is one outcome of the grouping: a candidate album (Err nil), or a
// branch that fails as a whole, with no partial import of it (§7.2 rule 4).
type branch struct {
	Dir *srcDir
	Err *Error
}

// grouping is the result of applying §7.2's rules to a tree.
type grouping struct {
	// Branches are sorted by the bytes of their path.
	Branches []branch
	// Unassigned are the files outside every branch (§7.2 rule 5), by path.
	Unassigned []*srcFile
	// Rejected are the entries outside every branch that were never
	// followed nor opened, by path.
	Rejected []rejectedEntry
}

// group applies the grouping rules of §7.2 to a tree whose files carry the
// scan's view of audio (srcFile.Audio). base is the path of the tree's root
// under /import, for the messages. It does no I/O. In Phase 2:
//
//   - rule 1: a directory with direct audio and no audio further down is a
//     candidate, its whole subtree included (subdirectories without audio
//     are its attachments);
//   - rule 4: direct audio together with audio further down is ambiguous:
//     the branch fails as a whole, CodeAmbiguousCandidate;
//   - rules 2 and 3: a directory without direct audio whose audio is only
//     in direct children named CD<N> or Disc <N> is one multi-disc album,
//     which Phase 5 groups; until then the branch fails with
//     CodeMultiDiscNotSupported, and is never grouped otherwise;
//   - rule 5: in the other branches independent albums are searched
//     recursively; files outside every branch are unassigned.
//
// A candidate also fails when its subtree holds a rejected entry
// (CodeSourceRejected) or exceeds the limits (catalog.CodeTooManyFiles).
func group(root *srcDir, base string) grouping {
	var g grouping
	audio := audioBelow(root)
	var visit func(d *srcDir)
	visit = func(d *srcDir) {
		direct := slices.ContainsFunc(d.Files, func(f *srcFile) bool { return f.Audio })
		below := ""
		for _, s := range d.Dirs {
			if p := audio[s]; p != "" && (below == "" || p < below) {
				below = p
			}
		}
		switch {
		case direct && below != "":
			g.Branches = append(g.Branches, branch{Dir: d, Err: &Error{
				Code: CodeAmbiguousCandidate, Path: d.Rel,
				Message: fmt.Sprintf("%s has audio files and more audio below it (%q): import its subdirectories separately", label(base, d.Rel), joinRel(base, below)),
			}})
		case direct:
			g.Branches = append(g.Branches, branch{Dir: d, Err: checkCandidate(d, base)})
		case below != "" && isDiscLayout(d, audio):
			g.Branches = append(g.Branches, branch{Dir: d, Err: &Error{
				Code: CodeMultiDiscNotSupported, Path: d.Rel,
				Message: fmt.Sprintf("%s is a multi-disc album (CD<N> or Disc <N> directories), which this version does not import yet", label(base, d.Rel)),
			}})
		default:
			g.Unassigned = append(g.Unassigned, d.Files...)
			g.Rejected = append(g.Rejected, d.Rejected...)
			for _, s := range d.Dirs {
				visit(s)
			}
		}
	}
	visit(root)
	slices.SortFunc(g.Branches, func(a, b branch) int { return strings.Compare(a.Dir.Rel, b.Dir.Rel) })
	slices.SortFunc(g.Unassigned, func(a, b *srcFile) int { return strings.Compare(a.Rel, b.Rel) })
	slices.SortFunc(g.Rejected, func(a, b rejectedEntry) int { return strings.Compare(a.Rel, b.Rel) })
	return g
}

// audioBelow maps every directory to the smallest path (in bytes) of an
// audio file in its subtree, "" when it has none.
func audioBelow(root *srcDir) map[*srcDir]string {
	m := map[*srcDir]string{}
	var visit func(d *srcDir) string
	visit = func(d *srcDir) string {
		first := ""
		for _, f := range d.Files {
			if f.Audio && (first == "" || f.Rel < first) {
				first = f.Rel
			}
		}
		for _, s := range d.Dirs {
			if p := visit(s); p != "" && (first == "" || p < first) {
				first = p
			}
		}
		m[d] = first
		return first
	}
	visit(root)
	return m
}

// isDiscLayout is the shape of §7.2 rule 2, for a directory without direct
// audio: every directory below it that holds audio files is a direct child
// named CD<N> or Disc <N>, and no such child has audio further down.
func isDiscLayout(d *srcDir, audio map[*srcDir]string) bool {
	found := false
	for _, s := range d.Dirs {
		if audio[s] == "" {
			continue
		}
		if !isDiscName(lastSegment(s.Rel)) {
			return false
		}
		for _, ss := range s.Dirs {
			if audio[ss] != "" {
				return false
			}
		}
		found = true
	}
	return found
}

// isDiscName matches "CD<N>" and "Disc <N>" with N a positive decimal
// number, leading zeros allowed, ASCII case-insensitive (§7.2 rule 2).
func isDiscName(name string) bool {
	n := asciiLower(name)
	switch {
	case strings.HasPrefix(n, "cd"):
		n = n[len("cd"):]
	case strings.HasPrefix(n, "disc "):
		n = n[len("disc "):]
	default:
		return false
	}
	if n == "" || strings.Trim(n, "0123456789") != "" {
		return false
	}
	return strings.Trim(n, "0") != ""
}

// checkCandidate refuses a candidate whose subtree holds a rejected entry,
// or that exceeds the limits of §7.2 as the scan sees it. The import checks
// the tracks again by content.
func checkCandidate(d *srcDir, base string) *Error {
	if rej := d.rejected(); len(rej) > 0 {
		return rejectedError(rej[0], base)
	}
	files := d.files()
	if len(files) > MaxFiles {
		return &Error{Code: catalog.CodeTooManyFiles, Path: d.Rel,
			Message: fmt.Sprintf("%s has %d files, the maximum is %d", label(base, d.Rel), len(files), MaxFiles)}
	}
	tracks := 0
	for _, f := range files {
		if f.Audio {
			tracks++
		}
	}
	if tracks > MaxTracks {
		return &Error{Code: catalog.CodeTooManyFiles, Path: d.Rel,
			Message: fmt.Sprintf("%s has %d audio files, the maximum is %d tracks", label(base, d.Rel), tracks, MaxTracks)}
	}
	return nil
}

func rejectedError(r rejectedEntry, base string) *Error {
	full := joinRel(base, r.Rel)
	e := &Error{Code: CodeSourceRejected, Message: fmt.Sprintf("%q is a %s: symlinks and special files are not imported", full, r.Why)}
	if r.Valid {
		e.Path = r.Rel
	} else {
		e.Message = fmt.Sprintf("%q is %s", full, r.Why)
	}
	return e
}

// label names a directory under /import in messages.
func label(base, rel string) string {
	if full := joinRel(base, rel); full != "" {
		return fmt.Sprintf("%q", full)
	}
	return "the import root"
}

func lastSegment(rel string) string {
	return rel[strings.LastIndexByte(rel, '/')+1:]
}
