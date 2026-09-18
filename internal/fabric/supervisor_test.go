package fabric

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// instantBackoff replaces the supervisor's wait with one that fires immediately but still records
// what delay was asked for. Backoff reaches 30 seconds, and a test suite that actually waits that
// long is a test suite nobody runs.
func instantBackoff(s *Supervisor) *[]time.Duration {
	var (
		mu     sync.Mutex
		delays []time.Duration
	)
	s.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	return &delays
}

func waitStatus(t *testing.T, s *Supervisor, within time.Duration, what string, ok func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if st := s.Status(); ok(st) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	st := s.Status()
	t.Fatalf("timed out after %v waiting for %s; status was %+v", within, what, st)
	return st
}

func TestSupervisorRunsAndReportsRunning(t *testing.T) {
	s := NewSupervisor(Service{Name: "sleeper", Command: "sleep", Args: []string{"300"}}, StartOptions{})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	st := waitStatus(t, s, 10*time.Second, "the service to be running", func(st Status) bool {
		return st.State == StateRunning
	})
	if st.Pid == 0 {
		t.Error("running but no pid recorded")
	}
	if st.TotalStarts != 1 {
		t.Errorf("TotalStarts = %d, want 1", st.TotalStarts)
	}
	if s.Current() == nil {
		t.Error("Current() is nil while running")
	}
}

func TestSupervisorStartTwiceIsRefused(t *testing.T) {
	s := NewSupervisor(Service{Name: "sleeper", Command: "sleep", Args: []string{"300"}}, StartOptions{})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	if err := s.Start(context.Background()); err != ErrAlreadyStarted {
		t.Errorf("second Start = %v, want ErrAlreadyStarted", err)
	}
}

// RestartNo must run the thing once and then stop, not quietly resurrect it.
func TestRestartNoRunsOnce(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "once", Command: "sh", Args: []string{"-c", "echo ran once"}, Restart: RestartNo,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Wait()

	st := s.Status()
	if st.TotalStarts != 1 {
		t.Errorf("TotalStarts = %d, want 1", st.TotalStarts)
	}
	// Exited, not failed: `true` succeeded, and a supervisor that calls a clean exit a failure
	// makes every reader doubt the ones that really are.
	if st.State != StateExited {
		t.Errorf("State = %v, want exited (terminal)", st.State)
	}
	if !st.HasExited || !st.LastExit.Clean() {
		t.Errorf("LastExit = %+v (exited=%v), want a clean exit", st.LastExit, st.HasExited)
	}
}

// A clean exit under on-failure is taken at its word.
func TestRestartOnFailureAcceptsACleanExit(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "clean", Command: "true", Restart: RestartOnFailure,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Wait()

	if st := s.Status(); st.TotalStarts != 1 || st.State != StateExited {
		t.Errorf("status = %+v, want one start and a clean terminal state", st)
	}
}

// A non-zero exit under restart: no is a failure, and stays one.
func TestRestartNoCallsABadExitAFailure(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "bad", Command: "false", Restart: RestartNo,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Wait()

	if st := s.Status(); st.State != StateFailed {
		t.Errorf("State = %v, want failed", st.State)
	}
}

func TestRestartOnFailureRestartsAFailure(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "flaky", Command: "sh", Args: []string{"-c", "exit 1"}, Restart: RestartOnFailure,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	waitStatus(t, s, 20*time.Second, "several restarts", func(st Status) bool {
		return st.TotalStarts >= 3
	})
	if st := s.Status(); st.LastExit.Code != 1 {
		t.Errorf("LastExit = %+v, want code 1", st.LastExit)
	}
}

// The delays must grow. This is the property that stops a crash loop becoming a busy loop.
func TestBackoffGrowsAcrossRestarts(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "crasher", Command: "sh", Args: []string{"-c", "exit 1"}, Restart: RestartAlways,
	}, StartOptions{})
	delays := instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitStatus(t, s, 20*time.Second, "four starts", func(st Status) bool {
		return st.TotalStarts >= 4
	})
	s.Stop()

	got := *delays
	if len(got) < 3 {
		t.Fatalf("recorded %d delays, want at least 3: %v", len(got), got)
	}
	if got[0] != BackoffFirst {
		t.Errorf("first delay = %v, want %v", got[0], BackoffFirst)
	}
	for i := 1; i < len(got) && got[i-1] < BackoffMax; i++ {
		if got[i] <= got[i-1] {
			t.Errorf("delay %d (%v) did not grow beyond %d (%v); all: %v", i, got[i], i-1, got[i-1], got)
			break
		}
	}
}

// A service whose binary does not exist must not sit silently in backing-off forever. Design rule 1.
func TestMissingBinaryIsReportedNotSilent(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "ghost", Command: "definitely-not-a-real-binary-xyzzy", Restart: RestartAlways,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	st := waitStatus(t, s, 10*time.Second, "the start failure to be reported", func(st Status) bool {
		return st.LastError != ""
	})
	if !strings.Contains(st.LastError, "definitely-not-a-real-binary-xyzzy") {
		t.Errorf("LastError = %q, want it to name the command", st.LastError)
	}
	if st.TotalStarts != 0 {
		t.Errorf("TotalStarts = %d, want 0 - nothing ever started", st.TotalStarts)
	}
}

// ...and under a policy that is not "always", a typo is terminal rather than an infinite retry.
func TestMissingBinaryIsTerminalUnlessRestartAlways(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "ghost", Command: "definitely-not-a-real-binary-xyzzy", Restart: RestartOnFailure,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Wait()

	st := s.Status()
	if st.State != StateFailed {
		t.Errorf("State = %v, want failed", st.State)
	}
	if st.LastError == "" {
		t.Error("LastError is empty; the reason must survive into the terminal state")
	}
}

func TestStopEndsASupervisedService(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "sleeper", Command: "sleep", Args: []string{"300"}, Restart: RestartAlways,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitStatus(t, s, 10*time.Second, "running", func(st Status) bool { return st.State == StateRunning })

	s.Stop()

	if st := s.Status(); st.State != StateStopped {
		t.Errorf("State = %v, want stopped", st.State)
	}
	if s.Current() != nil {
		t.Error("Current() is not nil after Stop")
	}
	// Stopping deliberately must not trigger the restart policy.
	before := s.Status().TotalStarts
	time.Sleep(100 * time.Millisecond)
	if after := s.Status().TotalStarts; after != before {
		t.Errorf("TotalStarts went %d -> %d after Stop; a deliberate stop restarted the service", before, after)
	}
}

func TestStopIsSafeOnANeverStartedSupervisor(t *testing.T) {
	s := NewSupervisor(Service{Name: "idle", Command: "true"}, StartOptions{})
	s.Stop() // must not panic or block
	s.Wait()
	if st := s.Status(); st.State != StateStopped {
		t.Errorf("State = %v, want stopped", st.State)
	}
}

func TestCancellingTheContextStopsSupervision(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := NewSupervisor(Service{
		Name: "sleeper", Command: "sleep", Args: []string{"300"}, Restart: RestartAlways,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitStatus(t, s, 10*time.Second, "running", func(st Status) bool { return st.State == StateRunning })

	cancel()
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("cancelling the context did not end supervision")
	}
}

// A viewer attached across a restart should see one continuous stream, including a note saying
// what happened - not a pane that mysteriously starts again from the top.
func TestScrollbackSurvivesRestartAndSaysWhy(t *testing.T) {
	s := NewSupervisor(Service{
		Name:    "phoenix",
		Command: "sh",
		Args:    []string{"-c", "echo burning; exit 1"},
		Restart: RestartAlways,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// Wait for the *output* of the second run, not merely for the start counter: a process that
	// has been spawned has not necessarily printed anything yet, and asserting on the counter
	// then reading the buffer is a race that fails about as often as the scheduler feels like.
	waitFor(t, 20*time.Second, "the second run to print", func() bool {
		out, _ := s.Output().Snapshot()
		return strings.Count(string(out), "burning") >= 2
	})

	out, _ := s.Output().Snapshot()
	text := string(out)
	if strings.Count(text, "burning") < 2 {
		t.Errorf("scrollback did not survive the restart; got %q", text)
	}
	if !strings.Contains(text, "restarting") {
		t.Errorf("the restart was not announced in the output; got %q", text)
	}
	if !strings.Contains(text, "phoenix") {
		t.Errorf("the restart notice does not name the service; got %q", text)
	}
}

func TestChangedSignalsStatusChanges(t *testing.T) {
	s := NewSupervisor(Service{Name: "sleeper", Command: "sleep", Args: []string{"300"}}, StartOptions{})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	changed, stop := s.Watch()
	defer stop()

	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("no change signal after starting a service")
	}
}

func TestEveryWatcherSeesEveryChange(t *testing.T) {
	// One shared channel would have delivered each wakeup to exactly one of these, which is how
	// an attached client sits through the exit of the service it is watching.
	s := NewSupervisor(Service{Name: "sleeper", Command: "sleep", Args: []string{"300"}}, StartOptions{})

	a, stopA := s.Watch()
	defer stopA()
	b, stopB := s.Watch()
	defer stopB()

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	for i, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("watcher %d never woke up", i)
		}
	}
}

func TestStopWatchingEndsTheChannel(t *testing.T) {
	s := NewSupervisor(Service{Name: "sleeper", Command: "true"}, StartOptions{})
	ch, stop := s.Watch()
	stop()
	stop() // twice, because a caller with a defer and an early return will do exactly this

	select {
	case _, open := <-ch:
		if open {
			t.Error("channel delivered a value after the watcher was cancelled")
		}
	case <-time.After(time.Second):
		t.Error("channel was not closed when the watcher was cancelled")
	}
}

func TestStateStringsAreReadable(t *testing.T) {
	want := map[State]string{
		StateStopped:    "stopped",
		StateRunning:    "running",
		StateBackingOff: "backing-off",
		StateFailed:     "failed",
	}
	for st, s := range want {
		if got := st.String(); got != s {
			t.Errorf("State(%d).String() = %q, want %q", int(st), got, s)
		}
	}
}
