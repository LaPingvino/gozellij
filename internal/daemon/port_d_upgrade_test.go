package daemon

import (
	"testing"
)

// Ported from acceptance.sh "a split screen survives the daemon being replaced".
//
// The script runs `gozellij upgrade`, which execs a new daemon. Here the replacement is the server
// closing - every connection with it - and another listening on the same socket over the same
// fabric, which is what an attached client sees of an upgrade: the processes never stop, the
// connections all drop, and a daemon answers again a moment later.

func TestPortDSplitScreenSurvivesTheDaemonBeingReplaced(t *testing.T) {
	screenEnv(t)
	srv, fab, sock := newTestDaemon(t)
	dAdd(t, sock, "tickone", "", `i=0; while :; do printf "one-%d\r\n" $i; i=$((i+1)); sleep 0.05; done`)
	dAdd(t, sock, "ticktwo", "", `i=0; while :; do printf "two-%d\r\n" $i; i=$((i+1)); sleep 0.05; done`)

	s := attachScreen(t, sock, "tickone", 60, 10, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("one-")
	s.Prefix('|')
	s.Shows("two-")

	srv.Close()
	dRelisten(t, fab, sock)

	// Said out loud: what was printed while the daemon was away is not on this screen.
	s.BottomShows("reconnected")

	// And both panes are live, not two pictures of the moment the daemon went.
	one, two := dMaxNum(s.Text(), "one-"), dMaxNum(s.Text(), "two-")
	s.Until("both panes to move after the reconnection", func() bool {
		txt := s.Text()
		return dMaxNum(txt, "one-") > one && dMaxNum(txt, "two-") > two
	})

	// Left here, while the second daemon is still up: cleanups run last-first, so the screen's own
	// would find this daemon gone too and wait out a reconnect.
	s.Prefix('d')
	if err := s.Ended(); err != nil {
		t.Fatalf("detaching after the replacement: %v", err)
	}
}
