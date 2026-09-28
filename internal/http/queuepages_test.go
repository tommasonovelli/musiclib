package http

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/jobs"
)

// The server side of the Import and Activity pages (round 21, NOTES.md
// N-286 to N-290): words, the problem table, the first open tab, escaping.

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 9, 28, 14, 3, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		-5 * time.Second:               "just now",
		59 * time.Second:               "just now",
		time.Minute:                    "1 minute ago",
		2*time.Minute + 59*time.Second: "2 minutes ago",
		time.Hour:                      "1 hour ago",
		23 * time.Hour:                 "23 hours ago",
		24 * time.Hour:                 "1 day ago",
		29 * 24 * time.Hour:            "29 days ago",
		30 * 24 * time.Hour:            "29 August 2026",
	} {
		if got := relativeTime(now.Add(-d), now); got != want {
			t.Errorf("%v ago: %q, want %q", d, got, want)
		}
	}
	if got := timeOf(now, now); got.ISO != "2026-09-28T14:03:00Z" || got.Full != "28 September 2026 at 14:03 UTC" {
		t.Errorf("timeOf: %+v", got)
	}
}

// Retry is offered only where trying again can work (N-287): the owner's
// own failures among them.
func TestJobProblems(t *testing.T) {
	code := func(s string) *string { return &s }
	for c, fix := range map[string]string{
		"mixed_album": fixTitle, "album_title_missing": fixTitle, "album_folder_conflict": fixTitle, "path_reserved": fixTitle,
		"ambiguous_album_artist": fixArtist, "artist_folder_conflict": fixArtist,
		"ambiguous_candidate": "", "not_a_candidate": "", "no_valid_candidate": "", "corrupt_audio": "", "unsupported_audio": "",
		"duplicate_disc": "", "too_many_files": "", "source_rejected_entry": "", "invalid_tag": "",
		"source_not_found": fixRetry, "source_changed": fixRetry, "insufficient_space": fixRetry, "source_not_readable": fixRetry,
		"media_timeout": fixRetry, "catalog_db": fixRetry,
	} {
		p := jobProblem(jobs.KindImport, code(c))
		if p.Fix != fix || p.Sentence == "" || strings.Contains(p.Sentence, "_") {
			t.Errorf("%s: %+v, want the fix %q and a sentence without a code", c, p, fix)
		}
	}
	if p := jobProblem(jobs.KindScan, nil); p.Fix != fixRetry || !strings.HasPrefix(p.Sentence, "Looking for albums") {
		t.Errorf("a scan without a code: %+v", p)
	}
	if s := renderProblem(code("render_io")); !strings.Contains(s, "library folder") {
		t.Errorf("render problem %q", s)
	}
}

func TestImportPageTabsAndStates(t *testing.T) {
	e := pageEnv(t)
	album := e.seed("Miles Davis", "Kind of Blue")
	open := func(path string) string {
		t.Helper()
		status, _, body := pageRequest(t, e, path, testHost)
		if status != 200 {
			t.Fatalf("%s: %d %s", path, status, body)
		}
		return body
	}
	selected := func(body string) string {
		m := regexp.MustCompile(`data-tab="(\w+)" aria-current="true"`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no selected tab in %s", body)
		}
		return m[1]
	}
	// Only already there: the first tab that is not empty.
	present := e.newImport("Jazz")
	e.batchJob(present, "Jazz/A", "skipped", "", "", album)
	if got := selected(open("/import?batch=" + present.String())); got != tabPresent {
		t.Fatalf("already there only: %s", got)
	}
	imported := e.newImport("Jazz")
	e.batchJob(imported, "Jazz/A", "skipped", "", "", album)
	e.batchJob(imported, "Jazz/B", "done", "", "", album)
	if got := selected(open("/import?batch=" + imported.String())); got != tabImported {
		t.Fatalf("imported first: %s", got)
	}
	failed := e.newImport("Jazz")
	e.batchJob(failed, "Jazz/B", "done", "", "", album)
	e.batchJob(failed, "Jazz/C", "failed", "mixed_album", "two album tags", uuid.Nil)
	body := open("/import?batch=" + failed.String())
	if got := selected(body); got != tabAttention {
		t.Fatalf("needs attention first: %s", got)
	}
	// The counts, the chosen tab of a link, and the words.
	if !strings.Contains(body, `id="count-attention" class="count" data-live>1<`) || !strings.Contains(body, `id="count-imported" class="count" data-live>1<`) ||
		!strings.Contains(body, "The tracks have different album names.") || strings.Contains(body, "<p class=\"row-sentence\">mixed_album") {
		t.Fatalf("results page: %s", body)
	}
	if got := selected(open("/import?batch=" + failed.String() + "&tab=present")); got != tabPresent {
		t.Fatalf("a chosen tab: %s", got)
	}
	// Nothing at all yet (the scan runs): Needs attention, and no tabs shown.
	scanning := uuid.New()
	e.must(req{method: "POST", path: "/api/imports", body: importBody(scanning, "Rock")}, 201)
	body = open("/import?batch=" + scanning.String())
	if selected(body) != tabAttention || !strings.Contains(body, `id="tabs" class="tabs" data-hide hidden`) ||
		!strings.Contains(body, "Looking for albums in Rock…") || !strings.Contains(body, `<span id="nav-active" class="dot dot-busy" data-live><span class="sr-only">, in progress</span></span>`) {
		t.Fatalf("a scanning import: %s", body)
	}
	// An import that is not there, a bad tab, a bad id.
	if body := open("/import?batch=" + uuid.New().String()); !strings.Contains(body, "This import isn’t there any more.") {
		t.Fatalf("missing import: %s", body)
	}
	for _, q := range []string{"?batch=" + failed.String() + "&tab=x", "?batch=nope", "?other=1", "?path=a&path=b"} {
		if status, _, _ := pageRequest(t, e, "/import"+q, testHost); status != 422 {
			t.Errorf("/import%s: %d, want 422", q, status)
		}
	}
	if status, _, _ := pageRequest(t, e, "/activity?x=1", testHost); status != 422 {
		t.Error("/activity takes no parameter")
	}
}

// Catalog text and error text are escaped once, by html/template, on both
// pages: folder names, album names, error messages (§10.4).
func TestQueuePagesEscaping(t *testing.T) {
	e := pageEnv(t)
	hostile := `Evil <script>" & name`
	e.files(hostile + "/01.flac")
	album := e.seed(hostile, hostile)
	e.failRender(album)
	e.exec(`UPDATE jobs SET error_message = $2 WHERE kind = 'render' AND album_id = $1`, album, `disk <script>" & full`)
	b := e.newImport(hostile)
	e.batchJob(b, hostile+"/x", "failed", "mixed_album", `tags <script>" & more`, uuid.Nil)
	for _, path := range []string{"/import", "/activity", "/import?batch=" + b.String()} {
		_, _, body := pageRequest(t, e, path, testHost)
		if strings.Contains(body, "<script>") || !strings.Contains(body, "Evil &lt;script&gt;&#34; &amp; name") {
			t.Errorf("%s is not escaped: %s", path, body)
		}
	}
}

// The pages render only what has something to say (NOTES.md N-296): idle
// and settled, Activity is its empty sentence and Advanced, with no group,
// no count of 0, no «And 0 more», no notice or «Details» waiting for an
// error, and no «in progress» in the sidebar; busy, a group is there only
// when it has rows, and «And N more» only when some are not listed.
func TestQueuePagesRenderOnlyWhatIsThere(t *testing.T) {
	e := pageEnv(t)
	open := func(path string) string {
		t.Helper()
		status, _, body := pageRequest(t, e, path, testHost)
		if status != 200 {
			t.Fatalf("%s: %d %s", path, status, body)
		}
		return body
	}
	absent := func(what, body string, parts ...string) {
		t.Helper()
		for _, p := range parts {
			if strings.Contains(body, p) {
				t.Errorf("%s: %q is rendered", what, p)
			}
		}
	}
	// The owner's case: one failure, settled (dismissed), nothing running.
	batch := e.newImport("Jazz")
	settled := e.batchJob(batch, "Jazz/A", "failed", "mixed_album", "two album tags", uuid.Nil)
	e.exec(`UPDATE jobs SET dismissed_at = now() WHERE id = $1`, settled)
	body := open("/activity")
	if !strings.Contains(body, "Nothing in progress.") || !strings.Contains(body, `<span id="nav-active" class="dot dot-busy" data-live hidden></span>`) {
		t.Fatalf("idle Activity: %s", body)
	}
	absent("idle Activity", body, `class="group"`, "group-", "data-live>0<", "more.", `class="notice"`, `role="alert"`, "Details", ", in progress")
	absent("idle results", open("/import?batch="+batch.String()), `class="notice"`, `role="alert"`, ", in progress")

	// Busy: one running, 101 waiting, nothing failed.
	busy := e.newImport("Rock")
	e.batchJob(busy, "Rock/000", "running", "", "", uuid.Nil)
	for i := 1; i <= activityRows+1; i++ {
		e.batchJob(busy, "Rock/"+strings.Repeat("0", 3-len(strconv.Itoa(i)))+strconv.Itoa(i), "pending", "", "", uuid.Nil)
	}
	body = open("/activity")
	if !strings.Contains(body, `id="group-running"`) || !strings.Contains(body, `id="group-pending"`) ||
		!strings.Contains(body, `<p id="more-pending" class="empty-note" data-part data-live>And 1 more.</p>`) ||
		!strings.Contains(body, `<span class="sr-only">, in progress</span>`) {
		t.Fatalf("busy Activity: %s", body)
	}
	absent("busy Activity", body, "group-failed", "more-running", "Nothing in progress.", `class="notice"`, `role="alert"`)
}
