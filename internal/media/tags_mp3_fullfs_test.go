package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"musiclib/internal/faulttest"
)

// NOTES.md N-143: ENOSPC inside the helper's own write is its failure code
// no_space (media_tags_no_space), with the kernel's message, not the generic
// media_tags_io. On the really full filesystem of the tests (faulttest,
// docs/docker.md): an MP3 whose tag grows cannot be written when no block is
// free, and can when there is room.
func TestTagsMP3NoSpace(t *testing.T) {
	d := faulttest.FullFS(t)
	src := mp3Source(t, t.TempDir())
	tools := newTools(t)
	p := filepath.Join(d.Dir, "f.mp3")
	if err := os.WriteFile(p, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Fill(0); err != nil {
		t.Fatal(err)
	}
	err := tools.WriteManagedTags(t.Context(), openRW(t, p), FormatMP3, fullValues, nil)
	e := wantCode(t, err, CodeTagsNoSpace)
	if !strings.Contains(e.Msg, "No space left on device") {
		t.Fatalf("the kernel's message is lost: %q", e.Msg)
	}
	if err := d.Drain(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, src, 0o644); err != nil {
		t.Fatal(err)
	}
	writeMP3Checked(t, tools, p, fullValues, nil)
}
