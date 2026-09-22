package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Talking to systemd, in about a hundred lines and no cgo.
//
// The protocol is a datagram to the socket named by $NOTIFY_SOCKET carrying newline-separated
// key=value lines, with any descriptors attached as SCM_RIGHTS. That is the whole of it, and it is
// why there is no dependency here: libsystemd would be a cgo build for a write to a unix socket.
//
// What this is for is docs/USER_STORIES.md C3. A daemon that crashes currently takes every service
// with it, because the pty masters die with the process holding them. Handing them to systemd's
// file-descriptor store means the successor can pick them back up - measured, in that document,
// along with the two things about it that are not obvious.

// NotifyEnv names the socket systemd is listening on. Absent when nothing started us that way,
// which is the ordinary case for a daemon run from a shell.
const NotifyEnv = "NOTIFY_SOCKET"

// ErrNoNotifySocket means nothing is listening, which is not a failure: it is what running outside
// systemd looks like, and every caller here treats it as "there is nowhere to put this".
var ErrNoNotifySocket = errors.New("no NOTIFY_SOCKET in the environment")

// Notify sends one message to the service manager, with descriptors attached if any are given.
//
// The socket is opened unconnected and the destination given per message. A connected socket looks
// like the obvious choice and does not work: Go routes WriteMsgUnix through WriteTo, which a
// connected socket refuses with "use of WriteTo with pre-connected connection". That cost a probe
// run to find.
func Notify(state string, fds ...*os.File) error {
	addr := os.Getenv(NotifyEnv)
	if addr == "" {
		return ErrNoNotifySocket
	}
	// A leading @ is systemd's spelling of an abstract socket, whose name starts with a NUL.
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + addr[1:]
	}
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("opening a socket to talk to systemd: %w", err)
	}
	defer c.Close()

	var oob []byte
	if len(fds) > 0 {
		nums := make([]int, 0, len(fds))
		for _, f := range fds {
			if f == nil {
				return errors.New("a nil file cannot be sent to systemd")
			}
			nums = append(nums, int(f.Fd()))
		}
		oob = syscall.UnixRights(nums...)
	}
	if _, _, err := c.WriteMsgUnix([]byte(state), oob, &net.UnixAddr{Name: addr, Net: "unixgram"}); err != nil {
		return fmt.Errorf("telling systemd %q: %w", firstLine(state), err)
	}
	return nil
}

// NotifyReady says the daemon is up. Needed by Type=notify, which is what the unit has to be for
// the file-descriptor store to be usable.
func NotifyReady() error { return Notify("READY=1") }

// StoreFD hands a descriptor to systemd to hold for the next start.
//
// The name is how it comes back: a descriptor on its own does not say whose it is. systemd reserves
// ':' in a name, because that is what it joins them with in LISTEN_FDNAMES, and caps a name at 255
// bytes - both checked here rather than discovered as a name that silently did not arrive.
func StoreFD(name string, f *os.File) error {
	if err := checkFDName(name); err != nil {
		return err
	}
	return Notify("FDSTORE=1\nFDNAME="+name, f)
}

// RemoveStoredFD tells systemd to drop what it is holding under this name, for a service that has
// stopped on purpose. Without it the store fills up with descriptors of things nobody wants back.
func RemoveStoredFD(name string) error {
	if err := checkFDName(name); err != nil {
		return err
	}
	return Notify("FDSTOREREMOVE=1\nFDNAME=" + name)
}

// maxFDName is systemd's limit on one name in LISTEN_FDNAMES.
const maxFDName = 255

func checkFDName(name string) error {
	switch {
	case name == "":
		return errors.New("a stored descriptor needs a name, or nothing can tell which it is")
	case len(name) > maxFDName:
		return fmt.Errorf("the name %q is %d bytes; systemd's limit is %d", name, len(name), maxFDName)
	case strings.ContainsAny(name, ":\n\x00"):
		return fmt.Errorf("the name %q contains a character systemd uses to separate names", name)
	}
	return nil
}

// StoredFDs is what systemd handed back at startup, by the name it was stored under.
//
// Empty unless this process was started by systemd with something in the store, which is the
// ordinary case. The environment is cleared afterwards so that a child of this process does not
// inherit a claim on descriptors it does not have - the same reason sd_listen_fds unsets them.
func StoredFDs() map[string]*os.File {
	defer func() {
		_ = os.Unsetenv("LISTEN_PID")
		_ = os.Unsetenv("LISTEN_FDS")
		_ = os.Unsetenv("LISTEN_FDNAMES")
	}()

	// The pid check is the point of LISTEN_PID: these variables are inherited, so without it a
	// child would believe it owns descriptors that belong to its parent.
	if pid, err := strconv.Atoi(os.Getenv("LISTEN_PID")); err != nil || pid != os.Getpid() {
		return nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || n <= 0 {
		return nil
	}
	names := strings.Split(os.Getenv("LISTEN_FDNAMES"), ":")

	out := make(map[string]*os.File, n)
	for i := range n {
		// systemd's descriptors start at 3, immediately after standard input, output and error.
		fd := listenFDsStart + i
		name := ""
		if i < len(names) {
			name = names[i]
		}
		if name == "" || name == "unknown" {
			// systemd's placeholder for a descriptor stored without a name. Kept rather than
			// dropped, under something that cannot collide with a service name, because an
			// unnamed descriptor is still a descriptor somebody has to close.
			name = fmt.Sprintf("unnamed-%d", fd)
		}
		syscall.CloseOnExec(fd)
		out[name] = os.NewFile(uintptr(fd), name)
	}
	return out
}

// listenFDsStart is SD_LISTEN_FDS_START: the first descriptor systemd passes.
//
// A variable rather than a constant so that a test can point it somewhere harmless. The first
// version of the test for this put its own descriptors at 3 and 4 with dup2 - in a process shared
// with every other test in the package - and closed the daemon's listening socket out from under
// eight of them. Descriptor numbers are process-wide state, which is exactly the kind of thing
// that makes a test suite fail somewhere other than where the mistake is.
var listenFDsStart = 3

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
