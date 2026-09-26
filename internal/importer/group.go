package importer

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"musiclib/internal/catalog"
)

// The limits of one candidate (§7.2), checked before the import: no file
// beyond them is ever ignored. They are the catalog's, which checks them
// again at the commit. For a multi-disc candidate they count every disc.
const (
	MaxTracks = catalog.MaxTracks
	MaxFiles  = catalog.MaxFiles
)

// branch is one outcome of the grouping: a candidate album (Err nil), or a
// branch that fails as a whole, with no partial import of it (§7.2 rules 3
// and 4).
type branch struct {
	Dir *srcDir
	// Discs are the disc directories of a multi-disc candidate (§7.2 rule
	// 2), by path; nil for a single-disc one (rule 1).
	Discs []discDir
	Err   *Error
}

// discDir is a disc directory of a multi-disc candidate: a direct child
// named CD<N> or Disc <N>, and its number N.
type discDir struct {
	Dir *srcDir
	No  int
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
// under /import, for the messages. It does no I/O.
//
//   - rule 1: a directory with direct audio and no audio further down is a
//     candidate, its whole subtree included (subdirectories without audio
//     are its attachments);
//   - rule 4: direct audio together with audio further down is ambiguous:
//     the branch fails as a whole, CodeAmbiguousCandidate;
//   - rule 2: a directory without direct audio whose directories with audio
//     below are all direct children named CD<N> or Disc <N> (isDiscName),
//     at least one of them with audio directly in it (discShaped), is one
//     multi-disc candidate, its whole subtree included (multiDisc);
//   - rule 3: in it, two disc directories with the same number fail the
//     branch (CodeDuplicateDisc), and so does a number over 99
//     (catalog.CodeInvalidDisc, §4.2); its other subdirectories, which have
//     no audio, are attachments. Audio below a disc directory's own level
//     makes the branch ambiguous (CodeAmbiguousCandidate, NOTES.md N-184);
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
			return
		case direct:
			g.Branches = append(g.Branches, branch{Dir: d, Err: checkCandidate(d, base)})
			return
		case below != "" && discShaped(d, audio):
			g.Branches = append(g.Branches, multiDisc(d, audio, base))
			return
		}
		g.Unassigned = append(g.Unassigned, d.Files...)
		g.Rejected = append(g.Rejected, d.Rejected...)
		for _, s := range d.Dirs {
			visit(s)
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

// discShaped reports whether a directory without direct audio has the shape
// of §7.2 rule 2 as far as its children go: every child that holds audio is
// named CD<N> or Disc <N>, and at least one of them has audio directly in
// it. Audio deeper than such a child is then multiDisc's error, not another
// grouping. When no disc-named child has direct audio (a data-CD backup,
// CD1/Artist - Album/*.mp3), the directory is not disc-shaped and rule 5
// applies (NOTES.md N-184).
func discShaped(d *srcDir, audio map[*srcDir]string) bool {
	direct := false
	for _, s := range d.Dirs {
		if audio[s] == "" {
			continue
		}
		if !isDiscName(lastSegment(s.Rel)) {
			return false
		}
		if slices.ContainsFunc(s.Files, func(f *srcFile) bool { return f.Audio }) {
			direct = true
		}
	}
	return direct
}

// multiDisc is the branch of a disc-shaped directory (§7.2 rules 2 and 3).
// Every direct child named CD<N> or Disc <N> is a disc directory, with or
// without audio (NOTES.md N-183). The checks, in this order, each fail the
// branch as a whole:
//   - audio below a disc directory's own level: CodeAmbiguousCandidate
//     (N-184);
//   - two disc directories with the same number: CodeDuplicateDisc;
//   - a number over 99 (§4.2, §7.3): catalog.CodeInvalidDisc, never
//     truncated;
//   - then checkCandidate: rejected entries and the limits, over all discs.
func multiDisc(d *srcDir, audio map[*srcDir]string, base string) branch {
	br := branch{Dir: d}
	for _, s := range d.Dirs {
		if n, ok := discNumber(lastSegment(s.Rel)); ok {
			br.Discs = append(br.Discs, discDir{Dir: s, No: n})
		}
	}
	fail := func(code, format string, args ...any) branch {
		br.Err = &Error{Code: code, Path: d.Rel, Message: fmt.Sprintf(format, args...)}
		return br
	}
	for _, disc := range br.Discs {
		deep := ""
		for _, s := range disc.Dir.Dirs {
			if p := audio[s]; p != "" && (deep == "" || p < deep) {
				deep = p
			}
		}
		if deep != "" {
			return fail(CodeAmbiguousCandidate,
				"%s is a multi-disc album, but %q is below its disc directory %q, not directly in it: move the audio files into the disc directory",
				label(base, d.Rel), joinRel(base, deep), joinRel(base, disc.Dir.Rel))
		}
	}
	byNo := map[int]*srcDir{}
	for _, disc := range br.Discs {
		if other, ok := byNo[disc.No]; ok {
			return fail(CodeDuplicateDisc, "%s has two directories for disc %d, %q and %q: keep one, or renumber one of them",
				label(base, d.Rel), disc.No, joinRel(base, other.Rel), joinRel(base, disc.Dir.Rel))
		}
		byNo[disc.No] = disc.Dir
	}
	for _, disc := range br.Discs {
		if disc.No > catalog.MaxDisc {
			return fail(catalog.CodeInvalidDisc, "%s: the disc directory %q has a number beyond the maximum disc, %d",
				label(base, d.Rel), joinRel(base, disc.Dir.Rel), catalog.MaxDisc)
		}
	}
	br.Err = checkCandidate(d, base)
	return br
}

// isDiscName matches "CD<N>" and "Disc <N>" with N a positive decimal
// number, leading zeros allowed, ASCII case-insensitive (§7.2 rule 2).
// "CD 1", "Disc1" and "CD0" are not disc names (NOTES.md N-120, N-183).
func isDiscName(name string) bool {
	_, ok := discNumber(name)
	return ok
}

// discNumber is the number N of a disc name (isDiscName), and whether name
// is one. A number too large for an int is math.MaxInt: over 99 anyway, it
// never wraps.
func discNumber(name string) (int, bool) {
	n := asciiLower(name)
	switch {
	case strings.HasPrefix(n, "cd"):
		n = n[len("cd"):]
	case strings.HasPrefix(n, "disc "):
		n = n[len("disc "):]
	default:
		return 0, false
	}
	if n == "" || strings.Trim(n, "0123456789") != "" {
		return 0, false
	}
	n = strings.TrimLeft(n, "0")
	if n == "" {
		return 0, false
	}
	v, err := strconv.Atoi(n)
	if err != nil {
		return math.MaxInt, true
	}
	return v, true
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
