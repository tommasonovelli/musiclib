package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	nethttp "net/http"
	"net/url"
	"unicode/utf8"

	"github.com/google/uuid"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/importer"
	"musiclib/internal/media"
)

// The album editor's content endpoints of §10.2 (round 14): the cover,
// the attachments, the lyrics of a track, the addition and the deletion
// of a track.
// Every one is a change of the album and requires the album's ETag in
// If-Match (§10.2), compared by the catalog in the transaction of the
// change (N-147); every one answers the album as it is after the change,
// like PUT /api/albums/{id} (N-150).
//
// The order of the checks (N-173): the ids of the path (404), If-Match
// (428, 400), the media type (415), the query (422), a declared length
// over the limit (413); then, for a body that is read or a blob that is
// pinned, the album on a snapshot (404, 412, and 404 or 409 for what the
// request names); then one of the MaxConcurrentUploads copy slots (§6.1,
// N-199), held while the body is read, checked and pinned; the body (413,
// 422); the space (507); the put; for a track, its reading with the
// import's checks, outside the slot (409, 422); the transaction, which
// decides (404, 412, 409, 422).

// attachmentKeys are the keys of the JSON body that chooses an attachment.
var attachmentKeys = []string{"attachment_id"}

// putCover is PUT /api/albums/{id}/cover: an upload of a JPEG or PNG, or
// {"attachment_id"} to choose an image attachment of the album (§10.2).
// §8.5: valid JPEG or PNG, at most 20 MiB and 40 Mpixel, bytes kept as
// they are; N-091: embeddable in every audio format of the album (422
// cover_not_embeddable otherwise). A chosen attachment stays an
// attachment (§7.4); an upload is only the cover (N-174).
func (h *handlers) putCover(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, rev, ok := h.albumPrecondition(w, r)
	if !ok {
		return
	}
	kind, e := choiceOrUpload(r.Header)
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	var choice catalog.CoverChoice
	if kind == bodyChoice {
		if choice, ok = h.coverAttachment(w, r, id, rev); !ok {
			return
		}
	} else if choice, ok = h.coverUpload(w, r, id, rev); !ok {
		return
	}
	_, _, err := h.catalog.SetCover(r.Context(), id, rev, choice)
	h.albumChanged(w, r, id, err)
}

// coverAttachment reads {"attachment_id"}, finds the attachment in the
// album and validates its image (§8.5): an image that is not a valid
// cover stays an attachment and is refused as the cover (N-091).
func (h *handlers) coverAttachment(w nethttp.ResponseWriter, r *nethttp.Request, id uuid.UUID, rev int64) (catalog.CoverChoice, bool) {
	att, _, ok := h.chosenAttachment(w, r, id, rev)
	if !ok {
		return catalog.CoverChoice{}, false
	}
	f, err := h.blobs.Open(att.Blob.Hash)
	if err != nil {
		h.api.fail(w, r, err)
		return catalog.CoverChoice{}, false
	}
	format, e, err := validateCover(f, att.Blob.Size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		h.api.fail(w, r, err)
		return catalog.CoverChoice{}, false
	case e != nil:
		h.api.writeError(w, e.with("attachment_id", att.ID.String()))
		return catalog.CoverChoice{}, false
	}
	return catalog.CoverChoice{
		Blob:       catalog.Blob{Hash: att.Blob.Hash, Size: att.Blob.Size, Format: format},
		Attachment: att.ID,
	}, true
}

// coverUpload reads an uploaded cover (at most MaxCoverBytes), validates
// it (§8.5) and pins it.
func (h *handlers) coverUpload(w nethttp.ResponseWriter, r *nethttp.Request, id uuid.UUID, rev int64) (catalog.CoverChoice, bool) {
	if e := declaredTooLarge(r, MaxCoverBytes); e != nil {
		h.api.writeError(w, e)
		return catalog.CoverChoice{}, false
	}
	v, ok := h.snapshot(w, r, id, rev)
	if !ok {
		return catalog.CoverChoice{}, false
	}
	release, ok := h.acquireUpload(w, r)
	if !ok {
		return catalog.CoverChoice{}, false
	}
	defer release()
	data, e := readUpload(w, r, MaxCoverBytes)
	if e != nil {
		h.api.writeError(w, e)
		return catalog.CoverChoice{}, false
	}
	format, e, err := validateCover(bytes.NewReader(data), int64(len(data)))
	if e == nil && err == nil {
		// N-091 on the snapshot, before the put (N-173): a valid image
		// the album cannot embed leaves no blob.
		err = h.catalog.CheckCoverFits(catalog.Blob{Hash: sha256Hex(data), Size: int64(len(data)), Format: format}, audioFormats(v))
	}
	if e == nil && err == nil {
		var b catalog.Blob
		if b, e, err = h.pinUpload(r, bytes.NewReader(data), int64(len(data)), MaxCoverBytes); e == nil && err == nil {
			b.Format = format
			return catalog.CoverChoice{Blob: b}, true
		}
	}
	if err != nil {
		h.api.fail(w, r, err)
	} else {
		h.api.writeError(w, e)
	}
	return catalog.CoverChoice{}, false
}

// deleteCover is DELETE /api/albums/{id}/cover: the album has no cover
// any more, in the output and in the tags (§10.2). An album without a
// cover is a no-op.
func (h *handlers) deleteCover(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.albumCommand(w, r, h.catalog.RemoveCover)
}

// postAttachment is POST /api/albums/{id}/attachments?path=<relative
// path>: the body is the file (UploadMediaType, at most 256 MiB, §10.2),
// materialized under Extras/<path> (§5.1). The path is validated with the
// one normalization of §5.2 before any byte is read, and must not collide
// with another attachment's (409). 201 with the album, and the new
// attachment's content as Location.
func (h *handlers) postAttachment(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, rev, ok := h.albumPrecondition(w, r)
	if !ok {
		return
	}
	if e := checkUploadType(r.Header); e != nil {
		h.api.writeError(w, e)
		return
	}
	relPath, e := attachmentQuery(r)
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	if _, err := catalog.AttachmentPath(relPath); err != nil {
		h.api.fail(w, r, err)
		return
	}
	if e := declaredTooLarge(r, MaxAttachmentBytes); e != nil {
		h.api.writeError(w, e)
		return
	}
	v, ok := h.snapshot(w, r, id, rev)
	if !ok {
		return
	}
	existing := make([]string, len(v.Attachments))
	for i, a := range v.Attachments {
		existing[i] = a.RelPath
	}
	if err := catalog.AttachmentConflict(existing, relPath); err != nil {
		h.api.fail(w, r, err)
		return
	}
	release, ok := h.acquireUpload(w, r)
	if !ok {
		return
	}
	// The token goes back before the catalog transaction (N-199), and
	// also if pinUpload panics (net/http recovers it).
	b, e, err := func() (catalog.Blob, *Error, error) {
		defer release()
		return h.pinUpload(r, uploadReader(w, r, MaxAttachmentBytes), estimateOf(r, MaxAttachmentBytes), MaxAttachmentBytes)
	}()
	switch {
	case err != nil:
		h.api.fail(w, r, err)
		return
	case e != nil:
		h.api.writeError(w, e)
		return
	}
	_, att, err := h.catalog.AddAttachment(r.Context(), id, rev, relPath, b)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/albums/"+id.String()+"/attachments/"+att.String()+"/content")
	v, err = h.catalog.GetAlbum(r.Context(), id)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	h.api.writeJSON(w, nethttp.StatusCreated, albumRep(v))
}

// attachmentQuery reads the one query parameter of an attachment upload,
// path: exactly once, and nothing else (N-172). As in any query string, a
// "+" is a space: a literal plus is sent as %2B.
func attachmentQuery(r *nethttp.Request) (string, *Error) {
	return uploadQuery(r, "path", "the attachment's relative path")
}

// uploadQuery reads the one query parameter of an upload, key: exactly
// once, and nothing else. what says what it is.
func uploadQuery(r *nethttp.Request, key, what string) (string, *Error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", invalidField(key, "must be one query parameter, URL-encoded ("+key+"=<"+what+">)")
	}
	for k := range q {
		if k != key {
			return "", newError(nethttp.StatusUnprocessableEntity, CodeUnknownField, "unknown query parameter %q", k).
				with("field", k)
		}
	}
	switch vs := q[key]; len(vs) {
	case 0:
		return "", newError(nethttp.StatusUnprocessableEntity, CodeMissingField,
			"the query parameter %q (%s) is required", key, what).with("field", key)
	case 1:
		return vs[0], nil
	default:
		return "", invalidField(key, "must be given once")
	}
}

// postTrack is POST /api/albums/{id}/tracks?name=<file name>: the body is
// an audio file (UploadMediaType, at most MaxTrackBytes), added to the
// album as a new track. The name is reduced to its last segment and
// validated before any byte is read; it is the track's source path and,
// without a title tag, its title. Once pinned, and the upload slot given
// back, the file is read with exactly the checks of an import
// (TrackReader: the probe, the complete decode, the tags), outside any
// transaction: a file that cannot be a track is 422 with the importer's
// code. A file already a track of the album is 409 track_exists. 201
// with the album, its warnings (as an import reports them), and the new
// track's original as Location.
func (h *handlers) postTrack(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, rev, ok := h.albumPrecondition(w, r)
	if !ok {
		return
	}
	if e := checkUploadType(r.Header); e != nil {
		h.api.writeError(w, e)
		return
	}
	raw, e := uploadQuery(r, "name", "the file's name")
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	name, err := catalog.TrackFileName(raw)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	if e := declaredTooLarge(r, MaxTrackBytes); e != nil {
		h.api.writeError(w, e)
		return
	}
	v, ok := h.snapshot(w, r, id, rev)
	if !ok {
		return
	}
	release, ok := h.acquireUpload(w, r)
	if !ok {
		return
	}
	// The upload slot goes back before the decode and the transaction: it
	// covers only the copy of the body.
	b, e, err := func() (catalog.Blob, *Error, error) {
		defer release()
		return h.pinUpload(r, uploadReader(w, r, MaxTrackBytes), estimateOf(r, MaxTrackBytes), MaxTrackBytes)
	}()
	if err == nil && e == nil {
		// On the snapshot, before the decode; AddTrack decides.
		err = catalog.CheckTrackAbsent(v, b.Hash)
	}
	switch {
	case err != nil:
		h.api.fail(w, r, err)
		return
	case e != nil:
		h.api.writeError(w, e)
		return
	}
	info, ws, err := h.tracks.ReadTrack(r.Context(), blobstore.Blob{SHA256: b.Hash, Size: b.Size}, name)
	if err != nil {
		h.trackRefused(w, r, err)
		return
	}
	b.Format = info.Format
	if ms, ok := media.DurationMS(info.Duration); ok {
		b.DurationMS = &ms
	}
	_, track, err := h.catalog.AddTrack(r.Context(), id, rev, catalog.NewTrack{
		Blob: b, SourcePath: name, Title: info.Title, Artist: info.Artist, Genre: info.Genre, Disc: info.Disc, No: info.No,
	})
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/albums/"+id.String()+"/tracks/"+track.String()+"/original")
	v, err = h.catalog.GetAlbum(r.Context(), id)
	if err != nil {
		h.api.fail(w, r, err)
		return
	}
	out := addedTrackJSON{albumJSON: albumRep(v), Warnings: make([]warningJSON, len(ws))}
	for i, wn := range ws {
		out.Warnings[i] = warningJSON{Code: string(wn.Code), Message: wn.Message}
		if wn.Path != "" {
			p := wn.Path
			out.Warnings[i].Path = &p
		}
	}
	h.api.writeJSON(w, nethttp.StatusCreated, out)
}

// addedTrackJSON is the answer of POST /api/albums/{id}/tracks: the album
// after the change and the warnings of the file's reading, as an import
// reports them (an empty list without any).
type addedTrackJSON struct {
	albumJSON
	Warnings []warningJSON `json:"warnings"`
}

// trackRefusals are the importer's codes of a file that cannot be a track,
// as at an import: 422 with the importer's message. Any other failure of
// the reading (a tool's timeout or failure, a corrupt blob) is the
// server's.
var trackRefusals = map[string]bool{
	importer.CodeCorruptAudio: true, importer.CodeUnsupportedAudio: true, importer.CodeUnrenderableTag: true,
	importer.CodeInvalidTag: true, catalog.CodeInvalidDisc: true, catalog.CodeInvalidTrackNumber: true,
}

// trackRefused answers a failure of TrackReader.ReadTrack.
func (h *handlers) trackRefused(w nethttp.ResponseWriter, r *nethttp.Request, err error) {
	var ie *importer.Error
	if errors.As(err, &ie) && trackRefusals[ie.Code] {
		h.api.writeError(w, newError(nethttp.StatusUnprocessableEntity, ie.Code, "%s", ie.Message).with("path", ie.Path))
		return
	}
	h.api.fail(w, r, err)
}

// deleteAttachment is DELETE /api/albums/{id}/attachments/{attachment}:
// the attachment leaves the catalog and the output, with no undo; its blob
// stays (§4.3). The cover, if it is the same image, stays (N-176).
func (h *handlers) deleteAttachment(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, rev, ok := h.albumPrecondition(w, r)
	if !ok {
		return
	}
	att, ok := h.subID(w, r, "attachment", catalog.CodeAttachmentNotFound)
	if !ok {
		return
	}
	if e := refuseBody(w, r); e != nil {
		h.api.writeError(w, e)
		return
	}
	_, _, err := h.catalog.DeleteAttachment(r.Context(), id, rev, att)
	h.albumChanged(w, r, id, err)
}

// deleteTrack is DELETE /api/albums/{id}/tracks/{track}: the confirmed
// removal of a track (§4.3, §10.2), with no undo; the blob stays. The last
// track of an album cannot be removed (422 no_tracks).
func (h *handlers) deleteTrack(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.trackCommand(w, r, h.catalog.DeleteTrack)
}

// moveJSON is the answer of a move of tracks: both albums as they are
// after it, each with its revision and ETag.
type moveJSON struct {
	From albumJSON `json:"from"`
	To   albumJSON `json:"to"`
}

// moveTracks is POST /api/albums/{id}/move-tracks with the If-Match of the
// album {id}, the source, and the body {"tracks": [<track id>, ...], "to":
// <album id>}: the tracks go to the end of the album "to" (catalog
// MoveTracks), which needs no If-Match since it is only appended to; its
// revision is bumped. A source left without tracks goes to the trash. 200
// with both albums.
func (h *handlers) moveTracks(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, rev, ok := h.albumPrecondition(w, r)
	if !ok {
		return
	}
	body, e := readObject(w, r, "tracks", "to")
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	tracks, e := body.IDs("tracks")
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	to, e := body.ID("to")
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	if _, err := h.catalog.MoveTracks(r.Context(), id, rev, tracks, to); err != nil {
		h.api.fail(w, r, err)
		return
	}
	var out moveJSON
	for _, a := range []struct {
		id  uuid.UUID
		rep *albumJSON
	}{{id, &out.From}, {to, &out.To}} {
		v, err := h.catalog.GetAlbum(r.Context(), a.id)
		if err != nil {
			h.api.fail(w, r, err)
			return
		}
		*a.rep = albumRep(v)
	}
	h.api.writeJSON(w, nethttp.StatusOK, out)
}

// putLyrics is PUT /api/albums/{id}/tracks/{track}/lyrics: an upload of
// an LRC file (at most 2 MiB, valid UTF-8, §10.2), or {"attachment_id"}
// to assign an .lrc attachment of the album, which stays an attachment
// (N-177). The file is written next to the track, with its basename
// (§5.1).
func (h *handlers) putLyrics(w nethttp.ResponseWriter, r *nethttp.Request) {
	id, rev, ok := h.albumPrecondition(w, r)
	if !ok {
		return
	}
	track, ok := h.subID(w, r, "track", catalog.CodeTrackNotFound)
	if !ok {
		return
	}
	kind, e := choiceOrUpload(r.Header)
	if e != nil {
		h.api.writeError(w, e)
		return
	}
	var choice catalog.LyricsChoice
	if kind == bodyChoice {
		choice, ok = h.lyricsAttachment(w, r, id, rev, track)
	} else {
		choice, ok = h.lyricsUpload(w, r, id, rev, track)
	}
	if !ok {
		return
	}
	_, _, err := h.catalog.SetLyrics(r.Context(), id, rev, track, choice)
	h.albumChanged(w, r, id, err)
}

// lyricsAttachment reads {"attachment_id"}, finds the attachment in the
// album, and on the same snapshot the track (404) and the attachment's
// .lrc name (422), so that a refusal known from the catalog reads no byte
// of the blob; then it checks that the content is UTF-8. The transaction
// checks the track and the name again, and decides.
func (h *handlers) lyricsAttachment(w nethttp.ResponseWriter, r *nethttp.Request, id uuid.UUID, rev int64, track uuid.UUID) (catalog.LyricsChoice, bool) {
	att, v, ok := h.chosenAttachment(w, r, id, rev)
	if !ok {
		return catalog.LyricsChoice{}, false
	}
	if _, ok := findTrack(v, track); !ok {
		h.api.fail(w, r, &catalog.Error{Code: catalog.CodeTrackNotFound, Message: "album " + id.String() + " has no track " + track.String()})
		return catalog.LyricsChoice{}, false
	}
	if err := catalog.CheckLyricsAttachment(att.RelPath); err != nil {
		h.api.fail(w, r, err)
		return catalog.LyricsChoice{}, false
	}
	f, err := h.blobs.Open(att.Blob.Hash)
	if err != nil {
		h.api.fail(w, r, err)
		return catalog.LyricsChoice{}, false
	}
	valid, err := validLyrics(r.Context(), f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		h.api.fail(w, r, err)
		return catalog.LyricsChoice{}, false
	case !valid:
		h.api.writeError(w, lyricsError().with("attachment_id", att.ID.String()))
		return catalog.LyricsChoice{}, false
	}
	return catalog.LyricsChoice{Blob: catalog.Blob{Hash: att.Blob.Hash, Size: att.Blob.Size}, Attachment: att.ID}, true
}

// lyricsUpload reads an uploaded LRC file (at most MaxLyricsBytes), checks
// that it is UTF-8 and pins it.
func (h *handlers) lyricsUpload(w nethttp.ResponseWriter, r *nethttp.Request, id uuid.UUID, rev int64, track uuid.UUID) (catalog.LyricsChoice, bool) {
	if e := declaredTooLarge(r, MaxLyricsBytes); e != nil {
		h.api.writeError(w, e)
		return catalog.LyricsChoice{}, false
	}
	v, ok := h.snapshot(w, r, id, rev)
	if !ok {
		return catalog.LyricsChoice{}, false
	}
	if _, ok := findTrack(v, track); !ok {
		h.api.fail(w, r, &catalog.Error{Code: catalog.CodeTrackNotFound, Message: "album " + id.String() + " has no track " + track.String()})
		return catalog.LyricsChoice{}, false
	}
	release, ok := h.acquireUpload(w, r)
	if !ok {
		return catalog.LyricsChoice{}, false
	}
	defer release()
	data, e := readUpload(w, r, MaxLyricsBytes)
	if e == nil && !utf8.Valid(data) {
		e = lyricsError()
	}
	if e != nil {
		h.api.writeError(w, e)
		return catalog.LyricsChoice{}, false
	}
	b, e, err := h.pinUpload(r, bytes.NewReader(data), int64(len(data)), MaxLyricsBytes)
	switch {
	case err != nil:
		h.api.fail(w, r, err)
		return catalog.LyricsChoice{}, false
	case e != nil:
		h.api.writeError(w, e)
		return catalog.LyricsChoice{}, false
	}
	return catalog.LyricsChoice{Blob: b}, true
}

// deleteLyrics is DELETE /api/albums/{id}/tracks/{track}/lyrics: the
// track has no LRC file any more. A track without one is a no-op.
func (h *handlers) deleteLyrics(w nethttp.ResponseWriter, r *nethttp.Request) {
	h.trackCommand(w, r, h.catalog.RemoveLyrics)
}

// trackCommand runs a bodiless change of a track with the album's
// revision of If-Match, then answers the album.
func (h *handlers) trackCommand(w nethttp.ResponseWriter, r *nethttp.Request,
	change func(ctx context.Context, album uuid.UUID, ifMatch int64, track uuid.UUID) (int64, bool, error)) {
	id, rev, ok := h.albumPrecondition(w, r)
	if !ok {
		return
	}
	track, ok := h.subID(w, r, "track", catalog.CodeTrackNotFound)
	if !ok {
		return
	}
	if e := refuseBody(w, r); e != nil {
		h.api.writeError(w, e)
		return
	}
	_, _, err := change(r.Context(), id, rev, track)
	h.albumChanged(w, r, id, err)
}

// chosenAttachment reads the {"attachment_id"} body and finds that
// attachment in the album's snapshot, which it also returns: 404
// attachment_not_found if the album has none with that id (an attachment
// of another album included).
func (h *handlers) chosenAttachment(w nethttp.ResponseWriter, r *nethttp.Request, id uuid.UUID, rev int64) (catalog.AttachmentView, catalog.AlbumView, bool) {
	body, e := readObject(w, r, attachmentKeys...)
	var att uuid.UUID
	if e == nil {
		att, e = body.ID("attachment_id")
	}
	if e != nil {
		h.api.writeError(w, e)
		return catalog.AttachmentView{}, catalog.AlbumView{}, false
	}
	v, ok := h.snapshot(w, r, id, rev)
	if !ok {
		return catalog.AttachmentView{}, catalog.AlbumView{}, false
	}
	a, ok := findAttachment(v, att)
	if !ok {
		h.api.fail(w, r, &catalog.Error{Code: catalog.CodeAttachmentNotFound,
			Message: "album " + id.String() + " has no attachment " + att.String()})
	}
	return a, v, ok
}

// acquireUpload is uploadSlot answering its refusal: ok false means the
// answer is written.
func (h *handlers) acquireUpload(w nethttp.ResponseWriter, r *nethttp.Request) (func(), bool) {
	release, e, err := h.uploadSlot(r)
	switch {
	case err != nil:
		h.api.fail(w, r, err)
		return nil, false
	case e != nil:
		h.api.writeError(w, e)
		return nil, false
	}
	return release, true
}

// pinUpload is pin in the request's context, then the failpoint between
// the put and the transaction.
func (h *handlers) pinUpload(r *nethttp.Request, src io.Reader, estimate, limit int64) (catalog.Blob, *Error, error) {
	b, e, err := h.pin(r.Context(), src, estimate, limit)
	if e != nil || err != nil {
		return catalog.Blob{}, e, err
	}
	if err := h.pinned(); err != nil {
		return catalog.Blob{}, nil, errors.Join(errors.New("failpoint upload_pinned"), err)
	}
	return b, nil, nil
}

// subID parses the id of a sub-resource of the album in the path, named
// by key: an id that is not in canonical form names nothing, 404 with the
// resource's code.
func (h *handlers) subID(w nethttp.ResponseWriter, r *nethttp.Request, key, code string) (uuid.UUID, bool) {
	raw := r.PathValue(key)
	id, ok := parseID(raw)
	if !ok {
		h.api.writeError(w, newError(nethttp.StatusNotFound, code, "%s %q does not exist", key, raw))
	}
	return id, ok
}

func findAttachment(v catalog.AlbumView, id uuid.UUID) (catalog.AttachmentView, bool) {
	for _, a := range v.Attachments {
		if a.ID == id {
			return a, true
		}
	}
	return catalog.AttachmentView{}, false
}

func findTrack(v catalog.AlbumView, id uuid.UUID) (catalog.TrackView, bool) {
	for _, t := range v.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return catalog.TrackView{}, false
}

// audioFormats are the formats of the album's tracks in a snapshot.
func audioFormats(v catalog.AlbumView) []string {
	out := make([]string, len(v.Tracks))
	for i, t := range v.Tracks {
		out[i] = t.Blob.Format
	}
	return out
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
