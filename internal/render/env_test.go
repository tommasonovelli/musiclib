package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// env is a build environment on real ext4 (TMPDIR in the gate): /data with
// originals, library and work, the blob store, the real pinned tools and
// the builder. library/ is never given to the builder; the tests check
// that it and originals/ are untouched.
type env struct {
	t     *testing.T
	data  string
	work  *fsops.Root
	blobs *blobstore.Store
	tools *media.Tools
	b     *Builder
}

// sharedTools are the verified tools, one Runner for the whole test binary.
var sharedTools = sync.OnceValues(func() (*media.Tools, error) {
	return media.NewTools(context.Background(), media.NewRunner(4), media.FFmpegPath, media.FFprobePath, media.TagsPath)
})

func testTools(t testing.TB) *media.Tools {
	t.Helper()
	tools, err := sharedTools()
	if err != nil {
		t.Fatalf("NewTools: %v (run the tests in Docker, docs/docker.md)", err)
	}
	return tools
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, data: filepath.Join(t.TempDir(), "data"), tools: testTools(t)}
	for _, d := range []string{"originals", "library", "work"} {
		if err := os.MkdirAll(filepath.Join(e.data, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Something already published, which no build may touch.
	e.writeHost("library/Someone/Old Album/01 - x.flac", []byte("published bytes"))
	originals := e.root("originals")
	e.work = e.root("work")
	var err error
	if e.blobs, err = blobstore.New(originals, e.work); err != nil {
		t.Fatal(err)
	}
	if e.b, err = New(Config{Tools: e.tools, Blobs: e.blobs, Work: e.work, Budget: jobs.NewBudget()}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) root(rel string) *fsops.Root {
	e.t.Helper()
	r, err := fsops.OpenRoot(filepath.Join(e.data, rel))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		if err := r.Close(); err != nil {
			e.t.Error(err)
		}
	})
	return r
}

func (e *env) writeHost(rel string, b []byte) {
	e.t.Helper()
	p := filepath.Join(e.data, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// put pins b as a blob and returns its snapshot reference.
func (e *env) put(b []byte, format string) jobs.SnapshotBlob {
	e.t.Helper()
	blob, err := e.blobs.Put(context.Background(), bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	return jobs.SnapshotBlob{Hash: blob.SHA256, Size: blob.Size, Format: format}
}

// album is a snapshot builder over real blobs.
type album struct {
	e *env
	s *jobs.RenderSnapshot
}

func (e *env) album(artist, title string) *album {
	return &album{e: e, s: &jobs.RenderSnapshot{
		Attempt:       jobs.Attempt{JobID: uuid.New(), Ticket: 1},
		RenderVersion: Version,
		Artist:        jobs.SnapshotArtist{ID: uuid.New(), Name: artist, Revision: 1},
		Album:         jobs.SnapshotAlbum{ID: uuid.New(), Title: title, Year: 1959, Genre: textOf("Jazz"), Revision: 2},
	}}
}

func (a *album) track(disc, no int, title string, s song) *jobs.SnapshotTrack {
	a.s.Tracks = append(a.s.Tracks, jobs.SnapshotTrack{ID: uuid.New(), Disc: disc, No: no, Title: title,
		SourcePath: title + ".flac", Blob: a.e.put(s.flac(a.e.t), catalog.FormatFLAC)})
	return &a.s.Tracks[len(a.s.Tracks)-1]
}

func (a *album) lyrics(tr *jobs.SnapshotTrack, text string) {
	tr.Lyrics = a.e.put([]byte(text), "")
}

func (a *album) cover(img []byte, format string) {
	a.s.Cover = a.e.put(img, format)
}

func (a *album) attach(rel string, b []byte) {
	a.s.Attachments = append(a.s.Attachments, jobs.SnapshotAttachment{ID: uuid.New(), RelPath: rel, Blob: a.e.put(b, "")})
}

func (a *album) plan() Plan {
	a.e.t.Helper()
	p, err := NewPlan(a.s, Version)
	if err != nil {
		a.e.t.Fatalf("NewPlan: %v", err)
	}
	return p
}

// build runs a build and checks that library/ and originals/ are unchanged,
// and that a failed build left nothing in work/render.
func (e *env) build(p Plan) (Result, error) {
	e.t.Helper()
	lib, orig := e.tree("library"), e.tree("originals")
	res, err := e.b.Build(context.Background(), p)
	e.checkUntouched(lib, orig)
	if err != nil {
		e.checkNoBuilds()
	}
	return res, err
}

func (e *env) mustBuild(p Plan) Result {
	e.t.Helper()
	res, err := e.build(p)
	if err != nil {
		var me *media.Error
		if errors.As(err, &me) {
			e.t.Fatalf("Build: %v\nstderr: %s", err, me.Stderr)
		}
		e.t.Fatalf("Build: %v", err)
	}
	return res
}

func (e *env) checkUntouched(lib, orig map[string]string) {
	e.t.Helper()
	for name, before := range map[string]map[string]string{"library": lib, "originals": orig} {
		if after := e.tree(name); !mapsEqual(before, after) {
			e.t.Fatalf("%s/ changed during a build:\nbefore %v\nafter  %v", name, before, after)
		}
	}
}

// checkNoBuilds: work/render holds no build directory.
func (e *env) checkNoBuilds() {
	e.t.Helper()
	ents, err := os.ReadDir(filepath.Join(e.data, "work", workDir))
	if err != nil {
		e.t.Fatal(err)
	}
	if len(ents) > 0 {
		e.t.Fatalf("work/render holds %d entries after a failed build", len(ents))
	}
}

// tree maps every entry under data/rel to its type, mode and content hash.
func (e *env) tree(rel string) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	base := filepath.Join(e.data, rel)
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(base, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		v := info.Mode().String()
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += " " + sha(b)
		}
		out[filepath.ToSlash(r)] = v
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// staged lists the regular files of a build's album directory, relative to
// it, sorted, with their bytes.
func (e *env) staged(res Result) (files map[string][]byte, names []string, dirs []string) {
	e.t.Helper()
	files = map[string][]byte{}
	base := filepath.Join(e.data, "work", filepath.FromSlash(res.Staging))
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(base, p)
		r = filepath.ToSlash(r)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if info.Mode().Perm() != dirPerm {
				e.t.Errorf("directory %s has mode %v", r, info.Mode())
			}
			if r != "." {
				dirs = append(dirs, r)
			}
		case d.Type().IsRegular():
			if info.Mode().Perm() != filePerm {
				e.t.Errorf("file %s has mode %v", r, info.Mode())
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files[r] = b
			names = append(names, r)
		default:
			e.t.Errorf("%s is a %v", r, info.Mode())
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	sort.Strings(names)
	return files, names, dirs
}

// blob reads a blob's bytes back from originals/.
func (e *env) blob(hash string) []byte {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.data, "originals", hash[:2], hash[2:4], hash))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// hostFile writes b to a temporary file and opens it, for the tools.
func (e *env) hostFile(name string, b []byte) *os.File {
	e.t.Helper()
	dir := e.t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		e.t.Fatal(err)
	}
	r, err := fsops.OpenRoot(dir)
	if err != nil {
		e.t.Fatal(err)
	}
	f, err := r.Open(name)
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		if err := f.Close(); err != nil {
			e.t.Error(err)
		}
	})
	return f
}

func (e *env) inspect(name string, b []byte) media.Inspection {
	e.t.Helper()
	in, err := e.tools.Inspect(context.Background(), e.hostFile(name, b), catalog.FormatFLAC)
	if err != nil {
		e.t.Fatalf("Inspect %s: %v", name, err)
	}
	return in
}

func (e *env) digest(name string, b []byte) media.Digest {
	e.t.Helper()
	d, err := e.tools.AudioDigest(context.Background(), e.hostFile(name, b))
	if err != nil {
		e.t.Fatalf("AudioDigest %s: %v", name, err)
	}
	return d
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
