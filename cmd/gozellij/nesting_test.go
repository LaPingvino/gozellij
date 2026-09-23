package main

import (
	"os"
	"strings"
	"testing"
)

// Attaching from inside a gozellij service. Found in use: `gozellij attach shell` typed inside
// shell drew the pane's own output back into itself - "double rendering, shaking".

func TestAttachingAServiceToItselfIsRefused(t *testing.T) {
	t.Setenv("GOZELLIJ", "shell")
	err := refuseNesting("shell", 0, false, false)
	if err == nil {
		t.Fatal("attaching shell from inside shell was allowed")
	}
	// Refused even with -nest: there is no version of this that is not a feedback loop.
	if refuseNesting("shell", 0, false, true) == nil {
		t.Fatal("-nest allowed a service to be attached to itself")
	}
	// And it says what was probably wanted, since reviving a frozen pane is how this was found.
	if !strings.Contains(err.Error(), "u reconnects") {
		t.Errorf("the refusal does not say how to revive the pane: %v", err)
	}
}

func TestNestingIsRefusedByDefaultAndAllowedWithTheFlag(t *testing.T) {
	t.Setenv("GOZELLIJ", "shell")
	err := refuseNesting("work", 0, false, false)
	if err == nil {
		t.Fatal("attaching another service from inside one was allowed without -nest")
	}
	if !strings.Contains(err.Error(), "-nest") || !strings.Contains(err.Error(), " l ") {
		t.Errorf("the refusal should offer switching in place and the way to nest anyway: %v", err)
	}
	if refuseNesting("work", 0, false, true) != nil {
		t.Error("-nest did not allow it")
	}
}

func TestOutsideGozellijNothingIsRefused(t *testing.T) {
	t.Setenv("GOZELLIJ", "")
	if err := refuseNesting("shell", 0, false, false); err != nil {
		t.Errorf("an attach from outside gozellij was refused: %v", err)
	}
}

// The name is not the only evidence, and after a rename it is wrong: GOZELLIJ was frozen into the
// shell when it started. Being underneath the service's process is the same feedback loop whatever
// the variable says - including when it says nothing, or names another service.
func TestAttachingTheServiceYouAreRunningUnderIsRefusedWhateverItIsCalled(t *testing.T) {
	under := os.Getppid() // something this test genuinely runs beneath
	for _, inside := range []string{"", "old-name", "work"} {
		t.Setenv("GOZELLIJ", inside)
		if refuseNesting("new-name", under, true, false) == nil {
			t.Errorf("GOZELLIJ=%q: attaching the service this runs under was allowed", inside)
		}
		if err := refuseNesting("new-name", under, true, true); err == nil || !strings.Contains(err.Error(), "itself") {
			t.Errorf("GOZELLIJ=%q: -nest let a service be attached to itself: %v", inside, err)
		}
	}
	// And a service this does not run under is not caught by the tree check.
	t.Setenv("GOZELLIJ", "")
	if err := refuseNesting("elsewhere", 999999999, true, false); err != nil {
		t.Errorf("an unrelated pid was treated as an ancestor: %v", err)
	}
}

// Inside a shell renamed from "shell" to "work", GOZELLIJ still says shell. Bare gozellij there
// was refused as attaching shell to itself - a service that no longer existed. When the daemon
// answers, only the process tree decides that; the name is for when it cannot be asked.
func TestAStaleNameIsNotEvidenceWhenTheDaemonAnswered(t *testing.T) {
	t.Setenv("GOZELLIJ", "shell")
	err := refuseNesting("shell", 0, true, false)
	if err != nil && strings.Contains(err.Error(), "itself") {
		t.Errorf("a shell that no longer exists was called this one: %v", err)
	}
	if err == nil {
		t.Error("still inside a gozellij service, so this is nesting and should say so")
	}
	if refuseNesting("shell", 0, true, true) != nil {
		t.Error("-nest did not allow it")
	}
	// With no daemon to ask, the name is all there is, and it is still believed.
	if err := refuseNesting("shell", 0, false, true); err == nil || !strings.Contains(err.Error(), "itself") {
		t.Errorf("without a daemon the name no longer protects: %v", err)
	}
}
