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

// A Ctrl still held from the prefix means the key it is held over.
//
// Found in use: Ctrl-B ? typed with Ctrl still down arrived as 0x1f and was reported as a key that
// does nothing.
func TestAHeldCtrlStillMeansTheKey(t *testing.T) {
	prefix := byte(0x02) // Ctrl-B, which is what this was found with
	for _, c := range []struct {
		name string
		key  byte
		want attachOutcome
	}{
		{"Ctrl-D", 0x04, outcomeDetached},
		{"Ctrl-N", 0x0e, outcomeNext},
		{"Ctrl-K", 0x0b, outcomeRemove},
		{"Ctrl-U", 0x15, outcomeRevive},
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		in := startTerminalInput(r, prefix)
		_, _ = w.Write([]byte{prefix, c.key})
		select {
		case got := <-in.cmds:
			if got != c.want {
				t.Errorf("prefix then %s arrived as %d, want %d", c.name, got, c.want)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("prefix then %s never arrived as a command", c.name)
		}
		in.stop()
		w.Close()
		r.Close()
	}
}

func TestTheUnctrlledKeysThatKeepTheirMeaning(t *testing.T) {
	in := &terminalInput{prefix: 0x02}
	// The prefix twice is still the literal prefix, not Ctrl-B read as b (scroll back).
	if got := in.unctrl(0x02); got != 0x02 {
		t.Errorf("the prefix pressed twice became %q", got)
	}
	// Ctrl-L is redraw and Tab is switch pane, and both were bound before any of this.
	if got := in.unctrl(0x0c); got != 0x0c {
		t.Errorf("Ctrl-L became %q; it is redraw", got)
	}
	if got := in.unctrl(0x09); got != 0x09 {
		t.Errorf("Tab became %q; it switches panes", got)
	}
	if got := in.unctrl(0x1f); got != '?' {
		t.Errorf("Ctrl-/ became %q, want ?", got)
	}
}
