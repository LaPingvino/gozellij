package fabric

import (
	"fmt"
	"github.com/creack/pty"
	"os"
	"syscall"
	"time"
)

// Adopt takes over a process that is already running, with its pty, without restarting it.
//
// This is what makes a daemon upgrade invisible to the things it is running. `syscall.Exec`
// replaces the daemon's image while keeping its pid, so:
//
//   - the children are still *our* children, and can still be waited on;
//   - the pty master descriptors survive, if FD_CLOEXEC was cleared on them first;
//
// and the process on the other end of the pty never notices that the program supervising it was
// replaced. It does not see a hangup, because the master was never closed.
//
// What is lost is the exec.Cmd, which died with the old binary's memory - hence waitAdopted below.
func Adopt(s Service, pid int, ptyFD int, started time.Time, opts StartOptions) (*Process, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if pid <= 0 {
		return nil, fmt.Errorf("adopting %s: invalid pid %d", s.Name, pid)
	}
	if ptyFD < 0 {
		return nil, fmt.Errorf("adopting %s: invalid pty descriptor %d", s.Name, ptyFD)
	}

	// Check the process is actually there before claiming it. Adopting a pid that has already
	// gone would leave a supervisor reporting a running service that does not exist, which is a
	// worse lie than admitting the handover lost one.
	if err := syscall.Kill(pid, 0); err != nil {
		return nil, fmt.Errorf("adopting %s: process %d is not there: %w", s.Name, pid, err)
	}

	out := opts.Output
	ownsOutput := false
	if out == nil {
		out = NewOutputBuffer(opts.OutputBytes)
		ownsOutput = true
	}

	f := os.NewFile(uintptr(ptyFD), "pty-"+s.Name)
	if f == nil {
		return nil, fmt.Errorf("adopting %s: descriptor %d is not usable", s.Name, ptyFD)
	}

	if started.IsZero() {
		started = time.Now()
	}

	// Find the cgroup the process is already in, rather than making a new one it is not in.
	// Without this an upgraded daemon keeps every process and quietly loses the ability to stop
	// them completely - the promise would still be printed in the docs and no longer be true.
	var group *Cgroup
	if opts.Cgroups.Available() {
		g, gerr := opts.Cgroups.Adopt(pid)
		if gerr != nil {
			logf("%s: could not reclaim the cgroup of process %d (%v); "+
				"stopping it will fall back to a process-group kill", s.Name, pid, gerr)
		} else {
			group = g
		}
	}

	// The size it already has: the pty kept it across the exec.
	if ws, werr := pty.GetsizeFull(f); werr == nil {
		out.SetSize(int(ws.Cols), int(ws.Rows))
	}

	p := &Process{
		Service:    s,
		Output:     out,
		cgroup:     group,
		ownsOutput: ownsOutput,
		cmd:        nil, // adopted: see the note on Process.cmd
		pid:        pid,
		pty:        f,
		started:    started,
		done:       make(chan struct{}),
		drained:    make(chan struct{}),
	}

	go p.drain()
	go p.reap()

	return p, nil
}

// waitAdopted waits for a child we inherited across an exec and reports how it ended.
//
// os/exec is not available here - there is no Cmd, it died with the old binary's memory - so this
// is Wait4 directly. It is only correct because exec-in-place preserved the pid: we are still the
// parent, so the child is still ours to reap and nothing else can take it.
//
// It returns an Exit rather than an *os.ProcessState because that type cannot be constructed
// outside os/exec, and inventing one would mean lying about where the information came from.
func waitAdopted(pid int) Exit {
	for {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &ws, 0, nil)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			// ECHILD: already reaped, or never ours. Nothing left to wait for. Say which,
			// rather than reporting a clean exit for a process we never saw end.
			return Exit{Code: -1, Signal: fmt.Sprintf("unknown: waiting for adopted process %d: %v", pid, err)}
		}
		if wpid != pid {
			continue
		}
		if ws.Signaled() {
			return Exit{Code: -1, Signal: ws.Signal().String()}
		}
		return Exit{Code: ws.ExitStatus()}
	}
}
