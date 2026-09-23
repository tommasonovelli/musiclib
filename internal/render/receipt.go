package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"musiclib/internal/blobstore"
)

// ReceiptSchemaVersion is the only schema_version of the receipt (§9.2).
const ReceiptSchemaVersion = 1

// maxReceiptBytes bounds what ParseReceipt reads: 10,000 files (§7.2) plus
// the tracks' LRC files and the cover, at about 1.2 KB each at most.
const maxReceiptBytes = 16 << 20

// Receipt is the content of .musiclib.json (§9.2), exactly: no names, no
// titles, no timestamps, and not itself among its files.
type Receipt struct {
	AlbumID       uuid.UUID
	BuildID       uuid.UUID
	AlbumRevision int64
	RenderVersion string
	// Files are sorted by the bytes of RelativePath (UTF-8), strictly: no
	// path twice.
	Files []ReceiptFile
}

// ReceiptFile is one file of the album directory, relative to it.
type ReceiptFile struct {
	RelativePath string
	Size         int64
	SHA256       string
}

// Encode serializes the receipt in its one canonical form, which is the
// file's bytes and whose SHA-256 is the receipt_hash (§9.2):
//
//	{"schema_version":1,"album_id":"<uuid>","build_id":"<uuid>",
//	 "album_revision":<n>,"render_version":"<s>",
//	 "files":[{"relative_path":"<s>","size":<n>,"sha256":"<hex>"},...]}
//
// on one line (the line break above is for reading only):
//   - compact JSON (RFC 8259), the keys in exactly this order, no space, no
//     trailing newline;
//   - UUIDs in lowercase 8-4-4-4-12 form; integers in decimal without sign,
//     leading zero or exponent;
//   - strings raw UTF-8, with only `"`, `\` and the control characters
//     U+0000..U+001F escaped: `\"`, `\\`, and `\u00XX` with lowercase hex.
//     Nothing else is escaped: not "<", ">", "&", U+2028 or U+2029 (unlike
//     encoding/json, whose choices this format does not depend on).
//
// It refuses a receipt that ParseReceipt would refuse: the two are exact
// inverses.
func (r Receipt) Encode() ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	return r.encode(), nil
}

func (r Receipt) encode() []byte {
	var b bytes.Buffer
	b.WriteString(`{"schema_version":`)
	b.WriteString(strconv.Itoa(ReceiptSchemaVersion))
	b.WriteString(`,"album_id":`)
	writeString(&b, r.AlbumID.String())
	b.WriteString(`,"build_id":`)
	writeString(&b, r.BuildID.String())
	b.WriteString(`,"album_revision":`)
	b.WriteString(strconv.FormatInt(r.AlbumRevision, 10))
	b.WriteString(`,"render_version":`)
	writeString(&b, r.RenderVersion)
	b.WriteString(`,"files":[`)
	for i, f := range r.Files {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"relative_path":`)
		writeString(&b, f.RelativePath)
		b.WriteString(`,"size":`)
		b.WriteString(strconv.FormatInt(f.Size, 10))
		b.WriteString(`,"sha256":`)
		writeString(&b, f.SHA256)
		b.WriteByte('}')
	}
	b.WriteString("]}")
	return b.Bytes()
}

// writeString writes s, valid UTF-8, as a JSON string of the canonical form.
func writeString(b *bytes.Buffer, s string) {
	const hexDigits = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

// ReceiptHash is the receipt_hash of encoded receipt bytes: their SHA-256 in
// lowercase hex (§9.2).
func ReceiptHash(encoded []byte) string {
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// receiptJSON is the shape ParseReceipt decodes before checking that the
// input is exactly the canonical encoding of the result.
type receiptJSON struct {
	SchemaVersion *int64            `json:"schema_version"`
	AlbumID       *string           `json:"album_id"`
	BuildID       *string           `json:"build_id"`
	AlbumRevision *int64            `json:"album_revision"`
	RenderVersion *string           `json:"render_version"`
	Files         []receiptFileJSON `json:"files"`
}

type receiptFileJSON struct {
	RelativePath *string `json:"relative_path"`
	Size         *int64  `json:"size"`
	SHA256       *string `json:"sha256"`
}

// ParseReceipt reads a receipt strictly: the publisher's recovery (§9.4) and
// doctor (§11.3) trust nothing else. It accepts exactly the bytes Encode
// produces, so it refuses unknown, missing or repeated fields, another key
// order, whitespace, another escaping or number form, and a trailing
// newline; and it checks the content: schema_version 1, two non-nil UUIDs,
// a positive revision, a render_version, and files with valid relative
// paths (not the receipt itself), sizes and SHA-256, sorted by path bytes
// with no path twice. Any refusal is CodeReceiptInvalid.
func ParseReceipt(data []byte) (Receipt, error) {
	fail := func(format string, args ...any) (Receipt, error) {
		return Receipt{}, errorf(CodeReceiptInvalid, "the receipt is not valid: "+format, args...)
	}
	if len(data) > maxReceiptBytes {
		return fail("%d bytes, the maximum is %d", len(data), maxReceiptBytes)
	}
	if !utf8.Valid(data) {
		return fail("not valid UTF-8")
	}
	var j receiptJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&j); err != nil {
		return fail("%v", err)
	}
	if dec.More() {
		return fail("data after the receipt")
	}
	if j.SchemaVersion == nil || j.AlbumID == nil || j.BuildID == nil || j.AlbumRevision == nil ||
		j.RenderVersion == nil || j.Files == nil {
		return fail("a field is missing")
	}
	if *j.SchemaVersion != ReceiptSchemaVersion {
		return fail("schema_version %d, only %d is known", *j.SchemaVersion, ReceiptSchemaVersion)
	}
	albumID, err1 := uuid.Parse(*j.AlbumID)
	buildID, err2 := uuid.Parse(*j.BuildID)
	if err1 != nil || err2 != nil {
		return fail("album_id or build_id is not a UUID")
	}
	r := Receipt{AlbumID: albumID, BuildID: buildID, AlbumRevision: *j.AlbumRevision, RenderVersion: *j.RenderVersion,
		Files: make([]ReceiptFile, len(j.Files))}
	for i, f := range j.Files {
		if f.RelativePath == nil || f.Size == nil || f.SHA256 == nil {
			return fail("file %d misses a field", i)
		}
		r.Files[i] = ReceiptFile{RelativePath: *f.RelativePath, Size: *f.Size, SHA256: *f.SHA256}
	}
	if err := r.validate(); err != nil {
		return Receipt{}, err
	}
	if !bytes.Equal(r.encode(), data) {
		return fail("not in the canonical form")
	}
	return r, nil
}

// validate checks the content rules shared by Encode and ParseReceipt.
func (r Receipt) validate() error {
	fail := func(format string, args ...any) error {
		return errorf(CodeReceiptInvalid, "the receipt is not valid: "+format, args...)
	}
	switch {
	case r.AlbumID == uuid.Nil || r.BuildID == uuid.Nil:
		return fail("a nil album_id or build_id")
	case r.AlbumRevision <= 0:
		return fail("album_revision %d", r.AlbumRevision)
	case r.RenderVersion == "" || !utf8.ValidString(r.RenderVersion):
		return fail("an empty or invalid render_version")
	}
	for i, f := range r.Files {
		if err := checkOutputPath(f.RelativePath); err != nil {
			return fail("file %d: %v", i, err)
		}
		if f.RelativePath == ReceiptName {
			return fail("the receipt lists itself")
		}
		if strings.ContainsFunc(f.RelativePath, func(c rune) bool { return c < 0x20 }) {
			return fail("file %d: a control character in the path", i)
		}
		if f.Size < 0 {
			return fail("file %q: size %d", f.RelativePath, f.Size)
		}
		if blobstore.ValidateSHA(f.SHA256) != nil {
			return fail("file %q: sha256 %q", f.RelativePath, f.SHA256)
		}
		if i > 0 && r.Files[i-1].RelativePath >= f.RelativePath {
			return fail("files not sorted by path bytes, or a path twice: %q then %q", r.Files[i-1].RelativePath, f.RelativePath)
		}
	}
	return nil
}

// String names the receipt for logs.
func (r Receipt) String() string {
	return fmt.Sprintf("receipt of album %s revision %d build %s (%d files)", r.AlbumID, r.AlbumRevision, r.BuildID, len(r.Files))
}
