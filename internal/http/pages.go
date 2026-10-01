package http

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/media"
	"musiclib/web"
)

// staticAssets are the files served under /static/ (DESIGN.md §2.3): the
// exact embedded names and their Content-Type. Anything else is 404.
var staticAssets = map[string]string{
	"app.css":    "text/css; charset=utf-8",
	"app.js":     "text/javascript; charset=utf-8",
	"queue.js":   "text/javascript; charset=utf-8",
	"library.js": "text/javascript; charset=utf-8",
	"sidebar.js": "text/javascript; charset=utf-8",
	"OFL.txt":    "text/plain; charset=utf-8",
	favicon:      "image/svg+xml",
	grain:        "image/svg+xml",
	fontLatin:    "font/woff2",
	fontLatinExt: "font/woff2",
}

// favicon is the MusicLib symbol in Violet (NOTES.md N-309, N-312). Its
// name carries no version, so it is cached for a day, not for good: a new
// symbol reaches every browser within a day. /favicon.ico is not served
// (404): every page names this file, so browsers have no reason to ask.
const (
	favicon      = "favicon.svg"
	faviconCache = "public, max-age=86400"
)

// grain is the noise tile of the sidebar's glow. The stylesheet cannot
// inline it (the CSP forbids data: images); like the favicon it is cached
// for a day, so it is not fetched again on every page.
const grain = "grain.svg"

// The self-hosted Hanken Grotesk subsets (NOTES.md N-244). Their names carry
// the Google Fonts version, so a new version is a new URL and the files can
// be cached as immutable.
const (
	fontLatin    = "hanken-grotesk-v12-latin.woff2"
	fontLatinExt = "hanken-grotesk-v12-latin-ext.woff2"
)

// Pages serves everything outside /api: the sign-in and sign-out forms,
// the static UI files, and the pages, which share the API's Host and Origin
// boundary, its session and its availability. Sign-in and the static files
// need neither a session nor the catalog, so they work during the boot.
// Pages never place catalog text into HTML except through html/template.
func (a *API) Pages(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	form := r.Method == http.MethodPost && (r.URL.Path == "/login" || r.URL.Path == "/logout")
	if e := a.checkBoundary(r, form); e != nil {
		a.writeError(w, e)
		return
	}
	switch r.URL.Path {
	case "/login":
		a.login(w, r)
		return
	case "/logout":
		a.logout(w, r)
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
	if !a.signedIn(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
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
		a.importPage(w, r, b)
	case r.URL.Path == "/activity":
		a.activityPage(w, r, b.Catalog)
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
// and cached for a year, the favicon and the grain tile for a day; the
// stylesheet and modules keep no-store, so an upgraded server never runs
// with a stale module.
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
	switch {
	case ctype == "font/woff2":
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case name == favicon, name == grain:
		w.Header().Set("Cache-Control", faviconCache)
	}
	if _, err := w.Write(b); err != nil {
		a.log.Debug("writing UI asset", "error", err)
	}
}

type pageData struct {
	Title string
	// Nav is the sidebar entry of the page: library, import, activity,
	// trash or fix (the "Needs attention" filter).
	Nav string
	// FixCount is the number of albums to fix, on every page (N-245).
	FixCount int64
	// Back shows the link back above the title: to the Trash when Nav is
	// "trash" (a trashed album), to the Library otherwise (N-281).
	Back, Queue bool
	// Active shows the sidebar's activity dot: work of the page is in
	// progress, and queue.js polls (Import and Activity).
	Active  bool
	Library *libraryData
	// Editor is the album page; it draws its own head (the title is a
	// field), so the layout's page head is left out.
	Editor *editorData
	// Import and Activity are the queue views (round 21, N-286 to N-289).
	Import   *importView
	Activity *activityView
	// Login is the sign-in page; the layout draws no sidebar for it.
	Login *loginView
}

type pageArtist struct {
	ID, Name string
	// URL is the Library filtered to the artist (search hits only).
	URL string
	// ETag is the artist's, for the album page's «Rename artist».
	ETag string
}

// editorData is the album page (round 20, NOTES.md N-256): the album, its
// §10.3 status with the Library's word, its tracks by disc, the
// attachments the cover and the lyrics can be chosen from, the cover limit
// in plain words and every artist of the catalog.
type editorData struct {
	Album albumJSON
	ETag  string
	// Genre is the album's genre, "" without one: the placeholder of the
	// tracks that inherit it.
	Genre string
	// Status is the §10.3 status (a CSS and test contract), StatusWord its
	// word (statusWord), empty for Aligned and Archived
	// (N-248). Pending is true while a render job is pending or running:
	// the page polls.
	Status, StatusWord string
	Pending            bool
	// Initials stand in for a missing cover, as in the Library.
	Initials string
	// Discs are the tracks by disc, in the album's order. MultiDisc shows
	// the disc headings: only when there is more than one disc.
	Discs     []pageDisc
	MultiDisc bool
	// Length is the line under the tracks: their number and how long the
	// album plays (albumLength).
	Length string
	// ImageAttachments are the attachments the cover can be chosen from
	// (coverCandidate); LyricsAttachments the .lrc ones.
	ImageAttachments, LyricsAttachments []attachmentJSON
	// CoverMB is the largest cover the album takes, in whole megabytes.
	CoverMB int64
	// ArtistAlbums is how many albums, trashed ones included, a rename of
	// the album's artist changes (§4.3): the words of its confirmation.
	ArtistAlbums int
	// Artists are every artist of the catalog, the choices of the artist
	// field (the list of GET /api/artists, N-146).
	Artists []pageArtist
}

type pageDisc struct {
	No     int32
	Tracks []pageTrack
}

// pageTrack is a track row. Artist and Genre are the track's own values,
// "" when it inherits the album's (§4.1); GenreNone is the explicit «no
// genre» (genre "" in the API), whose field is empty too. Time is the
// read-only duration (N-302), "" while unknown, and Length its HTML
// duration string for <time datetime>.
type pageTrack struct {
	ID, Title, Artist, Genre string
	Disc, No                 int32
	GenreNone, Lyrics        bool
	Time, Length             string
}

func trackRow(t trackJSON) pageTrack {
	p := pageTrack{ID: t.ID, Title: t.Title, Disc: t.Disc, No: t.No, Lyrics: t.LyricsHash != nil}
	if t.Artist != nil {
		p.Artist = *t.Artist
	}
	if t.Genre != nil {
		p.Genre, p.GenreNone = *t.Genre, *t.Genre == ""
	}
	if t.DurationMS != nil {
		s := seconds(*t.DurationMS)
		p.Time, p.Length = clock(s), fmt.Sprintf("PT%dS", s)
	}
	return p
}

// seconds is a duration in milliseconds rounded to the nearest second, the
// precision the page shows (N-302); library.js rounds the same way.
func seconds(ms int64) int64 { return (ms + 500) / 1000 }

// clock is a track's duration as a player shows it: m:ss, or h:mm:ss from
// one hour.
func clock(s int64) string {
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// albumLength is the discreet line under the track table (N-302): the
// number of tracks and, when every duration is known, how long the album
// plays, in words: «12 tracks, 45 minutes», «1 track, 1 hour 3 minutes».
// A total with unknown parts would be wrong, so it is left out, and so is
// the total of no track (an album in the trash whose tracks were moved).
func albumLength(tracks []trackJSON) string {
	out := plural(len(tracks), "track")
	if len(tracks) == 0 {
		return out
	}
	var ms int64
	for _, t := range tracks {
		if t.DurationMS == nil {
			return out
		}
		ms += *t.DurationMS
	}
	s := seconds(ms)
	switch m := (s + 30) / 60; {
	case s < 60:
		return out + ", " + plural(int(s), "second")
	case m < 60:
		return out + ", " + plural(int(m), "minute")
	case m%60 == 0:
		return out + ", " + plural(int(m/60), "hour")
	default:
		return out + ", " + plural(int(m/60), "hour") + " " + plural(int(m%60), "minute")
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
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
// and CSS contract); StatusWord is its English word, empty when the status
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
	a.execute(w, r, http.StatusOK, file, data)
}

// execute writes the page file in the layout, with status. It needs no
// catalog: the sign-in page uses it directly.
func (a *API) execute(w http.ResponseWriter, r *http.Request, status int, file string, data pageData) {
	t, err := template.ParseFS(web.Assets, "layout.html", file)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
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
	d := pageData{Title: "Library", Nav: "library", Library: lib}
	switch {
	case f.Trashed:
		d.Title, d.Nav = "Trash", "trash"
	case f.Failed:
		d.Title, d.Nav = "Needs attention", "fix"
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

// statusWord is the word of a §10.3 status on every page, the Library's
// tiles and panel and the album page alike (NOTES.md N-256, N-270): the
// glossary of the UI principles in English. It is "" when the status needs
// no mention: Aligned («Up to date») is the norm, and Archived («In the
// trash») is the norm of the trash (N-248), so neither word is shown.
func statusWord(status string) string {
	switch status {
	case "Error":
		return "Needs attention"
	case "Processing":
		return "Updating"
	case "Queued":
		return "Waiting"
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
	ed := &editorData{Album: albumRep(v), ETag: ETag(KindAlbum, v.ID, v.Revision), Initials: initials(v.Title),
		CoverMB: coverMB(audioFormats(v))}
	if v.Genre != nil {
		ed.Genre = *v.Genre
	}
	for _, ar := range artists {
		ed.Artists = append(ed.Artists, pageArtist{ID: ar.ID.String(), Name: ar.Name, ETag: ETag(KindArtist, ar.ID, ar.Revision)})
	}
	if ed.ArtistAlbums, err = artistAlbums(r, c, v.ArtistID); err != nil {
		a.fail(w, r, err)
		return
	}
	d := pageData{Title: v.Title, Nav: "library", Back: true, Editor: ed}
	if v.Trashed {
		d.Nav = "trash"
	}
	var state *string
	if st.Job != nil {
		state = &st.Job.State
	}
	ed.Status = statusBadge(st.Trashed, st.Revision, st.PublishedRevision, st.PublishedRenderer, st.PublishedPath, state, a.renderVersion)
	ed.StatusWord = statusWord(ed.Status)
	ed.Pending = state != nil && (*state == "pending" || *state == "running")
	for _, t := range ed.Album.Tracks {
		if n := len(ed.Discs); n == 0 || ed.Discs[n-1].No != t.Disc {
			ed.Discs = append(ed.Discs, pageDisc{No: t.Disc})
		}
		ed.Discs[len(ed.Discs)-1].Tracks = append(ed.Discs[len(ed.Discs)-1].Tracks, trackRow(t))
	}
	ed.MultiDisc = len(ed.Discs) > 1
	ed.Length = albumLength(ed.Album.Tracks)
	for _, at := range ed.Album.Attachments {
		if coverCandidate(at) {
			ed.ImageAttachments = append(ed.ImageAttachments, at)
		}
		if strings.HasSuffix(strings.ToLower(at.RelPath), ".lrc") {
			ed.LyricsAttachments = append(ed.LyricsAttachments, at)
		}
	}
	a.renderPage(w, r, c, "album.html", d)
}

// artistAlbums counts the albums of an artist, active and trashed, page
// by page.
func artistAlbums(r *http.Request, c *catalog.Service, id uuid.UUID) (int, error) {
	n := 0
	for _, trashed := range []bool{false, true} {
		f := catalog.AlbumFilter{ArtistID: id, Trashed: trashed, Limit: catalog.MaxPageSize}
		for {
			page, err := c.ListAlbums(r.Context(), f)
			if err != nil {
				return 0, err
			}
			n += len(page.Albums)
			if page.Next == nil {
				break
			}
			f.After = page.Next
		}
	}
	return n, nil
}

// coverCandidate reports whether the cover picker offers an attachment
// (NOTES.md N-257, superseding N-209's "every attachment"): only what PUT
// /cover can accept, a JPEG or a PNG (§8.5). An attachment whose blob has a
// known format is a candidate exactly when it is one of the two. An
// uploaded attachment has no format hint (N-118, N-209): it is a candidate
// when its name says JPEG or PNG. PUT /cover still validates the bytes;
// the picker only leaves out what cannot be a cover.
func coverCandidate(at attachmentJSON) bool {
	if f := at.Blob.Format; f != nil {
		return *f == catalog.FormatJPEG || *f == catalog.FormatPNG
	}
	switch strings.ToLower(path.Ext(at.RelPath)) {
	case ".jpg", ".jpeg", ".png":
		return true
	}
	return false
}

// coverMB is the largest cover an album with tracks of these audio formats
// takes (§8.5 and N-091, the rule of PUT /cover), in whole decimal
// megabytes rounded down, so that the words never promise more than the
// server accepts: 16 with a FLAC track (16,777,173 bytes for a JPEG, one
// more for a PNG), 20 otherwise (20 MiB = 20,971,520 bytes).
func coverMB(formats []string) int64 {
	limit := int64(MaxCoverBytes)
	for _, f := range formats {
		if n, ok := media.MaxEmbeddedCover(f, media.FormatJPEG); ok && n < limit {
			limit = n
		}
	}
	return limit / 1_000_000
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
