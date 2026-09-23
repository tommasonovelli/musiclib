package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"musiclib/internal/blobstore"
	"musiclib/internal/fsops"
	"musiclib/internal/media"
	"musiclib/internal/store"
)

// More error codes of the build.
const (
	// CodeIO: a read of an original or a write of the staging failed for a
	// reason other than space (§11.2), such as EIO.
	CodeIO = "render_io"
	// CodeCanceled: the build's context ended (a shutdown, §11.1): the
	// tools were killed through the Runner and the staging removed.
	CodeCanceled = "render_canceled"
)

const (
	// workDir is the renderer's directory under /data/work: one directory
	// per build, render/<build_id>/album (§3.1, §9.1).
	workDir = "render"
	// albumDir is the album directory inside a build's directory.
	albumDir = "album"
	// spaceMargin is the free space a build must leave on /data (§11.2).
	spaceMargin = 1 << 30
	// tagSlack bounds what a tag write adds to a track besides the cover:
	// the managed fields (at most 11 texts of 1,024 characters) and TagLib's
	// padding, which it caps at 1 MiB (N-085).
	tagSlack = 2 << 20
	// receiptSlack bounds the receipt's bytes per file: a path of at most
	// 1,031 bytes (Extras/ plus §5.2's 1,024), a size, a SHA-256, the JSON.
	receiptSlack = 2 << 10
	// copyBuffer is the unit of the streaming copies (§6.1: never a whole
	// file in memory).
	copyBuffer = 1 << 20
	// dirPerm and filePerm are the modes of the output; the process umask
	// is 022 (§11.1).
	dirPerm  = 0o755
	filePerm = 0o644
)

// Config are the builder's dependencies, passed explicitly (§2.3).
type Config struct {
	Tools *media.Tools
	// Blobs reads the originals (§9.1 step 5).
	Blobs *blobstore.Store
	// Work is /data/work, on the filesystem of library/ (§3.1): the builds
	// live in work/render, and its free space is the space of a build.
	Work *fsops.Root
}

// Builder builds the staging directory of a plan (§9.1 steps 4 to 8). It
// is safe for concurrent use by the pool's workers: every build has its own
// directory and state. It never opens library/: it has no root for it.
type Builder struct {
	tools *media.Tools
	blobs *blobstore.Store
	work  *fsops.Root
}

// New returns a builder and creates work/render durably if missing.
func New(cfg Config) (*Builder, error) {
	if cfg.Tools == nil || cfg.Blobs == nil || cfg.Work == nil {
		return nil, errorf(CodeInvalidArgument, "the builder needs the tools, the blob store and work")
	}
	if err := cfg.Work.MkdirAllSync(workDir, dirPerm); err != nil {
		return nil, err
	}
	return &Builder{tools: cfg.Tools, blobs: cfg.Blobs, work: cfg.Work}, nil
}

// Result is a finished build, what the publisher consumes (§9.3): the
// values of the publication journal.
type Result struct {
	BuildID       uuid.UUID
	AlbumID       uuid.UUID
	AlbumRevision int64
	RenderVersion string
	// Removal: the plan was a removal (§9.1 step 3). There is no staging
	// and no receipt; the build id names the retired directory (§9.3).
	Removal bool
	// Staging is the built album directory relative to /data/work,
	// render/<build_id>/album; "" for a removal.
	Staging string
	// ReceiptHash is the SHA-256 of the staging's .musiclib.json (§9.2);
	// "" for a removal.
	ReceiptHash string
}

// StagingDir is the album directory of a build, relative to /data/work.
func StagingDir(buildID uuid.UUID) string {
	return buildDir(buildID) + "/" + albumDir
}

func buildDir(buildID uuid.UUID) string { return workDir + "/" + buildID.String() }

// Discard removes the directory of a build, whatever it holds: Build uses
// it on every failure, the publisher after a superseded build or once the
// old staging is no longer needed (§6.3, §9.3). A build that does not exist
// (a removal, or one already discarded) is not an error. It never touches
// library/ or originals/.
func (b *Builder) Discard(ctx context.Context, buildID uuid.UUID) error {
	if buildID == uuid.Nil {
		return errorf(CodeInvalidArgument, "no build id")
	}
	return b.work.RemoveAll(ctx, buildDir(buildID))
}

// Build carries out a plan made by NewPlan for this renderer (§9.1): a new
// build id (UUIDv7); for a removal, nothing else. Otherwise, after §11.2's
// space check, the whole album in work/render/<build_id>/album:
//
//  1. the cover, then every track one at a time (§6.1) followed by its LRC,
//     then the attachments; every copy streams its original through
//     SHA-256, compares it with the blob's name (a corrupt original stops
//     the build, blobstore.CodeCorrupt), and reads the copy back (§9.1
//     steps 5 and 7);
//  2. for a track: the audio digest of the copy, its tags inspected, the
//     managed tags written, inspected again and verified against the plan
//     and the first inspection (media.VerifyTags), the digest again; the
//     two digests must be equal (§8.4, §9.1 step 6);
//  3. the final size and SHA-256 of every file, then the receipt (§9.2);
//     every file is fsynced when it is complete, then every new directory
//     bottom-up, the album directory, and its parents render/<build_id>,
//     render and work (§9.1 step 8).
//
// On any failure, cancellation included, the build's directory is removed
// (Discard) before Build returns, and nothing outside it was written:
// library/ and originals/ are untouched (§9.1). Cancelling ctx kills the
// running tool through the Runner (§8.5).
func (b *Builder) Build(ctx context.Context, p Plan) (res Result, err error) {
	if p.RenderVersion != Version {
		return Result{}, errorf(CodeVersionMismatch, "the plan is for render_version %q, this renderer is %q", p.RenderVersion, Version)
	}
	if p.AlbumID == uuid.Nil || p.AlbumRevision <= 0 {
		return Result{}, errorf(CodeInvalidArgument, "the plan has no album or revision")
	}
	res = Result{BuildID: store.NewID(), AlbumID: p.AlbumID, AlbumRevision: p.AlbumRevision,
		RenderVersion: p.RenderVersion, Removal: p.Removal}
	if p.Removal {
		return res, nil
	}
	if len(p.Tracks) == 0 {
		return Result{}, errorf(CodeInvalidArgument, "the plan has no track")
	}
	if err := b.checkSpace(estimate(p)); err != nil {
		return Result{}, err
	}
	defer func() {
		if err != nil {
			// The removal must run even when ctx is what ended the build.
			err = errors.Join(err, b.Discard(context.WithoutCancel(ctx), res.BuildID))
			res = Result{}
		}
	}()
	bd, err := b.stage(res.BuildID)
	if err != nil {
		return res, err
	}
	defer func() { err = errors.Join(err, bd.close()) }()
	if res.ReceiptHash, err = bd.run(ctx, p); err != nil {
		return res, err
	}
	res.Staging = StagingDir(res.BuildID)
	return res, nil
}

// checkSpace is §11.2's check before a build: the estimate plus the 1 GiB
// margin must fit in the space available to the process on /data.
//
// There is no process-wide budget yet (NOTES.md N-114): the pool round adds
// the in-memory reservation of the estimates of the jobs in progress here,
// and in the importer's checkSpace, so that two workers never spend the
// same free space. Every write still handles ENOSPC (§11.2).
func (b *Builder) checkSpace(estimate int64) error {
	fs, err := b.work.StatFS()
	if err != nil {
		return err
	}
	if fs.FreeBytes-spaceMargin < estimate {
		return errorf(CodeInsufficientSpace, "the build needs about %d bytes plus a margin of %d, %d are available",
			estimate, int64(spaceMargin), fs.FreeBytes)
	}
	return nil
}

// estimate is the conservative space estimate of §11.2 for a plan: every
// file at its original size, and each track grown by the embedded cover
// and tagSlack; plus the receipt.
func estimate(p Plan) int64 {
	var cover, n int64
	if p.Cover != nil {
		cover = p.Cover.Blob.Size
		n += cover
	}
	for _, t := range p.Tracks {
		n += t.Blob.Size + cover + tagSlack
	}
	for _, c := range p.Copies {
		n += c.Blob.Size
	}
	return n + int64(len(p.Tracks)+len(p.Copies)+2)*receiptSlack
}

// build is the state of one build in progress.
type build struct {
	b     *Builder
	id    uuid.UUID
	stage *fsops.Root // work/render/<id>/album
	// extras is stage/Extras, opened when the first attachment is copied:
	// a rel_path may have §5.2's 16 levels below it, which the album root
	// could not reach within the same limit (NOTES.md N-132).
	extras *fsops.Root
	// dirs are the directories created in the album directory, relative to
	// it.
	dirs  map[string]bool
	files []ReceiptFile
}

// stage creates the build's directory exclusively (a new UUIDv7 never
// exists; if it did, nothing would be adopted) and opens the album
// directory as a root.
func (b *Builder) stage(id uuid.UUID) (*build, error) {
	if err := b.work.Mkdir(buildDir(id), dirPerm); err != nil {
		return nil, err
	}
	if err := b.work.Mkdir(StagingDir(id), dirPerm); err != nil {
		return nil, err
	}
	stage, err := b.work.SubRoot(StagingDir(id))
	if err != nil {
		return nil, err
	}
	return &build{b: b, id: id, stage: stage, dirs: map[string]bool{}}, nil
}

func (bd *build) close() error {
	var errs []error
	if bd.extras != nil {
		errs = append(errs, bd.extras.Close())
	}
	return errors.Join(append(errs, bd.stage.Close())...)
}

// run builds every file of the plan and returns the receipt_hash.
func (bd *build) run(ctx context.Context, p Plan) (_ string, err error) {
	var cover *os.File
	var expected *media.ExpectedCover
	if p.Cover != nil {
		if err := bd.copyFile(ctx, Copy{Path: p.Cover.Path, Blob: p.Cover.Blob}); err != nil {
			return "", err
		}
		// The verified copy, read-only, is what every track embeds.
		if cover, err = bd.stage.Open(p.Cover.Path); err != nil {
			return "", err
		}
		defer func() {
			if cover != nil {
				err = errors.Join(err, closeFile(cover, p.Cover.Path))
			}
		}()
		expected = &media.ExpectedCover{MIME: p.Cover.MIME, Size: p.Cover.Blob.Size, SHA256: p.Cover.Blob.Hash}
	}
	lyrics := map[string][]Copy{}
	var attachments []Copy
	for _, c := range p.Copies {
		if strings.HasPrefix(c.Path, extrasDir+"/") {
			attachments = append(attachments, c)
			continue
		}
		stem := strings.TrimSuffix(c.Path, ".lrc")
		lyrics[stem] = append(lyrics[stem], c)
	}
	for _, t := range p.Tracks {
		var mc *media.Cover
		if cover != nil {
			mc = &media.Cover{File: cover, Format: p.Cover.Format}
		}
		if err := bd.track(ctx, t, mc, expected); err != nil {
			return "", err
		}
		stem := t.Path[:strings.LastIndexByte(t.Path, '.')]
		for _, c := range lyrics[stem] {
			if err := bd.copyFile(ctx, c); err != nil {
				return "", err
			}
		}
		delete(lyrics, stem)
	}
	if len(lyrics) > 0 {
		return "", errorf(CodeInvalidArgument, "the plan has LRC files without their track")
	}
	for _, c := range attachments {
		if err := bd.copyFile(ctx, c); err != nil {
			return "", err
		}
	}
	if cover != nil {
		f := cover
		cover = nil
		if err := closeFile(f, p.Cover.Path); err != nil {
			return "", err
		}
	}
	hash, err := bd.receipt(ctx, p)
	if err != nil {
		return "", err
	}
	return hash, bd.syncDirs()
}

// track builds one track (§9.1 step 6): the verified copy, the audio digest
// before, the tag write, the reread of the managed and unmanaged tags, the
// audio digest after.
func (bd *build) track(ctx context.Context, t Track, cover *media.Cover, expected *media.ExpectedCover) (err error) {
	f, err := bd.copy(ctx, t.Path, t.Blob)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, closeFile(f, t.Path))
		}
	}()
	tools := bd.b.tools
	before, err := tools.AudioDigest(ctx, f)
	if err != nil {
		return err
	}
	in, err := tools.Inspect(ctx, f, t.Format)
	if err != nil {
		return err
	}
	if err := tools.WriteManagedTags(ctx, f, t.Format, t.Tags, cover); err != nil {
		return err
	}
	if err := failpoint("tags-written", t.Path, f); err != nil {
		return err
	}
	out, err := tools.Inspect(ctx, f, t.Format)
	if err != nil {
		return err
	}
	if err := media.VerifyTags(t.Tags, expected, in, out); err != nil {
		return err
	}
	after, err := tools.AudioDigest(ctx, f)
	if err != nil {
		return err
	}
	if after != before {
		return &Error{Code: CodeAudioChanged, Names: []string{t.Path},
			Message: fmt.Sprintf("the audio of %q changed with the tag write: %s before, %s after", t.Path, describe(before), describe(after))}
	}
	size, sum, err := hashFile(ctx, f, t.Path)
	if err != nil {
		return err
	}
	bd.files = append(bd.files, ReceiptFile{RelativePath: t.Path, Size: size, SHA256: sum})
	closed = true
	return bd.syncClose(f, t.Path)
}

func describe(d media.Digest) string {
	return fmt.Sprintf("%d Hz, %d channels (%s), %d frames, PCM %s", d.SampleRate, d.Channels, d.Layout, d.Frames, d.PCMSHA256)
}

// copyFile is a byte-for-byte copy (§9.1 step 7): the copy verified, its
// hash the blob's, then fsynced and closed.
func (bd *build) copyFile(ctx context.Context, c Copy) error {
	f, err := bd.copy(ctx, c.Path, c.Blob)
	if err != nil {
		return err
	}
	bd.files = append(bd.files, ReceiptFile{RelativePath: c.Path, Size: c.Blob.Size, SHA256: c.Blob.Hash})
	return bd.syncClose(f, c.Path)
}

// copy creates path and copies the original blob into it, streaming, with
// the SHA-256 of what is read compared with the blob's name (§9.1 step 5)
// and the copy read back and compared too. It returns the copy open
// read-write, its offset unspecified.
func (bd *build) copy(ctx context.Context, path string, blob Blob) (_ *os.File, err error) {
	src, err := bd.b.blobs.Open(blob.Hash)
	if err != nil {
		return nil, err
	}
	dst, err := bd.create(path)
	if err != nil {
		return nil, errors.Join(err, closeFile(src, "the original "+blob.Hash))
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, closeFile(dst, path))
		}
	}()
	n, got, err := stream(ctx, src, dst, path)
	if err = errors.Join(err, closeFile(src, "the original "+blob.Hash)); err != nil {
		return nil, err
	}
	if got != blob.Hash {
		return nil, &Error{Code: blobstore.CodeCorrupt, Names: []string{path},
			Message: fmt.Sprintf("the original %s of %q does not match its name: its %d bytes hash to %s; restore it from the backup (§11.3)",
				blob.Hash, path, n, got)}
	}
	if n != blob.Size {
		return nil, errorf(CodeInvalidSnapshot, "the original %s has %d bytes, the catalog says %d", blob.Hash, n, blob.Size)
	}
	size, sum, err := hashFile(ctx, dst, path)
	if err != nil {
		return nil, err
	}
	if size != blob.Size || sum != blob.Hash {
		return nil, &Error{Code: CodeCopyMismatch, Names: []string{path},
			Message: fmt.Sprintf("the copy %q reads back as %d bytes with SHA-256 %s, not the original %s", path, size, sum, blob.Hash)}
	}
	return dst, nil
}

// stream copies src into dst in copyBuffer units and returns the bytes
// copied and the SHA-256 of what was read, checking ctx between units.
func stream(ctx context.Context, src io.Reader, dst *os.File, path string) (int64, string, error) {
	h := sha256.New()
	buf := make([]byte, copyBuffer)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, "", canceled(err)
		}
		k, rerr := src.Read(buf)
		if k > 0 {
			h.Write(buf[:k])
			if err := failpoint("write", path, dst); err != nil {
				return 0, "", writeErr(path, err)
			}
			if _, err := dst.Write(buf[:k]); err != nil {
				return 0, "", writeErr(path, err)
			}
			n += int64(k)
		}
		if rerr == io.EOF {
			return n, hex.EncodeToString(h.Sum(nil)), nil
		}
		if rerr != nil {
			return 0, "", ioErr("reading the original of "+path, rerr)
		}
	}
}

// create creates a new file of the album directory, read-write, after its
// missing directories. It never replaces anything.
func (bd *build) create(path string) (*os.File, error) {
	root, rel, err := bd.rootOf(path)
	if err != nil {
		return nil, err
	}
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		created, err := root.MkdirAll(rel[:i], dirPerm)
		for _, d := range created {
			bd.dirs[bd.albumRel(root, d)] = true
		}
		if err != nil {
			return nil, err
		}
	}
	return root.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_EXCL, filePerm)
}

// rootOf returns the root and relative path of an album path: Extras/...
// through the Extras root, created when first needed.
func (bd *build) rootOf(path string) (*fsops.Root, string, error) {
	rest, ok := strings.CutPrefix(path, extrasDir+"/")
	if !ok {
		return bd.stage, path, nil
	}
	if bd.extras == nil {
		if err := bd.stage.Mkdir(extrasDir, dirPerm); err != nil {
			return nil, "", err
		}
		bd.dirs[extrasDir] = true
		r, err := bd.stage.SubRoot(extrasDir)
		if err != nil {
			return nil, "", err
		}
		bd.extras = r
	}
	return bd.extras, rest, nil
}

// albumRel is the album-relative path of rel under root.
func (bd *build) albumRel(root *fsops.Root, rel string) string {
	if root == bd.extras {
		return extrasDir + "/" + rel
	}
	return rel
}

// receipt writes .musiclib.json (§9.2) from the files built, which must be
// exactly the plan's, and returns its hash.
func (bd *build) receipt(ctx context.Context, p Plan) (string, error) {
	files := slices.Clone(bd.files)
	slices.SortFunc(files, func(a, b ReceiptFile) int { return strings.Compare(a.RelativePath, b.RelativePath) })
	got := make([]string, len(files))
	for i, f := range files {
		got[i] = f.RelativePath
	}
	if !slices.Equal(got, p.Files()) {
		return "", errorf(CodeInvalidArgument, "the files built are not the plan's")
	}
	r := Receipt{AlbumID: p.AlbumID, BuildID: bd.id, AlbumRevision: p.AlbumRevision, RenderVersion: p.RenderVersion, Files: files}
	data, err := r.Encode()
	if err != nil {
		return "", err
	}
	f, err := bd.create(ReceiptName)
	if err != nil {
		return "", err
	}
	if err := failpoint("write", ReceiptName, f); err != nil {
		return "", errors.Join(writeErr(ReceiptName, err), closeFile(f, ReceiptName))
	}
	if _, err := f.Write(data); err != nil {
		return "", errors.Join(writeErr(ReceiptName, err), closeFile(f, ReceiptName))
	}
	hash := ReceiptHash(data)
	size, sum, err := hashFile(ctx, f, ReceiptName)
	if err == nil && (size != int64(len(data)) || sum != hash) {
		err = errorf(CodeCopyMismatch, "the receipt does not read back as written")
	}
	if err != nil {
		return "", errors.Join(err, closeFile(f, ReceiptName))
	}
	return hash, bd.syncClose(f, ReceiptName)
}

// syncClose fsyncs a complete file and closes it, checking both (§13.2).
func (bd *build) syncClose(f *os.File, path string) error {
	if err := failpoint("fsync-file", path, nil); err != nil {
		return errors.Join(writeErr(path, err), closeFile(f, path))
	}
	if err := fsops.SyncAndClose(f); err != nil {
		return writeErr(path, err)
	}
	return nil
}

// syncDirs fsyncs every new directory bottom-up (§9.1 step 8): the album's
// subdirectories deepest first, the album directory, then its parents
// render/<build_id>, render and work, the parent of the staging included.
// Every file was fsynced when it was complete, before any directory.
func (bd *build) syncDirs() error {
	dirs := make([]string, 0, len(bd.dirs))
	for d := range bd.dirs {
		dirs = append(dirs, d)
	}
	slices.SortFunc(dirs, func(a, b string) int {
		if da, db := strings.Count(a, "/"), strings.Count(b, "/"); da != db {
			return db - da
		}
		return strings.Compare(a, b)
	})
	for _, d := range dirs {
		root, rel := bd.stage, d
		if rest, ok := strings.CutPrefix(d, extrasDir+"/"); ok {
			root, rel = bd.extras, rest
		}
		if err := bd.syncDir(root, rel, StagingDir(bd.id)+"/"+d); err != nil {
			return err
		}
	}
	if err := bd.syncDir(bd.stage, "", StagingDir(bd.id)); err != nil {
		return err
	}
	for _, d := range []string{buildDir(bd.id), workDir, ""} {
		if err := bd.syncDir(bd.b.work, d, d); err != nil {
			return err
		}
	}
	return nil
}

// syncDir fsyncs one directory; trace is its path relative to work.
func (bd *build) syncDir(root *fsops.Root, rel, trace string) error {
	if err := failpoint("fsync-dir", trace, nil); err != nil {
		return writeErr(trace, err)
	}
	if err := root.SyncDir(rel); err != nil {
		return writeErr(trace, err)
	}
	return nil
}

// hashFile reads f from its start and returns its size and SHA-256.
func hashFile(ctx context.Context, f *os.File, path string) (int64, string, error) {
	h := sha256.New()
	buf := make([]byte, copyBuffer)
	var off int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, "", canceled(err)
		}
		k, err := f.ReadAt(buf, off)
		h.Write(buf[:k])
		off += int64(k)
		if err == io.EOF {
			return off, hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil {
			return 0, "", ioErr("reading back "+path, err)
		}
	}
}

// writeErr types a failed write of the staging: ENOSPC and EDQUOT are
// CodeInsufficientSpace (§11.2), anything else CodeIO.
func writeErr(path string, err error) error {
	if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
		return &Error{Code: CodeInsufficientSpace, Message: fmt.Sprintf("the disk is full while writing %q", path), Err: err}
	}
	if fsops.Code(err) != "" && fsops.Code(err) != fsops.CodeIO {
		return err
	}
	return ioErr("writing "+path, err)
}

func ioErr(what string, err error) error {
	return &Error{Code: CodeIO, Message: what, Err: err}
}

func canceled(err error) error {
	return &Error{Code: CodeCanceled, Message: "the build was cancelled", Err: err}
}

func closeFile(f *os.File, what string) error {
	if err := f.Close(); err != nil {
		return ioErr("closing "+what, err)
	}
	return nil
}

// failpoint, when a test sets it, runs at named points of a build and
// returns the error to inject there (§12.2, NOTES.md N-044): "write" before
// each write of a copy or of the receipt, "tags-written" after a track's
// tag write, "fsync-file" and "fsync-dir" before each fsync (paths of
// directories relative to work). f is the file concerned, or nil. It is nil
// in production.
var failpointHook func(point, path string, f *os.File) error

func failpoint(point, path string, f *os.File) error {
	if failpointHook == nil {
		return nil
	}
	return failpointHook(point, path, f)
}
