package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"musiclib/internal/fsops"
)

// The media tests run the real pinned ffprobe and ffmpeg (DESIGN.md §12.1),
// installed in the test and dev images at FFmpegPath and FFprobePath. The
// fixtures are generated at test time from lavfi sources (sine, noise with a
// fixed seed, silence) and with the pinned LAME for MP3: nothing
// copyrighted, nothing committed. The tools are required: a run without
// them fails instead of skipping.

// lamePath is the test-only MP3 encoder of the toolchain images.
const lamePath = "/usr/local/bin/lame"

// newTools returns Tools on the real binaries, with a Runner of 4 slots.
func newTools(t testing.TB) *Tools {
	t.Helper()
	tools, err := NewTools(t.Context(), NewRunner(4), FFmpegPath, FFprobePath)
	if err != nil {
		t.Fatalf("NewTools: %v (the pinned ffmpeg must be installed: run the tests in Docker, docs/docker.md)", err)
	}
	return tools
}

// gen runs the real ffmpeg to generate a fixture in dir and returns its
// path. It is test code: the application runs tools only through Runner.
func gen(t testing.TB, dir, name string, args ...string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	full := append([]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}, args...)
	full = append(full, out)
	run(t, FFmpegPath, full...)
	return out
}

// lame encodes the WAV file in to an MP3 in dir with the given options.
func lame(t testing.TB, in, dir, name string, opts ...string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	args := append([]string{"--quiet"}, opts...)
	run(t, lamePath, append(args, in, out)...)
	return out
}

func run(t testing.TB, path string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", path, strings.Join(args, " "), err, out)
	}
}

// Sources of the fixtures. Everything is deterministic: lavfi's sine and
// the fixed-seed noise generate the same samples on every run.
const (
	sine3s  = "sine=frequency=440:sample_rate=44100:duration=3"
	noise3s = "anoisesrc=color=pink:sample_rate=44100:seed=7:duration=3"
)

// lavfi returns the ffmpeg input arguments of a lavfi source.
func lavfi(src string) []string { return []string{"-f", "lavfi", "-i", src} }

// wav16 writes a 16-bit stereo WAV of the source, the input of lame and of
// the sample-level tests.
func wav16(t testing.TB, dir, name, src string) string {
	t.Helper()
	return gen(t, dir, name, append(lavfi(src), "-ac", "2", "-c:a", "pcm_s16le")...)
}

// open opens a fixture through internal/fsops, as the application will: a
// confined, regular-file-only descriptor.
func open(t testing.TB, path string) *os.File {
	t.Helper()
	root, err := fsops.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	f, err := root.Open(filepath.Base(path))
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Base(path), err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

// probe classifies a fixture.
func probe(t testing.TB, tools *Tools, path string) ProbeResult {
	t.Helper()
	p, err := tools.Probe(t.Context(), open(t, path))
	if err != nil {
		t.Fatalf("Probe(%s): %v", filepath.Base(path), err)
	}
	return p
}

// digest decodes a fixture that must be supported and decodable.
func digest(t testing.TB, tools *Tools, path string) Digest {
	t.Helper()
	d, err := tools.AudioDigest(t.Context(), open(t, path))
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			t.Fatalf("AudioDigest(%s): %v\nstderr: %s", filepath.Base(path), err, e.Stderr)
		}
		t.Fatalf("AudioDigest(%s): %v", filepath.Base(path), err)
	}
	return d
}

// wantCode checks the media code of an error.
func wantCode(t testing.TB, err error, code string) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error %v, want code %s", err, code)
	}
	return e
}

func readFile(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t testing.TB, path string, b []byte) string {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// garbage returns n deterministic pseudo-random bytes.
func garbage(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// wavData returns the offset and length of the "data" chunk of a WAV file.
func wavData(t testing.TB, b []byte) (int, int) {
	t.Helper()
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatal("not a RIFF/WAVE file")
	}
	for off := 12; off+8 <= len(b); {
		id, size := string(b[off:off+4]), int(binary.LittleEndian.Uint32(b[off+4:off+8]))
		if id == "data" {
			return off + 8, min(size, len(b)-off-8)
		}
		off += 8 + size + size%2
	}
	t.Fatal("no data chunk")
	return 0, 0
}

// ---------------------------------------------------------------------------
// Child processes: the test binary itself, as a tool run by the Runner or as
// a parent process that the tests kill. Tools get an empty environment, so
// the mode is passed on the command line after "--".

const helperMarker = "media-helper"

// helperArgs are the arguments that make the test binary run a helper mode.
func helperArgs(mode string, extra ...string) []string {
	return append([]string{"-test.run=^TestHelperProcess$", "-test.count=1", "--", helperMarker, mode}, extra...)
}

// testBinary is the absolute path of the running test binary.
func testBinary(t testing.TB) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestHelperProcess is not a test: it is the body of the child processes,
// and returns at once in a normal run.
func TestHelperProcess(t *testing.T) {
	args := flag.Args()
	if len(args) < 2 || args[0] != helperMarker {
		return
	}
	code := helperMain(args[1], args[2:])
	os.Exit(code)
}

func helperMain(mode string, args []string) int {
	switch mode {
	case "inspect":
		// Describe what the tool inherited: descriptors (with the identity
		// of their target), environment, working directory, process group.
		// A descriptor that survived execve has no FD_CLOEXEC; the ones this
		// helper's own Go runtime opens (since Go 1.25, the cgroup cpu.max
		// file) all have it, so the flag tells the two apart exactly.
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			fmt.Println("error:", err)
			return 1
		}
		for _, e := range entries {
			fd, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			var st unix.Stat_t
			if unix.Fstat(fd, &st) != nil {
				continue // the descriptor of the ReadDir itself, already closed
			}
			flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			if err != nil {
				fmt.Println("error:", err)
				return 1
			}
			if flags&unix.FD_CLOEXEC != 0 {
				continue
			}
			target, _ := os.Readlink("/proc/self/fd/" + e.Name())
			fmt.Printf("fd %d %d:%d %s\n", fd, st.Dev, st.Ino, target)
		}
		wd, _ := os.Getwd()
		fmt.Printf("env %d\ncwd %s\npgid %d\npid %d\n", len(os.Environ()), wd, unix.Getpgrp(), os.Getpid())
		return 0
	case "pdeathsig-parent":
		// A parent that starts a long tool through the Runner and prints the
		// tool's pid; the test then kills this process with SIGKILL.
		r := NewRunner(1)
		w := &lineWriter{lines: make(chan string, 4)}
		go func() {
			for line := range w.lines {
				fmt.Println("tool-pid", line)
			}
		}()
		_, err := r.Run(context.Background(), Command{
			Path: "/bin/sh", Args: []string{"-c", "echo $$; exec sleep 300"},
			Stdout: w, Timeout: time.Hour,
		})
		fmt.Println("run returned:", err)
		return 1
	default:
		fmt.Println("error: unknown mode", mode)
		return 2
	}
}

// lineWriter delivers every complete line written to it.
type lineWriter struct {
	mu    sync.Mutex
	buf   []byte
	lines chan string
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, b...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			return len(b), nil
		}
		w.lines <- string(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
}

// ---------------------------------------------------------------------------
// Processes, seen through /proc.

// procState returns the state letter of pid ("R", "S", "Z", ...) and its
// process group, or ok=false if it does not exist.
func procState(pid int) (state string, pgrp int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", 0, false
	}
	// pid (comm) state ppid pgrp ...; comm may contain spaces and parens.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 3 {
		return "", 0, false
	}
	pgrp, _ = strconv.Atoi(f[2])
	return f[0], pgrp, true
}

// alive reports whether pid exists and is not a zombie.
func alive(pid int) bool {
	st, _, ok := procState(pid)
	return ok && st != "Z" && st != "X"
}

// groupMembers returns the live (non-zombie) processes of a process group.
func groupMembers(t testing.TB, pgid int) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if st, pg, ok := procState(pid); ok && pg == pgid && st != "Z" && st != "X" {
			out = append(out, pid)
		}
	}
	return out
}

// waitDead waits until pid is gone or a zombie.
func waitDead(t testing.TB, pid int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for alive(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// openFDs lists the descriptors of this process.
func openFDs(t testing.TB) map[string]string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		target, _ := os.Readlink("/proc/self/fd/" + e.Name())
		out[e.Name()] = target
	}
	return out
}

// readLines reads r line by line into a channel, closed at EOF.
func readLines(r io.Reader) <-chan string {
	ch := make(chan string, 16)
	go func() {
		defer close(ch)
		s := bufio.NewScanner(r)
		for s.Scan() {
			ch <- s.Text()
		}
	}()
	return ch
}
