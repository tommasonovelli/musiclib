package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"musiclib/internal/media"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

// The server tests run the real boot on the ext4 TMPDIR of the gate and on
// a real PostgreSQL 17 (DESIGN.md §12.1), with the HTTP server on a
// loopback port.

// syncBuffer is an io.Writer safe for the concurrent writes of a logger and
// the reads of a test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// events parses the JSON log lines.
func (s *syncBuffer) events(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// messages returns the "msg" of every log event, in order.
func (s *syncBuffer) messages(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, ev := range s.events(t) {
		msg, _ := ev["msg"].(string)
		out = append(out, msg)
	}
	return out
}

func (s *syncBuffer) has(t *testing.T, msg string) bool {
	t.Helper()
	for _, m := range s.messages(t) {
		if m == msg {
			return true
		}
	}
	return false
}

// waitLog waits until a log event with the given message appears.
func (s *syncBuffer) waitLog(t *testing.T, msg string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !s.has(t, msg) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q log event within 30s; logs:\n%s", msg, s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// testConfig is a valid configuration on the given database.
func testConfig(dbURL string) Config {
	return Config{
		DatabaseURL:  dbURL,
		PublicOrigin: "http://127.0.0.1:8080",
		HTTPAddr:     "127.0.0.1:0",
		Workers:      1,
	}
}

// testPaths are a fresh data volume and import source.
func testPaths(t *testing.T) paths {
	t.Helper()
	return paths{data: t.TempDir(), imports: t.TempDir(), ffmpeg: media.FFmpegPath, ffprobe: media.FFprobePath, tags: media.TagsPath}
}

// testDaemon is one run() in a goroutine.
type testDaemon struct {
	base   string
	logs   *syncBuffer
	cancel context.CancelFunc
	done   chan error

	once sync.Once
	err  error
}

func startDaemon(t *testing.T, cfg Config, p paths) *testDaemon {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &testDaemon{
		base:   "http://" + ln.Addr().String(),
		logs:   &syncBuffer{},
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go func() { d.done <- run(ctx, cfg, p, ln, newLogger(d.logs)) }()
	t.Cleanup(func() {
		if err := d.stop(t); err != nil {
			t.Logf("run at cleanup: %v", err)
		}
	})
	return d
}

// stop cancels the run and returns its result.
func (d *testDaemon) stop(t *testing.T) error {
	t.Helper()
	d.cancel()
	return d.wait(t)
}

// wait returns the result of run, waiting for at most 60 s.
func (d *testDaemon) wait(t *testing.T) error {
	t.Helper()
	d.once.Do(func() {
		select {
		case d.err = <-d.done:
		case <-time.After(60 * time.Second):
			t.Fatalf("run did not return within 60s; logs:\n%s", d.logs)
		}
	})
	return d.err
}

// get returns the status and error code of a GET; status 0 if the server
// does not answer.
func (d *testDaemon) get(t *testing.T, path string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(d.base + path)
	if err != nil {
		return 0, err.Error()
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var body errorBody
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return resp.StatusCode, ""
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s: body is not JSON: %v", path, err)
	}
	if cc := resp.Header.Get("Cache-Control"); (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusServiceUnavailable) && cc != "no-store" {
		t.Fatalf("GET %s: Cache-Control %q", path, cc)
	}
	return resp.StatusCode, body.Code
}

// waitStatus polls path until it answers with the given status.
func (d *testDaemon) waitStatus(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, code := d.get(t, path)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s = %d (%s), want %d within 30s; logs:\n%s", path, got, code, want, d.logs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// allowConnections makes PostgreSQL accept or refuse connections to the
// database; refusing also terminates the current ones.
func allowConnections(t *testing.T, dbURL string, allow bool) {
	t.Helper()
	name := pgtest.DBName(t, dbURL)
	if allow {
		pgtest.AdminExec(t, "ALTER DATABASE "+name+" ALLOW_CONNECTIONS true")
		return
	}
	pgtest.AdminExec(t, "ALTER DATABASE "+name+" ALLOW_CONNECTIONS false")
	pgtest.AdminExec(t, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '"+name+"'")
}

// assertLockHeld checks that the volume lock is taken (by this process or
// another one).
func assertLockHeld(t *testing.T, data string) {
	t.Helper()
	v, err := volume.Acquire(data)
	if err == nil {
		if cerr := v.Close(); cerr != nil {
			t.Error(cerr)
		}
		t.Fatal("the volume lock is free")
	}
	if volume.Code(err) != volume.CodeLocked {
		t.Fatalf("Acquire: %v, want %s", err, volume.CodeLocked)
	}
}

// assertLockFree checks that the volume lock can be taken.
func assertLockFree(t *testing.T, data string) {
	t.Helper()
	v, err := volume.Acquire(data)
	if err != nil {
		t.Fatalf("the volume lock is not free: %v", err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
}

// assertShutdownOrder checks the shutdown log events: HTTP first, the lock
// released last (§11.1).
func assertShutdownOrder(t *testing.T, logs *syncBuffer, withPool bool) {
	t.Helper()
	want := []string{"http server stopped", "volume lock released"}
	if withPool {
		want = []string{"http server stopped", "database pool closed", "volume lock released"}
	}
	msgs := logs.messages(t)
	start := -1
	for i, m := range msgs {
		if m == want[0] {
			start = i
		}
	}
	if start < 0 {
		t.Fatalf("no shutdown in the logs:\n%s", logs)
	}
	got := msgs[start:]
	if n := len(got); n > 0 && got[n-1] == "stopped" {
		got = got[:n-1]
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("shutdown events %q, want %q", got, want)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}
