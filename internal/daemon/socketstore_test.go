package daemon

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Adopting the listening socket, checked without systemd.
//
// A test cannot make systemd crash a unit and hand the descriptor back, so the handover itself is
// verified by hand against a transient user unit - docs/USER_STORIES.md C3 has the transcript.
// What is checked here is the half that is this program's: that a socket placed where systemd
// would place it is picked up and served, and that anything else is refused rather than served
// wrongly. A daemon serving the wrong socket is a daemon nobody can reach.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// handOver places a listening socket where systemd would have put it.
func handOver(t *testing.T, ln *net.UnixListener, name string) {
	t.Helper()
	f, err := ln.File()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	placeFDs(t, f)
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", "1")
	t.Setenv("LISTEN_FDNAMES", name)
}

func listenSomewhere(t *testing.T) (*net.UnixListener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln, path
}

func TestTheListeningSocketComesBackFromTheStore(t *testing.T) {
	ln, path := listenSomewhere(t)
	handOver(t, ln, socketFDName)

	got, ok := adoptListener(path, quietLogger())
	if !ok {
		t.Fatal("the socket was not adopted")
	}
	defer got.Close()
	if got.Addr().String() != path {
		t.Fatalf("adopted a listener on %q, want %q", got.Addr(), path)
	}
	// And it is the same socket, not a new one on the same name: a client that connected to the
	// old one is accepted here.
	done := make(chan error, 1)
	go func() {
		c, err := net.Dial("unix", path)
		if err == nil {
			c.Close()
		}
		done <- err
	}()
	conn, err := got.Accept()
	if err != nil {
		t.Fatalf("accepting on the adopted socket: %v", err)
	}
	conn.Close()
	if err := <-done; err != nil {
		t.Fatalf("dialling the adopted socket: %v", err)
	}
}

func TestASocketForADifferentPathIsRefused(t *testing.T) {
	ln, _ := listenSomewhere(t)
	handOver(t, ln, socketFDName)

	// The same descriptor, offered as the socket for somewhere else. Serving it would be a daemon
	// listening where nobody is looking.
	if _, ok := adoptListener("/tmp/somewhere-else.sock", quietLogger()); ok {
		t.Fatal("a socket for another path was adopted")
	}
}

func TestSomethingThatIsNotASocketIsRefused(t *testing.T) {
	f, err := os.CreateTemp("", "notasocket")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	placeFDs(t, f)
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", "1")
	t.Setenv("LISTEN_FDNAMES", socketFDName)

	if _, ok := adoptListener("/tmp/whatever.sock", quietLogger()); ok {
		t.Fatal("a regular file was adopted as the listening socket")
	}
}

func TestDescriptorsForSomethingElseAreKeptNotClosed(t *testing.T) {
	// The services' pty masters come back through the same door. A function whose job is the
	// socket must not decide their fate - it hands them on.
	t.Cleanup(func() { clearPendingFDs() })
	clearPendingFDs()

	ln, path := listenSomewhere(t)
	other, err := os.CreateTemp("", "pty")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	lf, err := ln.File()
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	placeFDs(t, lf, other)
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", "2")
	t.Setenv("LISTEN_FDNAMES", socketFDName+":shell-99")

	got, ok := adoptListener(path, quietLogger())
	if !ok {
		t.Fatal("the socket was not adopted")
	}
	defer got.Close()

	pending := PendingFDs()
	if len(pending) != 1 || pending["shell-99"] == nil {
		t.Fatalf("what was left for somebody else: %v", pending)
	}
	// Still open, because nothing has claimed it yet.
	if _, err := pending["shell-99"].Stat(); err != nil {
		t.Fatalf("the descriptor left for later was closed: %v", err)
	}
}

func TestNothingLeftOverIsClosedAndDropped(t *testing.T) {
	t.Cleanup(func() { clearPendingFDs() })
	clearPendingFDs()

	f, err := os.CreateTemp("", "orphan")
	if err != nil {
		t.Fatal(err)
	}
	keepForLater("shell-99", f)
	ReleaseUnadoptedFDs(quietLogger())

	if len(PendingFDs()) != 0 {
		t.Fatalf("something was still pending: %v", PendingFDs())
	}
	if _, err := f.Stat(); err == nil {
		t.Fatal("a descriptor nobody claimed was left open")
	}
}

func clearPendingFDs() {
	for k, v := range pendingFDs {
		_ = v.Close()
		delete(pendingFDs, k)
	}
}
