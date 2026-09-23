package daemon

import (
	"os"
	"path/filepath"
)

// The first time somebody attaches, they are told how to leave.
//
// This was missing and it is the worst thing to have been missing. A multiplexer takes over your
// terminal; the first thing it owes you is the key that gives it back. The status line said which
// service you were looking at and how much memory the machine had, and nothing about the fact that
// Ctrl-] is a key at all - so the only way to find out was to read the README, which you cannot
// do, because you are inside the thing you are trying to leave.
//
// Once, not every time. A hint that appears on every attach is noise by the third day, and the
// permanent part of the answer is the `keys` widget in the status line, which is four characters
// and always there.

// firstRunMarker is the file whose existence means "this person has been shown the keys".
//
// In the state directory rather than the runtime one: it has to survive a reboot, or the first
// login after every restart is somebody's first time again.
func firstRunMarker() string { return filepath.Join(StateDir(), "seen-the-keys") }

// FirstAttach reports whether this is the first attach on this machine, and records that it is not
// any more.
//
// The recording is best-effort on purpose. If the state directory cannot be written - read-only
// home, full disk - the right outcome is to greet somebody twice, not to refuse to attach or to
// stay silent about how to get out.
func FirstAttach() bool {
	path := firstRunMarker()
	if _, err := os.Stat(path); err == nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return true
	}
	if err := os.WriteFile(path, []byte("The keys have been shown once. Delete this to see them again.\n"), 0o600); err != nil {
		return true
	}
	return true
}

// firstRunGreeting is what a first-time user is told, in one line, because it lands on a status
// line that is one line.
func firstRunGreeting(label string) string {
	return "welcome - this is gozellij. " + prefixHelp(label)
}
