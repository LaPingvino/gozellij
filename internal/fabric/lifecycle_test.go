package fabric

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The first command pair anybody types. It used to write enabled: true, start nothing, and report
// success - design rule 1 in the most visible place there is.
func TestStopThenStartActuallyStartsItAgain(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{
		Name: "web", Command: "sh", Args: []string{"-c", "while :; do echo alive; sleep 0.2; done"},
		Restart: RestartAlways,
	}, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	first := waitFabric(t, f, "web", 15*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})

	if err := f.Stop("web"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st, _ := f.Status("web"); st.State != StateStopped {
		t.Fatalf("after Stop, state = %v, want stopped", st.State)
	}

	if err := f.Start("web"); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	second := waitFabric(t, f, "web", 15*time.Second, "running again", func(st Status) bool {
		return st.State == StateRunning
	})

	if second.Pid == 0 {
		t.Fatal("started again but there is no process")
	}
	if second.Pid == first.Pid {
		t.Errorf("the same pid %d came back from the dead", first.Pid)
	}
}

// Same for a service that gave up: start must revive it rather than reporting success and sulking.
func TestStartRevivesAFinishedService(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{
		Name: "oneshot", Command: "sh", Args: []string{"-c", "echo RAN"}, Restart: RestartNo,
	}, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, f, "oneshot", 15*time.Second, "finished", func(st Status) bool {
		return st.Finished()
	})

	if err := f.Start("oneshot"); err != nil {
		t.Fatalf("Start after it finished: %v", err)
	}

	// Counted in the output, not in TotalStarts. Reviving means a *fresh* supervisor, whose
	// counter starts at zero again - so a test that watched TotalStarts was satisfied by the
	// first run and would have passed if Start had done nothing at all. The output buffer is
	// carried across deliberately, so it is the thing that can see both runs.
	out, err := f.Output("oneshot")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		data, _ := out.Snapshot()
		if n := strings.Count(string(data), "RAN"); n >= 2 {
			return
		}
		if time.Now().After(deadline) {
			data, _ := out.Snapshot()
			t.Fatalf("the service was never run again; its output was %q", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Output survives a stop and start, so `logs` still shows what it said before you stopped it. An
// operator does not care whether the gap was a crash or their own hand.
func TestScrollbackSurvivesStopAndStart(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{
		Name: "talker", Command: "sh", Args: []string{"-c", "echo BEFORE_THE_STOP; sleep 30"},
	}, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, f, "talker", 15*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})
	out, err := f.Output("talker")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if snap, _ := out.Snapshot(); strings.Contains(string(snap), "BEFORE_THE_STOP") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := f.Stop("talker"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := f.Start("talker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFabric(t, f, "talker", 15*time.Second, "running again", func(st Status) bool {
		return st.State == StateRunning
	})

	after, err := f.Output("talker")
	if err != nil {
		t.Fatalf("Output after restart: %v", err)
	}
	snap, _ := after.Snapshot()
	if !strings.Contains(string(snap), "BEFORE_THE_STOP") {
		t.Errorf("scrollback was lost across stop/start; buffer is now %q", snap)
	}
}

// Design rule 5 says a service file can be repaired with a text editor. That promise is only kept
// if something actually re-reads it; `restart` used to build from the in-memory copy, so an edit
// was honoured by an upgrade and ignored by a restart. Half-working is worse than either.
func TestRestartRereadsAHandEditedDefinition(t *testing.T) {
	f, r := newTestFabric(t)
	if err := f.Add(Service{
		Name: "edited", Command: "sh", Args: []string{"-c", "echo ORIGINAL; sleep 30"},
		Restart: RestartAlways,
	}, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, f, "edited", 15*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})

	// Edit the file behind the daemon's back, exactly as somebody would at 3am.
	path := filepath.Join(r.Dir(), "edited.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the service file: %v", err)
	}
	edited := strings.Replace(string(b), "echo ORIGINAL; sleep 30", "echo EDITED_BY_HAND; sleep 30", 1)
	if edited == string(b) {
		t.Fatalf("test setup failed: could not find the command in %s", path)
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatalf("writing the edited file: %v", err)
	}

	if err := f.Restart("edited"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	waitFabric(t, f, "edited", 15*time.Second, "running the edited command", func(st Status) bool {
		return st.State == StateRunning
	})

	out, _ := f.Output("edited")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if snap, _ := out.Snapshot(); strings.Contains(string(snap), "EDITED_BY_HAND") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap, _ := out.Snapshot()
	t.Errorf("restart ignored the hand-edited definition; output was %q", snap)
}
