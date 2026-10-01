package importer

import (
	"context"
	"fmt"
	"time"

	"musiclib/internal/blobstore"
	"musiclib/internal/jobs"
)

// TrackInfo is one file read as a track of an existing album: what an
// import reads from each of its tracks, before the album's metadata is
// inferred. The texts are normalized as at the import ("" is an absent or
// empty tag).
type TrackInfo struct {
	// Format is the blobs.format value of the audio.
	Format string
	// Duration is what the container declares, 0 when it declares nothing
	// (unknown).
	Duration time.Duration
	// Title is the title tag, or the file name without its extension.
	Title  string
	Artist string
	Genre  string
	// Disc is the positive disc tag, 0 without one; No the positive track
	// number tag, 0 without one.
	Disc int
	No   int
}

// ReadTrack reads b, an uploaded file already pinned in the blob store,
// as one track, with exactly the checks of an import (readAudio: the
// probe, the complete decode, the tags and their warnings) and the same
// normalization of its tags (readTags). name is the file's name, used in
// messages, for the extension rule of readAudio and as the fallback title.
//
// A file that is not audio is refused: CodeCorruptAudio with a known audio
// extension, as at the import; CodeUnsupportedAudio without one, where an
// import would keep it as an attachment. It does no catalog write.
func (im *Importer) ReadTrack(ctx context.Context, b blobstore.Blob, name string) (TrackInfo, []jobs.Warning, error) {
	a, audio, ws, err := im.readAudio(ctx, b.SHA256, name)
	if err != nil {
		return TrackInfo{}, ws, err
	}
	if !audio {
		return TrackInfo{}, ws, &Error{Code: CodeUnsupportedAudio, Path: name,
			Message: fmt.Sprintf("%q is not an audio file: a track must be FLAC, MP3 or M4A (AAC or ALAC)", name)}
	}
	r, err := readTags(trackTags{Path: name, Tags: a.tags.Managed})
	if err != nil {
		return TrackInfo{}, ws, err
	}
	t := TrackInfo{Format: a.format, Duration: a.duration, Title: r.title, Artist: r.artist, Genre: r.genre, Disc: r.tagDisc}
	if r.trackOK {
		t.No = r.track
	}
	if t.Title == "" {
		if t.Title, err = basenameTitle(name); err != nil {
			return TrackInfo{}, ws, err
		}
	}
	return t, ws, nil
}
