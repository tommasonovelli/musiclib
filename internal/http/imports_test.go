package http

import (
	"bytes"
	"encoding/base64"
	nethttp "net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"musiclib/internal/catalog"
	"musiclib/internal/importer"
	"musiclib/internal/names"
)

// Round 16: GET /api/albums, GET /api/import-source, POST /api/imports and
// GET /api/imports/{id} over a real server, the real catalog on
// PostgreSQL 17 and a real /import on ext4 (§12.1).

func albumNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	var out []string
	for _, raw := range body["albums"].([]any) {
		a := raw.(map[string]any)
		out = append(out, a["artist_name"].(string)+"/"+a["title"].(string))
	}
	return out
}

func TestListAlbumsAPI(t *testing.T) {
	e := newEnv(t)
	kob := e.seed("Miles Davis", "Kind of Blue")
	e.seed("Miles Davis", "Sketches of Spain")
	e.seed("Björk", "Homogenic")
	gone := e.seed("Bill Evans", "Portrait in Jazz")
	_, tag := e.album(gone)
	e.must(req{method: "DELETE", path: "/api/albums/" + gone.String(), ifMatch: tag}, ok)

	r := e.must(req{method: "GET", path: "/api/albums"}, ok)
	if got := albumNames(t, r.body); !slices.Equal(got, []string{"Björk/Homogenic", "Miles Davis/Kind of Blue", "Miles Davis/Sketches of Spain"}) {
		t.Fatalf("the library: %q", got)
	}
	if r.body["next"] != nil || r.header.Get("Cache-Control") != "no-store" || r.header.Get("ETag") != "" {
		t.Fatalf("next %v, headers %v", r.body["next"], r.header)
	}
	// The summary: the album's fields in the declared order, its ETag the
	// album's.
	s := r.body["albums"].([]any)[1].(map[string]any)
	_, kobTag := e.album(kob)
	if s["id"] != kob.String() || s["etag"] != kobTag || s["artist_name"] != "Miles Davis" || s["year"] != float64(1959) ||
		s["genre"] != "Jazz" || s["trashed"] != false || s["cover"] == nil || len(s) != 11 {
		t.Fatalf("summary %v", s)
	}
	if !bytes.HasPrefix(r.raw, []byte(`{"albums":[{"id":"`)) || !bytes.Contains(r.raw, []byte(`"revision":1,"etag":"\"album:`)) {
		t.Fatalf("field order: %s", r.raw)
	}
	search := func(query string) []string {
		return albumNames(t, e.must(req{method: "GET", path: "/api/albums?" + query}, ok).body)
	}
	for query, want := range map[string][]string{
		"q=BJO%CC%88RK":                      {"Björk/Homogenic"}, // NFD, upper case
		"q=kind+of":                          {"Miles Davis/Kind of Blue"},
		"q=miles":                            {"Miles Davis/Kind of Blue", "Miles Davis/Sketches of Spain"},
		"q=zzz":                              nil,
		"trash=true":                         {"Bill Evans/Portrait in Jazz"},
		"trash=false&q=%20":                  {"Björk/Homogenic", "Miles Davis/Kind of Blue", "Miles Davis/Sketches of Spain"},
		"artist=" + e.artistOf(kob).String(): {"Miles Davis/Kind of Blue", "Miles Davis/Sketches of Spain"},
		"artist=" + uuid.NewString():         nil,
	} {
		if got := search(query); !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", query, got, want)
		}
	}
	// Pages of one: the cursors lead through the whole list, in order.
	var paged []string
	next := ""
	for range 10 {
		q := "limit=1"
		if next != "" {
			q += "&after=" + url.QueryEscape(next)
		}
		p := e.must(req{method: "GET", path: "/api/albums?" + q}, ok)
		paged = append(paged, albumNames(t, p.body)...)
		n, _ := p.body["next"].(string)
		if n == "" {
			break
		}
		next = n
	}
	if !slices.Equal(paged, []string{"Björk/Homogenic", "Miles Davis/Kind of Blue", "Miles Davis/Sketches of Spain"}) {
		t.Fatalf("paged: %q", paged)
	}
	e.must(req{method: "GET", path: "/api/albums?limit=200"}, ok)
	e.must(req{method: "GET", path: "/api/albums?limit=50"}, ok)
	tampered := base64.RawURLEncoding.EncodeToString([]byte(`[ "b", "c", "` + kob.String() + `"]`))
	for query, field := range map[string]string{
		"limit=0": "limit", "limit=201": "limit", "limit=-1": "limit", "limit=050": "limit", "limit=1.5": "limit",
		"limit=abc": "limit", "limit=": "limit", "limit=1&limit=2": "limit",
		"trash=yes": "trash", "trash=": "trash", "trash=TRUE": "trash",
		"artist=x": "artist", "artist=" + strings.ToUpper(kob.String()): "artist",
		"after=%21%21": "after", "after=" + tampered: "after",
		"after=" + base64.RawURLEncoding.EncodeToString([]byte(`["a","b"]`)): "after",
		// A NUL survives the re-encoding, and PostgreSQL would refuse it
		// as a text parameter (500): refused here.
		"after=" + base64.RawURLEncoding.EncodeToString([]byte(`["\u0000","","`+kob.String()+`"]`)):   "after",
		"after=" + base64.RawURLEncoding.EncodeToString([]byte(`["a","b\u0000","`+kob.String()+`"]`)): "after",
		// Not UTF-8: raw bytes, and a lone surrogate escape.
		"after=" + base64.RawURLEncoding.EncodeToString([]byte("[\"\xff\",\"\",\""+kob.String()+"\"]")): "after",
		"after=" + base64.RawURLEncoding.EncodeToString([]byte(`["\ud800","","`+kob.String()+`"]`)):     "after",
		"q=a&q=b": "q",
	} {
		r := e.wantError(req{method: "GET", path: "/api/albums?" + query}, nethttp.StatusUnprocessableEntity, CodeInvalidField)
		if r.details()["field"] != field {
			t.Errorf("%s: details %v", query, r.details())
		}
	}
	e.wantError(req{method: "GET", path: "/api/albums?page=2"}, nethttp.StatusUnprocessableEntity, CodeUnknownField)
	e.wantError(req{method: "GET", path: "/api/albums?q=%01"}, nethttp.StatusUnprocessableEntity, names.CodeTextControlChar)
	e.wantError(req{method: "GET", path: "/api/albums?q=a%00b"}, nethttp.StatusUnprocessableEntity, names.CodeTextControlChar)
	e.wantError(req{method: "GET", path: "/api/albums?q=%FF"}, nethttp.StatusUnprocessableEntity, names.CodeInvalidUTF8)
	r = e.wantError(req{method: "POST", path: "/api/albums", body: `{}`}, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed)
	if r.header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("Allow %q", r.header.Get("Allow"))
	}
}

// entries is the entries of an import-source answer as "name:type".
func entries(t *testing.T, body map[string]any) []string {
	t.Helper()
	var out []string
	for _, raw := range body["entries"].([]any) {
		en := raw.(map[string]any)
		out = append(out, en["name"].(string)+":"+en["type"].(string))
	}
	return out
}

func TestImportSource(t *testing.T) {
	e := newEnv(t)
	in := e.imports
	for _, d := range []string{"B dir", "Z/sub", "noread"} {
		if err := os.MkdirAll(filepath.Join(in, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"a.txt", ".hidden", "Z/inner.flac", "bad\xff", ".DS_Store"} {
		if err := os.WriteFile(filepath.Join(in, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink to the data volume (§7.1), one outside everything, one
	// inside /import: all listed as symlinks, none followed.
	for name, target := range map[string]string{"link-data": e.data, "link-out": "/etc", "link-in": "B dir"} {
		if err := os.Symlink(target, filepath.Join(in, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(in, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	list := func(query string) resp {
		return e.must(req{method: "GET", path: "/api/import-source" + query}, ok)
	}
	root := list("")
	want := []string{".DS_Store:file", ".hidden:file", "B dir:directory", "Z:directory", "a.txt:file", "bad�:invalid_name",
		"fifo:special", "link-data:symlink", "link-in:symlink", "link-out:symlink", "noread:directory"}
	if got := entries(t, root.body); !slices.Equal(got, want) || root.body["path"] != "" {
		t.Fatalf("/import: %q, want %q (by name bytes)", got, want)
	}
	if !bytes.Equal(list("?path=").raw, root.raw) {
		t.Fatal("path= is not /import itself")
	}
	if got := entries(t, list("?path=Z").body); !slices.Equal(got, []string{"inner.flac:file", "sub:directory"}) {
		t.Fatalf("Z: %q", got)
	}
	if r := list("?path=B+dir"); len(r.body["entries"].([]any)) != 0 || r.body["path"] != "B dir" {
		t.Fatalf("B dir: %s", r.raw)
	}
	for query, want := range map[string]struct {
		status int
		code   string
	}{
		"?path=link-data":                  {nethttp.StatusUnprocessableEntity, importer.CodeSourceRejected},
		"?path=link-out":                   {nethttp.StatusUnprocessableEntity, importer.CodeSourceRejected},
		"?path=link-in":                    {nethttp.StatusUnprocessableEntity, importer.CodeSourceRejected},
		"?path=link-in/x":                  {nethttp.StatusUnprocessableEntity, importer.CodeSourceRejected},
		"?path=link-data/work":             {nethttp.StatusUnprocessableEntity, importer.CodeSourceRejected},
		"?path=a.txt":                      {nethttp.StatusUnprocessableEntity, importer.CodeSourceNotDirectory},
		"?path=fifo":                       {nethttp.StatusUnprocessableEntity, importer.CodeSourceNotDirectory},
		"?path=missing":                    {nethttp.StatusNotFound, importer.CodeSourceNotFound},
		"?path=Z/missing/x":                {nethttp.StatusNotFound, importer.CodeSourceNotFound},
		"?path=..":                         {nethttp.StatusUnprocessableEntity, names.CodePathDotSegment},
		"?path=../etc":                     {nethttp.StatusUnprocessableEntity, names.CodePathDotSegment},
		"?path=Z/../..":                    {nethttp.StatusUnprocessableEntity, names.CodePathDotSegment},
		"?path=.":                          {nethttp.StatusUnprocessableEntity, names.CodePathDotSegment},
		"?path=/etc":                       {nethttp.StatusUnprocessableEntity, names.CodePathAbsolute},
		"?path=" + url.QueryEscape(e.data): {nethttp.StatusUnprocessableEntity, names.CodePathAbsolute},
		"?path=Z/":                         {nethttp.StatusUnprocessableEntity, names.CodePathEmptySegment},
		"?path=Z//sub":                     {nethttp.StatusUnprocessableEntity, names.CodePathEmptySegment},
		"?path=%00":                        {nethttp.StatusUnprocessableEntity, names.CodePathNulByte},
		"?path=%FF":                        {nethttp.StatusUnprocessableEntity, names.CodeInvalidUTF8},
		"?path=a&path=b":                   {nethttp.StatusUnprocessableEntity, CodeInvalidField},
		"?dir=Z":                           {nethttp.StatusUnprocessableEntity, CodeUnknownField},
		"?path=%zz":                        {nethttp.StatusUnprocessableEntity, CodeInvalidField},
	} {
		r := e.wantError(req{method: "GET", path: "/api/import-source" + query}, want.status, want.code)
		if bytes.Contains(r.raw, []byte(e.imports)) || bytes.Contains(r.raw, []byte(e.data)) && !strings.Contains(query, url.QueryEscape(e.data)) {
			t.Errorf("%s: an absolute path in the answer: %s", query, r.raw)
		}
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(filepath.Join(in, "noread"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(in, "noread"), 0o755) })
		e.wantError(req{method: "GET", path: "/api/import-source?path=noread"}, nethttp.StatusUnprocessableEntity, importer.CodeSourceNotReadable)
	}
	for _, r := range []resp{root, list("?path=Z")} {
		if bytes.Contains(r.raw, []byte(e.imports)) || bytes.Contains(r.raw, []byte(e.data)) || r.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("an absolute path or a cacheable answer: %s", r.raw)
		}
	}
	e.wantError(req{method: "POST", path: "/api/import-source", body: `{}`}, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed)
}

func importBody(id uuid.UUID, path string) map[string]any {
	return map[string]any{"id": id.String(), "path": path}
}

func TestCreateImport(t *testing.T) {
	e := newEnv(t)
	id := uuid.New() // any version: the client's request UUID
	r := e.must(req{method: "POST", path: "/api/imports", body: importBody(id, "in/Jazz")}, nethttp.StatusCreated)
	if r.header.Get("Location") != "/api/imports/"+id.String() || r.body["id"] != id.String() || r.body["path"] != "in/Jazz" ||
		r.body["state"] != catalog.BatchScanning || len(r.body["candidates"].([]any)) != 0 {
		t.Fatalf("created: %s", r.raw)
	}
	scan := r.body["scan"].(map[string]any)
	if scan["kind"] != "scan" || scan["state"] != "pending" || scan["batch_id"] != id.String() || scan["overrides"] != nil ||
		scan["album_id"] != nil || scan["source_rel"] != nil {
		t.Fatalf("scan %v", scan)
	}
	// The same request again: the same batch, the same answer, 200.
	again := e.must(req{method: "POST", path: "/api/imports", body: importBody(id, "in/Jazz")}, ok)
	if !bytes.Equal(again.raw, r.raw) || again.header.Get("Location") != "" {
		t.Fatalf("repeated: %s\nfirst: %s", again.raw, r.raw)
	}
	if got := e.must(req{method: "GET", path: "/api/imports/" + id.String()}, ok); !bytes.Equal(got.raw, r.raw) {
		t.Fatalf("GET: %s", got.raw)
	}
	// The same UUID with another path: 409, naming the path recorded; the
	// path differs by NFC only: another path too (kept as on disk, §5.2).
	for _, other := range []string{"in/Rock", "in/Jazz/sub", "in/jazz", ""} {
		c := e.wantError(req{method: "POST", path: "/api/imports", body: importBody(id, other)}, nethttp.StatusConflict,
			catalog.CodeImportBatchConflict)
		if c.details()["path"] != "in/Jazz" {
			t.Fatalf("conflict details %v", c.details())
		}
	}
	nfc := uuid.New()
	e.must(req{method: "POST", path: "/api/imports", body: importBody(nfc, "Björk")}, nethttp.StatusCreated)
	e.wantError(req{method: "POST", path: "/api/imports", body: importBody(nfc, "Björk")}, nethttp.StatusConflict, catalog.CodeImportBatchConflict)
	// /import itself.
	e.must(req{method: "POST", path: "/api/imports", body: importBody(uuid.New(), "")}, nethttp.StatusCreated)

	batches := e.count(`SELECT count(*) FROM import_batches`)
	for body, code := range map[string]string{
		`{"id":"` + uuid.NewString() + `","path":"/abs"}`:               names.CodePathAbsolute,
		`{"id":"` + uuid.NewString() + `","path":"../x"}`:               names.CodePathDotSegment,
		`{"id":"` + uuid.NewString() + `","path":"a//b"}`:               names.CodePathEmptySegment,
		`{"id":"` + uuid.NewString() + `","path":"a\u0000b"}`:           names.CodePathNulByte,
		`{"id":"00000000-0000-0000-0000-000000000000","path":"x"}`:      CodeInvalidField,
		`{"id":"` + strings.ToUpper(uuid.NewString()) + `","path":"x"}`: CodeInvalidField,
		`{"id":"` + uuid.NewString() + `","path":null}`:                 CodeInvalidField,
		`{"id":"` + uuid.NewString() + `"}`:                             CodeMissingField,
		`{"id":"` + uuid.NewString() + `","path":"x","root":"y"}`:       CodeUnknownField,
	} {
		e.wantError(req{method: "POST", path: "/api/imports", body: body}, nethttp.StatusUnprocessableEntity, code)
	}
	e.wantError(req{method: "POST", path: "/api/imports", body: `{"id":"x","id":"y","path":"a"}`}, nethttp.StatusBadRequest, CodeDuplicateKey)
	e.wantError(req{method: "POST", path: "/api/imports", body: importBody(uuid.New(), "x"),
		headers: map[string][]string{"Content-Type": {"text/plain"}}}, nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	e.wantError(req{method: "POST", path: "/api/imports", body: importBody(uuid.New(), "x"),
		headers: map[string][]string{RequestHeader: {""}}}, nethttp.StatusForbidden, CodeRequestHeaderRequired)
	if got := e.count(`SELECT count(*) FROM import_batches`); got != batches {
		t.Fatalf("a refused request created a batch: %d, want %d", got, batches)
	}
	e.wantError(req{method: "GET", path: "/api/imports/" + uuid.NewString()}, nethttp.StatusNotFound, catalog.CodeImportBatchNotFound)
	e.wantError(req{method: "GET", path: "/api/imports/xyz"}, nethttp.StatusNotFound, catalog.CodeImportBatchNotFound)
	e.wantError(req{method: "GET", path: "/api/imports"}, nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed)
}

// Eight concurrent requests with one UUID: one batch, one scan job, one
// 201 and seven 200 with the same report (§7.1).
func TestCreateImportConcurrently(t *testing.T) {
	e := newEnv(t)
	for round := range 5 {
		id := uuid.New()
		var wg sync.WaitGroup
		results := make([]resp, 8)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = e.do(req{method: "POST", path: "/api/imports", body: importBody(id, "in")})
			}()
		}
		wg.Wait()
		created := 0
		for _, r := range results {
			switch r.status {
			case nethttp.StatusCreated:
				created++
			case ok:
			default:
				t.Fatalf("round %d: %d %s", round, r.status, r.raw)
			}
			if r.body["scan"].(map[string]any)["id"] != results[0].body["scan"].(map[string]any)["id"] {
				t.Fatalf("round %d: two scan jobs answered", round)
			}
		}
		if created != 1 || e.count(`SELECT count(*) FROM import_batches WHERE id = $1`, id) != 1 ||
			e.count(`SELECT count(*) FROM jobs WHERE batch_id = $1`, id) != 1 {
			t.Fatalf("round %d: %d created", round, created)
		}
	}
}
