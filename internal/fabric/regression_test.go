package fabric

// Regressions found by an independent adversarial pass over the logs and shell commits.
//
// Each of these failed when it was written. They are kept as assertions of the fixed behaviour
// rather than as descriptions of the bugs, so that a future change that reintroduces one is caught
// by a test that says what should happen.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

	if err := f.Remove("x"); err != nil {
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
