package daemon

import (
	"io"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// `gozellij rm` of a service somebody is following with logs -f must end the follow. It did not:
// the stream ended on the daemon's side and the handler then waited for a client with no reason
// to hang up - so the follower never returned, and went on being counted as a viewer, under a
// name a new service could then take.
func TestRemovingAFollowedServiceEndsTheFollow(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)
	if _, err := c.Add("x", ipc.AddRequest{Command: "sleep", Args: []string{"30"}, Start: true}); err != nil {
		t.Fatal(err)
	}
	follower := dial(t, sock)
	done := make(chan error, 1)
	go func() { done <- follower.FollowLogs("x", 0, io.Discard, io.Discard) }()

	deadline := time.Now().Add(5 * time.Second)
	for viewersOf(t, sock, "x") != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the follower was never counted")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := dial(t, sock).Remove("x", false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logs -f is still following a service that was removed")
	}

	// And a new service under the same name starts with nobody watching it.
	if _, err := dial(t, sock).Add("x", ipc.AddRequest{Command: "sleep", Args: []string{"30"}, Start: true}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for viewersOf(t, sock, "x") != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("a new x is counted with %d viewers it never had", viewersOf(t, sock, "x"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
