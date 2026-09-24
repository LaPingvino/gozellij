package daemon

import (
	"strings"
	"testing"
)

// Ported from acceptance.sh "the prefix keys nobody had pressed": p, comma, k and u.
//
// The script walks to a service by pressing n until the status line names it. A terminal wide
// enough for the tabs to stay beside any message makes that unnecessary: the bar says where you
// are after every key, so each step waits for exactly the tab it expects.

func TestPortDThePrefixKeysNobodyHadPressed(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	// Listed by name: doomed, first, second.
	dAdd(t, sock, "first", "no", `printf "FIRST-IS-UP\r\n"; exec cat`)
	dAdd(t, sock, "second", "no", `printf "SECOND-IS-UP\r\n"; exec cat`)
	dAdd(t, sock, "doomed", "no", `printf "DOOMED-IS-UP\r\n"; exec cat`)

	s := attachScreen(t, sock, "first", 220, 8, AttachOptions{Replay: true, Mode: RenderOff})
	s.BottomShows("[first]")

	// p goes back the way n came.
	s.Prefix('n')
	s.BottomShows("[second]")
	s.Prefix('p')
	s.BottomShows("[first]")

	// comma offers the name you already have, to edit; Ctrl-U then a new name renames it.
	s.Prefix(',')
	s.BottomShows("rename first to: first_")
	s.Type("\x15renamed-inside\r")
	eventually(t, "first to be called renamed-inside", func() bool {
		return dExists(t, sock, "renamed-inside") && !dExists(t, sock, "first")
	})
	s.BottomShows("renamed first to renamed-inside")

	// k removes the service you are looking at, and moves on rather than dropping you out.
	// Order is now doomed, renamed-inside, second: p reaches doomed.
	s.Prefix('p')
	s.BottomShows("[doomed]")
	s.Prefix('k')
	eventually(t, "doomed to be removed", func() bool { return !dExists(t, sock, "doomed") })
	s.Until("a tab other than doomed", func() bool {
		b := s.Bottom()
		return !strings.Contains(b, "[doomed]") && (strings.Contains(b, "[renamed-inside]") || strings.Contains(b, "[second]"))
	})
	if !s.StillAttached() {
		t.Fatal("removing one service of three ended the attach")
	}

	// n onto a stopped service keeps you in gozellij, and u starts it again.
	if _, err := dial(t, sock).Stop("second"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "second to stop", func() bool { return dPid(t, sock, "second") == 0 })
	// Two services are left, so one n at most.
	if !strings.Contains(s.Bottom(), "[second]") {
		s.Prefix('n')
		s.BottomShows("[second]")
	}
	s.BottomShows("not running")
	if !s.StillAttached() {
		t.Fatal("landing on a stopped service ended the attach")
	}
	s.Prefix('u')
	eventually(t, "second to be running again", func() bool { return dPid(t, sock, "second") != 0 })
	if !s.StillAttached() {
		t.Fatal("reviving ended the attach")
	}
}
