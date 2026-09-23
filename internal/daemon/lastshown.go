package daemon

import (
	"os"
	"path/filepath"
	"strings"
)

// Which service a terminal was last showing, for `gozellij shell -last`.
//
// A login lands in the shell called "shell" unless told otherwise. Once shells can be renamed, the
// one you were working in may not be called that any more, and whether the next login should
// follow it is a choice - so it is a flag, and the default stays what it was. This is what the
// flag reads.
//
// Written every time an attach starts showing something - the first attach and every switch - and
// not only when it ends, because the way people leave is closing the window, which ends the client
// with a signal and runs nothing on the way out.

func lastShownPath() string { return filepath.Join(StateDir(), "last-shown") }

// RememberShown records that a terminal is now showing service. Failures are ignored: this is a
// convenience for the next login, and not worth failing an attach over.
func RememberShown(service string) {
	if service == "" {
		return
	}
	dir := StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".last-shown.*")
	if err != nil {
		return
	}
	_, werr := tmp.WriteString(service + "\n")
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), lastShownPath()); err != nil {
		os.Remove(tmp.Name())
	}
}

// followRename moves the record along when the service it names is renamed - and only then, so a
// rename seen by one terminal does not overwrite a switch made since in another.
func followRename(from, to string) {
	if LastShown() == from {
		RememberShown(to)
	}
}

// LastShown is the service a terminal was last showing, or "".
func LastShown() string {
	b, err := os.ReadFile(lastShownPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
