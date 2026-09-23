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
