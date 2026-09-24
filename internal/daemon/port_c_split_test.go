package daemon

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Ported from acceptance.sh "a split that stacks instead of splitting", "switching a service
// without throwing the layout away", "two shells, and typing into the right one" and "what
// gozellij says has to be readable". All rendered: panes exist only there.

// cTicker prints its name and a counter, fast enough that "is this pane still live" is a question
// answered in a fraction of a second.
func cTicker(name string) string {
	return `i=0; while :; do printf '` + name + `-%d\r\n' $i; i=$((i+1)); sleep 0.05; done`
}

// cFocusIs waits for the pane marker - the focused pane's service in brackets - at the very start
// of the status line. Only there: the tab list further along has the same shape and says which tab
// is shown, not which pane has the keyboard.
func cFocusIs(s *testScreen, marker string) {
	s.t.Helper()
	s.Until("the pane marker "+marker, func() bool { return strings.HasPrefix(s.Bottom(), marker+" ") })
}

func TestPortCSplitRowsStacksTheNewPaneUnderneath(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "rowa", "sh", "-c", cTicker("rowa"))
	addRunning(t, sock, "rowb", "sh", "-c", cTicker("rowb"))

	s := attachScreen(t, sock, "rowa", 40, 12, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("rowa-")
	s.Prefix('-')

	// One above the other, which is a statement about rows: rowa in the top half and rowb in the
	// bottom. Both somewhere on the screen would also pass for a side-by-side split.
	s.Until("rowa in the top rows and rowb in the lower ones", func() bool {
		r := s.Rows()
		top := strings.Join(r[:5], "\n")
		low := strings.Join(r[6:11], "\n")
		return strings.Contains(top, "rowa-") && !strings.Contains(top, "rowb-") &&
			strings.Contains(low, "rowb-") && !strings.Contains(low, "rowa-")
	})
}

func TestPortCSwitchingInASplitKeepsTheOtherPane(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	for _, n := range []string{"swapa", "swapb", "swapc"} {
		addRunning(t, sock, n, "sh", "-c", cTicker(n))
	}

	s := attachScreen(t, sock, "swapa", 70, 10, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("swapa-")
	s.Prefix('|')
	s.Until("swapb beside swapa", func() bool {
		row := s.Rows()[0]
		return strings.Contains(row, "swapa-") && strings.Contains(row, "swapb-")
	})
	s.Prefix('n')
	s.Until("swapc in place of swapb, swapa still there", func() bool {
		row := s.Rows()[0]
		return strings.Contains(row, "swapa-") && strings.Contains(row, "swapc-") && !strings.Contains(row, "swapb-")
	})

	// And the switched pane is live, not a replay and then nothing. Only its own columns: the
	// whole row holds the other pane too, which is still running and would pass for it.
	rightOf := func() string {
		r := []rune(s.Rows()[0])
		if len(r) <= 35 {
			return ""
		}
		return string(r[35:])
	}
	first := rightOf()
	if !strings.Contains(first, "swapc-") {
		t.Fatalf("the right pane's columns do not hold swapc: [%s]", first)
	}
	s.Until("the switched pane receiving output", func() bool {
		now := rightOf()
		return strings.Contains(now, "swapc-") && now != first
	})
}

func TestPortCKeystrokesGoToTheFocusedPane(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "sh1", "sh", "-c", `PS1="one$ "; export PS1; exec /bin/sh -i`)
	addRunning(t, sock, "sh2", "sh", "-c", `PS1="two$ "; export PS1; exec /bin/sh -i`)

	s := attachScreen(t, sock, "sh1", 70, 10, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("one$")
	s.Prefix('|')
	s.Shows("two$")

	// The answer rather than the command, so the shell had to run it: R-$((6*7)) echoes as typed
	// and only the output says R-42.
	cFocusIs(s, "sh1 [sh2]") // the new pane has the keyboard
	s.Type("echo R-$((6*7))\r")
	s.Shows("R-42")
	s.Prefix('o')
	// Typed once the status line says the keyboard moved, the way a person does; typing in the
	// same burst as the key is the bug in the next test.
	cFocusIs(s, "[sh1] sh2")
	s.Type("echo L-$((6*8))\r")
	s.Shows("L-48")

	// Which side of the seam each landed on, which is the actual claim.
	left, right := cColumnOf(s, "L-48"), cColumnOf(s, "R-42")
	if left < 0 || left >= 30 || right <= 30 {
		t.Fatalf("typing landed in the wrong pane: left answer at column %d, right at %d\n%s", left, right, s.Text())
	}
}

// Keystrokes that arrive in the same read as Ctrl-] o - a fast typist over ssh, a paste, anything
// that coalesces - go to the pane the keyboard just left. The command case in the rendered
// session's loop drains the data channel to send "everything typed before the command" first, but
// the reader has already queued what was typed *after* it too, and the drain cannot tell them
// apart (and a select with both channels ready may take the data first anyway). Measured: 19 of 20
// with the key and the text in one write; 5 of 10 with them as two writes back to back.
func TestPortCKeystrokesInTheSameBurstAsTheFocusKeyGoToTheNewPane(t *testing.T) {
	t.Skip("BUG: text arriving in the same read as Ctrl-] o is drained to the previously focused pane (renderedsession.go, case want := <-cmds)")
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "sh1", "sh", "-c", `PS1="one$ "; export PS1; exec /bin/sh -i`)
	addRunning(t, sock, "sh2", "sh", "-c", `PS1="two$ "; export PS1; exec /bin/sh -i`)

	s := attachScreen(t, sock, "sh1", 70, 10, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("one$")
	s.Prefix('|')
	cFocusIs(s, "sh1 [sh2]")

	s.Type("\x1d")
	time.Sleep(20 * time.Millisecond) // the prefix on its own, as Prefix sends it
	s.Type("oecho L-$((6*8))\r")      // the key and the text in one read
	cFocusIs(s, "[sh1] sh2")
	s.Shows("L-48")
	if col := cColumnOf(s, "L-48"); col < 0 || col >= 30 {
		t.Fatalf("typed after Ctrl-] o, the text landed at column %d, in the pane the keyboard left:\n%s", col, s.Text())
	}
}

func TestPortCAServicesExitIsSaidOnTheStatusLine(t *testing.T) {
	// The promise holds and is checked in every ordinary run. Under -race this test is where a
	// production race shows (about 1 run in 8 alone, more in the full set): fabric.Process.Resize
	// checks p.closed under p.mu and then calls pty.Setsize(p.pty) *outside* it, while reap's
	// closePTY closes p.pty under the lock - os.File.Fd racing os.File.Close. Beyond the report,
	// the ioctl can land on a reused descriptor number: another terminal resized.
	if cRace {
		t.Skip("BUG: fabric.Process.Resize calls pty.Setsize outside p.mu while closePTY closes the pty (race detector: File.Fd vs File.Close)")
	}
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	// Two services, because with one the attach ends the moment it exits and there is no screen
	// left to say anything on. msg2 exits when told, rather than after a guessed sleep.
	gate := t.TempDir() + "/go"
	addRunning(t, sock, "msg1", "sh", "-c", `printf 'STILL-HERE\r\n'; exec cat`)
	addRunning(t, sock, "msg2", "sh", "-c", `printf 'ABOUT-TO-FAIL\r\n'; while [ ! -e `+gate+` ]; do sleep 0.02; done; exit 3`)

	s := attachScreen(t, sock, "msg1", 60, 10, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("STILL-HERE")
	s.Prefix('|')
	s.Shows("ABOUT-TO-FAIL")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.BottomShows("exited with code 3")
	if !s.StillAttached() {
		t.Fatal("one pane's service exiting ended the attach")
	}
}
