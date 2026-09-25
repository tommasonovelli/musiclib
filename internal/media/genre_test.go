package media

import (
	"os"
	"path/filepath"
	"testing"
)

// MP3GenreWritable is the helper's rule (owner decision N-162), value by
// value: a write of the genre succeeds exactly when the predicate says so,
// and reads back as written; otherwise the helper refuses it.
func TestMP3GenreWritableIsTheHelpersRule(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := mp3Source(t, dir)
	for g, want := range map[string]bool{
		"Rock": true, "Rock (13)": true, "13 Songs": true, "(Rock": true, "(": true, ")": true, " 13": true, "13 ": true,
		"192": true, "200": true, "999": true, "1000": true, "0191": true, "-1": true, "1e2": true, "(Rock)x": false,
		"(Rock)": false, "(13)": false, "(RX)": false, "(CR)": false, "()": false, "(13)Pop": false, "((13) literal": false,
		"((x": false, "((": false, "13": false, "0": false, "000": false, "101": false, "191": false, "(999)": false,
	} {
		if got := MP3GenreWritable(g); got != want {
			t.Errorf("MP3GenreWritable(%q) = %v, want %v", g, got, want)
		}
		p := mp3At(t, t.TempDir(), "g.mp3", src)
		err := tools.WriteManagedTags(t.Context(), openRW(t, p), FormatMP3, TagValues{Genre: g}, nil)
		switch {
		case want && err != nil:
			t.Errorf("genre %q: the helper refused it: %v", g, err)
		case !want && Code(err) != CodeTagsInvalidRequest:
			t.Errorf("genre %q: the helper answered %v, not a refusal", g, err)
		case want:
			if got := inspectMP3(t, tools, p).Managed.Genre; len(got) != 1 || got[0] != g {
				t.Errorf("genre %q read back as %q", g, got)
			}
		}
	}
}

// The APE tag has a bound (N-163): a footer declaring more than 256 MiB is
// corrupt for the helper and refused by the decode window, before anything
// is read (a sparse file: nothing is allocated).
func TestMP3APETagBound(t *testing.T) {
	dir := t.TempDir()
	src := mp3Source(t, dir)
	for name, size := range map[string]int64{"just over": 256<<20 + 1, "far over": 1 << 31} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "big.mp3")
			f, err := os.Create(p)
			if err != nil {
				t.Fatal(err)
			}
			footer := apeTag(false)[:32]
			binary32 := func(b []byte, v uint32) { b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24) }
			binary32(footer[12:], uint32(size))
			if _, err := f.Write(src); err != nil {
				t.Fatal(err)
			}
			// The items area is a hole of size - 32 bytes.
			if _, err := f.WriteAt(footer, int64(len(src))+size-32); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			for name, tools := range helpers(t) {
				if _, err := tools.Inspect(t.Context(), open(t, p), FormatMP3); Code(err) != CodeTagsCorrupt {
					t.Errorf("%s: Inspect: %v", name, err)
				}
			}
			_, err = newTools(t).AudioDigest(t.Context(), open(t, p))
			wantCode(t, err, CodeDecode)
		})
	}
}

// An ID3v1 title, artist or album that is only the 30-character Latin-1
// truncation of the winning value agrees with it (N-164); anything else
// still conflicts.
func TestTagsMP3ID3v1Truncation(t *testing.T) {
	dir := t.TempDir()
	tools := newTools(t)
	src := mp3Source(t, dir)
	long := "Premières parties : l'ouverture du bal masqué" // no white space at the 30th character
	for _, tc := range []struct {
		name, full, v1 string
		conflict       bool
	}{
		{"truncated Latin-1", long, string([]rune(long)[:30]), false},
		{"truncated at a space", "Twenty-nine characters here!! and more", "Twenty-nine characters here!!", false},
		{"shorter and equal", "Short", "Short", false},
		{"another value", long, "Something else", true},
		{"a shorter prefix", long, string([]rune(long)[:29]), true},
		{"not Latin-1", "Björk ☃ " + long, "Björk ? " + string([]rune(long)[:22]), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mp3At(t, t.TempDir(), "f.mp3", id3Tag(4, 0, 0, frame("TIT2", textBody(encUTF8, tc.full))), src,
				id3v1(tc.v1, "", "", "", "", 0, 255))
			in := inspectMP3(t, tools, p)
			if got := len(in.Conflicts) > 0; got != tc.conflict {
				t.Fatalf("conflicts %+v", in.Conflicts)
			}
		})
	}
}
