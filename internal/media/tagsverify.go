package media

import (
	"slices"
	"strconv"
	"strings"
)

// ExpectedCover identifies the cover a write embedded: the image's MIME type
// and the size and SHA-256 of its bytes (the cover blob).
type ExpectedCover struct {
	MIME   string
	Size   int64
	SHA256 string
}

// VerifyTags is the check of §9.1 step 6 after a write: before is the
// inspection of the staging copy before WriteManagedTags, after the one of
// the same file afterwards. It requires that
//   - after holds exactly the managed values of want (absent fields absent),
//     with no conflicting alias left;
//   - after embeds exactly the expected cover as its one picture, a front
//     cover, or no picture at all when cover is nil;
//   - before has no opaque field that blocks a write (Inspection.Blocking):
//     such a field is not in Unmanaged, so its loss would go unseen; and
//     after has no opaque field at all;
//   - the unmanaged fields are exactly the same in both.
//
// The comparison excludes what a write may remove, and nothing else (§8.3):
//   - the managed fields, their aliases and the sort fields, which the
//     canonical form of Inspection.Unmanaged leaves out;
//   - the pictures, which are the managed cover (Inspection.Pictures);
//   - the ID3v2 and ID3v1 tags of a FLAC file, which a write strips by the
//     declared rule of NOTES.md N-090. They are never in Unmanaged: the
//     inspection reports them as opaque fields, reason foreign_tag, with
//     Removed set, so they are not Blocking; after must not have them, as
//     it has no opaque field at all.
//
// The migration of an ID3v1 comment (§8.3) concerns MP3 only and comes with
// the MP3 writer (Phase 4).
//
// Any difference is CodeTagsVerification: the album must not be published
// (§12.2, "Tag writer ... perde un tag non gestito").
func VerifyTags(want TagValues, cover *ExpectedCover, before, after Inspection) error {
	fail := func(msg string) error { return newErr(CodeTagsVerification, "verify tags", msg, nil) }
	if after.Format != before.Format {
		return fail("the inspections are of different formats")
	}
	// An opaque field is absent from Unmanaged on both sides, so the
	// comparison below cannot see it lost: a write of a file that had one it
	// does not remove anyway must have been refused (§8.3).
	if b := before.Blocking(); len(b) > 0 {
		return fail("the file had fields a write cannot keep: " + opaqueKeys(b))
	}
	if len(after.Opaque) > 0 {
		return fail("the written file has opaque fields: " + opaqueKeys(after.Opaque))
	}
	if len(after.Conflicts) > 0 {
		return fail("the written file has conflicting values for " + after.Conflicts[0].Field)
	}
	expected := expectedManaged(want)
	got := after.Managed
	gotFields := got.fields()
	for i, f := range expected.fields() {
		if !slices.Equal(*f.v, *gotFields[i].v) {
			return fail("the managed field " + f.name + " was not written as requested")
		}
	}
	if err := verifyCover(cover, after.Pictures); err != "" {
		return fail(err)
	}
	if msg := diffUnmanaged(before.Unmanaged, after.Unmanaged); msg != "" {
		return fail(msg)
	}
	return nil
}

// expectedManaged is how Inspect reports the values a write wrote.
func expectedManaged(v TagValues) ManagedTags {
	text := func(s string) []string {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	number := func(n int) []string {
		if n == 0 {
			return nil
		}
		return []string{strconv.Itoa(n)}
	}
	m := ManagedTags{
		Title: text(v.Title), Artist: text(v.Artist), AlbumArtist: text(v.AlbumArtist), Album: text(v.Album),
		Track: number(v.Track), TrackTotal: number(v.TrackTotal), Disc: number(v.Disc), DiscTotal: number(v.DiscTotal),
		Date: text(v.Date), Genre: text(v.Genre),
	}
	if v.Compilation {
		m.Compilation = []string{"1"}
	}
	return m
}

// verifyCover returns why the pictures are not the expected cover, or "".
func verifyCover(cover *ExpectedCover, pictures []Picture) string {
	if cover == nil {
		if len(pictures) > 0 {
			return strconv.Itoa(len(pictures)) + " picture(s) left where no cover was requested"
		}
		return ""
	}
	if len(pictures) != 1 {
		return strconv.Itoa(len(pictures)) + " pictures where exactly one cover was requested"
	}
	p := pictures[0]
	if p.Type != PictureFrontCover || p.MIME != cover.MIME || p.Size != cover.Size || p.SHA256 != cover.SHA256 {
		return "the embedded picture is not the requested front cover"
	}
	return ""
}

// diffUnmanaged names the first unmanaged key that was added, removed or
// changed, or returns "".
func diffUnmanaged(before, after []KeyValues) string {
	index := func(kvs []KeyValues) map[string][]string {
		m := make(map[string][]string, len(kvs))
		for _, kv := range kvs {
			m[kv.Key] = kv.Values
		}
		return m
	}
	b, a := index(before), index(after)
	for _, kv := range before {
		got, ok := a[kv.Key]
		switch {
		case !ok:
			return "the unmanaged field " + kv.Key + " was lost"
		case !slices.Equal(got, kv.Values):
			return "the unmanaged field " + kv.Key + " changed"
		}
	}
	for _, kv := range after {
		if _, ok := b[kv.Key]; !ok {
			return "the unmanaged field " + kv.Key + " appeared"
		}
	}
	if len(before) != len(after) {
		return "the unmanaged fields have duplicate keys"
	}
	return ""
}

func opaqueKeys(o []OpaqueField) string {
	keys := make([]string, len(o))
	for i, f := range o {
		keys[i] = f.Key + " (" + f.Reason + ")"
	}
	return strings.Join(keys, ", ")
}
