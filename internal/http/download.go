package http

import (
	"io"
	nethttp "net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"musiclib/internal/catalog"
)

// Downloads by entity id (§10.2 last paragraph, NOTES.md N-175): the blob
// of a track, an attachment, the cover or a track's lyrics, found by the
// ids of the path within one snapshot of the album, never by a pathname
// the client sends. An id of another album's entity is 404, like an id
// that names nothing. Arbitrary files are attachments (Content-Disposition:
// attachment) with nosniff; only a JPEG or PNG the application validated
// (blobs.format, N-118) is shown inline. A PDF is served as
// application/pdf, still as an attachment: downloadable, and opened by the
// browser's own viewer. Ranges and HEAD work (http.ServeContent).

// Codes of the downloads.
const (
	// CodeCoverNotFound: the album has no cover (404).
	CodeCoverNotFound = "cover_not_found"
	// CodeLyricsNotFound: the track has no LRC file (404).
	CodeLyricsNotFound = "lyrics_not_found"
)

// contentTypes are the media types of the known blob formats (§4.2).
var contentTypes = map[string]string{
	catalog.FormatFLAC:    "audio/flac",
	catalog.FormatMP3:     "audio/mpeg",
	catalog.FormatM4AAAC:  "audio/mp4",
	catalog.FormatM4AALAC: "audio/mp4",
	catalog.FormatJPEG:    "image/jpeg",
	catalog.FormatPNG:     "image/png",
}

// inlineFormats are the formats shown inline: validated images only.
var inlineFormats = map[string]bool{catalog.FormatJPEG: true, catalog.FormatPNG: true}

// download is one blob to serve.
type download struct {
	blob catalog.BlobRef
	// name is the file name offered to the browser, from the catalog (the
	// original's base name, the attachment's, cover.jpg): never a path.
	name string
	// contentType overrides the type of the blob's format ("" keeps it).
	contentType string
}

// downloadOriginal is GET /api/albums/{id}/tracks/{track}/original: the
// track's original audio (§3.1), named as it was imported.
func (h *handlers) downloadOriginal(w nethttp.ResponseWriter, r *nethttp.Request) {
	v, ok := h.albumToRead(w, r)
	if !ok {
		return
	}
	t, ok := h.trackOf(w, r, v)
	if !ok {
		return
	}
	h.serve(w, r, download{blob: t.Blob, name: path.Base(t.SourcePath)})
}

// downloadLyrics is GET /api/albums/{id}/tracks/{track}/lyrics: the LRC
// file of a track, UTF-8 text, named after the track (§5.1).
func (h *handlers) downloadLyrics(w nethttp.ResponseWriter, r *nethttp.Request) {
	v, ok := h.albumToRead(w, r)
	if !ok {
		return
	}
	t, ok := h.trackOf(w, r, v)
	if !ok {
		return
	}
	if t.LyricsHash == nil {
		h.api.writeError(w, newError(nethttp.StatusNotFound, CodeLyricsNotFound, "track %s has no lyrics", t.ID))
		return
	}
	base := path.Base(t.SourcePath)
	h.serve(w, r, download{
		blob:        catalog.BlobRef{Hash: *t.LyricsHash, Size: -1},
		name:        strings.TrimSuffix(base, path.Ext(base)) + ".lrc",
		contentType: "text/plain; charset=utf-8",
	})
}

// downloadAttachment is GET
// /api/albums/{id}/attachments/{attachment}/content.
func (h *handlers) downloadAttachment(w nethttp.ResponseWriter, r *nethttp.Request) {
	v, ok := h.albumToRead(w, r)
	if !ok {
		return
	}
	id, ok := h.subID(w, r, "attachment", catalog.CodeAttachmentNotFound)
	if !ok {
		return
	}
	a, ok := findAttachment(v, id)
	if !ok {
		h.api.writeError(w, newError(nethttp.StatusNotFound, catalog.CodeAttachmentNotFound,
			"album %s has no attachment %s", v.ID, id))
		return
	}
	h.serve(w, r, download{blob: a.Blob, name: path.Base(a.RelPath)})
}

// downloadCover is GET /api/albums/{id}/cover: the album's cover, a
// validated JPEG or PNG, inline.
func (h *handlers) downloadCover(w nethttp.ResponseWriter, r *nethttp.Request) {
	v, ok := h.albumToRead(w, r)
	if !ok {
		return
	}
	if v.Cover == nil {
		h.api.writeError(w, newError(nethttp.StatusNotFound, CodeCoverNotFound, "album %s has no cover", v.ID))
		return
	}
	ext := "jpg"
	if v.Cover.Format == catalog.FormatPNG {
		ext = "png"
	}
	h.serve(w, r, download{blob: *v.Cover, name: "cover." + ext})
}

// albumToRead reads the album of the path in one snapshot.
func (h *handlers) albumToRead(w nethttp.ResponseWriter, r *nethttp.Request) (catalog.AlbumView, bool) {
	id, ok := h.pathID(w, r, KindAlbum)
	if !ok {
		return catalog.AlbumView{}, false
	}
	v, err := h.catalog.GetAlbum(r.Context(), id)
	if err != nil {
		h.api.fail(w, r, err)
		return catalog.AlbumView{}, false
	}
	return v, true
}

// trackOf finds the track of the path in the album.
func (h *handlers) trackOf(w nethttp.ResponseWriter, r *nethttp.Request, v catalog.AlbumView) (catalog.TrackView, bool) {
	id, ok := h.subID(w, r, "track", catalog.CodeTrackNotFound)
	if !ok {
		return catalog.TrackView{}, false
	}
	t, ok := findTrack(v, id)
	if !ok {
		h.api.writeError(w, newError(nethttp.StatusNotFound, catalog.CodeTrackNotFound, "album %s has no track %s", v.ID, id))
	}
	return t, ok
}

// serve sends a blob: its type, the disposition, and the bytes through
// http.ServeContent (ranges, HEAD). A blob missing or not matching its
// recorded size is the store's corruption (§11.3): 500, logged. The
// Content-Type is always set, so ServeContent never sniffs.
func (h *handlers) serve(w nethttp.ResponseWriter, r *nethttp.Request, d download) {
	f, err := h.blobs.Open(d.blob.Hash)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			h.api.log.Warn("closing a downloaded blob", "hash", d.blob.Hash, "error", err.Error())
		}
	}()
	fi, err := f.Stat()
	if err == nil && d.blob.Size >= 0 && fi.Size() != d.blob.Size {
		h.api.log.Error("a blob does not have its recorded size", "hash", d.blob.Hash,
			"recorded", d.blob.Size, "found", fi.Size())
		h.api.writeError(w, internalError())
		return
	}
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	ctype, inline := d.contentType, false
	if ctype == "" {
		ctype, inline = contentTypes[d.blob.Format], inlineFormats[d.blob.Format]
	}
	if ctype == "" {
		ctype = sniffDocument(f)
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", contentDisposition(inline, d.name))
	nethttp.ServeContent(w, r, "", time.Time{}, f)
}

// sniffDocument is the type of a blob without a known format: a PDF by its
// signature (§10.2: "PDF scaricabile e apribile nel browser"), anything
// else application/octet-stream. Nothing else is guessed.
func sniffDocument(f io.ReaderAt) string {
	head := make([]byte, 5)
	if n, _ := f.ReadAt(head, 0); n == len(head) && string(head) == "%PDF-" {
		return "application/pdf"
	}
	return "application/octet-stream"
}

// contentDisposition is the Content-Disposition of a download (RFC 6266):
// attachment, or inline for a validated image, with the file name twice:
// filename, an ASCII fallback in a quoted-string in which every character
// that is not printable ASCII, and '"', '\' and '%', is '_'; and
// filename*, the name in UTF-8 percent-encoded (RFC 8187), where only the
// attr-chars are literal. No byte of the name reaches the header
// unescaped, so a name cannot end the field (CR, LF) or add a parameter.
func contentDisposition(inline bool, name string) string {
	if name == "" || name == "." || name == "/" || !utf8.ValidString(name) {
		name = "download"
	}
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	var fallback, encoded strings.Builder
	for _, c := range name {
		if c < 0x20 || c >= 0x7f || c == '"' || c == '\\' || c == '%' {
			fallback.WriteByte('_')
		} else {
			fallback.WriteRune(c)
		}
	}
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(name); i++ {
		c := name[i]
		if isAttrChar(c) {
			encoded.WriteByte(c)
		} else {
			encoded.WriteString("%" + string(hex[c>>4]) + string(hex[c&15]))
		}
	}
	return kind + `; filename="` + fallback.String() + `"; filename*=UTF-8''` + encoded.String()
}

// isAttrChar is RFC 8187's attr-char: ALPHA, DIGIT and !#$&+-.^_`|~.
func isAttrChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("!#$&+-.^_`|~", c) >= 0
}
