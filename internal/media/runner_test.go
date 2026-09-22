package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"musiclib/internal/fsops"
)

// The Runner tests start real processes on the real kernel (§8.5, §12.2
// "SIGTERM/SIGKILL con più worker e helper attivi"). /bin/sh is used as a
// tool that starts other processes, to check that a whole process group is
// killed; the Runner itself never uses a shell.

const sh = "/bin/sh"

// A tool that runs past its timeout is killed with its whole group, and the
// error says timeout, not failure (§8.5). The tool here is the real decoder
// on a long input, as AudioDigest runs it, with a tiny timeout.
func TestRunTimeoutKillsLongDecode(t *testing.T) {
	dir := t.TempDir()
	long := gen(t, dir, "long.flac", append(lavfi("anoisesrc=color=pink:sample_rate=44100:seed=1:duration=600"), "-ac", "2", "-c:a", "flac")...)
	f := open(t, long)
	r := NewRunner(1)
	var pcm countWriter
	start := time.Now()
	_, err := r.Run(t.Context(), Command{
		Path: FFmpegPath, Args: decodeArgs("flac"), Files: []*os.File{f},
		Stdout: &pcm, Timeout: 50 * time.Millisecond,
	})
	e := wantCode(t, err, CodeTimeout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a timeout must wrap context.DeadlineExceeded: %v", err)
	}
	if e.ExitCode != -1 {
		t.Fatalf("exit code %d, want -1 (killed)", e.ExitCode)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run returned %v after a 50ms timeout", elapsed)
	}
	if pcm.n.Load() == 0 {
		t.Fatal("the decoder produced nothing before the timeout: the test did not exercise a running decode")
	}
	if pcm.n.Load() >= 600*44100*2*8 {
		t.Fatal("the decode completed: the timeout did not stop it")
	}
}

// Cancelling the context kills every process of the tool's group, not only
// the tool, and Run returns only when none of them is alive (§8.5: "si
// termina e si attende tutto il gruppo su cancellazione").
func TestRunCancelKillsAndReapsWholeGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := &lineWriter{lines: make(chan string, 8)}
	done := make(chan error, 1)
	go func() {
		_, err := NewRunner(1).Run(ctx, Command{
			Path: sh,
			// The shell prints its pid (= the group id) and the pids of two
			// background children, then waits for them.
			Args:    []string{"-c", "sleep 300 & a=$!; sleep 300 & b=$!; echo $$ $a $b; wait"},
			Stdout:  w,
			Timeout: time.Hour,
		})
		done <- err
	}()
	var pids []int
	select {
	case line := <-w.lines:
		for _, f := range strings.Fields(line) {
			pid, err := strconv.Atoi(f)
			if err != nil {
				t.Fatalf("bad pid line %q", line)
			}
			pids = append(pids, pid)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the tool did not start")
	}
	pgid := pids[0]
	if members := groupMembers(t, pgid); len(members) != 3 {
		t.Fatalf("group %d has %v, want the shell and two children", pgid, members)
	}
	for _, pid := range pids {
		if _, pg, ok := procState(pid); !ok || pg != pgid {
			t.Fatalf("pid %d is not in group %d", pid, pgid)
		}
	}

	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	wantCode(t, err, CodeCanceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancellation must wrap context.Canceled: %v", err)
	}
	// Checked right after Run returned, without waiting: Run must already
	// have waited for the whole group.
	if members := groupMembers(t, pgid); len(members) != 0 {
		t.Fatalf("processes of the group survived: %v", members)
	}
	for _, pid := range pids {
		if alive(pid) {
			t.Fatalf("pid %d survived the cancellation", pid)
		}
	}
	if err := unix.Kill(-pgid, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("kill(-%d, 0) = %v, want ESRCH: the group still exists", pgid, err)
	}
}

// A tool that exits normally but leaves a process of its group behind: the
// straggler is killed before Run returns (§8.5: nothing of an old attempt
// keeps working).
func TestRunKillsStragglersAfterNormalExit(t *testing.T) {
	var out bytes.Buffer
	start := time.Now()
	_, err := NewRunner(1).Run(t.Context(), Command{
		Path: sh,
		// The background child keeps stdout, so without the group kill Run
		// would wait 30 s for EOF.
		Args:    []string{"-c", "sleep 30 & echo $!"},
		Stdout:  &out,
		Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run waited %v for a straggler instead of killing it", elapsed)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("output %q", out.String())
	}
	if alive(pid) {
		t.Fatalf("the straggler %d is alive after Run returned", pid)
	}
}

// Pdeathsig (§8.5, §12.2): when the application dies abruptly, even by
// SIGKILL, the tool it started dies with it. A real parent process runs a
// tool through the Runner and is killed; the tool must not survive it.
func TestRunToolDiesWithParent(t *testing.T) {
	cmd := exec.Command(testBinary(t), helperArgs("pdeathsig-parent")...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(stdout)
	var tool int
	select {
	case line := <-lines:
		pid, ok := strings.CutPrefix(line, "tool-pid ")
		if !ok {
			t.Fatalf("unexpected line from the parent: %q", line)
		}
		if tool, err = strconv.Atoi(pid); err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the parent did not report its tool")
	}
	t.Cleanup(func() { _ = unix.Kill(tool, unix.SIGKILL) }) // only matters if the test fails
	if !alive(tool) {
		t.Fatal("the tool is not running")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("the parent exited normally")
	}
	if !waitDead(t, tool, 5*time.Second) {
		t.Fatalf("the tool %d survived its parent's SIGKILL", tool)
	}
}

// The global semaphore of §6.1: at most `slots` tools run at once, however
// many goroutines call the Runner. Each tool registers itself in a
// directory while it runs and reports how many it saw, so the bound is
// measured on the processes themselves.
func TestRunSemaphoreBoundsConcurrency(t *testing.T) {
	const slots, callers = 2, 8
	dir := t.TempDir()
	r := NewRunner(slots)
	script := `mkdir "$1/$$" && n=$(ls "$1" | wc -l) && sleep 0.2 && rmdir "$1/$$" && echo "$n"`
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen []int
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out bytes.Buffer
			if _, err := r.Run(t.Context(), Command{
				Path: sh, Args: []string{"-c", script, "sh", dir},
				Stdout: &out, Timeout: time.Minute,
			}); err != nil {
				t.Error(err)
				return
			}
			n, err := strconv.Atoi(strings.TrimSpace(out.String()))
			if err != nil {
				t.Errorf("output %q", out.String())
				return
			}
			mu.Lock()
			seen = append(seen, n)
			mu.Unlock()
		}()
	}
	wg.Wait()
	peak := 0
	for _, n := range seen {
		peak = max(peak, n)
	}
	if peak > slots {
		t.Fatalf("%d tools ran at once, the Runner allows %d", peak, slots)
	}
	if peak < slots {
		t.Fatalf("at most %d tool ran at once: the test did not exercise concurrency", peak)
	}
}

// A caller waiting for a slot is released by its context, and never starts
// its tool.
func TestRunCancelWhileWaitingForSlot(t *testing.T) {
	r := NewRunner(1)
	busyCtx, stopBusy := context.WithCancel(t.Context())
	defer stopBusy()
	busy := make(chan error, 1)
	w := &lineWriter{lines: make(chan string, 1)}
	go func() {
		_, err := r.Run(busyCtx, Command{Path: sh, Args: []string{"-c", "echo up; exec sleep 300"},
			Stdout: w, Timeout: time.Minute})
		busy <- err
	}()
	<-w.lines // the only slot is taken
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := r.Run(ctx, Command{Path: sh, Args: []string{"-c", "touch " + filepath.Join(dir, "started")}, Timeout: time.Minute})
	wantCode(t, err, CodeCanceled)
	if _, serr := os.Stat(filepath.Join(dir, "started")); serr == nil {
		t.Fatal("the tool started without a slot")
	}
	stopBusy()
	wantCode(t, <-busy, CodeCanceled)
	// The slot is free again.
	if _, err := r.Run(t.Context(), Command{Path: "/bin/true", Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
}

// Standard error is kept up to 64 KiB (§8.5) and the rest is drained, so a
// verbose tool neither blocks nor fills the memory.
func TestRunStderrBounded(t *testing.T) {
	res, err := NewRunner(1).Run(t.Context(), Command{
		Path: sh, Args: []string{"-c", "head -c 1000000 /dev/zero | tr '\\0' x >&2; echo done"},
		Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stderr) != StderrLimit || !res.StderrTruncated {
		t.Fatalf("stderr kept %d bytes (truncated %v), want %d and truncated", len(res.Stderr), res.StderrTruncated, StderrLimit)
	}
	if StderrLimit != 64<<10 {
		t.Fatalf("StderrLimit = %d, §8.5 says 64 KiB", StderrLimit)
	}
	if strings.Trim(string(res.Stderr), "x") != "" {
		t.Fatal("stderr content altered")
	}

	// Below the limit: kept whole, not truncated; also on failure.
	res, err = NewRunner(1).Run(t.Context(), Command{
		Path: sh, Args: []string{"-c", "echo boom >&2; exit 4"}, Timeout: time.Minute,
	})
	e := wantCode(t, err, CodeToolFailed)
	if string(res.Stderr) != "boom\n" || res.StderrTruncated || string(e.Stderr) != "boom\n" {
		t.Fatalf("stderr %q / %q", res.Stderr, e.Stderr)
	}
	if strings.Contains(err.Error(), "boom") {
		t.Fatal("stderr must not be part of Error(): it may quote tag contents (§11.1)")
	}
}

// A non-zero exit is a failure even when the tool printed plausible output
// (§8.5), and so is a death by signal.
func TestRunNonZeroExitIsFailure(t *testing.T) {
	var out bytes.Buffer
	_, err := NewRunner(1).Run(t.Context(), Command{
		Path:    sh,
		Args:    []string{"-c", `printf '{"format": {"format_name": "flac"}, "streams": []}'; exit 3`},
		Stdout:  &out,
		Timeout: time.Minute,
	})
	e := wantCode(t, err, CodeToolFailed)
	if e.ExitCode != 3 || out.Len() == 0 {
		t.Fatalf("exit code %d, output %q", e.ExitCode, out.String())
	}

	_, err = NewRunner(1).Run(t.Context(), Command{
		Path: sh, Args: []string{"-c", "echo ok; kill -SEGV $$"}, Timeout: time.Minute,
	})
	e = wantCode(t, err, CodeToolFailed)
	if e.ExitCode != -1 || !strings.Contains(e.Msg, "signal") {
		t.Fatalf("a signal death: %v (exit %d)", err, e.ExitCode)
	}
}

// Descriptors (§8.5): the tool gets 0, 1, 2 and the files it is given,
// nothing else. In particular it does not inherit the volume flock, taken
// here through fsops exactly as the server takes it, nor any other file the
// process has open. Standard input is /dev/null, the environment is empty
// and the working directory is "/".
func TestRunDescriptorsEnvironmentAndCwd(t *testing.T) {
	dir := t.TempDir()
	root, err := fsops.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	}()
	lock, err := root.Lock(".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
	other, err := os.Create(filepath.Join(dir, "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := other.Close(); err != nil {
			t.Error(err)
		}
	}()
	input := writeFile(t, filepath.Join(dir, "input"), []byte("x"))
	in := open(t, input)

	var lockSt, inSt unix.Stat_t
	if err := unix.Stat(filepath.Join(dir, ".lock"), &lockSt); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fstat(int(in.Fd()), &inSt); err != nil {
		t.Fatal(err)
	}
	lockID := fmt.Sprintf("%d:%d", lockSt.Dev, lockSt.Ino)
	inID := fmt.Sprintf("%d:%d", inSt.Dev, inSt.Ino)

	inspect := func(files ...*os.File) (fds map[int]string, ids map[int]string, props map[string]string) {
		var out bytes.Buffer
		if _, err := NewRunner(1).Run(t.Context(), Command{
			Path: testBinary(t), Args: helperArgs("inspect"), Files: files,
			Stdout: &out, Timeout: time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
		fds, ids, props = map[int]string{}, map[int]string{}, map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			f := strings.Fields(line)
			switch {
			case len(f) >= 3 && f[0] == "fd":
				n, _ := strconv.Atoi(f[1])
				fds[n] = strings.Join(f[3:], " ")
				ids[n] = f[2]
			case len(f) == 2:
				props[f[0]] = f[1]
			default:
				t.Fatalf("unexpected line %q", line)
			}
		}
		return fds, ids, props
	}

	// Control: a lock file passed on purpose is seen, so the check below
	// can see an inherited lock (N-034).
	lockAgain, err := os.Open(filepath.Join(dir, ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lockAgain.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, ids, _ := inspect(in, lockAgain); ids[4] != lockID {
		t.Fatalf("control: descriptor 4 is %q, want the lock file %s", ids[4], lockID)
	}

	fds, ids, props := inspect(in)
	if len(fds) != 4 {
		t.Fatalf("the tool has descriptors %v, want exactly 0, 1, 2, 3", fds)
	}
	for _, fd := range []int{0, 1, 2, 3} {
		if _, ok := fds[fd]; !ok {
			t.Fatalf("descriptor %d missing: %v", fd, fds)
		}
	}
	if fds[0] != "/dev/null" {
		t.Fatalf("stdin is %q, want /dev/null", fds[0])
	}
	if !strings.HasPrefix(fds[1], "pipe:") || !strings.HasPrefix(fds[2], "pipe:") {
		t.Fatalf("stdout %q, stderr %q: want pipes", fds[1], fds[2])
	}
	for fd, id := range ids {
		if id == lockID {
			t.Fatalf("the tool inherited the volume lock as descriptor %d", fd)
		}
	}
	if ids[3] != inID {
		t.Fatalf("descriptor 3 is %s, want the input %s", ids[3], inID)
	}
	if props["env"] != "0" || props["cwd"] != "/" {
		t.Fatalf("env %s, cwd %s: want an empty environment and /", props["env"], props["cwd"])
	}
	if props["pgid"] != props["pid"] {
		t.Fatalf("the tool is not the leader of its own group: %v", props)
	}
	if pg := unix.Getpgrp(); strconv.Itoa(pg) == props["pgid"] {
		t.Fatal("the tool shares the test's process group")
	}
}

// Stdin, when given, reaches the tool through a pipe; a tool that exits
// without reading all of it fails the call.
func TestRunStdin(t *testing.T) {
	input := bytes.Repeat([]byte("json input\n"), 50000) // larger than a pipe buffer
	var out bytes.Buffer
	if _, err := NewRunner(1).Run(t.Context(), Command{
		Path: "/bin/cat", Stdin: input, Stdout: &out, Timeout: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), input) {
		t.Fatalf("stdin not delivered intact: %d bytes back of %d", out.Len(), len(input))
	}

	_, err := NewRunner(1).Run(t.Context(), Command{
		Path: "/bin/true", Stdin: input, Timeout: time.Minute,
	})
	wantCode(t, err, CodeToolFailed)
}

// The output limit kills the tool when exceeded; a failing Stdout does too.
func TestRunStdoutLimitAndWriterError(t *testing.T) {
	var out bytes.Buffer
	_, err := NewRunner(1).Run(t.Context(), Command{
		Path: "/bin/cat", Args: []string{"/dev/zero"}, Stdout: &out, StdoutLimit: 1000, Timeout: time.Minute,
	})
	wantCode(t, err, CodeOutputTooLarge)
	if out.Len() > 1000 {
		t.Fatalf("%d bytes passed a limit of 1000", out.Len())
	}

	boom := errors.New("sink failed")
	_, err = NewRunner(1).Run(t.Context(), Command{
		Path: "/bin/cat", Args: []string{"/dev/zero"}, Stdout: failingWriter{boom}, Timeout: time.Minute,
	})
	wantCode(t, err, CodeIO)
	if !errors.Is(err, boom) {
		t.Fatalf("the writer's error is lost: %v", err)
	}
}

// Invalid calls and tools that cannot start.
func TestRunInvalidCalls(t *testing.T) {
	r := NewRunner(1)
	_, err := r.Run(t.Context(), Command{Path: "sh", Timeout: time.Minute})
	wantCode(t, err, CodeInvalidArgument)
	_, err = r.Run(t.Context(), Command{Path: sh})
	wantCode(t, err, CodeInvalidArgument)
	_, err = r.Run(t.Context(), Command{Path: "/nonexistent/tool", Timeout: time.Minute})
	wantCode(t, err, CodeToolUnavailable)
	notExec := writeFile(t, filepath.Join(t.TempDir(), "tool"), []byte("#!/bin/sh\n"))
	_, err = r.Run(t.Context(), Command{Path: notExec, Timeout: time.Minute})
	wantCode(t, err, CodeToolUnavailable)

	defer func() {
		if recover() == nil {
			t.Fatal("NewRunner(0) did not panic")
		}
	}()
	NewRunner(0)
}

// No descriptor of this process leaks through any path of Run: success,
// failure, timeout, cancellation, output limit, start failure.
func TestRunNoDescriptorLeaks(t *testing.T) {
	r := NewRunner(2)
	calls := []Command{
		{Path: sh, Args: []string{"-c", "echo out; echo err >&2"}, Timeout: time.Minute},
		{Path: sh, Args: []string{"-c", "exit 7"}, Timeout: time.Minute},
		{Path: sh, Args: []string{"-c", "sleep 300 & sleep 300"}, Timeout: 20 * time.Millisecond},
		{Path: "/bin/cat", Args: []string{"/dev/zero"}, StdoutLimit: 10, Timeout: time.Minute},
		{Path: "/bin/cat", Stdin: []byte("in"), Timeout: time.Minute},
		{Path: "/nonexistent", Timeout: time.Minute},
	}
	for _, c := range calls { // warm-up: lazily opened runtime descriptors
		_, _ = r.Run(t.Context(), c)
	}
	before := openFDs(t)
	for range 5 {
		for _, c := range calls {
			_, _ = r.Run(t.Context(), c) // outcomes are tested elsewhere
		}
		ctx, cancel := context.WithCancel(t.Context())
		go func() { time.Sleep(20 * time.Millisecond); cancel() }()
		_, _ = r.Run(ctx, Command{Path: sh, Args: []string{"-c", "sleep 300"}, Timeout: time.Minute})
		cancel()
	}
	after := openFDs(t)
	for fd, target := range after {
		if _, ok := before[fd]; !ok {
			t.Errorf("descriptor %s (%s) leaked", fd, target)
		}
	}
}

type countWriter struct{ n atomic.Int64 }

func (w *countWriter) Write(b []byte) (int, error) {
	w.n.Add(int64(len(b)))
	return len(b), nil
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// Run returns only when no process of the group exists any more, zombies
// included (§8.5: "si termina e si attende tutto il gruppo"). Here a process
// that this test starts joins the tool's group; once killed it stays a
// zombie until the test reaps it, and Run must still be waiting then.
func TestRunWaitsUntilTheGroupIsGone(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := &lineWriter{lines: make(chan string, 1)}
	type result struct {
		err error
		at  time.Time
	}
	done := make(chan result, 1)
	go func() {
		_, err := NewRunner(1).Run(ctx, Command{
			Path: sh, Args: []string{"-c", "echo $$; exec sleep 300"}, Stdout: w, Timeout: time.Minute,
		})
		done <- result{err, time.Now()}
	}()
	pgid, err := strconv.Atoi(<-w.lines)
	if err != nil {
		t.Fatal(err)
	}
	joiner := exec.Command("/bin/sleep", "300")
	joiner.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := joiner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = joiner.Process.Kill() }) // only matters if the test fails early
	if _, pg, ok := procState(joiner.Process.Pid); !ok || pg != pgid {
		t.Fatalf("the joiner is in group %d, want %d", pg, pgid)
	}

	cancel()
	select {
	case r := <-done:
		t.Fatalf("Run returned (%v) while a process of its group still existed", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if alive(joiner.Process.Pid) {
		t.Fatal("the joiner was not killed with the group")
	}
	reaped := time.Now()
	if err := joiner.Wait(); err == nil {
		t.Fatal("the joiner exited normally")
	}
	select {
	case r := <-done:
		wantCode(t, r.err, CodeCanceled)
		if r.at.Before(reaped) {
			t.Fatal("Run returned before the last process of the group was reaped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the group was gone")
	}
}
