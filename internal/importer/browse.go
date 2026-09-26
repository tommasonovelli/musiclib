package importer

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"musiclib/internal/fsops"
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
