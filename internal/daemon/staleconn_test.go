package daemon

import (
	"testing"

	"github.com/LaPingvino/gozellij/internal/vt/grid"
)

// A pane outlives its connections, and a message from one it has moved on from is stale.
//
// This is the regression test for a crash seen in production: "frame exceeds the maximum size:
// peer announced 218959215 bytes", which is 0x0D0D0D6F - three carriage returns and an 'o', read
// out of somebody's terminal output as if it were a length.
//
// The sequence was: Ctrl-] n swaps a pane to another service, closing its connection and starting
// a reader on the new one; the old reader wakes from its blocked read, reports `gone`; the session
// treats that as this pane's connection having ended, reconnects, and starts a *second* reader.
// Two goroutines then call ReadFrame on one Reader, which ipc says is not safe, and their header
// and body reads interleave.
//
// No mutex would have fixed that - it was not unsynchronised access to one thing, it was two
// owners of one thing. The fix is that a reader owns its connection and every message says which
// connection it came from.

func TestAMessageFromAConnectionThePaneHasLeftIsIgnored(t *testing.T) {
	current := &Client{}
	previous := &Client{}
	p := &livePane{service: "shell", client: current, term: grid.New(20, 4)}

	said := ""
	note := func(msg string) { said = msg }
	colour := func(int) (string, bool) { return "", false }

	// The stale reader's parting message. Acting on this is what started the second reader.
	applyEvent("", paneEvent{pane: p, from: previous, gone: true}, nil, note, colour)

	if p.finished {
		t.Error("a stale disconnection marked the pane finished")
	}
	if said != "" {
		t.Errorf("a stale disconnection said %q to the user", said)
	}
}

func TestOutputFromTheOldServiceDoesNotLandInTheNewOne(t *testing.T) {
	// The same staleness, in the direction that would put one service's output on another's
	// screen: after a swap, the old connection may still have a frame in flight.
	current := &Client{}
	previous := &Client{}
	p := &livePane{service: "logs", client: current, term: grid.New(20, 4)}

	applyEvent("", paneEvent{pane: p, from: previous, data: []byte("FROM-THE-OLD-SERVICE")}, nil,
		func(string) {}, func(int) (string, bool) { return "", false })

	var seen string
	for _, row := range p.term.Snapshot() {
		for _, c := range row {
			seen += c.Content
		}
	}
	if len(seen) > 0 && seen[0] != ' ' {
		t.Fatalf("the old service's output was drawn in the pane: %q", seen)
	}
}

func TestAMessageFromThePanesOwnConnectionIsActedOn(t *testing.T) {
	// And the check does not simply drop everything, which would be a quieter version of the
	// same bug: a pane that never updates.
	current := &Client{}
	p := &livePane{service: "shell", client: current, term: grid.New(20, 4)}

	applyEvent("", paneEvent{pane: p, from: current, data: []byte("HELLO")}, nil,
		func(string) {}, func(int) (string, bool) { return "", false })

	var first string
	for _, c := range p.term.Snapshot()[0][:5] {
		first += c.Content
	}
	if first != "HELLO" {
		t.Fatalf("output from the pane's own connection was dropped: row 0 is %q", first)
	}
}
