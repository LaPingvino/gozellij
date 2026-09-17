package fabric

// Adversarial tests for Supervisor, written independently of supervisor_test.go.
//
// Backoff is never waited for in real time: s.after is replaced either with a channel that fires
// at once (instantBackoff, from supervisor_test.go) or with one that never fires (advHeldBackoff,
// below) so a test can look at the backing-off state for as long as it likes.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// advHeldBackoff replaces the supervisor's wait with one that never fires and reports the delay it
// was asked for. The supervisor then sits in StateBackingOff until its context is cancelled, which
// is exactly the state Stop has to be able to get it out of.
func advHeldBackoff(s *Supervisor) *[]time.Duration {
	var (
		mu     sync.Mutex
		delays []time.Duration
	)
	s.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return make(chan time.Time) // never fires
	}
	return &delays
}

// advStopWithin runs Stop and fails if it does not return in time. A Stop that hangs would
// otherwise hang the whole package run rather than the one test.
func advStopWithin(t *testing.T, s *Supervisor, within time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("Stop did not return within %v; status %+v", within, s.Status())
	}
}

// Stop while the supervisor is *backing off* - no live process, just a pending timer. The loop is
// blocked in backOff's select; cancel must win it, the state must be Stopped, NextRestart cleared,
// and nothing must have been started in the meantime.
func TestAdvStopDuringBackingOffReallyStops(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "crasher", Command: "sh", Args: []string{"-c", "exit 3"}, Restart: RestartAlways,
	}, StartOptions{})
	delays := advHeldBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, s, 10*time.Second, "backing off", func(st Status) bool {
		return st.State == StateBackingOff
	})
	if st.Pid != 0 || !st.StartedAt.IsZero() {
		t.Errorf("backing off but Pid=%d StartedAt=%v", st.Pid, st.StartedAt)
	}
	if st.NextRestart.IsZero() {
		t.Error("backing off with a zero NextRestart")
	}
	if st.TotalStarts != 1 || st.Restarts != 1 {
		t.Errorf("TotalStarts=%d Restarts=%d, want 1 and 1", st.TotalStarts, st.Restarts)
	}
	if !st.HasExited || st.LastExit.Code != 3 {
		t.Errorf("LastExit=%+v HasExited=%v, want code 3", st.LastExit, st.HasExited)
	}
	if s.Current() != nil {
		t.Error("Current() non-nil while backing off")
	}

	advStopWithin(t, s, 10*time.Second)

	st = s.Status()
	if st.State != StateStopped {
		t.Errorf("State after Stop = %v, want stopped", st.State)
	}
	if !st.NextRestart.IsZero() {
		t.Errorf("NextRestart after Stop = %v, want zero", st.NextRestart)
	}
	if st.TotalStarts != 1 {
		t.Errorf("TotalStarts after Stop = %d; something was started during or after Stop", st.TotalStarts)
	}
	if got := *delays; len(got) != 1 || got[0] != BackoffFirst {
		t.Errorf("delays = %v, want exactly [%v]", got, BackoffFirst)
	}
	// Stop again and Wait: both must return at once on a stopped supervisor.
	advStopWithin(t, s, time.Second)
	s.Wait()
}

// The same for a spawn failure under RestartAlways: the supervisor backs off with nothing ever
// started, so Status must say so - not a pid, not a start count, but an error.
func TestAdvSpawnFailureBackingOffStatusIsHonest(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "ghost", Command: "definitely-not-a-real-binary-adv", Restart: RestartAlways,
	}, StartOptions{})
	advHeldBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, s, 10*time.Second, "backing off", func(st Status) bool {
		return st.State == StateBackingOff
	})
	if st.LastError == "" || !strings.Contains(st.LastError, "definitely-not-a-real-binary-adv") {
		t.Errorf("LastError = %q, want it to name the command", st.LastError)
	}
	if st.Pid != 0 || st.TotalStarts != 0 || st.HasExited {
		t.Errorf("nothing was ever started, but Pid=%d TotalStarts=%d HasExited=%v", st.Pid, st.TotalStarts, st.HasExited)
	}
	if st.NextRestart.IsZero() {
		t.Error("backing off with zero NextRestart")
	}
	if st.Restarts != 1 {
		t.Errorf("Restarts = %d, want 1 (one failed attempt)", st.Restarts)
	}
	// The failure is also visible in the output stream, where a person watching would look.
	out, _ := s.Output().Snapshot()
	if !strings.Contains(string(out), "restarting") {
		t.Errorf("the output does not announce the retry; got %q", out)
	}
	advStopWithin(t, s, 10*time.Second)
	if st := s.Status(); st.State != StateStopped || !st.NextRestart.IsZero() || st.LastError == "" {
		t.Errorf("after Stop: %+v; want stopped, NextRestart zero, LastError preserved", st)
	}
}

// Starting with an already-cancelled context must start nothing and say Stopped - not Failed,
// not Running, and with TotalStarts 0.
func TestAdvStartWithCancelledContextStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewSupervisor(Service{
		Name: "sleeper", Command: "sleep", Args: []string{"300"}, Restart: RestartAlways,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	st := s.Status()
	if st.State != StateStopped || st.TotalStarts != 0 || st.Pid != 0 {
		t.Errorf("status = %+v, want stopped with nothing started", st)
	}
	if s.Current() != nil {
		t.Error("Current() non-nil")
	}
	advStopWithin(t, s, time.Second)
}

// A supervisor cannot be restarted after Stop: Start must refuse loudly rather than appear to work
// while the loop is gone. (Pinned as documentation of the contract, not judged.)
func TestAdvStartAfterStopIsRefusedNotSilent(t *testing.T) {
	s := NewSupervisor(Service{Name: "once", Command: "true"}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	advStopWithin(t, s, time.Second)
	err := s.Start(context.Background())
	if !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("Start after Stop = %v; a Start that neither errors nor runs would be a silent no-op", err)
	}
	if st := s.Status(); st.TotalStarts != 1 {
		t.Errorf("TotalStarts = %d after refused restart, want 1", st.TotalStarts)
	}
}

// Under RestartNo a process that exits with a *signal* must also be terminal (Failed), with the
// signal recorded, and must not be restarted.
func TestAdvRestartNoOnSignalIsTerminalWithSignalRecorded(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "selfkill", Command: "sh", Args: []string{"-c", "kill -TERM $$"}, Restart: RestartNo,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	st := s.Status()
	if st.State != StateFailed || st.TotalStarts != 1 {
		t.Errorf("status = %+v, want failed after one start", st)
	}
	if !st.HasExited || st.LastExit.Signal == "" || st.LastExit.Clean() {
		t.Errorf("LastExit = %+v, want a signal recorded and not clean", st.LastExit)
	}
	if st.Pid != 0 || !st.StartedAt.IsZero() {
		t.Errorf("terminal state still carries Pid=%d StartedAt=%v", st.Pid, st.StartedAt)
	}
}

// Backoff reset is decided by `ranFor >= StableAfter`. The existing test covers exactly
// StableAfter; this covers one nanosecond short of it, which must *not* reset - a boundary that
// is easy to flip while refactoring the comparison.
func TestAdvOneNanosecondShortOfStableDoesNotReset(t *testing.T) {
	var tr RestartTracker
	tr.Died(0)
	tr.Died(0)
	tr.Died(0) // 4s
	if got := tr.Died(StableAfter - time.Nanosecond); got != 8*time.Second {
		t.Errorf("Died(StableAfter-1ns) = %v, want 8s (still flapping)", got)
	}
	if tr.Restarts != 4 {
		t.Errorf("Restarts = %d, want 4", tr.Restarts)
	}
	if got := tr.Died(StableAfter); got != BackoffFirst || tr.Restarts != 1 {
		t.Errorf("Died(StableAfter) = %v, Restarts %d; want %v and 1", got, tr.Restarts, BackoffFirst)
	}
	// A stable run followed by a stable run: each is a fresh first failure.
	if got := tr.Died(StableAfter * 10); got != BackoffFirst || tr.Restarts != 1 {
		t.Errorf("second stable death = %v, Restarts %d; want %v and 1", got, tr.Restarts, BackoffFirst)
	}
	// And a negative duration (clock went backwards) is treated as flapping, not as stable.
	if got := tr.Died(-time.Hour); got != 2*time.Second {
		t.Errorf("Died(-1h) = %v, want 2s", got)
	}
}

// Backoff is the supervisor's own business; the exit code of a crash must not change the delay
// schedule, and neither must the *policy*: on-failure and always must produce identical delays
// for identical crashes.
func TestAdvBackoffScheduleIsIndependentOfPolicyAndExitCode(t *testing.T) {
	var got [][]time.Duration
	for _, c := range []struct {
		policy RestartPolicy
		code   string
	}{{RestartAlways, "1"}, {RestartOnFailure, "1"}, {RestartAlways, "42"}, {RestartOnFailure, "255"}} {
		s := NewSupervisor(Service{
			Name: "c", Command: "sh", Args: []string{"-c", "exit " + c.code}, Restart: c.policy,
		}, StartOptions{})
		delays := instantBackoff(s)
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitStatus(t, s, 30*time.Second, "four starts", func(st Status) bool { return st.TotalStarts >= 4 })
		advStopWithin(t, s, 10*time.Second)
		d := append([]time.Duration(nil), (*delays)[:3]...)
		got = append(got, d)
	}
	want := []time.Duration{BackoffFirst, 2 * BackoffFirst, 4 * BackoffFirst}
	for i, d := range got {
		for j := range want {
			if d[j] != want[j] {
				t.Errorf("case %d: delays = %v, want prefix %v", i, d, want)
				break
			}
		}
	}
}

// TotalStarts must count exactly the spawns that happened: with a held backoff after the first
// crash it is exactly 1 for as long as we care to look, and Restarts matches.
func TestAdvTotalStartsDoesNotOvercountAcrossStatusReads(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "crasher", Command: "sh", Args: []string{"-c", "exit 1"}, Restart: RestartAlways,
	}, StartOptions{})
	advHeldBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, s, 10*time.Second, "backing off", func(st Status) bool { return st.State == StateBackingOff })
	for i := 0; i < 100; i++ {
		if st := s.Status(); st.TotalStarts != 1 {
			t.Fatalf("read %d: TotalStarts = %d, want 1", i, st.TotalStarts)
		}
	}
	advStopWithin(t, s, 10*time.Second)
}

// Every Status snapshot must be internally consistent. These are invariants that hold at *every*
// instant, so an observer spinning on Status() during a crash loop can never legitimately see them
// broken - and if it does, even once, that is a bug, not a scheduling accident.
//
//   - StateRunning implies Pid != 0 and a non-zero StartedAt.
//   - StateRunning implies NextRestart is zero: there is no restart pending while it runs.
//   - StateBackingOff implies Pid == 0 and NextRestart non-zero.
//   - StateStopped/StateFailed imply Pid == 0.
//
// This is a one-sided detector: a violation it reports is real; a clean run is not proof. Both
// violations it currently finds are reproducible every run on this machine (hundreds of hits per
// 30 restarts for the first, essentially continuous for the second).
func TestAdvStatusSnapshotsAreInternallyConsistent(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "crasher", Command: "sh", Args: []string{"-c", "exit 1"}, Restart: RestartAlways,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var (
		runningPidZero      atomic.Int64
		runningNextRestart  atomic.Int64
		backingOffWithPid   atomic.Int64
		backingOffNoNext    atomic.Int64
		terminalWithPid     atomic.Int64
		runningNoStartedAt  atomic.Int64
		exampleRunningPid0  atomic.Value
		exampleRunningNextR atomic.Value
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			st := s.Status()
			switch st.State {
			case StateRunning:
				if st.Pid == 0 {
					runningPidZero.Add(1)
					exampleRunningPid0.CompareAndSwap(nil, st)
				}
				if st.StartedAt.IsZero() {
					runningNoStartedAt.Add(1)
				}
				if !st.NextRestart.IsZero() {
					runningNextRestart.Add(1)
					exampleRunningNextR.CompareAndSwap(nil, st)
				}
			case StateBackingOff:
				if st.Pid != 0 {
					backingOffWithPid.Add(1)
				}
				if st.NextRestart.IsZero() {
					backingOffNoNext.Add(1)
				}
			case StateStopped, StateFailed:
				if st.Pid != 0 {
					terminalWithPid.Add(1)
				}
			}
		}
	}()
	waitStatus(t, s, 60*time.Second, "30 starts", func(st Status) bool { return st.TotalStarts >= 30 })
	close(stop)
	wg.Wait()
	advStopWithin(t, s, 10*time.Second)

	if n := backingOffWithPid.Load(); n != 0 {
		t.Errorf("StateBackingOff with a non-zero Pid seen %d times", n)
	}
	if n := backingOffNoNext.Load(); n != 0 {
		t.Errorf("StateBackingOff with a zero NextRestart seen %d times", n)
	}
	if n := terminalWithPid.Load(); n != 0 {
		t.Errorf("Stopped/Failed with a non-zero Pid seen %d times", n)
	}

	var found []string
	if n := runningPidZero.Load(); n != 0 {
		found = append(found, "StateRunning with Pid 0 (and StartedAt zero, HasExited true) seen "+
			fmt.Sprint(n)+" times, e.g. "+fmt.Sprintf("%+v", exampleRunningPid0.Load()))
	}
	if n := runningNextRestart.Load(); n != 0 {
		found = append(found, "StateRunning with a non-zero (stale, past) NextRestart seen "+
			fmt.Sprint(n)+" times, e.g. "+fmt.Sprintf("%+v", exampleRunningNextR.Load()))
	}
	if n := runningNoStartedAt.Load(); n != 0 && runningPidZero.Load() == 0 {
		found = append(found, "StateRunning with a zero StartedAt seen "+fmt.Sprint(n)+" times")
	}
	if len(found) > 0 {
		t.Error("FIXED BUG REGRESSED: " + strings.Join(found, "; ") + ". " +
			"(1) run() publishes the exit at supervisor.go:297-302 (Pid=0, StartedAt zero, HasExited) " +
			"while State is still StateRunning, and fires Changed() - so a watcher is invited to read " +
			"'running, pid 0'; the state is corrected only in a later setStatus. " +
			"(2) The StateRunning setStatus at :275-281 does not clear NextRestart, so after any " +
			"restart a running service carries the previous backoff deadline forever; server.go:453 " +
			"copies it onto the wire (omitempty does nothing for time.Time) and main.go:208 has to " +
			"guard with time.Until(...) > 0 to hide it.")
	}
}

// Deterministic companion to the detector above for the NextRestart case: it does not need a
// spinning observer, because the stale value persists for the whole time the service runs.
func TestAdvNextRestartIsClearedOnceRunningAgain(t *testing.T) {
	// A process that crashes once and then runs long: after the one restart it is running and
	// has no restart pending.
	s := NewSupervisor(Service{
		Name:    "once-then-long",
		Command: "sh",
		Args:    []string{"-c", "if [ -e \"$ADV_MARK\" ]; then exec sleep 300; fi; : > \"$ADV_MARK\"; exit 1"},
		Restart: RestartAlways,
		Env:     []string{"ADV_MARK=" + t.TempDir() + "/ran-once"},
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer advStopWithin(t, s, 15*time.Second)

	st := waitStatus(t, s, 20*time.Second, "running after one restart", func(st Status) bool {
		return st.State == StateRunning && st.TotalStarts == 2
	})
	if st.Pid == 0 {
		t.Fatalf("running with pid 0: %+v", st)
	}
	if st.Restarts != 1 {
		t.Errorf("Restarts = %d, want 1", st.Restarts)
	}
	if !st.NextRestart.IsZero() {
		t.Error("FIXED BUG REGRESSED: service is StateRunning (pid " + fmt.Sprint(st.Pid) + ") but Status.NextRestart = " +
			st.NextRestart.Format(time.RFC3339Nano) + " (in the past) - the StateRunning setStatus " +
			"never clears it. Doc says 'valid in StateBackingOff' but the value is still shipped over IPC " +
			"(daemon/server.go:453) and cmd/gozellij/main.go:208 has to guard it with time.Until > 0.")
	}
}

// The supervisor writes its restart notice into the output buffer with Fprintf and drops the
// result. With a borrowed, already-closed buffer that write fails - and nothing says so: the
// process is restarted with its output going nowhere. Rule 1 (minor: it requires the lender to
// have closed a buffer it lent out, which is itself a bug, but the supervisor is the one that
// notices and stays quiet).
func TestAdvRestartNoticeIntoClosedBufferIsSwallowed(t *testing.T) {
	out := NewOutputBuffer(1024)
	out.Close()
	s := NewSupervisor(Service{
		Name: "mute", Command: "sh", Args: []string{"-c", "echo hi; exit 1"}, Restart: RestartAlways,
	}, StartOptions{Output: out})
	advHeldBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// New contract: a service whose output buffer is closed is NOT restarted into a void. The
	// supervisor notices that the restart notice cannot be written - which means no viewer could
	// ever see this service again - and stops, saying why. Previously it swallowed the error and
	// kept restarting a process whose output went nowhere, with Status carrying no trace of it.
	st := waitStatus(t, s, 10*time.Second, "a terminal state", func(st Status) bool {
		return st.State == StateFailed
	})
	advStopWithin(t, s, 10*time.Second)
	if st.LastError == "" {
		t.Error("FIXED BUG REGRESSED: the output buffer was closed and Status carries no trace of it " +
			"(LastError empty); the supervisor must say why it stopped rather than looping in the dark")
	}
	if !strings.Contains(st.LastError, "output buffer") {
		t.Errorf("LastError = %q, want it to name the output buffer as the reason", st.LastError)
	}
}

// A running service must have a Current() whose Pid matches Status().Pid - two views of the same
// process that must not disagree, at any instant while it is running. Read both under one
// snapshot each and only compare when both say "running".
func TestAdvCurrentAndStatusAgreeOnThePid(t *testing.T) {
	s := NewSupervisor(Service{Name: "sleeper", Command: "sleep", Args: []string{"300"}}, StartOptions{})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer advStopWithin(t, s, 15*time.Second)
	st := waitStatus(t, s, 10*time.Second, "running", func(st Status) bool { return st.State == StateRunning })
	p := s.Current()
	if p == nil {
		t.Fatal("Current() nil while Status says running")
	}
	if p.Pid() != st.Pid {
		t.Errorf("Current().Pid() = %d, Status().Pid = %d", p.Pid(), st.Pid)
	}
	if !p.StartedAt().Equal(st.StartedAt) {
		t.Errorf("StartedAt disagrees: process %v, status %v", p.StartedAt(), st.StartedAt)
	}
	if st.HasExited {
		t.Error("HasExited true on the very first run")
	}
}

// Stop must leave the process actually dead, not merely the status saying so.
func TestAdvStopKillsTheProcessNotJustTheStatus(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "sleeper", Command: "sleep", Args: []string{"300"}, Restart: RestartAlways,
	}, StartOptions{})
	instantBackoff(s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, s, 10*time.Second, "running", func(st Status) bool { return st.State == StateRunning })
	p := s.Current()
	advStopWithin(t, s, 15*time.Second)
	if _, exited := p.Exited(); !exited {
		t.Fatal("Stop returned but the process it was supervising has not exited")
	}
	if st := s.Status(); !st.HasExited || st.LastExit.Signal == "" {
		t.Errorf("after Stop, LastExit = %+v (exited=%v); expected the SIGTERM to be recorded", st.LastExit, st.HasExited)
	}
}
