package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// feedTerminal drives the input state machine through a pipe, which behaves like a terminal for
// reading purposes without needing one.
func feedTerminal(t *testing.T, keys string) (*terminalInput, func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	input := startTerminalInput(r)
	go func() {
		w.WriteString(keys)
		w.Close()
	}()
	return input, func() {
		input.stop()
		r.Close()
	}
}

// collectData reads whatever the state machine decided belongs to the service, until it goes quiet.
func collectData(input *terminalInput, within time.Duration) string {
	var got strings.Builder
	deadline := time.After(within)
	for {
		select {
		case chunk := <-input.data:
			got.Write(chunk)
		case <-deadline:
			return got.String()
		}
	}
}

func TestOrdinaryKeystrokesGoToTheService(t *testing.T) {
	input, done := feedTerminal(t, "hello world")
	defer done()

	if got := collectData(input, 500*time.Millisecond); got != "hello world" {
		t.Errorf("service received %q, want %q", got, "hello world")
	}
}

func TestThePrefixKeyIsNotSentToTheService(t *testing.T) {
	// It is the one key that is ours. If it reached the far end it would also do whatever it
	// means there, which for some programs is something.
	input, done := feedTerminal(t, "ab\x1dd")
	defer done()

	select {
	case want := <-input.cmds:
		if want != outcomeDetached {
			t.Errorf("command = %v, want detach", want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl-] d did not ask for a detach")
	}
	if got := collectData(input, 200*time.Millisecond); got != "ab" {
		t.Errorf("service received %q, want only the bytes before the prefix", got)
	}
}

func TestDoublingThePrefixSendsALiteral(t *testing.T) {
	// Without this there would be no way to type Ctrl-] at the program you are attached to.
	input, done := feedTerminal(t, "x\x1d\x1dy")
	defer done()

	if got := collectData(input, 500*time.Millisecond); got != "x\x1dy" {
		t.Errorf("service received %q, want a literal Ctrl-] between x and y", got)
	}
}

func TestPrefixCommands(t *testing.T) {
	cases := []struct {
		keys string
		want attachOutcome
	}{
		{"\x1dd", outcomeDetached},
		{"\x1dn", outcomeNext},
		{"\x1d ", outcomeNext},
		{"\x1dp", outcomePrev},
	}
	for _, tc := range cases {
		t.Run(tc.keys, func(t *testing.T) {
			input, done := feedTerminal(t, tc.keys)
			defer done()
			select {
			case got := <-input.cmds:
				if got != tc.want {
					t.Errorf("command = %v, want %v", got, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%q produced no command", tc.keys)
			}
		})
	}
}

func TestAnUnknownPrefixCommandIsNotSentOnAsInput(t *testing.T) {
	// It says so on stderr instead. Forwarding it would type a stray character at the service,
	// and swallowing it silently is indistinguishable from a dropped keystroke.
	input, done := feedTerminal(t, "\x1dZtail")
	defer done()

	if got := collectData(input, 500*time.Millisecond); got != "tail" {
		t.Errorf("service received %q, want only %q", got, "tail")
	}
	select {
	case cmd := <-input.cmds:
		t.Errorf("an unknown command produced %v", cmd)
	default:
	}
}

func TestSwitchingWalksTheServicesInOrderAndWraps(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)

	for _, name := range []string{"beta", "alpha", "gamma"} {
		if _, err := c.Add(name, ipc.AddRequest{Command: "sleep", Args: []string{"300"}}); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}

	// Sorted by name, which is arbitrary but stable - a switcher whose order changed between
	// presses would be useless.
	steps := []struct {
		from    string
		forward bool
		want    string
	}{
		{"alpha", true, "beta"},
		{"beta", true, "gamma"},
		{"gamma", true, "alpha"}, // wraps
		{"alpha", false, "gamma"},
		{"beta", false, "alpha"},
	}
	for _, s := range steps {
		got, err := neighbourService(sock, s.from, s.forward)
		if err != nil {
			t.Fatalf("from %s forward=%v: %v", s.from, s.forward, err)
		}
		if got != s.want {
			t.Errorf("from %s forward=%v gave %s, want %s", s.from, s.forward, got, s.want)
		}
	}
}

func TestSwitchingWithOneServiceSaysSoRatherThanMoving(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)
	if _, err := c.Add("only", ipc.AddRequest{Command: "sleep", Args: []string{"300"}}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	_, err := neighbourService(sock, "only", true)
	if err == nil {
		t.Fatal("switching with one service succeeded; there is nowhere to go")
	}
	if !strings.Contains(err.Error(), "only") {
		t.Errorf("error does not name the service: %v", err)
	}
}

func TestSwitchingFromAServiceThatHasGoneLandsSomewhere(t *testing.T) {
	// The service you were watching can be removed while you watch it. Going to the first one
	// beats refusing to move and leaving the user stuck on something that no longer exists.
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := c.Add(name, ipc.AddRequest{Command: "sleep", Args: []string{"300"}}); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}

	got, err := neighbourService(sock, "vanished", true)
	if err != nil {
		t.Fatalf("neighbourService: %v", err)
	}
	if got != "alpha" {
		t.Errorf("landed on %q, want alpha", got)
	}
}
