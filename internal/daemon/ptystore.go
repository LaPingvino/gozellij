package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/LaPingvino/gozellij/internal/fabric"
)

// Keeping the services alive across a daemon crash.
//
// This is what docs/USER_STORIES.md C3 is about and the last piece of it. The daemon hands each
// service's pty master to systemd, which holds it; when the daemon is killed and restarted, the
// descriptors come back and the processes on the other end of them never noticed. They kept
// running the whole time - they were only ever children of the daemon, not part of it - and what
// used to be lost was the terminal they were talking to, which is what made them unrecoverable.
//
// Three things about this are measured rather than assumed, and all three are in C3: the store
// works for a user unit, the unit must carry KillMode=process or systemd kills the services on
// the very restart this exists to survive, and an adopted process can be watched but not waited
// for - so its eventual exit arrives without a code.

// ptyFDPrefix marks a stored descriptor as a service's terminal, so that the socket and anything
// stored later are told apart without guessing.
const ptyFDPrefix = "gzpty-"

// ptyFDName is what a service's terminal is stored under: the prefix, the pid, then the name.
//
// The pid is in there because a descriptor on its own says nothing about what is on the other end
// of it, and adopting one means claiming a specific process. The name comes last and takes the
// rest, because a service name may contain a dash and a pid may not.
func ptyFDName(service string, pid int) string {
	return fmt.Sprintf("%s%d-%s", ptyFDPrefix, pid, service)
}

// parsePTYFDName reads a name back. Anything it cannot read is not ours.
func parsePTYFDName(name string) (service string, pid int, ok bool) {
	rest, found := strings.CutPrefix(name, ptyFDPrefix)
	if !found {
		return "", 0, false
	}
	pidStr, service, found := strings.Cut(rest, "-")
	if !found || service == "" {
		return "", 0, false
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return "", 0, false
	}
	return service, pid, true
}

// storePTY hands a service's terminal to systemd to hold.
//
// Nothing here is fatal. A daemon that cannot use the store is the daemon this was before the
// store existed, and a service that runs without a safety net is much better than one that will
// not start because the net could not be strung up.
func StorePTY(log *slog.Logger, service string, pid int, pty *os.File) {
	if pty == nil {
		return
	}
	name := ptyFDName(service, pid)
	switch err := StoreFD(name, pty); {
	case err == nil:
		log.Debug("systemd is holding this service's terminal", "service", service, "pid", pid)
	case err == ErrNoNotifySocket:
		// Not started by systemd. Said once at startup rather than per service; see noteNoStore.
	default:
		log.Warn("systemd would not hold this service's terminal, so it will not survive a crash",
			"service", service, "pid", pid, "err", err)
	}
}

// dropPTY tells systemd to stop holding a terminal whose process has ended.
//
// Rule 1 applied to descriptors: a store that only ever grows fills with terminals of things that
// no longer exist, and the next daemon would try to adopt every one of them.
func DropPTY(log *slog.Logger, service string, pid int) {
	if err := RemoveStoredFD(ptyFDName(service, pid)); err != nil && err != ErrNoNotifySocket {
		log.Debug("could not tell systemd to drop a terminal", "service", service, "pid", pid, "err", err)
	}
}

// RecoveredPTYs turns what systemd handed back into handovers the fabric can adopt.
//
// Everything it refuses, it says why. A descriptor whose process has gone is the ordinary case
// after a service exited during the gap, and adopting it would leave a supervisor reporting a
// service that does not exist - which is a worse lie than admitting one was lost.
func RecoveredPTYs(log *slog.Logger) []fabric.Handover {
	var out []fabric.Handover
	for name, f := range PendingFDs() {
		service, pid, ok := parsePTYFDName(name)
		if !ok {
			continue
		}
		if err := syscall.Kill(pid, 0); err != nil {
			log.Info("a service's terminal came back but the service did not; starting it fresh",
				"service", service, "pid", pid, "err", err)
			releaseFD(log, name)
			continue
		}
		// Duplicated, not borrowed. The number alone was handed over here, while the *os.File
		// that carried it from systemd still owned it - so once nothing referenced that File any
		// more, Go's finaliser closed the descriptor underneath the running service. It showed up
		// an hour later as "cannot keep pty fd 4 across exec: bad file descriptor" on the next
		// reload, which dropped every service from the handover. A dup gives the fabric a
		// descriptor of its own and lets this one go safely.
		dup, derr := syscall.Dup(int(f.Fd()))
		if derr != nil {
			log.Warn("could not take a copy of a recovered terminal; the service will be started fresh",
				"service", service, "pid", pid, "err", derr)
			releaseFD(log, name)
			continue
		}
		_ = f.Close()
		out = append(out, fabric.Handover{
			Name:  service,
			Pid:   pid,
			PTYFd: dup,
			// The real one, from the kernel. This used to be time.Now() with a comment saying
			// the error would be seconds; it is the whole time since the crash and it grows.
			// A shell running since yesterday reported twenty minutes of uptime, because that
			// is when the daemon came back. Now only when /proc cannot say.
			StartedAt: startOf(pid),
			Orphan:    true,
		})
		claimFD(name)
		log.Info("taking a service's terminal back from systemd", "service", service, "pid", pid)
	}
	return out
}

// startOf is when a process began, falling back to now when the kernel cannot be asked - which
// keeps a recovered service's uptime honest instead of restarting it at every crash.
func startOf(pid int) time.Time {
	if t, ok := fabric.ProcessStart(pid); ok {
		return t
	}
	return time.Now()
}

// NoteIfNothingIsHolding says once, at startup, that nothing is holding the terminals - rather than once per
// service, and rather than not at all. A promise this program makes in its own documentation is
// not being kept on this machine, and silence about that is the failure this project is built to
// avoid.
func NoteIfNothingIsHolding(log *slog.Logger, services int) {
	if services == 0 || os.Getenv(NotifyEnv) != "" {
		return
	}
	log.Info("nothing is holding the services' terminals, so a daemon crash will still take them with it; "+
		"run the daemon from the systemd user unit in packaging/ to change that",
		"services", services)
}
