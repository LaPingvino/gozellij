package daemon

import (
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Ported from acceptance.sh "tabs" and "exiting one shell of several moves you on, not out".

func addRunning(t *testing.T, sock, name string, command string, args ...string) {
	t.Helper()
	if _, err := dial(t, sock).Add(name, ipc.AddRequest{Command: command, Args: args, Start: true}); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
}

func TestScreenTabsSwitchWithN(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "aaa", "sh", "-c", `printf 'IN-AAA\r\n'; exec cat`)
	addRunning(t, sock, "bbb", "sh", "-c", `printf 'IN-BBB\r\n'; exec cat`)

	s := attachScreen(t, sock, "aaa", 80, 12, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("IN-AAA")
	s.BottomShows("[aaa]")

	s.Prefix('n')
	s.Shows("IN-BBB")
	s.BottomShows("[bbb]")
	if strings.Contains(s.Text(), "IN-AAA") {
		t.Errorf("switching tabs left the previous service on screen:\n%s", s.Text())
	}

	// And typing goes to the tab shown.
	s.Type("to-bbb\r")
	s.Shows("to-bbb")

	s.Prefix('p')
	s.BottomShows("[aaa]")
}

func TestScreenExitingOneShellOfSeveralMovesYouOn(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "anchor", "sh", "-c", `printf 'ANCHOR\r\n'; exec cat`)
	addRunning(t, sock, "leaving", "sh", "-c", `printf 'LEAVING\r\n'; read x; exit 0`)

	s := attachScreen(t, sock, "anchor", 80, 12, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("ANCHOR")
	s.Prefix('n')
	s.Shows("LEAVING")
	s.Type("\r") // leaving exits under you
	s.BottomShows("now on anchor")
	s.BottomShows("[anchor]") // the tabs stay visible next to the message
	if !s.StillAttached() {
		t.Fatal("exiting one of two services ended the attach")
	}
}

func TestScreenExitingTheLastServiceGivesTheTerminalBack(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "only", "sh", "-c", `printf 'ONLY\r\n'; read x; exit 0`)

	s := attachScreen(t, sock, "only", 80, 12, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("ONLY")
	s.Type("\r")
	if err := s.Ended(); err != nil {
		t.Fatalf("the attach ended with %v", err)
	}
}
