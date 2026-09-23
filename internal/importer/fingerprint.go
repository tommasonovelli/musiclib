package importer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
)

// fingerprintEntry is one file of a candidate for the fingerprint (§7.6):
// its original path relative to the candidate (N-099), its size and its
// SHA-256.
type fingerprintEntry struct {
	Path string
	Size int64
	Hash string
}

// fingerprintJSON is §7.6's serialization: compact UTF-8 JSON of the list of
// [path, size, hash] triples sorted by the UTF-8 bytes of the path, with no
// HTML escaping and no trailing newline. Only strings and integers occur, so
// the encoding is unique.
func fingerprintJSON(entries []fingerprintEntry) ([]byte, error) {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b fingerprintEntry) int { return strings.Compare(a.Path, b.Path) })
	triples := make([][3]any, len(sorted))
	for i, e := range sorted {
		triples[i] = [3]any{e.Path, e.Size, e.Hash}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(triples); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// fingerprint is the SHA-256 of fingerprintJSON, lowercase hex: the value of
// albums.import_fingerprint (§7.6). It covers every non-ignored file of the
// candidate, and neither the absolute root nor any metadata.
func fingerprint(entries []fingerprintEntry) (string, error) {
	b, err := fingerprintJSON(entries)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
