package volume

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"musiclib/internal/fsops"
)

// The two marker files at the top of /data share one strict text format:
// fixed "key=value" lines, in a fixed order, each terminated by "\n", with
// nothing before, between or after them. There is no whitespace trimming,
// no comment, no CR, no optional key and no unknown key. A store id is a
// UUID in canonical form: 36 lowercase characters with hyphens, not nil.
//
//	/data/.musiclib-store            /data/.maintenance
//	store_id=<uuid>\n                operation=<rebuild|restore>\n
//	                                 store_id=<uuid>\n
//
// Anything else is malformed and blocks the boot: a marker is never guessed
// at, repaired or rewritten automatically (DESIGN.md §11.1, §11.3).

// Maintenance operations that leave /data/.maintenance behind while they run
// (DESIGN.md §11.3, §11.4).
const (
	OpRebuild = "rebuild"
	OpRestore = "restore"
)

// maxMarkerSize bounds what is read from a marker: a well-formed one is
// under 100 bytes, and anything larger is malformed without reading it all.
const maxMarkerSize = 512

// Maintenance is the content of /data/.maintenance: the operation in
// progress and the store it works on (§11.3: "operazione e store_id").
type Maintenance struct {
	Operation string
	StoreID   uuid.UUID
}

// Encode returns the marker's bytes. It is the only writer of the format;
// the rebuild and restore subcommands (Phase 6) create the marker with it.
func (m Maintenance) Encode() []byte {
	return []byte("operation=" + m.Operation + "\nstore_id=" + m.StoreID.String() + "\n")
}

// ParseMaintenance parses /data/.maintenance strictly (see the format above).
func ParseMaintenance(b []byte) (Maintenance, error) {
	vals, err := parseLines(b, "operation", "store_id")
	if err != nil {
		return Maintenance{}, err
	}
	op := vals[0]
	if op != OpRebuild && op != OpRestore {
		return Maintenance{}, fmt.Errorf("unknown operation %q (want %q or %q)", op, OpRebuild, OpRestore)
	}
	id, err := parseStoreID(vals[1])
	if err != nil {
		return Maintenance{}, err
	}
	return Maintenance{Operation: op, StoreID: id}, nil
}

// EncodeStoreMarker returns the bytes of /data/.musiclib-store for id.
func EncodeStoreMarker(id uuid.UUID) []byte {
	return []byte("store_id=" + id.String() + "\n")
}

// ParseStoreMarker parses /data/.musiclib-store strictly (see the format
// above).
func ParseStoreMarker(b []byte) (uuid.UUID, error) {
	vals, err := parseLines(b, "store_id")
	if err != nil {
		return uuid.Nil, err
	}
	return parseStoreID(vals[0])
}

// parseLines checks that b is exactly one "key=value\n" line per key, in
// order, and returns the values.
func parseLines(b []byte, keys ...string) ([]string, error) {
	s := string(b)
	if !strings.HasSuffix(s, "\n") {
		return nil, errors.New("the content does not end with a newline")
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) != len(keys) {
		return nil, fmt.Errorf("%d lines, want %d", len(lines), len(keys))
	}
	vals := make([]string, len(keys))
	for i, key := range keys {
		v, ok := strings.CutPrefix(lines[i], key+"=")
		if !ok {
			return nil, fmt.Errorf("line %d does not start with %q", i+1, key+"=")
		}
		vals[i] = v
	}
	return vals, nil
}

// parseStoreID accepts only the canonical form: uuid.Parse alone would also
// take uppercase, braces and "urn:uuid:" prefixes.
func parseStoreID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s {
		return uuid.Nil, fmt.Errorf("store id %q is not a canonical lowercase UUID", s)
	}
	if id == uuid.Nil {
		return uuid.Nil, errors.New("store id is the nil UUID")
	}
	return id, nil
}

// readMarker reads a marker file of root. found is false, with a nil error,
// only when the name does not exist. Something at the name that is not a
// regular file (a directory, a symlink, a FIFO), or a file that is too
// large, is reported with malformedCode: it is never followed, and a FIFO
// is never waited on (fsops.Open). Other failures are CodePermission or
// CodeIO.
func readMarker(root *fsops.Root, rel, malformedCode string) (content []byte, found bool, err error) {
	f, err := root.Open(rel)
	switch fsops.Code(err) {
	case "":
	case fsops.CodeNotFound:
		return nil, false, nil
	case fsops.CodeIsDirectory, fsops.CodeSymlink, fsops.CodeSpecialFile:
		return nil, true, newErr(malformedCode, rel+" is not a regular file", err)
	case fsops.CodePermission:
		return nil, true, newErr(CodePermission, "cannot read "+rel, err)
	default:
		return nil, true, newErr(CodeIO, "cannot open "+rel, err)
	}
	b, rerr := io.ReadAll(io.LimitReader(f, maxMarkerSize+1))
	if cerr := f.Close(); cerr != nil || rerr != nil {
		return nil, true, newErr(CodeIO, "cannot read "+rel, errors.Join(rerr, cerr))
	}
	if len(b) > maxMarkerSize {
		return nil, true, newErr(malformedCode, fmt.Sprintf("%s is larger than %d bytes", rel, maxMarkerSize), nil)
	}
	return b, true, nil
}
