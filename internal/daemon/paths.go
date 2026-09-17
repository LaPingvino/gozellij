// Package daemon serves a fabric over a unix socket.
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
)

// SocketName is the socket inside the runtime directory.
const SocketName = "fabric.sock"

// maxSocketPath is the practical limit on a unix socket path.
//
// sockaddr_un.sun_path is 108 bytes on Linux and 104 on macOS/BSD, including the terminating NUL.
// Exceeding it fails deep inside bind() with a message that does not mention path length at all,
// so we check it ourselves and say what is wrong. This cost an afternoon in the previous project.
const maxSocketPath = 100

// RuntimeDir is where the socket lives: $XDG_RUNTIME_DIR/gozellij, or a fallback under /tmp keyed
// by uid when the runtime dir is not set (some ssh and cron environments).
func RuntimeDir() string {
	if d := os.Getenv("GOZELLIJ_RUNTIME_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "gozellij")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("gozellij-%d", os.Getuid()))
}

// StateDir is where service definitions live. This is state the fabric must not lose, so it goes
// somewhere that survives a reboot - unlike the runtime directory, which does not.
func StateDir() string {
	if d := os.Getenv("GOZELLIJ_STATE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "gozellij")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "gozellij")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("gozellij-state-%d", os.Getuid()))
}

// SocketPath is the full path to the daemon socket.
func SocketPath() string { return filepath.Join(RuntimeDir(), SocketName) }

// CheckSocketPath reports whether a socket path is usable, with an error that says what to do
// about it rather than leaving the caller with a bare EINVAL from bind().
func CheckSocketPath(path string) error {
	if len(path) >= maxSocketPath {
		return fmt.Errorf("socket path is %d bytes, which is too long for a unix socket (limit ~%d): %s\n"+
			"set GOZELLIJ_RUNTIME_DIR to something shorter", len(path), maxSocketPath, path)
	}
	return nil
}
