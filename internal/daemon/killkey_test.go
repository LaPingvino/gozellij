package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/status"
)

// The keys that act on the service you are looking at have to reach the session as commands.
//
// k did nothing in either mode when this was written - no message, no removal - and the only way
// to tell whether the key was lost in the reader or in the session was to ask the reader alone.
func TestTheRemoveAndReviveKeysBecomeCommands(t *testing.T) {
	for _, c := range []struct {
		key  byte
		want attachOutcome
	}{
		{'k', outcomeRemove},
		{'K', outcomeRemove},
		{'u', outcomeRevive},
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		in := startTerminalInput(r, status.DefaultPrefix)
		if _, err := w.Write([]byte{status.DefaultPrefix, c.key}); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-in.cmds:
			if got != c.want {
				t.Errorf("prefix %q arrived as outcome %d, want %d", c.key, got, c.want)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("prefix %q never arrived as a command", c.key)
		}
		in.stop()
		w.Close()
		r.Close()
	}
}
