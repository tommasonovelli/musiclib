package http

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"musiclib/internal/catalog"
)

func pageRequest(t *testing.T, e *env, path, host string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, res.Header, string(body)
}

func TestPagesCatalogEscapingAndBoundary(t *testing.T) {
	e := newEnv(t)
	e.srv.Close()
	// The server under test includes the same API handler plus the page routes.
	// A real PostgreSQL-backed catalog supplies the values.
	mux := http.NewServeMux()
	mux.Handle("/api/", e.api)
	mux.HandleFunc("/", e.api.Pages)
	e.srv = httptest.NewServer(mux)
	id := e.seed(`<script>" &`, `Album <script>alert(1)</script> & "`)
	e.exec(`UPDATE attachments SET rel_path = $1 WHERE album_id = $2 AND rel_path = 'Scans/Booklet.pdf'`, `Scans/<script>" &.pdf`, id)
	cases := []string{"/", "/albums/" + id.String(), "/import", "/activity"}
	for _, path := range cases {
		status, headers, body := pageRequest(t, e, path, testHost)
		if status != 200 {
			t.Fatalf("%s: %d %s", path, status, body)
		}
		if !strings.Contains(headers.Get("Content-Security-Policy"), "default-src 'self'") || headers.Get("X-Content-Type-Options") != "nosniff" || headers.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s: headers: %v", path, headers)
		}
		if strings.Contains(body, "<script>alert(1)</script>") || strings.Contains(body, `<script>" &`) {
			t.Fatalf("unescaped title in %s", path)
		}
		if path == "/albums/"+id.String() && (!strings.Contains(body, `id="read-only"`) || !strings.Contains(body, "Scans/&lt;script&gt;") || strings.Contains(body, "Scans/<script>") || !strings.Contains(body, "/api/albums/"+id.String())) {
			t.Fatalf("unescaped or missing attachment: %s", body)
		}
	}
	for _, path := range []string{"/albums/invalid", "/albums/" + id.String() + "/other", "/missing", "/static/missing.js"} {
		status, _, _ := pageRequest(t, e, path, testHost)
		if status != 404 {
			t.Errorf("%s = %d", path, status)
		}
	}
	status, _, _ := pageRequest(t, e, "/", "evil.test")
	if status != 421 {
		t.Errorf("bad Host = %d", status)
	}
	status, headers, body := pageRequest(t, e, "/static/app.js", testHost)
	if status != 200 || headers.Get("Content-Type") != "text/javascript; charset=utf-8" || headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("app.js: %d %v", status, headers)
	}
	if !strings.Contains(body, "If-Match") || !strings.Contains(body, "X-Musiclib-Request") {
		t.Fatal("mutation guards missing from embedded module")
	}
}

func TestLibraryFiltersAndProcessingState(t *testing.T) {
	e := newEnv(t)
	e.srv.Close()
	mux := http.NewServeMux()
	mux.Handle("/api/", e.api)
	mux.HandleFunc("/", e.api.Pages)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	id := e.seed("Artist One", "Find Me")
	other := e.seed("Artist Two", "Other Title")
	_, _, body := pageRequest(t, e, "/", testHost)
	if !strings.Contains(body, "Find Me") || !strings.Contains(body, "Other Title") || !strings.Contains(body, `data-status="Queued"`) || !strings.Contains(body, "In attesa") || !strings.Contains(body, "/albums/"+id.String()) {
		t.Fatalf("library page missing catalog/status: %s", body)
	}
	_, _, body = pageRequest(t, e, "/?q=find", testHost)
	if !strings.Contains(body, "Find Me") || strings.Contains(body, "Other Title") {
		t.Fatalf("search: %s", body)
	}
	artist := e.must(req{method: "GET", path: "/api/albums/" + id.String()}, 200).body["artist_id"].(string)
	_, _, body = pageRequest(t, e, "/?artist="+artist, testHost)
	if !strings.Contains(body, "Find Me") || strings.Contains(body, "Other Title") {
		t.Fatalf("artist filter: %s", body)
	}
	etag := e.must(req{method: "GET", path: "/api/albums/" + other.String()}, 200).header.Get("ETag")
	e.must(req{method: "DELETE", path: "/api/albums/" + other.String(), ifMatch: etag}, 200)
	_, _, body = pageRequest(t, e, "/?trash=true", testHost)
	if !strings.Contains(body, "Other Title") || strings.Contains(body, "Find Me") {
		t.Fatalf("trash filter: %s", body)
	}
}

func TestLibraryCursor(t *testing.T) {
	e := newEnv(t)
	e.srv.Close()
	mux := http.NewServeMux()
	mux.Handle("/api/", e.api)
	mux.HandleFunc("/", e.api.Pages)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	for i := 0; i < 51; i++ {
		e.seed("Cursor Artist", fmt.Sprintf("Cursor %03d", i))
	}
	_, _, first := pageRequest(t, e, "/?q=cursor", testHost)
	match := regexp.MustCompile(` href="([^"]+)" rel="next">Mostra altri</a>`).FindStringSubmatch(first)
	if len(match) != 2 {
		t.Fatal("50-entry page has no cursor link")
	}
	if strings.Count(first, `class="tile"`) != 50 {
		t.Fatal("first page must contain 50 albums")
	}
	_, _, second := pageRequest(t, e, html.UnescapeString(match[1]), testHost)
	if strings.Count(second, `class="tile"`) != 1 || strings.Contains(second, `rel="next"`) || !strings.Contains(second, "Cursor 050") {
		t.Fatalf("cursor page: %s", second)
	}
}

func TestStatusBadges(t *testing.T) {
	current := "version"
	for _, tc := range []struct {
		name                string
		trash               bool
		rev, published      int64
		renderer, path, job *string
		want                string
	}{
		{"aligned", false, 2, 2, ptr(current), ptr("artist/album"), nil, "Aligned"},
		{"old renderer", false, 2, 2, ptr("old"), ptr("artist/album"), nil, "Queued"},
		{"old revision", false, 2, 1, ptr(current), ptr("artist/album"), nil, "Queued"},
		{"pending", false, 2, 2, ptr(current), ptr("artist/album"), ptr("pending"), "Queued"},
		{"running", false, 2, 2, ptr(current), ptr("artist/album"), ptr("running"), "Processing"},
		{"failed", false, 2, 2, ptr(current), ptr("artist/album"), ptr("failed"), "Error"},
		{"archived", true, 3, 3, nil, nil, nil, "Archived"},
		{"removing", true, 3, 2, ptr(current), ptr("artist/album"), nil, "Queued"},
		{"trash running", true, 3, 2, ptr(current), ptr("artist/album"), ptr("running"), "Processing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := statusBadge(tc.trash, tc.rev, tc.published, tc.renderer, tc.path, tc.job, current)
			if got != tc.want {
				t.Fatal(fmt.Sprintf("%s != %s", got, tc.want))
			}
		})
	}
}

// Every download link the album page renders must reach a real endpoint.
func TestAlbumPageLinksResolve(t *testing.T) {
	e := newEnv(t)
	e.srv.Close()
	mux := http.NewServeMux()
	mux.Handle("/api/", e.api)
	mux.HandleFunc("/", e.api.Pages)
	e.srv = httptest.NewServer(mux)
	a := e.seedReal("Miles Davis", "Kind of Blue", catalog.FormatFLAC)
	_, _, body := pageRequest(t, e, "/albums/"+a.id.String(), testHost)
	links := regexp.MustCompile(`href="(/api/[^"]+)"`).FindAllStringSubmatch(body, -1)
	if len(links) == 0 {
		t.Fatal("no download links")
	}
	for _, m := range links {
		path := html.UnescapeString(m[1])
		if d := e.download("GET", path); d.status != http.StatusOK {
			t.Errorf("%s: %d %s", path, d.status, d.body)
		}
	}
}
