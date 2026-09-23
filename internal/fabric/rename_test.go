package fabric

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newLoggingFabric(t *testing.T) (*Fabric, *Registry, string) {
	t.Helper()
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	logs := t.TempDir()
	f := NewFabric(reg, StartOptions{LogDir: logs})
	t.Cleanup(f.Shutdown)
	return f, reg, logs
}

func waitForLog(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(path); strings.Contains(string(b), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("%s never said %q; it holds:\n%s", path, want, b)
}

// A service renamed after it has run: the old name is gone everywhere, the transcript went with
// it, and what the service says under its new name lands in that same file.
func TestRenameMovesTheDefinitionAndTheLog(t *testing.T) {
	f, reg, logs := newLoggingFabric(t)
	if err := f.Add(Service{Name: "before", Command: "sh", Args: []string{"-c", "echo FIRST-RUN"}}, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, f, "before", 10*time.Second, "finished", func(st Status) bool { return !st.Live() })
	waitForLog(t, LogPath(logs, "before"), "FIRST-RUN")

	if err := f.Rename("before", "after"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if _, err := f.Status("before"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("the old name still answers: %v", err)
	}
	if _, err := reg.Get("before"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("the old definition is still on disk: %v", err)
	}
	def, err := reg.Get("after")
	if err != nil || def.Name != "after" || def.Command != "sh" {
		t.Fatalf("the new definition is %+v, %v", def, err)
	}
	if _, err := os.Stat(LogPath(logs, "before")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old log is still there: %v", err)
	}
	waitForLog(t, LogPath(logs, "after"), "FIRST-RUN")

	// And the service runs under the new name, writing to the moved file - not recreating the old.
	if err := f.Start("after"); err != nil {
		t.Fatalf("Start after rename: %v", err)
	}
	waitFabric(t, f, "after", 10*time.Second, "finished again", func(st Status) bool {
		return !st.Live() && st.TotalStarts >= 1
	})
	b, _ := os.ReadFile(LogPath(logs, "after"))
	if strings.Count(string(b), "FIRST-RUN") < 2 {
		t.Errorf("the second run did not reach the moved log:\n%s", b)
	}
	if _, err := os.Stat(LogPath(logs, "before")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("running again recreated the old log: %v", err)
	}
	if names := f.List(); len(names) != 1 || names[0].Service != "after" {
		t.Errorf("ls shows %+v", names)
	}
}

// Every way a rename can be wrong is refused, and leaves things exactly as they were.
func TestRenameRefusals(t *testing.T) {
	f, reg, _ := newLoggingFabric(t)
	for _, name := range []string{"a", "b"} {
		if err := f.Add(Service{Name: name, Command: "true"}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Add(Service{Name: "live", Command: "sleep", Args: []string{"300"}}, true); err != nil {
		t.Fatal(err)
	}
	waitFabric(t, f, "live", 10*time.Second, "running", func(st Status) bool { return st.State == StateRunning })

	cases := []struct {
		from, to string
		want     error
	}{
		{"a", "b", ErrServiceExists},
		{"missing", "c", ErrNoSuchService},
		{"live", "c", ErrRenameRunning},
		{"a", "a", nil},
		{"a", "", nil},
		{"a", "has/slash", nil},
	}
	for _, c := range cases {
		err := f.Rename(c.from, c.to)
		if err == nil {
			t.Errorf("rename %q %q succeeded", c.from, c.to)
			continue
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("rename %q %q: %v, want %v", c.from, c.to, err, c.want)
		}
	}
	for _, name := range []string{"a", "b", "live"} {
		if _, err := reg.Get(name); err != nil {
			t.Errorf("%s was lost by a refused rename: %v", name, err)
		}
		if _, err := f.Status(name); err != nil {
			t.Errorf("%s no longer answers after a refused rename: %v", name, err)
		}
	}
	if st, _ := f.Status("live"); st.State != StateRunning {
		t.Errorf("a refused rename disturbed the running service: %s", st.State)
	}
	if _, err := reg.Get("c"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("a refused rename left a definition for c: %v", err)
	}
}

// A log left under the new name by an earlier rm -keep-logs is somebody else's transcript. Moving
// over it destroys it and leaving it has this service append to it, so the rename is refused.
func TestRenameDoesNotTouchALogItFindsThere(t *testing.T) {
	f, reg, logs := newLoggingFabric(t)
	if err := os.WriteFile(LogPath(logs, "taken"), []byte("SOMEBODY-ELSE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.Add(Service{Name: "mine", Command: "sh", Args: []string{"-c", "echo MINE"}}, true); err != nil {
		t.Fatal(err)
	}
	waitFabric(t, f, "mine", 10*time.Second, "finished", func(st Status) bool { return !st.Live() })
	waitForLog(t, LogPath(logs, "mine"), "MINE")

	err := f.Rename("mine", "taken")
	if err == nil || !strings.Contains(err.Error(), "still has a log") {
		t.Errorf("the rename did not refuse over an existing log: %v", err)
	}
	if _, err := reg.Get("mine"); err != nil {
		t.Errorf("the refused rename moved the service anyway: %v", err)
	}
	if b, _ := os.ReadFile(LogPath(logs, "taken")); string(b) != "SOMEBODY-ELSE\n" {
		t.Errorf("the existing log was changed:\n%s", b)
	}
	waitForLog(t, LogPath(logs, "mine"), "MINE")
}
