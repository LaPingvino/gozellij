package daemon

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// The name a terminal is stored under has to survive the round trip, because it is the only thing
// that says which service and which process a bare descriptor belongs to.

func TestAStoredTerminalsNameRoundTrips(t *testing.T) {
	for _, c := range []struct {
		service string
		pid     int
	}{
		{"shell", 42},
		{"my-web-server", 1},
		{"a-b-c-d", 999999},
		{"x", 2147483647},
		// A name that is all digits, which is the one that would confuse a parser splitting
		// from the wrong end.
		{"12345", 7},
	} {
		name := ptyFDName(c.service, c.pid)
		service, pid, ok := parsePTYFDName(name)
		if !ok || service != c.service || pid != c.pid {
			t.Errorf("%q -> service %q pid %d ok=%v, want %q and %d",
				name, service, pid, ok, c.service, c.pid)
		}
		if err := checkFDName(name); err != nil {
			// A name systemd will not carry is a terminal that silently does not get stored.
			t.Errorf("%q is not a name systemd can hold: %v", name, err)
		}
	}
}

func TestSomethingElsesNameIsNotMistakenForATerminal(t *testing.T) {
	for _, name := range []string{
		socketFDName,
		"",
		"gzpty-",
		"gzpty-shell",   // no pid
		"gzpty--shell",  // empty pid
		"gzpty-0-shell", // pid zero is not a process
		"gzpty-x-shell", // pid that is not a number
		"gzpty-12-",     // no service
		"unnamed-3",
	} {
		if _, _, ok := parsePTYFDName(name); ok {
			t.Errorf("%q was read as a service's terminal", name)
		}
	}
}

func TestATerminalNameForAVeryLongServiceIsStillCarryable(t *testing.T) {
	// systemd caps a name at 255 bytes. A service name close to that is the case where the
	// prefix and pid push it over, and the failure would be a terminal that is never stored -
	// visible only as a crash that lost a service it promised to keep.
	long := strings.Repeat("s", 200)
	if err := checkFDName(ptyFDName(long, 123456)); err != nil {
		t.Fatalf("a 200-character service name cannot be stored: %v", err)
	}
}

func TestATerminalWhoseProcessIsGoneIsNotAdopted(t *testing.T) {
	freshCollection(t)
	f, err := os.CreateTemp("", "pty")
	if err != nil {
		t.Fatal(err)
	}
	// A pid that is not running. 0 is refused by the parser, so use one that cannot be alive:
	// the parser accepts it and the liveness check is what must reject it.
	dead := deadPid(t)
	keepForLater(ptyFDName("ghost", dead), f)

	if got := RecoveredPTYs(quietLogger()); len(got) != 0 {
		t.Fatalf("adopted %d terminals whose processes are gone: %+v", len(got), got)
	}
	// And it was let go rather than left pending for ever.
	if len(PendingFDs()) != 0 {
		t.Fatalf("the terminal of a process that no longer exists is still being held: %v", PendingFDs())
	}
}

func TestATerminalWhoseProcessIsAliveBecomesAHandover(t *testing.T) {
	freshCollection(t)
	f, err := os.CreateTemp("", "pty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// This process is certainly alive.
	keepForLater(ptyFDName("alive", os.Getpid()), f)

	got := RecoveredPTYs(quietLogger())
	if len(got) != 1 {
		t.Fatalf("got %d handovers, want 1", len(got))
	}
	h := got[0]
	if h.Name != "alive" || h.Pid != os.Getpid() {
		t.Fatalf("handover is %+v", h)
	}
	if !h.Orphan {
		// Without this the fabric would wait4 for a process it is not the parent of, get ECHILD
		// and report the service finished a moment after adopting it.
		t.Fatal("a recovered process was not marked as one this daemon cannot wait for")
	}
	if h.StartedAt.IsZero() {
		t.Fatal("a recovered process has no start time, which prints as 1970")
	}
	// Claimed, so nothing closes it out from under the fabric.
	if len(PendingFDs()) != 0 {
		t.Fatalf("a terminal that was adopted is still pending: %v", PendingFDs())
	}
}

func TestSomethingElseInTheStoreIsLeftForItsOwner(t *testing.T) {
	freshCollection(t)
	f, err := os.CreateTemp("", "other")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	keepForLater(socketFDName, f)

	if got := RecoveredPTYs(quietLogger()); len(got) != 0 {
		t.Fatalf("the listening socket was read as a service's terminal: %+v", got)
	}
	if len(PendingFDs()) != 1 {
		t.Fatal("the socket was taken or dropped by the terminal recovery")
	}
}

// deadPid returns a pid with no process behind it.
func deadPid(t *testing.T) int {
	t.Helper()
	max, err := os.ReadFile("/proc/sys/kernel/pid_max")
	if err != nil {
		t.Skip("cannot read pid_max to find an unused pid")
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(max)))
	if err != nil {
		t.Skip("pid_max is not a number")
	}
	// One past the maximum can never be a running process, which is what this needs - rather
	// than a number that merely happens to be free right now.
	return n + 1
}
