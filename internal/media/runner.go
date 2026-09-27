package media

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Limits of every native tool invocation (DESIGN.md §8.5).
const (
	// InspectTimeout bounds inspect, tag and image operations, and every
	// ffprobe run.
	InspectTimeout = 30 * time.Second
	// DecodeTimeout bounds the full decode of one track.
	DecodeTimeout = 30 * time.Minute
	// StderrLimit is how much of a tool's standard error is kept; the rest
	// is read and discarded, so the tool never blocks on a full pipe.
	StderrLimit = 64 << 10

	// groupWaitTimeout bounds the wait for a killed process group to
	// disappear. SIGKILL cannot be ignored, so only a process stuck in the
	// kernel (or a zombie nobody reaps: see NOTES.md N-076) can exceed it.
	groupWaitTimeout = 10 * time.Second
	readChunk        = 64 << 10
)

// errTimedOut is the cause of a context that expired because of the
// per-call timeout, as opposed to the caller's own cancellation.
var errTimedOut = errors.New("tool timeout")

// Runner runs native tools. It is the only way the application starts a
// process (§2.1, §8.5), and it carries the global semaphore of §6.1: at most
// `slots` tools run at once, whoever calls them. There is one Runner per
// process, built at boot with WORKERS slots and passed explicitly to the
// adapters that need it (§2.3); it has no package-level state.
type Runner struct {
	slots chan struct{}
}

// NewRunner returns a Runner that lets at most slots tools run at once.
// slots is WORKERS (§6.1), already validated by the configuration; a value
// below 1 is a programming error and panics.
func NewRunner(slots int) *Runner {
	if slots < 1 {
		panic("media: NewRunner needs at least one slot")
	}
	return &Runner{slots: make(chan struct{}, slots)}
}

// Command is one invocation of a native tool.
//
// The tool gets an empty environment, "/" as working directory, and only
// these descriptors: 0 (Stdin, or /dev/null), 1 and 2 (pipes read by the
// Runner) and Files as 3, 4, ... Everything else the process holds, the
// volume flock included, is close-on-exec (§8.5).
type Command struct {
	// Path is the absolute path of the executable. It is executed directly:
	// arguments never go through a shell.
	Path string
	Args []string
	// Files are inherited as descriptors 3, 4, ... in this order. The child
	// shares their file offsets.
	Files []*os.File
	// Env is an explicit child environment. Nil gives an empty environment;
	// credentials for offline PostgreSQL tools must never be passed in argv.
	Env []string
	// Stdin, when not nil, is written to the tool's standard input through a
	// pipe that is closed afterwards. A tool that exits successfully without
	// reading all of it fails the call. Nil means /dev/null.
	Stdin []byte
	// Stdout receives the tool's standard output while it runs, so that a
	// decoder's output is never held in memory (§6.1). Nil discards it.
	Stdout io.Writer
	// StdoutLimit, when positive, is the most bytes Stdout may receive; the
	// tool is killed when it writes more.
	StdoutLimit int64
	// Timeout bounds the run, from the moment the tool gets a slot; it is
	// required (§8.5).
	Timeout time.Duration
}

// Result is what the Runner keeps of a run besides stdout.
type Result struct {
	// Stderr holds the first StderrLimit bytes of the standard error.
	Stderr          []byte
	StderrTruncated bool
}

// Run waits for a slot, starts the tool in its own process group with
// SIGKILL as parent-death signal, and waits until it and every other process
// of its group are gone. It returns an error for a non-zero exit, a signal, a
// timeout (CodeTimeout), a cancellation of ctx (CodeCanceled), an output over
// StdoutLimit or a failing Stdout, whatever the tool printed.
//
// On timeout or cancellation the whole group is killed with SIGKILL and
// reaped before Run returns. The group is also killed after a normal exit of
// the tool, so that nothing it may have left behind keeps running (§8.5:
// "si termina e si attende tutto il gruppo").
func (r *Runner) Run(ctx context.Context, c Command) (Result, error) {
	op := filepath.Base(c.Path)
	if !filepath.IsAbs(c.Path) {
		return Result{}, newErr(CodeInvalidArgument, op, "the tool path must be absolute", nil)
	}
	if c.Timeout <= 0 {
		return Result{}, newErr(CodeInvalidArgument, op, "a timeout is required", nil)
	}
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return Result{}, newErr(CodeCanceled, op, "cancelled while waiting for a tool slot", ctx.Err())
	}
	defer func() { <-r.slots }()

	runCtx, cancel := context.WithTimeoutCause(ctx, c.Timeout, errTimedOut)
	defer cancel()
	p, err := start(c)
	if err != nil {
		return Result{}, err
	}
	return p.wait(runCtx, ctx, c, op)
}

// proc is one started tool and the goroutines serving its pipes.
type proc struct {
	cmd  *exec.Cmd
	pgid int

	stdout, stderr *os.File // parent's read ends

	exited  chan error // waitid(WNOWAIT): the leader exited, not yet reaped
	outDone chan error // stdout fully read, or the copy failed
	errDone chan struct{}
	inDone  chan error // nil if there is no stdin pipe

	errBuf    []byte
	errTrunc  bool
	errReadEr error
	killErr   error // first failure of killGroup (only wait's goroutine calls it)
}

// start creates the pipes and starts the tool. The parent's copies of the
// child's pipe ends are closed before it returns, so that EOF on stdout and
// stderr means that no process holds them any more.
func start(c Command) (*proc, error) {
	op := filepath.Base(c.Path)
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, newErr(CodeIO, op, "stdout pipe", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return nil, newErr(CodeIO, op, "stderr pipe", errors.Join(err, outR.Close(), outW.Close()))
	}
	var inR, inW *os.File
	if c.Stdin != nil {
		if inR, inW, err = os.Pipe(); err != nil {
			return nil, newErr(CodeIO, op, "stdin pipe",
				errors.Join(err, outR.Close(), outW.Close(), errR.Close(), errW.Close()))
		}
	}

	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env = append([]string{}, c.Env...) // not nil: never inherit the environment
	cmd.Dir = "/"
	cmd.Stdout = outW
	cmd.Stderr = errW
	if inR != nil {
		cmd.Stdin = inR
	} // else os/exec opens /dev/null
	cmd.ExtraFiles = c.Files
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Own process group (pgid = pid), so that the whole group can be
		// killed; SIGKILL if the thread that started it (and therefore this
		// process) dies, including by SIGKILL (§8.5, §12.2). Go re-checks
		// the parent after prctl, closing the window of a parent that dies
		// during the fork.
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	startErr := cmd.Start()
	closeErr := closeFiles(outW, errW, inR)
	if startErr != nil {
		return nil, &Error{Code: CodeToolUnavailable, Op: op, Msg: "cannot start the tool", ExitCode: -1,
			Err: errors.Join(startErr, closeErr, closeFiles(outR, errR, inW))}
	}

	p := &proc{
		cmd:     cmd,
		pgid:    cmd.Process.Pid,
		stdout:  outR,
		stderr:  errR,
		exited:  make(chan error, 1),
		outDone: make(chan error, 1),
		errDone: make(chan struct{}),
	}
	go p.waitExit()
	go func() { p.outDone <- copyOut(c.Stdout, outR, c.StdoutLimit) }()
	go p.drainStderr()
	if inW != nil {
		p.inDone = make(chan error, 1)
		go func() { p.inDone <- writeIn(inW, c.Stdin) }()
	}
	if closeErr != nil {
		// The tool runs; the parent could not close its copy of a pipe end,
		// so EOF may never come. Kill it and report.
		p.killGroup()
		_, werr := p.wait(context.Background(), context.Background(), c, op)
		return nil, newErr(CodeIO, op, "closing the child's pipe ends", errors.Join(closeErr, werr))
	}
	return p, nil
}

// wait is the rest of Run: it waits for the tool, kills and reaps the group,
// collects the pipes and turns the outcome into an error. runCtx carries the
// timeout, ctx the caller's cancellation.
func (p *proc) wait(runCtx, ctx context.Context, c Command, op string) (Result, error) {
	var (
		killedFor error // why the Runner killed the tool, nil if it did not
		outErr    error
		outFinish bool
	)
	outDone := p.outDone
	for exited := false; !exited; {
		select {
		case <-p.exited:
			exited = true
		case <-runCtx.Done():
			if killedFor == nil {
				killedFor = context.Cause(runCtx)
				p.killGroup()
			}
			runCtx = context.Background() // do not select it again
		case err := <-outDone:
			outDone, outFinish = nil, true
			if err != nil && killedFor == nil {
				outErr, killedFor = err, err
				p.killGroup()
			}
		}
	}
	// The leader has exited but is not reaped, so its pid, which is the
	// group id, cannot be reused yet: killing the group is safe. It removes
	// anything the tool left running.
	p.killGroup()
	werr := p.cmd.Wait() // reaps the leader
	groupErr := waitGroupGone(p.pgid)
	if !outFinish {
		if err := <-p.outDone; err != nil && outErr == nil {
			outErr = err
		}
	}
	<-p.errDone
	var inErr error
	if p.inDone != nil {
		inErr = <-p.inDone
	}
	closeErr := closeFiles(p.stdout, p.stderr)

	res := Result{Stderr: p.errBuf, StderrTruncated: p.errTrunc}
	state := p.cmd.ProcessState
	var exitErr *exec.ExitError
	if state == nil || (werr != nil && !errors.As(werr, &exitErr)) {
		return res, newErr(CodeIO, op, "waiting for the tool", werr)
	}
	exitCode, signal := state.ExitCode(), ""
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		signal = ws.Signal().String()
	}
	fail := func(code, msg string, err error) (Result, error) {
		return res, &Error{Code: code, Op: op, Msg: msg, ExitCode: exitCode, Stderr: p.errBuf, Err: err}
	}
	switch {
	case killedFor != nil && errors.Is(killedFor, errTimedOut):
		return fail(CodeTimeout, "killed after "+c.Timeout.String(), context.DeadlineExceeded)
	case killedFor != nil && ctx.Err() != nil:
		return fail(CodeCanceled, "killed on cancellation", ctx.Err())
	case outErr != nil:
		var me *Error
		if errors.As(outErr, &me) {
			return fail(me.Code, me.Msg, me.Err)
		}
		return fail(CodeIO, "consuming the standard output", outErr)
	case exitCode != 0 || signal != "":
		return fail(CodeToolFailed, exitMsg(exitCode, signal), nil)
	case inErr != nil:
		return fail(CodeToolFailed, "the tool did not read its whole standard input", inErr)
	case p.errReadEr != nil:
		return fail(CodeIO, "reading the standard error", p.errReadEr)
	case groupErr != nil || p.killErr != nil:
		return fail(CodeIO, "killing and waiting for the process group", errors.Join(p.killErr, groupErr))
	case closeErr != nil:
		return fail(CodeIO, "closing the pipes", closeErr)
	}
	return res, nil
}

// waitExit waits until the leader exits, without reaping it (WNOWAIT).
func (p *proc) waitExit() {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, p.pgid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err == unix.EINTR {
			continue
		}
		p.exited <- err
		return
	}
}

// killGroup sends SIGKILL to the tool's process group. It is called only
// while the leader is not reaped, so the group id still belongs to it and
// the kill cannot reach an unrelated process. ESRCH (nothing left) is not an
// error; any other failure is kept and reported by wait.
func (p *proc) killGroup() {
	if err := unix.Kill(-p.pgid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) && p.killErr == nil {
		p.killErr = err
	}
}

// waitGroupGone polls until no process of the group exists any more. After
// the SIGKILL of killGroup this only waits for the kernel to finish the
// kills and for the init process to reap orphans (compose.yaml runs
// `init: true`).
func waitGroupGone(pgid int) error {
	deadline := time.Now().Add(groupWaitTimeout)
	for delay := 100 * time.Microsecond; ; delay = min(2*delay, 20*time.Millisecond) {
		err := unix.Kill(-pgid, 0)
		switch {
		case errors.Is(err, unix.ESRCH):
			return nil
		case err != nil && !errors.Is(err, unix.EPERM):
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("the process group still exists " + groupWaitTimeout.String() + " after SIGKILL")
		}
		time.Sleep(delay)
	}
}

// drainStderr keeps the first StderrLimit bytes of the standard error and
// discards the rest, reading until EOF.
func (p *proc) drainStderr() {
	defer close(p.errDone)
	buf := make([]byte, readChunk)
	for {
		n, err := p.stderr.Read(buf)
		if n > 0 {
			keep := min(n, StderrLimit-len(p.errBuf))
			p.errBuf = append(p.errBuf, buf[:keep]...)
			if keep < n {
				p.errTrunc = true
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			p.errReadEr = err
			return
		}
	}
}

// copyOut copies the tool's stdout to w (nil discards it), failing with
// CodeOutputTooLarge past limit (when positive). It returns nil at EOF.
func copyOut(w io.Writer, r io.Reader, limit int64) error {
	if w == nil {
		w = io.Discard
	}
	buf := make([]byte, readChunk)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			total += int64(n)
			if limit > 0 && total > limit {
				return newErr(CodeOutputTooLarge, "stdout", "the tool wrote more than the allowed output", nil)
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// writeIn writes the whole input and closes the pipe.
func writeIn(w *os.File, data []byte) error {
	_, err := w.Write(data)
	return errors.Join(err, w.Close())
}

// closeFiles closes every non-nil file and joins the errors.
func closeFiles(files ...*os.File) error {
	var errs []error
	for _, f := range files {
		if f != nil {
			if err := f.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
