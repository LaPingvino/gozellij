package daemon

import (
	"fmt"
	"strings"
	"testing"
)

// The byte pipe's status line on a screen somebody has been using: full, cursor on the last row.
// That is the condition under which the terminal once appeared to hang - the cursor ended up below
// the new bottom margin, where a line feed does not scroll - and a blank screen hides it. Ported
// from acceptance.sh "what the screen actually does".

func filledScreen(rows int) []byte {
	var b strings.Builder
	for i := 1; i <= rows+16; i++ {
		fmt.Fprintf(&b, "earlier-%d\r\n", i)
	}
	return []byte(b.String())
}

func TestScreenTheStatusLineOnAScreenAlreadyInUse(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "sh1", "sh", "-c", `PS1='in$ ' exec sh -i`)

	const cols, rows = 80, 24
	s := attachScreenOver(t, sock, "sh1", cols, rows, AttachOptions{Replay: true, Mode: RenderOff}, filledScreen(rows))

	// The status line is on the last row, and that row is reserved.
	s.BottomShows("up ")
	if top, bottom := s.ScrollRegion(); top != 0 || bottom != rows-2 {
		t.Errorf("scrolling region %d-%d, want 0-%d with the last row reserved", top, bottom, rows-2)
	}

	// Attaching leaves the cursor where the shell had it, near the bottom - not homed, which
	// would print the prompt over the top of what is already there.
	if row, _ := s.Cursor(); row < rows-6 {
		t.Errorf("after attaching the cursor is on row %d of 0-%d: the screen was drawn over", row, rows-1)
	}

	// Typing works, and the output lands inside the region, not on the reserved row.
	s.Type("echo SCREEN-$((6*7))-OK\r")
	s.Until("the output inside the region", func() bool {
		r := s.Rows()
		return strings.Contains(strings.Join(r[:rows-1], "\n"), "SCREEN-42-OK")
	})

	// A full-screen program keeps its own last row: the service's bottom row is the one above
	// the status line, and what it draws there stays there.
	s.Type(fmt.Sprintf("printf '\\033[%d;1HPROGRAMS-LAST-ROW'; sleep 5\r", rows-1))
	s.Until("the program's last row intact above the status line", func() bool {
		r := s.Rows()
		return strings.Contains(r[rows-2], "PROGRAMS-LAST-ROW") && strings.Contains(r[rows-1], "up ")
	})
	s.Type("\x03")

	// Detaching gives the terminal back: the row released, the cursor not homed.
	s.Prefix('d')
	if err := s.Ended(); err != nil {
		t.Fatalf("detach: %v", err)
	}
	// Waited for, because the attach ending and its last bytes reaching the terminal are two
	// events: the release is written just before the loop returns.
	s.Until("the reserved row to be released", func() bool {
		top, bottom := s.ScrollRegion()
		return top == 0 && bottom == rows-1
	})
	if row, _ := s.Cursor(); row < rows-6 {
		t.Errorf("after detaching the cursor is on row %d: the next prompt would print over the screen", row)
	}
}
