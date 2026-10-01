package catalog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/names"
)

// The editor's content operations (§10.2, round 14) on a real PostgreSQL
// 17: every effective change bumps the album's revision and enqueues its
// render in one transaction (§4.3), every no-op does neither, and every
// refusal writes nothing.

// unchanged checks that a refused or no-op operation left the album at
// rev, without a render row and without a wake-up.
func (e *env) unchanged(album uuid.UUID, rev int64, wakes int32) {
	e.t.Helper()
	if got := e.album(album).Revision; got != rev {
		e.t.Fatalf("revision %d, want %d unchanged", got, rev)
	}
	if _, ok := e.renderJob(album); ok {
		e.t.Fatal("a render was enqueued")
	}
	if e.wakes.Load() != wakes {
		e.t.Fatal("the pool was woken")
	}
}

// bumped checks one effective change: revision rev+1, a render pending,
// the pool woken; then clears the render for the next step.
func (e *env) bumped(album uuid.UUID, rev, got int64) {
	e.t.Helper()
	if got != rev+1 || e.album(album).Revision != rev+1 {
		e.t.Fatalf("revision %d (returned %d), want %d", e.album(album).Revision, got, rev+1)
	}
	if j, ok := e.renderJob(album); !ok || j.State != "pending" {
		e.t.Fatalf("no pending render: %+v", j)
	}
	e.clearRenders()
}

func (e *env) attachmentID(album uuid.UUID, relPath string) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	if err := e.db.QueryRow(context.Background(), `SELECT id FROM attachments WHERE album_id = $1 AND rel_path = $2`,
		album, relPath).Scan(&id); err != nil {
		e.t.Fatalf("attachment %q: %v", relPath, err)
	}
	return id
}

func (e *env) blobFormat(hash string) string {
	e.t.Helper()
	var f *string
	if err := e.db.QueryRow(context.Background(), `SELECT format FROM blobs WHERE hash = $1`, hash).Scan(&f); err != nil {
		e.t.Fatal(err)
	}
	return deref(f)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestSetCover(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Miles Davis", "Kind of Blue")
	other := e.importAlbum("Miles Davis", "Milestones")
	e.clearRenders()
	rev := e.album(id).Revision
	w0 := e.wakes.Load()
	jpg := catalog.Blob{Hash: newHash(), Size: 4000, Format: catalog.FormatJPEG}

	// Preconditions, in the transaction (§10.1).
	_, _, err := e.svc.SetCover(ctx, id, 0, catalog.CoverChoice{Blob: jpg})
	wantCode(t, err, catalog.CodePreconditionRequired)
	_, _, err = e.svc.SetCover(ctx, id, rev+1, catalog.CoverChoice{Blob: jpg})
	if ce := wantCode(t, err, catalog.CodePreconditionFailed); ce.Details.Revision != rev {
		t.Fatalf("412 details %+v", ce.Details)
	}
	_, _, err = e.svc.SetCover(ctx, uuid.New(), 1, catalog.CoverChoice{Blob: jpg})
	wantCode(t, err, catalog.CodeAlbumNotFound)
	// Content refusals (§8.5), before the transaction.
	_, _, err = e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: catalog.Blob{Hash: newHash(), Size: 10}})
	wantCode(t, err, catalog.CodeInvalidCover)
	_, _, err = e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: catalog.Blob{Hash: newHash(), Size: catalog.MaxCoverBytes + 1, Format: catalog.FormatPNG}})
	wantCode(t, err, catalog.CodeInvalidCover)
	_, _, err = e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: catalog.Blob{Hash: "nope", Size: 1, Format: catalog.FormatPNG}})
	wantCode(t, err, catalog.CodeInvalidBlob)
	e.unchanged(id, rev, w0)

	// N-091: a cover one audio format of the album cannot embed.
	e.fits = func(c catalog.Blob, format string) error {
		if c.Size > 3000 {
			return errors.New("too large for " + format)
		}
		return nil
	}
	_, _, err = e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: jpg})
	if ce := wantCode(t, err, catalog.CodeCoverNotEmbeddable); len(ce.Details.Names) != 1 || ce.Details.Names[0] != catalog.FormatFLAC {
		t.Fatalf("details %+v", ce.Details)
	}
	// The same question without the database, as the API asks it first.
	if ce := wantCode(t, e.svc.CheckCoverFits(jpg, []string{catalog.FormatMP3, catalog.FormatFLAC, catalog.FormatMP3}),
		catalog.CodeCoverNotEmbeddable); ce.Details.Names[0] != catalog.FormatFLAC {
		t.Fatalf("details %+v", ce.Details)
	}
	e.unchanged(id, rev, w0)
	if n := e.count(`SELECT count(*) FROM blobs WHERE hash = $1`, jpg.Hash); n != 0 {
		t.Fatal("a refused cover registered its blob")
	}
	e.fits = nil

	// The upload: registered with its content's format, bumped, enqueued.
	got, changed, err := e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: jpg})
	if err != nil || !changed {
		t.Fatalf("SetCover: %v %v", changed, err)
	}
	e.bumped(id, rev, got)
	if a := e.album(id); *a.CoverHash != jpg.Hash || e.blobFormat(jpg.Hash) != catalog.FormatJPEG {
		t.Fatalf("cover %v, format %q", a.CoverHash, e.blobFormat(jpg.Hash))
	}
	rev++
	wakes := e.wakes.Load()
	// The same cover again: a no-op.
	got, changed, err = e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: jpg})
	if err != nil || changed || got != rev {
		t.Fatalf("the same cover: %d %v %v", got, changed, err)
	}
	e.unchanged(id, rev, wakes)

	// An attachment of the album whose blob has no known format (N-118):
	// the image is validated by the caller, the blob learns its format,
	// and the attachment stays (§7.4).
	scan := catalog.Blob{Hash: newHash(), Size: 2000}
	rev, _ = e.addAttachment(id, rev, "Scans/front.png", scan)
	attID := e.attachmentID(id, "Scans/front.png")
	png := catalog.Blob{Hash: scan.Hash, Size: scan.Size, Format: catalog.FormatPNG}
	// Another album's attachment is not found, whatever its content.
	otherAtt := e.attachmentID(other, "cover.jpg")
	_, _, err = e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: png, Attachment: otherAtt})
	wantCode(t, err, catalog.CodeAttachmentNotFound)
	// An attachment whose blob is not the one checked: a caller's error.
	_, _, err = e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: jpg, Attachment: attID})
	wantCode(t, err, catalog.CodeInvalidArgument)
	e.unchanged(id, rev, e.wakes.Load())
	got, changed, err = e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: png, Attachment: attID})
	if err != nil || !changed {
		t.Fatalf("choosing an attachment: %v %v", changed, err)
	}
	e.bumped(id, rev, got)
	rev++
	if a := e.album(id); *a.CoverHash != scan.Hash || e.blobFormat(scan.Hash) != catalog.FormatPNG ||
		e.count(`SELECT count(*) FROM attachments WHERE id = $1`, attID) != 1 {
		t.Fatal("the chosen attachment is not the cover, or is no longer an attachment")
	}

	// A blob already known with another size or another format.
	e.exec(`INSERT INTO blobs (hash, size, format, created_at) VALUES ($1, 1, 'flac', now())`, newHash())
	known := newHash()
	e.exec(`INSERT INTO blobs (hash, size, format, created_at) VALUES ($1, 4000, 'flac', now())`, known)
	_, _, err = e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: catalog.Blob{Hash: known, Size: 4000, Format: catalog.FormatJPEG}})
	wantCode(t, err, catalog.CodeInvalidBlobFormat)
	_, _, err = e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: catalog.Blob{Hash: known, Size: 4001, Format: catalog.FormatJPEG}})
	wantCode(t, err, catalog.CodeBlobMismatch)
	e.unchanged(id, rev, e.wakes.Load())
}

// addAttachment adds an attachment and returns the new revision.
func (e *env) addAttachment(album uuid.UUID, rev int64, relPath string, b catalog.Blob) (int64, uuid.UUID) {
	e.t.Helper()
	got, att, err := e.svc.AddAttachment(context.Background(), album, rev, relPath, b)
	if err != nil {
		e.t.Fatalf("AddAttachment %q: %v", relPath, err)
	}
	e.bumped(album, rev, got)
	return got, att
}

func TestRemoveCover(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Miles Davis", "Kind of Blue")
	e.clearRenders()
	rev := e.album(id).Revision
	w0 := e.wakes.Load()
	cover := *e.album(id).CoverHash
	_, _, err := e.svc.RemoveCover(ctx, id, 0)
	wantCode(t, err, catalog.CodePreconditionRequired)
	_, _, err = e.svc.RemoveCover(ctx, id, rev-1+2)
	wantCode(t, err, catalog.CodePreconditionFailed)
	e.unchanged(id, rev, w0)
	got, changed, err := e.svc.RemoveCover(ctx, id, rev)
	if err != nil || !changed {
		t.Fatalf("RemoveCover: %v %v", changed, err)
	}
	e.bumped(id, rev, got)
	rev++
	// The blob stays, and so does the attachment holding the same image
	// (§4.3, §7.4).
	if e.album(id).CoverHash != nil || e.count(`SELECT count(*) FROM blobs WHERE hash = $1`, cover) != 1 ||
		e.count(`SELECT count(*) FROM attachments WHERE album_id = $1 AND blob_hash = $2`, id, cover) != 1 {
		t.Fatal("after the removal")
	}
	wakes := e.wakes.Load()
	got, changed, err = e.svc.RemoveCover(ctx, id, rev)
	if err != nil || changed || got != rev {
		t.Fatalf("removing an absent cover: %d %v %v", got, changed, err)
	}
	e.unchanged(id, rev, wakes)

	// In the trash too (N-178): the removal render, idempotent.
	if _, _, err := e.svc.TrashAlbum(ctx, id, rev); err != nil {
		t.Fatal(err)
	}
	e.clearRenders()
	rev++
	got, _, err = e.svc.SetCover(ctx, id, rev, catalog.CoverChoice{Blob: catalog.Blob{Hash: cover, Size: 500_000, Format: catalog.FormatJPEG}})
	if err != nil {
		t.Fatal(err)
	}
	e.bumped(id, rev, got)
}

func TestAddAttachment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Miles Davis", "Kind of Blue")
	e.clearRenders()
	rev := e.album(id).Revision
	w0 := e.wakes.Load()
	b := catalog.Blob{Hash: newHash(), Size: 123}

	_, _, err := e.svc.AddAttachment(ctx, id, 0, "notes.txt", b)
	wantCode(t, err, catalog.CodePreconditionRequired)
	_, _, err = e.svc.AddAttachment(ctx, id, rev+1, "notes.txt", b)
	wantCode(t, err, catalog.CodePreconditionFailed)
	_, _, err = e.svc.AddAttachment(ctx, uuid.New(), 1, "notes.txt", b)
	wantCode(t, err, catalog.CodeAlbumNotFound)

	// §5.2: refused before the transaction, with the names code.
	for p, code := range map[string]string{
		"":            names.CodePathEmpty,
		"/etc/passwd": names.CodePathAbsolute,
		"../outside":  names.CodePathDotSegment,
		"a/../../b":   names.CodePathDotSegment,
		"./a":         names.CodePathDotSegment,
		"a//b":        names.CodePathEmptySegment,
		"a/":          names.CodePathEmptySegment,
		"a\x00b":      names.CodePathNulByte,
		"bad\xffutf8": names.CodeInvalidUTF8,
		deep(17):      names.CodePathTooDeep,
		long(1025):    names.CodePathTooLong,
	} {
		_, _, err := e.svc.AddAttachment(ctx, id, rev, p, b)
		wantCode(t, err, code)
		if _, err := catalog.AttachmentPath(p); catalog.Code(err) != code {
			t.Fatalf("AttachmentPath(%q): %v", p, err)
		}
	}
	e.unchanged(id, rev, w0)
	// Collisions after normalization with the album's attachments
	// (Scans/Booklet.pdf and cover.jpg), naming both (§5.2).
	for p, other := range map[string]string{
		"Scans/Booklet.pdf":   "Scans/Booklet.pdf", // the same path
		"scans/booklet.PDF":   "Scans/Booklet.pdf", // casefold: the same file, and a directory spelled two ways
		"Scans":               "Scans/Booklet.pdf", // a file where a directory is needed
		"cover.jpg/inner.txt": "cover.jpg",         // a directory where a file is
		"SCANS/other.pdf":     "Scans/Booklet.pdf", // one directory spelled two ways
		"Scans/Booklet.pdf ":  "Scans/Booklet.pdf", // trimmed by the sanitization
	} {
		_, _, err := e.svc.AddAttachment(ctx, id, rev, p, b)
		ce := wantCode(t, err, catalog.CodeAttachmentCollision)
		if len(ce.Details.Names) != 2 || (ce.Details.Names[0] != other && ce.Details.Names[1] != other) {
			t.Fatalf("%q: details %+v", p, ce.Details)
		}
		if err := catalog.AttachmentConflict([]string{"Scans/Booklet.pdf", "cover.jpg"}, p); catalog.Code(err) != catalog.CodeAttachmentCollision {
			t.Fatalf("AttachmentConflict(%q): %v", p, err)
		}
	}
	// An NFC variant of an existing NFD name is the same file.
	nfd := catalog.Blob{Hash: newHash(), Size: 1}
	rev, _ = e.addAttachment(id, rev, "Liner/Cafe\u0301.txt", nfd)
	_, _, err = e.svc.AddAttachment(ctx, id, rev, "Liner/Caf\u00e9.txt", b)
	wantCode(t, err, catalog.CodeAttachmentCollision)
	e.unchanged(id, rev, e.wakes.Load())
	if n := e.count(`SELECT count(*) FROM blobs WHERE hash = $1`, b.Hash); n != 0 {
		t.Fatal("a refused attachment registered its blob")
	}

	// Accepted: the path kept as given, its key the normalized one.
	got, att, err := e.svc.AddAttachment(ctx, id, rev, "Scans/Back Cover?.pdf", b)
	if err != nil {
		t.Fatal(err)
	}
	e.bumped(id, rev, got)
	rev++
	var relPath, key, hash string
	if err := e.db.QueryRow(ctx, `SELECT rel_path, path_key, blob_hash FROM attachments WHERE id = $1 AND album_id = $2`,
		att, id).Scan(&relPath, &key, &hash); err != nil {
		t.Fatal(err)
	}
	sp, _ := names.SanitizeRelFilePath("Scans/Back Cover?.pdf")
	if relPath != "Scans/Back Cover?.pdf" || key != sp.Key || hash != b.Hash || e.blobFormat(b.Hash) != "" {
		t.Fatalf("attachment %q %q %q", relPath, key, hash)
	}
	// The same blob under another path is another attachment.
	rev, _ = e.addAttachment(id, rev, "Copy of the back.pdf", b)
	// In the trash too (N-178).
	if _, _, err := e.svc.TrashAlbum(ctx, id, rev); err != nil {
		t.Fatal(err)
	}
	e.clearRenders()
	rev++
	e.addAttachment(id, rev, "trash.txt", catalog.Blob{Hash: newHash(), Size: 3})
}

func deep(levels int) string {
	p := "d"
	for i := 1; i < levels; i++ {
		p += "/d"
	}
	return p
}

func long(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(i%26)
		if i%100 == 99 {
			b[i] = '/'
		}
	}
	return string(b)
}

func TestDeleteAttachment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Miles Davis", "Kind of Blue")
	other := e.importAlbum("Bill Evans", "Portrait in Jazz")
	e.clearRenders()
	rev := e.album(id).Revision
	w0 := e.wakes.Load()
	coverAtt := e.attachmentID(id, "cover.jpg")
	cover := *e.album(id).CoverHash

	_, _, err := e.svc.DeleteAttachment(ctx, id, 0, coverAtt)
	wantCode(t, err, catalog.CodePreconditionRequired)
	_, _, err = e.svc.DeleteAttachment(ctx, id, rev+1, coverAtt)
	wantCode(t, err, catalog.CodePreconditionFailed)
	_, _, err = e.svc.DeleteAttachment(ctx, id, rev, e.attachmentID(other, "cover.jpg"))
	wantCode(t, err, catalog.CodeAttachmentNotFound)
	_, _, err = e.svc.DeleteAttachment(ctx, id, rev, uuid.New())
	wantCode(t, err, catalog.CodeAttachmentNotFound)
	e.unchanged(id, rev, w0)
	if e.count(`SELECT count(*) FROM attachments WHERE album_id = $1`, other) != 2 {
		t.Fatal("another album's attachment was touched")
	}

	// The attachment that holds the cover's image: it goes, the cover
	// stays (N-176), and so does the blob.
	got, changed, err := e.svc.DeleteAttachment(ctx, id, rev, coverAtt)
	if err != nil || !changed {
		t.Fatalf("DeleteAttachment: %v %v", changed, err)
	}
	e.bumped(id, rev, got)
	rev++
	if e.count(`SELECT count(*) FROM attachments WHERE id = $1`, coverAtt) != 0 || *e.album(id).CoverHash != cover ||
		e.count(`SELECT count(*) FROM blobs WHERE hash = $1`, cover) != 1 {
		t.Fatal("after deleting the cover's attachment")
	}
	// Deleted twice: not found, nothing changes.
	_, _, err = e.svc.DeleteAttachment(ctx, id, rev, coverAtt)
	wantCode(t, err, catalog.CodeAttachmentNotFound)
	e.unchanged(id, rev, e.wakes.Load())
}

func TestSetLyrics(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Miles Davis", "Kind of Blue")
	other := e.importAlbum("Bill Evans", "Portrait in Jazz")
	e.clearRenders()
	rev := e.album(id).Revision
	w0 := e.wakes.Load()
	tracks := e.tracks(id)
	first, second := tracks[0].ID, tracks[1].ID
	lrc := catalog.Blob{Hash: newHash(), Size: 40}

	_, _, err := e.svc.SetLyrics(ctx, id, 0, second, catalog.LyricsChoice{Blob: lrc})
	wantCode(t, err, catalog.CodePreconditionRequired)
	_, _, err = e.svc.SetLyrics(ctx, id, rev+1, second, catalog.LyricsChoice{Blob: lrc})
	wantCode(t, err, catalog.CodePreconditionFailed)
	_, _, err = e.svc.SetLyrics(ctx, id, rev, e.tracks(other)[0].ID, catalog.LyricsChoice{Blob: lrc})
	wantCode(t, err, catalog.CodeTrackNotFound)
	// A blob known as an image cannot be lyrics (checkRole, as at the import).
	cover := *e.album(id).CoverHash
	_, _, err = e.svc.SetLyrics(ctx, id, rev, second, catalog.LyricsChoice{Blob: catalog.Blob{Hash: cover, Size: 500_000}})
	wantCode(t, err, catalog.CodeInvalidBlobFormat)
	e.unchanged(id, rev, w0)

	got, changed, err := e.svc.SetLyrics(ctx, id, rev, second, catalog.LyricsChoice{Blob: lrc})
	if err != nil || !changed {
		t.Fatalf("SetLyrics: %v %v", changed, err)
	}
	e.bumped(id, rev, got)
	rev++
	wakes := e.wakes.Load()
	if _, changed, err = e.svc.SetLyrics(ctx, id, rev, second, catalog.LyricsChoice{Blob: lrc}); err != nil || changed {
		t.Fatalf("the same lyrics: %v %v", changed, err)
	}
	e.unchanged(id, rev, wakes)

	// An attachment: .lrc only; it stays an attachment (N-177).
	_, _, err = e.svc.SetLyrics(ctx, id, rev, first, catalog.LyricsChoice{
		Blob:       catalog.Blob{Hash: e.bookletHash(id), Size: 1_000_000},
		Attachment: e.attachmentID(id, "Scans/Booklet.pdf")})
	wantCode(t, err, catalog.CodeInvalidLyrics)
	text := catalog.Blob{Hash: newHash(), Size: 50}
	rev, att := e.addAttachment(id, rev, "Lyrics/So What.LRC", text)
	_, _, err = e.svc.SetLyrics(ctx, id, rev, first, catalog.LyricsChoice{Blob: text, Attachment: e.attachmentID(other, "Scans/Booklet.pdf")})
	wantCode(t, err, catalog.CodeAttachmentNotFound)
	got, changed, err = e.svc.SetLyrics(ctx, id, rev, first, catalog.LyricsChoice{Blob: text, Attachment: att})
	if err != nil || !changed {
		t.Fatalf("assigning an attachment: %v %v", changed, err)
	}
	e.bumped(id, rev, got)
	rev++
	if tr := e.lyricsOf(id, first); tr != text.Hash || e.count(`SELECT count(*) FROM attachments WHERE id = $1`, att) != 1 {
		t.Fatal("the assigned attachment")
	}
	// Deleting that attachment keeps the track's lyrics: they reference the
	// blob, not the attachment (N-176).
	got, changed, err = e.svc.DeleteAttachment(ctx, id, rev, att)
	if err != nil || !changed {
		t.Fatalf("deleting the assigned attachment: %v %v", changed, err)
	}
	e.bumped(id, rev, got)
	rev++
	if e.lyricsOf(id, first) != text.Hash || e.count(`SELECT count(*) FROM attachments WHERE id = $1`, att) != 0 {
		t.Fatal("after deleting the assigned attachment")
	}

	// Removal, then a no-op.
	got, changed, err = e.svc.RemoveLyrics(ctx, id, rev, first)
	if err != nil || !changed {
		t.Fatalf("RemoveLyrics: %v %v", changed, err)
	}
	e.bumped(id, rev, got)
	rev++
	if e.lyricsOf(id, first) != "" || e.count(`SELECT count(*) FROM blobs WHERE hash = $1`, text.Hash) != 1 {
		t.Fatal("after the removal")
	}
	wakes = e.wakes.Load()
	if _, changed, err = e.svc.RemoveLyrics(ctx, id, rev, first); err != nil || changed {
		t.Fatalf("removing absent lyrics: %v %v", changed, err)
	}
	_, _, err = e.svc.RemoveLyrics(ctx, id, rev, uuid.New())
	wantCode(t, err, catalog.CodeTrackNotFound)
	e.unchanged(id, rev, wakes)
}

func (e *env) bookletHash(album uuid.UUID) string {
	e.t.Helper()
	var h string
	if err := e.db.QueryRow(context.Background(), `SELECT blob_hash FROM attachments WHERE album_id = $1 AND rel_path = 'Scans/Booklet.pdf'`,
		album).Scan(&h); err != nil {
		e.t.Fatal(err)
	}
	return h
}

func (e *env) lyricsOf(album, track uuid.UUID) string {
	e.t.Helper()
	var h *string
	if err := e.db.QueryRow(context.Background(), `SELECT lyrics_hash FROM tracks WHERE id = $1 AND album_id = $2`,
		track, album).Scan(&h); err != nil {
		e.t.Fatal(err)
	}
	return deref(h)
}

func TestDeleteTrack(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Miles Davis", "Kind of Blue")
	other := e.importAlbum("Bill Evans", "Portrait in Jazz")
	e.clearRenders()
	rev := e.album(id).Revision
	w0 := e.wakes.Load()
	tracks := e.tracks(id)

	_, _, err := e.svc.DeleteTrack(ctx, id, 0, tracks[0].ID)
	wantCode(t, err, catalog.CodePreconditionRequired)
	_, _, err = e.svc.DeleteTrack(ctx, id, rev+1, tracks[0].ID)
	wantCode(t, err, catalog.CodePreconditionFailed)
	_, _, err = e.svc.DeleteTrack(ctx, id, rev, e.tracks(other)[0].ID)
	wantCode(t, err, catalog.CodeTrackNotFound)
	e.unchanged(id, rev, w0)

	// The first track, with its lyrics: the row goes, the blobs stay.
	var audio, lyrics string
	if err := e.db.QueryRow(ctx, `SELECT blob_hash, lyrics_hash FROM tracks WHERE id = $1`, tracks[0].ID).Scan(&audio, &lyrics); err != nil {
		t.Fatal(err)
	}
	got, changed, err := e.svc.DeleteTrack(ctx, id, rev, tracks[0].ID)
	if err != nil || !changed {
		t.Fatalf("DeleteTrack: %v %v", changed, err)
	}
	e.bumped(id, rev, got)
	rev++
	if len(e.tracks(id)) != 1 || e.count(`SELECT count(*) FROM blobs WHERE hash IN ($1, $2)`, audio, lyrics) != 2 {
		t.Fatal("after the deletion")
	}
	// The last one stays (§4.3), in the trash too.
	_, _, err = e.svc.DeleteTrack(ctx, id, rev, tracks[1].ID)
	wantCode(t, err, catalog.CodeNoTracks)
	if _, _, err := e.svc.TrashAlbum(ctx, id, rev); err != nil {
		t.Fatal(err)
	}
	e.clearRenders()
	rev++
	_, _, err = e.svc.DeleteTrack(ctx, id, rev, tracks[1].ID)
	wantCode(t, err, catalog.CodeNoTracks)
	_, _, err = e.svc.DeleteTrack(ctx, id, rev, tracks[0].ID)
	wantCode(t, err, catalog.CodeTrackNotFound)
	e.unchanged(id, rev, e.wakes.Load())
	if len(e.tracks(id)) != 1 {
		t.Fatal("the last track went")
	}
}

// Two concurrent deletions of the two tracks of an album on the same
// revision: exactly one succeeds and the other is 412, so the album is
// never left without tracks through a race.
func TestDeleteTrackConcurrent(t *testing.T) {
	e := newEnv(t)
	for round := 0; round < 10; round++ {
		id := e.importAlbum("Artist", "Album "+string(rune('A'+round)))
		rev := e.album(id).Revision
		tracks := e.tracks(id)
		errs := make(chan error, 2)
		for _, tr := range tracks {
			go func() {
				_, _, err := e.svc.DeleteTrack(context.Background(), id, rev, tr.ID)
				errs <- err
			}()
		}
		var codes []string
		for range tracks {
			codes = append(codes, catalog.Code(<-errs))
		}
		if !(codes[0] == "" && codes[1] == catalog.CodePreconditionFailed) && !(codes[1] == "" && codes[0] == catalog.CodePreconditionFailed) {
			t.Fatalf("round %d: %q", round, codes)
		}
		if len(e.tracks(id)) != 1 {
			t.Fatalf("round %d: %d tracks", round, len(e.tracks(id)))
		}
	}
}

// trackRow is a track of the album by its id, as stored.
func (e *env) trackRow(id uuid.UUID) (disc, no int32, title string, artist, genre *string, source, blob string) {
	e.t.Helper()
	if err := e.db.QueryRow(context.Background(),
		`SELECT disc, no, title, artist, genre, source_path, blob_hash FROM tracks WHERE id = $1`, id).
		Scan(&disc, &no, &title, &artist, &genre, &source, &blob); err != nil {
		e.t.Fatalf("track %s: %v", id, err)
	}
	return
}

// A file added as a track of an existing album: one change of the album
// (If-Match in the transaction, revision bumped, render enqueued), the
// place of its tags or the end of the album, the artist and genre
// inherited when they are the album's, never a second track of the same
// blob, and the album's per-format rules checked with the new format.
func TestAddTrack(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.importAlbum("Miles Davis", "Kind of Blue") // Jazz, a JPEG cover, FLAC tracks 1 and 2
	e.clearRenders()
	rev := e.album(id).Revision
	w0 := e.wakes.Load()
	flac := func(title, artist, genre string, disc, no int) catalog.NewTrack {
		return catalog.NewTrack{Blob: catalog.Blob{Hash: newHash(), Size: 4000, Format: catalog.FormatFLAC, DurationMS: ptr(int64(337_000))},
			SourcePath: title + ".flac", Title: title, Artist: artist, Genre: genre, Disc: disc, No: no}
	}
	add := func(nt catalog.NewTrack) uuid.UUID {
		t.Helper()
		got, track, err := e.svc.AddTrack(ctx, id, rev, nt)
		if err != nil {
			t.Fatalf("AddTrack %q: %v", nt.Title, err)
		}
		e.bumped(id, rev, got)
		rev++
		w0 = e.wakes.Load()
		return track
	}

	// Preconditions, in the transaction; refusals before it.
	bg := flac("Blue in Green", "", "", 1, 3)
	_, _, err := e.svc.AddTrack(ctx, id, 0, bg)
	wantCode(t, err, catalog.CodePreconditionRequired)
	_, _, err = e.svc.AddTrack(ctx, id, rev+1, bg)
	wantCode(t, err, catalog.CodePreconditionFailed)
	_, _, err = e.svc.AddTrack(ctx, uuid.New(), 1, bg)
	wantCode(t, err, catalog.CodeAlbumNotFound)
	for format, code := range map[string]string{"": catalog.CodeInvalidBlobFormat, catalog.FormatJPEG: catalog.CodeInvalidBlobFormat, "ogg": catalog.CodeInvalidBlobFormat} {
		nt := flac("x", "", "", 0, 0)
		nt.Blob.Format, nt.Blob.DurationMS = format, nil
		_, _, err = e.svc.AddTrack(ctx, id, rev, nt)
		wantCode(t, err, code)
	}
	for p, code := range map[string]string{"dir/x.flac": catalog.CodeInvalidArgument, "": names.CodePathEmpty, "..": names.CodePathDotSegment} {
		nt := flac("x", "", "", 0, 0)
		nt.SourcePath = p
		_, _, err = e.svc.AddTrack(ctx, id, rev, nt)
		wantCode(t, err, code)
	}
	nt := flac("", "", "", 0, 0)
	_, _, err = e.svc.AddTrack(ctx, id, rev, nt)
	wantCode(t, err, names.CodeTextEmpty)
	e.unchanged(id, rev, w0)
	if n := e.count(`SELECT count(*) FROM tracks WHERE album_id = $1`, id); n != 2 {
		t.Fatalf("%d tracks after the refusals", n)
	}

	// The place of its tags, free: kept. The album's artist (in another
	// case) and genre: inherited. The blob is recorded with its format
	// and duration.
	bg.Artist, bg.Genre = "MILES DAVIS", "Jazz"
	tr := add(bg)
	disc, no, title, artist, genre, source, blob := e.trackRow(tr)
	if disc != 1 || no != 3 || title != "Blue in Green" || artist != nil || genre != nil || source != "Blue in Green.flac" || blob != bg.Blob.Hash {
		t.Fatalf("track %d %d %q %v %v %q", disc, no, title, artist, genre, source)
	}
	if e.blobFormat(bg.Blob.Hash) != catalog.FormatFLAC || e.count(`SELECT count(*) FROM blobs WHERE hash = $1 AND duration_ms = 337000`, bg.Blob.Hash) != 1 {
		t.Fatal("the blob's format or duration")
	}
	// A taken place: appended after the last number of the last disc. Its
	// own artist and genre are kept.
	tr = add(flac("All Blues", "Cannonball Adderley", "Modal", 1, 2))
	disc, no, _, artist, genre, _, _ = e.trackRow(tr)
	if disc != 1 || no != 4 || deref(artist) != "Cannonball Adderley" || deref(genre) != "Modal" {
		t.Fatalf("appended: %d %d %v %v", disc, no, artist, genre)
	}
	// Another disc, free: kept; then no number: appended to that disc.
	tr = add(flac("Flamenco Sketches", "", "", 2, 1))
	if disc, no, _, _, _, _, _ = e.trackRow(tr); disc != 2 || no != 1 {
		t.Fatalf("disc 2: %d %d", disc, no)
	}
	tr = add(flac("Bonus", "", "", 0, 0))
	if disc, no, _, _, _, _, _ = e.trackRow(tr); disc != 2 || no != 2 {
		t.Fatalf("no number: %d %d", disc, no)
	}

	// The same audio is never a second track: 409 naming it, on the
	// snapshot too.
	dup := flac("Again", "", "", 0, 0)
	dup.Blob = bg.Blob
	_, _, err = e.svc.AddTrack(ctx, id, rev, dup)
	if ce := wantCode(t, err, catalog.CodeTrackExists); ce.Details.TrackID == uuid.Nil || len(ce.Details.Names) != 1 || ce.Details.Names[0] != "Blue in Green" {
		t.Fatalf("details %+v", ce.Details)
	}
	v, err := e.svc.GetAlbum(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, catalog.CheckTrackAbsent(v, bg.Blob.Hash), catalog.CodeTrackExists)
	if err := catalog.CheckTrackAbsent(v, newHash()); err != nil {
		t.Fatal(err)
	}
	e.unchanged(id, rev, w0)

	// The genre rule with the new format: an MP3 with a genre an MP3 cannot hold,
	// and an MP3 in an album where a FLAC track has such a genre.
	mp3 := func(genre string) catalog.NewTrack {
		nt := flac("MP3 "+genre, "", genre, 0, 0)
		nt.Blob.Format = catalog.FormatMP3
		return nt
	}
	bad := mp3("(Rock)")
	_, _, err = e.svc.AddTrack(ctx, id, rev, bad)
	wantCode(t, err, catalog.CodeGenreNotWritable)
	u := e.update(id)
	u.Tracks[0].Genre = ptr("13")
	if rev, _, err = e.svc.UpdateAlbum(ctx, id, rev, u); err != nil {
		t.Fatal(err)
	}
	e.clearRenders()
	w0 = e.wakes.Load()
	_, _, err = e.svc.AddTrack(ctx, id, rev, mp3(""))
	wantCode(t, err, catalog.CodeGenreNotWritable)
	// The cover rule with the new format: the album's cover cannot be embedded in
	// an MP3.
	e.mu.Lock()
	e.fits = func(c catalog.Blob, format string) error {
		if format == catalog.FormatMP3 {
			return errors.New("too large for mp3")
		}
		return nil
	}
	e.mu.Unlock()
	u.Tracks[0].Genre = nil
	if rev, _, err = e.svc.UpdateAlbum(ctx, id, rev, u); err != nil {
		t.Fatal(err)
	}
	e.clearRenders()
	w0 = e.wakes.Load()
	ok := mp3("")
	_, _, err = e.svc.AddTrack(ctx, id, rev, ok)
	if ce := wantCode(t, err, catalog.CodeCoverNotEmbeddable); ce.Details.Names[0] != catalog.FormatMP3 {
		t.Fatalf("details %+v", ce.Details)
	}
	e.unchanged(id, rev, w0)
	if n := e.count(`SELECT count(*) FROM blobs WHERE hash = ANY($1)`, []string{bad.Blob.Hash, ok.Blob.Hash}); n != 0 {
		t.Fatal("a refused track registered its blob")
	}
	e.mu.Lock()
	e.fits = nil
	e.mu.Unlock()
	add(ok)

	// In the trash, like every content operation.
	if rev, _, err = e.svc.TrashAlbum(ctx, id, rev); err != nil {
		t.Fatal(err)
	}
	e.clearRenders()
	add(flac("Trashed", "", "", 0, 0))

	// A last disc numbered up to the end: nowhere to append.
	e.exec(`UPDATE tracks SET no = 999 WHERE album_id = $1 AND disc = 2 AND no = 2`, id)
	_, _, err = e.svc.AddTrack(ctx, id, rev, flac("Too many", "", "", 0, 0))
	wantCode(t, err, catalog.CodeInvalidTrackNumber)
}
