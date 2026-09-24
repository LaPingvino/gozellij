package daemon

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// Ported from acceptance.sh, "the rendered attach, which owns the screen" through "the set that
// draws boxes has to draw".

func TestPortBRenderedAttachOwnsTheScreen(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "renderdemo", "sh", "-c", `printf 'RENDERED-OUTPUT-HERE\r\n'; exec sleep 60`)

	s, tap := bAttach(t, sock, "renderdemo", 60, 12, AttachOptions{Replay: true, Mode: RenderOn}, bOpts{})
	// a rendered attach shows the service's output
	s.Shows("RENDERED-OUTPUT-HERE")
	// the rendered attach draws the status line on the last row
	s.BottomShows("renderdemo")
	// the rendered attach borrows no scrolling region
	if r, ok := bBorrowedRegion(tap.Bytes(), 12); ok {
		t.Errorf("the rendered attach set a scrolling region %v on a 12-row screen", r)
	}
}

// -no-render falls back to the byte pipe even with GOZELLIJ_RENDER set. The byte pipe is told apart
// by what it does and the rendered attach does not: it reserves the last row with a region.
func TestPortBNoRenderBeatsTheEnvironment(t *testing.T) {
	screenEnv(t)
	t.Setenv("GOZELLIJ_RENDER", "1")
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "renderdemo", "sh", "-c", `printf 'RENDERED-OUTPUT-HERE\r\n'; exec sleep 60`)

	s, tap := bAttach(t, sock, "renderdemo", 60, 12, AttachOptions{Replay: true, Mode: RenderOff}, bOpts{})
	s.Shows("RENDERED-OUTPUT-HERE")
	s.Until("the byte pipe's reserved row (a scrolling region of 1-11)", func() bool {
		_, ok := bBorrowedRegion(tap.Bytes(), 12)
		return ok
	})
}

func TestPortBTwoServicesOneScreen(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "renderdemo", "sh", "-c", `printf 'RENDERED-OUTPUT-HERE\r\n'; exec sleep 60`)

	s, tap := bAttach(t, sock, "renderdemo", 60, 12, AttachOptions{Replay: true, Mode: RenderOn}, bOpts{})
	s.Shows("RENDERED-OUTPUT-HERE")
	addRunning(t, sock, "rendertwo", "sh", "-c", `printf 'SECOND-PANE-TEXT\r\n'; exec sleep 60`)

	// Ctrl-] | puts two services side by side on one screen
	s.Prefix('|')
	side := regexp.MustCompile(`RENDERED-OUTPUT-HERE.*SECOND-PANE-TEXT`)
	s.Until("both services on one row", func() bool { return side.MatchString(s.Text()) })

	// the status line marks which pane the keyboard is going to - at the start of the line, where
	// the tab bar's own brackets cannot pass for it
	s.Until("the pane marker on the new pane", func() bool {
		return strings.HasPrefix(s.Bottom(), "renderdemo [rendertwo]")
	})

	// Ctrl-] o moves the keyboard to the other pane
	s.Prefix('o')
	s.Until("the pane marker on the first pane", func() bool {
		return strings.HasPrefix(s.Bottom(), "[renderdemo] rendertwo")
	})

	// a split screen still borrows no scrolling region
	if r, ok := bBorrowedRegion(tap.Bytes(), 12); ok {
		t.Errorf("the split screen set a scrolling region %v", r)
	}
}

func TestPortBHelpAnswersOnTheStatusLine(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "helpy", "sh", "-c", `printf 'HELP-DEMO\r\n'; exec sleep 60`)

	s := attachScreen(t, sock, "helpy", 80, 8, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("HELP-DEMO")
	s.BottomShows("helpy")
	if strings.Contains(s.Bottom(), "detach") {
		t.Fatalf("the status line says detach before any help was asked for: %q", s.Bottom())
	}

	// Ctrl-] ? answers on the status line, where it can be read
	s.Prefix('?')
	s.BottomShows("detach")

	// a key that does nothing says so
	s.Prefix('z')
	s.BottomShows("does nothing")
}

func TestPortBPickerIsVisibleAndStaysUp(t *testing.T) {
	home := screenEnv(t)
	// The status line's tick is what used to draw the last frame over the menu; as fast as it goes.
	bStatusEvery(t, home, "250ms")
	_, _, sock := newTestDaemon(t)
	for _, n := range []string{"pika", "pikb", "pikc"} {
		addRunning(t, sock, n, "sh", "-c", `i=0; while :; do printf '`+n+`-%d\r\n' $i; i=$((i+1)); sleep 0.1; done`)
	}

	s := attachScreen(t, sock, "pika", 60, 10, AttachOptions{Replay: true, Mode: RenderOn})
	bStderrTo(t, s)
	s.Shows("pika-")

	// Ctrl-] l shows its menu in a rendered attach, and it stays up
	s.Prefix('l')
	menu := func() bool { t := s.Text(); return strings.Contains(t, "pick a service") && strings.Contains(t, "3 pikc") }
	s.Until("the picker's menu", menu)
	// Longer than several status ticks: the thing being tested is that time passing does not erase it.
	time.Sleep(800 * time.Millisecond)
	if !menu() {
		t.Fatalf("the menu was drawn over within a few status ticks; the screen shows:\n%s", s.Text())
	}

	// choosing from the menu switches to that service
	s.Type("3")
	s.Shows("pikc-")
	s.BottomShows("[pikc]")
}

func TestPortBUnimplementedSequenceIsSaid(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	// OSC 52, a clipboard write, which the emulator does not implement (see the note in
	// acceptance.sh: it must stay something on the list in internal/vt/grid/probe_test.go).
	addRunning(t, sock, "exotic", "sh", "-c", `printf '\033]52;c;aGVsbG8=\007clipboard\r\n'; exec sleep 60`)

	s := attachScreen(t, sock, "exotic", 70, 6, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("clipboard")
	// an unimplemented sequence is said out loud rather than silently dropped
	s.BottomShows("does not implement")
}

func TestPortBRedrawResendsWhatTheTerminalWasAssumedToHave(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "drawn", "sh", "-c",
		`printf '\033]2;TITLE-HERE\007\033]7;file://box/redrawn-here\033\\\033[?2004hCONTENT\r\n'; exec sleep 60`)

	s, tap := bAttach(t, sock, "drawn", 40, 6, AttachOptions{Replay: true, Mode: RenderOn}, bOpts{})
	s.Shows("CONTENT")
	s.Until("the title, directory and paste mode passed on once", func() bool {
		return tap.Count("TITLE-HERE") > 0 && tap.Count("redrawn-here") > 0 && tap.Count("\x1b[?2004h") > 0
	})
	title, dir, paste := tap.Count("TITLE-HERE"), tap.Count("redrawn-here"), tap.Count("\x1b[?2004h")

	// Ctrl-] r repaints and re-sends what the terminal was assumed to have
	s.Prefix('r')
	s.Until("the title and the paste mode sent again", func() bool {
		return tap.Count("TITLE-HERE") > title && tap.Count("\x1b[?2004h") > paste
	})
	// and the working directory comes back with them
	s.Until("the directory sent again", func() bool { return tap.Count("redrawn-here") > dir })
}

// bSeam is the column the right-hand pane starts at: where the second service's output begins.
func bSeam(s *testScreen, marker string) int {
	for _, r := range s.Rows() {
		if i := strings.Index(r, marker); i >= 0 {
			return i
		}
	}
	return -1
}

func TestPortBPanesThatAreNotTheSameSize(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	for _, n := range []string{"resa", "resb"} {
		addRunning(t, sock, n, "sh", "-c", `while :; do printf '`+n+`-XXXXXXXXXXXXXXXXXXXX\r\n'; sleep 0.2; done`)
	}

	s := attachScreen(t, sock, "resa", 60, 8, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("resa-")
	s.Prefix('|')
	s.Until("the second pane", func() bool { return bSeam(s, "resb-") > 0 })
	s.Until("the keyboard on the second pane", func() bool { return strings.HasPrefix(s.Bottom(), "resa [resb]") })
	even := bSeam(s, "resb-")

	// Ctrl-] > gives the focused pane more of the screen
	s.Prefix('>')
	s.Until("the seam moving left", func() bool { return bSeam(s, "resb-") >= 0 && bSeam(s, "resb-") < even })
	grown := bSeam(s, "resb-")

	// Ctrl-] < gives it less
	s.Prefix('<')
	s.Prefix('<')
	s.Until("the seam moving back right", func() bool { return bSeam(s, "resb-") > grown })

	// a pane cannot be grown until its neighbour is a stripe. Thirty presses: unbounded, that
	// leaves the neighbour about a seventh of the screen.
	for range 30 {
		s.Prefix('>')
	}
	// A command after them all, whose answer is visible: once focus has moved, every grow before
	// it has been applied and painted.
	s.Prefix('o')
	s.Until("the keyboard on the first pane", func() bool { return strings.HasPrefix(s.Bottom(), "[resa] resb") })
	s.Until("the second pane still drawn", func() bool { return bSeam(s, "resb-") >= 0 })
	if seam := bSeam(s, "resb-"); seam < 10 {
		t.Errorf("after thirty presses the neighbour is %d columns wide; the screen shows:\n%s", seam, s.Text())
	}
}

func TestPortBLineDrawingSetDrawsLines(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "boxy", "sh", "-c", `printf '\033(0lqqqk\033(B\r\n\033(0mqqqj\033(B\r\n'; exec sleep 60`)

	s, tap := bAttach(t, sock, "boxy", 20, 6, AttachOptions{Replay: true, Mode: RenderOn}, bOpts{})
	// the line-drawing set draws lines, not letters
	s.Until("a box", func() bool {
		r := s.Rows()
		return r[0] == "┌───┐" && r[1] == "└───┘"
	})
	// Drawn by the client, not handed to this terminal to draw: the letters never arrive here.
	if strings.Contains(string(tap.Bytes()), "lqqqk") {
		t.Errorf("the client passed the letters through for the terminal to translate")
	}
}
