package http

import (
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/web"
)

// staticAssets are the files served under /static/ (DESIGN.md §2.3): the
// exact embedded names and their Content-Type. Anything else is 404.
var staticAssets = map[string]string{
	"app.css":    "text/css; charset=utf-8",
	"app.js":     "text/javascript; charset=utf-8",
	"queue.js":   "text/javascript; charset=utf-8",
	"library.js": "text/javascript; charset=utf-8",
	"OFL.txt":    "text/plain; charset=utf-8",
	fontLatin:    "font/woff2",
	fontLatinExt: "font/woff2",
}

// The self-hosted Hanken Grotesk subsets (NOTES.md N-244). Their names carry
// the Google Fonts version, so a new version is a new URL and the files can
// be cached as immutable.
const (
	fontLatin    = "hanken-grotesk-v12-latin.woff2"
	fontLatinExt = "hanken-grotesk-v12-latin-ext.woff2"
)

// HTML pages share the API's availability and §10.4 Host/Origin boundary.
// They never place catalog text into HTML except through html/template.
func (a *API) Pages(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	if e := a.checkBoundary(r); e != nil {
		a.writeError(w, e)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		a.writeError(w, newError(http.StatusMethodNotAllowed, CodeMethodNotAllowed, "pages are read-only"))
		return
	}
	if path.Clean(r.URL.Path) != r.URL.Path {
		a.writeError(w, notFound())
		return
	}
	if name, ok := strings.CutPrefix(r.URL.Path, "/static/"); ok {
		a.staticAsset(w, name)
		return
	}
	st := a.state.Load()
	if st.router == nil {
		a.writeError(w, newError(http.StatusServiceUnavailable, st.code, "%s", st.message))
		return
	}
	b := st.backend
	if b.Catalog == nil {
		a.writeError(w, newError(http.StatusServiceUnavailable, CodeNotReady, "catalog unavailable"))
		return
	}
	switch {
	case r.URL.Path == "/":
		a.libraryPage(w, r, b.Catalog)
	case r.URL.Path == "/import":
		a.renderPage(w, r, b.Catalog, "import.html", pageData{Title: "Importa", Nav: "import", Queue: true})
	case r.URL.Path == "/activity":
		a.renderPage(w, r, b.Catalog, "activity.html", pageData{Title: "Attività", Nav: "activity", Queue: true})
	case strings.HasPrefix(r.URL.Path, "/albums/"):
		id, err := uuid.Parse(strings.TrimPrefix(r.URL.Path, "/albums/"))
		if err != nil || id == uuid.Nil || id.String() != strings.TrimPrefix(r.URL.Path, "/albums/") {
			a.writeError(w, notFound())
			return
		}
		a.albumPage(w, r, b.Catalog, id)
	default:
		a.writeError(w, notFound())
	}
}

// staticAsset serves one embedded UI file. The fonts are versioned by name
// and cached for a year; the stylesheet and modules keep no-store, so an
// upgraded server never runs with a stale module.
func (a *API) staticAsset(w http.ResponseWriter, name string) {
	ctype, ok := staticAssets[name]
	if !ok {
		a.writeError(w, notFound())
		return
	}
	b, err := web.Assets.ReadFile(name)
	if err != nil {
		a.writeError(w, notFound())
		return
	}
	w.Header().Set("Content-Type", ctype)
	if ctype == "font/woff2" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if _, err := w.Write(b); err != nil {
		a.log.Debug("writing UI asset", "error", err)
	}
}

type pageData struct {
	Title string
	// Nav is the sidebar entry of the page: library, import, activity,
	// trash or fix (the "Da sistemare" filter).
	Nav string
	// FixCount is the number of albums to fix, on every page (N-245).
	FixCount int64
	// Back shows the link back to the library above the title.
	Back, Pending, Queue bool
	Status               string
	Album                albumJSON
	Artists              []pageArtist
	ETag                 string
	// The album editor's attachment choices.
	ImageAttachments, LyricsAttachments []attachmentJSON
	Library                             *libraryData
}

type pageArtist struct {
	ID, Name string
	// URL is the Library filtered to the artist (search hits only).
	URL      string
	Selected bool
}

// libraryData is the Library view: its filters, the artists matching the
// search, one page of albums and the link to the next one.
type libraryData struct {
	Query, ArtistID, ArtistName string
	Trash, Fix                  bool
	// Empty is the empty state to show when there are no albums: library,
	// search, trash, fix or artist.
	Empty        string
	Artists      []pageArtist
	Albums       []pageAlbum
	Next, AllURL string
}

// pageAlbum is one cover of the grid. Status is the §10.3 status (a test
// and CSS contract); StatusWord is its Italian word, empty when the status
// needs no attention (Aligned, and Archived in the trash).
type pageAlbum struct {
	ID, Title, ArtistName, Status, StatusWord, Initials string
	Cover                                               bool
	Year                                                *int32
}

func (a *API) renderPage(w http.ResponseWriter, r *http.Request, c *catalog.Service, file string, data pageData) {
	n, err := c.CountFailedAlbums(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	data.FixCount = n
	t, err := template.ParseFS(web.Assets, "layout.html", file)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		a.log.Error("writing UI page", "error", err)
	}
}

// libraryArtistHits is how many artists the search offers above the grid.
const libraryArtistHits = 6

func (a *API) libraryPage(w http.ResponseWriter, r *http.Request, c *catalog.Service) {
	params, e := queryParams(r, "q", "artist", "trash", "fix", "after")
	if e != nil {
		a.writeError(w, e)
		return
	}
	f := catalog.AlbumFilter{Query: params["q"], Limit: catalog.DefaultPageSize}
	f.ArtistID, e = queryID(params, "artist")
	for _, flag := range []string{"trash", "fix"} {
		if v, ok := params[flag]; e == nil && ok && v != "true" {
			e = invalidField(flag, "must be true")
		}
	}
	f.Trashed, f.Failed = params["trash"] == "true", params["fix"] == "true"
	if e == nil && f.Trashed && f.Failed {
		e = invalidField("fix", "cannot be combined with trash")
	}
	if after := params["after"]; e == nil && after != "" {
		f.After, e = decodeAlbumCursor(after)
	}
	if e != nil {
		a.writeError(w, e)
		return
	}
	page, err := c.ListAlbums(r.Context(), f)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	lib := &libraryData{Query: f.Query, Trash: f.Trashed, Fix: f.Failed}
	v := libraryValues(f)
	d := pageData{Title: "Libreria", Nav: "library", Library: lib}
	switch {
	case f.Trashed:
		d.Title, d.Nav = "Cestino", "trash"
	case f.Failed:
		d.Title, d.Nav = "Da sistemare", "fix"
	}
	if f.Query != "" || f.ArtistID != uuid.Nil {
		artists, err := c.ListArtists(r.Context())
		if err != nil {
			a.fail(w, r, err)
			return
		}
		for _, ar := range artists {
			if ar.ID == f.ArtistID {
				lib.ArtistID, lib.ArtistName = ar.ID.String(), ar.Name
			}
		}
		// An unknown artist id lists nothing, as the API does.
		if f.ArtistID != uuid.Nil && lib.ArtistID == "" {
			lib.ArtistID = f.ArtistID.String()
		}
		if f.ArtistID == uuid.Nil {
			hits, err := catalog.MatchArtists(artists, f.Query, libraryArtistHits)
			if err != nil {
				a.fail(w, r, err)
				return
			}
			for _, ar := range hits {
				lib.Artists = append(lib.Artists, pageArtist{ID: ar.ID.String(), Name: ar.Name, URL: libraryURL(url.Values{"artist": {ar.ID.String()}, "trash": v["trash"], "fix": v["fix"]})})
			}
		}
		if lib.ArtistName != "" {
			d.Title = lib.ArtistName
		}
	}
	for _, al := range page.Albums {
		status := statusBadge(al.Trashed, al.Revision, al.PublishedRevision, al.PublishedRenderer, al.PublishedPath, al.JobState, a.renderVersion)
		lib.Albums = append(lib.Albums, pageAlbum{
			ID: al.ID.String(), Title: al.Title, ArtistName: al.ArtistName, Status: status,
			StatusWord: statusWord(status), Initials: initials(al.Title), Cover: al.Cover != nil, Year: al.Year,
		})
	}
	lib.AllURL = libraryURL(url.Values{"trash": v["trash"], "fix": v["fix"]})
	if page.Next != nil {
		v.Set("after", encodeAlbumCursor(*page.Next))
		lib.Next = libraryURL(v)
	}
	lib.Empty = emptyState(f)
	a.renderPage(w, r, c, "library.html", d)
}

// libraryValues are the query parameters that reproduce f's filters.
func libraryValues(f catalog.AlbumFilter) url.Values {
	v := url.Values{}
	if f.Query != "" {
		v.Set("q", f.Query)
	}
	if f.ArtistID != uuid.Nil {
		v.Set("artist", f.ArtistID.String())
	}
	if f.Trashed {
		v.Set("trash", "true")
	}
	if f.Failed {
		v.Set("fix", "true")
	}
	return v
}

func libraryURL(v url.Values) string {
	for k, vs := range v {
		if len(vs) == 0 {
			delete(v, k)
		}
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

// emptyState names the empty state of a filter without albums: a search
// wins over the view it searches, and the library is empty only when
// nothing filters it.
func emptyState(f catalog.AlbumFilter) string {
	switch {
	case strings.TrimSpace(f.Query) != "":
		return "search"
	case f.ArtistID != uuid.Nil:
		return "artist"
	case f.Trashed:
		return "trash"
	case f.Failed:
		return "fix"
	}
	return "library"
}

// statusWord is the Italian word of a §10.3 status in the Library, from
// the glossary of the UI principles; "" when the status needs no mention:
// Aligned is the norm and Archived is the norm of the trash (N-248).
func statusWord(status string) string {
	switch status {
	case "Error":
		return "Da sistemare"
	case "Processing":
		return "In aggiornamento"
	case "Queued":
		return "In attesa"
	}
	return ""
}

// initials are the letters shown on an album without a cover: the first
// letter or digit of the title's first word and of its last word, in upper
// case, at most two. A title without letters or digits has none.
func initials(title string) string {
	var first []rune
	for _, w := range strings.FieldsFunc(title, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		for _, r := range w {
			first = append(first, unicode.ToUpper(r))
			break
		}
	}
	switch len(first) {
	case 0:
		return ""
	case 1:
		return string(first)
	}
	return string([]rune{first[0], first[len(first)-1]})
}

func (a *API) albumPage(w http.ResponseWriter, r *http.Request, c *catalog.Service, id uuid.UUID) {
	v, err := c.GetAlbum(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	st, err := c.GetAlbumStatus(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	artists, err := c.ListArtists(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d := pageData{Title: v.Title, Nav: "library", Back: true, Album: albumRep(v), ETag: ETag(KindAlbum, v.ID, v.Revision)}
	if v.Trashed {
		d.Nav = "trash"
	}
	for _, ar := range artists {
		d.Artists = append(d.Artists, pageArtist{ID: ar.ID.String(), Name: ar.Name, Selected: ar.ID == v.ArtistID})
	}
	var state *string
	if st.Job != nil {
		state = &st.Job.State
	}
	d.Status = statusBadge(st.Trashed, st.Revision, st.PublishedRevision, st.PublishedRenderer, st.PublishedPath, state, a.renderVersion)
	d.Pending = state != nil && (*state == "pending" || *state == "running")
	for _, at := range d.Album.Attachments {
		// An uploaded attachment may have no format hint. The API inspects
		// the bytes and validates image dimensions on selection.
		d.ImageAttachments = append(d.ImageAttachments, at)
		if strings.HasSuffix(strings.ToLower(at.RelPath), ".lrc") {
			d.LyricsAttachments = append(d.LyricsAttachments, at)
		}
	}
	a.renderPage(w, r, c, "album.html", d)
}

// statusBadge implements DESIGN.md §10.3. A stale active album with no job
// is queued work, never incorrectly shown as aligned.
func statusBadge(trashed bool, revision, published int64, renderer, publishedPath, job *string, current string) string {
	if job != nil {
		switch *job {
		case "failed":
			return "Error"
		case "running":
			return "Processing"
		case "pending":
			return "Queued"
		}
	}
	if trashed {
		if publishedPath == nil {
			return "Archived"
		}
		return "Queued"
	}
	if revision == published && renderer != nil && *renderer == current && publishedPath != nil {
		return "Aligned"
	}
	return "Queued"
}
