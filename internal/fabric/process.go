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

// DrainAbandon is how long reap waits for the pty reader to finish *after* closing the pty, before
// giving up on it.
//
// It has to give up, and that is worth explaining. Closing an os.File does not interrupt a read
// already in flight on a descriptor the runtime cannot poll, and a pty master is one of those: the
// read returns when something writes or when the last writer closes the slave. A grandchild
// holding the slave open therefore keeps the reader blocked for as long as it lives - and reap was
// waiting for that reader unconditionally, so Process.Wait never returned, so `gozellij stop` hung
// for ever. (Measured: a service with a child that ignores SIGHUP made stop hang until the client
// timed out at thirty seconds, and the daemon never finished stopping it at all.)
//
// Killing the process group, below, makes this rare. It cannot make it impossible, because a
// grandchild that calls setsid leaves the group. So the wait is bounded, and what is left behind
// is said out loud rather than hidden.
const DrainAbandon = 2 * time.Second

// Process is one running instance of a Service.
//
// It owns the child, its pty, and the goroutines draining it. Everything a viewer needs is in
// Output; nothing here knows what a terminal looks like.
type Process struct {
	Service Service
	Output  *OutputBuffer

	// cmd is nil for an adopted process: after an exec-in-place the daemon keeps the same pid,
	// so the children are still its children and can still be waited on - but there is no
	// exec.Cmd to do it with, because the struct that held it died with the old binary.
	cmd        *exec.Cmd
	pid        int
	pty        *os.File
	started    time.Time
	ownsOutput bool

	// cgroup is this run's cgroup, nil when there is none. It is what makes Stop complete
	// rather than nearly complete.
	cgroup *Cgroup

	// reaped is set the instant wait() returns, which is when the pid stops being ours. It is
	// separate from exited, which is published later, after the output has been drained.
	reaped bool

	// stopMu serialises Stop. See the note there.
	stopMu sync.Mutex

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
	// LogDir is where each service's output is appended on disk. Empty means output is kept in
	// RAM only, which is what every test that does not care about logs gets.
	//
	// It lives on StartOptions rather than being derived inside the fabric because the daemon
	// owns where state goes, and a test must be able to put it under t.TempDir().
	LogDir string
	// LogBytes is the size at which a log file is rotated. Zero means DefaultLogBytes.
	LogBytes int64
	// Watchers, when set, is an existing watcher set for a replacement supervisor to inherit,
	// so that whoever is watching the *service* keeps being told about it. Like Output, the
	// lender still owns it.
	Watchers *StatusWatchers
	// Cgroups, when available, puts each process in its own cgroup so that stopping a service
	// stops everything it started - including a child that called setsid, which is the one case
	// a process-group kill cannot reach. Nil means process groups only.
	Cgroups *Cgroups
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

	// Put the child in its own cgroup, at fork rather than afterwards: a process that forks
	// before we get round to writing its pid leaves grandchildren outside, and those are exactly
	// the ones a cgroup exists to catch.
	//
	// The pty setup fills in the rest of SysProcAttr (setsid, the controlling terminal) on top
	// of whatever is here, so setting these first is safe.
	var group *Cgroup
	var groupDir *os.File
	if opts.Cgroups.Available() {
		g, gerr := opts.Cgroups.Create(s.Name)
		if gerr != nil {
			// Not fatal. A service that runs and can only be mostly stopped beats a service
			// that does not run - but it must not be a silent downgrade, so it is logged.
			logf("service %s: %v; falling back to a process-group kill", s.Name, gerr)
		} else {
			d, derr := g.Open()
			if derr != nil {
				logf("service %s: %v; falling back to a process-group kill", s.Name, derr)
				g.Remove()
			} else {
				group, groupDir = g, d
				cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(d.Fd())}
			}
		}
	}

	cols, rows := opts.Cols, opts.Rows
	if cols <= 0 {
		cols = defaultCols
	}
	if rows <= 0 {
		rows = defaultRows
	}

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil && group != nil {
		// clone3 is what places a child in a cgroup, and a seccomp policy that forbids it fails
		// here and nowhere else. Try once more without, and put the child in the cgroup after
		// the fact - a smaller guarantee, and much better than refusing to run the service.
		// The error may or may not be about the cgroup - a missing working directory fails here
		// too - so do not assert a cause we do not know. Say what was tried and what happened.
		logf("service %s: could not start it in a cgroup (%v); trying again without one", s.Name, err)
		cmd = exec.Command(path, s.Args...)
		cmd.Dir = s.Dir
		cmd.Env = append(os.Environ(), s.Env...)
		cmd.Env = append(cmd.Env, opts.ExtraEnv...)
		f, err = pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
		if err == nil {
			if aerr := group.Add(cmd.Process.Pid); aerr != nil {
				logf("service %s: %v; falling back to a process-group kill", s.Name, aerr)
				group.Remove()
				group = nil
			}
		}
	}
	if groupDir != nil {
		groupDir.Close()
	}
	if err != nil {
		if group != nil {
			group.Remove()
		}
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
		cgroup:     group,
		ownsOutput: ownsOutput,
		cmd:        cmd,
		pid:        cmd.Process.Pid,
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
	var exit Exit
	if p.cmd != nil {
		err := p.cmd.Wait()
		exit = exitFrom(err, p.cmd.ProcessState)
	} else {
		exit = waitAdopted(p.pid)
	}

	// Record the reap immediately. Between here and the exit being published below there is up
	// to DrainGrace+DrainAbandon of waiting, and for all of it the pid has already been freed by
	// the kernel and may have been handed to somebody else - so anything that signals by pid has
	// to know the child is gone before the waiting starts, not after.
	p.mu.Lock()
	p.reaped = true
	p.mu.Unlock()

	// The child is gone, but bytes it wrote may still be in flight. Give the reader a moment,
	// then close the pty - and then stop waiting for it, because closing is not guaranteed to
	// wake it. See DrainAbandon.
	select {
	case <-p.drained:
	case <-time.After(DrainGrace):
		p.closePTY()
		select {
		case <-p.drained:
		case <-time.After(DrainAbandon):
			// Say so in the service's own output, where somebody reading its logs will find
			// it, rather than only in the daemon's log where nobody is looking.
			fmt.Fprintf(p.Output, "\r\n[gozellij] %s: something is still holding this "+
				"service's terminal open, so its output is no longer being read\r\n",
				p.Service.Name)
			p.logf("gave up waiting for the pty reader after %s; something still holds the slave open", DrainAbandon)
		}
	}

	p.mu.Lock()
	p.exit = exit
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
func (p *Process) Pid() int { return p.pid }

// PTYFd is the file descriptor of the pty master.
//
// Exposed for exec-in-place: the descriptor has to survive into the new binary, which means
// clearing FD_CLOEXEC on it and telling the successor which number to pick up.
func (p *Process) PTYFd() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.pty == nil {
		return -1
	}
	return int(p.pty.Fd())
}

// Adopted reports whether this process was inherited across an exec rather than spawned here.
func (p *Process) Adopted() bool { return p.cmd == nil }

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
//
// An adopted process has no os.Process handle, so it is signalled by pid. That is safe here
// precisely because the pid is still ours: exec-in-place keeps it, so nobody else can have
// recycled it while we hold the parent slot.
func (p *Process) Signal(sig os.Signal) error {
	if p.pid <= 0 {
		return ErrProcessGone
	}
	if p.cmd != nil && p.cmd.Process != nil {
		if err := p.cmd.Process.Signal(sig); err != nil {
			if errors.Is(err, os.ErrProcessDone) {
				return ErrProcessGone
			}
			return fmt.Errorf("signalling %s: %w", p.Service.Name, err)
		}
		return nil
	}
	sysSig, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("signalling %s: %v is not a unix signal", p.Service.Name, sig)
	}
	if err := syscall.Kill(p.pid, sysSig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrProcessGone
		}
		return fmt.Errorf("signalling %s: %w", p.Service.Name, err)
	}
	return nil
}

// ErrNotGroupLeader means the child does not lead its own process group, so its group cannot be
// signalled without signalling processes that are not ours.
var ErrNotGroupLeader = errors.New("process does not lead its own process group")

// SignalGroup sends a signal to the child's whole process group.
//
// Stopping only the child is not stopping the service. A service is usually a shell, and a shell
// starts things: those children die when the pty is closed and they get SIGHUP, but one that
// ignores SIGHUP simply stays - measured, and it also wedged the pty reader, which is what made
// `stop` hang for ever.
//
// The guard is the important part. The child is a session leader (the pty setup calls setsid), so
// its process group id equals its pid, and killing -pid reaches exactly its descendants. If that
// is somehow not true, this refuses: a process group we do not lead may be *ours* - the daemon's
// own - and `kill(-pgid)` would take down the fabric and every other service with it. Refusing and
// falling back to the single pid is strictly better than being clever here.
func (p *Process) SignalGroup(sig syscall.Signal) error {
	if p.pid <= 0 {
		return ErrProcessGone
	}
	p.mu.Lock()
	reaped := p.reaped
	p.mu.Unlock()
	if reaped {
		// The pid has been returned to the kernel and may belong to somebody else by now.
		// Signalling "its" process group could reach an unrelated one.
		return ErrProcessGone
	}
	pgid, err := syscall.Getpgid(p.pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrProcessGone
		}
		return fmt.Errorf("finding the process group of %s: %w", p.Service.Name, err)
	}
	if pgid != p.pid {
		return ErrNotGroupLeader
	}
	if err := syscall.Kill(-pgid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrProcessGone
		}
		return fmt.Errorf("signalling the process group of %s: %w", p.Service.Name, err)
	}
	return nil
}

// stopSignal sends sig to the whole process group, falling back to the child alone.
func (p *Process) stopSignal(sig syscall.Signal) {
	err := p.SignalGroup(sig)
	if err == nil || errors.Is(err, ErrProcessGone) {
		return
	}
	if !errors.Is(err, ErrNotGroupLeader) {
		p.logf("%v to the process group failed: %v", sig, err)
	}
	if err := p.Signal(sig); err != nil && !errors.Is(err, ErrProcessGone) {
		// Nothing useful to do about it, but it should not vanish either.
		p.logf("%v failed: %v", sig, err)
	}
}

// Stop asks the process to exit: SIGTERM, then SIGKILL if it is still there after StopGrace.
//
// Both go to the whole process group, so stopping a service stops what the service started. A
// grandchild that has called setsid is in a different session and survives this; that needs a
// cgroup, and is the reason the systemd unit asks for one.
//
// It returns how the process ended. A process that ignores SIGTERM is not an error - it is the
// normal case for a few programs - but the caller can tell the difference, because a killed
// process reports its signal.
func (p *Process) Stop() Exit {
	// One stop at a time. Two callers run this concurrently as a matter of course - the
	// supervisor nudges the process while its own loop also stops it on cancellation - and
	// without this they interleave in a way that defeats the whole grace period: the second
	// caller finds the leader already reaped, takes the early path below, and sweeps the cgroup
	// at once, killing children the first caller was in the middle of waiting for. Measured: a
	// child given half a second to shut down got sixteen milliseconds.
	//
	// It also means a service is never sent two SIGTERMs microseconds apart by two goroutines,
	// which for a runtime that treats the second as "stop arguing and quit" is the difference
	// between a clean shutdown and a forced one.
	p.stopMu.Lock()
	defer p.stopMu.Unlock()

	if _, done := p.Exited(); done {
		// The leader is gone, but what it started may not be. Ask, wait, and only then sweep -
		// the same courtesy the main path gives, because "the process you named has already
		// exited" is not a reason to kill its children without warning.
		deadline := time.Now().Add(StopGrace)
		// Everything, with no skipping: on this path the process group was never signalled,
		// because the leader has already been reaped and signalling a freed pid's group is not
		// safe. Skipping the leader's group here meant children that stayed in it got no
		// SIGTERM at all - they sat out the whole grace period and were then SIGKILLed, which
		// is the opposite of what this path was added to do.
		p.signalCgroup(syscall.SIGTERM, false)
		p.waitCgroupEmpty(deadline)
		p.sweepCgroup()
		return p.Wait()
	}

	// One deadline for the whole stop, not one per participant. The grace period belongs to the
	// *service*, and a second version of this gave it entirely to the leader: it waited for the
	// leader to be reaped and then killed the cgroup at once, so a child still flushing when its
	// parent exited was SIGKILLed thirteen milliseconds into a five second grace. That is worse
	// than the process-group version it replaced, which at least left the children alone.
	deadline := time.Now().Add(StopGrace)

	// SIGTERM twice over: to the process group, which is everything that stayed in it, and
	// individually to anything in the cgroup that left the group. cgroup v2 has no "signal this
	// cgroup" - only cgroup.kill, which is SIGKILL - so a graceful stop for a process that
	// called setsid has to be addressed to it by pid.
	p.stopSignal(syscall.SIGTERM)
	p.signalCgroup(syscall.SIGTERM, true)

	select {
	case <-p.done:
	case <-time.After(time.Until(deadline)):
		p.stopSignal(syscall.SIGKILL)
	}

	// The leader is gone; the rest of the service may not be. Give it what is left of the grace
	// period to finish on its own before the cgroup is killed.
	p.waitCgroupEmpty(deadline)

	// Then always sweep, and this is the line the first version got wrong: it killed the cgroup
	// only after the grace period expired, so a service whose main process exited promptly
	// returned early and never swept at all, and the setsid'd child this whole mechanism exists
	// for survived every well-behaved stop.
	p.sweepCgroup()
	return p.Wait()
}

// waitCgroupEmpty waits until nothing is left in the cgroup, or until the deadline.
func (p *Process) waitCgroupEmpty(deadline time.Time) {
	if p.cgroup == nil {
		return
	}
	for {
		pids, err := p.cgroup.Pids()
		if err != nil || len(pids) == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// signalCgroup sends a signal to every process in the cgroup, individually.
//
// groupSignalled says whether the leader's process group has already been sent this signal. When
// it has, members of that group are skipped: a second SIGTERM a hundred microseconds after the
// first is harmless to a shell, whose handler has not run yet so the signals coalesce, but a
// runtime with asynchronous handlers gets both and "a second SIGTERM means stop arguing and quit"
// is a common pattern. When it has not, skipping them means never signalling them.
func (p *Process) signalCgroup(sig syscall.Signal, groupSignalled bool) {
	if p.cgroup == nil {
		return
	}
	pids, err := p.cgroup.Pids()
	if err != nil {
		p.logf("could not list the cgroup: %v", err)
		return
	}
	for _, pid := range pids {
		if groupSignalled {
			if pgid, err := syscall.Getpgid(pid); err == nil && pgid == p.pid {
				continue
			}
		}
		if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			p.logf("signalling %d in the cgroup: %v", pid, err)
		}
	}
}

// removeCgroupWithRetry deletes the cgroup directory, retrying while the kernel finishes reaping.
func removeCgroupWithRetry(g *Cgroup) error {
	const within = 2 * time.Second
	deadline := time.Now().Add(within)
	for {
		err := g.Remove()
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sweepCgroup kills whatever is left in the cgroup. It is a no-op when there is no cgroup, and
// when the cgroup is already empty.
func (p *Process) sweepCgroup() {
	if p.cgroup == nil {
		return
	}
	if err := p.cgroup.Kill(); err != nil {
		p.logf("cgroup kill failed: %v", err)
	}
}

// Cgroup is this process's cgroup, nil when it has none.
func (p *Process) Cgroup() *Cgroup { return p.cgroup }

// Close releases the process's resources. It does not stop the child; use Stop for that.
//
// A borrowed output buffer (StartOptions.Output) is left open: it belongs to whoever lent it, and
// closing it here would cut off every viewer the moment one process restarted.
func (p *Process) Close() {
	p.closePTY()
	if p.cgroup != nil {
		// A cgroup directory can only be removed once it is empty, and cgroup.kill is
		// asynchronous: a task that has been SIGKILLed is still listed until it finishes
		// exiting, so an immediate rmdir gets EBUSY. Retrying briefly is the difference between
		// a clean sweep and one dead directory per stop for the life of the daemon - measured
		// at five out of five stops before this loop existed.
		//
		// If it still will not go, say so once. Each run gets a fresh name, so a leftover
		// blocks nothing, and a directory that outlives its service is worth a line.
		// In the background, because Close is on the path that publishes a service's exit and
		// this can take seconds. A service that exits leaving a detached child behind has a
		// cgroup that will never empty - deliberately, since nobody asked for that child to be
		// killed - and waiting two seconds for an rmdir that cannot succeed made `exit` in a
		// shell with a nohup'd job take two seconds to come back.
		go func(g *Cgroup, name string) {
			if err := removeCgroupWithRetry(g); err != nil {
				logf("%s: could not remove the cgroup, so something it started is still "+
					"running: %v", name, err)
			}
		}(p.cgroup, p.Service.Name)
	}
	if p.ownsOutput {
		p.Output.Close()
	}
}

// logf is a placeholder for the fabric's logger, which arrives with the daemon. It deliberately
// goes to stderr rather than nowhere: a swallowed diagnostic is how the last project lost an
// afternoon.
func (p *Process) logf(format string, args ...any) {
	logf("%s: "+format, append([]any{p.Service.Name}, args...)...)
}

// logf is the same, for the places that have no Process yet - notably Start, which has to report
// a cgroup it could not create before there is anything to hang the message on.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gozellij: "+format+"\n", args...)
}

// interface checks
var (
	_ io.Writer = (*Process)(nil)
)
