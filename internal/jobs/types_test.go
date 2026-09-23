package jobs

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// jobs.warnings is validated by a closed Go type (§4.2).
func TestWarnings(t *testing.T) {
	b, err := EncodeWarnings(nil)
	if err != nil || string(b) != "[]" {
		t.Fatalf("EncodeWarnings(nil) = %s, %v; want []", b, err)
	}
	good := []Warning{
		{Code: WarnTracksRenumbered, Message: "disc 1 numbered by name"},
		{Code: WarnUnassignedFile, Message: "outside every candidate", Path: "a/b.txt"},
	}
	b, err = EncodeWarnings(good)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeWarnings(b)
	if err != nil || len(back) != 2 || back[0] != good[0] || back[1] != good[1] {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	for _, w := range []Warning{
		{Code: "free_text", Message: "m"},
		{Code: WarnTagConflict, Message: ""},
		{Code: WarnTagConflict, Message: strings.Repeat("x", maxWarningMessage+1)},
		{Code: WarnTagConflict, Message: "bad \xff"},
		{Code: WarnUnassignedFile, Message: "m", Path: "/abs"},
		{Code: WarnUnassignedFile, Message: "m", Path: "a/../b"},
	} {
		if _, err := EncodeWarnings([]Warning{w}); Code(err) != CodeInvalidResult {
			t.Errorf("EncodeWarnings(%+v) = %v, want %s", w, err, CodeInvalidResult)
		}
	}
	for _, raw := range []string{
		`null`, `{}`, `[{"code":"tag_conflict","message":"m","extra":1}]`, `[] []`,
		`[{"code":"nope","message":"m"}]`, `[{"code":"tag_conflict"}]`, `not json`,
	} {
		if _, err := DecodeWarnings([]byte(raw)); Code(err) != CodeInvalidResult {
			t.Errorf("DecodeWarnings(%s) = %v, want %s", raw, err, CodeInvalidResult)
		}
	}
}

// jobs.overrides: only artist and title (§7.3); Encode, the only writer,
// wants normalized texts; the decode checks the closed shape (N-106).
func TestOverrides(t *testing.T) {
	for _, tc := range []struct {
		o    Overrides
		want string
	}{
		{Overrides{}, `{}`},
		{Overrides{Artist: ptr("ABBA")}, `{"artist":"ABBA"}`},
		{Overrides{Artist: ptr("A"), Title: ptr("B")}, `{"artist":"A","title":"B"}`},
	} {
		b, err := tc.o.Encode()
		if err != nil || string(b) != tc.want {
			t.Errorf("Encode(%+v) = %s, %v; want %s", tc.o, b, err, tc.want)
		}
		back, err := DecodeOverrides(b)
		if err != nil || !eqPtr(back.Artist, tc.o.Artist) || !eqPtr(back.Title, tc.o.Title) {
			t.Errorf("DecodeOverrides(%s) = %+v, %v", b, back, err)
		}
	}
	for _, o := range []Overrides{
		{Artist: ptr("")}, {Title: ptr(" padded ")}, {Title: ptr("é")}, {Artist: ptr("bell\a")},
		{Title: ptr(strings.Repeat("x", 1025))},
	} {
		if _, err := o.Encode(); Code(err) != CodeInvalidOverrides {
			t.Errorf("Encode(%+v) = %v, want %s", o, err, CodeInvalidOverrides)
		}
	}
	for _, raw := range []string{`{"artist":1}`, `{"album":"x"}`, `{} {}`, `[]`, `"x"`} {
		if _, err := DecodeOverrides([]byte(raw)); Code(err) != CodeInvalidOverrides {
			t.Errorf("DecodeOverrides(%s) = %v, want %s", raw, err, CodeInvalidOverrides)
		}
	}
	// A value the claim must not refuse: it would block the queue.
	if o, err := DecodeOverrides([]byte(`{"title":" padded "}`)); err != nil || *o.Title != " padded " {
		t.Errorf("DecodeOverrides of a non-normalized value = %+v, %v; want it decoded", o, err)
	}
}

func TestClipMessage(t *testing.T) {
	if got := clipMessage("short"); got != "short" {
		t.Errorf("clipMessage(short) = %q", got)
	}
	if got := clipMessage("a\xffb"); got != "a�b" {
		t.Errorf("invalid UTF-8 kept: %q", got)
	}
	long := strings.Repeat("€", maxErrorMessage) // 3 bytes each
	got := clipMessage(long)
	if len(got) > maxErrorMessage || !utf8.ValidString(got) || !strings.HasPrefix(long, got) || len(got) < maxErrorMessage-2 {
		t.Errorf("clipped to %d bytes (valid %v)", len(got), utf8.ValidString(got))
	}
}

func eqPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
