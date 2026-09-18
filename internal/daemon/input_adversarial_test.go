package daemon

import (
	"testing"
	"time"
)

// command() flushes pending bytes to `data` and then sends to `cmds`; both channels are buffered.
// runSession's select has no ordering between the two cases, so when both are ready at once the
// Go spec picks one at random. This test shows that nothing in the channel structure prevents the
// command from being taken while the bytes typed *before* it are still queued - which, in
// AttachLoop, delivers them to the next service.
func TestAdversarialBytesBeforeACommandCanStillBeQueuedWhenTheCommandArrives(t *testing.T) {
	input, done := feedTerminal(t, "ab\x1dn")
	defer done()

	select {
	case got := <-input.cmds:
		if got != outcomeNext {
			t.Fatalf("command = %v, want next", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no command")
	}

	// The command has been consumed; what happened to "ab"?
	select {
	case chunk := <-input.data:
		t.Errorf("after taking the switch command, %q was still queued in data: a session that "+
			"took the command first will send it to the service it switches to", chunk)
	case <-time.After(200 * time.Millisecond):
		// The bytes were consumed before the command: not what this test shows.
	}
}
