// Package faulttest is the test side of the failpoints of DESIGN.md §12.2
// (internal/failpoint): real crashes in child processes, a hook a test can
// switch while a component lives, and a really full filesystem.
//
// Only tests import it (TestOnlyTestsImportFaulttest): nothing here is
// linked into musiclibd. It uses os and unix directly, like the tests'
// own set-up code; the application reaches the disk only through fsops.
//
// A package with crash tests has one TestHelperProcess, the body of its
// child processes:
//
//	func TestHelperProcess(t *testing.T) {
//		mode := faulttest.Mode()
//		if mode == "" {
//			return // a normal run
//		}
//		faulttest.Exit(childMain(t, mode))
//	}
//
// and its tests run the test binary itself as the child (RunChild), with
// the mode and the environment the child needs. A child's hook built by
// Crash kills it with SIGKILL at the named point: no deferred function,
// no close, no rollback; the kernel drops its descriptors, its flocks and
// its database sessions. The parent then plays the next boot.
//
// SIGKILL keeps the page cache: the fsync ordering is argued, not tested
// against a power cut (NOTES.md N-044, N-061, N-136).
package faulttest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"musiclib/internal/failpoint"
)

// Kill sends SIGKILL to the process itself and never returns.
func Kill() {
	if err := unix.Kill(os.Getpid(), unix.SIGKILL); err != nil {
		panic(fmt.Sprintf("faulttest: SIGKILL to self: %v", err))
	}
	select {} // SIGKILL is never delivered late; this does not return
}

// Crash returns a hook that kills the process at the first point named
// name, and lets every other point go on.
func Crash(name string) failpoint.Hook {
	return CrashWhen(func(p failpoint.Point) bool { return p.Name == name })
}

// CrashWhen returns a hook that kills the process at the first point for
// which match is true.
func CrashWhen(match func(failpoint.Point) bool) failpoint.Hook {
	return func(p failpoint.Point) error {
		if match(p) {
			Kill()
		}
		return nil
	}
}

// Switch is a hook whose behaviour a test changes while the component
// holding it lives: the component is built once with Switch.Hook, and the
// test calls Set. It is safe for concurrent use. A Switch belongs to one
// test's environment; nothing is shared between tests.
type Switch struct {
	mu sync.Mutex
	fn failpoint.Hook
}

// Hook is the hook to give the component.
func (s *Switch) Hook() failpoint.Hook {
	return func(p failpoint.Point) error {
		s.mu.Lock()
		fn := s.fn
		s.mu.Unlock()
		if fn == nil {
			return nil
		}
		return fn(p)
	}
}

// Set replaces the behaviour; nil makes every point go on.
func (s *Switch) Set(fn failpoint.Hook) {
	s.mu.Lock()
	s.fn = fn
	s.mu.Unlock()
}

const (
	// modeEnv carries the child's mode.
	modeEnv = "MUSICLIB_FAULTTEST_MODE"
	// resultPrefix marks the child's result line on its output.
	resultPrefix = "FAULTTEST RESULT: "
	// Killed is RunChild's result for a child that SIGKILL ended.
	Killed = "killed"
)

// Mode is the child mode of this process, "" in a normal test run.
func Mode() string { return os.Getenv(modeEnv) }

// Exit prints the child's result line and ends the child with status 0.
func Exit(result string) {
	fmt.Println(resultPrefix + result)
	os.Exit(0)
}

// Child is a running child process.
type Child struct {
	mode   string
	cmd    *exec.Cmd
	cancel context.CancelFunc
	ctx    context.Context
	out    *bytes.Buffer
	// waited is set by Wait, from the test's goroutine only.
	waited bool
}

// Start runs the test binary's TestHelperProcess as a child in mode, with
// env ("K=V") added to this process's environment. The child is killed
// after timeout.
func Start(t testing.TB, timeout time.Duration, mode string, env ...string) *Child {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
	cmd.Env = append(append(os.Environ(), modeEnv+"="+mode), env...)
	out := &bytes.Buffer{}
	// One writer for both: exec serializes the writes.
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("starting child %s: %v", mode, err)
	}
	c := &Child{mode: mode, cmd: cmd, cancel: cancel, ctx: ctx, out: out}
	t.Cleanup(func() {
		// A test that failed before Wait must not leave the child running:
		// kill it and reap it. Its exit status is of no interest any more.
		if !c.waited {
			c.cancel()
			if err := c.cmd.Wait(); err != nil {
				t.Logf("child %s abandoned by a failed test: %v", c.mode, err)
			}
		}
	})
	return c
}

// Wait waits for the child and returns its result line, or Killed if
// SIGKILL ended it. A timeout, another exit or a missing result line fails
// the test with the child's output. Call it from the test's goroutine.
func (c *Child) Wait(t testing.TB) string {
	t.Helper()
	c.waited = true
	err := c.cmd.Wait()
	defer c.cancel()
	if c.ctx.Err() != nil {
		t.Fatalf("child %s timed out\n%s", c.mode, c.out)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
			return Killed
		}
	}
	if err != nil {
		t.Fatalf("child %s: %v\n%s", c.mode, err, c.out)
	}
	for _, line := range strings.Split(c.out.String(), "\n") {
		if res, ok := strings.CutPrefix(line, resultPrefix); ok {
			return res
		}
	}
	t.Fatalf("child %s: no result\n%s", c.mode, c.out)
	return ""
}

// Output is what the child wrote so far; read it only after Wait.
func (c *Child) Output() string { return c.out.String() }

// RunChild is Start then Wait.
func RunChild(t testing.TB, timeout time.Duration, mode string, env ...string) string {
	t.Helper()
	return Start(t, timeout, mode, env...).Wait(t)
}
