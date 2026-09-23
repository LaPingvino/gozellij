package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A stand-in for systemd: a datagram socket that records what it was told and any descriptors that
// came with it.
//
// The real thing is checked by hand against a transient user unit - the transcript is in
// docs/USER_STORIES.md C3 - because a test cannot make systemd crash and restart it. What is
// checked here is everything up to the socket: that the message is the shape systemd parses, that
// the descriptor really travels, and that the handback is read the way sd_listen_fds reads it.
type fakeSystemd struct {
	conn *net.UnixConn
	path string
}

func startFakeSystemd(t *testing.T) *fakeSystemd {
	t.Helper()
	// A short path: a unix socket's name has about a hundred bytes to live in, and t.TempDir()
	// under a long test name has been known to exceed it.
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "n")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	t.Setenv(NotifyEnv, path)
	return &fakeSystemd{conn: conn, path: path}
}

// next reads one message, returning what was said and the descriptors that came with it.
func (f *fakeSystemd) next(t *testing.T) (string, []int) {
	t.Helper()
	_ = f.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	oob := make([]byte, 4096)
	n, oobn, _, _, err := f.conn.ReadMsgUnix(buf, oob)
	if err != nil {
		t.Fatalf("nothing arrived: %v", err)
	}
	var fds []int
	if oobn > 0 {
		scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			t.Fatalf("control message: %v", err)
		}
		for _, scm := range scms {
			got, err := syscall.ParseUnixRights(&scm)
			if err != nil {
				t.Fatalf("rights: %v", err)
			}
			fds = append(fds, got...)
		}
	}
	return string(buf[:n]), fds
}

func TestNotifyWithNoSocketIsNotAFailure(t *testing.T) {
	t.Setenv(NotifyEnv, "")
	// Running outside systemd is the ordinary case, not an error to report. Everything that calls
	// this has to be able to tell the two apart.
	if err := NotifyReady(); err != ErrNoNotifySocket {
		t.Fatalf("with no socket: %v, want ErrNoNotifySocket", err)
	}
}

func TestReadyIsWhatSystemdExpects(t *testing.T) {
	sd := startFakeSystemd(t)
	if err := NotifyReady(); err != nil {
		t.Fatal(err)
	}
	if msg, fds := sd.next(t); msg != "READY=1" || len(fds) != 0 {
		t.Fatalf("sent %q with %d descriptors", msg, len(fds))
	}
}

func TestAStoredDescriptorTravelsWithItsName(t *testing.T) {
	sd := startFakeSystemd(t)
	f, err := os.CreateTemp("", "stored")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("THE-REAL-FILE"); err != nil {
		t.Fatal(err)
	}

	if err := StoreFD("shell-42", f); err != nil {
		t.Fatal(err)
	}
	msg, fds := sd.next(t)
	if msg != "FDSTORE=1\nFDNAME=shell-42" {
		t.Fatalf("sent %q", msg)
	}
	if len(fds) != 1 {
		t.Fatalf("%d descriptors arrived, want 1", len(fds))
	}
	// And it is the same open file, not a copy of its name: read through the descriptor that
	// crossed the socket. This is the part that makes the whole scheme possible, so it is checked
	// rather than assumed.
	got := os.NewFile(uintptr(fds[0]), "arrived")
	defer got.Close()
	buf := make([]byte, 32)
	n, _ := got.ReadAt(buf, 0)
	if string(buf[:n]) != "THE-REAL-FILE" {
		t.Fatalf("the descriptor that arrived reads %q", string(buf[:n]))
	}
}

func TestRemovingAStoredDescriptorNamesIt(t *testing.T) {
	sd := startFakeSystemd(t)
	if err := RemoveStoredFD("shell-42"); err != nil {
		t.Fatal(err)
	}
	if msg, fds := sd.next(t); msg != "FDSTOREREMOVE=1\nFDNAME=shell-42" || len(fds) != 0 {
		t.Fatalf("sent %q with %d descriptors", msg, len(fds))
	}
}

func TestANameSystemdCannotCarryIsRefusedHere(t *testing.T) {
	sd := startFakeSystemd(t)
	// ':' is what systemd joins names with in LISTEN_FDNAMES, so a name containing one comes back
	// as two names and the descriptors line up with nothing. Refused rather than sent and lost.
	for _, name := range []string{"", "a:b", "a\nb", strings.Repeat("x", maxFDName+1)} {
		if err := StoreFD(name, os.Stdin); err == nil {
			t.Errorf("the name %q was accepted", name)
		}
		if err := RemoveStoredFD(name); err == nil {
			t.Errorf("the name %q was accepted for removal", name)
		}
	}
	_ = sd
}

func TestNothingComesBackWhenNothingWasHandedOver(t *testing.T) {
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")
	t.Setenv("LISTEN_FDNAMES", "")
	if got := StoredFDs(); len(got) != 0 {
		t.Fatalf("found %d descriptors with nothing in the environment", len(got))
	}
}

func TestDescriptorsAddressedToAnotherProcessAreLeftAlone(t *testing.T) {
	// LISTEN_PID is the whole reason this check exists: the variables are inherited, so a child
	// that skipped it would believe it owns its parent's descriptors and close them.
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()+1))
	t.Setenv("LISTEN_FDS", "1")
	t.Setenv("LISTEN_FDNAMES", "shell-42")
	if got := StoredFDs(); len(got) != 0 {
		t.Fatalf("took %d descriptors meant for another process", len(got))
	}
}

func TestWhatComesBackIsNamedAndTheEnvironmentIsCleared(t *testing.T) {
	// Two real descriptors, placed where systemd would have put them, so this reads the same
	// numbers sd_listen_fds would.
	a, err := os.CreateTemp("", "fda")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.CreateTemp("", "fdb")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// Placed away from the low numbers the rest of this package's tests are using. See the note
	// on listenFDsStart.
	at := placeFDs(t, a, b)
	_ = at

	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", "2")
	t.Setenv("LISTEN_FDNAMES", "shell-42:logs-7")

	got := StoredFDs()
	if len(got) != 2 {
		t.Fatalf("got %d descriptors back, want 2: %v", len(got), got)
	}
	for _, name := range []string{"shell-42", "logs-7"} {
		if got[name] == nil {
			t.Errorf("nothing came back under %q; got %v", name, keysOf(got))
		}
	}
	// Cleared, so that anything this daemon spawns does not think the descriptors are its own.
	for _, k := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		if os.Getenv(k) != "" {
			t.Errorf("%s survived, so a child would claim these descriptors too", k)
		}
	}
	for _, f := range got {
		_ = f.Close()
	}
}

func TestAnUnnamedDescriptorStillComesBack(t *testing.T) {
	f, err := os.CreateTemp("", "fdc")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	placeFDs(t, f)
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", "1")
	// systemd's own placeholder when a descriptor was stored without a name.
	t.Setenv("LISTEN_FDNAMES", "unknown")

	got := StoredFDs()
	if len(got) != 1 {
		t.Fatalf("an unnamed descriptor was dropped: %v", keysOf(got))
	}
	// Under something that cannot be a service name, because it is still a descriptor somebody
	// has to close.
	for k, v := range got {
		if !strings.HasPrefix(k, "unnamed-") {
			t.Errorf("it came back as %q", k)
		}
		_ = v.Close()
	}
}

func keysOf(m map[string]*os.File) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// placeFDs puts copies of the given files at consecutive descriptor numbers and points
// listenFDsStart at them, the way systemd would have placed them at 3.
//
// High numbers, not 3: this is one process for the whole package, and taking 3 and 4 closed the
// daemon's listening socket out from under eight other tests. Restored afterwards, so the choice
// does not leak into the next test either.
func placeFDs(t *testing.T, files ...*os.File) int {
	t.Helper()
	base := freeFDRun(t, len(files))
	old := listenFDsStart
	listenFDsStart = base
	t.Cleanup(func() { listenFDsStart = old })
	for i, f := range files {
		target := base + i
		if err := syscall.Dup2(int(f.Fd()), target); err != nil {
			t.Fatalf("placing a descriptor at %d: %v", target, err)
		}
		// Not closed here. Whatever reads the handover wraps this number in an *os.File of its
		// own and closes it - the adopter when it claims one, clearPendingFDs when nothing does -
		// so a close here was the second close of the same number. Between the two, the number
		// can be handed to something else, and the second close then destroys that: a test's
		// listening socket, in the rare failures where a daemon that had just started refused
		// its own clients. An uncollected descriptor leaking into the test process is harmless;
		// a double close is not.
	}
	return base
}

// freeFDRun finds n consecutive descriptor numbers nothing is using.
//
// The first version of this took 60 and 61 on the grounds that they were probably free. Probably
// is not a property: dup2 onto a number in use closes what was there without a word, and the test
// that finds out is some other test in this package failing inside a temporary directory whose
// handle has quietly become something else. Descriptor numbers are process-wide state and the only
// safe way to pick one is to ask.
func freeFDRun(t *testing.T, n int) int {
	t.Helper()
	if n <= 0 {
		n = 1
	}
	for base := 40; base < 400; base++ {
		free := true
		for i := range n {
			if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(base+i), syscall.F_GETFD, 0); errno != syscall.EBADF {
				free = false
				break
			}
		}
		if free {
			return base
		}
	}
	t.Fatal("no run of unused descriptor numbers to borrow")
	return 0
}
