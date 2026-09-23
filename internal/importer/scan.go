package importer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"

	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/names"
)

// noProbeExtensions are the extensions whose files the scan does not probe:
// the kinds §7.2 names as never needing an audio probe (images, PDF, CUE,
// LOG) and the LRC files of §7.4. Compared ASCII case-insensitively. They
// make up nearly every non-audio file of a rip, so the scan runs about one
// probe per unusual file only (NOTES.md N-115). The import probes every
// file of its candidate anyway, by content, on the verified copies (§7.3).
var noProbeExtensions = [...]string{
	"jpg", "jpeg", "png", "gif", "bmp", "tif", "tiff", "webp",
	"pdf", "cue", "log", "lrc",
}

// scanAudio is the scan's view of whether a source file is audio, used only
// to group candidates (§7.2):
//   - a known audio extension (media.HasKnownAudioExtension) is audio,
//     whatever its content: a corrupt one is the album's error, found by the
//     import;
//   - an empty file, or one with an extension of noProbeExtensions, is not;
//   - any other file is probed (ffprobe on the source, read only): audio,
//     supported or not, is audio.
//
// The final recognition is by content, at the import (§7.2).
func (im *Importer) scanAudio(ctx context.Context, r *fsops.Root, f *srcFile) (bool, error) {
	name := path.Base(f.Rel)
	if media.HasKnownAudioExtension(name) {
		return true, nil
	}
	ext := path.Ext(name)
	if f.ID.Size == 0 || (ext != "" && slices.Contains(noProbeExtensions[:], asciiLower(ext[1:]))) {
		return false, nil
	}
	file, err := im.src.open(r, f.Rel)
	if err != nil {
		return false, err
	}
	p, err := im.tools.Probe(ctx, file)
	if cerr := file.Close(); cerr != nil && err == nil {
		err = &Error{Code: fsops.CodeIO, Message: fmt.Sprintf("closing %q", f.Rel), Err: cerr}
	}
	if err != nil {
		return false, err
	}
	return p.Class == media.ClassAudio || p.Class == media.ClassUnsupportedAudio, nil
}

// markAudio sets srcFile.Audio on every file of the tree, in path order.
func (im *Importer) markAudio(ctx context.Context, r *fsops.Root, d *srcDir) error {
	for _, f := range d.files() {
		a, err := im.scanAudio(ctx, r, f)
		if err != nil {
			return err
		}
		f.Audio = a
	}
	return nil
}

// scan is the work of a scan job: the batch's directory walked, candidates
// grouped, the import jobs and the scan's outcome built (§7.2). An error is
// the scan's failure: the root is missing, is not a directory, or a tool or
// the filesystem failed.
func (im *Importer) scan(ctx context.Context, c *jobs.Claim) (catalog.ScanOutcome, error) {
	b, err := im.catalog.GetImportBatch(ctx, c.BatchID)
	if err != nil {
		return catalog.ScanOutcome{}, err
	}
	segs, err := names.SplitRelPathOrRoot(b.RootRel)
	if err != nil {
		return catalog.ScanOutcome{}, err
	}
	r, owned, err := im.src.sub(b.RootRel)
	if err != nil {
		return catalog.ScanOutcome{}, sourceError(b.RootRel, err)
	}
	tree, err := walk(ctx, im.src, r, len(segs), "")
	if err == nil {
		err = im.markAudio(ctx, r, tree)
	}
	if owned {
		if cerr := r.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if err != nil {
		return catalog.ScanOutcome{}, sourceError(b.RootRel, err)
	}
	return scanOutcome(c, b.RootRel, group(tree, b.RootRel)), nil
}

// scanOutcome turns a grouping into the import jobs and the scan's result.
// Paths are relative to /import. Without any valid candidate the scan is
// failed with CodeNoValidCandidate: a batch completed with an explanation,
// not an empty success (§7.2).
func scanOutcome(c *jobs.Claim, rootRel string, g grouping) catalog.ScanOutcome {
	o := catalog.ScanOutcome{Attempt: c.Attempt, BatchID: c.BatchID}
	valid := 0
	for _, br := range g.Branches {
		j := jobs.ImportJob{SourceRel: joinRel(rootRel, br.Dir.Rel)}
		if br.Err != nil {
			j.ErrorCode, j.ErrorMessage = br.Err.Code, br.Err.Message
		} else {
			valid++
		}
		o.Imports = append(o.Imports, j)
	}
	var ws []jobs.Warning
	for _, f := range g.Unassigned {
		p := joinRel(rootRel, f.Rel)
		ws = append(ws, jobs.Warning{Code: jobs.WarnUnassignedFile, Path: p,
			Message: fmt.Sprintf("%q is outside every album candidate and was not imported", p)})
	}
	for _, r := range g.Rejected {
		e := rejectedError(r, rootRel)
		w := jobs.Warning{Code: jobs.WarnRejectedEntry, Message: e.Message}
		if r.Valid {
			w.Path = joinRel(rootRel, r.Rel)
		}
		ws = append(ws, w)
	}
	o.Result = jobs.Result{State: jobs.StateDone, Warnings: ws}
	if valid == 0 {
		o.Result.State, o.Result.ErrorCode = jobs.StateFailed, CodeNoValidCandidate
		switch {
		case len(g.Branches) == 0:
			o.Result.ErrorMessage = fmt.Sprintf("no directory with audio files under %s", label(rootRel, ""))
		default:
			o.Result.ErrorMessage = fmt.Sprintf("%d album candidate(s) under %s, none of them importable: see their errors",
				len(g.Branches), label(rootRel, ""))
		}
	}
	return o
}

// sourceError types the failure to open or read a directory of /import.
func sourceError(rel string, err error) error {
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	switch fsops.Code(err) {
	case fsops.CodeNotFound:
		return &Error{Code: CodeSourceNotFound, Message: fmt.Sprintf("%s does not exist under /import", label(rel, "")), Err: err}
	case fsops.CodeNotDirectory:
		return &Error{Code: CodeSourceNotDirectory, Message: fmt.Sprintf("%s is not a directory", label(rel, "")), Err: err}
	case fsops.CodeSymlink, fsops.CodeSpecialFile:
		return &Error{Code: CodeSourceRejected, Message: fmt.Sprintf("%s is or goes through a symlink or a special file", label(rel, "")), Err: err}
	}
	return err
}
