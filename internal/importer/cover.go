package importer

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // the cover formats of §8.5, registered for image.Decode
	_ "image/png"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/names"
)

// Limits of a cover (§8.5). The size limit is the catalog's, which checks it
// again at the commit.
const (
	MaxCoverBytes  = catalog.MaxCoverBytes
	MaxCoverPixels = 40_000_000
)

// coverNames are the external cover files of §7.4, in the order of
// preference: cover.*, then folder.*, then front.*, at the root of the
// candidate.
var coverNames = [...]string{"cover", "folder", "front"}

// validateCover is §8.5's check of an image, done in Go (N-073: the pinned
// ffmpeg has no PNG decoder): JPEG or PNG by content, at most MaxCoverBytes,
// at most MaxCoverPixels, and a complete decode. It returns the blobs.format
// value, or why the image is not a valid cover.
func validateCover(r io.ReadSeeker, size int64) (string, error) {
	if size > MaxCoverBytes {
		return "", fmt.Errorf("it takes %d bytes, the maximum is %d", size, MaxCoverBytes)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	cfg, format, err := image.DecodeConfig(r)
	if err != nil || (format != media.FormatJPEG && format != media.FormatPNG) {
		return "", errors.New("it is not a JPEG or PNG image")
	}
	if px := int64(cfg.Width) * int64(cfg.Height); px <= 0 || px > MaxCoverPixels {
		return "", fmt.Errorf("it has %d×%d pixels, the maximum is %d", cfg.Width, cfg.Height, MaxCoverPixels)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if _, decoded, err := image.Decode(r); err != nil || decoded != format {
		return "", fmt.Errorf("the %s image does not decode completely", format)
	}
	return format, nil
}

// coverTrack is a track as the cover selection sees it: its path, its blob
// and its embedded pictures, in the final track order.
type coverTrack struct {
	Path     string
	Blob     blobstore.Blob
	Format   string
	Pictures []media.Picture
}

// coverFile is a file of the candidate that may be an external cover.
type coverFile struct {
	Path string
	Blob blobstore.Blob
}

// coverSelection is the outcome of chooseCover.
type coverSelection struct {
	// Cover is the chosen blob with its format; nil for none (§7.4:
	// cover_hash = NULL).
	Cover    *catalog.Blob
	Warnings []jobs.Warning
}

// chooseCover is §7.4's cover selection, with the owner's rule N-091:
//
//  1. the candidate's root files cover.*, then folder.*, then front.*; in
//     each group by normalized name (names.Key), then by the name's bytes;
//  2. the embedded front covers, the most frequent first (by SHA-256), a
//     tie going to the smallest hash;
//  3. every embedded picture, in track order and file order, whatever its
//     type (ffmpeg writes type 0, N-087).
//
// Each candidate must be a valid JPEG or PNG (validateCover) and fit every
// audio format of the album (CoverFits, N-091); otherwise it is skipped with
// a warning and the next one is tried. The same image (hash) is tried once.
// A chosen embedded picture is extracted and pinned through the blob store;
// an external one is already a blob and stays an attachment too (§7.4).
func (im *Importer) chooseCover(ctx context.Context, files []coverFile, tracks []coverTrack) (coverSelection, error) {
	var sel coverSelection
	formats := audioFormatsOf(tracks)
	tried := map[string]bool{}
	fits := func(b catalog.Blob) error {
		for _, f := range formats {
			if err := CoverFits(b, f); err != nil {
				return err
			}
		}
		return nil
	}
	for _, f := range externalCovers(files) {
		if tried[f.Blob.SHA256] {
			continue
		}
		tried[f.Blob.SHA256] = true
		b, w, err := im.tryExternal(f, fits)
		if err != nil {
			return coverSelection{}, err
		}
		if w != nil {
			sel.Warnings = append(sel.Warnings, *w)
			continue
		}
		sel.Cover = &b
		return sel, nil
	}
	for _, p := range embeddedCandidates(tracks) {
		if tried[p.pic.SHA256] {
			continue
		}
		tried[p.pic.SHA256] = true
		b, w, err := im.tryEmbedded(ctx, p, fits)
		if err != nil {
			return coverSelection{}, err
		}
		if w != nil {
			sel.Warnings = append(sel.Warnings, *w)
			continue
		}
		sel.Cover = &b
		return sel, nil
	}
	return sel, nil
}

// audioFormatsOf returns the audio formats of the tracks, sorted.
func audioFormatsOf(tracks []coverTrack) []string {
	var out []string
	for _, t := range tracks {
		if !slices.Contains(out, t.Format) {
			out = append(out, t.Format)
		}
	}
	slices.Sort(out)
	return out
}

// externalCovers returns the root files named cover.*, folder.* and
// front.* in the order of §7.4, ties by normalized name and then by bytes,
// never by scan order.
func externalCovers(files []coverFile) []coverFile {
	var out []coverFile
	for _, name := range coverNames {
		var group []coverFile
		for _, f := range files {
			base := path.Base(f.Path)
			if f.Path != base || path.Ext(base) == "" {
				continue // not at the root, or no extension
			}
			if names.Key(strings.TrimSuffix(base, path.Ext(base))) == name {
				group = append(group, f)
			}
		}
		slices.SortFunc(group, func(a, b coverFile) int {
			return cmp.Or(strings.Compare(names.Key(a.Path), names.Key(b.Path)), strings.Compare(a.Path, b.Path))
		})
		out = append(out, group...)
	}
	return out
}

// embeddedPicture is one embedded picture and the track that holds it.
type embeddedPicture struct {
	track coverTrack
	pic   media.Picture
}

// embeddedCandidates returns the embedded pictures in the order of §7.4:
// the front covers by descending frequency then ascending hash, one
// occurrence each (the first in track order), then every picture in track
// and file order.
func embeddedCandidates(tracks []coverTrack) []embeddedPicture {
	var all []embeddedPicture
	count := map[string]int{}
	first := map[string]embeddedPicture{}
	for _, t := range tracks {
		for _, p := range t.Pictures {
			e := embeddedPicture{track: t, pic: p}
			all = append(all, e)
			if p.Type == media.PictureFrontCover {
				if count[p.SHA256] == 0 {
					first[p.SHA256] = e
				}
				count[p.SHA256]++
			}
		}
	}
	fronts := make([]string, 0, len(count))
	for h := range count {
		fronts = append(fronts, h)
	}
	slices.SortFunc(fronts, func(a, b string) int { return cmp.Or(count[b]-count[a], strings.Compare(a, b)) })
	out := make([]embeddedPicture, 0, len(fronts)+len(all))
	for _, h := range fronts {
		out = append(out, first[h])
	}
	return append(out, all...)
}

// tryExternal validates an external cover candidate from its blob. A
// candidate that is refused gives a warning, not an error.
func (im *Importer) tryExternal(f coverFile, fits func(catalog.Blob) error) (catalog.Blob, *jobs.Warning, error) {
	r, err := im.blobs.Open(f.Blob.SHA256)
	if err != nil {
		return catalog.Blob{}, nil, err
	}
	format, verr := validateCover(r, f.Blob.Size)
	if err := r.Close(); err != nil {
		return catalog.Blob{}, nil, &Error{Code: fsops.CodeIO, Message: "closing blob " + f.Blob.SHA256, Err: err}
	}
	if verr != nil {
		return catalog.Blob{}, &jobs.Warning{Code: jobs.WarnCoverSkipped, Path: f.Path,
			Message: fmt.Sprintf("%q is not used as the cover: %v", f.Path, verr)}, nil
	}
	b := catalog.Blob{Hash: f.Blob.SHA256, Size: f.Blob.Size, Format: format}
	if err := fits(b); err != nil {
		return catalog.Blob{}, &jobs.Warning{Code: jobs.WarnCoverNotEmbeddable, Path: f.Path,
			Message: fmt.Sprintf("%q is not used as the cover, it stays an attachment: %v", f.Path, err)}, nil
	}
	return b, nil, nil
}

// tryEmbedded extracts an embedded picture into work/import, validates it,
// and pins it through the blob store if it is a valid cover that fits.
func (im *Importer) tryEmbedded(ctx context.Context, e embeddedPicture, fits func(catalog.Blob) error) (catalog.Blob, *jobs.Warning, error) {
	where := fmt.Sprintf("picture %d embedded in %q", e.pic.Index, e.track.Path)
	skipped := func(code jobs.WarningCode, why error) (catalog.Blob, *jobs.Warning, error) {
		return catalog.Blob{}, &jobs.Warning{Code: code, Path: e.track.Path,
			Message: fmt.Sprintf("the %s is not used as the cover: %v", where, why)}, nil
	}
	if e.pic.Size > MaxCoverBytes {
		return skipped(jobs.WarnCoverSkipped, fmt.Errorf("it takes %d bytes, the maximum is %d", e.pic.Size, MaxCoverBytes))
	}
	tmp := workDir + "/" + rand.Text() + ".img"
	var (
		b    catalog.Blob
		w    *jobs.Warning
		err  error
		made bool
	)
	b, w, made, err = im.extractAndPin(ctx, e, tmp, fits)
	if made {
		if rerr := im.work.Remove(tmp); rerr != nil && err == nil {
			err = rerr
		}
	}
	if err != nil || w != nil {
		return catalog.Blob{}, w, err
	}
	return b, nil, nil
}

// extractAndPin is tryEmbedded's work on the temporary tmp; made reports
// whether tmp was created.
func (im *Importer) extractAndPin(ctx context.Context, e embeddedPicture, tmp string, fits func(catalog.Blob) error) (b catalog.Blob, w *jobs.Warning, made bool, err error) {
	where := fmt.Sprintf("picture %d embedded in %q", e.pic.Index, e.track.Path)
	dst, err := im.work.CreateExclusive(tmp, 0o600)
	if err != nil {
		return catalog.Blob{}, nil, false, err
	}
	made = true
	audio, err := im.blobs.Open(e.track.Blob.SHA256)
	if err != nil {
		return catalog.Blob{}, nil, made, errors.Join(err, dst.Close())
	}
	imgs, err := im.tools.ExtractImages(ctx, audio, e.track.Format, []media.ImageTarget{{Index: e.pic.Index, Dst: dst}})
	if media.Code(err) == media.CodeTagsNoSpace {
		// The helper met ENOSPC writing work/import (§11.2, N-143).
		err = &Error{Code: CodeInsufficientSpace, Message: "the disk is full while extracting the " + where, Err: err}
	}
	err = errors.Join(err, closeErr(audio, "the audio blob"), closeErr(dst, "the extracted picture"))
	if err != nil {
		return catalog.Blob{}, nil, made, err
	}
	if imgs[0].SHA256 != e.pic.SHA256 || imgs[0].Size != e.pic.Size {
		return catalog.Blob{}, nil, made, &Error{Code: media.CodeOutputInvalid,
			Message: fmt.Sprintf("the %s was extracted with another content than inspected", where)}
	}
	img, err := im.work.Open(tmp)
	if err != nil {
		return catalog.Blob{}, nil, made, err
	}
	defer func() { err = errors.Join(err, closeErr(img, "the extracted picture")) }()
	format, verr := validateCover(img, e.pic.Size)
	if verr != nil {
		return catalog.Blob{}, &jobs.Warning{Code: jobs.WarnCoverSkipped, Path: e.track.Path,
			Message: fmt.Sprintf("the %s is not used as the cover: %v", where, verr)}, made, nil
	}
	cand := catalog.Blob{Hash: e.pic.SHA256, Size: e.pic.Size, Format: format}
	if ferr := fits(cand); ferr != nil {
		return catalog.Blob{}, &jobs.Warning{Code: jobs.WarnCoverNotEmbeddable, Path: e.track.Path,
			Message: fmt.Sprintf("the %s is not used as the cover: %v", where, ferr)}, made, nil
	}
	if _, err := img.Seek(0, io.SeekStart); err != nil {
		return catalog.Blob{}, nil, made, &Error{Code: fsops.CodeIO, Message: "rewinding the extracted picture", Err: err}
	}
	pinned, err := im.blobs.Put(ctx, img)
	if err != nil {
		return catalog.Blob{}, nil, made, err
	}
	if pinned.SHA256 != cand.Hash || pinned.Size != cand.Size {
		return catalog.Blob{}, nil, made, &Error{Code: media.CodeOutputInvalid,
			Message: fmt.Sprintf("the %s changed between its check and its pin", where)}
	}
	return cand, nil, made, nil
}

// closeErr closes f and types a failure.
func closeErr(f *os.File, what string) error {
	if err := f.Close(); err != nil {
		return &Error{Code: fsops.CodeIO, Message: "closing " + what, Err: err}
	}
	return nil
}
