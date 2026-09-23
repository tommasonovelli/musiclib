package catalog

import (
	"strings"

	"musiclib/internal/names"
)

// CollisionKind says how two paths collide after normalization (§5.2).
type CollisionKind int

const (
	// NoCollision: the paths are distinct after normalization.
	NoCollision CollisionKind = iota
	// SameFile: two files have the same path key.
	SameFile
	// FileIsDirectory: a file's path key is the key of a directory another
	// file needs.
	FileIsDirectory
	// DirectorySpelling: two files need a directory with the same key but a
	// different spelling, such as "Scans/" and "scans/" (owner decision
	// 2026-09-23, NOTES.md N-131).
	DirectorySpelling
)

// PathCollision is the one rule of §5.2 for a set of output paths made of
// final segments (already through names.Segment or names.FileSegment),
// "/"-joined: "collisioni dopo la normalizzazione, comprese quelle fra un
// file e una directory, producono un errore esplicito con entrambi i nomi".
// It returns the first collision as the indexes of the two paths:
//   - SameFile: the earlier and the later path with one key;
//   - DirectorySpelling: the earlier and the later path whose directory
//     prefixes share a key but not their exact spelling;
//   - FileIsDirectory: the file, then the first path that needs a
//     directory with its key.
//
// The first two are looked for in the order of paths, then the third. The
// catalog's import commit (attachments under Extras/) and the render
// planner (the whole album directory) both use it (§13.2: one
// implementation).
func PathCollision(paths []string) (a, b int, kind CollisionKind) {
	type dir struct {
		spelling string
		owner    int
	}
	files := make(map[string]int, len(paths))
	dirs := map[string]dir{}
	keys := make([]string, len(paths))
	for i, p := range paths {
		segs := strings.Split(p, "/")
		keys[i] = names.PathKey(segs)
		if other, ok := files[keys[i]]; ok {
			return other, i, SameFile
		}
		files[keys[i]] = i
		for d := 1; d < len(segs); d++ {
			k, spelling := names.PathKey(segs[:d]), strings.Join(segs[:d], "/")
			seen, ok := dirs[k]
			switch {
			case !ok:
				dirs[k] = dir{spelling: spelling, owner: i}
			case seen.spelling != spelling:
				return seen.owner, i, DirectorySpelling
			}
		}
	}
	for i := range paths {
		if d, ok := dirs[keys[i]]; ok {
			return i, d.owner, FileIsDirectory
		}
	}
	return 0, 0, NoCollision
}
