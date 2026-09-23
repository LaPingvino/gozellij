package daemon

import (
	"testing"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Pending means "the process you have is still the previous definition, restart it to pick this
// up". It used to mean "the service is running", which is a different thing: setting a value to
// the one it already holds asked for a restart nothing would change, and a restart costs you the
// process.
//
// The first fix for that compared the definition before and after the call, which is a third
// question again and wrong in the opposite direction: set a value, forget to restart, set the
// same value again, and you were told nothing was pending while the old process was still
// running. Rule 1 inverted. The baseline is what the live process was started with.
func TestSetAsksForARestartWhileTheProcessIsTheOldDefinition(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)

	if _, err := c.Add("svc", ipc.AddRequest{
		Command: "sleep", Args: []string{"300"}, Restart: "no", Start: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	always, no := "always", "no"
	cmd, sameArgs, otherArgs := "sleep", []string{"300"}, []string{"301"}

	for _, step := range []struct {
		what string
		req  ipc.SetRequest
		want bool
	}{
		// The process was started with restart=no and `sleep 300`.
		{"setting a value to the one it already has", ipc.SetRequest{Restart: &no}, false},
		{"setting the command it already runs", ipc.SetRequest{Command: &cmd, Args: &sameArgs}, false},
		{"a real change", ipc.SetRequest{Restart: &always}, true},
		// The same set again. Nothing changed this time, but the process is still the old
		// definition, so there is still something a restart would pick up. This is the case the
		// before-and-after comparison got wrong.
		{"the same change a second time", ipc.SetRequest{Restart: &always}, true},
		// And back: the definition on disk now matches the running process again, so there is
		// nothing left to pick up - which also stops this passing by always answering true.
		{"putting it back to what is running", ipc.SetRequest{Restart: &no}, false},
		// Args alone, because they are a slice: the field a hand-written equality forgets.
		{"changing only the arguments", ipc.SetRequest{Command: &cmd, Args: &otherArgs}, true},
	} {
		reply, err := c.Set("svc", step.req)
		if err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		if reply.Pending != step.want {
			t.Fatalf("%s: pending=%v, want %v", step.what, reply.Pending, step.want)
		}
	}
}

// A service that is not running has nothing pending whatever the definition says: the next start
// reads what is on disk.
func TestSetOnAStoppedServiceAsksForNoRestart(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)

	if _, err := c.Add("idle", ipc.AddRequest{Command: "sleep", Args: []string{"300"}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	always := "always"
	reply, err := c.Set("idle", ipc.SetRequest{Restart: &always})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if reply.Pending {
		t.Fatal("a stopped service asked for a restart")
	}
}
