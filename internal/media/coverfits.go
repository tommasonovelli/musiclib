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
// audioFormat, in bytes. ok is false when the pair has no known limit: the
// audio formats of Phase 4 (N-094), or another cover format.
//
// For FLAC it is the helper's own bound (flac.cpp, writeFlac): a PICTURE
// block of 32 + len(MIME) + len(description) + len(data) bytes at most
// 16,777,215, with an empty description.
func MaxEmbeddedCover(audioFormat, coverFormat string) (int64, bool) {
	mime, ok := coverMIME[coverFormat]
	if !ok || audioFormat != FormatFLAC {
		return 0, false
	}
	return flacMaxBlockLength - flacPictureHeader - int64(len(mime)), true
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
