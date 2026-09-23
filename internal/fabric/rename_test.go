package fabric

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	cases := []struct {
		from, to string
		want     error
	}{
		{"a", "b", ErrServiceExists},
		{"missing", "c", ErrNoSuchService},
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
	for _, name := range []string{"a", "b"} {
		if _, err := reg.Get(name); err != nil {
			t.Errorf("%s was lost by a refused rename: %v", name, err)
		}
		if _, err := f.Status(name); err != nil {
			t.Errorf("%s no longer answers after a refused rename: %v", name, err)
		}
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

// The case rename exists for: a shell you are living in. It goes on running as the same process,
// what it says after the rename lands in the moved log exactly once, and the descriptor kept for
// a crash is filed under the new name - left under the old one, a crash would resurrect the
// service as something that no longer exists.
func TestRenameARunningService(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatal(err)
	}
	logs := t.TempDir()
	var mu sync.Mutex
	var events []string
	f := NewFabric(reg, StartOptions{
		LogDir: logs,
		OnRunning: func(name string, pid int, pty *os.File) {
			mu.Lock()
			events = append(events, fmt.Sprintf("running %s %d pty=%v", name, pid, pty != nil))
			mu.Unlock()
		},
		OnEnded: func(name string, pid int) {
			mu.Lock()
			events = append(events, fmt.Sprintf("ended %s %d", name, pid))
			mu.Unlock()
		},
	})
	t.Cleanup(f.Shutdown)

	// Prints a numbered line every 50ms, so a line written twice or lost at the seam shows.
	script := `i=0; while :; do i=$((i+1)); echo "TICK-$i"; sleep 0.05; done`
	if err := f.Add(Service{Name: "old", Command: "sh", Args: []string{"-c", script}}, true); err != nil {
		t.Fatal(err)
	}
	st := waitFabric(t, f, "old", 10*time.Second, "running", func(st Status) bool { return st.State == StateRunning })
	pid := st.Pid
	waitForLog(t, LogPath(logs, "old"), "TICK-5")

	if err := f.Rename("old", "new"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	now, err := f.Status("new")
	if err != nil || now.State != StateRunning || now.Pid != pid || now.Service != "new" {
		t.Fatalf("after the rename: %+v, %v; want the same process (%d) running as new", now, err, pid)
	}
	if _, err := f.Status("old"); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("the old name still answers: %v", err)
	}
	hs := f.Handovers()
	if len(hs) != 1 || hs[0].Name != "new" || hs[0].Pid != pid {
		t.Errorf("a crash now would hand over %+v", hs)
	}
	mu.Lock()
	got := strings.Join(events, "; ")
	mu.Unlock()
	want := fmt.Sprintf("running old %d pty=true; ended old %d; running new %d pty=true", pid, pid, pid)
	if got != want {
		t.Errorf("the descriptor store was told:\n  %s\nwant:\n  %s", got, want)
	}

	// Output from after the rename reaches the moved file, and every line is there exactly once.
	b, _ := os.ReadFile(LogPath(logs, "new"))
	last := strings.Count(string(b), "TICK-")
	waitForLog(t, LogPath(logs, "new"), fmt.Sprintf("TICK-%d\r\n", last+10))
	if _, err := os.Stat(LogPath(logs, "old")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old log is back: %v", err)
	}
	b, _ = os.ReadFile(LogPath(logs, "new"))
	seen := map[string]int{}
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n") {
		if strings.HasPrefix(line, "TICK-") {
			seen[line]++
		}
	}
	for i := 1; i <= last+10; i++ {
		if n := seen[fmt.Sprintf("TICK-%d", i)]; n != 1 {
			t.Errorf("TICK-%d is in the log %d times", i, n)
		}
	}
	if !strings.Contains(string(b), "renamed from old.log") {
		t.Errorf("nothing in the log marks the rename")
	}

	// Stopping it under its new name tells the store under its new name.
	if err := f.Stop("new"); err != nil {
		t.Fatal(err)
	}
	waitFabric(t, f, "new", 10*time.Second, "stopped", func(st Status) bool { return !st.Live() })
	mu.Lock()
	lastEvent := events[len(events)-1]
	mu.Unlock()
	if lastEvent != fmt.Sprintf("ended new %d", pid) {
		t.Errorf("stopping told the store %q", lastEvent)
	}
}

// A Handle names the service as it was last seen. After a rename and then a remove it used to fall
// back to the name it was followed under - one that had stopped existing two steps earlier - and
// that is the name an attached client's last message then used.
func TestAHandleRemembersTheLastNameAfterARemove(t *testing.T) {
	f, _, _ := newLoggingFabric(t)
	if err := f.Add(Service{Name: "before", Command: "true"}, false); err != nil {
		t.Fatal(err)
	}
	h, err := f.Follow("before")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Rename("before", "after"); err != nil {
		t.Fatal(err)
	}
	if got := h.Name(); got != "after" {
		t.Fatalf("Name after the rename = %q", got)
	}
	if err := f.Remove("after", false); err != nil {
		t.Fatal(err)
	}
	if got := h.Name(); got != "after" {
		t.Errorf("Name after rename then remove = %q, want after", got)
	}
	if _, err := h.Status(); !errors.Is(err, ErrNoSuchService) || !strings.Contains(err.Error(), "after") {
		t.Errorf("Status of a removed service: %v", err)
	}
}
