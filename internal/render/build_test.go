package render

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
)

// setFailpoint installs fn for the test. Builds in these tests are never
// parallel: the hook is package state.
func setFailpoint(t *testing.T, fn func(point, path string, f *os.File) error) {
	t.Helper()
	failpointHook = fn
	t.Cleanup(func() { failpointHook = nil })
}

// kindOfBlueAlbum is the §1.1 example over real blobs: two tracks with old
// tags, aliases, sort keys, unmanaged fields and an embedded picture, an
// LRC, a JPEG cover, nested and Unicode attachments.
func kindOfBlueAlbum(t *testing.T, e *env) *album {
	a := e.album("Miles Davis", "Kind of Blue")
	t1 := a.track(1, 1, "So What", song{
		tags: []string{"TITLE=Old title", "ARTIST=Old artist", "ALBUM ARTIST=alias", "TITLESORT=sort", "TRACKNUMBER=7/9",
			"COMMENT=keep me", "COMMENT=and me", "REPLAYGAIN_TRACK_GAIN=-6.50 dB", "COMPOSER=Miles", "COMPOSERSORT=Davis"},
		pictures: []flacBlock{picture(t, 3, pngImage(t, 8, 8, 9)), picture(t, 0, jpegImage(t, 4, 4, 2))},
	})
	a.lyrics(t1, "[00:01.00]So what\n")
	t2 := a.track(1, 2, "Freddie Freeloader", song{src: sine(660, 0.3), tags: []string{"COMMENT=second", "GENRE=Old"}})
	t2.Artist, t2.Genre = textOf("Wynton Kelly"), textOf("")
	a.cover(jpegImage(t, 32, 24, 1), catalog.FormatJPEG)
	a.attach("booklet.pdf", []byte("%PDF-1.4 not really"))
	a.attach("Scans/Ünïcödé/front:1.jpg", jpegImage(t, 16, 16, 3))
	a.attach("rip.log", []byte("EAC log\n"))
	return a
}

// §9.1 steps 4–8 on a normal album: exactly the planned files, the copies
// byte for byte, every track with the expected tags, its unmanaged fields
// and its audio, the receipt of §9.2, and nothing outside work/render.
func TestBuildAlbum(t *testing.T) {
	e := newEnv(t)
	a := kindOfBlueAlbum(t, e)
	p := a.plan()
	res := e.mustBuild(p)

	if res.BuildID.Version() != 7 || res.AlbumID != a.s.Album.ID || res.AlbumRevision != 2 || res.RenderVersion != Version ||
		res.Removal || res.Staging != "render/"+res.BuildID.String()+"/album" {
		t.Fatalf("result %+v", res)
	}
	files, names, dirs := e.staged(res)
	want := append(p.Files(), ReceiptName)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("staged %q\nwant   %q", names, want)
	}
	slices.Sort(dirs)
	if wantDirs := []string{"Extras", "Extras/Scans", "Extras/Scans/Ünïcödé"}; !slices.Equal(dirs, wantDirs) {
		t.Fatalf("directories %q, want %q", dirs, wantDirs)
	}

	// §9.2: the receipt lists every other file with its real size and hash.
	r, err := ParseReceipt(files[ReceiptName])
	if err != nil {
		t.Fatal(err)
	}
	if ReceiptHash(files[ReceiptName]) != res.ReceiptHash || r.BuildID != res.BuildID || r.AlbumID != a.s.Album.ID ||
		r.AlbumRevision != 2 || r.RenderVersion != Version || len(r.Files) != len(p.Files()) {
		t.Fatalf("receipt %v, hash %s", r, res.ReceiptHash)
	}
	for _, f := range r.Files {
		if b := files[f.RelativePath]; int64(len(b)) != f.Size || sha(b) != f.SHA256 {
			t.Errorf("receipt entry %+v does not describe the file", f)
		}
	}

	// §9.1 step 7: byte-for-byte copies.
	if !bytes.Equal(files["cover.jpg"], e.blob(a.s.Cover.Hash)) {
		t.Error("cover.jpg is not the cover blob")
	}
	for _, c := range p.Copies {
		if !bytes.Equal(files[c.Path], e.blob(c.Blob.Hash)) {
			t.Errorf("%s is not its blob", c.Path)
		}
	}
	// §9.1 step 6: the managed tags as planned, the unmanaged ones kept, the
	// cover as the one picture, and the same audio as the original.
	cover := &media.ExpectedCover{MIME: "image/jpeg", Size: a.s.Cover.Size, SHA256: a.s.Cover.Hash}
	for _, tr := range p.Tracks {
		in, out := e.blob(tr.Blob.Hash), files[tr.Path]
		before, after := e.inspect("in.flac", in), e.inspect("out.flac", out)
		if err := media.VerifyTags(tr.Tags, cover, before, after); err != nil {
			t.Errorf("%s: %v", tr.Path, err)
		}
		if e.digest("in.flac", in) != e.digest("out.flac", out) {
			t.Errorf("%s: the audio changed", tr.Path)
		}
	}
	so := e.inspect("so.flac", files["01 - So What.flac"])
	if !slices.Equal(so.Managed.Title, []string{"So What"}) || !slices.Equal(so.Managed.Track, []string{"1"}) ||
		!slices.Equal(so.Managed.TrackTotal, []string{"2"}) || !slices.Equal(so.Managed.Date, []string{"1959"}) {
		t.Errorf("managed tags %+v", so.Managed)
	}
	if kv := find(so.Unmanaged, "vorbis:COMMENT"); !slices.Equal(kv, []string{"keep me", "and me"}) {
		t.Errorf("COMMENT %q", kv)
	}
	if find(so.Unmanaged, "vorbis:COMPOSERSORT") == nil || find(so.Unmanaged, "vorbis:TITLESORT") != nil {
		t.Errorf("sort keys %+v", so.Unmanaged)
	}
	fred := e.inspect("fred.flac", files["02 - Freddie Freeloader.flac"])
	if !slices.Equal(fred.Managed.Artist, []string{"Wynton Kelly"}) || !slices.Equal(fred.Managed.AlbumArtist, []string{"Miles Davis"}) ||
		len(fred.Managed.Genre) != 0 {
		t.Errorf("overrides %+v", fred.Managed)
	}
	// The build's parent chain exists and the builder never needs it again.
	if err := e.b.Discard(context.Background(), res.BuildID); err != nil {
		t.Fatal(err)
	}
	e.checkNoBuilds()
	if err := e.b.Discard(context.Background(), res.BuildID); err != nil {
		t.Fatalf("a second Discard: %v", err)
	}
}

func find(kvs []media.KeyValues, key string) []string {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Values
		}
	}
	return nil
}

// Without a cover the output has no picture at all (§8.2), and a multi-disc
// album goes into its disc directories.
func TestBuildWithoutCoverMultiDisc(t *testing.T) {
	e := newEnv(t)
	a := e.album("Artist", "Box")
	a.track(1, 1, "One", song{tags: []string{"TITLE=x"}, pictures: []flacBlock{picture(t, 3, pngImage(t, 4, 4, 1))}})
	lrc := a.track(2, 1, "Two", song{src: sine(550, 0.2)})
	a.lyrics(lrc, "[00:00.50]two\n")
	p := a.plan()
	res := e.mustBuild(p)
	files, names, dirs := e.staged(res)
	if want := []string{".musiclib.json", "Disc 1/01 - One.flac", "Disc 2/01 - Two.flac", "Disc 2/01 - Two.lrc"}; !slices.Equal(names, want) {
		t.Fatalf("staged %q", names)
	}
	slices.Sort(dirs)
	if !slices.Equal(dirs, []string{"Disc 1", "Disc 2"}) {
		t.Fatalf("dirs %q", dirs)
	}
	for _, tr := range p.Tracks {
		out := e.inspect("out.flac", files[tr.Path])
		if len(out.Pictures) != 0 {
			t.Errorf("%s keeps %d pictures", tr.Path, len(out.Pictures))
		}
		if err := media.VerifyTags(tr.Tags, nil, e.inspect("in.flac", e.blob(tr.Blob.Hash)), out); err != nil {
			t.Error(err)
		}
	}
}

// N-090, N-128: a FLAC with a trailing ID3v1 and one with a leading ID3v2
// build; the output has neither, and the same audio.
func TestBuildID3(t *testing.T) {
	e := newEnv(t)
	a := e.album("Artist", "ID3")
	a.track(1, 1, "Trailing", song{tags: []string{"TITLE=t"}, id3v1: true})
	a.track(1, 2, "Leading", song{src: sine(330, 0.2), tags: []string{"TITLE=l"}, id3v2: true})
	a.track(1, 3, "Both", song{src: sine(770, 0.2), id3v1: true, id3v2: true})
	p := a.plan()
	res := e.mustBuild(p)
	files, _, _ := e.staged(res)
	for _, tr := range p.Tracks {
		in, out := e.blob(tr.Blob.Hash), files[tr.Path]
		if bytes.HasPrefix(out, []byte("ID3")) || bytes.Equal(out[len(out)-128:][:3], []byte("TAG")) {
			t.Errorf("%s keeps an ID3 tag", tr.Path)
		}
		if o := e.inspect("out.flac", out); len(o.Opaque) != 0 {
			t.Errorf("%s: opaque fields %+v", tr.Path, o.Opaque)
		}
		if e.digest("in.flac", in) != e.digest("out.flac", out) {
			t.Errorf("%s: the audio changed", tr.Path)
		}
	}
}

// §9.2: the music files are deterministic for the same input and
// render_version; only the receipt's build_id changes.
func TestBuildDeterministic(t *testing.T) {
	e := newEnv(t)
	p := kindOfBlueAlbum(t, e).plan()
	r1, r2 := e.mustBuild(p), e.mustBuild(p)
	if r1.BuildID == r2.BuildID || r1.ReceiptHash == r2.ReceiptHash {
		t.Fatalf("two builds share an id or a receipt: %+v %+v", r1, r2)
	}
	compareBuilds(t, e, r1, e, r2)
}

// compareBuilds requires the same files, byte for byte, and receipts that
// differ only by build_id.
func compareBuilds(t *testing.T, e1 *env, r1 Result, e2 *env, r2 Result) {
	t.Helper()
	f1, n1, _ := e1.staged(r1)
	f2, n2, _ := e2.staged(r2)
	if !slices.Equal(n1, n2) {
		t.Fatalf("different files: %q and %q", n1, n2)
	}
	for _, name := range n1 {
		if name != ReceiptName && !bytes.Equal(f1[name], f2[name]) {
			t.Errorf("%s differs between the builds", name)
		}
	}
	a, err := ParseReceipt(f1[ReceiptName])
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseReceipt(f2[ReceiptName])
	if err != nil {
		t.Fatal(err)
	}
	b.BuildID = a.BuildID
	ea, _ := a.Encode()
	eb, _ := b.Encode()
	if !bytes.Equal(ea, eb) {
		t.Fatalf("the receipts differ beyond build_id:\n%s\n%s", ea, eb)
	}
}

// §9.1 step 5, §12.2 "Blob esistente corrotto": a corrupt original stops
// the build, whichever file it is, with nothing left behind.
func TestBuildCorruptOriginal(t *testing.T) {
	for _, which := range []string{"track", "cover", "lyrics", "attachment", "missing"} {
		t.Run(which, func(t *testing.T) {
			e := newEnv(t)
			a := kindOfBlueAlbum(t, e)
			hash := map[string]string{"track": a.s.Tracks[1].Blob.Hash, "cover": a.s.Cover.Hash, "lyrics": a.s.Tracks[0].Lyrics.Hash,
				"attachment": a.s.Attachments[2].Blob.Hash, "missing": a.s.Attachments[0].Blob.Hash}[which]
			path := filepath.Join(e.data, "originals", hash[:2], hash[2:4], hash)
			if which == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				b := e.blob(hash)
				b[len(b)/2] ^= 0x40 // same size, other bytes
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, b, 0o444); err != nil {
					t.Fatal(err)
				}
			}
			_, err := e.build(a.plan())
			want := blobstore.CodeCorrupt
			if which == "missing" {
				want = blobstore.CodeNotFound
			}
			if Code(err) != want {
				t.Fatalf("error %v, want %s", err, want)
			}
			if which != "missing" && !strings.Contains(err.Error(), hash) {
				t.Errorf("the error does not name the blob: %v", err)
			}
		})
	}
}

// §12.2 "Tag writer altera i campioni": a write that changes the samples,
// invisible to every tag check, fails on the digest comparison alone.
func TestBuildTagWriterAltersSamples(t *testing.T) {
	e := newEnv(t)
	a := e.album("Artist", "Altered")
	a.track(1, 1, "a", song{tags: []string{"COMMENT=x"}})
	other := baseFLAC(t, sine(441, 0.3))
	setFailpoint(t, func(point, path string, f *os.File) error {
		if point != "tags-written" {
			return nil
		}
		// Keep the written metadata (so STREAMINFO and every tag stay), put
		// the frames of another sine behind it.
		return rewrite(t, f, func(ff flacFile) flacFile { ff.audio = other.audio; return ff })
	})
	_, err := e.build(a.plan())
	wantRenderCode(t, err, CodeAudioChanged)
}

// §12.2 "... o perde un tag non gestito": a write that drops an unmanaged
// field fails the verification of §9.1 step 6.
func TestBuildTagWriterLosesUnmanagedField(t *testing.T) {
	e := newEnv(t)
	a := e.album("Artist", "Lossy")
	a.track(1, 1, "a", song{tags: []string{"COMMENT=precious", "REPLAYGAIN_TRACK_GAIN=1 dB"}})
	setFailpoint(t, func(point, path string, f *os.File) error {
		if point != "tags-written" {
			return nil
		}
		return rewrite(t, f, func(ff flacFile) flacFile {
			for i, blk := range ff.blocks {
				if blk.typ == blockVorbis {
					_, entries := comments(t, blk)
					entries = slices.DeleteFunc(entries, func(s string) bool { return strings.HasPrefix(s, "COMMENT=") })
					ff.blocks[i] = vorbis(entries...)
				}
			}
			return ff
		})
	})
	_, err := e.build(a.plan())
	if Code(err) != media.CodeTagsVerification || !strings.Contains(err.Error(), "COMMENT") {
		t.Fatalf("error %v, want %s naming COMMENT", err, media.CodeTagsVerification)
	}
}

// rewrite replaces the content of a staged FLAC in place.
func rewrite(t *testing.T, f *os.File, edit func(flacFile) flacFile) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	b := make([]byte, st.Size())
	if _, err := f.ReadAt(b, 0); err != nil {
		return err
	}
	out := edit(parseFLAC(t, b)).bytes()
	if _, err := f.WriteAt(out, 0); err != nil {
		return err
	}
	return f.Truncate(int64(len(out)))
}

// §12.2 "Disco pieno durante build": ENOSPC at any write leaves the old
// output and the originals intact (checked by env.build), and no staging.
func TestBuildNoSpace(t *testing.T) {
	for _, at := range []string{"write:01 - So What.flac", "write:Extras/Scans/Ünïcödé/front_1.jpg", "write:" + ReceiptName,
		"fsync-file:02 - Freddie Freeloader.flac", "fsync-dir:"} {
		t.Run(at, func(t *testing.T) {
			e := newEnv(t)
			p := kindOfBlueAlbum(t, e).plan()
			point, path, _ := strings.Cut(at, ":")
			hit := false
			setFailpoint(t, func(pt, pa string, f *os.File) error {
				if pt == point && (pa == path || point == "fsync-dir" && strings.HasSuffix(pa, "/album")) {
					hit = true
					return &os.PathError{Op: "write", Path: pa, Err: syscall.ENOSPC}
				}
				return nil
			})
			_, err := e.build(p)
			if !hit {
				t.Fatal("the failpoint was not reached")
			}
			wantRenderCode(t, err, CodeInsufficientSpace)
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("the cause is lost: %v", err)
			}
		})
	}
}

// §11.2: the estimate plus 1 GiB must fit before anything is written.
func TestBuildSpaceCheck(t *testing.T) {
	e := newEnv(t)
	p := kindOfBlueAlbum(t, e).plan()
	p.Tracks[0].Blob.Size = 1 << 55
	_, err := e.build(p)
	wantRenderCode(t, err, CodeInsufficientSpace)
	// Every file, each track grown by the cover and tagSlack, the receipt.
	a := kindOfBlueAlbum(t, e)
	want := a.s.Cover.Size + a.s.Tracks[0].Lyrics.Size
	for _, tr := range a.s.Tracks {
		want += tr.Blob.Size + a.s.Cover.Size + tagSlack
	}
	for _, at := range a.s.Attachments {
		want += at.Blob.Size
	}
	want += int64(2+4+2) * receiptSlack // 2 tracks, 4 copies, cover and receipt
	if n := estimate(a.plan()); n != want {
		t.Fatalf("estimate %d, want %d", n, want)
	}
}

// §11.2 and N-114: a build reserves its estimate in the process budget and
// the Result holds it until the caller releases it (the staging is
// installed or discarded); a failed build releases it at once; another
// job's reservation counts against the free space.
func TestBuildReservesSpace(t *testing.T) {
	e := newEnv(t)
	a := kindOfBlueAlbum(t, e)
	p := a.plan()
	res := e.mustBuild(p)
	if got := e.b.budget.Reserved(); got != estimate(p) || res.Space == nil {
		t.Fatalf("reserved %d, want the estimate %d", got, estimate(p))
	}
	if res.Dir != p.Dir.Path || res.Dir == "" {
		t.Fatalf("Result.Dir %q, want %q", res.Dir, p.Dir.Path)
	}
	res.Space.Release()
	res.Space.Release() // idempotent
	if err := e.b.Discard(context.Background(), res.BuildID); err != nil {
		t.Fatal(err)
	}
	if got := e.b.budget.Reserved(); got != 0 {
		t.Fatalf("reserved %d after the release", got)
	}

	// A failure after the reservation releases it.
	setFailpoint(t, func(pt, pa string, f *os.File) error {
		if pt == "write" && pa == ReceiptName {
			return &os.PathError{Op: "write", Path: pa, Err: syscall.EIO}
		}
		return nil
	})
	if _, err := e.build(p); err == nil {
		t.Fatal("the failpoint did not fail the build")
	}
	setFailpoint(t, nil)
	if got := e.b.budget.Reserved(); got != 0 {
		t.Fatalf("reserved %d after a failed build", got)
	}

	// Another job holds all the free space but the margin and 1 MiB: the
	// build is refused before anything is written, and passes once it is
	// released.
	fs, err := e.work.StatFS()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := e.b.budget.Reserve(fs.FreeBytes, fs.FreeBytes-jobs.SpaceMargin-(1<<20))
	if other == nil {
		t.Skipf("only %d bytes free on the test volume", fs.FreeBytes)
	}
	_, err = e.build(p)
	wantRenderCode(t, err, CodeInsufficientSpace)
	other.Release()
	e.mustBuild(p).Space.Release()
}

// §9.1 step 8: every file fsynced once, before any directory; every new
// directory after its subdirectories; then the album directory, and its
// parents render/<build_id>, render and work, the staging's parent
// included.
func TestBuildFsyncOrder(t *testing.T) {
	e := newEnv(t)
	p := kindOfBlueAlbum(t, e).plan()
	var events []string
	setFailpoint(t, func(point, path string, f *os.File) error {
		if strings.HasPrefix(point, "fsync") {
			events = append(events, point+":"+path)
		}
		return nil
	})
	res := e.mustBuild(p)
	_, _, dirs := e.staged(res)
	var fileSyncs, dirSyncs []string
	for i, ev := range events {
		point, path, _ := strings.Cut(ev, ":")
		if point == "fsync-file" {
			if len(dirSyncs) > 0 {
				t.Fatalf("file %s fsynced after a directory (event %d)", path, i)
			}
			fileSyncs = append(fileSyncs, path)
		} else {
			dirSyncs = append(dirSyncs, path)
		}
	}
	wantFiles := append(p.Files(), ReceiptName)
	if fileSyncs[len(fileSyncs)-1] != ReceiptName {
		t.Errorf("the receipt is not the last file: %q", fileSyncs)
	}
	slices.Sort(fileSyncs)
	slices.Sort(wantFiles)
	if !slices.Equal(fileSyncs, wantFiles) {
		t.Fatalf("files fsynced %q\nwant %q", fileSyncs, wantFiles)
	}
	stage := StagingDir(res.BuildID)
	wantDirs := []string{stage, "render/" + res.BuildID.String(), "render", ""}
	for _, d := range dirs {
		wantDirs = append(wantDirs, stage+"/"+d)
	}
	got := slices.Clone(dirSyncs)
	slices.Sort(got)
	slices.Sort(wantDirs)
	if !slices.Equal(got, wantDirs) {
		t.Fatalf("directories fsynced %q\nwant %q", got, wantDirs)
	}
	if tail := dirSyncs[len(dirSyncs)-4:]; !slices.Equal(tail, []string{stage, "render/" + res.BuildID.String(), "render", ""}) {
		t.Fatalf("the chain ends with %q", tail)
	}
	for i, a := range dirSyncs {
		for _, b := range dirSyncs[i+1:] {
			if strings.HasPrefix(b, a+"/") || a == "" && b != "" {
				t.Fatalf("%q fsynced before its subdirectory %q", a, b)
			}
		}
	}
}

// §8.5, §11.1: a cancellation during a build kills the running tool (the
// Runner's process group) and removes the staging.
func TestBuildCancel(t *testing.T) {
	e := newEnv(t)
	a := e.album("Artist", "Long")
	a.track(1, 1, "short", song{})
	a.track(1, 2, "long", song{src: sine(440, 300)})
	p := a.plan()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	killed := make(chan int, 1)
	go func() {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			if pid := childTool(t); pid > 0 {
				killed <- pid
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
		close(killed)
	}()
	lib, orig := e.tree("library"), e.tree("originals")
	_, err := e.b.Build(ctx, p)
	pid, ok := <-killed
	if !ok {
		t.Fatal("no tool was seen running")
	}
	if c := Code(err); c != media.CodeCanceled && c != CodeCanceled {
		t.Fatalf("error %v, want a cancellation", err)
	}
	if _, serr := os.Stat("/proc/" + strconv.Itoa(pid)); !os.IsNotExist(serr) {
		t.Fatalf("tool %d still exists after Build returned", pid)
	}
	e.checkUntouched(lib, orig)
	e.checkNoBuilds()
}

// childTool returns the pid of a child of this process running ffmpeg or
// musiclib-tags, or 0.
func childTool(t *testing.T) int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	self := os.Getpid()
	for _, ent := range ents {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + ent.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || close < open {
			continue
		}
		comm, rest := s[open+1:close], strings.Fields(s[close+1:])
		if len(rest) < 2 {
			continue
		}
		if ppid, _ := strconv.Atoi(rest[1]); ppid == self && (comm == "ffmpeg" || comm == "musiclib-tags") {
			return pid
		}
	}
	return 0
}

// §9.1 step 3: a removal builds nothing, but has a build id (§9.3 names the
// retired directory after it).
func TestBuildRemoval(t *testing.T) {
	e := newEnv(t)
	a := kindOfBlueAlbum(t, e)
	a.s.Album.Deleted = true
	res, err := e.build(a.plan())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Removal || res.BuildID == uuid.Nil || res.Staging != "" || res.ReceiptHash != "" || res.AlbumRevision != 2 {
		t.Fatalf("result %+v", res)
	}
	e.checkNoBuilds()
	if err := e.b.Discard(context.Background(), res.BuildID); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRefusals(t *testing.T) {
	e := newEnv(t)
	p := kindOfBlueAlbum(t, e).plan()
	other := p
	other.RenderVersion = "musiclib-render/0"
	if _, err := e.build(other); Code(err) != CodeVersionMismatch {
		t.Errorf("another render_version: %v", err)
	}
	empty := p
	empty.Tracks = nil
	if _, err := e.build(empty); Code(err) != CodeInvalidArgument {
		t.Errorf("no tracks: %v", err)
	}
	if err := e.b.Discard(context.Background(), uuid.Nil); Code(err) != CodeInvalidArgument {
		t.Errorf("Discard without id: %v", err)
	}
	if _, err := New(Config{}); Code(err) != CodeInvalidArgument {
		t.Errorf("New without dependencies: %v", err)
	}
	e.checkNoBuilds()
}

// §9.1 step 7 "verificata": a copy whose written bytes differ from what was
// read (here, a stray byte written before the data) is caught by the read
// back, and the build fails.
func TestBuildCopyReadBack(t *testing.T) {
	for _, path := range []string{"cover.jpg", "01 - So What.flac", "Extras/rip.log"} {
		t.Run(path, func(t *testing.T) {
			e := newEnv(t)
			p := kindOfBlueAlbum(t, e).plan()
			done := false
			setFailpoint(t, func(point, pa string, f *os.File) error {
				if point == "write" && pa == path && !done {
					done = true
					_, err := f.Write([]byte{0})
					return err
				}
				return nil
			})
			_, err := e.build(p)
			wantRenderCode(t, err, CodeCopyMismatch)
		})
	}
}

// N-131, N-133: an attachment at §5.2's limits below Extras/ (16 levels;
// 1,024 bytes) is 17 levels and 1,031 bytes in the album. It plans, builds,
// and its receipt round-trips through ParseReceipt.
func TestBuildDeepAttachment(t *testing.T) {
	e := newEnv(t)
	a := e.album("Artist", "Deep")
	a.track(1, 1, "a", song{})
	deep := strings.Repeat("d/", 15) + "f.txt"
	long := strings.Repeat(strings.Repeat("x", 169)+"/", 6) + strings.Repeat("y", 1024-6*170)
	a.attach(deep, []byte("sixteen levels"))
	a.attach(long, []byte("1,024 bytes"))
	p := a.plan()
	res := e.mustBuild(p)
	files, names, _ := e.staged(res)
	for _, rel := range []string{"Extras/" + deep, "Extras/" + long} {
		if !slices.Contains(names, rel) {
			t.Fatalf("%q not built", rel)
		}
	}
	r, err := ParseReceipt(files[ReceiptName])
	if err != nil {
		t.Fatalf("ParseReceipt: %v", err)
	}
	var listed []string
	for _, f := range r.Files {
		listed = append(listed, f.RelativePath)
	}
	if !slices.Contains(listed, "Extras/"+deep) || !slices.Contains(listed, "Extras/"+long) {
		t.Fatalf("receipt %q", listed)
	}
}
