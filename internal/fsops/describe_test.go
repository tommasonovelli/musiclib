package fsops

import (
	"os"
	"path/filepath"
	"testing"
)

// Describe reads the identity of what was actually opened: equal to a
// Stat of the same path, and different once the path names another file
// (§7.1: a replacement between the walk and the open is seen).
func TestDescribe(t *testing.T) {
	r, dir := newRoot(t)
	writeAbs(t, filepath.Join(dir, "a"), "content")
	st, err := r.Stat("a")
	if err != nil {
		t.Fatal(err)
	}
	f, err := r.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := Describe(f)
	if err != nil {
		t.Fatal(err)
	}
	if fi != st {
		t.Errorf("Describe %+v, Stat %+v", fi, st)
	}
	// Replace the path: the open file keeps its identity, the path does not.
	if err := os.Rename(filepath.Join(dir, "a"), filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	writeAbs(t, filepath.Join(dir, "a"), "content")
	now, err := r.Stat("a")
	if err != nil {
		t.Fatal(err)
	}
	again, err := Describe(f)
	if err != nil {
		t.Fatal(err)
	}
	if again.Ino != st.Ino || now.Ino == st.Ino {
		t.Errorf("inodes: opened %d, described %d, new path %d", st.Ino, again.Ino, now.Ino)
	}
	mustClose(t, f)
	if _, err := Describe(f); Code(err) == "" {
		t.Errorf("Describe of a closed file: %v", err)
	}
}
