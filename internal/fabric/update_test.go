package fabric

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

// A wrong flag is fixed in place, and takes effect at the restart - not by rm and add, which
// stopped the service and deleted its log.
func TestUpdateChangesTheDefinitionForTheNextStart(t *testing.T) {
	f, reg := newTestFabric(t)
	if err := f.Add(Service{Name: "u", Command: "sleep", Args: []string{"300"}}, true); err != nil {
		t.Fatal(err)
	}
	before := waitFabric(t, f, "u", 10*time.Second, "running", func(st Status) bool { return st.State == StateRunning })
	orig, _ := reg.Get("u")

	saved, err := f.Update("u", func(d *Service) {
		d.Args = []string{"200"}
		d.Restart = RestartAlways
		// Neither of these is Update's to change.
		d.Name = "hijacked"
		d.NoLog = true
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if saved.Name != "u" || saved.NoLog || !saved.CreatedAt.Equal(orig.CreatedAt) || saved.Enabled != orig.Enabled {
		t.Errorf("Update changed what it must not: %+v (was %+v)", saved, orig)
	}
	if _, err := reg.Get("hijacked"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("a name change slipped through Update: %v", err)
	}

	// Still the old process: the new definition waits for the next start.
	if st, _ := f.Status("u"); st.Pid != before.Pid {
		t.Errorf("Update disturbed the running process: pid %d then %d", before.Pid, st.Pid)
	}
	if err := f.Restart("u"); err != nil {
		t.Fatal(err)
	}
	waitFabric(t, f, "u", 10*time.Second, "running again", func(st Status) bool {
		return st.State == StateRunning && st.Pid != before.Pid
	})
	p, _ := f.Process("u")
	if p == nil || len(p.Service.Args) != 1 || p.Service.Args[0] != "200" {
		t.Errorf("the restarted process runs %+v, want sleep 200", p)
	}
}

func TestUpdateRefusesWhatItCannotSave(t *testing.T) {
	f, reg := newTestFabric(t)
	if _, err := f.Update("missing", func(*Service) {}); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("updating a missing service: %v", err)
	}
	if err := f.Add(Service{Name: "v", Command: "true"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Update("v", func(d *Service) { d.Command = "" }); err == nil {
		t.Error("an empty command was saved")
	}
	if d, _ := reg.Get("v"); d.Command != "true" {
		t.Errorf("a refused update changed the file: %+v", d)
	}
}

// The supervisor restarts a crashed process by itself, and that respawn is a "next start" too. It
// used the definition the supervisor was built with, so a changed command came back as the old one.
func TestUpdateReachesTheRespawnAfterACrash(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{Name: "w", Command: "sleep", Args: []string{"300"}, Restart: RestartAlways}, true); err != nil {
		t.Fatal(err)
	}
	st := waitFabric(t, f, "w", 10*time.Second, "running", func(st Status) bool { return st.State == StateRunning })
	if _, err := f.Update("w", func(d *Service) { d.Args = []string{"200"} }); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(st.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFabric(t, f, "w", 20*time.Second, "respawned", func(s Status) bool {
		return s.State == StateRunning && s.Pid != st.Pid && s.Pid != 0
	})
	p, _ := f.Process("w")
	if p == nil || len(p.Service.Args) != 1 || p.Service.Args[0] != "200" {
		t.Errorf("the respawned process runs %+v, want sleep 200", p)
	}
}

// And the case a restart policy exists for: set to always on a running service, then it crashes.
func TestARestartPolicySetWhileRunningAppliesToTheCrash(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{Name: "x", Command: "sleep", Args: []string{"300"}}, true); err != nil {
		t.Fatal(err)
	}
	st := waitFabric(t, f, "x", 10*time.Second, "running", func(st Status) bool { return st.State == StateRunning })
	if _, err := f.Update("x", func(d *Service) { d.Restart = RestartAlways }); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(st.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	got := waitFabric(t, f, "x", 20*time.Second, "back or gone", func(s Status) bool {
		return (s.State == StateRunning && s.Pid != st.Pid && s.Pid != 0) || s.Ended
	})
	if got.Ended {
		t.Errorf("restart=always was set and the crash ended it for good: %+v", got)
	}
}
