package catalog

import "testing"

// landing is the one placement rule of a track added to an album: the
// place its tags ask for when it is valid and free, otherwise the end of
// the album's last disc.
func TestLanding(t *testing.T) {
	slots := func(places ...[2]int32) Slots {
		s := Slots{}
		for _, p := range places {
			s[p] = true
		}
		return s
	}
	for _, c := range []struct {
		name             string
		occupied         Slots
		wantDisc, wantNo int
		disc, no         int32
	}{
		{"free place kept", slots([2]int32{1, 1}, [2]int32{1, 2}), 1, 5, 1, 5},
		{"free place on another disc kept", slots([2]int32{1, 1}), 3, 1, 3, 1},
		{"a number without a disc is disc 1", slots([2]int32{1, 1}, [2]int32{2, 1}), 0, 4, 1, 4},
		{"taken place appended", slots([2]int32{1, 1}, [2]int32{1, 2}), 1, 2, 1, 3},
		{"no number appended", slots([2]int32{1, 1}, [2]int32{1, 7}), 0, 0, 1, 8},
		{"a disc without a number appended", slots([2]int32{1, 1}), 2, 0, 1, 2},
		{"appended to the last disc", slots([2]int32{1, 1}, [2]int32{1, 9}, [2]int32{2, 3}), 1, 1, 2, 4},
		{"disc out of range appended", slots([2]int32{1, 1}), MaxDisc + 1, 1, 1, 2},
		{"number out of range appended", slots([2]int32{1, 1}), 1, MaxTrackNumber + 1, 1, 2},
		{"negative appended", slots([2]int32{1, 1}), -1, -1, 1, 2},
		{"empty album", slots(), 0, 0, 1, 1},
		{"empty album keeps the place", slots(), 2, 7, 2, 7},
		{"highest number", slots([2]int32{1, MaxTrackNumber - 1}), 0, 0, 1, MaxTrackNumber},
	} {
		disc, no, err := landing(c.occupied, c.wantDisc, c.wantNo)
		if err != nil || disc != c.disc || no != c.no {
			t.Errorf("%s: landing(%v, %d, %d) = %d, %d, %v; want %d, %d", c.name, c.occupied, c.wantDisc, c.wantNo, disc, no, err, c.disc, c.no)
		}
	}
	// A last disc numbered up to the end: nowhere to append, even if an
	// earlier disc has room.
	_, _, err := landing(slots([2]int32{1, 1}, [2]int32{2, MaxTrackNumber}), 2, MaxTrackNumber)
	if Code(err) != CodeInvalidTrackNumber {
		t.Fatalf("full disc: %v", err)
	}
	// Several tracks, one after the other.
	occupied := slots([2]int32{1, 1})
	for i, want := range []int32{2, 3, 4} {
		disc, no, err := landing(occupied, 1, 1)
		if err != nil || disc != 1 || no != want {
			t.Fatalf("track %d: %d, %d, %v", i, disc, no, err)
		}
		occupied[[2]int32{disc, no}] = true
	}
}
