package fabric

// Regressions found by an independent adversarial pass over the logs and shell commits.
//
// Each of these failed when it was written. They are kept as assertions of the fixed behaviour
// rather than as descriptions of the bugs, so that a future change that reintroduces one is caught
// by a test that says what should happen.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A watcher is watching a *service*, not the supervisor that happens to be running it. `start` on a
// stopped service replaces the supervisor, and a watcher left behind on the old one is never told
// anything again - which is how an attached client sat through the exit it was waiting for.
func TestWatchSurvivesTheSupervisorBeingReplacedByStart(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{Name: "w", Command: "true", Restart: RestartNo}, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	changed, stop, err := f.Watch("w")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer stop()

	if err := f.Start("w"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForWatcher(t, f, "w", changed, func(st Status) bool { return st.Finished() },
		"the service ran and finished but the watcher never heard about it")
}

// The same, for restart - which replaces the supervisor too.
func TestWatchSurvivesRestart(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{
		Name: "w", Command: "sh", Args: []string{"-c", "sleep 30"}, Restart: RestartNo,
	}, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, f, "w", 15*time.Second, "running", func(st Status) bool { return st.State == StateRunning })

	changed, stop, err := f.Watch("w")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer stop()

	if err := f.Restart("w"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if err := f.Stop("w"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	waitForWatcher(t, f, "w", changed, func(st Status) bool { return st.Finished() },
		"the service was restarted and then stopped, and the watcher heard nothing")
}

// A plain `restart` must not look, even for an instant, like a service that has finished.
//
// Stopping the old supervisor before installing the new one published a status saying stopped,
// exited, killed by SIGTERM - true of the old process, false of the service - and an attached
// client believed it and let go, two milliseconds before the service ran again.
func TestRestartNeverLooksFinished(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{
		Name: "w", Command: "sh", Args: []string{"-c", "sleep 30"}, Restart: RestartNo,
	}, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, f, "w", 15*time.Second, "running", func(st Status) bool { return st.State == StateRunning })

	changed, stop, err := f.Watch("w")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := f.Restart("w"); err != nil {
			t.Errorf("Restart: %v", err)
		}
	}()

	// Watch every status the restart publishes. None of them may say the service is over.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-done:
			if st, _ := f.Status("w"); st.Finished() {
				t.Fatalf("after restart the service reads as finished: %+v", st)
			}
			return
		case <-changed:
			if st, _ := f.Status("w"); st.Finished() {
				t.Fatalf("a restart published a status that says the service finished: %+v", st)
			}
		case <-deadline:
			t.Fatal("restart did not complete")
		}
	}
}

// A service whose binary does not exist never runs, so it never "exits" - but its supervisor has
// given up, and anything waiting for it to finish must be told. Bare `gozellij` with a wrong
// $SHELL showed an empty terminal that never ended.
func TestAServiceThatCannotStartCountsAsFinished(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "missing", Command: "/nonexistent/program", Restart: RestartNo,
	}, StartOptions{})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Wait()

	st := s.Status()
	if st.LastError == "" {
		t.Error("no LastError for a service that could not be spawned")
	}
	if !st.Finished() {
		t.Errorf("status %+v is not Finished(), so an attach to it would never end", st)
	}
}

// A service that has never been started is stopped, but it is not finished: something attached to
// it is reasonably waiting for somebody to start it.
func TestANeverStartedServiceIsNotFinished(t *testing.T) {
	s := NewSupervisor(Service{Name: "idle", Command: "true"}, StartOptions{})
	if st := s.Status(); st.Finished() {
		t.Errorf("status %+v says finished before anything ran", st)
	}
}

// A rotation that fails is not transient: the file grows past its cap for ever afterwards. The
// next successful write must not erase the record of it.
func TestARotationFailureIsNotErasedByTheNextWrite(t *testing.T) {
	dir := t.TempDir()
	path := LogPath(dir, "noisy")

	out := NewOutputBuffer(4096)
	sink, err := NewLogSink(out, path, 200)
	if err != nil {
		t.Fatalf("NewLogSink: %v", err)
	}
	defer sink.Close()

	// A directory nothing can be renamed within.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	for i := 0; i < 30; i++ {
		if _, err := out.Write([]byte("0123456789\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if e := sink.Err(); strings.Contains(e, "rotate") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("rotation is failing but Err() is %q, so `gozellij status` would say nothing", sink.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Removing a service leaves its log file behind. A later service of the same name with logging off
// must not be answered from it: that is not a stale answer, it is somebody else's answer.
func TestAServiceWithLoggingOffIsNeverAnsweredFromAFile(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	logs := t.TempDir()
	f := NewFabric(reg, StartOptions{LogDir: logs})
	t.Cleanup(f.Shutdown)

	if err := f.Add(Service{Name: "x", Command: "true"}, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out, err := f.Output("x")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	out.Write([]byte("FIRST-SERVICE-SECRET\n"))
	waitForFile(t, LogPath(logs, "x"), "FIRST-SERVICE-SECRET")

	if err := f.Remove("x", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := f.Add(Service{Name: "x", Command: "true", NoLog: true}, false); err != nil {
		t.Fatalf("Add again: %v", err)
	}

	tail, err := f.Logs("x", 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if strings.Contains(string(tail.Data), "FIRST-SERVICE-SECRET") {
		t.Errorf("logs of a service with logging off returned the previous service's file: %q", tail.Data)
	}
	if tail.Path != "" {
		t.Errorf("Path = %q, want empty: nothing is being written for this service", tail.Path)
	}
}

// When the log writer is broken the file stops where the writing stopped, so the newest output is
// only in memory. Answer from there, and say so - handing over the older half with the confidence
// of a complete answer is the failure this project exists to avoid.
func TestABrokenLogWriterAnswersFromMemoryAndSaysSo(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	logs := t.TempDir()
	f := NewFabric(reg, StartOptions{LogDir: logs})
	t.Cleanup(f.Shutdown)

	if err := f.Add(Service{Name: "y", Command: "true"}, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out, err := f.Output("y")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	out.Write([]byte("OLD\n"))
	waitForFile(t, LogPath(logs, "y"), "OLD")

	// Break the writer the way a full disk would.
	out.Sink().Close()
	out.SetSinkError("disk full (simulated)")
	out.Write([]byte("NEW-ONLY-IN-MEMORY\n"))

	tail, err := f.Logs("y", 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(string(tail.Data), "NEW-ONLY-IN-MEMORY") {
		t.Errorf("logs = %q, want the output the file never received", tail.Data)
	}
	if tail.Err == "" {
		t.Error("no explanation that the log is not being written; an incomplete answer must say it is incomplete")
	}
}

// A subscriber that lagged has its channel closed to wake its reader, and used to stay in the
// buffer's map for ever - iterated on every write, one dead entry per lag.
func TestALaggedSubscriberIsDroppedFromTheBuffer(t *testing.T) {
	out := NewOutputBuffer(64 << 10)

	_, sub, err := out.Attach(16)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// Never read from sub, and write more than its budget.
	for i := 0; i < 8; i++ {
		out.Write([]byte("wwwwwwwwwwwwwwww"))
	}
	if !sub.Lagged() {
		t.Fatal("precondition: the subscriber should have lagged")
	}
	// One more write, which is when the pruning happens.
	out.Write([]byte("x"))

	if n := out.Subscribers(); n != 0 {
		t.Errorf("%d subscriber(s) still registered after one lagged and was closed", n)
	}
}

// waitForWatcher waits for a watcher to report a status matching want.
func waitForWatcher(t *testing.T, f *Fabric, name string, changed <-chan struct{}, want func(Status) bool, complaint string) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		if st, err := f.Status(name); err == nil && want(st) {
			return
		}
		select {
		case _, open := <-changed:
			if !open {
				t.Fatalf("%s (the watch channel closed)", complaint)
			}
		case <-deadline:
			st, _ := f.Status(name)
			t.Fatalf("%s; status was %+v", complaint, st)
		}
	}
}

// alive reports whether a pid still exists. Signal 0 checks without sending anything.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// Stopping a service must stop what the service started.
//
// A shell's children die of SIGHUP when the pty closes - unless they ignore it, and then they used
// to simply stay. Worse, one holding the pty slave open wedged the reader that reap waits for, so
// `gozellij stop` never returned at all.
func TestStoppingAServiceStopsWhatItStarted(t *testing.T) {
	s := NewSupervisor(Service{
		Name: "parent", Command: "sh",
		// A child that ignores SIGHUP and keeps the terminal open.
		Args:    []string{"-c", `trap "" HUP; (trap "" HUP; exec sleep 600) & echo CHILD=$!; wait`},
		Restart: RestartNo,
	}, StartOptions{})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var child int
	deadline := time.Now().Add(10 * time.Second)
	for child == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the service never reported its child's pid")
		}
		data, _ := s.Output().Snapshot()
		if _, after, ok := strings.Cut(string(data), "CHILD="); ok {
			if line, _, ok := strings.Cut(after, "\n"); ok {
				child, _ = strconv.Atoi(strings.TrimSpace(line))
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !alive(child) {
		t.Fatalf("precondition: child %d is not running", child)
	}
	t.Cleanup(func() {
		if alive(child) {
			syscall.Kill(child, syscall.SIGKILL)
		}
	})

	// Bounded, because the bug was an unbounded wait rather than a slow one.
	done := make(chan struct{})
	go func() { defer close(done); s.Stop() }()
	select {
	case <-done:
	case <-time.After(StopGrace + DrainAbandon + 10*time.Second):
		t.Fatal("Stop never returned; something is still holding the pty and reap is waiting for it")
	}

	for i := 0; i < 100 && alive(child); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(child) {
		t.Errorf("child %d outlived the service it belonged to", child)
	}
}

// The guard that keeps a process-group kill from reaching the daemon's own group.
func TestSignalGroupRefusesAProcessItDoesNotLead(t *testing.T) {
	// Our own process is (almost certainly) not a group leader, and even if it were, this must
	// not signal anything: the point is that it refuses rather than guesses.
	p := &Process{Service: Service{Name: "pretend"}, pid: os.Getpid()}
	err := p.SignalGroup(syscall.SIGTERM)
	if pgid, gerr := syscall.Getpgid(os.Getpid()); gerr == nil && pgid != os.Getpid() {
		if !errors.Is(err, ErrNotGroupLeader) {
			t.Errorf("SignalGroup = %v, want ErrNotGroupLeader for a process that leads no group", err)
		}
	}
}

// Removing a service removes its log, because for a shell that file is the complete transcript of
// everything typed at it. Leaving it behind is a surprise nobody asked for.
func TestRemoveDeletesTheServicesLog(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	logs := t.TempDir()
	f := NewFabric(reg, StartOptions{LogDir: logs})
	t.Cleanup(f.Shutdown)

	if err := f.Add(Service{Name: "shell", Command: "true"}, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out, err := f.Output("shell")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	out.Write([]byte("cat ~/.ssh/id_ed25519\n"))
	path := LogPath(logs, "shell")
	waitForFile(t, path, "id_ed25519")

	if files := f.LogFiles("shell"); len(files) == 0 {
		t.Error("LogFiles reported nothing, so `rm` could not tell the user what it is deleting")
	}
	if err := f.Remove("shell", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Stat = %v; the transcript outlived the service", err)
	}
}

// ...unless you say otherwise, which is what a redefinition wants: bare `gozellij` replaces a
// shell definition that is not running, and its history should carry across.
func TestRemoveKeepsTheLogWhenAsked(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	logs := t.TempDir()
	f := NewFabric(reg, StartOptions{LogDir: logs})
	t.Cleanup(f.Shutdown)

	if err := f.Add(Service{Name: "shell", Command: "true"}, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out, _ := f.Output("shell")
	out.Write([]byte("KEEP-ME\n"))
	path := LogPath(logs, "shell")
	waitForFile(t, path, "KEEP-ME")

	if err := f.Remove("shell", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the log was deleted although -keep-logs was asked for: %v", err)
	}
	if !strings.Contains(string(data), "KEEP-ME") {
		t.Errorf("log = %q, want the history kept", data)
	}
}

// Removing a service that was never adopted must still clean up after it - otherwise a definition
// too broken to load is also a definition whose log can never be removed.
func TestRemoveOfAnUnloadableServiceStillDeletesItsLog(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistry(filepath.Join(dir, "services"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	logs := t.TempDir()
	f := NewFabric(reg, StartOptions{LogDir: logs})
	t.Cleanup(f.Shutdown)

	// On disk but never adopted, so the fabric has no supervisor for it.
	if err := reg.Add(Service{Name: "orphaned", Command: "true"}); err != nil {
		t.Fatalf("reg.Add: %v", err)
	}
	path := LogPath(logs, "orphaned")
	if err := os.WriteFile(path, []byte("old output\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := f.Remove("orphaned", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Stat = %v; the log of an unloadable service cannot be removed", err)
	}
}

// Two terminals running bare `gozellij` at the same moment must not be able to delete each other's
// work. From the client this was check-then-act: both saw the shell defined and not running, both
// removed it, and one removed the *other's* freshly created service, whose attach then failed with
// "no such service". Measured with three at once before it was one operation.
func TestEnsureIsSafeFromSeveralTerminalsAtOnce(t *testing.T) {
	f, _ := newTestFabric(t)

	svc := Service{
		Name: "shell", Command: "sh", Args: []string{"-c", "sleep 30"}, Restart: RestartNo,
	}

	const racers = 12
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := f.Ensure(svc); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("Ensure failed while several terminals raced: %v", err)
	}

	if _, err := f.Status("shell"); err != nil {
		t.Fatalf("the service does not exist after %d racers ensured it: %v", racers, err)
	}
	st := waitFabric(t, f, "shell", 15*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})

	// And exactly one of them: a racer that redefined a service another had just started would
	// leave the first process running with nothing pointing at it.
	if st.TotalStarts != 1 {
		t.Errorf("TotalStarts = %d, want 1: more than one shell was spawned", st.TotalStarts)
	}
}

// A shell somebody is already using is theirs: ensuring it again must not restart it underneath
// them.
func TestEnsureLeavesARunningServiceAlone(t *testing.T) {
	f, _ := newTestFabric(t)

	svc := Service{Name: "shell", Command: "sh", Args: []string{"-c", "sleep 30"}, Restart: RestartNo}
	if err := f.Ensure(svc); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	first := waitFabric(t, f, "shell", 15*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})

	// A second terminal, with a different environment, arriving while it runs.
	svc.Env = []string{"TERM=vt100"}
	if err := f.Ensure(svc); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	st, err := f.Status("shell")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Pid != first.Pid {
		t.Errorf("pid changed from %d to %d: ensuring a running service restarted it", first.Pid, st.Pid)
	}
}

// A shell that is not running is redefined from the terminal in front of you, so that TERM and the
// locale belong to this session rather than to one three logins ago.
func TestEnsureUpdatesTheDefinitionOfAStoppedService(t *testing.T) {
	f, _ := newTestFabric(t)

	svc := Service{Name: "shell", Command: "sh", Args: []string{"-c", "sleep 30"}, Restart: RestartNo}
	svc.Env = []string{"TERM=ancient"}
	if err := f.Ensure(svc); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	waitFabric(t, f, "shell", 15*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})
	if err := f.Stop("shell"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	svc.Env = []string{"TERM=xterm-256color"}
	if err := f.Ensure(svc); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	def, err := f.Definition("shell")
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(def.Env) == 0 || !strings.Contains(def.Env[0], "xterm-256color") {
		t.Errorf("env = %v, want the terminal that ensured it most recently", def.Env)
	}
	// And it exists throughout: never removed, so nothing else can trip over a gap.
	if st, serr := f.Status("shell"); serr != nil {
		t.Errorf("the service went missing during the update: %v", st)
	}
}
