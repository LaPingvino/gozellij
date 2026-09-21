package grid

import (
	"strings"
	"testing"
)

func viewRows(t *testing.T, term *Term, offset int) []string {
	t.Helper()
	var out []string
	for _, row := range term.Scrolled(offset).Snapshot() {
		s := ""
		for _, c := range row {
			s += c.Content
		}
		out = append(out, strings.TrimRight(s, " "))
	}
	return out
}

// Scrolling back must show the lines that scrolled off, in order, ending with the top of the live
// screen - not a jumble and not the same screen again.
func TestScrolledViewShowsHistoryThenScreen(t *testing.T) {
	term := New(20, 3)
	for i := 1; i <= 8; i++ {
		term.Write([]byte(strings.Repeat("", 0) + "line-" + string(rune('0'+i)) + "\r\n"))
	}
	// Each line ends with a newline, so the cursor sits on a fresh blank row: the live screen is
	// line-7, line-8 and that blank row, and six lines are in the scrollback.
	if got := viewRows(t, term, 0); got[0] != "line-7" || got[2] != "" {
		t.Fatalf("the live screen is %q, want line-7, line-8 and a blank row", got)
	}
	if term.MaxScroll() != 6 {
		t.Fatalf("scrollback holds %d lines, want 6", term.MaxScroll())
	}

	got := viewRows(t, term, 2)
	if got[0] != "line-5" || got[1] != "line-6" || got[2] != "line-7" {
		t.Fatalf("scrolled back two lines shows %q, want line-5 line-6 line-7", got)
	}

	all := viewRows(t, term, 6)
	if all[0] != "line-1" {
		t.Fatalf("scrolled all the way back shows %q, want it to start at line-1", all)
	}
}

// Asking to scroll further back than there is history stops at the oldest line rather than
// showing blank rows above it.
func TestScrollingPastTheTopStops(t *testing.T) {
	term := New(20, 2)
	for i := 1; i <= 5; i++ {
		term.Write([]byte("row-" + string(rune('0'+i)) + "\r\n"))
	}
	deep := viewRows(t, term, 999)
	if deep[0] != "row-1" {
		t.Fatalf("scrolling far past the top shows %q, want it clamped to row-1", deep)
	}
}

// The cursor belongs to the live screen. Drawing it among old lines would put it where the next
// character is not going to appear.
func TestScrolledViewHidesTheCursor(t *testing.T) {
	term := New(20, 2)
	for i := 1; i <= 5; i++ {
		term.Write([]byte("x\r\n"))
	}
	if !term.Scrolled(0).Cursor().Visible {
		t.Fatal("the live view is hiding the cursor")
	}
	if term.Scrolled(2).Cursor().Visible {
		t.Fatal("a scrolled-back view is showing a cursor")
	}
}

// The terminal keeps running while somebody is looking back: a view is not a mode.
func TestTheTerminalKeepsRunningWhileScrolled(t *testing.T) {
	term := New(20, 2)
	for i := 1; i <= 4; i++ {
		term.Write([]byte("old-" + string(rune('0'+i)) + "\r\n"))
	}
	back := term.Scrolled(2)
	term.Write([]byte("brand-new\r\n"))

	// The scrolled view has moved with the scrollback, because a line was added to it - what must
	// not happen is the write being lost or the live screen failing to show it.
	if got := viewRows(t, term, 0); got[0] != "brand-new" {
		t.Fatalf("the live screen is %q, want the new line on it", got)
	}
	if len(back.Snapshot()) != 2 {
		t.Fatal("the scrolled view stopped producing rows")
	}
}
