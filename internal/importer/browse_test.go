package importer

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"

	"musiclib/internal/fsops"
)

// The Import view's folder summaries (NOTES.md N-286), from a real
// directory on ext4: names and types only, nothing opened or followed.
func TestTallyOnDisk(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"CD1", "Scans"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A track whose content is not audio still counts as a track: the
	// summary reads names, the import reads content (§7.2).
	for _, f := range []string{"01 So What.FLAC", "02.mp3", "03.m4a", "cover.JPG", "back.png", "booklet.pdf", "rip.log", "noext", ".DS_Store"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("not audio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad\xff"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := fsops.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := src.Close(); err != nil {
			t.Error(err)
		}
	})
	ents, err := Browse(src, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Tally(ents), (Summary{Folders: 2, Tracks: 3, Images: 2, Others: 4, Skipped: 3}); got != want {
		t.Fatalf("Tally = %+v, want %+v", got, want)
	}
	empty, err := Browse(src, "Scans")
	if err != nil {
		t.Fatal(err)
	}
	if got := Tally(empty); got != (Summary{}) {
		t.Fatalf("an empty folder: %+v", got)
	}
}

// The display order: natural, case-insensitive through the §5.2 key, then
// by bytes.
func TestSortForDisplay(t *testing.T) {
	var ents []SourceEntry
	for _, n := range []string{"disc 10", "Disc 2", "abba", "ABBA", "Zappa", "Émile", "_x"} {
		ents = append(ents, SourceEntry{Name: n, Type: EntryDirectory})
	}
	SortForDisplay(ents)
	var got []string
	for _, e := range ents {
		got = append(got, e.Name)
	}
	if want := []string{"_x", "ABBA", "abba", "Disc 2", "disc 10", "Zappa", "Émile"}; !slices.Equal(got, want) {
		t.Fatalf("order %q, want %q", got, want)
	}
}
