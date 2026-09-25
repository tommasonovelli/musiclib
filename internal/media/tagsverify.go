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
//
//   - the managed fields, their aliases and the sort fields, which the
//     canonical form of Inspection.Unmanaged leaves out;
//
//   - the pictures, which are the managed cover (Inspection.Pictures);
//
//   - the ID3v2 and ID3v1 tags of a FLAC file, which a write strips by the
//     declared rule of NOTES.md N-090. They are never in Unmanaged: the
//     inspection reports them as opaque fields, reason foreign_tag, with
//     Removed set, so they are not Blocking; after must not have them, as
//     it has no opaque field at all.
//
//   - for MP3, the declared migration of §8.3 and nothing else: the key
//     "id3v1:comment" (the ID3v1 tag is removed) becomes, when no COMM frame
//     already holds that text, one more value of
//     "id3v2:COMM:XXX:legacy-id3v1" (migratedUnmanaged). The other ID3v1
//     fields and the APE items a write removes are managed fields, outside
//     Unmanaged by construction.
//
//   - for M4A, nothing beyond the first two points (NOTES.md N-167): the
//     numeric genre "gnre" and the freeform aliases are managed fields,
//     outside Unmanaged. What a write changes in the container (the free
//     padding next to the ilst, the size of moov, the position of the media
//     data after it, the chunk offsets) is outside the canonical form by
//     construction, and the audio stays covered by "mp4.samples", which is
//     read through the new offsets.
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
	wantUnmanaged := before.Unmanaged
	if before.Format == FormatMP3 {
		wantUnmanaged = migratedUnmanaged(before.Unmanaged)
	}
	if msg := diffUnmanaged(wantUnmanaged, after.Unmanaged); msg != "" {
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

// Keys of the ID3v1 migration of §8.3 (NOTES.md N-153): the comment of an
// ID3v1 tag, and the COMM frame (language "XXX", unknown; description
// "legacy-id3v1") the MP3 writer moves it to.
const (
	id3v1CommentKey = "id3v1:comment"
	commentPrefix   = "id3v2:COMM:"
	legacyComment   = commentPrefix + "XXX:legacy-id3v1"
)

// migratedUnmanaged is what the unmanaged fields of an MP3 must be after a
// write: before, without "id3v1:comment", whose text is added to the
// "legacy-id3v1" COMM frame unless a COMM frame already holds it (§8.3: "se
// identico a uno già presente non si duplica"). Nothing else changes.
func migratedUnmanaged(before []KeyValues) []KeyValues {
	var comment []string
	out := make([]KeyValues, 0, len(before)+1)
	for _, kv := range before {
		if kv.Key == id3v1CommentKey {
			comment = kv.Values
			continue
		}
		out = append(out, KeyValues{Key: kv.Key, Values: slices.Clone(kv.Values)})
	}
	for _, c := range comment {
		kept := false
		for _, kv := range out {
			if strings.HasPrefix(kv.Key, commentPrefix) && slices.Contains(kv.Values, c) {
				kept = true
			}
		}
		if kept {
			continue
		}
		i := slices.IndexFunc(out, func(kv KeyValues) bool { return kv.Key == legacyComment })
		if i < 0 {
			out = append(out, KeyValues{Key: legacyComment})
			i = len(out) - 1
		}
		out[i].Values = append(out[i].Values, c)
	}
	slices.SortFunc(out, func(a, b KeyValues) int { return strings.Compare(a.Key, b.Key) })
	return out
}
