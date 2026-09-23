package daemon

import (
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// The list says enough to choose by: which are running and for how long, and which another
// terminal is sitting in.
func TestPickLineSaysStateAgeAndOtherTerminals(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		s    ipc.StatusReply
		want string
	}{
		{ipc.StatusReply{Service: "shell", State: "running", StartedAt: now.Add(-3 * time.Hour)},
			"shell            running 3h"},
		{ipc.StatusReply{Service: "notes", State: "exited"}, "notes            exited"},
		{ipc.StatusReply{Service: "work", State: "running", StartedAt: now.Add(-90 * time.Second), Viewers: 1},
			"work             running 1m · open in 1 other terminal"},
		{ipc.StatusReply{Service: "pair", State: "running", StartedAt: now.Add(-72 * time.Hour), Viewers: 2},
			"pair             running 3d · open in 2 other terminals"},
	}
	for _, c := range cases {
		if got := pickLine(c.s, now); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
}

// The terminal that opened the list has just closed its own session, and must not appear in the
// list as somebody else. Asked for at once, it did about half the time.
func TestTheListDoesNotCountTheTerminalAskingForIt(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	if _, err := dial(t, sock).Add("here", ipc.AddRequest{Command: "sleep", Args: []string{"60"}, Start: true}); err != nil {
		t.Fatal(err)
	}
	miscounted := 0
	for i := 0; i < 30; i++ {
		a, err := Dial(sock)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Call(ipc.OpAttach, "here", ipc.AttachRequest{}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for viewersOf(t, sock, "here") != 1 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		a.Close()
		list, err := settledStatuses(sock, "here")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.Service == "here" && s.Viewers != 0 {
				miscounted++
			}
		}
	}
	if miscounted > 0 {
		t.Errorf("the closed session was listed as another terminal in %d of 30 lists", miscounted)
	}
}
