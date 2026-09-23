package daemon

import (
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Removing a service somebody is attached to has to answer.
//
// Ctrl-] k did nothing, and the trail led here: the remove came back as EOF - "waiting for the
// answer to service.remove: EOF" - so the service stayed and the key looked dead. The same remove
// from the command line, with a client attached in another terminal, worked. The difference was
// that the key's attach had just closed its own connection before asking.
func TestRemovingAServiceRightAfterDetachingFromItAnswers(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)

	// An interactive shell ignores SIGTERM, so stopping it takes the whole grace period - the
	// five seconds that turned out to matter.
	if _, err := c.Add("victim", ipc.AddRequest{
		Command: "sh", Args: []string{"-i"}, Start: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// Attach, then hang up - which is what the session does before acting on the key.
	a, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Call(ipc.OpAttach, "victim", ipc.AttachRequest{Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	a.Close()

	r := dial(t, sock)
	start := time.Now()
	_, err = r.Remove("victim", false)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("removing a service just detached from failed after %s: %v", took, err)
	}
	t.Logf("removed in %s", took)
}
