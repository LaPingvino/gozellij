package fabric

import (
	"errors"
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
