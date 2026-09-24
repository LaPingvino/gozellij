package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"

	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/daemon"
	"github.com/LaPingvino/gozellij/internal/fabric"
)

// Ported from acceptance.sh, the promises only the command line can show: its messages, its exit
// status, its flags reaching the daemon, and what doctor, ls and takeover print. Each test runs
// the real `run` against a daemon of its own, in this process, over a state directory of its own.

const aWait = 10 * time.Second

// aEnv gives a test a home, state, configuration and runtime directory of its own, so nothing it
// does can reach the real daemon or the real state. Returns the state and runtime directories.
func aEnv(t *testing.T) (state, runDir string) {
	t.Helper()
	home := t.TempDir()
	state = filepath.Join(home, "state")
	// Short, for the 108-byte socket path limit.
	runDir, err := os.MkdirTemp("", "gza")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runDir) })
	t.Setenv("HOME", home)
	t.Setenv("GOZELLIJ_STATE_DIR", state)
	t.Setenv("GOZELLIJ_RUNTIME_DIR", runDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("GOZELLIJ_STATUS_CONFIG", "")
	t.Setenv("GOZELLIJ", "")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return state, runDir
}

// aDaemon starts a daemon the way gozellijd does - definitions under <state>/services, logs under
// <state>/logs unless logsOff - listening on the socket the CLI will look for.
func aDaemon(t *testing.T, logsOff bool) (state, runDir string) {
	t.Helper()
	state, runDir = aEnv(t)
	reg, err := fabric.NewRegistry(filepath.Join(state, "services"))
	if err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(state, "logs")
	if logsOff {
		logDir = ""
	}
	fab := fabric.NewFabric(reg, fabric.StartOptions{LogDir: logDir})
	fab.Load()
	t.Cleanup(fab.Shutdown)
	srv, err := daemon.Listen(daemon.SocketPath(), fab, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.SetVersion(Version)
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { srv.Close() })
	return state, runDir
}

// aRunning is a `gozellij` command running in this process, with its standard output and error
// going to pipes this test reads.
type aRunning struct {
	mu       sync.Mutex
	out, err strings.Builder
	result   chan error
	restore  func()
	finished error
	done     bool
}

// aStart runs `gozellij args...` in the background. Only one may run at a time: os.Stdout and
// os.Stderr are the process's, and are swapped for as long as it runs.
func aStart(t *testing.T, args ...string) *aRunning {
	t.Helper()
	or, ow, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r := &aRunning{result: make(chan error, 1)}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = ow, ew
	var readers sync.WaitGroup
	for _, p := range []struct {
		from *os.File
		to   *strings.Builder
	}{{or, &r.out}, {er, &r.err}} {
		readers.Add(1)
		go func(from *os.File, to *strings.Builder) {
			defer readers.Done()
			b := make([]byte, 4096)
			for {
				n, err := from.Read(b)
				r.mu.Lock()
				to.Write(b[:n])
				r.mu.Unlock()
				if err != nil {
					return
				}
			}
		}(p.from, p.to)
	}
	r.restore = func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		ow.Close()
		ew.Close()
		readers.Wait()
		or.Close()
		er.Close()
	}
	go func() { r.result <- run(args) }()
	return r
}

// Out and Err are what it has printed so far.
func (r *aRunning) Out() string { r.mu.Lock(); defer r.mu.Unlock(); return r.out.String() }
func (r *aRunning) Err() string { r.mu.Lock(); defer r.mu.Unlock(); return r.err.String() }

// Wait waits for the command to finish and gives the terminal back.
func (r *aRunning) Wait(t *testing.T) error {
	t.Helper()
	if r.done {
		return r.finished
	}
	select {
	case r.finished = <-r.result:
	case <-time.After(aWait):
		r.restore()
		t.Fatalf("the command did not finish; it printed %q and %q", r.Out(), r.Err())
	}
	r.restore()
	r.done = true
	return r.finished
}

// aRun runs `gozellij args...` to the end and returns what it printed and how it ended.
func aRun(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	r := aStart(t, args...)
	err = r.Wait(t)
	return r.Out(), r.Err(), err
}

// aMust runs a command that has to succeed.
func aMust(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, err := aRun(t, args...)
	if err != nil {
		t.Fatalf("gozellij %s: %v\n%s%s", strings.Join(args, " "), err, out, errOut)
	}
	return out
}

// aField is one "key: value" line of `gozellij status name`, "" if the line is not there.
func aField(t *testing.T, name, key string) string {
	t.Helper()
	out, _, err := aRun(t, "status", name)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+":"))
		}
	}
	return ""
}

// aUntil waits for cond, or fails saying what it waited for.
func aUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(aWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", aWait, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// aLogsShow waits until `gozellij logs name` prints want.
func aLogsShow(t *testing.T, name, want string) string {
	t.Helper()
	var last string
	aUntil(t, fmt.Sprintf("%q in the logs of %s", want, name), func() bool {
		last, _, _ = aRun(t, "logs", name)
		return strings.Contains(last, want)
	})
	return last
}

// aGone is whether a process has ended; a zombie nobody has reaped yet has.
func aGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return true
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] == 'Z' || s[i+2] == 'X'
	}
	return false
}

// ------------------------------------------------------------ the shell captures YOUR term

// The half of "the shell gets the terminal you started it from" that the command line owns: the
// terminal's TERM goes into the shell's definition. The daemon half - a definition's TERM wins
// over the daemon's own - is TestPortAShellRunsWithTheTermItWasDefinedWith.
func TestPortAShellCapturesYourTerm(t *testing.T) {
	t.Setenv("TERM", "xterm-portA")
	for _, kv := range captureShellEnv() {
		if kv == "TERM=xterm-portA" {
			return
		}
	}
	t.Fatalf("the shell's environment does not carry the terminal's TERM: %q", captureShellEnv())
}

// ------------------------------------------------------------ one command, several services

func TestPortAOneCommandSeveralServices(t *testing.T) {
	aDaemon(t, false)
	names := []string{"multi_a", "multi_b", "multi_c"}
	for _, n := range names {
		aMust(t, "add", n, "-start", "--", "sleep", "60")
	}

	aMust(t, append([]string{"stop"}, names...)...)
	for _, n := range names {
		if st := aField(t, n, "state"); st != "stopped" {
			t.Errorf("after one stop of three, %s is %q", n, st)
		}
	}

	// A typo in the middle: the other two are still started, and the command still fails.
	_, errOut, err := aRun(t, "start", "multi_a", "nosuch_service", "multi_c")
	if err == nil || !strings.Contains(err.Error(), "1 of 3") {
		t.Errorf("start with one bad name of three returned %v", err)
	}
	if !strings.Contains(errOut, "nosuch_service") {
		t.Errorf("the bad name was not reported: %q", errOut)
	}
	for _, n := range []string{"multi_a", "multi_c"} {
		if st := aField(t, n, "state"); st != "running" {
			t.Errorf("%s was abandoned because of the typo: %q", n, st)
		}
	}

	out := aMust(t, append([]string{"rm"}, names...)...)
	if n := len(regexp.MustCompile(`(?m)^removed multi_`).FindAllString(out, -1)); n != 3 {
		t.Errorf("rm of three said removed %d times: %q", n, out)
	}
	if ls := aMust(t, "ls"); strings.Contains(ls, "multi_") {
		t.Errorf("rm of three left some behind:\n%s", ls)
	}
}

// ------------------------------------------------------- following several services at once

func TestPortAFollowingSeveralServicesAtOnce(t *testing.T) {
	aDaemon(t, false)
	aMust(t, "add", "follow_a", "-start", "--", "sh", "-c", `for i in 1 2 3 4 5; do echo AAA-$i; done; exec sleep 30`)
	aMust(t, "add", "follow_b", "-start", "--", "sh", "-c", `for i in 1 2 3 4 5; do echo BBB-$i; done; exec sleep 30`)
	aLogsShow(t, "follow_a", "AAA-5")
	aLogsShow(t, "follow_b", "BBB-5")

	// A follow ends when its service is removed, which is how these are ended: over a
	// connection of the test's own, while the command still has the terminal.
	removeAll := func(names ...string) {
		c, err := daemon.Dial(daemon.SocketPath())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		for _, n := range names {
			if _, err := c.Remove(n, true); err != nil {
				t.Fatalf("rm %s: %v", n, err)
			}
		}
	}

	// Refused first, while both services exist.
	_, _, err := aRun(t, "logs", "follow_a", "follow_b")
	if err == nil || !strings.Contains(err.Error(), "several with -f") {
		t.Errorf("logs of two services without -f returned %v", err)
	}

	both := aStart(t, "logs", "-f", "follow_a", "follow_b")
	aUntil(t, "both services in the follow", func() bool {
		o := both.Out()
		return strings.Contains(o, "follow_a | AAA-5") && strings.Contains(o, "follow_b | BBB-5")
	})
	removeAll("follow_a", "follow_b")
	if err := both.Wait(t); err != nil {
		t.Fatalf("logs -f of two ended with %v", err)
	}
	// Every line under its own name: both names appearing would also pass with the prefixes on
	// the wrong lines, which is the way this can actually go wrong.
	for _, line := range strings.Split(both.Out(), "\n") {
		if (strings.Contains(line, "AAA-") && !strings.HasPrefix(line, "follow_a | ")) ||
			(strings.Contains(line, "BBB-") && !strings.HasPrefix(line, "follow_b | ")) {
			t.Errorf("a line is not under its own service's name: %q", line)
		}
	}

	// One service is untouched, so `logs -f x | grep` does not have to strip anything.
	aMust(t, "add", "follow_a", "-start", "--", "sh", "-c", `for i in 1 2 3 4 5; do echo AAA-$i; done; exec sleep 30`)
	one := aStart(t, "logs", "-f", "follow_a")
	aUntil(t, "the single follow to print", func() bool { return strings.Contains(one.Out(), "AAA-5") })
	removeAll("follow_a")
	if err := one.Wait(t); err != nil {
		t.Fatalf("logs -f ended with %v", err)
	}
	if o := one.Out(); !regexp.MustCompile(`(?m)^AAA-1`).MatchString(o) || strings.Contains(o, "follow_a |") {
		t.Errorf("following one service added something:\n%s", o)
	}
}
