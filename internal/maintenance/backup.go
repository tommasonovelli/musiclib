package maintenance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/blobstore"
	"musiclib/internal/buildinfo"
	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
	"musiclib/internal/media"
	"musiclib/internal/render"
	"musiclib/internal/store"
	"musiclib/internal/volume"
)

// Manifest is the authoritative inventory of a completed backup. The dump
// preserves the catalog and import reports; published output is regenerated.
type Manifest struct {
	StoreID       uuid.UUID    `json:"store_id"`
	SchemaVersion int64        `json:"schema_version"`
	AppVersion    string       `json:"app_version"`
	RenderVersion string       `json:"render_version"`
	DumpSHA256    string       `json:"dump_sha256"`
	Blobs         []BackupBlob `json:"blobs"`
}
type BackupBlob struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

const (
	backupManifest = "manifest.json"
	backupDump     = "catalog.dump"
	backupBlobs    = "originals"
)

// Backup creates a new final directory only after a durable, verified dump
// and every original have been copied. The caller holds the volume flock and
// has identified and opened the existing layout. A failed run leaves a
// uniquely named .tmp directory for the operator; it never removes data.
func Backup(ctx context.Context, db *pgxpool.Pool, v *volume.Volume, to, dbURL string, hook failpoint.Hook) (outErr error) {
	if err := RequireCurrentSchema(ctx, db); err != nil {
		return err
	}
	version, err := store.LatestSchemaVersion()
	if err != nil {
		return fail(CodeSchema, "cannot read the embedded migrations", err)
	}
	parent, name, err := backupDestination(to, v)
	if err != nil {
		return err
	}
	defer func() { outErr = errors.Join(outErr, parent.Close()) }()
	if _, err := parent.Stat(name); err == nil {
		return refuse("backup_exists", "final backup already exists")
	} else if fsops.Code(err) != fsops.CodeNotFound {
		return err
	}
	tmp := ".musiclib-backup-" + rand.Text() + ".tmp"
	if err := parent.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	if err := parent.SyncDir(""); err != nil {
		return err
	}
	if err := hook.Hit("backup_temporary"); err != nil {
		return err
	}
	dest, err := parent.SubRoot(tmp)
	if err != nil {
		return err
	}
	defer func() { outErr = errors.Join(outErr, dest.Close()) }()
	dump, err := dest.CreateExclusive(backupDump, 0o600)
	if err != nil {
		return err
	}
	runner := media.NewRunner(1)
	// libpq accepts a URI in PGDATABASE; never put DATABASE_URL in argv,
	// findings or logs. A tool failure is deliberately reported without its
	// stderr, which may echo a connection string containing credentials.
	uri, password, credErr := pgConnection(dbURL)
	if credErr != nil {
		return errors.Join(credErr, dump.Close())
	}
	runResult, runErr := runner.Run(ctx, media.Command{Path: "/usr/lib/postgresql/17/bin/pg_dump", Args: []string{"--format=custom", "--no-owner", "--no-acl", "--dbname=" + uri}, Env: []string{"PGPASSWORD=" + password}, Stdout: dump, Timeout: 2 * time.Hour})
	closeErr := fsops.SyncAndClose(dump)
	if runErr != nil {
		return errors.Join(fail("backup_dump_failed", "pg_dump failed: "+safeToolStderr(runResult.Stderr, password), runErr), closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if err := dest.SyncDir(""); err != nil {
		return err
	}
	if err := hook.Hit("backup_dump"); err != nil {
		return err
	}
	dumpHash, _, err := hashFile(ctx, dest, backupDump)
	if err != nil {
		return err
	}
	if err := dest.Mkdir(backupBlobs, 0o755); err != nil {
		return err
	}
	if err := dest.SyncDir(""); err != nil {
		return err
	}
	originals, err := dest.SubRoot(backupBlobs)
	if err != nil {
		return err
	}
	blobs := make([]BackupBlob, 0)
	// All originals, including unreferenced ones, are preserved (§11.4).
	// Reject any unexpected entry rather than silently omitting user bytes.
	walkErr := walkBackupBlobs(ctx, v.Originals(), originals, "", &blobs, hook)
	closeOriginals := originals.Close()
	if err := errors.Join(walkErr, closeOriginals); err != nil {
		return err
	}
	if err := hook.Hit("backup_copied"); err != nil {
		return err
	}
	// The catalog size is checked too: a correct hash at the wrong catalog
	// size is still inconsistent and must not be blessed by a backup.
	catalogBlobs, err := store.New(db).DoctorBlobs(ctx)
	if err != nil {
		return err
	}
	found := make(map[string]int64, len(blobs))
	for _, b := range blobs {
		found[b.SHA256] = b.Size
	}
	for _, b := range catalogBlobs {
		if n, ok := found[b.Hash]; ok && n != b.Size {
			return fail("backup_blob_size", "catalog size differs for "+b.Hash, nil)
		}
		// A lookup, not a zero-value read: a missing zero-size blob must
		// not pass as present.
		if n, ok := found[b.Hash]; b.Referenced != nil && *b.Referenced && (!ok || n != b.Size) {
			return fail("backup_blob_missing", "referenced blob is absent: "+b.Hash, nil)
		}
	}
	manifest := Manifest{StoreID: v.StoreID(), SchemaVersion: version, AppVersion: buildinfo.Version, RenderVersion: render.Version, DumpSHA256: dumpHash, Blobs: blobs}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := durableFile(dest, backupManifest, data, 0o644); err != nil {
		return err
	}
	if err := dest.SyncDir(""); err != nil {
		return err
	}
	if err := hook.Hit("backup_before_rename"); err != nil {
		return err
	}
	if err := fsops.RenameNoReplace(parent, tmp, parent, name); err != nil {
		return err
	}
	return parent.SyncDir("")
}

// backupDestination validates the absolute path without ever traversing a
// symlink component, including an alias that would lead back into /data.
func backupDestination(to string, v *volume.Volume) (dest *fsops.Root, name string, result error) {
	if !filepath.IsAbs(to) || filepath.Clean(to) != to || to == "/data" || strings.HasPrefix(to, "/data/") {
		return nil, "", refuse("backup_destination", "use an absolute destination outside /data")
	}
	parentPath, name := filepath.Split(to)
	parentPath = filepath.Clean(parentPath)
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".musiclib-backup-") {
		return nil, "", refuse("backup_destination", "invalid final directory name")
	}
	root, err := fsops.OpenRoot("/")
	if err != nil {
		return nil, "", err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	var protected []fsops.FileInfo
	for _, media := range []*fsops.Root{v.Root(), v.Originals(), v.Library(), v.Work()} {
		if media == nil { // restore has no media layout until after verification
			continue
		}
		info, err := media.Stat("")
		if err != nil {
			return nil, "", err
		}
		protected = append(protected, info)
	}
	prefix := ""
	for _, component := range strings.Split(strings.TrimPrefix(parentPath, "/"), "/") {
		if component == "" {
			continue
		}
		if prefix != "" {
			prefix += "/"
		}
		prefix += component
		// SubRoot rejects symlinks at every intermediate component. Stat alone
		// reports the last symlink instead of following it.
		info, err := root.Stat(prefix)
		if err != nil {
			return nil, "", err
		}
		if info.Type != fsops.TypeDir {
			return nil, "", refuse("backup_destination", "directory path contains a non-directory or symlink")
		}
		for _, forbidden := range protected {
			if info.Dev == forbidden.Dev && info.Ino == forbidden.Ino {
				return nil, "", refuse("backup_destination", "destination is inside the data volume")
			}
		}
	}
	if prefix == "" {
		return nil, "", refuse("backup_destination", "a parent directory is required")
	}
	dest, err = root.SubRoot(prefix)
	if err != nil {
		return nil, "", err
	}
	if err := outsideDataSource(dest, v.Root()); err != nil {
		return nil, "", errors.Join(err, dest.Close())
	}
	return dest, name, nil
}

// outsideDataSource refuses a destination that lies inside the data
// directory on the underlying filesystem although not in this process's
// view: two bind mounts whose host paths are nested (MUSICLIB_BACKUP under
// MUSICLIB_DATA). Container paths and inodes cannot see it; the filesystem
// device and root in /proc/self/mountinfo can (N-228).
func outsideDataSource(dest, data *fsops.Root) error {
	dataSrc, err := fsops.SourceOf(data)
	if err != nil {
		return err
	}
	destSrc, err := fsops.SourceOf(dest)
	if err != nil {
		return err
	}
	if dataSrc.Contains(destSrc) {
		return refuse("backup_destination", "destination is inside the data volume's host directory")
	}
	return nil
}

func durableFile(root *fsops.Root, name string, data []byte, perm os.FileMode) error {
	f, err := root.CreateExclusive(name, perm)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	return errors.Join(writeErr, fsops.SyncAndClose(f))
}

func walkBackupBlobs(ctx context.Context, src, dest *fsops.Root, dir string, result *[]BackupBlob, hook failpoint.Hook) error {
	entries, err := src.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := e.Name
		if dir != "" {
			rel = dir + "/" + e.Name
		}
		depth := strings.Count(rel, "/")
		if e.Type == fsops.TypeDir && depth < 2 {
			if err := dest.Mkdir(rel, 0o755); err != nil {
				return err
			}
			if err := dest.SyncDir(dir); err != nil {
				return err
			}
			if err := walkBackupBlobs(ctx, src, dest, rel, result, hook); err != nil {
				return err
			}
			if err := dest.SyncDir(rel); err != nil {
				return err
			}
			continue
		}
		if e.Type != fsops.TypeRegular || depth != 2 || len(strings.Split(rel, "/")[0]) != 2 || len(strings.Split(rel, "/")[1]) != 2 || blobstore.ValidateSHA(e.Name) != nil || !strings.HasPrefix(e.Name, strings.ReplaceAll(dir, "/", "")) {
			return fail("backup_unsafe_original", fmt.Sprintf("unexpected entry %s (%s); no final backup created", rel, e.Type), nil)
		}
		input, err := src.Open(rel)
		if err != nil {
			return err
		}
		output, err := dest.CreateExclusive(rel, 0o444)
		if err != nil {
			return errors.Join(err, input.Close())
		}
		h := sha256.New()
		n, copyErr := io.Copy(output, &contextReader{ctx: ctx, input: io.TeeReader(input, h)})
		closeErr := errors.Join(input.Close(), fsops.SyncAndClose(output))
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != e.Name {
			return fail("backup_corrupt_blob", e.Name+" does not match its hash", nil)
		}
		*result = append(*result, BackupBlob{SHA256: e.Name, Size: n})
		if err := hook.Hit("backup_blob"); err != nil {
			return err
		}
	}
	return nil
}

// pgConnection parses either pgx URI or keyword/value syntax, and constructs
// a libpq URI from approved fields only. Neither argv nor errors contain a
// credential or a pgx-only query parameter.
func pgConnection(raw string) (string, string, error) {
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		return "", "", refuse("maintenance_database", "invalid DATABASE_URL")
	}
	if cfg.Host == "" || cfg.Database == "" {
		return "", "", refuse("maintenance_database", "DATABASE_URL needs host and database")
	}
	u := &url.URL{Scheme: "postgresql", User: url.User(cfg.User), Host: cfg.Host + ":" + strconv.Itoa(int(cfg.Port)), Path: "/" + cfg.Database}
	if strings.Contains(cfg.Host, ":") {
		u.Host = "[" + cfg.Host + "]:" + strconv.Itoa(int(cfg.Port))
	}
	q := url.Values{}
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", "", refuse("maintenance_database", "invalid DATABASE_URL")
		}
		for _, key := range []string{"sslmode", "sslrootcert", "sslcert", "sslkey", "connect_timeout", "application_name"} {
			if val := parsed.Query().Get(key); val != "" {
				q.Set(key, val)
			}
		}
	} else {
		if cfg.TLSConfig != nil {
			return "", "", refuse("maintenance_database", "use a PostgreSQL URI to preserve TLS settings for offline tools")
		}
		q.Set("sslmode", "disable")
	}
	u.RawQuery = q.Encode()
	return u.String(), cfg.Password, nil
}

var toolSecret = regexp.MustCompile(`(?i)(postgres(?:ql)?://)[^\s]+|password=[^\s]+|sslpassword=[^\s]+`)

func safeToolStderr(stderr []byte, password string) string {
	text := toolSecret.ReplaceAllString(string(stderr), "[redacted connection]")
	if password != "" {
		text = strings.ReplaceAll(text, password, "[redacted]")
	}
	return text
}

type contextReader struct {
	ctx   context.Context
	input io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.input.Read(p)
}
