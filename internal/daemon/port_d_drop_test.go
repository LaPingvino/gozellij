package daemon

import (
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Ported from acceptance.sh "a connection that drops leaves nothing behind" and "you can always
// get out, however loud it is".

func TestPortDAConnectionThatDropsLeavesNothingBehind(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	dAdd(t, sock, "dropped", "no", `i=0; while :; do printf "DROP-%d\r\n" $i; i=$((i+1)); sleep 0.05; done`)
	pid := dPid(t, sock, "dropped")

	// The script kills the client with SIGKILL; what the daemon sees of that is its connection
	// closing with no goodbye, which is what this does.
	c, err := attachRaw(t, sock, "dropped", ipc.AttachRequest{Cols: 60, Rows: 8, Replay: true})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	eventually(t, "the attached client to be counted", func() bool { return viewersOf(t, sock, "dropped") == 1 })

	c.Conn().Close()
	eventually(t, "the dropped client to stop being counted", func() bool { return viewersOf(t, sock, "dropped") == 0 })

	// The service it was watching is untouched.
	if now := dPid(t, sock, "dropped"); now != pid {
		t.Fatalf("the service was pid %d and is now %d", pid, now)
	}

	// And you can attach again and see it running.
	s := attachScreen(t, sock, "dropped", 60, 8, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("DROP-")
	before := dMaxNum(s.Text(), "DROP-")
	s.Until("the service to still be talking", func() bool { return dMaxNum(s.Text(), "DROP-") > before })
}

func TestPortDYouCanAlwaysGetOutHoweverLoudItIs(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	dAdd(t, sock, "torrent", "no", `exec yes "TORRENT-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"`)
	pid := dPid(t, sock, "torrent")

	s := attachScreen(t, sock, "torrent", 60, 8, AttachOptions{Replay: true, Mode: RenderOff})
	s.Until("the flood on screen", func() bool { return strings.Count(s.Text(), "TORRENT-") >= 2 })

	s.Prefix('d')
	if err := s.Ended(); err != nil {
		t.Fatalf("detaching under a flood: %v", err)
	}

	// The daemon still answers, and the service is the same process.
	if err := dial(t, sock).Ping(); err != nil {
		t.Fatalf("the daemon stopped answering: %v", err)
	}
	if now := dPid(t, sock, "torrent"); now != pid {
		t.Fatalf("the service was pid %d and is now %d", pid, now)
	}
}
