package http

import (
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/web"
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
	if r.URL.Path == "/static/app.css" || r.URL.Path == "/static/app.js" || r.URL.Path == "/static/queue.js" {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		b, err := web.Assets.ReadFile(name)
		if err != nil {
			a.writeError(w, notFound())
			return
		}
		if strings.HasSuffix(name, ".css") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		_, err = w.Write(b)
		if err != nil {
			a.log.Debug("writing UI asset", "error", err)
		}
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
		a.renderPage(w, r, "import.html", pageData{Title: "Import", Queue: true})
	case r.URL.Path == "/activity":
		a.renderPage(w, r, "activity.html", pageData{Title: "Activity", Queue: true})
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

type pageData struct {
	Title, AlbumLink, Query, Next, Status string
	Trash, Pending, Queue                 bool
	Artists                               []pageArtist
	Albums                                []pageAlbum
	Album                                 albumJSON
	ETag                                  string
	ImageAttachments, LyricsAttachments   []attachmentJSON
}
type pageArtist struct {
	ID, Name string
	Selected bool
}
type pageAlbum struct{ ID, Title, ArtistName, Status string }

func (a *API) renderPage(w http.ResponseWriter, r *http.Request, file string, data pageData) {
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

func (a *API) libraryPage(w http.ResponseWriter, r *http.Request, c *catalog.Service) {
	params, e := queryParams(r, "q", "artist", "trash", "after")
	if e != nil {
		a.writeError(w, e)
		return
	}
	f := catalog.AlbumFilter{Query: params["q"], Limit: 50}
	f.ArtistID, e = queryID(params, "artist")
	if e == nil && params["trash"] != "" && params["trash"] != "true" {
		e = invalidField("trash", "must be true")
	}
	f.Trashed = params["trash"] == "true"
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
	artists, err := c.ListArtists(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d := pageData{Title: "Library", Query: f.Query, Trash: f.Trashed}
	if len(page.Albums) != 0 {
		d.AlbumLink = "/albums/" + page.Albums[0].ID.String()
	}
	for _, ar := range artists {
		d.Artists = append(d.Artists, pageArtist{ar.ID.String(), ar.Name, ar.ID == f.ArtistID})
	}
	for _, al := range page.Albums {
		d.Albums = append(d.Albums, pageAlbum{al.ID.String(), al.Title, al.ArtistName, statusBadge(al.Trashed, al.Revision, al.PublishedRevision, al.PublishedRenderer, al.PublishedPath, al.JobState, a.renderVersion)})
	}
	if page.Next != nil {
		v := url.Values{"q": {f.Query}, "after": {encodeAlbumCursor(*page.Next)}}
		if f.ArtistID != uuid.Nil {
			v.Set("artist", f.ArtistID.String())
		}
		if f.Trashed {
			v.Set("trash", "true")
		}
		d.Next = "/?" + v.Encode()
	}
	a.renderPage(w, r, "library.html", d)
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
	d := pageData{Title: v.Title, AlbumLink: r.URL.Path, Album: albumRep(v), ETag: ETag(KindAlbum, v.ID, v.Revision)}
	for _, ar := range artists {
		d.Artists = append(d.Artists, pageArtist{ar.ID.String(), ar.Name, ar.ID == v.ArtistID})
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
	a.renderPage(w, r, "album.html", d)
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
