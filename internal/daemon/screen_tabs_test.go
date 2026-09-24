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

// Tabs are numbered in the order they were made, not by name, and Ctrl-] N goes to tab N. The
// numbers have to survive a rename: tabs name themselves now, and a number that moved when a name
// changed would make Ctrl-] 2 a guess.
func TestScreenNumberedTabsFollowCreationNotNames(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "zeta", "sh", "-c", `printf 'IN-ZETA\r\n'; exec cat`)
	addRunning(t, sock, "alpha", "sh", "-c", `printf 'IN-ALPHA\r\n'; exec cat`)

	s := attachScreen(t, sock, "alpha", 80, 12, AttachOptions{Replay: true, Mode: RenderOff})
	s.BottomShows("1:zeta 2:[alpha]")

	s.Prefix('1')
	s.Shows("IN-ZETA")
	s.BottomShows("1:[zeta] 2:alpha")

	// A rename to something that sorts last keeps its number.
	if _, err := dial(t, sock).Rename("zeta", "zzz-renamed"); err != nil {
		t.Fatal(err)
	}
	s.BottomShows("1:[zzz-renamed] 2:alpha")

	s.Prefix('2')
	s.Shows("IN-ALPHA")
	s.BottomShows("2:[alpha]")

	// A tab that is not there says so rather than doing nothing.
	s.Prefix('7')
	s.BottomShows("there is no tab 7")
	if !s.StillAttached() {
		t.Fatal("asking for a tab that does not exist ended the attach")
	}
}

// Ctrl-] { and } move the tab you are on; the order holds, and a tab made afterwards goes last.
func TestScreenMovingATab(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "one", "sh", "-c", `exec cat`)
	addRunning(t, sock, "two", "sh", "-c", `exec cat`)
	addRunning(t, sock, "three", "sh", "-c", `exec cat`)

	s := attachScreen(t, sock, "three", 100, 12, AttachOptions{Replay: true, Mode: RenderOff})
	s.BottomShows("1:one 2:two 3:[three]")

	s.Prefix('{')
	s.BottomShows("1:one 2:[three] 3:two")
	s.Prefix('{')
	s.BottomShows("1:[three] 2:one 3:two")
	s.Prefix('{')
	s.BottomShows("already at the end")

	s.Prefix('}')
	s.BottomShows("1:one 2:[three] 3:two")

	addRunning(t, sock, "four", "sh", "-c", `exec cat`)
	s.BottomShows("1:one 2:[three] 3:two 4:four")

	// The numbers are what Ctrl-] N goes by.
	s.Prefix('3')
	s.BottomShows("3:[two]")
}
