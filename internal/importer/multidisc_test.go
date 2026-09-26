package importer

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
)

// discsOf maps every branch with disc directories to "rel=N" pairs, in the
// branch's order.
func discsOf(g grouping) map[string][]string {
	out := map[string][]string{}
	for _, b := range g.Branches {
		if b.Discs == nil {
			continue
		}
		var ds []string
		for _, d := range b.Discs {
			ds = append(ds, fmt.Sprintf("%s=%d", d.Dir.Rel, d.No))
		}
		out[b.Dir.Rel] = ds
	}
	return out
}

// §7.2 rules 2 and 3 on synthetic trees: every layout the grouping must
// pin, with the disc directories and their numbers ("!" marks audio, "@" a
// rejected entry, a trailing "/" an empty directory).
func TestGroupMultiDisc(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tree       []string
		branches   map[string]string
		discs      map[string][]string
		unassigned []string
		rejected   []string
		msg        []string // substrings of the only failed branch's message
	}{
		{name: "CD1 and CD2", tree: []string{"Box/CD1/1.flac!", "Box/CD2/1.flac!"},
			branches: map[string]string{"Box": "ok"}, discs: map[string][]string{"Box": {"Box/CD1=1", "Box/CD2=2"}}},
		{name: "Disc 1 and Disc 2", tree: []string{"Box/Disc 1/1.flac!", "Box/Disc 2/1.flac!"},
			branches: map[string]string{"Box": "ok"}, discs: map[string][]string{"Box": {"Box/Disc 1=1", "Box/Disc 2=2"}}},
		{name: "mixed case and both spellings", tree: []string{"Box/cd1/1.flac!", "Box/DISC 2/1.flac!", "Box/Cd3/1.flac!"},
			branches: map[string]string{"Box": "ok"}, discs: map[string][]string{"Box": {"Box/Cd3=3", "Box/DISC 2=2", "Box/cd1=1"}}},
		{name: "leading zeros", tree: []string{"Box/CD01/1.flac!", "Box/Disc 002/1.flac!", "Box/CD0010/1.flac!"},
			branches: map[string]string{"Box": "ok"}, discs: map[string][]string{"Box": {"Box/CD0010=10", "Box/CD01=1", "Box/Disc 002=2"}}},
		{name: "non-contiguous discs", tree: []string{"Box/CD1/1.flac!", "Box/CD3/1.flac!"},
			branches: map[string]string{"Box": "ok"}, discs: map[string][]string{"Box": {"Box/CD1=1", "Box/CD3=3"}}},
		{name: "a single CD1", tree: []string{"Box/CD1/1.flac!", "Box/CD1/2.flac!"},
			branches: map[string]string{"Box": "ok"}, discs: map[string][]string{"Box": {"Box/CD1=1"}}},
		{name: "disc 99", tree: []string{"Box/CD99/1.flac!"},
			branches: map[string]string{"Box": "ok"}, discs: map[string][]string{"Box": {"Box/CD99=99"}}},
		{name: "the batch root is the multi-disc album", tree: []string{"CD1/1.flac!", "CD2/1.flac!", "cover.jpg"},
			branches: map[string]string{"": "ok"}, discs: map[string][]string{"": {"CD1=1", "CD2=2"}}},

		// Rule 3: duplicate numbers fail the whole branch, whatever the
		// spelling, with or without audio in the duplicate (N-183).
		{name: "duplicate CD1 and CD01", tree: []string{"Box/CD1/1.flac!", "Box/CD01/1.flac!", "Box/CD2/1.flac!"},
			branches: map[string]string{"Box": CodeDuplicateDisc}, discs: map[string][]string{"Box": {"Box/CD01=1", "Box/CD1=1", "Box/CD2=2"}},
			msg: []string{`"base/Box/CD01"`, `"base/Box/CD1"`, "disc 1"}},
		{name: "duplicate CD1 and Disc 1", tree: []string{"Box/CD1/1.flac!", "Box/Disc 1/1.flac!"},
			branches: map[string]string{"Box": CodeDuplicateDisc}, discs: map[string][]string{"Box": {"Box/CD1=1", "Box/Disc 1=1"}},
			msg: []string{`"base/Box/CD1"`, `"base/Box/Disc 1"`}},
		{name: "duplicate cd2 and CD2, case only", tree: []string{"Box/CD2/1.flac!", "Box/cd2/1.flac!"},
			branches: map[string]string{"Box": CodeDuplicateDisc}, discs: map[string][]string{"Box": {"Box/CD2=2", "Box/cd2=2"}}},
		{name: "duplicate without audio (owner decision, N-183)", tree: []string{"Box/CD1/1.flac!", "Box/CD01/scan.jpg"},
			branches: map[string]string{"Box": CodeDuplicateDisc}, discs: map[string][]string{"Box": {"Box/CD01=1", "Box/CD1=1"}}},
		{name: "CD100 is beyond the schema", tree: []string{"Box/CD1/1.flac!", "Box/CD100/1.flac!"},
			branches: map[string]string{"Box": catalog.CodeInvalidDisc}, discs: map[string][]string{"Box": {"Box/CD1=1", "Box/CD100=100"}},
			msg: []string{`"base/Box/CD100"`, "99"}},
		// A disc-named directory without audio is a disc directory (N-183,
		// owner decision): over 99 it fails the branch too.
		{name: "CD100 without audio is beyond the schema", tree: []string{"Box/CD1/1.flac!", "Box/CD100/scans/x.jpg"},
			branches: map[string]string{"Box": catalog.CodeInvalidDisc}, discs: map[string][]string{"Box": {"Box/CD1=1", "Box/CD100=100"}},
			msg: []string{`"base/Box/CD100"`, "99"}},
		{name: "a disc number too large for an int", tree: []string{"Box/CD99999999999999999999999/1.flac!"},
			branches: map[string]string{"Box": catalog.CodeInvalidDisc}},

		// Not disc names: ordinary directories, rule 5 applies (N-120).
		{name: "CD0 is not a disc", tree: []string{"Box/CD0/1.flac!"}, branches: map[string]string{"Box/CD0": "ok"}},
		{name: "CD 1 is not a disc", tree: []string{"Box/CD 1/1.flac!", "Box/CD 2/1.flac!"},
			branches: map[string]string{"Box/CD 1": "ok", "Box/CD 2": "ok"}},
		{name: "Disc1 is not a disc", tree: []string{"Box/Disc1/1.flac!", "Box/Disc2/1.flac!"},
			branches: map[string]string{"Box/Disc1": "ok", "Box/Disc2": "ok"}},

		// A non-disc sibling with audio: rule 5, as N-120's example; with
		// CD1 and CD2, three independent albums (owner decision, N-184).
		{name: "Box/CD1 next to Box/Bonus", tree: []string{"Box/CD1/1.flac!", "Box/Bonus/1.flac!", "Box/notes.txt"},
			branches: map[string]string{"Box/CD1": "ok", "Box/Bonus": "ok"}, unassigned: []string{"Box/notes.txt"}},
		{name: "Box/CD1 and Box/CD2 next to Box/Bonus", tree: []string{"Box/CD1/1.flac!", "Box/CD2/1.flac!", "Box/Bonus/1.flac!"},
			branches: map[string]string{"Box/CD1": "ok", "Box/CD2": "ok", "Box/Bonus": "ok"}, discs: map[string][]string{}},
		// Siblings without audio and root files are the candidate's.
		{name: "a sibling without audio is an attachment", tree: []string{"Box/CD1/1.flac!", "Box/CD2/1.flac!",
			"Box/Artwork/front.jpg", "Box/Artwork/Inner/back.png", "Box/cover.jpg", "Box/booklet.pdf", "Box/rip.cue", "Box/rip.log",
			"Box/CD1/Scans/disc.jpg", "Box/CD2/booklet.pdf"},
			branches: map[string]string{"Box": "ok"}, discs: map[string][]string{"Box": {"Box/CD1=1", "Box/CD2=2"}}},
		{name: "a disc directory without audio is kept", tree: []string{"Box/CD1/1.flac!", "Box/CD2/scan.jpg"},
			branches: map[string]string{"Box": "ok"}, discs: map[string][]string{"Box": {"Box/CD1=1", "Box/CD2=2"}}},

		// Audio below a disc directory when at least one disc directory has
		// direct audio: the whole branch is ambiguous, naming the misplaced
		// file (N-184, owner decision (b′)).
		{name: "audio below a disc directory (example A)", tree: []string{"Box/CD1/1.flac!", "Box/CD2/Bonus/x.flac!"},
			branches: map[string]string{"Box": CodeAmbiguousCandidate}, discs: map[string][]string{"Box": {"Box/CD1=1", "Box/CD2=2"}},
			msg: []string{`"base/Box/CD2/Bonus/x.flac"`, `"base/Box/CD2"`, "move the audio files into the disc directory"}},
		{name: "audio in and below a disc directory", tree: []string{"Box/CD1/1.flac!", "Box/CD1/Bonus/1.flac!", "Box/CD2/1.flac!"},
			branches: map[string]string{"Box": CodeAmbiguousCandidate}, discs: map[string][]string{"Box": {"Box/CD1=1", "Box/CD2=2"}},
			msg: []string{`"base/Box/CD1/Bonus/1.flac"`, `"base/Box/CD1"`}},
		{name: "one disc direct, another only nested", tree: []string{"Box/CD1/1.flac!", "Box/CD1/2.flac!",
			"Box/Disc 2/Side A/1.flac!", "Box/Disc 2/Side B/1.flac!"},
			branches: map[string]string{"Box": CodeAmbiguousCandidate}, discs: map[string][]string{"Box": {"Box/CD1=1", "Box/Disc 2=2"}},
			msg: []string{`"base/Box/Disc 2/Side A/1.flac"`, `"base/Box/Disc 2"`}},
		// No disc directory has direct audio: not rule-2 shaped, rule 5
		// finds the nested albums as in Phase 2 (N-184).
		{name: "only audio below the disc directory", tree: []string{"Box/CD1/sub/x.flac!"},
			branches: map[string]string{"Box/CD1/sub": "ok"}, discs: map[string][]string{}},
		{name: "a data-CD backup (example B)", tree: []string{"Rips/CD1/Artist - Album/1.mp3!", "Rips/CD1/Artist - Album/2.mp3!",
			"Rips/CD2/Other/1.mp3!", "Rips/CD2/readme.txt", "Rips/CD3/scan.jpg"},
			branches: map[string]string{"Rips/CD1/Artist - Album": "ok", "Rips/CD2/Other": "ok"}, discs: map[string][]string{},
			unassigned: []string{"Rips/CD2/readme.txt", "Rips/CD3/scan.jpg"}},
		// Without direct audio in a disc, the duplicate and over-99 checks
		// do not apply: they belong to a rule-2 shaped directory only.
		{name: "no direct audio: duplicate names are ordinary", tree: []string{"Rips/CD1/A/1.flac!", "Rips/CD01/B/1.flac!"},
			branches: map[string]string{"Rips/CD1/A": "ok", "Rips/CD01/B": "ok"}, discs: map[string][]string{}},
		{name: "no direct audio: CD100 is ordinary", tree: []string{"Rips/CD100/A/1.flac!"},
			branches: map[string]string{"Rips/CD100/A": "ok"}, discs: map[string][]string{}},
		// Direct audio and disc directories: rule 4.
		{name: "direct audio plus disc directories", tree: []string{"Box/intro.flac!", "Box/CD1/1.flac!", "Box/CD2/1.flac!"},
			branches: map[string]string{"Box": CodeAmbiguousCandidate}},

		// Nested in a larger tree next to single-disc albums.
		{name: "nested next to single-disc albums", tree: []string{"Music/A/1.flac!", "Music/Sets/Box/CD1/1.flac!",
			"Music/Sets/Box/CD2/1.flac!", "Music/Sets/Other/Disc 1/1.flac!", "Music/Sets/Single/1.flac!", "Music/readme.txt",
			"Music/Sets/Box/.DS_Store"},
			branches: map[string]string{"Music/A": "ok", "Music/Sets/Box": "ok", "Music/Sets/Other": "ok", "Music/Sets/Single": "ok"},
			discs: map[string][]string{"Music/Sets/Box": {"Music/Sets/Box/CD1=1", "Music/Sets/Box/CD2=2"},
				"Music/Sets/Other": {"Music/Sets/Other/Disc 1=1"}},
			unassigned: []string{"Music/readme.txt"}},
		// A symlink inside fails the multi-disc candidate, outside it is
		// reported.
		{name: "rejected entry inside a disc", tree: []string{"Box/CD1/1.flac!", "Box/CD2/link@"},
			branches: map[string]string{"Box": CodeSourceRejected}, discs: map[string][]string{"Box": {"Box/CD1=1", "Box/CD2=2"}}},
		{name: "rejected entry and files outside", tree: []string{"link@", "loose.txt", "Box/CD1/1.flac!"},
			branches: map[string]string{"Box": "ok"}, discs: map[string][]string{"Box": {"Box/CD1=1"}},
			unassigned: []string{"loose.txt"}, rejected: []string{"link"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := group(dirOf(tc.tree...), "base")
			b, u, r := outcome(g)
			if !reflect.DeepEqual(b, tc.branches) || !reflect.DeepEqual(u, tc.unassigned) || !reflect.DeepEqual(r, tc.rejected) {
				t.Errorf("branches %v unassigned %v rejected %v\nwant     %v unassigned %v rejected %v", b, u, r, tc.branches, tc.unassigned, tc.rejected)
			}
			want := tc.discs
			if want == nil {
				want = map[string][]string{}
			}
			if got := discsOf(g); tc.discs != nil && !reflect.DeepEqual(got, want) {
				t.Errorf("discs %v, want %v", got, want)
			}
			for _, br := range g.Branches {
				if br.Err == nil {
					continue
				}
				if br.Err.Code != CodeSourceRejected && br.Err.Path != br.Dir.Rel {
					t.Errorf("error path %q, want %q", br.Err.Path, br.Dir.Rel)
				}
				for _, s := range tc.msg {
					if !strings.Contains(br.Err.Message, s) {
						t.Errorf("message %q does not contain %s", br.Err.Message, s)
					}
				}
			}
		})
	}
}

// The limits of §7.2 count every disc of a multi-disc candidate: 10,000
// files and 1,000 tracks over all its discs, not per disc.
func TestGroupMultiDiscLimits(t *testing.T) {
	var tracks []string
	for i := range MaxTracks {
		tracks = append(tracks, fmt.Sprintf("Box/CD%d/%d.flac!", 1+i%2, i))
	}
	if b, _, _ := outcome(group(dirOf(tracks...), "")); b["Box"] != "ok" {
		t.Errorf("exactly %d tracks over two discs: %v", MaxTracks, b)
	}
	if b, _, _ := outcome(group(dirOf(append(tracks, "Box/CD2/x.flac!")...), "")); b["Box"] != catalog.CodeTooManyFiles {
		t.Errorf("%d tracks over two discs: %v", MaxTracks+1, b)
	}
	files := []string{"Box/CD1/1.flac!", "Box/CD2/1.flac!"}
	for i := range MaxFiles - 2 {
		files = append(files, fmt.Sprintf("%s/%d.txt", []string{"Box/CD1", "Box/CD2/Scans", "Box/Art"}[i%3], i))
	}
	if b, _, _ := outcome(group(dirOf(files...), "")); b["Box"] != "ok" {
		t.Errorf("exactly %d files: %v", MaxFiles, b)
	}
	if b, _, _ := outcome(group(dirOf(append(files, "Box/one-more.txt")...), "")); b["Box"] != catalog.CodeTooManyFiles {
		t.Errorf("%d files: %v", MaxFiles+1, b)
	}
}

// §7.2 rule 2's names, and their numbers.
func TestDiscNumber(t *testing.T) {
	for name, want := range map[string]int{
		"CD1": 1, "cd01": 1, "Cd12": 12, "Disc 1": 1, "DISC 007": 7, "disc 3": 3, "CD99": 99, "CD100": 100, "Disc 0100": 100,
		"CD 1": 0, "Disc1": 0, "CD0": 0, "Disc 00": 0, "CD": 0, "Disc ": 0, "CD1a": 0, "CD-1": 0, "diſc 1": 0,
		"Disc  1": 0, "CD１": 0, "CD+1": 0, " CD1": 0, "CD1 ": 0, "Disc-1": 0, "Disc_2": 0, "Disc.3": 0, "Discs 1": 0,
		// Only A-Z are folded: Go lowercases U+0130 to "i".
		"Dİsc 1": 0, "İ": 0,
	} {
		got, ok := discNumber(name)
		if ok != (want != 0) || (ok && got != want) {
			t.Errorf("discNumber(%q) = %d, %v; want %d", name, got, ok, want)
		}
	}
	if n, ok := discNumber("CD99999999999999999999999"); !ok || n <= catalog.MaxDisc {
		t.Errorf("a huge number: %d %v", n, ok)
	}
}

// The path of tracks_renumbered is the disc directory of the renumbered
// disc in a multi-disc candidate, as disc_tag_ignored's, and stays empty
// for a disc without a disc directory.
func TestRenumberedWarningPath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tracks []trackTags
		want   []string
	}{
		{"multi-disc: only the renumbered disc", []trackTags{
			td(1, "CD1/a", "TRACKNUMBER=1"), td(1, "CD1/b", "TRACKNUMBER=2"),
			td(2, "Disc 02/a"), td(2, "Disc 02/b", "TRACKNUMBER=1")},
			[]string{"tracks_renumbered Disc 02"}},
		{"multi-disc: two discs renumbered, by disc", []trackTags{
			td(1, "CD1/a", "TRACKNUMBER=0"), td(2, "CD2/a")},
			[]string{"tracks_renumbered CD1", "tracks_renumbered CD2"}},
		{"single disc, no disc directory", []trackTags{tt("a"), tt("b", "TRACKNUMBER=1")},
			[]string{"tracks_renumbered "}},
		{"discs from tags, no disc directory", []trackTags{
			tt("a", "DISCNUMBER=2"), tt("b", "DISCNUMBER=1")},
			[]string{"tracks_renumbered ", "tracks_renumbered "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := inferMetadata("Box", tc.tracks, jobs.Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, w := range m.Warnings {
				if w.Code == jobs.WarnTracksRenumbered {
					got = append(got, string(w.Code)+" "+w.Path)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("warnings %q, want %q", got, tc.want)
			}
		})
	}
}
