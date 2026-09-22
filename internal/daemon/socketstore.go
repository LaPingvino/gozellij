package daemon

import (
	"log/slog"
	"net"
	"os"
)

// Keeping the listening socket across a restart.
//
// The first thing the file-descriptor store is used for, and the smallest: not a service's pty but
// the daemon's own socket. It is worth doing on its own because it is the whole path in miniature -
// hand a descriptor over, get it back on the next start, check it is what it claims to be - and
// because of what it buys. `systemctl --user restart gozellijd` currently has a window where the
// socket does not exist and every `gozellij` command says "no daemon is running"; with the socket
// held by systemd the connections queue in its backlog instead and nobody notices the gap.
//
// The pty masters are the point of all this and they are the next piece. This one carries no risk
// of losing a process if it goes wrong.

// socketFDName is what the listening socket is stored under. Short, and it cannot collide with a
// service's own name because of the prefix every one of those carries.
const socketFDName = "gz-socket"

// adoptListener takes the listening socket back from systemd, if it handed one over and it is
// really the socket this daemon is meant to serve.
//
// Anything unexpected means starting clean rather than guessing: a descriptor that is not a unix
// listener, or is one for a different path, is closed and ignored. Serving the wrong socket would
// be a daemon nobody can reach, which is worse than a restart that drops a few connections.
func adoptListener(path string, log *slog.Logger) (net.Listener, bool) {
	CollectStoredFDs()
	f, ok := pendingFDs[socketFDName]
	if !ok {
		return nil, false
	}
	claimFD(socketFDName)
	defer f.Close()

	ln, err := net.FileListener(f)
	if err != nil {
		log.Warn("systemd handed back something that is not a listening socket; starting clean",
			"name", socketFDName, "err", err)
		return nil, false
	}
	ua, isUnix := ln.Addr().(*net.UnixAddr)
	if !isUnix || ua.Name != path {
		log.Warn("the socket systemd handed back is for a different path; starting clean",
			"handed", ln.Addr().String(), "want", path)
		ln.Close()
		return nil, false
	}
	log.Info("took the listening socket back from systemd", "path", path)
	return ln, true
}

// storeListener hands the socket to systemd to hold for the next start. Nothing here is fatal: a
// daemon that cannot use the store is the daemon we had before it existed.
func storeListener(ln net.Listener, log *slog.Logger) {
	ul, ok := ln.(*net.UnixListener)
	if !ok {
		return
	}
	// File() returns a duplicate, so this one is ours to close. It also puts *that* descriptor in
	// blocking mode, which is right for something that is only going to be handed across a socket.
	f, err := ul.File()
	if err != nil {
		log.Debug("could not duplicate the listening socket for systemd", "err", err)
		return
	}
	defer f.Close()
	if err := StoreFD(socketFDName, f); err != nil && err != ErrNoNotifySocket {
		log.Debug("systemd would not hold the listening socket", "err", err)
	}
}

// pendingFDs is what systemd handed back that nothing has adopted yet.
//
// Held rather than closed, because the descriptors of services are coming through here next and a
// function whose job is the socket must not decide their fate. Closed by ReleaseUnadoptedFDs once
// everything that wants one has had its chance.
var pendingFDs = map[string]*os.File{}

// collected guards the one-shot read of the environment. sd_listen_fds is a once-per-process
// thing - it clears LISTEN_FDS on the way out - so calling it twice would find nothing the second
// time and quietly drop every descriptor systemd handed back.
var collected bool

// CollectStoredFDs reads what systemd handed over and holds it until something claims it.
//
// Called once, early, before anything that wants a descriptor. Separate from adopting because the
// two claimants are in different places and start at different times: the fabric wants the
// services' terminals before it loads anything, and the server wants the listening socket after
// that.
func CollectStoredFDs() {
	if collected {
		return
	}
	collected = true
	for name, f := range StoredFDs() {
		keepForLater(name, f)
	}
}

func keepForLater(name string, f *os.File) { pendingFDs[name] = f }

// claimFD says somebody has taken responsibility for a descriptor, so ReleaseUnadoptedFDs must not
// close it. The file itself stays open and is now the claimant's.
func claimFD(name string) { delete(pendingFDs, name) }

// releaseFD closes one nobody wants and tells systemd to stop holding it.
func releaseFD(log *slog.Logger, name string) {
	f, ok := pendingFDs[name]
	if !ok {
		return
	}
	_ = f.Close()
	delete(pendingFDs, name)
	if err := RemoveStoredFD(name); err != nil && err != ErrNoNotifySocket {
		log.Debug("could not tell systemd to drop it", "name", name, "err", err)
	}
}

// PendingFDs is what came back from systemd and has not been adopted.
func PendingFDs() map[string]*os.File {
	out := make(map[string]*os.File, len(pendingFDs))
	for k, v := range pendingFDs {
		out[k] = v
	}
	return out
}

// ReleaseUnadoptedFDs closes what nobody claimed and tells systemd to stop holding it.
//
// Rule 1 with descriptors: a store quietly accumulating things nothing will ever use is a leak that
// looks like a feature. Called once the daemon has finished adopting.
func ReleaseUnadoptedFDs(log *slog.Logger) {
	for name, f := range pendingFDs {
		log.Info("nothing claimed a descriptor systemd was holding; dropping it", "name", name)
		_ = f.Close()
		if err := RemoveStoredFD(name); err != nil && err != ErrNoNotifySocket {
			log.Debug("could not tell systemd to drop it", "name", name, "err", err)
		}
		delete(pendingFDs, name)
	}
}
