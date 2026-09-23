package fabric

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitFor polls until cond holds, so tests do not depend on a fixed sleep being long enough on a
// loaded machine. (The machine this was written on runs at load 20+; fixed sleeps are how you get
// a suite that only passes when nobody is using the box.)
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, what)
}

func TestStartRunsAndCapturesOutput(t *testing.T) {
	p, err := Start(Service{Name: "echo", Command: "echo", Args: []string{"hello pty"}}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	exit := p.Wait()
	if !exit.Clean() {
		t.Errorf("exit = %+v, want clean", exit)
	}
	out, _ := p.Output.Snapshot()
	if !strings.Contains(string(out), "hello pty") {
		t.Errorf("output = %q, want it to contain %q", out, "hello pty")
	}
	if p.Pid() == 0 {
		t.Error("Pid = 0, want the child's pid")
	}
}

// The output a process writes on its way out is usually the output that matters. Closing the pty
// the instant the child exits throws it away.
func TestFinalOutputBeforeExitIsNotLost(t *testing.T) {
	p, err := Start(Service{
		Name:    "dying",
		Command: "sh",
		Args:    []string{"-c", "echo last words before I go; exit 3"},
	}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	exit := p.Wait()
	if exit.Code != 3 {
		t.Errorf("exit code = %d, want 3", exit.Code)
	}
	out, _ := p.Output.Snapshot()
	if !strings.Contains(string(out), "last words before I go") {
		t.Errorf("lost the dying process's output; got %q", out)
	}
}

// RestartOnFailure depends on telling these two apart, so they must not both look like "exited".
func TestSignalledExitIsDistinguishableFromCleanExit(t *testing.T) {
	p, err := Start(Service{Name: "sleeper", Command: "sleep", Args: []string{"300"}}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	if err := p.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	exit := p.Wait()
	if exit.Clean() {
		t.Error("a SIGKILLed process reported a clean exit")
	}
	if exit.Signal == "" {
		t.Errorf("exit = %+v, want a signal name recorded", exit)
	}
	if !RestartOnFailure.ShouldRestart(exit) {
		t.Error("on-failure should restart a process that was killed")
	}
}

func TestExitCodeIsReported(t *testing.T) {
	p, err := Start(Service{Name: "failer", Command: "sh", Args: []string{"-c", "exit 42"}}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	exit := p.Wait()
	if exit.Code != 42 || exit.Signal != "" {
		t.Errorf("exit = %+v, want code 42 and no signal", exit)
	}
}

// A command that does not exist must fail loudly at spawn time. Otherwise it looks like a process
// that exited instantly, and a restart policy would retry it forever, cheerfully.
func TestMissingCommandFailsAtStartNotSilently(t *testing.T) {
	_, err := Start(Service{Name: "ghost", Command: "definitely-not-a-real-binary-xyzzy"}, StartOptions{})
	if err == nil {
		t.Fatal("Start succeeded for a command that does not exist")
	}
	if !strings.Contains(err.Error(), "definitely-not-a-real-binary-xyzzy") {
		t.Errorf("error does not name the command: %v", err)
	}
}

func TestInvalidServiceIsRejected(t *testing.T) {
	if _, err := Start(Service{Name: "", Command: "echo"}, StartOptions{}); err == nil {
		t.Error("Start accepted a service with no name")
	}
	if _, err := Start(Service{Name: "ok", Command: ""}, StartOptions{}); err == nil {
		t.Error("Start accepted a service with no command")
	}
}

func TestWriteSendsInputAndCatIsAPty(t *testing.T) {
	p, err := Start(Service{Name: "cat", Command: "cat"}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	if _, err := p.Write([]byte("ping\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitFor(t, 5*time.Second, "cat to echo the input back", func() bool {
		out, _ := p.Output.Snapshot()
		return strings.Contains(string(out), "ping")
	})

	_ = p.Stop()
}

// The child must have a controlling terminal, otherwise everything that checks isatty behaves
// differently inside gozellij than outside it - which is the sort of difference that makes people
// distrust a multiplexer.
func TestChildSeesATerminal(t *testing.T) {
	p, err := Start(Service{
		Name:    "istty",
		Command: "sh",
		Args:    []string{"-c", "if [ -t 1 ]; then echo IS_TTY; else echo NOT_TTY; fi"},
	}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	p.Wait()

	out, _ := p.Output.Snapshot()
	if !strings.Contains(string(out), "IS_TTY") {
		t.Errorf("child did not see a tty; output was %q", out)
	}
}

func TestInitialSizeIsHonouredAndResizeIsSeen(t *testing.T) {
	p, err := Start(Service{
		Name:    "size",
		Command: "sh",
		Args:    []string{"-c", "stty size; sleep 300"},
	}, StartOptions{Cols: 100, Rows: 40})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	waitFor(t, 5*time.Second, "stty to report the initial size", func() bool {
		out, _ := p.Output.Snapshot()
		return strings.Contains(string(out), "40 100")
	})

	if err := p.Resize(120, 50); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	_ = p.Stop()
}

func TestResizeRejectsNonsenseSizes(t *testing.T) {
	p, err := Start(Service{Name: "sleeper", Command: "sleep", Args: []string{"300"}}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	defer p.Stop()

	for _, sz := range [][2]int{{0, 24}, {80, 0}, {-1, -1}} {
		if err := p.Resize(sz[0], sz[1]); err == nil {
			t.Errorf("Resize(%d, %d) was accepted, want an error", sz[0], sz[1])
		}
	}
}

func TestStopTerminatesAWellBehavedProcess(t *testing.T) {
	p, err := Start(Service{Name: "sleeper", Command: "sleep", Args: []string{"300"}}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	start := time.Now()
	exit := p.Stop()
	if took := time.Since(start); took > StopGrace {
		t.Errorf("Stop took %v; a process that honours SIGTERM should not need the kill grace", took)
	}
	if exit.Signal == "" {
		t.Errorf("exit = %+v, want the terminating signal recorded", exit)
	}
}

// A process that ignores SIGTERM must still be stopped. This one traps it and keeps going.
func TestStopKillsAProcessThatIgnoresSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("takes StopGrace seconds by design")
	}
	p, err := Start(Service{
		Name:    "stubborn",
		Command: "sh",
		// TERM and HUP both, because both are asked politely now: a process that only ignores
		// TERM goes when the SIGHUP arrives a quarter of a second later - see the test below -
		// and the point of this one is what happens to something that ignores everything polite.
		Args: []string{"-c", "trap '' TERM HUP; echo TRAP_SET; while :; do sleep 0.2; done"},
	}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	// Wait until the trap is actually installed. Signalling straight after Start races the
	// shell's first line, and a SIGTERM that arrives before `trap` runs kills it by default -
	// which looks exactly like "Stop failed to escalate" and is nothing of the kind.
	waitFor(t, 10*time.Second, "the shell to install its TERM trap", func() bool {
		out, _ := p.Output.Snapshot()
		return strings.Contains(string(out), "TRAP_SET")
	})

	exit := p.Stop()
	if _, done := p.Exited(); !done {
		t.Fatal("process is still running after Stop")
	}
	if exit.Signal != syscall.SIGKILL.String() {
		t.Errorf("exit = %+v, want it killed with %v", exit, syscall.SIGKILL)
	}
}

// An interactive shell ignores SIGTERM on purpose, and used to take the whole five-second grace to
// stop. It goes on SIGHUP - the signal a closing terminal sends - a quarter of a second in.
//
// Found in use: removing a shell from inside an attach looked like the key did nothing, because it
// did its work five seconds later.
func TestAProcessThatOnlyIgnoresSIGTERMGoesQuickly(t *testing.T) {
	p, err := Start(Service{
		Name:    "shell-like",
		Command: "sh",
		Args:    []string{"-c", "trap '' TERM; echo TRAP_SET; while :; do sleep 0.2; done"},
	}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	waitFor(t, 10*time.Second, "the shell to install its TERM trap", func() bool {
		out, _ := p.Output.Snapshot()
		return strings.Contains(string(out), "TRAP_SET")
	})

	start := time.Now()
	exit := p.Stop()
	took := time.Since(start)
	if exit.Signal != syscall.SIGHUP.String() {
		t.Errorf("exit = %+v, want it ended by %v", exit, syscall.SIGHUP)
	}
	// Well under the grace period, which is the whole point.
	if took > StopGrace/2 {
		t.Errorf("stopping it took %s; something that goes on SIGHUP should not wait out the grace", took)
	}
}

func TestStopOnAnAlreadyDeadProcessIsHarmless(t *testing.T) {
	p, err := Start(Service{Name: "quick", Command: "true"}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	first := p.Wait()
	second := p.Stop()
	if first != second {
		t.Errorf("Stop changed the recorded exit from %+v to %+v", first, second)
	}
}

// Writing to a dead process must say what is wrong, not return an opaque EIO.
func TestWriteAfterExitReportsProcessGone(t *testing.T) {
	p, err := Start(Service{Name: "quick", Command: "true"}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	p.Wait()

	if _, err := p.Write([]byte("anyone there?")); !errors.Is(err, ErrProcessGone) {
		t.Errorf("Write after exit = %v, want ErrProcessGone", err)
	}
	if err := p.Resize(80, 24); !errors.Is(err, ErrProcessGone) {
		t.Errorf("Resize after exit = %v, want ErrProcessGone", err)
	}
}

func TestServiceDirAndEnvAreApplied(t *testing.T) {
	dir := t.TempDir()
	// Resolve symlinks: on macOS TempDir is under /var, which is a link to /private/var, and
	// pwd reports the resolved path.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	p, err := Start(Service{
		Name:    "env",
		Command: "sh",
		Args:    []string{"-c", "pwd; echo marker=$GOZELLIJ_TEST_MARKER"},
		Dir:     dir,
		Env:     []string{"GOZELLIJ_TEST_MARKER=present"},
	}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	p.Wait()

	out, _ := p.Output.Snapshot()
	got := string(out)
	if !strings.Contains(got, resolved) {
		t.Errorf("working directory not applied: output %q does not contain %q", got, resolved)
	}
	if !strings.Contains(got, "marker=present") {
		t.Errorf("environment not applied: output %q", got)
	}
}

// The output of a process nobody is watching must keep being drained, or the child blocks in
// write() once the pty buffer fills. This is the single most important property in this file.
func TestUnwatchedChattyProcessIsNotWedged(t *testing.T) {
	p, err := Start(Service{
		Name:    "chatty",
		Command: "sh",
		// Far more than a pty buffer holds.
		Args: []string{"-c", "i=0; while [ $i -lt 2000 ]; do echo 'line of output to fill the buffer'; i=$((i+1)); done; echo FINISHED"},
	}, StartOptions{OutputBytes: 4096})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	done := make(chan Exit, 1)
	go func() { done <- p.Wait() }()

	select {
	case exit := <-done:
		if !exit.Clean() {
			t.Errorf("exit = %+v, want clean", exit)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a chatty process with nobody attached never finished - it was wedged writing to a full pty")
	}

	// The ring is far smaller than the output, so only the tail survives - and the tail is what
	// is on screen, which is the part that matters.
	out, _ := p.Output.Snapshot()
	if !strings.Contains(string(out), "FINISHED") {
		t.Errorf("the newest output was not kept; tail was %q", lastN(out, 80))
	}
	if len(out) > 4096 {
		t.Errorf("buffer holds %d bytes, want at most 4096", len(out))
	}
}

func TestAttachReceivesLiveProcessOutput(t *testing.T) {
	p, err := Start(Service{
		Name:    "ticker",
		Command: "sh",
		Args:    []string{"-c", "i=0; while [ $i -lt 20 ]; do echo tick-$i; i=$((i+1)); sleep 0.05; done"},
	}, StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	_, sub, err := p.Output.Attach(1 << 16)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer sub.Detach()

	var seen strings.Builder
	deadline := time.After(20 * time.Second)
	for {
		select {
		case chunk, ok := <-sub.C():
			if !ok {
				t.Fatalf("subscription closed before the expected output; saw %q", seen.String())
			}
			seen.Write(chunk)
			sub.Consumed(len(chunk))
			if strings.Contains(seen.String(), "tick-19") {
				return
			}
		case <-deadline:
			t.Fatalf("did not see the live output in time; saw %q", seen.String())
		}
	}
}

func lastN(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

func TestMain(m *testing.M) {
	// Re-exec as a well-behaved child when asked. A test that needs a process which catches
	// SIGTERM, takes a measurable moment over it and then records that it finished cannot use a
	// shell for it: a shell defers a trap until the command it is running returns, may or may
	// not re-enter the handler, and differs between dash and bash. Chasing a test that failed
	// one run in three led here, and the flakiness was the shell's, not the fabric's.
	if done := os.Getenv("GOZELLIJ_TEST_SLOW_CHILD"); done != "" {
		slowChild(done)
		return
	}

	// Keep the tests honest about inheriting the developer's environment.
	os.Unsetenv("GOZELLIJ_TEST_MARKER")
	os.Exit(m.Run())
}

// slowChild waits for SIGTERM, spends half a second shutting down, and records that it got to
// finish. It is the thing a graceful stop is supposed to protect.
//
// It announces itself ready *after* installing the handler, and the test waits for that. Without
// it the test is a race it loses about half the time: a signal that arrives before Notify runs
// takes the default action and kills the process outright, so the test would be measuring how
// fast a Go runtime starts rather than whether the stop was graceful.
func slowChild(done string) {
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)

	if err := os.WriteFile(done+".ready", []byte("ready"), 0o600); err != nil {
		os.Exit(1)
	}

	<-term
	time.Sleep(500 * time.Millisecond)
	_ = os.WriteFile(done, []byte("finished"), 0o600)
	os.Exit(0)
}
