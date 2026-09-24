package fabric

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The mark is part of the definition, not the name: a shell tab renamed with Ctrl-] , still closes
// when you exit it. Recognising tabs by name was the first version, and a rename quietly opted out.
func TestCloseOnExitSurvivesARename(t *testing.T) {
	f, _ := newTestFabric(t)
	if err := f.Add(Service{Name: "shell-3", Command: "sleep", Args: []string{"30"}, CloseOnExit: true}, false); err != nil {
		t.Fatal(err)
	}
	if err := f.Rename("shell-3", "work"); err != nil {
		t.Fatal(err)
	}
	def, err := f.Definition("work")
	if err != nil || !def.CloseOnExit {
		t.Fatalf("after rename: %+v, %v; want CloseOnExit kept", def, err)
	}
}

// A closed tab keeps its log, and logs still answers for it - from the file, and saying so. But a
// service that exists is always answered as itself: one with logging off must not be answered from
// a file an earlier service of that name left.
func TestTheLogOfAClosedTabIsStillReadable(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatal(err)
	}
	logs := t.TempDir()
	f := NewFabric(r, StartOptions{LogDir: logs})
	t.Cleanup(f.Shutdown)

	if err := f.Add(Service{Name: "tab", Command: "sh", Args: []string{"-c", "echo LEFT-BEHIND"}}, true); err != nil {
		t.Fatal(err)
	}
	waitFabric(t, f, "tab", 10*time.Second, "exited", func(s Status) bool { return s.HasExited })
	if err := f.Remove("tab", true); err != nil {
		t.Fatal(err)
	}

	tail, err := f.Logs("tab", 0)
	if err != nil || !strings.Contains(string(tail.Data), "LEFT-BEHIND") || tail.Note == "" {
		t.Fatalf("logs of the closed tab: %q note=%q, %v", tail.Data, tail.Note, err)
	}

	// A name that never had a log is still unknown.
	if _, err := f.Logs("never", 0); !errors.Is(err, ErrNoSuchService) {
		t.Errorf("logs of a name with no file: %v, want ErrNoSuchService", err)
	}

	// A new service under that name, with logging off, is answered as itself - not from the file.
	if err := f.Add(Service{Name: "tab", Command: "sleep", Args: []string{"30"}, NoLog: true}, false); err != nil {
		t.Fatal(err)
	}
	tail, err = f.Logs("tab", 0)
	if err != nil || strings.Contains(string(tail.Data), "LEFT-BEHIND") {
		t.Errorf("a service with logging off was answered from an old file: %q, %v", tail.Data, err)
	}
}
