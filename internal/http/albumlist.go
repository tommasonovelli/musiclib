package http

import (
	"encoding/base64"
	"encoding/json"
	nethttp "net/http"
	"strings"
	"unicode/utf8"

	"musiclib/internal/catalog"
)

// albumSummaryJSON is an album in GET /api/albums (N-190): the fields of
// the album's representation (N-150) without its tracks and attachments,
// with its revision and ETag, so that a list action (trash, restore) can
// send If-Match without reading the album first. No processing state:
// that is the album's status (§10.1).
type albumSummaryJSON struct {
	ID          string    `json:"id"`
	Revision    int64     `json:"revision"`
	ETag        string    `json:"etag"`
	ArtistID    string    `json:"artist_id"`
	ArtistName  string    `json:"artist_name"`
	Title       string    `json:"title"`
	Year        *int32    `json:"year"`
	Genre       *string   `json:"genre"`
	Compilation bool      `json:"compilation"`
	Trashed     bool      `json:"trashed"`
	Cover       *blobJSON `json:"cover"`
}

// albumListJSON is a page: the albums in the library's order, and the
// cursor of the next page (null on the last one).
type albumListJSON struct {
	Albums []albumSummaryJSON `json:"albums"`
	Next   *string            `json:"next"`
}

func albumSummaryRep(a catalog.AlbumSummary) albumSummaryJSON {
	out := albumSummaryJSON{
		ID: a.ID.String(), Revision: a.Revision, ETag: ETag(KindAlbum, a.ID, a.Revision),
		ArtistID: a.ArtistID.String(), ArtistName: a.ArtistName, Title: a.Title, Year: a.Year, Genre: a.Genre,
		Compilation: a.Compilation, Trashed: a.Trashed,
	}
	if a.Cover != nil {
		c := blobRep(*a.Cover)
		out.Cover = &c
	}
	return out
}

// listAlbums is GET /api/albums (§10.2): the query parameters are
//
//	q      the search text, by title or artist (N-191)
//	artist an artist id: that artist's albums only
//	trash  "true" for the trash, "false" (the default) for the library
//	limit  1..200, default 50 (N-192)
//	after  the cursor "next" of the previous page
func (h *handlers) listAlbums(w nethttp.ResponseWriter, r *nethttp.Request) {
	q, e := queryParams(r, "q", "artist", "trash", "limit", "after")
	var f catalog.AlbumFilter
	if e == nil {
		f.Query = q["q"]
		f.ArtistID, e = queryID(q, "artist")
	}
	if e == nil {
		switch v, ok := q["trash"]; {
		case !ok || v == "false":
		case v == "true":
			f.Trashed = true
		default:
			e = invalidField("trash", `must be "true" or "false"`)
		}
	}
	if e == nil {
		f.Limit, e = pageLimit(q)
	}
	if v, ok := q["after"]; ok && e == nil {
		f.After, e = decodeAlbumCursor(v)
	}
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	page, err := h.catalog.ListAlbums(r.Context(), f)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	out := albumListJSON{Albums: make([]albumSummaryJSON, len(page.Albums))}
	for i, a := range page.Albums {
		out.Albums[i] = albumSummaryRep(a)
	}
	if page.Next != nil {
		c := encodeAlbumCursor(*page.Next)
		out.Next = &c
	}
	h.api.writeJSON(w, nethttp.StatusOK, out)
}

// encodeAlbumCursor is the opaque "next" of a page: the base64url (no
// padding) of the JSON array [artist key, title key, id]. It carries only
// normalized keys and an id, nothing a client should read.
func encodeAlbumCursor(c catalog.AlbumCursor) string {
	b, err := json.Marshal([3]string{c.ArtistKey, c.TitleKey, c.ID.String()})
	if err != nil {
		// Three strings always marshal.
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeAlbumCursor accepts exactly what encodeAlbumCursor writes: a
// cursor that does not encode back to the same text is refused (422), so
// that one position has one spelling. A key holding NUL or invalid UTF-8
// is refused too: no folder key holds either (§5.2), and PostgreSQL would
// reject such a text parameter with a server error (SQLSTATE 22021).
func decodeAlbumCursor(s string) (*catalog.AlbumCursor, *Error) {
	refuse := invalidField("after", `must be the "next" cursor of a previous page`)
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, refuse
	}
	var parts [3]string
	var raw []json.RawMessage
	if json.Unmarshal(b, &raw) != nil || len(raw) != 3 || json.Unmarshal(b, &parts) != nil {
		return nil, refuse
	}
	for _, k := range parts[:2] {
		if !utf8.ValidString(k) || strings.IndexByte(k, 0) >= 0 {
			return nil, refuse
		}
	}
	id, ok := parseID(parts[2])
	if !ok {
		return nil, refuse
	}
	c := catalog.AlbumCursor{ArtistKey: parts[0], TitleKey: parts[1], ID: id}
	if encodeAlbumCursor(c) != s {
		return nil, refuse
	}
	return &c, nil
}
