package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"mime"
	nethttp "net/http"
	"unicode/utf8"

	"github.com/google/uuid"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/media"
)

// The upload protocol of §10.2 (NOTES.md N-172, N-173): the body is the
// file itself, Content-Type application/octet-stream, and its format is
// read from the content, never from a name or a declared type (§4.2). The
// blob is pinned through the single put primitive (blobstore.Put, §7.5)
// before the catalog transaction that compares If-Match; a save that
// fails after the put leaves an unreferenced blob, never a broken
// reference.

// The size limits of §10.2 and §8.5. One byte more is 413 body_too_large,
// as soon as it is read.
const (
	MaxAttachmentBytes = 256 << 20
	MaxLyricsBytes     = 2 << 20
	MaxCoverBytes      = media.MaxCoverBytes
)

// UploadMediaType is the one Content-Type of an upload's body.
const UploadMediaType = "application/octet-stream"

// Codes of the upload protocol.
const (
	// CodeInsufficientSpace: the process's space budget or the disk cannot
	// hold the upload (§11.2): 507.
	CodeInsufficientSpace = "insufficient_space"
	// CodeUploadIncomplete: the body could not be read to its end (the
	// client went away, or a malformed chunked body): 400.
	CodeUploadIncomplete = "upload_incomplete"
	// CodeInvalidCover: an upload or an attachment chosen as the cover that
	// is not a valid JPEG or PNG of §8.5 (the catalog's code): 422.
	CodeInvalidCover = catalog.CodeInvalidCover
	// CodeInvalidLyrics: lyrics that are not valid UTF-8 text, or an
	// attachment chosen as lyrics that is not an .lrc file: 422.
	CodeInvalidLyrics = catalog.CodeInvalidLyrics
)

// bodyKind is what a PUT of the cover or of the lyrics carries: the choice
// of an attachment (JSON), or an upload.
type bodyKind int

const (
	bodyChoice bodyKind = iota
	bodyUpload
)

// choiceOrUpload reads the Content-Type of a PUT that accepts either
// {"attachment_id"} as JSON (N-148's media type) or an upload
// (UploadMediaType); anything else is 415.
func choiceOrUpload(h nethttp.Header) (bodyKind, *Error) {
	if checkContentType(h) == nil {
		return bodyChoice, nil
	}
	if checkUploadType(h) == nil {
		return bodyUpload, nil
	}
	return 0, newError(nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
		"the body must be Content-Type: application/json ({\"attachment_id\"}) or %s (the file)", UploadMediaType)
}

// checkUploadType requires exactly one Content-Type, UploadMediaType
// without parameters.
func checkUploadType(h nethttp.Header) *Error {
	values := h.Values("Content-Type")
	if len(values) == 1 {
		if mt, params, err := mime.ParseMediaType(values[0]); err == nil && mt == UploadMediaType && len(params) == 0 {
			return nil
		}
	}
	return newError(nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
		"the body must be the file, with Content-Type: %s", UploadMediaType)
}

func tooLarge(limit int64) *Error {
	return newError(nethttp.StatusRequestEntityTooLarge, CodeBodyTooLarge,
		"the file is larger than %d bytes", limit).with("limit", limit)
}

// declaredTooLarge refuses a body whose Content-Length already exceeds
// limit, before anything is read.
func declaredTooLarge(r *nethttp.Request, limit int64) *Error {
	if r.ContentLength > limit {
		return tooLarge(limit)
	}
	return nil
}

// readUpload reads a small upload (a cover, lyrics) whole, at most limit
// bytes: its content is checked before it is pinned, so that an invalid
// file never becomes a blob.
func readUpload(w nethttp.ResponseWriter, r *nethttp.Request, limit int64) ([]byte, *Error) {
	if e := declaredTooLarge(r, limit); e != nil {
		return nil, e
	}
	b, err := io.ReadAll(nethttp.MaxBytesReader(w, r.Body, limit))
	if e := bodyError(err, limit); e != nil {
		return nil, e
	}
	return b, nil
}

// bodyError types a failure to read an upload's body.
func bodyError(err error, limit int64) *Error {
	var mbe *nethttp.MaxBytesError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &mbe):
		return tooLarge(limit)
	default:
		return newError(nethttp.StatusBadRequest, CodeUploadIncomplete, "the file could not be read to its end")
	}
}

// pin puts src, whose size is at most estimate bytes, into the blob store
// (§7.5), with the space of §11.2: estimate is reserved in the process's
// budget against the free space of /data minus the margin, and released
// once the put is over, whatever its outcome. A body over its limit (a
// MaxBytesReader) is 413, ENOSPC 507; the temporary is removed on every
// failure by the put itself.
func (h *handlers) pin(ctx context.Context, src io.Reader, estimate, limit int64) (catalog.Blob, *Error, error) {
	fs, err := h.work.StatFS()
	if err != nil {
		return catalog.Blob{}, nil, err
	}
	res, avail := h.budget.Reserve(fs.FreeBytes, estimate)
	if res == nil {
		return catalog.Blob{}, newError(nethttp.StatusInsufficientStorage, CodeInsufficientSpace,
			"the upload needs up to %d bytes; the server can use %d now", estimate, max(0, avail)).
			with("needed", estimate), nil
	}
	defer res.Release()
	b, err := h.blobs.Put(ctx, src)
	if err != nil {
		var mbe *nethttp.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			return catalog.Blob{}, tooLarge(limit), nil
		case blobstore.Code(err) == blobstore.CodeNoSpace:
			return catalog.Blob{}, newError(nethttp.StatusInsufficientStorage, CodeInsufficientSpace,
				"the disk is full: the upload was not saved"), nil
		case blobstore.Code(err) == blobstore.CodeSource, ctx.Err() != nil:
			// The client stopped sending, or went away: nothing to log as
			// the server's failure.
			return catalog.Blob{}, newError(nethttp.StatusBadRequest, CodeUploadIncomplete, "the file could not be read to its end"), nil
		}
		return catalog.Blob{}, nil, err
	}
	return catalog.Blob{Hash: b.SHA256, Size: b.Size}, nil, nil
}

// pinned is the failpoint between the put and the catalog transaction
// (N-142, N-173).
func (h *handlers) pinned() error {
	return h.api.failpoints.Hit("upload_pinned")
}

// snapshot reads the album and compares the revision of If-Match on it,
// before a body is read or a blob pinned (N-173): a missing album is 404
// and a stale revision 412 at once, so that no upload is stored for a
// change that cannot happen. The change compares again in its
// transaction, which alone decides.
func (h *handlers) snapshot(w nethttp.ResponseWriter, r *nethttp.Request, id uuid.UUID, rev int64) (catalog.AlbumView, bool) {
	v, err := h.catalog.GetAlbum(r.Context(), id)
	if err == nil {
		err = catalog.CheckRevision("album", id, rev, v.Revision)
	}
	if err != nil {
		h.api.fail(w, r, err)
		return catalog.AlbumView{}, false
	}
	return v, true
}

// validateCover is §8.5's check of an image as a cover: 422 invalid_cover
// with the reason for a refusal, an error for a failure to read it.
func validateCover(r io.ReadSeeker, size int64) (string, *Error, error) {
	format, err := media.ValidateCover(r, size)
	var me *media.Error
	switch {
	case err == nil:
		return format, nil, nil
	case errors.As(err, &me) && me.Code == media.CodeInvalidImage:
		return "", newError(nethttp.StatusUnprocessableEntity, CodeInvalidCover,
			"the image cannot be the cover: %s (a JPEG or PNG of at most %d bytes and %d pixels, §8.5)",
			me.Msg, media.MaxCoverBytes, media.MaxCoverPixels), nil
	}
	return "", nil, err
}

// validLyrics reads r to its end and reports whether it is valid UTF-8, in
// constant memory (§10.2: "testo UTF-8 valido"): the rule of the importer
// (NOTES.md N-117), for an attachment chosen as lyrics.
func validLyrics(ctx context.Context, r io.Reader) (bool, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	for n := 0; ; n++ {
		if n%(1<<16) == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		c, size, err := br.ReadRune()
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if c == utf8.RuneError && size == 1 {
			return false, nil
		}
	}
}

// lyricsError is the refusal of lyrics that are not UTF-8.
func lyricsError() *Error {
	return newError(nethttp.StatusUnprocessableEntity, CodeInvalidLyrics,
		"the lyrics are not valid UTF-8 text (§10.2): convert the .lrc file to UTF-8")
}

// uploadReader is the body of a streamed upload: at most limit bytes, one
// more is a MaxBytesError that pin turns into 413.
func uploadReader(w nethttp.ResponseWriter, r *nethttp.Request, limit int64) io.Reader {
	return nethttp.MaxBytesReader(w, r.Body, limit)
}

// estimateOf is the space an upload may take: its Content-Length when the
// client sent one (the server reads no more), otherwise the limit.
func estimateOf(r *nethttp.Request, limit int64) int64 {
	if r.ContentLength >= 0 && r.ContentLength <= limit {
		return r.ContentLength
	}
	return limit
}
