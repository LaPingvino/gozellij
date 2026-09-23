package main

import (
	"os"
	"testing"
)

// The /proc reading is checked by running it against a real machine, which is what it is for. What
// is checked here is the part that turns what it found into advice, because that is where a wrong
// answer is actively harmful rather than merely unhelpful.

func TestTheSessionYouAreSittingInIsRecognised(t *testing.T) {
	// The reason this matters: without it the advice at the bottom of `takeover` says
	// `kill <pid>` about the session the person is reading it in.
	holders := []ptyHolder{
		{Pid: 111, Name: "other", Terminal: []terminalIn{{Pts: 3}, {Pts: 9}}},
		{Pid: 222, Name: "mine", Terminal: []terminalIn{{Pts: 42}}},
	}
	mine, ok := ptsOfProcess(os.Getpid())
	if !ok {
		t.Skip("this test process is not on a pts")
	}
	holders[1].Terminal[0].Pts = mine
	if got := holderOfMyTerminal(holders); got != 222 {
		t.Fatalf("holderOfMyTerminal = %d, want 222 - the one holding pts %d", got, mine)
	}
	// And nothing is claimed when none of them holds it, which is the ordinary case when you run
	// this from outside every multiplexer on the machine.
	holders[1].Terminal[0].Pts = mine + 1000
	if got := holderOfMyTerminal(holders); got != 0 {
		t.Fatalf("holderOfMyTerminal = %d with no match, want 0", got)
	}
}

func TestASuggestedNameIsSomethingYouCouldType(t *testing.T) {
	// Service names become log file names, so they have to be plain. And they have to be
	// recognisable, or the advice is a list of old-1, old-2, old-3 that means nothing.
	for _, c := range []struct{ dir, want string }{
		{"/home/joop/esperanto-kurso-gae", "esperanto-kurso-gae"},
		{"/home/joop/My Project", "my-project"},
		{"/home/joop/weird!name", "weird-name"},
		{"/", "shell"},
	} {
		if got := suggestedName(terminalIn{Dir: c.dir}, 0); got != c.want {
			t.Errorf("suggestedName(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

func TestAShellIsNotReportedAsWhatIsRunningInItsOwnTerminal(t *testing.T) {
	// Otherwise every idle terminal looks busy, and the one with real work in it does not stand
	// out from the ones without.
	for _, cmd := range []string{"/bin/bash", "bash -l", "-bash", "/usr/bin/zsh", "sh"} {
		if !isShell(cmd) {
			t.Errorf("%q was not recognised as a shell", cmd)
		}
	}
	for _, cmd := range []string{"/opt/claude-code/bin/claude", "vim x.txt", "ssh host", "bashful"} {
		if isShell(cmd) {
			t.Errorf("%q was called a shell", cmd)
		}
	}
}
