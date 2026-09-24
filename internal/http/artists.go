package http

import (
	nethttp "net/http"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
)

// artistJSON is the representation of an artist (§10.2): name, revision
// and ETag, no derived counter.
type artistJSON struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
	ETag     string `json:"etag"`
}

func artistRep(a catalog.Artist) artistJSON {
	return artistJSON{ID: a.ID.String(), Name: a.Name, Revision: a.Revision, ETag: ETag(KindArtist, a.ID, a.Revision)}
}

type artistListJSON struct {
	Artists []artistJSON `json:"artists"`
}

// listArtists is GET /api/artists: every artist of the catalog, those
// without albums included (owner decision N-146), by folder key.
func (h *handlers) listArtists(w nethttp.ResponseWriter, r *nethttp.Request) {
	artists, err := h.catalog.ListArtists(r.Context())
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	out := artistListJSON{Artists: make([]artistJSON, len(artists))}
	for i, a := range artists {
		out.Artists[i] = artistRep(a)
	}
	h.api.writeJSON(w, nethttp.StatusOK, out)
}

// createArtist is POST /api/artists {"name"}: 201 with the new artist and
// its Location, or 409 artist_exists / artist_folder_conflict with the
// existing artist in details.artist (§10.2).
func (h *handlers) createArtist(w nethttp.ResponseWriter, r *nethttp.Request) {
	body, e := readObject(w, r, "name")
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	name, e := body.String("name")
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	a, err := h.catalog.CreateArtist(r.Context(), name)
	if err != nil {
		if a.ID != uuid.Nil {
			// A conflict: the existing artist, read in the same
			// transaction, goes with the error.
			e := translate(err)
			h.api.writeError(w, e.with("artist", artistRep(a)))
			return
		}
		h.api.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/artists/"+a.ID.String())
	h.api.writeJSON(w, nethttp.StatusCreated, artistRep(a))
}

// getArtist is GET /api/artists/{id}, with its ETag.
func (h *handlers) getArtist(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, ok := h.pathID(w, r, KindArtist)
	if !ok {
		return
	}
	a, err := h.catalog.GetArtist(r.Context(), id)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", ETag(KindArtist, a.ID, a.Revision))
	h.api.writeJSON(w, nethttp.StatusOK, artistRep(a))
}

// renameArtist is PUT /api/artists/{id} {"name"} with If-Match: the
// atomic rename of §4.3, which bumps and enqueues every album of the
// artist. 200 with the artist as it is after the change.
func (h *handlers) renameArtist(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, ok := h.pathID(w, r, KindArtist)
	if !ok {
		return
	}
	rev, e := ifMatch(r.Header, KindArtist, id)
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	body, e := readObject(w, r, "name")
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	name, e := body.String("name")
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	if _, _, err := h.catalog.RenameArtist(r.Context(), id, rev, name); err != nil {
		h.api.fail(w, r, err)
		return
	}
	a, err := h.catalog.GetArtist(r.Context(), id)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	h.api.writeJSON(w, nethttp.StatusOK, artistRep(a))
}

// pathID parses the {id} of the path. An id that is not in canonical form
// names no resource: 404 with the resource's not-found code.
func (h *handlers) pathID(w nethttp.ResponseWriter, r *nethttp.Request, kind string) (uuid.UUID, bool) {
	raw := r.PathValue("id")
	id, ok := parseID(raw)
	if !ok {
		code := catalog.CodeAlbumNotFound
		if kind == KindArtist {
			code = catalog.CodeArtistNotFound
		}
		h.api.writeError(w, newError(nethttp.StatusNotFound, code, "%s %q does not exist", kind, raw))
	}
	return id, ok
}
