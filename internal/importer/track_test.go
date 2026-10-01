package importer

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"musiclib/internal/blobstore"
	"musiclib/internal/jobs"
)

// ReadTrack is an import's reading of one track, for a file uploaded into
// an existing album: the same probe, full decode and tags, the same
// normalization, and a refusal for whatever is not audio.
func TestReadTrack(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pin := func(b []byte) blobstore.Blob {
		t.Helper()
		pb, err := e.blobs.Put(ctx, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return pb
	}

	good := track{tags: []string{"TITLE=Blue in Green", "ARTIST=Bill Evans", "ARTIST=Miles Davis", "GENRE=Jazz", "DISCNUMBER=2", "TRACKNUMBER=3"}}.flac(t)
	info, ws, err := e.im.ReadTrack(ctx, pin(good), "03 Blue in Green.flac")
	if err != nil || len(ws) != 0 {
		t.Fatalf("ReadTrack: %v %v", ws, err)
	}
	want := TrackInfo{Format: "flac", Duration: info.Duration, Title: "Blue in Green", Artist: "Bill Evans; Miles Davis", Genre: "Jazz", Disc: 2, No: 3}
	if info != want || info.Duration <= 0 || info.Duration > time.Minute {
		t.Fatalf("info %+v, want %+v", info, want)
	}

	// No tags: the title is the file name, no disc, no number.
	info, _, err = e.im.ReadTrack(ctx, pin(track{freq: 330}.flac(t)), "So What.flac")
	if err != nil || info.Title != "So What" || info.Disc != 0 || info.No != 0 || info.Artist != "" {
		t.Fatalf("untagged: %+v %v", info, err)
	}

	// An ID3 tag in a FLAC: accepted with the import's warning.
	_, ws, err = e.im.ReadTrack(ctx, pin(track{freq: 550, tags: []string{"TITLE=x"}, id3v2: true}.flac(t)), "x.flac")
	if err != nil || len(ws) != 1 || ws[0].Code != jobs.WarnFLACID3 {
		t.Fatalf("ID3: %v %v", ws, err)
	}

	// Refusals, as at the import; and what an import keeps as an
	// attachment is not a track.
	for name, c := range map[string]struct {
		body []byte
		code string
	}{
		"1.flac":    {good[:len(good)/2], CodeCorruptAudio},
		"text.mp3":  {[]byte("not audio at all"), CodeCorruptAudio},
		"notes.txt": {[]byte("not audio at all"), CodeUnsupportedAudio},
		"bad.flac":  {track{freq: 660, tags: []string{"COMMENT=caf\xe9"}}.flac(t), CodeUnrenderableTag},
	} {
		_, _, err := e.im.ReadTrack(ctx, pin(c.body), name)
		var ie *Error
		if !errors.As(err, &ie) || ie.Code != c.code || ie.Path != name {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
}
