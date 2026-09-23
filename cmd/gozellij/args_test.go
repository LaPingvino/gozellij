package main

import (
	"strings"
	"testing"
)

// Every subcommand, given nothing, has to say what it wants.
//
// `gozellij add` - which is what somebody types to find out what add takes - panicked with a Go
// stack trace, because the argument slice was indexed before the check that the arguments were
// there. The message that would have helped was four lines further down and never ran.
//
// A table over every command rather than a test for the one that broke: they are all the same
// shape, this is the first thing a person does with an unfamiliar CLI, and a stack trace is the
// least useful thing a program can print at somebody who is trying to learn it.
func TestEverySubcommandWithNoArgumentsSaysWhatItWants(t *testing.T) {
	// Not the ones that do something without arguments (ls, ping, doctor, stats, shell,
	// login-setup, upgrade): those talk to a daemon, and this test is about argument handling.
	for _, cmd := range []string{"add", "attach", "logs", "status", "start", "stop", "restart", "rm", "rename", "set"} {
		t.Run(cmd, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("`gozellij %s` panicked: %v", cmd, r)
				}
			}()
			err := run([]string{cmd})
			if err == nil {
				t.Fatalf("`gozellij %s` with no arguments reported success", cmd)
			}
			// And the message has to name what is missing, not merely fail.
			if !strings.Contains(err.Error(), "need") {
				t.Errorf("`gozellij %s` says %q, which does not say what it wants", cmd, err)
			}
		})
	}
}

func TestAddWithANameButNoCommandStillExplains(t *testing.T) {
	// The other half of the same slice: one argument, where the command should be.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	err := run([]string{"add", "web"})
	if err == nil || !strings.Contains(err.Error(), "name and a command") {
		t.Fatalf("got %v, want an explanation that a command is missing", err)
	}
}
