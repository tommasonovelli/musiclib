package importer

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"musiclib/internal/fsops"
	"musiclib/internal/names"
)

// source is the read-only view of /import (§7.1, §3.2 guarantee 1): the only
// holder of the /import root, with the only three operations the importer
// performs on it: describe, list and open for reading. Nothing under
// /import is ever created, written, renamed or removed; the mount is
// read-only as well (§3.1).
type source struct {
	root *fsops.Root
}

func (s source) stat(r *fsops.Root, rel string) (fsops.FileInfo, error) { return r.Stat(rel) }

func (s source) readDir(r *fsops.Root, rel string) ([]fsops.DirEntry, error) { return r.ReadDir(rel) }

// open opens a regular file read-only; anything else is refused by fsops
// without blocking (N-013, N-030).
func (s source) open(r *fsops.Root, rel string) (*os.File, error) { return r.Open(rel) }

// sub opens a directory under /import as a root of its own; "" is /import
// itself, which the caller must not close (owned reports whether it must).
func (s source) sub(rel string) (r *fsops.Root, owned bool, err error) {
	if rel == "" {
		return s.root, false, nil
	}
	r, err = s.root.SubRoot(rel)
	return r, err == nil, err
}

// ignoredNames are the only files the scan ignores (§7.2), compared ASCII
// case-insensitively. Every other file, hidden ones included, is kept.
var ignoredNames = [...]string{".ds_store", "thumbs.db", "desktop.ini"}

// ignored reports whether a regular file is one of ignoredNames. Only A-Z
// are folded: Unicode folding would make, for example, "ſ" (U+017F) equal
// to "s".
func ignored(name string) bool {
	return slices.Contains(ignoredNames[:], asciiLower(name))
}

func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// identity is what §7.1 compares to detect a change of the source: the
// type, the file identity (st_dev, st_ino), the size and the mtime.
type identity struct {
	Type fsops.FileType
	Dev  uint64
	Ino  uint64
	Size int64
	// MTime is in nanoseconds since the epoch: comparable with ==.
	MTime int64
}

func identityOf(fi fsops.FileInfo) identity {
	return identity{Type: fi.Type, Dev: fi.Dev, Ino: fi.Ino, Size: fi.Size, MTime: fi.MTime.UnixNano()}
}

// srcFile is a regular file of the source that is not ignored.
type srcFile struct {
	// Rel is relative to the walk's root, exactly as on disk.
	Rel string
	ID  identity
	// Audio is the scan's view (scanAudio); the import decides by content.
	Audio bool
}

// srcDir is a directory of the source, with its entries sorted by the bytes
// of their names (fsops.Root.ReadDir), never in filesystem order (§7.3).
type srcDir struct {
	Rel      string
	ID       identity
	Files    []*srcFile
	Dirs     []*srcDir
	Rejected []rejectedEntry
}

// rejectedEntry is an entry that is never followed nor opened: a symlink, a
// special file, or a name that is not a valid relative path (§5.2, N-030).
type rejectedEntry struct {
	// Rel is the path as on disk; when Valid is false it is not a valid
	// relative path (invalid UTF-8, too deep) and is only quoted.
	Rel   string
	Valid bool
	Why   string
}

// walk reads the source tree under rel of r, recursively. depth is the
// number of segments of r's own path under /import: the full path of every
// entry must stay within names.MaxPathDepth (§5.2), and a directory beyond
// it is rejected, not entered. Only lstat, getdents and nothing that
// follows a symlink is used (fsops.Root.Stat, ReadDir).
func walk(ctx context.Context, src source, r *fsops.Root, depth int, rel string) (*srcDir, error) {
	fi, err := src.stat(r, rel)
	if err != nil {
		return nil, err
	}
	if fi.Type != fsops.TypeDir {
		return nil, &Error{Code: CodeSourceNotDirectory, Message: fmt.Sprintf("%q is a %s, not a directory", rel, fi.Type), Path: rel}
	}
	return walkDir(ctx, src, r, depth, rel, identityOf(fi))
}

func walkDir(ctx context.Context, src source, r *fsops.Root, depth int, rel string, id identity) (*srcDir, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d := &srcDir{Rel: rel, ID: id}
	entries, err := src.readDir(r, rel)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		child := joinRel(rel, e.Name)
		if why := invalidPath(child, depth); why != "" {
			d.Rejected = append(d.Rejected, rejectedEntry{Rel: child, Why: why})
			continue
		}
		// The type is the lstat's: getdents' d_type is only a hint, and the
		// entry may have been replaced since.
		fi, err := src.stat(r, child)
		if err != nil {
			return nil, err
		}
		switch fi.Type {
		case fsops.TypeRegular:
			if !ignored(e.Name) {
				d.Files = append(d.Files, &srcFile{Rel: child, ID: identityOf(fi)})
			}
		case fsops.TypeDir:
			sub, err := walkDir(ctx, src, r, depth, child, identityOf(fi))
			if err != nil {
				return nil, err
			}
			d.Dirs = append(d.Dirs, sub)
		default:
			d.Rejected = append(d.Rejected, rejectedEntry{Rel: child, Valid: true, Why: fi.Type.String()})
		}
	}
	return d, nil
}

// invalidPath says why child, under a root depth segments below /import,
// cannot be a source path (§5.2), or "".
func invalidPath(child string, depth int) string {
	segs, err := names.SplitRelPath(child)
	if err != nil {
		return "not a valid relative path (" + names.Code(err) + ")"
	}
	if depth+len(segs) > names.MaxPathDepth {
		return fmt.Sprintf("deeper than %d levels under /import", names.MaxPathDepth)
	}
	return ""
}

// joinRel joins relative paths, "" being the root.
func joinRel(parts ...string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "/")
}

// each calls fn for d and every directory below it, parents first, in
// name order.
func (d *srcDir) each(fn func(*srcDir)) {
	fn(d)
	for _, s := range d.Dirs {
		s.each(fn)
	}
}

// files returns every file of the subtree, sorted by the bytes of the path.
func (d *srcDir) files() []*srcFile {
	var out []*srcFile
	d.each(func(x *srcDir) { out = append(out, x.Files...) })
	slices.SortFunc(out, func(a, b *srcFile) int { return strings.Compare(a.Rel, b.Rel) })
	return out
}

// rejected returns every rejected entry of the subtree, sorted by path.
func (d *srcDir) rejected() []rejectedEntry {
	var out []rejectedEntry
	d.each(func(x *srcDir) { out = append(out, x.Rejected...) })
	slices.SortFunc(out, func(a, b rejectedEntry) int { return strings.Compare(a.Rel, b.Rel) })
	return out
}

// snapshot is the identity of every directory and file of the subtree, by
// path ("" is the root): what §7.1 compares at the end of an import.
func (d *srcDir) snapshot() map[string]identity {
	m := map[string]identity{}
	d.each(func(x *srcDir) {
		m[x.Rel] = x.ID
		for _, f := range x.Files {
			m[f.Rel] = f.ID
		}
		for _, r := range x.Rejected {
			m[r.Rel] = identity{Type: fsops.TypeUnknown}
		}
	})
	return m
}

// compareSnapshots returns the first path, in byte order, whose presence or
// identity differs between before and after, and whether one does.
func compareSnapshots(before, after map[string]identity) (string, bool) {
	var diff []string
	for p, id := range before {
		if got, ok := after[p]; !ok || got != id {
			diff = append(diff, p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			diff = append(diff, p)
		}
	}
	if len(diff) == 0 {
		return "", false
	}
	slices.Sort(diff)
	return diff[0], true
}
