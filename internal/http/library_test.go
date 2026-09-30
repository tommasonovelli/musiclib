package http

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/store"
)

// pageEnv is newEnv with the page routes next to the API, as the daemon
// mounts them.
func pageEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.srv.Close()
	mux := http.NewServeMux()
	mux.Handle("/api/", e.api)
	mux.HandleFunc("/", e.api.Pages)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

// align publishes an album at its revision with the current renderer and
// no job: §10.3 Aligned.
func (e *env) align(id uuid.UUID) {
	e.t.Helper()
	e.exec(`UPDATE albums SET published_path = 'Published/' || id::text, published_revision = revision,
		published_renderer = $2, published_build = $3, published_receipt_hash = $4 WHERE id = $1`,
		id, testRender, store.NewID(), newHash())
	e.exec(`DELETE FROM jobs WHERE kind = 'render' AND album_id = $1`, id)
}

// failRender makes the album's render job failed: §10.3 Error.
func (e *env) failRender(id uuid.UUID) {
	e.t.Helper()
	e.exec(`UPDATE jobs SET state = 'failed', error_code = 'render_io', error_message = 'disk full'
		WHERE kind = 'render' AND album_id = $1`, id)
}

// runRender makes the album's render job running: §10.3 Processing.
func (e *env) runRender(id uuid.UUID) {
	e.t.Helper()
	e.exec(`UPDATE jobs SET state = 'running', claimed = requested WHERE kind = 'render' AND album_id = $1`, id)
}

// tileOf is the grid entry of one album in a Library page.
func tileOf(t *testing.T, body string, id uuid.UUID) string {
	t.Helper()
	m := regexp.MustCompile(`<li class="tile" data-album="` + id.String() + `"[^>]*>.*?</a></li>`).FindString(body)
	if m == "" {
		t.Fatalf("no tile for %s in %s", id, body)
	}
	return m
}

func TestLibraryGridTiles(t *testing.T) {
	e := pageEnv(t)
	aligned := e.seed("Miles Davis", "Kind of Blue")
	e.align(aligned)
	failed := e.seed("Bill Evans", "Sunday at the Village Vanguard")
	e.failRender(failed)
	// Each tile shows its own album's year, not the fixture's 1959.
	e.exec(`UPDATE albums SET year = 1961 WHERE id = $1`, failed)
	queued := e.seed("Chet Baker", "Chet")
	e.exec(`UPDATE albums SET cover_hash = NULL, year = NULL WHERE id = $1`, queued)
	running := e.seed("Dave Brubeck", "Time Out")
	e.runRender(running)
	_, _, body := pageRequest(t, e, "/", testHost)
	if tile := tileOf(t, body, running); !strings.Contains(tile, `data-status="Processing"`) || !strings.Contains(tile, `class="dot"`) || !strings.Contains(tile, `<span class="tile-status">Updating</span>`) {
		t.Errorf("running tile: %s", tile)
	}

	tile := tileOf(t, body, aligned)
	for _, want := range []string{`data-status="Aligned"`, `<img src="/api/albums/` + aligned.String() + `/cover" alt="" width="200" height="200" loading="lazy" decoding="async">`, `<span class="tile-title">Kind of Blue</span>`, `<span class="tile-artist">Miles Davis</span>`, `<span class="tile-year">1959</span>`} {
		if !strings.Contains(tile, want) {
			t.Errorf("aligned tile lacks %s: %s", want, tile)
		}
	}
	if strings.Contains(tile, `class="dot"`) || strings.Contains(tile, "tile-status") || strings.Contains(tile, "Up to date") {
		t.Errorf("aligned album shows a status: %s", tile)
	}
	tile = tileOf(t, body, failed)
	if !strings.Contains(tile, `data-status="Error"`) || !strings.Contains(tile, `class="dot"`) || !strings.Contains(tile, `<span class="tile-status">Needs attention</span>`) || !strings.Contains(tile, `<span class="tile-year">1961</span>`) {
		t.Errorf("failed tile: %s", tile)
	}
	tile = tileOf(t, body, queued)
	if !strings.Contains(tile, `class="art art-none"`) || !strings.Contains(tile, `<span class="initials" aria-hidden="true">C</span>`) ||
		strings.Contains(tile, "<img") || strings.Contains(tile, "tile-year") || !strings.Contains(tile, "Waiting") {
		t.Errorf("coverless queued tile: %s", tile)
	}
	if !strings.Contains(body, `data-count="1"`) {
		t.Error("the Needs attention count is not 1")
	}
}

// The count is rendered on every page; the filter lists failed albums,
// active and trashed, and refuses to combine with the trash filter.
func TestLibraryFixFilterAndCount(t *testing.T) {
	e := pageEnv(t)
	fine := e.seed("Artist A", "Fine album")
	broken := e.seed("Artist B", "Broken album")
	e.failRender(broken)
	trashed := e.seed("Artist C", "Broken trashed album")
	_, etag := e.album(trashed)
	e.must(req{method: "DELETE", path: "/api/albums/" + trashed.String(), ifMatch: etag}, 200)
	e.failRender(trashed)
	id := e.seed("Artist D", "Page album")
	for _, path := range []string{"/", "/import", "/activity", "/albums/" + id.String(), "/?trash=true"} {
		_, _, body := pageRequest(t, e, path, testHost)
		if !regexp.MustCompile(`<a class="fix-filter" href="/\?fix=true" data-count="2"[^>]*>.*<span class="count"><span class="sr-only">: </span>2</span></a>`).MatchString(body) {
			t.Errorf("%s: no count of 2 in %s", path, body)
		}
	}
	_, _, body := pageRequest(t, e, "/?fix=true", testHost)
	if !strings.Contains(body, "Broken album") || !strings.Contains(body, "Broken trashed album") || strings.Contains(body, "Fine album") ||
		!strings.Contains(body, `<h1 class="page-title">Needs attention</h1>`) || !strings.Contains(body, `href="/?fix=true" data-count="2" aria-current="page"`) {
		t.Fatalf("fix filter: %s", body)
	}
	_, _, body = pageRequest(t, e, "/?fix=true&q=trashed", testHost)
	if !strings.Contains(body, "Broken trashed album") || strings.Contains(body, ">Broken album<") || !strings.Contains(body, `<input type="hidden" name="fix" value="true">`) {
		t.Fatalf("search inside the fix filter: %s", body)
	}
	if status, _, _ := pageRequest(t, e, "/?fix=true&trash=true", testHost); status != 422 {
		t.Errorf("fix with trash = %d", status)
	}
	if status, _, _ := pageRequest(t, e, "/?fix=1", testHost); status != 422 {
		t.Errorf("fix=1 = %d", status)
	}
	e.align(broken)
	e.exec(`DELETE FROM jobs WHERE album_id = $1`, trashed)
	_, _, body = pageRequest(t, e, "/?fix=true", testHost)
	if !strings.Contains(body, "No albums need attention.") || !strings.Contains(body, `data-count="0"`) {
		t.Fatalf("empty fix filter: %s", body)
	}
	_ = fine
}

func TestLibraryEmptyStatesAndTrash(t *testing.T) {
	e := pageEnv(t)
	_, _, body := pageRequest(t, e, "/", testHost)
	if !strings.Contains(body, "Your library is empty.") || !strings.Contains(body, "Import your music folder. The originals stay as they are.") ||
		!strings.Contains(body, `<a class="btn btn-primary" href="/import">Import music</a>`) || strings.Contains(body, `id="grid"`) {
		t.Fatalf("empty library: %s", body)
	}
	_, _, body = pageRequest(t, e, "/?trash=true", testHost)
	if !strings.Contains(body, "The trash is empty.") || !strings.Contains(body, `<h1 class="page-title">Trash</h1>`) || strings.Contains(body, "Import music") {
		t.Fatalf("empty trash: %s", body)
	}
	id := e.seed("Miles Davis", "Kind of Blue")
	_, _, body = pageRequest(t, e, "/?q=%3Cscript%3E%22%26", testHost)
	if !strings.Contains(body, "No albums for “&lt;script&gt;&#34;&amp;”.") || strings.Contains(body, `<script>"&`) {
		t.Fatalf("empty search: %s", body)
	}
	_, etag := e.album(id)
	e.must(req{method: "DELETE", path: "/api/albums/" + id.String(), ifMatch: etag}, 200)
	_, _, body = pageRequest(t, e, "/?trash=true", testHost)
	if !strings.Contains(body, `data-album="`+id.String()+`"`) || !regexp.MustCompile(`<a href="/\?trash=true" aria-current="page"><svg [^>]*><use href="#i-trash"/></svg><span class="nav-label">Trash</span></a>`).MatchString(body) {
		t.Fatalf("trash grid: %s", body)
	}
	_, _, body = pageRequest(t, e, "/", testHost)
	if !strings.Contains(body, "Your library is empty.") {
		t.Fatalf("library with only trash: %s", body)
	}
}

// The artist is chosen through the search: matching artists are links to
// the artist filter, which keeps the trash or fix view it was searched in.
func TestLibraryArtistHits(t *testing.T) {
	e := pageEnv(t)
	miles := e.seed("Miles Davis", "Kind of Blue")
	e.seed("Milestone Players", "Milestones")
	e.seed("Bill Evans", "Miles Ahead Tribute")
	_, _, body := pageRequest(t, e, "/?q=MILES", testHost)
	artist := e.artistOf(miles)
	if !strings.Contains(body, `<a href="/?artist=`+artist.String()+`">Miles Davis</a>`) || !strings.Contains(body, ">Milestone Players</a>") ||
		strings.Contains(body, ">Bill Evans</a>") || !strings.Contains(body, "Miles Ahead Tribute") {
		t.Fatalf("artist hits: %s", body)
	}
	_, _, body = pageRequest(t, e, "/?trash=true&q=miles", testHost)
	if !strings.Contains(body, `href="/?artist=`+artist.String()+`&amp;trash=true"`) {
		t.Fatalf("hits lost the trash view: %s", body)
	}
	_, _, body = pageRequest(t, e, "/?artist="+artist.String(), testHost)
	if !strings.Contains(body, `<h1 class="page-title">Miles Davis</h1>`) || !strings.Contains(body, "Albums by Miles Davis.") ||
		!strings.Contains(body, `<a href="/">All artists</a>`) || strings.Contains(body, "Milestones") || strings.Contains(body, "artist-hits") ||
		!strings.Contains(body, `<input type="hidden" name="artist" value="`+artist.String()+`">`) {
		t.Fatalf("artist filter: %s", body)
	}
	_, _, body = pageRequest(t, e, "/?artist="+uuid.New().String(), testHost)
	if !strings.Contains(body, "No albums by this artist.") || !strings.Contains(body, "Unknown artist.") {
		t.Fatalf("unknown artist: %s", body)
	}
}

// The static handler serves only the listed assets, each with its type;
// the versioned fonts are cacheable, everything else keeps no-store.
func TestStaticAssets(t *testing.T) {
	e := pageEnv(t)
	for _, c := range []struct{ path, ctype, cache, file string }{
		{"/static/app.css", "text/css; charset=utf-8", "no-store", "app.css"},
		{"/static/library.js", "text/javascript; charset=utf-8", "no-store", "library.js"},
		{"/static/queue.js", "text/javascript; charset=utf-8", "no-store", "queue.js"},
		{"/static/sidebar.js", "text/javascript; charset=utf-8", "no-store", "sidebar.js"},
		{"/static/" + fontLatin, "font/woff2", "public, max-age=31536000, immutable", fontLatin},
		{"/static/" + fontLatinExt, "font/woff2", "public, max-age=31536000, immutable", fontLatinExt},
		{"/static/OFL.txt", "text/plain; charset=utf-8", "no-store", "OFL.txt"},
		{"/static/" + grain, "image/svg+xml", faviconCache, grain},
	} {
		status, h, body := pageRequest(t, e, c.path, testHost)
		want, err := os.ReadFile("../../web/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		if status != 200 || h.Get("Content-Type") != c.ctype || h.Get("Cache-Control") != c.cache || h.Get("X-Content-Type-Options") != "nosniff" || body != string(want) {
			t.Errorf("%s: %d %v", c.path, status, h)
		}
	}
	if _, _, body := pageRequest(t, e, "/static/"+fontLatin, testHost); !strings.HasPrefix(body, "wOF2") {
		t.Error("the font is not WOFF2")
	}
	for _, path := range []string{"/static/layout.html", "/static/embed.go", "/static/", "/static/app.CSS", "/static/OFL"} {
		if status, _, _ := pageRequest(t, e, path, testHost); status != 404 {
			t.Errorf("%s = %d", path, status)
		}
	}
}

func TestInitials(t *testing.T) {
	for title, want := range map[string]string{
		"Kind of Blue": "KB", "chet": "C", "  ": "", "!!!": "", "The Köln Concert": "TC",
		"ÉTUDES (live) 1971": "É1", "東京 物語": "東物", "a-ha": "AH",
	} {
		if got := initials(title); got != want {
			t.Errorf("initials(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestStatusWords(t *testing.T) {
	for status, want := range map[string]string{
		"Aligned": "", "Archived": "", "Queued": "Waiting", "Processing": "Updating", "Error": "Needs attention",
	} {
		if got := statusWord(status); got != want {
			t.Errorf("statusWord(%s) = %q", status, got)
		}
	}
}
