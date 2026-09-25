package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/names"
)

// render_version is derived from every input of §2.1, each one a token of
// its own: dropping an input from the constant, or adding one without its
// token, fails here.
func TestVersionInputs(t *testing.T) {
	inputs := []string{RendererRevision, names.AlgorithmVersion, GoVersion,
		media.PinnedVersion, media.PinnedTagsVersion, media.PinnedTagLibVersion}
	tokenRE := regexp.MustCompile(`^[0-9A-Za-z.+-]+$`)
	for _, in := range inputs {
		if !tokenRE.MatchString(in) {
			t.Errorf("the input %q is not a token: render_version would be ambiguous", in)
		}
	}
	want := []string{
		"musiclib-render/" + RendererRevision,
		"names/" + names.AlgorithmVersion,
		GoVersion,
		"ffmpeg/" + media.PinnedVersion,
		"musiclib-tags/" + media.PinnedTagsVersion,
		"taglib/" + media.PinnedTagLibVersion,
	}
	if got := strings.Fields(Version); !slices.Equal(got, want) {
		t.Fatalf("render_version %q is not made of its inputs %q", Version, want)
	}
	// The value alone cannot show an input written as a literal ("names/1"
	// instead of names.AlgorithmVersion), which would stop following it:
	// the constant's expression must name every input.
	f, err := parser.ParseFile(token.NewFileSet(), "version.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "Version" {
			return true
		}
		ast.Inspect(spec.Values[0], func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				refs = append(refs, x.X.(*ast.Ident).Name+"."+x.Sel.Name)
				return false
			case *ast.Ident:
				refs = append(refs, x.Name)
			case *ast.BasicLit:
				if strings.ContainsAny(x.Value, "0123456789") {
					t.Errorf("the Version expression has a literal with a digit: %s", x.Value)
				}
			}
			return true
		})
		return false
	})
	for _, in := range []string{"RendererRevision", "names.AlgorithmVersion", "GoVersion", "media.PinnedVersion",
		"media.PinnedTagsVersion", "media.PinnedTagLibVersion"} {
		if !slices.Contains(refs, in) {
			t.Errorf("the Version expression does not reference %s (it references %q)", in, refs)
		}
	}
}

// The value is pinned: a change of any pinned version shows up here and in
// review, together with its consequence (§11.1 step 6: every album renders
// again at the next boot).
func TestVersionGolden(t *testing.T) {
	const want = "musiclib-render/2 names/1 go1.25.14 ffmpeg/8.1.3-musiclib1 musiclib-tags/3 taglib/2.3.2-musiclib1"
	if Version != want {
		t.Fatalf("render_version is %q, pinned %q: update the pin knowing that every album will render again", Version, want)
	}
}

// The binary is built with the toolchain render_version names (the
// Dockerfile's GO_IMAGE): a Go bump fails here until GoVersion follows it.
func TestGoVersionPinned(t *testing.T) {
	if runtime.Version() != GoVersion {
		t.Fatalf("built with %s, render_version names %s: bump GoVersion with the toolchain", runtime.Version(), GoVersion)
	}
}

// CheckTools ties the tools verified at boot to render_version.
func TestCheckTools(t *testing.T) {
	tools, err := media.NewTools(context.Background(), media.NewRunner(1), media.FFmpegPath, media.FFprobePath, media.TagsPath)
	if err != nil {
		t.Fatalf("NewTools: %v (run the tests in Docker, docs/docker.md)", err)
	}
	if err := CheckTools(tools.Versions()); err != nil {
		t.Fatalf("the installed tools: %v", err)
	}
	for name, edit := range map[string]func(*media.Versions){
		"ffmpeg":        func(v *media.Versions) { v.FFmpeg = "8.1.4-musiclib1" },
		"ffprobe":       func(v *media.Versions) { v.FFprobe = "" },
		"musiclib-tags": func(v *media.Versions) { v.Tags = "4" },
		"TagLib":        func(v *media.Versions) { v.TagLib = "2.3.2-musiclib2" },
	} {
		v := tools.Versions()
		edit(&v)
		if err := CheckTools(v); Code(err) != CodeVersionMismatch || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// pinnedBinaries are the sha256 of the tools in the images (amd64; N-073,
// N-083), which render_version names by their versions only. A rebuild that
// changes their bytes without a version bump fails here (NOTES.md N-130):
// the change must bump the tool's version, and so render_version, unless it
// is shown not to change any output, in which case only the pin changes.
var pinnedBinaries = map[string]string{
	media.FFmpegPath:  "3d67d1c2fc18f34ca7bb5cbb08becef864994047a263d97f35117f42da50be10",
	media.FFprobePath: "3f315e9f2ae071d5eceb147b201c9ae817dcc22cd974334e4f7ec0c230155c16",
	media.TagsPath:    "590e6c750c43c568f3f5fc1978c2e5ea8fce3c94cbb422251e4aac36812b14f4",
}

func TestToolBinariesPinned(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skipf("the pins are of the amd64 images, this is %s", runtime.GOARCH)
	}
	for path, want := range pinnedBinaries {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("%v (run the tests in Docker, docs/docker.md)", err)
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			t.Errorf("%s has sha256 %s, pinned %s: its bytes changed without a version bump (N-130)", path, got, want)
		}
	}
}

// rendererDigests pins the renderer's own output for each RendererRevision:
// the SHA-256 of rendererTranscript.
var rendererDigests = map[string]string{
	"1": "01d38d79f5433acd1be4a0a21cb67a1b7520ecd0b2999949194981c60b006446",
	"2": "698ef861981a333d3fe0505697ba60c626621b103fbe9987e7d67f42a41eaa71",
}

// TestRendererRevisionPinned fails when the planner's layout or tag mapping,
// or the receipt's encoding, changes: such a change can change a byte of an
// album directory, so it needs a new RendererRevision (and so a new
// render_version, and every album renders again).
func TestRendererRevisionPinned(t *testing.T) {
	want, ok := rendererDigests[RendererRevision]
	if !ok {
		t.Fatalf("RendererRevision %q has no pinned digest", RendererRevision)
	}
	h := sha256.New()
	rendererTranscript(t, h)
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		t.Fatalf("the renderer's output changed (digest %s, pinned %q for RendererRevision %q): bump RendererRevision "+
			"and pin the new digest", got, want, RendererRevision)
	}
}

// rendererTranscript writes the plans of reference snapshots and a
// reference receipt to w.
func rendererTranscript(t *testing.T, w io.Writer) {
	t.Helper()
	multi := kindOfBlue()
	multi.Tracks = []jobs.SnapshotTrack{track(1, 1, "a"), track(1, 9, "b"), track(2, 10, "c"), track(3, 100, "d")}
	multi.Tracks[2].Artist, multi.Tracks[2].Genre = textOf("Guest"), textOf("")
	multi.Tracks[3].Genre = textOf("Other")
	multi.Tracks[3].Lyrics = jobs.SnapshotBlob{Hash: hashOf("l"), Size: 3}
	multi.Album.Compilation, multi.Album.Year = true, 999
	multi.Cover.Format = "png"
	multi.Tracks[1].Blob.Format = "mp3" // since RendererRevision 2
	bare := kindOfBlue()
	bare.Cover, bare.Album.Genre, bare.Album.Year, bare.Attachments = jobs.SnapshotBlob{}, jobs.Text{}, 0, nil
	long := kindOfBlue()
	long.Tracks[0].Title = strings.Repeat("Long title ", 30)
	long.Artist.Name = strings.Repeat("A/B ", 60)
	for _, s := range []*jobs.RenderSnapshot{kindOfBlue(), multi, bare, long} {
		p := mustPlan(t, s)
		fmt.Fprintf(w, "dir %q %q\n", p.Dir.Path, p.Dir.Key)
		if p.Cover != nil {
			fmt.Fprintf(w, "cover %q %s %s %s\n", p.Cover.Path, p.Cover.Blob.Hash, p.Cover.Format, p.Cover.MIME)
		}
		for _, tr := range p.Tracks {
			fmt.Fprintf(w, "track %q %s %s %+v\n", tr.Path, tr.Blob.Hash, tr.Format, tr.Tags)
		}
		for _, c := range p.Copies {
			fmt.Fprintf(w, "copy %q %s\n", c.Path, c.Blob.Hash)
		}
		fmt.Fprintf(w, "files %q\n", p.Files())
	}
	r := goldenReceipt()
	r.AlbumID, r.BuildID = uuid.MustParse("00000000-0000-7000-8000-000000000001"), uuid.MustParse("00000000-0000-7000-8000-000000000002")
	enc, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, "receipt %s\n", enc)
}
