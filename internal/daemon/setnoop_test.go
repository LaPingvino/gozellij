package daemon

import (
	"testing"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Pending means "the process you have is still the previous definition, restart it to pick this
// up". It used to mean "the service is running", which is a different thing: setting a value to
// the one it already holds asked for a restart nothing would change, and a restart costs you the
// process. The CLI refuses a set with no flags; this is the case it cannot see, because flags
// were given and only the daemon holds the values they are being compared against.
func TestSetAsksForARestartOnlyWhenSomethingChanged(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)

	if _, err := c.Add("svc", ipc.AddRequest{
		Command: "sleep", Args: []string{"300"}, Restart: "no", Start: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	always, no := "always", "no"
	changed, err := c.Set("svc", ipc.SetRequest{Restart: &always})
	if err != nil {
		t.Fatalf("Set to always: %v", err)
	}
	if !changed.Pending {
		t.Fatal("a real change did not say the running process is still the old definition")
	}

	same, err := c.Set("svc", ipc.SetRequest{Restart: &always})
	if err != nil {
		t.Fatalf("Set to always again: %v", err)
	}
	if same.Pending {
		t.Fatal("setting a value to what it already is asked for a restart")
	}

	// Back the other way, so this cannot pass by always answering no after the first call.
	back, err := c.Set("svc", ipc.SetRequest{Restart: &no})
	if err != nil {
		t.Fatalf("Set back to no: %v", err)
	}
	if !back.Pending {
		t.Fatal("changing the value back did not say the running process is still the old one")
	}

	// A command and its arguments, which are the slice fields the signature exists for: a
	// field-by-field equality is what forgets one of these.
	cmd, args := "sleep", []string{"300"}
	sameCmd, err := c.Set("svc", ipc.SetRequest{Command: &cmd, Args: &args})
	if err != nil {
		t.Fatalf("Set the same command: %v", err)
	}
	if sameCmd.Pending {
		t.Fatal("setting the command to the one it already runs asked for a restart")
	}
	other := []string{"301"}
	diffArgs, err := c.Set("svc", ipc.SetRequest{Command: &cmd, Args: &other})
	if err != nil {
		t.Fatalf("Set different args: %v", err)
	}
	if !diffArgs.Pending {
		t.Fatal("changing only the arguments did not ask for a restart")
	}
}
