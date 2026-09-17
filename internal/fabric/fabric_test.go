package fabric

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestFabric(t *testing.T) (*Fabric, *Registry) {
	t.Helper()
	r, err := NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	f := NewFabric(r, StartOptions{})
	t.Cleanup(f.Shutdown)
	return f, r
}

func waitFabric(t *testing.T, f *Fabric, name string, within time.Duration, what string, ok func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		st, err := f.Status(name)
		if err == nil && ok(st) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, _ := f.Status(name)
	t.Fatalf("timed out after %v waiting for %s; status was %+v", within, what, st)
	return st
}

func TestAddPersistsAndCanStart(t *testing.T) {
	f, r := newTestFabric(t)
	svc := Service{Name: "sleeper", Command: "sleep", Args: []string{"300"}}

	if err := f.Add(svc, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, f, "sleeper", 10*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})

	// It must be on disk, and marked as wanted.
	stored, err := r.Get("sleeper")
	if err != nil {
		t.Fatalf("Get from registry: %v", err)
	}
	if !stored.Enabled {
		t.Error("a service started via Add was not recorded as enabled")
	}
}

func TestAddWithoutStartingLeavesItStopped(t *testing.T) {
	f, r := newTestFabric(t)
	if err := f.Add(Service{Name: "idle", Command: "sleep", Args: []string{"300"}}, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	st, err := f.Status("idle")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != StateStopped {
		t.Errorf("State = %v, want stopped", st.State)
	}
	stored, _ := r.Get("idle")
	if stored.Enabled {
		t.Error("a service added without --start was recorded as enabled")
	}
}

func TestDuplicateAddIsRefused(t *testing.T) {
	f, _ := newTestFabric(t)
	svc := Service{Name: "dup", Command: "sleep", Args: []string{"300"}}
	if err := f.Add(svc, false); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := f.Add(svc, false); !errors.Is(err, ErrServiceExists) {
		t.Errorf("second Add = %v, want ErrServiceExists", err)
	}
}

func TestUnknownServiceIsReportedNotIgnored(t *testing.T) {
	f, _ := newTestFabric(t)
	for _, call := range []struct {
		name string
		err  error
	}{
		{"Start", f.Start("ghost")},
		{"Stop", f.Stop("ghost")},
		{"Restart", f.Restart("ghost")},
	} {
		if !errors.Is(call.err, ErrNoSuchService) {
			t.Errorf("%s(ghost) = %v, want ErrNoSuchService", call.name, call.err)
		}
	}
	if _, err := f.Status("ghost"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("Status(ghost) = %v, want ErrNoSuchService", err)
	}
	if _, err := f.Output("ghost"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("Output(ghost) = %v, want ErrNoSuchService", err)
	}
}

// The point of Enabled: a reboot must be uneventful. What was running comes back, what you
// stopped stays stopped.
func TestLoadRestoresWhatWasRunningAndLeavesTheRestAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "services")
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// First "boot": one service wanted, one deliberately not.
	first := NewFabric(r, StartOptions{})
	if err := first.Add(Service{Name: "wanted", Command: "sleep", Args: []string{"300"}}, true); err != nil {
		t.Fatalf("Add wanted: %v", err)
	}
	if err := first.Add(Service{Name: "unwanted", Command: "sleep", Args: []string{"300"}}, false); err != nil {
		t.Fatalf("Add unwanted: %v", err)
	}
	waitFabric(t, first, "wanted", 10*time.Second, "wanted to run", func(st Status) bool {
		return st.State == StateRunning
	})
	first.Shutdown()

	// Second "boot": same directory, brand new fabric.
	second := NewFabric(r, StartOptions{})
	defer second.Shutdown()
	if errs := second.Load(); len(errs) != 0 {
		t.Fatalf("Load reported problems: %v", errs)
	}

	waitFabric(t, second, "wanted", 10*time.Second, "wanted to come back", func(st Status) bool {
		return st.State == StateRunning
	})
	st, err := second.Status("unwanted")
	if err != nil {
		t.Fatalf("Status(unwanted): %v", err)
	}
	if st.State == StateRunning {
		t.Error("a service that was deliberately not started came back by itself after a reload")
	}
}

func TestStopSurvivesAReload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "services")
	r, _ := NewRegistry(dir)

	first := NewFabric(r, StartOptions{})
	if err := first.Add(Service{Name: "svc", Command: "sleep", Args: []string{"300"}}, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, first, "svc", 10*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})
	if err := first.Stop("svc"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	first.Shutdown()

	second := NewFabric(r, StartOptions{})
	defer second.Shutdown()
	if errs := second.Load(); len(errs) != 0 {
		t.Fatalf("Load: %v", errs)
	}
	time.Sleep(200 * time.Millisecond)
	st, _ := second.Status("svc")
	if st.State == StateRunning {
		t.Error("a service stopped on purpose restarted itself after a reload")
	}
}

// One broken file must not stop the other services coming up - and must not disappear either.
func TestLoadReportsBadDefinitionsButStartsTheGoodOnes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "services")
	r, _ := NewRegistry(dir)
	if err := r.Add(Service{Name: "good", Command: "sleep", Args: []string{"300"}, Enabled: true}); err != nil {
		t.Fatalf("Add good: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write broken: %v", err)
	}

	f := NewFabric(r, StartOptions{})
	defer f.Shutdown()
	errs := f.Load()
	if len(errs) != 1 {
		t.Fatalf("Load errors = %v, want exactly one", errs)
	}
	waitFabric(t, f, "good", 10*time.Second, "the good service to run", func(st Status) bool {
		return st.State == StateRunning
	})
}

func TestListIsSortedAndCoversEverything(t *testing.T) {
	f, _ := newTestFabric(t)
	for _, n := range []string{"web", "api", "db"} {
		if err := f.Add(Service{Name: n, Command: "sleep", Args: []string{"300"}}, false); err != nil {
			t.Fatalf("Add %s: %v", n, err)
		}
	}
	got := f.List()
	if len(got) != 3 {
		t.Fatalf("List returned %d services, want 3", len(got))
	}
	if got[0].Service != "api" || got[1].Service != "db" || got[2].Service != "web" {
		t.Errorf("List is not sorted: %v, %v, %v", got[0].Service, got[1].Service, got[2].Service)
	}
}

func TestRemoveStopsAndDeletes(t *testing.T) {
	f, r := newTestFabric(t)
	if err := f.Add(Service{Name: "doomed", Command: "sleep", Args: []string{"300"}}, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, f, "doomed", 10*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})

	if err := f.Remove("doomed"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := f.Status("doomed"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("Status after Remove = %v, want ErrNoSuchService", err)
	}
	if _, err := r.Get("doomed"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("definition survived Remove: %v", err)
	}
}

// A definition the fabric could not adopt must still be removable, or it becomes a file you can
// see and cannot get rid of.
func TestRemoveWorksForADefinitionWithNoSupervisor(t *testing.T) {
	f, r := newTestFabric(t)
	if err := r.Add(Service{Name: "orphan", Command: "sleep"}); err != nil {
		t.Fatalf("registry Add: %v", err)
	}
	if err := f.Remove("orphan"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := r.Get("orphan"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("definition survived: %v", err)
	}
}

// Restarting by hand should not inherit whatever backoff the previous failures built up.
func TestRestartClearsTheFlappingHistory(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{
		Name: "flapper", Command: "sh", Args: []string{"-c", "exit 1"}, Restart: RestartAlways,
	}, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, f, "flapper", 30*time.Second, "a few restarts", func(st Status) bool {
		return st.Restarts >= 2
	})

	if err := f.Restart("flapper"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	st, _ := f.Status("flapper")
	if st.Restarts > 1 {
		t.Errorf("Restarts = %d after a manual restart, want the history cleared", st.Restarts)
	}
}

func TestOutputAndProcessAreReachable(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{
		Name: "talker", Command: "sh", Args: []string{"-c", "echo hello from talker; sleep 300"},
	}, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, f, "talker", 10*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})

	out, err := f.Output("talker")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := out.Snapshot()
		if len(snap) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap, _ := out.Snapshot()
	if len(snap) == 0 {
		t.Error("no output captured for a running service")
	}

	p, err := f.Process("talker")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if p == nil || p.Pid() == 0 {
		t.Error("no live process for a running service")
	}
}

// Each service needs its own output, or every log in the system ends up in one stream.
func TestServicesDoNotShareOutput(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{Name: "one", Command: "sh", Args: []string{"-c", "echo I_AM_ONE; sleep 300"}}, true); err != nil {
		t.Fatalf("Add one: %v", err)
	}
	if err := f.Add(Service{Name: "two", Command: "sh", Args: []string{"-c", "echo I_AM_TWO; sleep 300"}}, true); err != nil {
		t.Fatalf("Add two: %v", err)
	}
	waitFabric(t, f, "one", 10*time.Second, "one running", func(st Status) bool { return st.State == StateRunning })
	waitFabric(t, f, "two", 10*time.Second, "two running", func(st Status) bool { return st.State == StateRunning })

	oneOut, _ := f.Output("one")
	twoOut, _ := f.Output("two")
	if oneOut == twoOut {
		t.Fatal("two services share one output buffer")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := oneOut.Snapshot()
		b, _ := twoOut.Snapshot()
		if len(a) > 0 && len(b) > 0 {
			if strings.Contains(string(a), "I_AM_TWO") || strings.Contains(string(b), "I_AM_ONE") {
				t.Fatal("output leaked between services")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("did not see output from both services")
}

func TestShutdownStopsEverythingAndIsIdempotent(t *testing.T) {
	f, _ := newTestFabric(t)
	for _, n := range []string{"a", "b", "c"} {
		if err := f.Add(Service{Name: n, Command: "sleep", Args: []string{"300"}}, true); err != nil {
			t.Fatalf("Add %s: %v", n, err)
		}
		waitFabric(t, f, n, 10*time.Second, n+" running", func(st Status) bool {
			return st.State == StateRunning
		})
	}

	done := make(chan struct{})
	go func() { f.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown did not finish; services are probably being stopped one at a time")
	}

	if _, err := f.Status("a"); !errors.Is(err, ErrFabricClosed) {
		t.Errorf("Status after Shutdown = %v, want ErrFabricClosed", err)
	}
	f.Shutdown() // must not panic or block
}
