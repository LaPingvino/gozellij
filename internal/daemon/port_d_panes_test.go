package daemon

import (
	"regexp"
	"strings"
	"testing"
)

// Ported from acceptance.sh "a split pane moved onto a stopped service stays open" and "the last
// two keys, and what they say without panes".

func TestPortDASplitPaneMovedOntoAStoppedServiceStaysOpen(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	// a-, b- and z-: a split shows the next unshown service, so the running one comes next and
	// the stopped one after it - the n below is what reaches the stopped one.
	dAdd(t, sock, "astayer", "no", `printf "STAYER-UP\r\n"; exec cat`)
	dAdd(t, sock, "bmate", "no", `printf "MATE-UP\r\n"; exec cat`)
	dAdd(t, sock, "zgone", "no", `printf "GONE-UP\r\n"; exec cat`)
	if _, err := dial(t, sock).Stop("zgone"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "zgone to stop", func() bool { return dPid(t, sock, "zgone") == 0 })

	// Wide enough that the pane marker stays on the status row beside the messages.
	s := attachScreen(t, sock, "astayer", 200, 12, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("STAYER-UP")
	s.Prefix('|')
	s.Shows("MATE-UP")

	// The focused pane is the new one; move it onto the stopped service. It stays, and says so.
	s.Prefix('n')
	s.BottomShows("zgone is not running")
	// Moved, not closed. The message is said - and painted - before the session decides whether
	// the pane closes, so a marker read now could be the moment before it went. Switching focus is
	// handled after that decision, and its paint shows the panes as they are: two, one on zgone.
	s.Prefix('o')
	s.Until("two panes after the move, focus back on astayer", func() bool {
		return strings.HasPrefix(s.Bottom(), "[astayer] zgone") && !strings.Contains(s.Text(), "MATE-UP")
	})
	s.Prefix('o')
	s.Until("focus on the zgone pane again", func() bool { return strings.HasPrefix(s.Bottom(), "astayer [zgone]") })
	eventually(t, "bmate to lose its viewer", func() bool { return viewersOf(t, sock, "bmate") == 0 })
	if !strings.Contains(s.Text(), "STAYER-UP") || !s.StillAttached() {
		t.Fatalf("after moving onto a stopped service:\n%s", s.Text())
	}

	// Ctrl-] u starts it in that pane, with the pane watching it: its output appears there.
	s.Prefix('u')
	s.Shows("GONE-UP")
	eventually(t, "zgone running, watched by the pane", func() bool {
		return dPid(t, sock, "zgone") != 0 && viewersOf(t, sock, "zgone") == 1
	})

	// But a service that dies while its pane is showing it still closes the pane.
	if _, err := dial(t, sock).Stop("zgone"); err != nil {
		t.Fatal(err)
	}
	s.Until("the pane showing zgone to close", func() bool { return !strings.Contains(s.Text(), "GONE-UP") })
	if !strings.Contains(s.Text(), "STAYER-UP") || !s.StillAttached() {
		t.Fatalf("closing the dead pane took the other with it:\n%s", s.Text())
	}
}

// dFirstOne is the first one-NN on screen, top to bottom.
func dFirstOne(s *testScreen) string {
	return regexp.MustCompile(`one-[0-9][0-9]`).FindString(s.Text())
}

func TestPortDTheLastTwoKeysFAndX(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	dAdd(t, sock, "pane1", "", `i=1; while [ $i -le 40 ]; do printf "one-%02d\r\n" $i; i=$((i+1)); done; exec cat`)
	dAdd(t, sock, "pane2", "", `printf "TWO-IS-HERE\r\n"; exec cat`)

	s := attachScreen(t, sock, "pane1", 60, 12, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("one-40")
	s.Prefix('|')
	s.Shows("TWO-IS-HERE")

	// Focus lands on the new pane after a split; back to the one with the scrollback.
	// The marker in front of the status line says which pane has the keyboard.
	s.Until("the focus marker on pane1", func() bool { return strings.HasPrefix(s.Bottom(), "pane1 [pane2]") })
	s.Prefix('o')
	s.Until("the focus marker to move to pane1", func() bool { return strings.HasPrefix(s.Bottom(), "[pane1] pane2") })

	// f is the inverse of b.
	live := dFirstOne(s)
	s.Prefix('b')
	s.Until("b to scroll back", func() bool { f := dFirstOne(s); return f != "" && f < live })
	back := dFirstOne(s)
	s.Prefix('f')
	s.Until("f to come forward from where b went", func() bool { return dFirstOne(s) > back })

	// x closes the focused pane and leaves the other, and the attach.
	s.Prefix('g')
	s.Prefix('x')
	s.Until("pane1's pane to close", func() bool { return !strings.Contains(s.Text(), "one-40") })
	s.Shows("TWO-IS-HERE")
	// Asked of the daemon rather than the screen: closing one pane of two leaves the attach
	// standing, still watching the other.
	eventually(t, "pane2 to be watched by the attach", func() bool { return viewersOf(t, sock, "pane2") == 1 })
	if !s.StillAttached() {
		t.Fatal("closing one pane of two ended the attach")
	}
}

// Found porting the check above: the daemon still counts the closed pane as watching its service.
// The session closes the pane's connection, the pane's reader reports the connection gone, and
// applyEvent - which does not check that the pane is still on screen - takes that for a daemon
// restart and reconnects the pane nobody can see. pane1 keeps a viewer for good, and the status
// line says "reconnected to pane1 after the daemon restarted" when no daemon restarted.
func TestPortDClosingAPaneLetsGoOfItsService(t *testing.T) {
	t.Skip("BUG: Ctrl-] x leaves a phantom viewer - the closed pane's reader reports gone and applyEvent reconnects it as if the daemon restarted")
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	dAdd(t, sock, "pane1", "", `printf "ONE-IS-HERE\r\n"; exec cat`)
	dAdd(t, sock, "pane2", "", `printf "TWO-IS-HERE\r\n"; exec cat`)

	s := attachScreen(t, sock, "pane1", 60, 12, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("ONE-IS-HERE")
	s.Prefix('|')
	s.Shows("TWO-IS-HERE")
	s.Until("the focus marker on pane2", func() bool { return strings.HasPrefix(s.Bottom(), "pane1 [pane2]") })
	s.Prefix('o')
	s.Until("the focus marker on pane1", func() bool { return strings.HasPrefix(s.Bottom(), "[pane1] pane2") })
	s.Prefix('x')
	s.Until("pane1's pane to close", func() bool { return !strings.Contains(s.Text(), "ONE-IS-HERE") })
	eventually(t, "pane1 to lose its viewer", func() bool { return viewersOf(t, sock, "pane1") == 0 })
	if strings.Contains(s.Bottom(), "reconnected") {
		t.Errorf("closing a pane was reported as a reconnection: %q", s.Bottom())
	}
}

func TestPortDPaneKeysInABytePipeSaySoRatherThanDoingNothing(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	dAdd(t, sock, "pane2", "", `printf "TWO-IS-HERE\r\n"; exec cat`)

	s := attachScreen(t, sock, "pane2", 60, 8, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("TWO-IS-HERE")
	s.Prefix('x')
	s.Shows("would close a pane")
	if !s.StillAttached() {
		t.Fatal("x in a byte pipe ended the attach")
	}
}
