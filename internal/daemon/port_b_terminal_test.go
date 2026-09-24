package daemon

import (
	"testing"
)

// Ported from acceptance.sh, "a program that asks the terminal gets an answer" through "the window
// title has to reach the terminal".

func TestPortBProgramAskingWhereTheCursorIsGetsAnAnswer(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	// Asks only once the attach is there (a keystroke says so), from row 2 column 1, and prints
	// the answer it read back so the screen can show it.
	addRunning(t, sock, "asker", "sh", "-c", `stty -echo -icanon min 1; printf 'READY\r\n'
dd bs=1 count=1 >/dev/null 2>&1
printf '\033[2;1H\033[6n'
r=$(dd bs=1 count=6 2>/dev/null | od -An -c | tr -d ' \n')
printf '\r\nGOT:%s:END\r\n' "$r"; exec sleep 60`)

	// This terminal's own answer to a cursor report is dropped: a byte pipe would get one from
	// here, and the promise is that the rendered attach answers by itself.
	s, _ := bAttach(t, sock, "asker", 44, 6, AttachOptions{Replay: true, Mode: RenderOn}, bOpts{dropCursorReports: true})
	s.Shows("READY")
	s.Type("x")
	// a rendered attach answers a program that asks where the cursor is
	s.Shows(`GOT:033[2;1R:END`)
}

func TestPortBCursorShapeReachesTheTerminal(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "shaper", "sh", "-c", `printf '\033[5 qBAR-CURSOR\r\n'; exec sleep 60`)

	s := attachScreen(t, sock, "shaper", 40, 5, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("BAR-CURSOR")
	// a rendered attach passes the cursor shape to the terminal
	s.Until("a bar cursor (shape 5)", func() bool { return bShape(s) == 5 })

	// and resets it on the way out
	s.Prefix('d')
	if err := s.Ended(); err != nil {
		t.Fatalf("detaching ended with %v", err)
	}
	s.Until("the cursor shape put back to the default", func() bool { return bShape(s) == 0 })
}

func TestPortBWorkingDirectoryReachesTheTerminal(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "pwder", "sh", "-c", `printf '\033]7;file://box/tmp/where-i-am\033\\here\r\n'; exec sleep 60`)

	s := attachScreen(t, sock, "pwder", 40, 5, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("here")
	// a rendered attach passes the working directory to the terminal
	s.Until("the terminal told the directory", func() bool { return bDir(s) == "file://box/tmp/where-i-am" })
}

func TestPortBWindowTitleReachesTheTerminal(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "titler", "sh", "-c", `printf '\033]2;MY-WINDOW-TITLE\007running\r\n'; exec sleep 60`)

	s := attachScreen(t, sock, "titler", 40, 5, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("running")
	// a rendered attach passes the window title to the terminal
	s.Until("the terminal's title", func() bool { return bTitle(s) == "MY-WINDOW-TITLE" })
}
