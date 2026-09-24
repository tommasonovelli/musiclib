package http

import (
	"context"
	nethttp "net/http"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

// albumJSON is the desired aggregate of an album (§10.2 GET
// /api/albums/{id}): the fields of the PUT body, plus what is read-only
// here (the artist's name, the trash flag, cover, blobs, attachments), the
// revision and the ETag. No processing state: that is statusJSON.
type albumJSON struct {
	ID          string           `json:"id"`
	Revision    int64            `json:"revision"`
	ETag        string           `json:"etag"`
	ArtistID    string           `json:"artist_id"`
	ArtistName  string           `json:"artist_name"`
	Title       string           `json:"title"`
	Year        *int32           `json:"year"`
	Genre       *string          `json:"genre"`
	Compilation bool             `json:"compilation"`
	Trashed     bool             `json:"trashed"`
	Cover       *blobJSON        `json:"cover"`
	Tracks      []trackJSON      `json:"tracks"`
	Attachments []attachmentJSON `json:"attachments"`
}

// blobJSON is a referenced blob: its SHA-256, size and content-derived
// format (null for any other content, §4.2).
type blobJSON struct {
	Hash   string  `json:"hash"`
	Size   int64   `json:"size"`
	Format *string `json:"format"`
}

// trackJSON is one track. artist null inherits the album artist; genre
// null inherits the album genre, "" is explicitly none (§4.1).
type trackJSON struct {
	ID         string   `json:"id"`
	Disc       int32    `json:"disc"`
	No         int32    `json:"no"`
	Title      string   `json:"title"`
	Artist     *string  `json:"artist"`
	Genre      *string  `json:"genre"`
	SourcePath string   `json:"source_path"`
	Blob       blobJSON `json:"blob"`
	LyricsHash *string  `json:"lyrics_hash"`
}

type attachmentJSON struct {
	ID      string   `json:"id"`
	RelPath string   `json:"rel_path"`
	Blob    blobJSON `json:"blob"`
}

func blobRep(b catalog.BlobRef) blobJSON {
	out := blobJSON{Hash: b.Hash, Size: b.Size}
	if b.Format != "" {
		f := b.Format
		out.Format = &f
	}
	return out
}

func albumRep(v catalog.AlbumView) albumJSON {
	out := albumJSON{
		ID: v.ID.String(), Revision: v.Revision, ETag: ETag(KindAlbum, v.ID, v.Revision),
		ArtistID: v.ArtistID.String(), ArtistName: v.ArtistName, Title: v.Title, Year: v.Year, Genre: v.Genre,
		Compilation: v.Compilation, Trashed: v.Trashed,
		Tracks: make([]trackJSON, len(v.Tracks)), Attachments: make([]attachmentJSON, len(v.Attachments)),
	}
	if v.Cover != nil {
		c := blobRep(*v.Cover)
		out.Cover = &c
	}
	for i, t := range v.Tracks {
		out.Tracks[i] = trackJSON{
			ID: t.ID.String(), Disc: t.Disc, No: t.No, Title: t.Title, Artist: t.Artist, Genre: t.Genre,
			SourcePath: t.SourcePath, Blob: blobRep(t.Blob), LyricsHash: t.LyricsHash,
		}
	}
	for i, a := range v.Attachments {
		out.Attachments[i] = attachmentJSON{ID: a.ID.String(), RelPath: a.RelPath, Blob: blobRep(a.Blob)}
	}
	return out
}

// statusJSON is the processing state of an album (§10.2 GET
// /api/albums/{id}/status): a separate resource, without a catalog ETag,
// never cached.
type statusJSON struct {
	AlbumID           string   `json:"album_id"`
	Revision          int64    `json:"revision"`
	Trashed           bool     `json:"trashed"`
	PublishedRevision int64    `json:"published_revision"`
	PublishedRenderer *string  `json:"published_renderer"`
	PublishedPath     *string  `json:"published_path"`
	Renderer          string   `json:"renderer"`
	Job               *jobJSON `json:"job"`
}

type jobJSON struct {
	ID           string    `json:"id"`
	State        string    `json:"state"`
	ErrorCode    *string   `json:"error_code"`
	ErrorMessage *string   `json:"error_message"`
	QueuedAt     time.Time `json:"queued_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (h *handlers) statusRep(s catalog.AlbumStatus) statusJSON {
	out := statusJSON{
		AlbumID: s.AlbumID.String(), Revision: s.Revision, Trashed: s.Trashed,
		PublishedRevision: s.PublishedRevision, PublishedRenderer: s.PublishedRenderer,
		PublishedPath: s.PublishedPath, Renderer: h.api.renderVersion,
	}
	if j := s.Job; j != nil {
		out.Job = &jobJSON{
			ID: j.ID.String(), State: j.State, ErrorCode: j.ErrorCode, ErrorMessage: jobMessage(j.ErrorCode, j.ErrorMessage),
			QueuedAt: j.QueuedAt.UTC(), UpdatedAt: j.UpdatedAt.UTC(),
		}
	}
	return out
}

// jobMessage is a stored job message as the API shows it. The failure
// paths already store catalog.DatabaseJobMessage for a database error
// (catalog.JobMessage, N-150); a row whose code is a database code is
// shown with that message whatever it holds, so that no database text
// reaches the client even from a row written otherwise (§10.1).
func jobMessage(code, message *string) *string {
	if code == nil || message == nil {
		return message
	}
	switch *code {
	case catalog.CodeDB, jobs.CodeDB, store.CodeConnectionLost, store.CodeCommitUncertain,
		store.CodeRetriesExhausted, store.CodeCanceled:
		m := catalog.DatabaseJobMessage
		return &m
	}
	return message
}

// getAlbum is GET /api/albums/{id}, with its ETag.
func (h *handlers) getAlbum(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, ok := h.pathID(w, r, KindAlbum)
	if !ok {
		return
	}
	v, err := h.catalog.GetAlbum(r.Context(), id)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", ETag(KindAlbum, v.ID, v.Revision))
	h.api.writeJSON(w, nethttp.StatusOK, albumRep(v))
}

// albumStatus is GET /api/albums/{id}/status.
func (h *handlers) albumStatus(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, ok := h.pathID(w, r, KindAlbum)
	if !ok {
		return
	}
	s, err := h.catalog.GetAlbumStatus(r.Context(), id)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	h.api.writeJSON(w, nethttp.StatusOK, h.statusRep(s))
}

// The keys of the PUT body (§10.2).
var (
	albumKeys = []string{"artist_id", "title", "year", "genre", "compilation", "tracks"}
	trackKeys = []string{"id", "disc", "no", "title", "artist", "genre"}
)

// decodeAlbumUpdate reads the body of PUT /api/albums/{id}: exactly
// {artist_id, title, year, genre, compilation, tracks: [{id, disc, no,
// title, artist, genre}]}, every key present (null where allowed). Blobs
// and ids are not editable: they are not fields of the body. A track id
// listed twice is 422 duplicate_id (§10.1); whether the list is exactly the
// album's tracks, and every value, the catalog decides in its transaction.
func decodeAlbumUpdate(body object) (catalog.AlbumUpdate, *Error) {
	var u catalog.AlbumUpdate
	var e *Error
	if u.ArtistID, e = body.ID("artist_id"); e != nil {
		return u, e
	}
	if u.Title, e = body.String("title"); e != nil {
		return u, e
	}
	if u.Year, e = body.NullableInt("year"); e != nil {
		return u, e
	}
	if u.Genre, e = body.NullableString("genre"); e != nil {
		return u, e
	}
	if u.Compilation, e = body.Bool("compilation"); e != nil {
		return u, e
	}
	tracks, e := body.Objects("tracks", trackKeys...)
	if e != nil {
		return u, e
	}
	seen := make(map[uuid.UUID]bool, len(tracks))
	u.Tracks = make([]catalog.TrackUpdate, len(tracks))
	for i, t := range tracks {
		tu := &u.Tracks[i]
		if tu.ID, e = t.ID("id"); e != nil {
			return u, e
		}
		if seen[tu.ID] {
			return u, newError(nethttp.StatusUnprocessableEntity, CodeDuplicateID, "track %s is listed twice", tu.ID).
				with("id", tu.ID.String())
		}
		seen[tu.ID] = true
		if tu.Disc, e = t.Int("disc"); e != nil {
			return u, e
		}
		if tu.No, e = t.Int("no"); e != nil {
			return u, e
		}
		if tu.Title, e = t.String("title"); e != nil {
			return u, e
		}
		if tu.Artist, e = t.NullableString("artist"); e != nil {
			return u, e
		}
		if tu.Genre, e = t.NullableString("genre"); e != nil {
			return u, e
		}
	}
	return u, nil
}

// updateAlbum is PUT /api/albums/{id} with If-Match: the album's metadata
// and tracks in one transaction (§10.2); another artist_id is the album's
// reassignment (§4.3). 200 with the album as it is after the change.
func (h *handlers) updateAlbum(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, rev, ok := h.albumPrecondition(w, r)
	if !ok {
		return
	}
	body, e := readObject(w, r, albumKeys...)
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	u, e := decodeAlbumUpdate(body)
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	_, _, err := h.catalog.UpdateAlbum(r.Context(), id, rev, u)
	if catalog.Code(err) == catalog.CodeArtistNotFound {
		// The album exists; the artist it names does not: invalid content,
		// not a missing target.
		e := translate(err)
		e.Status = nethttp.StatusUnprocessableEntity
		h.api.writeError(w, e)
		return
	}
	h.albumChanged(w, r, id, err)
}

// trashAlbum is DELETE /api/albums/{id} with If-Match: the album goes to
// the trash (§4.3). 200 with the album.
func (h *handlers) trashAlbum(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.albumCommand(w, r, h.catalog.TrashAlbum)
}

// restoreAlbum is POST /api/albums/{id}/restore with If-Match (§4.3).
func (h *handlers) restoreAlbum(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.albumCommand(w, r, h.catalog.RestoreAlbum)
}

// albumCommand runs a bodiless change of an album with the revision of
// If-Match, then answers the album.
func (h *handlers) albumCommand(w nethttp.ResponseWriter, r *nethttp.Request,
	change func(ctx context.Context, id uuid.UUID, ifMatch int64) (int64, bool, error)) {
	id, rev, ok := h.albumPrecondition(w, r)
	if !ok {
		return
	}
	if e := refuseBody(w, r); e != nil {
		h.api.writeError(w, e)
		return
	}
	_, _, err := change(r.Context(), id, rev)
	h.albumChanged(w, r, id, err)
}

// renderAlbum is POST /api/albums/{id}/render with If-Match: the forced
// render of §10.2, without a new revision. 202 with the status.
func (h *handlers) renderAlbum(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, rev, ok := h.albumPrecondition(w, r)
	if !ok {
		return
	}
	if e := refuseBody(w, r); e != nil {
		h.api.writeError(w, e)
		return
	}
	if _, _, err := h.catalog.RequestRender(r.Context(), id, rev); err != nil {
		h.api.fail(w, r, err)
		return
	}
	s, err := h.catalog.GetAlbumStatus(r.Context(), id)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	h.api.writeJSON(w, nethttp.StatusAccepted, h.statusRep(s))
}

// albumPrecondition reads the album id of the path and the revision of
// If-Match, the album's ETag (§10.1, §10.2).
func (h *handlers) albumPrecondition(w nethttp.ResponseWriter, r *nethttp.Request) (uuid.UUID, int64, bool) {
	id, ok := h.pathID(w, r, KindAlbum)
	if !ok {
		return uuid.Nil, 0, false
	}
	rev, e := ifMatch(r.Header, KindAlbum, id)
	if e != nil {
		h.api.writeError(w, e)
		return uuid.Nil, 0, false
	}
	return id, rev, true
}

// albumChanged answers a change of an album: the error, or the album as
// it is now. The representation carries its revision and ETag in the
// body; the response has no ETag field, since the saved texts may differ
// from the request's by normalization (RFC 9110 §9.3.4).
func (h *handlers) albumChanged(w nethttp.ResponseWriter, r *nethttp.Request, id uuid.UUID, err error) {
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	v, err := h.catalog.GetAlbum(r.Context(), id)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	h.api.writeJSON(w, nethttp.StatusOK, albumRep(v))
}
