// Package blobstore pins and reads the immutable originals (DESIGN.md §3.1,
// §7.5). It is the single blob-put implementation required by §13.2, used by
// import and uploads alike.
//
// Layout: originals/ab/cd/<sha256>, temporaries in work/blobs/<random>.tmp;
// both roots are on the same filesystem (§3.1). There is no operation that
// removes or rewrites a pinned blob (§3.2 guarantee 2, no GC in v1).
package blobstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
)

const (
	tempDir    = "blobs"
	tempSuffix = ".tmp"
	// blobPerm is set at creation, so a blob is read-only from its first
	// byte and the rename in step 4 needs no chmod (N-042). Creating with
	// O_WRONLY is allowed on a mode the file does not have yet.
	blobPerm = 0o444
	dirPerm  = 0o755
	bufSize  = 256 << 10
)

// Blob identifies a pinned original: lowercase hex SHA-256 and size.
type Blob struct {
	SHA256 string
	Size   int64
}

// Store holds the originals root and the work root that contains the
// temporaries. It is safe for concurrent use.
type Store struct {
	originals *fsops.Root
	work      *fsops.Root
	// failpoints is nil in production. The package's tests set it to
	// inject an error, or to crash the process, at the named points of a
	// put: temp_synced, temp_verified, shards_synced, pinned (§12.2,
	// NOTES.md N-142).
	failpoints failpoint.Hook
}

// New returns a Store over the two roots, creating work/blobs durably if it
// is missing.
func New(originals, work *fsops.Root) (*Store, error) {
	if err := work.MkdirAllSync(tempDir, dirPerm); err != nil {
		return nil, wrap("create temp dir", "", err)
	}
	return &Store{originals: originals, work: work}, nil
}

// Put copies src into the store following §7.5 and returns its hash and size.
// A nil error means the blob is pinned, verified and durable; only then may a
// transaction reference it. Putting content that is already pinned verifies
// the existing file instead of trusting its name, so concurrent puts of the
// same content all succeed on one intact file.
//
// The temporary is removed on every path that did not rename it, and a
// failed removal is part of the returned error.
func (s *Store) Put(ctx context.Context, src io.Reader) (_ Blob, err error) {
	tmp := tempDir + "/" + rand.Text() + tempSuffix
	f, err := s.work.CreateExclusive(tmp, blobPerm)
	if err != nil {
		return Blob{}, wrap("create temp", "", err)
	}
	renamed := false
	defer func() {
		if renamed {
			return
		}
		if rerr := s.work.Remove(tmp); rerr != nil {
			err = errors.Join(err, wrap("remove temp", "", rerr))
		}
	}()

	// Steps 1-2: copy, hash, fsync, close, then re-read what landed on disk.
	b, err := writeTemp(ctx, f, src)
	if err != nil {
		return Blob{}, err
	}
	if err := s.failpoints.Hit("temp_synced"); err != nil {
		return Blob{}, wrap("temp_synced", b.SHA256, err)
	}
	got, err := readBlob(ctx, s.work, tmp, false)
	if err != nil {
		return Blob{}, wrap("reread temp", b.SHA256, err)
	}
	if got != b {
		return Blob{}, &Error{Code: CodeIO, Op: "reread temp", SHA: b.SHA256,
			Err: fmt.Errorf("re-read %s (%d bytes), wrote %d bytes", got.SHA256, got.Size, b.Size)}
	}
	if err := s.failpoints.Hit("temp_verified"); err != nil {
		return Blob{}, wrap("temp_verified", b.SHA256, err)
	}

	// Step 3: the chain is synced even when it already existed, because a
	// concurrent put may have created it and not synced it yet.
	shard, rel := shardOf(b.SHA256), relOf(b.SHA256)
	if _, err := s.originals.MkdirAll(shard, dirPerm); err != nil {
		return Blob{}, wrap("create shard", b.SHA256, err)
	}
	if err := s.originals.SyncDirAndParents(shard); err != nil {
		return Blob{}, wrap("sync shard", b.SHA256, err)
	}
	if err := s.failpoints.Hit("shards_synced"); err != nil {
		return Blob{}, wrap("shards_synced", b.SHA256, err)
	}

	// Step 4.
	err = fsops.RenameNoReplace(s.work, tmp, s.originals, rel)
	switch {
	case err == nil:
		renamed = true
	case fsops.Code(err) == fsops.CodeExists:
		if err := s.checkExisting(ctx, b); err != nil {
			return Blob{}, err
		}
	default:
		return Blob{}, wrap("pin", b.SHA256, err)
	}
	if err := s.failpoints.Hit("pinned"); err != nil {
		return Blob{}, wrap("pinned", b.SHA256, err)
	}

	// Step 5, on both branches: a concurrent put may have renamed without
	// syncing yet (§7.5).
	if err := s.work.SyncDir(tempDir); err != nil {
		return Blob{}, wrap("sync temp dir", b.SHA256, err)
	}
	if err := s.originals.SyncDir(shard); err != nil {
		return Blob{}, wrap("sync shard", b.SHA256, err)
	}
	return b, nil
}

// writeTemp performs steps 1-2 up to close; f is closed on every path.
func writeTemp(ctx context.Context, f *os.File, src io.Reader) (Blob, error) {
	h := sha256.New()
	cr := &ctxReader{ctx: ctx, r: src}
	n, err := io.CopyBuffer(io.MultiWriter(f, h), cr, make([]byte, bufSize))
	if err != nil {
		if cr.err != nil && ctx.Err() == nil {
			err = &Error{Code: CodeSource, Op: "read source", Err: err}
		} else {
			err = wrap("write temp", "", err)
		}
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, wrap("close temp", "", cerr))
		}
		return Blob{}, err
	}
	if err := fsops.SyncAndClose(f); err != nil {
		return Blob{}, wrap("sync temp", "", err)
	}
	return Blob{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// checkExisting is the already-exists branch of §7.5: whatever sits at the
// blob's name is re-hashed and fsynced before the temp is discarded, and a
// mismatch is reported, never replaced (§12.2 "Blob esistente corrotto").
func (s *Store) checkExisting(ctx context.Context, want Blob) error {
	got, err := readBlob(ctx, s.originals, relOf(want.SHA256), true)
	if err != nil {
		return readErr("verify existing", want.SHA256, err)
	}
	return compare("verify existing", want, got)
}

// Open opens a pinned blob read-only. Only a regular file is returned. The
// caller hashes what it copies (§9.1 step 5): opening proves nothing about
// the content.
func (s *Store) Open(sha string) (*os.File, error) {
	if err := ValidateSHA(sha); err != nil {
		return nil, err
	}
	f, err := s.originals.Open(relOf(sha))
	if err != nil {
		return nil, readErr("open", sha, err)
	}
	return f, nil
}

// Check is the doctor's normal check (§11.3): the blob exists as a regular
// file of the expected size. It does not read the content.
func (s *Store) Check(want Blob) error {
	if err := ValidateSHA(want.SHA256); err != nil {
		return err
	}
	fi, err := s.originals.Stat(relOf(want.SHA256))
	if err != nil {
		return readErr("stat", want.SHA256, err)
	}
	if fi.Type != fsops.TypeRegular {
		return &Error{Code: CodeCorrupt, Op: "stat", SHA: want.SHA256,
			Err: fmt.Errorf("found a %s", fi.Type)}
	}
	return compare("stat", want, Blob{SHA256: want.SHA256, Size: fi.Size})
}

// Verify is the doctor's deep check (§11.3): it re-hashes the whole blob and
// returns its size, which the caller compares with the catalog when it has
// one (unreferenced blobs have none).
func (s *Store) Verify(ctx context.Context, sha string) (int64, error) {
	if err := ValidateSHA(sha); err != nil {
		return 0, err
	}
	got, err := readBlob(ctx, s.originals, relOf(sha), false)
	if err != nil {
		return 0, readErr("verify", sha, err)
	}
	return got.Size, compare("verify", Blob{SHA256: sha, Size: got.Size}, got)
}

// CleanTemps removes the leftover temporaries of interrupted puts and returns
// their names (§11.1 step 5). It must run before any Put starts: an in-flight
// temporary is indistinguishable from a leftover. It only lists work/blobs,
// so originals are out of its reach; entries it did not create are left.
func (s *Store) CleanTemps(ctx context.Context) ([]string, error) {
	entries, err := s.work.ReadDir(tempDir)
	if err != nil {
		return nil, wrap("list temps", "", err)
	}
	var removed []string
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return removed, wrap("clean temps", "", err)
		}
		if e.Type != fsops.TypeRegular || !strings.HasSuffix(e.Name, tempSuffix) {
			continue
		}
		if err := s.work.Remove(tempDir + "/" + e.Name); err != nil {
			return removed, wrap("remove temp", "", err)
		}
		removed = append(removed, e.Name)
	}
	return removed, nil
}

// ValidateSHA accepts exactly 64 lowercase hex digits, the only form that
// maps to a path (§3.1); it runs before any path is built from input.
func ValidateSHA(sha string) error {
	ok := len(sha) == 2*sha256.Size
	for i := 0; ok && i < len(sha); i++ {
		c := sha[i]
		ok = '0' <= c && c <= '9' || 'a' <= c && c <= 'f'
	}
	if !ok {
		return &Error{Code: CodeInvalidHash, Op: "validate",
			Err: fmt.Errorf("%q is not 64 lowercase hex digits", sha)}
	}
	return nil
}

func shardOf(sha string) string { return sha[:2] + "/" + sha[2:4] }
func relOf(sha string) string   { return shardOf(sha) + "/" + sha }

func compare(op string, want, got Blob) error {
	if got == want {
		return nil
	}
	return &Error{Code: CodeCorrupt, Op: op, SHA: want.SHA256,
		Err: fmt.Errorf("content is %s (%d bytes), want %d bytes", got.SHA256, got.Size, want.Size)}
}

// readBlob hashes a regular file of r, optionally fsyncing it through the
// same descriptor. The context is checked between reads.
func readBlob(ctx context.Context, r *fsops.Root, rel string, sync bool) (_ Blob, err error) {
	f, err := r.Open(rel)
	if err != nil {
		return Blob{}, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	h := sha256.New()
	n, err := io.CopyBuffer(h, &ctxReader{ctx: ctx, r: f}, make([]byte, bufSize))
	if err != nil {
		return Blob{}, err
	}
	if sync {
		if err := fsops.SyncFile(f); err != nil {
			return Blob{}, err
		}
	}
	return Blob{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// ctxReader makes a long copy cancellable (§13.2) and records whether a
// failure came from the reader rather than from the writer.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
	err error
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		c.err = err
		return 0, err
	}
	n, err := c.r.Read(p)
	if err != nil && err != io.EOF {
		c.err = err
	}
	return n, err
}
