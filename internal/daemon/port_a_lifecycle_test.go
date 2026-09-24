package daemon

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Ported from acceptance.sh: "the environment", "logs outlive the daemon", "enabled services come
// back" and "tree-kill".

// aLogsText is a service's output as `logs` returns it, or "" if it cannot be read.
func aLogsText(t *testing.T, sock, name string) string {
	t.Helper()
	out, err := dial(t, sock).Logs(name, 0)
	if err != nil {
		return ""
	}
	return string(out.Data)
}

// aGone reports whether a process no longer exists, counting a zombie as gone: it has ended, and
// whoever inherited it has simply not reaped it yet.
func aGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return true
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	// The state is the field after the parenthesised command name.
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] == 'Z' || s[i+2] == 'X'
	}
	return false
}

func TestPortALogsReturnsWhatAServicePrinted(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	// Computed by the service, so the command line cannot be what is found.
	addRunning(t, sock, "envprobe", "sh", "-c", `echo "PRINTED-$((6*7))"; sleep 30`)
	eventually(t, "the service's output in logs", func() bool {
		return strings.Contains(aLogsText(t, sock, "envprobe"), "PRINTED-42")
	})
}

func TestPortALogsOutliveTheDaemon(t *testing.T) {
	state := t.TempDir()
	srv, _, sock := loggingDaemon(t, state)
	c := dial(t, sock)
	if _, err := c.Add("pineapple", ipc.AddRequest{
		Command: "sh", Args: []string{"-c", `echo "PINEAPPLE-$((6*7))"; sleep 30`}, Start: true,
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the marker in the first daemon's log", func() bool {
		return strings.Contains(aLogsText(t, sock, "pineapple"), "PINEAPPLE-42")
	})
	// Stopped, which also disables it: a fresh daemon that started it again would print the marker
	// again, and the check would pass without the file.
	if _, err := c.Stop("pineapple"); err != nil {
		t.Fatal(err)
	}
	// The first daemon goes away without tidying anything, as far as this process can manage it:
	// its socket is closed and nothing it holds is consulted again.
	srv.Close()

	_, _, sock2 := loggingDaemon(t, state)
	got := aLogsText(t, sock2, "pineapple")
	if n := strings.Count(got, "PINEAPPLE-42"); n != 1 {
		t.Fatalf("a fresh daemon's logs has the marker %d times, want exactly once: %q", n, got)
	}
	if st, err := dial(t, sock2).Status("pineapple"); err != nil || st.Pid != 0 {
		t.Fatalf("the stopped service was started again (%+v, %v), so the log proves nothing", st, err)
	}
}

func TestPortAEnabledServicesComeBack(t *testing.T) {
	state := t.TempDir()
	_, fab, sock := loggingDaemon(t, state)
	addRunning(t, sock, "tabtarget", "sleep", "30")
	addRunning(t, sock, "switchedoff", "sleep", "30")
	if _, err := dial(t, sock).Stop("switchedoff"); err != nil {
		t.Fatal(err)
	}
	// Reboot-equivalent: every process is gone and a new daemon reads the same state.
	fab.Shutdown()

	_, _, sock2 := loggingDaemon(t, state)
	eventually(t, "tabtarget running under the fresh daemon", func() bool {
		st, err := dial(t, sock2).Status("tabtarget")
		return err == nil && st.State == "running" && st.Pid != 0
	})
	// And the one that was stopped on purpose stays stopped, or "comes back" means "everything
	// is started".
	if st, err := dial(t, sock2).Status("switchedoff"); err != nil || st.Pid != 0 {
		t.Errorf("a stopped service came back: %+v %v", st, err)
	}
}

func TestPortATreeKillTakesTheChildren(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "stubborn", "sh", "-c",
		`trap '' HUP; (trap '' HUP; exec sleep 47000) & echo "CHILD=$!."; wait`)

	var child int
	re := regexp.MustCompile(`CHILD=(\d+)\.`)
	eventually(t, "the service to report its child", func() bool {
		m := re.FindStringSubmatch(aLogsText(t, sock, "stubborn"))
		if m == nil {
			return false
		}
		child, _ = strconv.Atoi(m[1])
		return true
	})
	// Without this the check passes whenever the child never started.
	if aGone(child) {
		t.Fatalf("child %d is not running, so there is nothing to test", child)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	if _, err := dial(t, sock).Stop("stubborn"); err != nil {
		t.Fatal(err)
	}
	eventually(t, fmt.Sprintf("child %d, which ignores SIGHUP, to go with its service", child), func() bool {
		return aGone(child)
	})
}
