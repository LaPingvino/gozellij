package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Renaming the service you are attached to must not end the attach.
//
// It did, and only running it attached showed it: the daemon looked the service up by name for
// every status change and every keystroke, so the notification the rename itself sent was answered
// "no such service" and the terminal said "[gozellij: shell exited]" while the shell went on running
// under its new name.
func TestAnAttachSurvivesItsServiceBeingRenamed(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)
	if _, err := c.Add("before", ipc.AddRequest{Command: "sh", Args: []string{"-i"}, Start: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	a, err := attachRaw(t, sock, "before", ipc.AttachRequest{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := dial(t, sock).Rename("before", "after"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	// And the client is told, because every later request it makes about this service - remove,
	// revive, reconnect - has to use the new name.
	if ev := eventOfKind(t, a, ipc.EventRenamed, 5*time.Second); ev.Service != "after" {
		t.Errorf("the rename event says the service is now %q, want after", ev.Service)
	}

	if err := a.Writer().WriteFrame(ipc.KindData, []byte("echo TYPED-$((6*7))\n")); err != nil {
		t.Fatalf("typing after the rename: %v", err)
	}
	collect(t, a, "TYPED-42", 5*time.Second, nil)

	if n := viewersOf(t, sock, "after"); n != 1 {
		t.Errorf("ls counts %d viewers under the new name, want 1", n)
	}

	// And when it does end, it ends under the name it has now.
	if err := a.Writer().WriteFrame(ipc.KindData, []byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	if msg := finishedMessage(t, a, 5*time.Second); !strings.Contains(msg, "after") {
		t.Errorf("the attach ended with %q, which does not name the service as it is now", msg)
	}
}

func viewersOf(t *testing.T, sock, name string) int {
	t.Helper()
	list, err := dial(t, sock).List()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list.Services {
		if s.Service == name {
			return s.Viewers
		}
	}
	t.Fatalf("%s is not in the list", name)
	return 0
}

// finishedMessage reads until the daemon says the service has finished, and returns what it said.
func finishedMessage(t *testing.T, c *Client, within time.Duration) string {
	t.Helper()
	return eventOfKind(t, c, ipc.EventFinished, within).Message
}

// eventOfKind reads until an event of that kind arrives.
func eventOfKind(t *testing.T, c *Client, kind string, within time.Duration) ipc.Event {
	t.Helper()
	_ = c.Conn().SetReadDeadline(time.Now().Add(within))
	defer c.Conn().SetReadDeadline(time.Time{})
	for {
		frame, payload, err := c.Reader().ReadFrame()
		if err != nil {
			t.Fatalf("no %s event before the stream ended: %v", kind, err)
		}
		if frame != ipc.KindEvent {
			continue
		}
		var ev ipc.Event
		if json.Unmarshal(payload, &ev) == nil && ev.Kind == kind {
			return ev
		}
	}
}
