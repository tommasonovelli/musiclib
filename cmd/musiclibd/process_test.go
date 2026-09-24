package main

import (
	"bufio"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"musiclib/internal/media"
	"musiclib/internal/store/pgtest"
)

// helperEnv makes the test binary run serve() as a child process.
const helperEnv = "MUSICLIB_SERVE_HELPER"

// TestHelperProcess is not a test: it is the body of the server child
// processes, and returns at once in a normal run. It runs serve() exactly as
// main does, with the test's data and import paths instead of /data and
// /import.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) == "" {
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	os.Exit(serve(log, os.Getenv, paths{data: os.Getenv("TEST_DATA"), imports: os.Getenv("TEST_IMPORT"),
		ffmpeg: media.FFmpegPath, ffprobe: media.FFprobePath, tags: media.TagsPath}))
}

// serverProcess is a musiclibd server running as a real child process.
type serverProcess struct {
	cmd    *exec.Cmd
	stderr *syncBuffer
	addr   string
	done   chan struct{} // closed when the child has been waited for
}

func startServerProcess(t *testing.T, dbURL string, p paths, umask int, extraEnv ...string) *serverProcess {
	t.Helper()
	addr := "127.0.0.1:" + closedPort(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), helperEnv+"=1",
		"TEST_DATA="+p.data, "TEST_IMPORT="+p.imports,
		envDatabaseURL+"="+dbURL, envPublicOrigin+"=http://127.0.0.1:8080",
		envHTTPAddr+"="+addr, envWorkers+"=1")
	// Later entries win (os/exec keeps the last value of a duplicate).
	cmd.Env = append(cmd.Env, extraEnv...)
	s := &serverProcess{cmd: cmd, stderr: &syncBuffer{}, addr: addr, done: make(chan struct{})}
	cmd.Stderr = s.stderr
	// The child inherits the umask: start it with a hostile one, which the
	// server must replace with 022 (§11.1, N-020). Tests of this package do
	// not run in parallel, so changing the process umask here is safe.
	old := unix.Umask(umask)
	err := cmd.Start()
	unix.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		// The exit status is read from cmd.ProcessState after done.
		_ = cmd.Wait()
		close(s.done)
	}()
	t.Cleanup(func() {
		select {
		case <-s.done:
		default:
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error(err)
			}
			<-s.done
		}
	})
	return s
}

// wait returns the exit code of the child, waiting for at most 60 s.
func (s *serverProcess) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(60 * time.Second):
		t.Fatalf("the server did not exit within 60s; stderr:\n%s", s.stderr)
	}
	ws, ok := s.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || ws.Signaled() {
		t.Fatalf("the server did not exit normally: %v; stderr:\n%s", s.cmd.ProcessState, s.stderr)
	}
	return ws.ExitStatus()
}

// healthy runs the healthcheck subcommand logic against the child.
func (s *serverProcess) healthy() int {
	return healthcheck(slog.New(slog.DiscardHandler), env(map[string]string{envHTTPAddr: s.addr}))
}

func (s *serverProcess) waitHealthy(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for s.healthy() != exitOK {
		if time.Now().After(deadline) {
			t.Fatalf("the server is not healthy after 30s; stderr:\n%s", s.stderr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A real server process: it becomes healthy, holds the lock against a second
// process, sets umask 022, stops on SIGTERM with exit 0 releasing the lock
// last, and never logs the database password.
func TestServerProcessLifecycle(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	s := startServerProcess(t, dbURL, p, 0o077)

	s.waitHealthy(t)
	if got := processUmask(t, s.cmd.Process.Pid); got != 0o022 {
		t.Fatalf("server umask %#o, want 022", got)
	}

	// Another process cannot take the volume: neither this test process nor
	// a second server, which exits 1 with volume_locked.
	assertLockHeld(t, p.data)
	second := startServerProcess(t, dbURL, p, 0o022)
	if code := second.wait(t); code != exitFailure {
		t.Fatalf("second server exit %d, want %d; stderr:\n%s", code, exitFailure, second.stderr)
	}
	if !strings.Contains(second.stderr.String(), `"code":"volume_locked"`) {
		t.Fatalf("second server stderr:\n%s", second.stderr)
	}
	if got := s.healthy(); got != exitOK {
		t.Fatal("the first server stopped being healthy")
	}

	if err := s.cmd.Process.Signal(unix.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := s.wait(t); code != exitOK {
		t.Fatalf("exit %d after SIGTERM; stderr:\n%s", code, s.stderr)
	}
	assertShutdownOrder(t, s.stderr, true)
	assertLockFree(t, p.data)
	if got := s.healthy(); got != exitFailure {
		t.Fatalf("healthcheck after the exit: %d", got)
	}

	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	if pw, ok := u.User.Password(); ok && len(pw) >= 4 {
		for _, out := range []string{s.stderr.String(), second.stderr.String()} {
			if strings.Contains(out, pw) {
				t.Fatalf("the database password appears in the logs:\n%s", out)
			}
		}
	} else {
		t.Log("the test database URL has no password: the log check is vacuous")
	}
	for _, ev := range s.stderr.events(t) {
		if ev["level"] == "ERROR" {
			t.Fatalf("error logged during a clean run: %v", ev)
		}
	}
}

// SIGINT stops the server like SIGTERM.
func TestServerProcessSIGINT(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	s := startServerProcess(t, dbURL, p, 0o022)
	s.waitHealthy(t)
	if err := s.cmd.Process.Signal(unix.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := s.wait(t); code != exitOK {
		t.Fatalf("exit %d after SIGINT; stderr:\n%s", code, s.stderr)
	}
	assertLockFree(t, p.data)
}

// processUmask reads the Umask line of /proc/<pid>/status (Linux 4.7).
func processUmask(t *testing.T, pid int) int {
	t.Helper()
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Skipf("no /proc status: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Umask:"); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 8, 32)
			if err != nil {
				t.Fatal(err)
			}
			return int(n)
		}
	}
	t.Skip("no Umask line in /proc status")
	return 0
}
