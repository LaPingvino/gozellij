package fabric

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// DrainGrace is how long we wait for the last of a dead process's output to arrive before closing
// the pty out from under the reader.
//
// A process can exit while bytes it already wrote are still in the pty. Closing immediately would
// throw away exactly the output that usually matters - the error message it printed on its way
// out. Waiting forever is not an option either, because a grandchild holding the slave open keeps
// the read alive indefinitely.
const DrainGrace = 250 * time.Millisecond

// StopGrace is how long a process gets to exit after SIGTERM before it is sent SIGKILL.
const StopGrace = 5 * time.Second

// Process is one running instance of a Service.
//
// It owns the child, its pty, and the goroutines draining it. Everything a viewer needs is in
// Output; nothing here knows what a terminal looks like.
type Process struct {
	Service Service
	Output  *OutputBuffer

	cmd        *exec.Cmd
	pty        *os.File
	started    time.Time
	ownsOutput bool

	// done is closed once the child has exited and its output has been drained.
	done chan struct{}
	// drained is closed by the reader goroutine when it stops.
	drained chan struct{}

	mu       sync.Mutex
	exit     Exit
	exitedAt time.Time
	exited   bool
	closed   bool
}

// StartOptions tune a single spawn.
type StartOptions struct {
	// Cols and Rows are the initial pty size. Zero means a sensible default rather than 0x0,
	// which makes curses programs behave very strangely.
	Cols, Rows int
	// OutputBytes is the size of the retained output ring. Zero means DefaultOutputBytes.
	// Ignored when Output is set.
	OutputBytes int
	// Output, when set, is an existing buffer to write into instead of a fresh one. The
	// supervisor uses this so that scrollback survives a restart: you get to see what the
	// process said just before it died, immediately above the line saying it is coming back.
	// A borrowed buffer is not closed by Process.Close - the lender still owns it.
	Output *OutputBuffer
	// ExtraEnv is appended after the service's own Env.
	ExtraEnv []string
}

const (
	defaultCols = 80
	defaultRows = 24
)

// Start runs a service in a new pty.
//
// The child gets its own session with the pty as controlling terminal, so job control, SIGWINCH
// and Ctrl-C behave the way they do in a real terminal (creack/pty arranges this).
func Start(s Service, opts StartOptions) (*Process, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	// Resolve the command up front so "command not found" is reported as itself, at spawn time,
	// rather than as a mysterious immediate exit that the restart policy then dutifully retries
	// forever.
	path, err := exec.LookPath(s.Command)
	if err != nil {
		return nil, fmt.Errorf("service %s: cannot run %q: %w", s.Name, s.Command, err)
	}

	cmd := exec.Command(path, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)
	cmd.Env = append(cmd.Env, opts.ExtraEnv...)

	cols, rows := opts.Cols, opts.Rows
	if cols <= 0 {
		cols = defaultCols
	}
	if rows <= 0 {
		rows = defaultRows
	}

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, fmt.Errorf("service %s: starting %s: %w", s.Name, path, err)
	}

	out := opts.Output
	ownsOutput := false
	if out == nil {
		out = NewOutputBuffer(opts.OutputBytes)
		ownsOutput = true
	}

	p := &Process{
		Service:    s,
		Output:     out,
		ownsOutput: ownsOutput,
		cmd:        cmd,
		pty:        f,
		started:    time.Now(),
		done:       make(chan struct{}),
		drained:    make(chan struct{}),
	}

	go p.drain()
	go p.reap()

	return p, nil
}

// drain copies the pty to the output buffer until the child is gone.
func (p *Process) drain() {
	defer close(p.drained)
	buf := make([]byte, 32<<10)
	for {
		n, err := p.pty.Read(buf)
		if n > 0 {
			// A closed buffer here means we are shutting down; stop rather than spin.
			if _, werr := p.Output.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// reap waits for the child, gives its remaining output a moment to arrive, and records the exit.
func (p *Process) reap() {
	err := p.cmd.Wait()

	// The child is gone, but bytes it wrote may still be in flight. Give the reader a moment,
	// then close the pty so it cannot block forever on a grandchild holding the slave open.
	select {
	case <-p.drained:
	case <-time.After(DrainGrace):
		p.closePTY()
		<-p.drained
	}

	p.mu.Lock()
	p.exit = exitFrom(err, p.cmd.ProcessState)
	p.exitedAt = time.Now()
	p.exited = true
	p.mu.Unlock()

	p.closePTY()
	close(p.done)
}

// exitFrom turns what os/exec gives us into our own Exit.
//
// A process killed by a signal reports exit code -1 through ProcessState, which is not a code any
// process ever returned; recording the signal name and leaving the code at -1 keeps the two
// distinguishable, which is what RestartOnFailure needs.
func exitFrom(waitErr error, st *os.ProcessState) Exit {
	if st == nil {
		// No process state at all: something went wrong we cannot describe properly. Say so
		// rather than reporting a clean exit.
		if waitErr != nil {
			return Exit{Code: -1, Signal: "unknown: " + waitErr.Error()}
		}
		return Exit{Code: -1, Signal: "unknown"}
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return Exit{Code: -1, Signal: ws.Signal().String()}
	}
	return Exit{Code: st.ExitCode()}
}

func (p *Process) closePTY() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	_ = p.pty.Close()
}

// Pid returns the child's process id, or 0 if it never started.
func (p *Process) Pid() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// StartedAt is when the process was spawned.
func (p *Process) StartedAt() time.Time { return p.started }

// Ran returns how long the process has been running, or how long it ran before exiting.
//
// The supervisor feeds this to RestartTracker.Died to decide whether the process was flapping or
// had settled, so it has to be the real lifetime and not "time since start, measured whenever you
// happened to ask".
func (p *Process) Ran() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return p.exitedAt.Sub(p.started)
	}
	return time.Since(p.started)
}

// Done is closed once the process has exited and its output has been drained.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait blocks until the process has exited and returns how it ended.
func (p *Process) Wait() Exit {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit
}

// Exited reports whether the process has finished, and how, without blocking.
func (p *Process) Exited() (Exit, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit, p.exited
}

// ErrProcessGone is returned when input or a resize is sent to a process that has exited.
var ErrProcessGone = errors.New("process has exited")

// Write sends input to the process, as if typed.
func (p *Process) Write(b []byte) (int, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return 0, ErrProcessGone
	}
	n, err := p.pty.Write(b)
	if err != nil {
		// Writing to a pty whose child is gone gives EIO. Report it as what it means, not as
		// an opaque io error.
		if errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
			return n, ErrProcessGone
		}
		return n, fmt.Errorf("writing to %s: %w", p.Service.Name, err)
	}
	return n, nil
}

// Resize changes the pty window size, which makes the child see SIGWINCH.
func (p *Process) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid size %dx%d: both must be positive", cols, rows)
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return ErrProcessGone
	}
	if err := pty.Setsize(p.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		if errors.Is(err, os.ErrClosed) {
			return ErrProcessGone
		}
		return fmt.Errorf("resizing %s to %dx%d: %w", p.Service.Name, cols, rows, err)
	}
	return nil
}

// Signal sends a signal to the child.
func (p *Process) Signal(sig os.Signal) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return ErrProcessGone
	}
	if err := p.cmd.Process.Signal(sig); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return ErrProcessGone
		}
		return fmt.Errorf("signalling %s: %w", p.Service.Name, err)
	}
	return nil
}

// Stop asks the process to exit: SIGTERM, then SIGKILL if it is still there after StopGrace.
//
// It returns how the process ended. A process that ignores SIGTERM is not an error - it is the
// normal case for a few programs - but the caller can tell the difference, because a killed
// process reports its signal.
func (p *Process) Stop() Exit {
	if _, done := p.Exited(); done {
		return p.Wait()
	}

	if err := p.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, ErrProcessGone) {
		// Nothing useful to do about it, but it should not vanish either.
		p.logf("SIGTERM failed: %v", err)
	}

	select {
	case <-p.done:
		return p.Wait()
	case <-time.After(StopGrace):
	}

	if err := p.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, ErrProcessGone) {
		p.logf("SIGKILL failed: %v", err)
	}
	return p.Wait()
}

// Close releases the process's resources. It does not stop the child; use Stop for that.
//
// A borrowed output buffer (StartOptions.Output) is left open: it belongs to whoever lent it, and
// closing it here would cut off every viewer the moment one process restarted.
func (p *Process) Close() {
	p.closePTY()
	if p.ownsOutput {
		p.Output.Close()
	}
}

// logf is a placeholder for the fabric's logger, which arrives with the daemon. It deliberately
// goes to stderr rather than nowhere: a swallowed diagnostic is how the last project lost an
// afternoon.
func (p *Process) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gozellij: %s: "+format+"\n", append([]any{p.Service.Name}, args...)...)
}

// interface checks
var (
	_ io.Writer = (*Process)(nil)
)
