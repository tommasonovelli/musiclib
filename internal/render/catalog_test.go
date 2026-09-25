package render

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/store/pgtest"
)

// The vertical slice up to the staging (§13.1): an album imported from an
// /import-like directory by the real importer into a real PostgreSQL 17,
// its render claimed with its REPEATABLE READ snapshot (§6.2), planned and
// built twice. The two builds are the same bytes (§9.2).
func TestBuildFromCatalog(t *testing.T) {
	e := newEnv(t)
	db := pgtest.New(t)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "import")
	write := func(rel string, b []byte) {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tags := func(n, title string) []string {
		return []string{"TITLE=" + title, "ARTIST=Miles Davis", "ALBUM=Kind of Blue", "TRACKNUMBER=" + n,
			"DATE=1959-08-17", "GENRE=Jazz", "COMMENT=from the rip " + n}
	}
	write("Kind of Blue/01 So What.flac", song{tags: tags("1", "So What")}.flac(t))
	write("Kind of Blue/02 Freddie.flac", song{src: sine(660, 0.3), tags: tags("2", "Freddie Freeloader")}.flac(t))
	write("Kind of Blue/01 So What.lrc", []byte("[00:01.00]So what\n"))
	write("Kind of Blue/cover.jpg", jpegImage(t, 40, 40, 5))
	write("Kind of Blue/Scans/Back cover.png", pngImage(t, 10, 10, 6))
	write("Kind of Blue/rip.log", []byte("log\n"))

	cat, err := catalog.New(db, nil, importer.CoverFits, importer.GenreFits)
	if err != nil {
		t.Fatal(err)
	}
	source := e.hostRoot(src)
	im, err := importer.New(importer.Config{Catalog: cat, Tools: e.tools, Blobs: e.blobs, Source: source, Work: e.work,
		Budget: jobs.NewBudget(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateImportBatch(ctx, uuid.New(), ""); err != nil {
		t.Fatal(err)
	}
	var snap *jobs.RenderSnapshot
	for snap == nil {
		c, err := jobs.ClaimNext(ctx, db, Version)
		if err != nil || c == nil {
			t.Fatalf("claim: %v %v", c, err)
		}
		switch c.Kind {
		case jobs.KindScan:
			err = im.ExecuteScan(ctx, c)
		case jobs.KindImport:
			err = im.ExecuteImport(ctx, c)
		case jobs.KindRender:
			snap = c.Render
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := NewPlan(snap, Version)
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir.Path != "Miles Davis/Kind of Blue" || p.AlbumRevision != 1 {
		t.Fatalf("plan %+v", p)
	}
	want := []string{"01 - So What.flac", "01 - So What.lrc", "02 - Freddie Freeloader.flac",
		"Extras/Scans/Back cover.png", "Extras/cover.jpg", "Extras/rip.log", "cover.jpg"}
	if got := p.Files(); !slices.Equal(got, want) {
		t.Fatalf("files %q\nwant  %q", got, want)
	}
	r1, r2 := e.mustBuild(p), e.mustBuild(p)
	compareBuilds(t, e, r1, e, r2)

	files, _, _ := e.staged(r1)
	cover := &media.ExpectedCover{MIME: "image/jpeg", Size: snap.Cover.Size, SHA256: snap.Cover.Hash}
	for _, tr := range p.Tracks {
		in, out := e.blob(tr.Blob.Hash), files[tr.Path]
		if err := media.VerifyTags(tr.Tags, cover, e.inspect("in.flac", in), e.inspect("out.flac", out)); err != nil {
			t.Errorf("%s: %v", tr.Path, err)
		}
		if e.digest("in.flac", in) != e.digest("out.flac", out) {
			t.Errorf("%s: the audio changed", tr.Path)
		}
	}
	so := e.inspect("so.flac", files["01 - So What.flac"])
	if !slices.Equal(so.Managed.Date, []string{"1959"}) || !slices.Equal(so.Managed.AlbumArtist, []string{"Miles Davis"}) ||
		!slices.Equal(find(so.Unmanaged, "vorbis:COMMENT"), []string{"from the rip 1"}) {
		t.Errorf("tags %+v %+v", so.Managed, so.Unmanaged)
	}
}

// hostRoot opens a directory of the test as a root.
func (e *env) hostRoot(dir string) *fsops.Root {
	e.t.Helper()
	r, err := fsops.OpenRoot(dir)
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
