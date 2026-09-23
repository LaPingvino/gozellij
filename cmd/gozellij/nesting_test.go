package main

import (
	"strings"
	"testing"
)

// Attaching from inside a gozellij service. Found in use: `gozellij attach shell` typed inside
// shell drew the pane's own output back into itself - "double rendering, shaking".

func TestAttachingAServiceToItselfIsRefused(t *testing.T) {
	t.Setenv("GOZELLIJ", "shell")
	err := refuseNesting("shell", false)
	if err == nil {
		t.Fatal("attaching shell from inside shell was allowed")
	}
	// Refused even with -nest: there is no version of this that is not a feedback loop.
	if refuseNesting("shell", true) == nil {
		t.Fatal("-nest allowed a service to be attached to itself")
	}
	// And it says what was probably wanted, since reviving a frozen pane is how this was found.
	if !strings.Contains(err.Error(), "u reconnects") {
		t.Errorf("the refusal does not say how to revive the pane: %v", err)
	}
}

func TestNestingIsRefusedByDefaultAndAllowedWithTheFlag(t *testing.T) {
	t.Setenv("GOZELLIJ", "shell")
	err := refuseNesting("work", false)
	if err == nil {
		t.Fatal("attaching another service from inside one was allowed without -nest")
	}
	if !strings.Contains(err.Error(), "-nest") || !strings.Contains(err.Error(), " l ") {
		t.Errorf("the refusal should offer switching in place and the way to nest anyway: %v", err)
	}
	if refuseNesting("work", true) != nil {
		t.Error("-nest did not allow it")
	}
}

func TestOutsideGozellijNothingIsRefused(t *testing.T) {
	t.Setenv("GOZELLIJ", "")
	if err := refuseNesting("shell", false); err != nil {
		t.Errorf("an attach from outside gozellij was refused: %v", err)
	}
}
