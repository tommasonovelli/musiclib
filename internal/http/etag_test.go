package http

import (
	nethttp "net/http"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
)

func TestETagForm(t *testing.T) {
	id := uuid.MustParse("01920000-0000-7000-8000-00000000abcd")
	if got, want := ETag(KindAlbum, id, 12), `"album:01920000-0000-7000-8000-00000000abcd:12"`; got != want {
		t.Fatalf("ETag = %s, want %s", got, want)
	}
	if got, want := ETag(KindArtist, id, 1), `"artist:01920000-0000-7000-8000-00000000abcd:1"`; got != want {
		t.Fatalf("ETag = %s, want %s", got, want)
	}
}

// §10.1 and NOTES.md N-147: every If-Match form and what it becomes.
func TestIfMatch(t *testing.T) {
	id := uuid.MustParse("01920000-0000-7000-8000-00000000abcd")
	other := uuid.MustParse("01920000-0000-7000-8000-00000000ffff")
	mine := func(rev int64) string { return ETag(KindAlbum, id, rev) }
	const (
		required = nethttp.StatusPreconditionRequired
		invalid  = nethttp.StatusBadRequest
	)
	for _, tc := range []struct {
		name   string
		lines  []string // nil: no If-Match at all
		rev    int64
		status int // 0: accepted with rev
	}{
		{"absent", nil, 0, required},
		{"empty", []string{""}, 0, required},
		{"only commas", []string{" , ,"}, 0, required},
		{"star", []string{"*"}, 0, required},
		{"star with spaces", []string{"  *  "}, 0, required},
		{"star in a list", []string{`*, ` + mine(3)}, 0, invalid},
		{"exact", []string{mine(3)}, 3, 0},
		{"large revision", []string{mine(9223372036854775807)}, 9223372036854775807, 0},
		{"with whitespace", []string{" \t" + mine(3) + " "}, 3, 0},
		{"in a list", []string{ETag(KindAlbum, other, 3) + ", " + mine(4) + ` , "x"`}, 4, 0},
		{"split over two lines", []string{ETag(KindArtist, id, 9), mine(5)}, 5, 0},
		{"empty elements", []string{", " + mine(6) + ",,"}, 6, 0},
		{"weak", []string{"W/" + mine(3)}, noMatch, 0},
		{"another album", []string{ETag(KindAlbum, other, 3)}, noMatch, 0},
		{"the artist with the same id", []string{ETag(KindArtist, id, 3)}, noMatch, 0},
		{"a foreign tag", []string{`"abc"`}, noMatch, 0},
		{"empty tag", []string{`""`}, noMatch, 0},
		{"revision zero", []string{mine(0)}, noMatch, 0},
		{"negative revision", []string{`"album:` + id.String() + `:-1"`}, noMatch, 0},
		{"leading zero", []string{`"album:` + id.String() + `:03"`}, noMatch, 0},
		{"plus sign", []string{`"album:` + id.String() + `:+3"`}, noMatch, 0},
		{"overflow", []string{`"album:` + id.String() + `:9223372036854775808"`}, noMatch, 0},
		{"uppercase id", []string{`"album:01920000-0000-7000-8000-00000000ABCD:3"`}, noMatch, 0},
		{"uppercase kind", []string{`"ALBUM:` + id.String() + `:3"`}, noMatch, 0},
		{"trailing text", []string{`"album:` + id.String() + `:3:x"`}, noMatch, 0},
		{"twice, same revision", []string{mine(3) + ", " + mine(3)}, 0, invalid},
		{"twice, different revisions", []string{mine(3) + ", " + mine(4)}, 0, invalid},
		{"weak and strong", []string{"W/" + mine(3) + ", " + mine(4)}, 4, 0},
		{"unquoted", []string{"album:" + id.String() + ":3"}, 0, invalid},
		{"lowercase weak prefix", []string{"w/" + mine(3)}, 0, invalid},
		{"unterminated", []string{`"album:` + id.String() + `:3`}, 0, invalid},
		{"quote inside", []string{`"a"b"`}, 0, invalid},
		{"space inside", []string{`"a b"`}, 0, invalid},
		{"control inside", []string{"\"a\x01b\""}, 0, invalid},
		{"DEL inside", []string{"\"a\x7fb\""}, 0, invalid},
		{"obs-text accepted", []string{"\"\xe9\", " + mine(2)}, 2, 0},
		{"garbage after a tag", []string{mine(3) + " x"}, 0, invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := nethttp.Header{}
			for _, l := range tc.lines {
				h.Add("If-Match", l)
			}
			rev, e := ifMatch(h, KindAlbum, id)
			if tc.status != 0 {
				if e == nil || e.Status != tc.status {
					t.Fatalf("ifMatch = %d, %v; want status %d", rev, e, tc.status)
				}
				if tc.status == required && e.Code != catalog.CodePreconditionRequired {
					t.Fatalf("code %s", e.Code)
				}
				if tc.status == invalid && e.Code != CodeInvalidIfMatch {
					t.Fatalf("code %s", e.Code)
				}
				return
			}
			if e != nil || rev != tc.rev {
				t.Fatalf("ifMatch = %d, %v; want %d", rev, e, tc.rev)
			}
		})
	}
}
