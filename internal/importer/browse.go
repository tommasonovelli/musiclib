package importer

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"musiclib/internal/fsops"
	"musiclib/internal/media"
	"musiclib/internal/names"
)

// CodeSourceNotReadable: a directory under /import the process may not
// list (permissions).
const CodeSourceNotReadable = "source_not_readable"

// The types of a SourceEntry.
const (
	EntryDirectory = "directory"
	EntryFile      = "file"
	// EntrySymlink is listed, never followed (§5.2, §7.2, §10.4): the scan
	// rejects it, and no path through it can be browsed or imported.
	EntrySymlink = "symlink"
	// EntrySpecial is a FIFO, a socket or a device: never opened.
	EntrySpecial = "special"
	// EntryInvalidName is an entry whose name is not valid UTF-8: it can be
	// neither browsed nor imported (§5.2), and its name is shown with
	// U+FFFD in place of the invalid bytes.
	EntryInvalidName = "invalid_name"
)

// SourceEntry is one entry of a directory under /import: its name (a
// single segment, never a path) and its type.
type SourceEntry struct {
	Name string
	Type string
}

// Browse is §10.2 GET /api/import-source: the entries of the directory rel
// of src (/import), sorted by the bytes of their names, never in the
// filesystem's order (§7.3). rel is relative and validated as a path
// before any system call ("" is /import itself, §5.2). The directory is
// opened by fsops with RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS (§10.4): no
// symlink is followed, whether it is rel itself or one of its parents,
// and nothing outside /import is reached. Only the names and types are
// read: no file is opened (§7.1: "senza leggere arbitrariamente l'host").
//
// Errors are the importer's source codes: CodeSourceNotFound,
// CodeSourceNotDirectory, CodeSourceRejected (rel is or goes through a
// symlink or a special file), CodeSourceNotReadable; a names error for an
// invalid rel.
func Browse(src *fsops.Root, rel string) ([]SourceEntry, error) {
	if _, err := names.SplitRelPathOrRoot(rel); err != nil {
		return nil, err
	}
	ents, err := src.ReadDir(rel)
	if err != nil {
		return nil, browseError(rel, err)
	}
	out := make([]SourceEntry, len(ents))
	for i, e := range ents {
		out[i] = SourceEntry{Name: e.Name, Type: entryType(e)}
	}
	return out, nil
}

func entryType(e fsops.DirEntry) string {
	switch {
	case !utf8.ValidString(e.Name):
		return EntryInvalidName
	case e.Type == fsops.TypeDir:
		return EntryDirectory
	case e.Type == fsops.TypeRegular:
		return EntryFile
	case e.Type == fsops.TypeSymlink:
		return EntrySymlink
	default:
		return EntrySpecial
	}
}

// DisplayName is a name as JSON can carry it: the invalid bytes of a name
// that is not UTF-8 are replaced with U+FFFD (EntryInvalidName).
func DisplayName(name string) string {
	return strings.ToValidUTF8(name, "�")
}

// browseError types the failure to list rel.
func browseError(rel string, err error) error {
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	if fsops.Code(err) == fsops.CodePermission {
		return &Error{Code: CodeSourceNotReadable, Message: fmt.Sprintf("%s cannot be read by the server", label(rel, "")), Path: rel, Err: err}
	}
	return sourceError(rel, err)
}

// Summary is what a directory under /import holds, from the names and
// types of its entries alone (§7.1: no file is opened; NOTES.md N-286):
// the Import view's «14 tracks, 2 images». Tracks are the files with an
// audio extension of §7.2 and Images those with an image extension of the
// scan (noProbeExtensions): an estimate for the eye, since the import
// recognises audio by content (§7.2). Skipped are the entries the scan
// never follows or opens: symlinks, special files, invalid names.
type Summary struct {
	Folders, Tracks, Images, Others, Skipped int
}

// imageExtensions are the image extensions of noProbeExtensions, lowercase,
// without the dot.
var imageExtensions = [...]string{"jpg", "jpeg", "png", "gif", "bmp", "tif", "tiff", "webp"}

// Tally sums up the entries of one directory.
func Tally(ents []SourceEntry) Summary {
	var s Summary
	for _, e := range ents {
		switch e.Type {
		case EntryDirectory:
			s.Folders++
		case EntryFile:
			ext := path.Ext(e.Name)
			switch {
			case media.HasKnownAudioExtension(e.Name):
				s.Tracks++
			case ext != "" && slices.Contains(imageExtensions[:], asciiLower(ext[1:])):
				s.Images++
			default:
				s.Others++
			}
		default:
			s.Skipped++
		}
	}
	return s
}

// SortForDisplay orders entries as the Import view lists them: by the
// natural order of §7.3 on their comparison keys (§5.2), so that "disc 2"
// comes before "Disc 10" whatever the case, then by the bytes of the
// names, so the order is total.
func SortForDisplay(ents []SourceEntry) {
	slices.SortStableFunc(ents, func(a, b SourceEntry) int {
		if c := naturalCompare(names.Key(a.Name), names.Key(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
}
