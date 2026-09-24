package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"slices"
	"strings"

	"github.com/google/uuid"

	"musiclib/internal/fsops"
	"musiclib/internal/render"
)

const (
	// retiredDir holds the directories taken out of library/ until they
	// are removed, work/retired/<build_id> (§3.1, §9.3).
	retiredDir = "retired"
	// dirPerm is the mode of the artist directories created in library/;
	// the process umask is 022 (§11.1).
	dirPerm = 0o755
)

// entryKind is what a path of library/ or work/ is on disk.
type entryKind int

const (
	absent entryKind = iota
	directory
	// unsafe: a symlink, a special file, a regular file, or a path whose
	// resolution meets one of them (§9.3: always refused).
	unsafe
)

// entry is an album directory as observed: its kind and, for a directory,
// its receipt.
type entry struct {
	kind entryKind
	// what describes an unsafe entry for messages.
	what string
	// receipt is the parsed .musiclib.json, nil if there is none or it is
	// not a valid receipt (output damage at the album's own path, §9.3);
	// receiptHash is the SHA-256 of its bytes.
	receipt     *render.Receipt
	receiptHash string
}

// holds reports whether the directory is exactly the build of j: its
// receipt names the album and the build, and has the journal's hash (§9.4:
// "ricevuta con build_id e hash attesi").
func (e entry) holds(j Journal) bool {
	return e.kind == directory && e.receipt != nil && e.receipt.AlbumID == j.AlbumID &&
		e.receipt.BuildID == j.BuildID && e.receiptHash == j.ReceiptHash
}

// foreign reports whether the directory carries the receipt of another
// album: never replaced nor retired (§9.3).
func (e entry) foreign(album uuid.UUID) bool {
	return e.kind == directory && e.receipt != nil && e.receipt.AlbumID != album
}

// observe describes rel under root without following anything: fsops
// resolves with RESOLVE_NO_SYMLINKS, so a symlink anywhere on the path is
// unsafe, not followed. Reading the receipt is the only content read: the
// critical section reads only small receipts and metadata (§2.2).
func observe(root *fsops.Root, rel string) (entry, error) {
	fi, err := root.Stat(rel)
	switch fsops.Code(err) {
	case "":
	case fsops.CodeNotFound:
		return entry{kind: absent}, nil
	case fsops.CodeSymlink, fsops.CodeNotDirectory, fsops.CodeSpecialFile:
		return entry{kind: unsafe, what: fsops.Code(err)}, nil
	default:
		return entry{}, err
	}
	if fi.Type != fsops.TypeDir {
		return entry{kind: unsafe, what: fi.Type.String()}, nil
	}
	e := entry{kind: directory}
	data, err := readReceipt(root, rel)
	if err != nil || data == nil {
		return e, err
	}
	if r, perr := render.ParseReceipt(data); perr == nil {
		e.receipt, e.receiptHash = &r, sha(data)
	}
	return e, nil
}

// readReceipt returns the bytes of dir's receipt, or nil if there is none
// or it is not a regular file (damage, not an error). At most
// render.MaxReceiptBytes + 1 bytes are read: ParseReceipt refuses more.
func readReceipt(root *fsops.Root, dir string) (_ []byte, err error) {
	f, err := root.Open(dir + "/" + render.ReceiptName)
	switch fsops.Code(err) {
	case "":
	case fsops.CodeNotFound, fsops.CodeIsDirectory, fsops.CodeSymlink, fsops.CodeSpecialFile, fsops.CodeNotDirectory:
		return nil, nil
	default:
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = wrap(CodeIO, cerr, "closing the receipt of %s", dir)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(f, render.MaxReceiptBytes+1))
	if err != nil {
		return nil, wrap(CodeIO, err, "reading the receipt of %s", dir)
	}
	return data, nil
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// checkStaging verifies that the build of j is in its staging, complete:
// a directory whose receipt names the album, the build, the revision and
// the renderer of j, with j's receipt hash. code is the error to return
// otherwise: CodeStagingInvalid before the journal, CodeIllegalState after.
func (p *Publisher) checkStaging(j Journal, code string) error {
	e, err := observe(p.work, j.staging())
	if err != nil {
		return err
	}
	switch {
	case e.kind == absent:
		return errorf(code, "%s: the staging %s does not exist", j, j.staging())
	case e.kind == unsafe:
		return errorf(code, "%s: the staging %s is a %s", j, j.staging(), e.what)
	case e.receipt == nil:
		return errorf(code, "%s: the staging %s has no valid receipt", j, j.staging())
	case !e.holds(j) || e.receipt.AlbumRevision != j.Revision || e.receipt.RenderVersion != j.Renderer:
		return errorf(code, "%s: the staging's receipt is %s with hash %s", j, e.receipt, e.receiptHash)
	}
	return nil
}

// checkParent verifies that the artist directory of an album path is
// absent or a real directory.
func (p *Publisher) checkParent(albumPath string, code string) error {
	e, err := p.observeDir(artistDir(albumPath))
	if err != nil {
		return err
	}
	if e == unsafe {
		return errorf(code, "the artist directory of %s is not a directory", albumPath)
	}
	return nil
}

// observeDir is observe without the receipt.
func (p *Publisher) observeDir(rel string) (entryKind, error) {
	fi, err := p.library.Stat(rel)
	switch fsops.Code(err) {
	case "":
	case fsops.CodeNotFound:
		return absent, nil
	case fsops.CodeSymlink, fsops.CodeNotDirectory, fsops.CodeSpecialFile:
		return unsafe, nil
	default:
		return absent, err
	}
	if fi.Type != fsops.TypeDir {
		return unsafe, nil
	}
	return directory, nil
}

// preflight is the check of §9.3 before the journal, under publishMu: the
// staging holds the build, and the destinations and their ownership on
// disk allow the planned transition. A conflict found here fails only this
// job (§9.3: "senza impegnare il journal globale"); nothing is changed.
func (p *Publisher) preflight(j Journal) error {
	if !j.Removal() {
		if err := p.checkStaging(j, CodeStagingInvalid); err != nil {
			return err
		}
		if err := p.checkParent(j.NewPath, CodeUnsafeEntry); err != nil {
			return err
		}
		e, err := observe(p.library, j.NewPath)
		if err != nil {
			return err
		}
		if err := checkNew(j, e, CodeUnsafeEntry, CodeDestinationOccupied, CodeForeignOutput); err != nil {
			return err
		}
	}
	if j.renames() {
		e, err := observe(p.library, j.OldPath)
		if err != nil {
			return err
		}
		if err := checkOld(j, e, CodeUnsafeEntry, CodeForeignOutput); err != nil {
			return err
		}
	}
	return nil
}

// checkNew is the ownership rule of §9.3 for the new path: absent, or the
// album's own published directory (exactly the old path), whatever it holds
// except another album's receipt: a missing receipt or an altered file
// there is output damage, which the build replaces.
func checkNew(j Journal, e entry, unsafeCode, occupiedCode, foreignCode string) error {
	switch {
	case e.kind == absent:
		return nil
	case e.kind == unsafe:
		return errorf(unsafeCode, "%s: the new path is a %s", j, e.what)
	case e.foreign(j.AlbumID):
		return errorf(foreignCode, "%s: the new path holds the output of album %s", j, e.receipt.AlbumID)
	case j.NewPath != j.OldPath:
		return errorf(occupiedCode, "%s: the new path is already a directory that this album has not published; "+
			"move it out of the library (§3.3)", j)
	}
	return nil
}

// checkOld is the rule for the old path to retire: absent (already
// retired), or a directory without another album's receipt.
func checkOld(j Journal, e entry, unsafeCode, foreignCode string) error {
	switch {
	case e.kind == unsafe:
		return errorf(unsafeCode, "%s: the old path is a %s", j, e.what)
	case e.foreign(j.AlbumID):
		return errorf(foreignCode, "%s: the old path holds the output of album %s", j, e.receipt.AlbumID)
	}
	return nil
}

// install is §9.3 B, filesystem only, with no transaction open. It is also
// the forward completion of §9.4: it looks at the disk and performs only
// the steps still missing, so that a crash anywhere, and any number of
// runs, end in the same state.
//
//   - New path holding the journal's build (its receipt: album, build and
//     hash): already installed; the exchange is never repeated.
//   - Otherwise the build must be in its staging, and the new path is
//     either absent (created parents, then RENAME_NOREPLACE) or exactly the
//     album's old path (RENAME_EXCHANGE: the old album goes into the
//     staging). Anything else is CodeIllegalState.
//   - The old path, when it differs from the new one: absent means already
//     retired; a directory is moved to work/retired/<build_id> with
//     RENAME_NOREPLACE, so an existing retired directory is never
//     overwritten. Then the old artist directory is removed if empty, with
//     rmdir; a failure there is only a warning.
//   - Every directory involved is fsynced: the new and old artist
//     directories, library/, the build's directory, work/retired, work/.
//
// Every refusal is CodeIllegalState and deletes nothing (§9.4).
func (p *Publisher) install(ctx context.Context, j Journal) error {
	// The whole observed state is checked before anything moves, so that a
	// refusal leaves the disk exactly as it was found.
	if err := p.checkTransition(j); err != nil {
		return err
	}
	var syncs []syncTarget
	if !j.Removal() {
		e, err := observe(p.library, j.NewPath)
		if err != nil {
			return err
		}
		if !e.holds(j) {
			if err := p.installNew(j, e); err != nil {
				return err
			}
		}
		syncs = append(syncs,
			syncTarget{p.library, artistDir(j.NewPath)}, syncTarget{p.library, ""},
			syncTarget{p.work, buildDir(j.BuildID)}, syncTarget{p.work, renderDir(j.BuildID)})
	}
	if j.renames() {
		if err := p.retireOld(j); err != nil {
			return err
		}
		if err := failpoint("retired"); err != nil {
			return err
		}
		p.removeArtistDir(j)
		syncs = append(syncs,
			syncTarget{p.library, artistDir(j.OldPath)}, syncTarget{p.library, ""},
			syncTarget{p.work, retiredDir}, syncTarget{p.work, ""})
	}
	if err := ctx.Err(); err != nil {
		return wrap(CodeCanceled, err, "%s: interrupted before the fsyncs", j)
	}
	return syncAll(syncs)
}

// checkTransition verifies, without changing anything, that the disk is in
// a state from which the journal's transition can be completed: the new
// path installed, or the staging complete with the new path absent or
// exactly the album's old directory; the old path absent, or a directory
// without another album's receipt whose retired name is still free.
func (p *Publisher) checkTransition(j Journal) error {
	if !j.Removal() {
		e, err := observe(p.library, j.NewPath)
		if err != nil {
			return err
		}
		if !e.holds(j) {
			if err := checkNew(j, e, CodeIllegalState, CodeIllegalState, CodeIllegalState); err != nil {
				return err
			}
			if err := p.checkStaging(j, CodeIllegalState); err != nil {
				return err
			}
			if err := p.checkParent(j.NewPath, CodeIllegalState); err != nil {
				return err
			}
		}
	}
	if !j.renames() {
		return nil
	}
	e, err := observe(p.library, j.OldPath)
	if err != nil || e.kind == absent {
		return err
	}
	if err := checkOld(j, e, CodeIllegalState, CodeIllegalState); err != nil {
		return err
	}
	r, err := observe(p.work, j.retired())
	if err != nil {
		return err
	}
	if r.kind != absent {
		return errorf(CodeIllegalState, "%s: %s already exists and the old directory is still in the library", j, j.retired())
	}
	return nil
}

// installNew moves the staging into place (§9.3 B), after checking again
// what the preflight checked: under publishMu nothing of this process can
// have changed it, but a recovery starts from whatever the disk holds.
func (p *Publisher) installNew(j Journal, e entry) error {
	if err := checkNew(j, e, CodeIllegalState, CodeIllegalState, CodeIllegalState); err != nil {
		return err
	}
	if err := p.checkStaging(j, CodeIllegalState); err != nil {
		return err
	}
	if e.kind == absent {
		if err := p.checkParent(j.NewPath, CodeIllegalState); err != nil {
			return err
		}
		if _, err := p.library.MkdirAll(artistDir(j.NewPath), dirPerm); err != nil {
			return fsErr(j, err, "creating the artist directory")
		}
		if err := fsops.RenameNoReplace(p.work, j.staging(), p.library, j.NewPath); err != nil {
			return fsErr(j, err, "installing the staging")
		}
	} else {
		// The album's own output at exactly the old path: the exchange
		// swaps the complete old and new directories (§3.2 guarantee 5).
		if err := fsops.RenameExchange(p.work, j.staging(), p.library, j.NewPath); err != nil {
			return fsErr(j, err, "exchanging the staging with the published directory")
		}
	}
	return failpoint("installed")
}

// retireOld moves the old directory to work/retired/<build_id>, once.
func (p *Publisher) retireOld(j Journal) error {
	e, err := observe(p.library, j.OldPath)
	if err != nil {
		return err
	}
	if e.kind == absent {
		return nil // already retired (§9.3): not a reason to delete anything else
	}
	if err := checkOld(j, e, CodeIllegalState, CodeIllegalState); err != nil {
		return err
	}
	if err := fsops.RenameNoReplace(p.library, j.OldPath, p.work, j.retired()); err != nil {
		if fsops.Code(err) == fsops.CodeExists {
			return wrap(CodeIllegalState, err, "%s: %s already exists and the old directory is still in the library", j, j.retired())
		}
		return fsErr(j, err, "retiring the old directory")
	}
	return nil
}

// removeArtistDir removes the old artist directory if it is now empty,
// under publishMu so that no other publisher races with it (§9.3). A
// directory that is not empty stays; any other failure is a warning.
func (p *Publisher) removeArtistDir(j Journal) {
	dir := artistDir(j.OldPath)
	if !j.Removal() && dir == artistDir(j.NewPath) {
		return // the new directory is in it
	}
	err := p.library.Rmdir(dir)
	switch fsops.Code(err) {
	case "", fsops.CodeNotFound, fsops.CodeDirNotEmpty:
	default:
		p.log.Warn("the old artist directory could not be removed", "album_id", j.AlbumID, "build_id", j.BuildID,
			"path", dir, "error", err.Error())
	}
}

type syncTarget struct {
	root *fsops.Root
	rel  string
}

// syncAll fsyncs every target once, deepest first. A directory that no
// longer exists (an artist directory just removed, a build directory
// cleaned by an operator) has nothing to make durable; its parent is in
// the list.
func syncAll(targets []syncTarget) error {
	slices.SortStableFunc(targets, func(a, b syncTarget) int {
		return depth(b.rel) - depth(a.rel)
	})
	done := map[syncTarget]bool{}
	for _, t := range targets {
		if done[t] {
			continue
		}
		done[t] = true
		if err := t.root.SyncDir(t.rel); err != nil {
			if fsops.Code(err) == fsops.CodeNotFound {
				continue
			}
			return wrap(CodeIO, err, "fsync of %s/%s", t.root.Name(), t.rel)
		}
	}
	return nil
}

func depth(rel string) int {
	if rel == "" {
		return 0
	}
	return strings.Count(rel, "/") + 1
}

// cleanup removes, after the release of publishMu, what the publication
// left in work/: the old album that an exchange put into the staging, and
// the retired directory (§9.3). A failure is an operational warning, never
// a failed publication; the boot resumes it (§11.1 step 5).
func (p *Publisher) cleanup(ctx context.Context, j Journal) {
	for _, rel := range []string{buildDir(j.BuildID), j.retired()} {
		if err := p.work.RemoveAll(ctx, rel); err != nil && fsops.Code(err) != fsops.CodeNotFound {
			p.log.Warn("cleanup after the publication failed; the boot will resume it", "album_id", j.AlbumID,
				"build_id", j.BuildID, "path", rel, "error", err.Error())
		}
	}
}

// buildDir is render/<build_id>, the staging's parent, relative to work/.
func buildDir(id uuid.UUID) string { return parent(render.StagingDir(id)) }

// renderDir is render, the builds' directory, relative to work/.
func renderDir(id uuid.UUID) string { return parent(buildDir(id)) }

func parent(rel string) string { return rel[:strings.LastIndexByte(rel, '/')] }

// fsErr types a filesystem failure after PREPARE: a refusal that reveals
// an unexpected entry (it exists, it is a symlink, a special file or not a
// directory) is CodeIllegalState; anything else, such as EIO or ENOSPC, is
// CodeIO. Both leave the journal pending.
func fsErr(j Journal, err error, what string) error {
	switch fsops.Code(err) {
	case fsops.CodeExists, fsops.CodeSymlink, fsops.CodeSpecialFile, fsops.CodeNotDirectory, fsops.CodeIsDirectory,
		fsops.CodeNotFound, fsops.CodeDirNotEmpty:
		return wrap(CodeIllegalState, err, "%s: %s", j, what)
	}
	return wrap(CodeIO, err, "%s: %s", j, what)
}
