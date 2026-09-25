package media

import (
	"strconv"
)

// flacMaxBlockLength is the largest FLAC metadata block, a 24-bit length
// (RFC 9639 §8.1; kMaxBlockLength in the helper).
const flacMaxBlockLength = 1<<24 - 1

// flacPictureHeader is the fixed part of a FLAC PICTURE block (RFC 9639
// §8.8): type, MIME length, description length, width, height, depth,
// colors and data length, 4 bytes each. The MIME type and the description
// follow; the writer's description is always empty (N-087).
const flacPictureHeader = 32

// id3v2MaxFrameBody is the largest ID3v2.4 frame body, a 28-bit synchsafe
// size (kMaxSynchsafe in the helper).
const id3v2MaxFrameBody = 1<<28 - 1

// apicOverhead is the fixed part of the APIC frame the writer makes: text
// encoding, the MIME terminator, picture type and the empty description's
// terminator (mp3.cpp, newFrames).
const apicOverhead = 4

// coverMIME is the MIME type WriteManagedTags embeds for each cover format
// (describeCover).
var coverMIME = map[string]string{FormatJPEG: "image/jpeg", FormatPNG: "image/png"}

// CoverMIME is the MIME type WriteManagedTags embeds for a cover of
// coverFormat ("jpeg" or "png"): the ExpectedCover.MIME that VerifyTags
// requires after the write. ok is false for any other format.
func CoverMIME(coverFormat string) (mime string, ok bool) {
	mime, ok = coverMIME[coverFormat]
	return mime, ok
}

// MaxEmbeddedCover returns the largest cover of format coverFormat ("jpeg"
// or "png") that WriteManagedTags can embed in an audio file of format
// audioFormat, in bytes. ok is false when the pair has no known limit: M4A,
// whose writer does not exist yet (N-094), or another cover format.
//
// For FLAC it is the helper's own bound (flac.cpp, writeFlac): a PICTURE
// block of 32 + len(MIME) + len(description) + len(data) bytes at most
// 16,777,215, with an empty description.
//
// For MP3 it is the bound of the APIC frame (mp3.cpp): a body of 4 +
// len(MIME) + len(data) bytes at most 268,435,455. The whole ID3v2 tag has
// the same bound, so a source tag already close to 256 MiB could still
// refuse the write (media_tags_too_large); §8.5 caps covers at 20 MiB, far
// below (NOTES.md N-155).
func MaxEmbeddedCover(audioFormat, coverFormat string) (int64, bool) {
	mime, ok := coverMIME[coverFormat]
	if !ok {
		return 0, false
	}
	switch audioFormat {
	case FormatFLAC:
		return flacMaxBlockLength - flacPictureHeader - int64(len(mime)), true
	case FormatMP3:
		return id3v2MaxFrameBody - apicOverhead - int64(len(mime)), true
	}
	return 0, false
}

// EmbeddedCoverFits is the owner's rule N-091 for one audio format: nil if
// a cover of coverFormat and size bytes can be embedded as the single front
// cover of an audio file of audioFormat (§8.2, §8.5), a CodeTagsTooLarge
// error saying why otherwise. A format without a known limit is refused
// (CodeTagsUnsupported): a cover is never accepted for a format whose writer
// does not exist yet.
func EmbeddedCoverFits(audioFormat, coverFormat string, size int64) error {
	const op = "cover limit"
	limit, ok := MaxEmbeddedCover(audioFormat, coverFormat)
	if !ok {
		return newErr(CodeTagsUnsupported, op,
			"no embedded cover limit is known for a "+coverFormat+" cover in "+audioFormat+" files", nil)
	}
	if size < 0 || size > limit {
		return newErr(CodeTagsTooLarge, op, "a "+coverFormat+" cover of "+strconv.FormatInt(size, 10)+
			" bytes does not fit in a "+audioFormat+" file, whose limit is "+strconv.FormatInt(limit, 10)+" bytes", nil)
	}
	return nil
}
