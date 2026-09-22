package fabric

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Adopting a process that is no longer ours.
//
// Adopt() handles the upgrade case, where `syscall.Exec` kept the pid and the children are still
// children. A daemon that *crashed* is a different matter: the replacement has a new pid, the
// services were reparented the moment the old one died, and nothing about them is ours any more.
// The pty master can be recovered - systemd's file-descriptor store holds it - but the process on
// the other end cannot be waited for.
//
// Measured, with a transient user unit, before any of this was written; the transcript is in
// docs/USER_STORIES.md C3:
//
//   - pidfd_open on a process that is not your child works, and poll on it returns POLLIN the
//     moment that process exits. So "it ended" is available.
//   - wait4 from a non-parent fails with ECHILD. So "with what code" is not.
//
// This is the whole difference between the two kinds of adoption, and it is a real loss rather
// than a detail to paper over: a service adopted after a crash reports an exit nobody can describe.
// Saying so is the only honest option; inventing a zero would make a crash look like a clean exit
// and stop `-restart on-failure` doing its job.

// AdoptOrphan takes over a process this daemon did not start and is not the parent of.
//
// Everything Adopt does, except that the exit is watched with a pidfd instead of waited for.
func AdoptOrphan(s Service, pid int, ptyFD int, started time.Time, opts StartOptions) (*Process, error) {
	p, err := Adopt(s, pid, ptyFD, started, opts)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.orphan = true
	p.mu.Unlock()
	return p, nil
}

// UnknownExit is what an adopted orphan's ending looks like.
//
// Not clean, deliberately. `-restart on-failure` has to decide something, and the two mistakes are
// not equal: restarting a service that had exited on purpose costs a process nobody wanted, while
// not restarting one that crashed costs the thing this program exists to provide. So an ending
// nobody could see is treated as a failure, and `status` says as much rather than printing a code.
var UnknownExit = Exit{Code: -1, Signal: unknownExitSignal}

// unknownExitSignal is the marker in the Signal field. A word rather than a code, because every
// consumer of Exit already prints Signal as text and would have printed "-1" as an exit status.
const unknownExitSignal = "ended while adopted, so how is not known"

// IsUnknown reports whether this exit is one nobody was in a position to observe.
func (e Exit) IsUnknown() bool { return e.Signal == unknownExitSignal }

// waitOrphan blocks until a process that is not our child exits.
//
// pidfd_open gives a descriptor that becomes readable when the process ends, and it works for any
// process this one is allowed to signal. There is no exit status at the end of it: the status went
// to whoever inherited the child, which after a daemon crash is the service manager.
func waitOrphan(pid int) Exit {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		// Already gone, or not ours to watch. Either way there is nothing to wait for, and
		// saying it ended is the truth.
		logf("cannot watch adopted process %d (%v); treating it as ended", pid, err)
		return UnknownExit
	}
	defer unix.Close(fd)

	for {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, perr := unix.Poll(fds, orphanPollInterval)
		if perr != nil {
			if perr == unix.EINTR {
				continue
			}
			logf("watching adopted process %d failed (%v); treating it as ended", pid, perr)
			return UnknownExit
		}
		if n > 0 {
			return UnknownExit
		}
		// A timeout rather than an infinite wait, so that a pidfd which somehow never fires is
		// still noticed: if the process is gone, stop waiting for it.
		if err := syscall.Kill(pid, 0); err != nil {
			return UnknownExit
		}
	}
}

// orphanPollInterval is how long each wait for an adopted process lasts before checking by hand
// that it is still there. Long enough to cost nothing, short enough that a pidfd that never fires
// does not leave a dead service reported as running for ever.
const orphanPollInterval = 30_000 // milliseconds
