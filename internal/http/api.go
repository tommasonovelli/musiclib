// Package http is the HTTP API of DESIGN.md §10 (§2.3: "handler
// HTML/JSON, validazione delle richieste"): the conventions of §10.1, the
// security boundary of §10.4, and the endpoints of §10.2 whose domain
// exists.
//
// Handlers validate and translate, nothing else. Every catalog write goes
// through the transactional services of internal/catalog (§13.2), which
// compare the revision of If-Match in the transaction of the change
// (§10.1); every read is one snapshot of the catalog. The package knows no
// SQL, no absolute path and no job framework, and uses the standard
// library's router only.
//
// The API is mounted under /api by cmd/musiclibd from the first moment of
// the boot (§11.1 step 1), and answers 503 until Enable hands it the
// catalog at the end of the boot; Disable makes it answer 503 again at
// shutdown, while publishing is suspended (N-135), or after a fatal
// database error (§6.4).
package http

import (
	"log/slog"
	nethttp "net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// RequestHeader is the header every mutating request must carry, with the
// value "1" (§10.4). A browser page of another origin cannot send it
// without a CORS preflight, which this server never grants.
const RequestHeader = "X-Musiclib-Request"

// Config are the API's dependencies and settings.
type Config struct {
	// PublicOrigin is the canonical origin of the server (§10.4, §11.1),
	// as cmd/musiclibd validates it: scheme, lowercase host, optional
	// non-default port, nothing else.
	PublicOrigin string
	// RenderVersion is the renderer of this binary (§2.1), reported by the
	// status of an album.
	RenderVersion string
	// Fatal is called once with the first fatal database error an API
	// request meets (store.IsFatal: a lost connection or an uncertain
	// commit). §6.4: the process stops its mutations and its workers and
	// restarts.
	Fatal func(error)
	Log   *slog.Logger
	// Failpoints is nil in production. Tests set it to act at the named
	// point of an upload: upload_pinned, after the blob is pinned and before
	// the catalog transaction (§12.2, NOTES.md N-142, N-173).
	Failpoints failpoint.Hook
}

// Backend is what the API serves: the catalog, and for the uploads and
// downloads of §10.2 the blob store (§7.5: the single put primitive), the
// process's space budget and the work root whose free space the budget
// compares (§11.2).
type Backend struct {
	Catalog *catalog.Service
	Blobs   *blobstore.Store
	Budget  *jobs.Budget
	Work    *fsops.Root
}

// API serves /api. It is safe for concurrent use.
type API struct {
	origin        string
	host          string
	renderVersion string
	fatal         func(error)
	fatalOnce     sync.Once
	log           *slog.Logger
	failpoints    failpoint.Hook

	// state is what /api does now: serve with a catalog, or refuse with
	// 503 and a code.
	state atomic.Pointer[state]
}

// state is either a router over a catalog, or the 503 to answer.
type state struct {
	router  nethttp.Handler
	code    string
	message string
}

// New returns the API, answering 503 not_ready until Enable.
func New(cfg Config) (*API, error) {
	u, err := url.Parse(cfg.PublicOrigin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Scheme+"://"+u.Host != cfg.PublicOrigin {
		return nil, newError(0, CodeInvalidConfig, "PUBLIC_ORIGIN %q is not a canonical origin", cfg.PublicOrigin)
	}
	if cfg.RenderVersion == "" || cfg.Fatal == nil || cfg.Log == nil {
		return nil, newError(0, CodeInvalidConfig, "the API needs the render version, the fatal-error hook and a logger")
	}
	a := &API{origin: cfg.PublicOrigin, host: u.Host, renderVersion: cfg.RenderVersion, fatal: cfg.Fatal, log: cfg.Log,
		failpoints: cfg.Failpoints}
	a.Disable(CodeNotReady, "boot or recovery in progress: try again shortly")
	return a, nil
}

// Enable makes the API serve b: the end of the boot (§11.1 step 7).
func (a *API) Enable(b Backend) {
	a.state.Store(&state{router: a.routes(b)})
}

// Disable makes every /api request answer 503 with code and message: at
// shutdown (§11.1: no mutation is accepted any more), while publishing is
// suspended (N-135), after a fatal database error (§6.4). Requests already
// running finish.
func (a *API) Disable(code, message string) {
	a.state.Store(&state{code: code, message: message})
}

// ServeHTTP is the whole of /api: the security headers, the boundary of
// §10.4, the availability, then the route.
func (a *API) ServeHTTP(w nethttp.ResponseWriter, r *nethttp.Request) {
	setSecurityHeaders(w.Header())
	w.Header().Set("Cache-Control", "no-store")
	if e := a.checkBoundary(r); e != nil {
		a.writeError(w, e)
		return
	}
	// The router would redirect a path that is not clean, or /api to
	// /api/; the API has no such aliases.
	if p := r.URL.Path; path.Clean(p) != p || !strings.HasPrefix(p, "/api/") {
		a.writeError(w, notFound())
		return
	}
	st := a.state.Load()
	if st.router == nil {
		a.writeError(w, newError(nethttp.StatusServiceUnavailable, st.code, "%s", st.message))
		return
	}
	st.router.ServeHTTP(w, r)
}

// SecurityHeaders sets the response headers of §10.4 on every response of
// next: the health endpoints and anything else the server answers outside
// /api. No CORS header is ever set.
func SecurityHeaders(next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		setSecurityHeaders(w.Header())
		next.ServeHTTP(w, r)
	})
}

func setSecurityHeaders(h nethttp.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
}

// checkBoundary is §10.4 (NOTES.md N-145):
//   - Host must be the host of PUBLIC_ORIGIN (ASCII case-insensitive): 421
//     host_not_allowed otherwise, the defense against DNS rebinding;
//   - Origin, if present, must be PUBLIC_ORIGIN exactly: "null", another
//     origin, an empty value or two Origin fields are 403
//     origin_not_allowed;
//   - a request other than GET and HEAD must carry X-Musiclib-Request: 1,
//     once: 403 request_header_required otherwise. Non-browser clients may
//     omit Origin, not this header.
func (a *API) checkBoundary(r *nethttp.Request) *Error {
	if !strings.EqualFold(r.Host, a.host) {
		return newError(nethttp.StatusMisdirectedRequest, CodeHostNotAllowed,
			"this server answers only for its PUBLIC_ORIGIN %s", a.origin)
	}
	if origins := r.Header.Values("Origin"); len(origins) > 1 || (len(origins) == 1 && origins[0] != a.origin) {
		return newError(nethttp.StatusForbidden, CodeOriginNotAllowed,
			"requests from another origin are refused; this server's origin is %s", a.origin)
	}
	if r.Method != nethttp.MethodGet && r.Method != nethttp.MethodHead {
		if v := r.Header.Values(RequestHeader); len(v) != 1 || v[0] != "1" {
			return newError(nethttp.StatusForbidden, CodeRequestHeaderRequired,
				"a %s request must carry the header %s: 1", r.Method, RequestHeader)
		}
	}
	return nil
}

// fail answers err, an error of the catalog. A fatal database error
// (§6.4) also disables the API and reports the error once, so that the
// process restarts; an unexpected error is logged with its cause, which
// the client never sees.
func (a *API) fail(w nethttp.ResponseWriter, r *nethttp.Request, err error) {
	e := translate(err)
	if store.IsFatal(err) {
		a.Disable(e.Code, e.Message)
		a.fatalOnce.Do(func() {
			a.log.Error("fatal database error in an API request", "code", e.Code, "method", r.Method,
				"path", r.URL.Path, "error", err.Error())
			a.fatal(err)
		})
	} else if e.Status >= nethttp.StatusInternalServerError {
		a.log.Error("API request failed", "code", e.Code, "method", r.Method, "path", r.URL.Path,
			"error", err.Error())
	}
	a.writeError(w, e)
}

func notFound() *Error {
	return newError(nethttp.StatusNotFound, CodeNotFound, "no such API endpoint")
}

// routes is the router of the endpoints of this phase (§10.2). Every path
// also has a pattern without a method, so that a wrong method is a JSON
// 405 with Allow, and /api/ catches the rest as a JSON 404.
func (a *API) routes(b Backend) nethttp.Handler {
	h := &handlers{api: a, catalog: b.Catalog, blobs: b.Blobs, budget: b.Budget, work: b.Work}
	mux := nethttp.NewServeMux()
	route := func(pattern string, methods map[string]nethttp.HandlerFunc) {
		var allow []string
		for _, m := range []string{nethttp.MethodGet, nethttp.MethodPost, nethttp.MethodPut, nethttp.MethodDelete} {
			fn, ok := methods[m]
			if !ok {
				continue
			}
			// A GET pattern serves HEAD as well.
			if m == nethttp.MethodGet {
				allow = append(allow, "GET", "HEAD")
			} else {
				allow = append(allow, m)
			}
			mux.HandleFunc(m+" "+pattern, fn)
		}
		allowed := strings.Join(allow, ", ")
		mux.HandleFunc(pattern, func(w nethttp.ResponseWriter, r *nethttp.Request) {
			w.Header().Set("Allow", allowed)
			a.writeError(w, newError(nethttp.StatusMethodNotAllowed, CodeMethodNotAllowed,
				"%s is not allowed here; allowed: %s", r.Method, allowed))
		})
	}
	route("/api/artists", map[string]nethttp.HandlerFunc{
		nethttp.MethodGet:  h.listArtists,
		nethttp.MethodPost: h.createArtist,
	})
	route("/api/artists/{id}", map[string]nethttp.HandlerFunc{
		nethttp.MethodGet: h.getArtist,
		nethttp.MethodPut: h.renameArtist,
	})
	route("/api/albums/{id}", map[string]nethttp.HandlerFunc{
		nethttp.MethodGet:    h.getAlbum,
		nethttp.MethodPut:    h.updateAlbum,
		nethttp.MethodDelete: h.trashAlbum,
	})
	route("/api/albums/{id}/status", map[string]nethttp.HandlerFunc{
		nethttp.MethodGet: h.albumStatus,
	})
	route("/api/albums/{id}/restore", map[string]nethttp.HandlerFunc{
		nethttp.MethodPost: h.restoreAlbum,
	})
	route("/api/albums/{id}/render", map[string]nethttp.HandlerFunc{
		nethttp.MethodPost: h.renderAlbum,
	})
	// The editor's content (§10.2, round 14): cover, attachments, lyrics,
	// track deletion, and the downloads by entity id.
	route("/api/albums/{id}/cover", map[string]nethttp.HandlerFunc{
		nethttp.MethodGet:    h.downloadCover,
		nethttp.MethodPut:    h.putCover,
		nethttp.MethodDelete: h.deleteCover,
	})
	route("/api/albums/{id}/attachments", map[string]nethttp.HandlerFunc{
		nethttp.MethodPost: h.postAttachment,
	})
	route("/api/albums/{id}/attachments/{attachment}", map[string]nethttp.HandlerFunc{
		nethttp.MethodDelete: h.deleteAttachment,
	})
	route("/api/albums/{id}/attachments/{attachment}/content", map[string]nethttp.HandlerFunc{
		nethttp.MethodGet: h.downloadAttachment,
	})
	route("/api/albums/{id}/tracks/{track}", map[string]nethttp.HandlerFunc{
		nethttp.MethodDelete: h.deleteTrack,
	})
	route("/api/albums/{id}/tracks/{track}/original", map[string]nethttp.HandlerFunc{
		nethttp.MethodGet: h.downloadOriginal,
	})
	route("/api/albums/{id}/tracks/{track}/lyrics", map[string]nethttp.HandlerFunc{
		nethttp.MethodGet:    h.downloadLyrics,
		nethttp.MethodPut:    h.putLyrics,
		nethttp.MethodDelete: h.deleteLyrics,
	})
	mux.HandleFunc("/api/", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		a.writeError(w, notFound())
	})
	return mux
}

// handlers are the endpoints over one backend.
type handlers struct {
	api     *API
	catalog *catalog.Service
	blobs   *blobstore.Store
	budget  *jobs.Budget
	work    *fsops.Root
}
