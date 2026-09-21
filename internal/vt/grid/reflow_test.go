package grid

import (
	"strings"
	"testing"
)

// Reflow is the one part of this emulator the oracle cannot check: tmux does not reflow at all, so
// it has no answer to compare against, and a conform case carries a single size so a resize cannot
// be expressed as one. These assert against what a terminal is specified to do, which is weaker
// evidence than the rest of the package rests on. Said here rather than left to be assumed.

func screenRows(t *testing.T, term *Term) []string {
	t.Helper()
	_, rows := term.Size()
	out := make([]string, rows)
	for r := 0; r < rows; r++ {
		out[r] = strings.TrimRight(row(t, term, r), " ")
	}
	return out
}

func historyRows(term *Term) []string {
	var out []string
	for _, line := range term.Scrollback() {
		s := ""
		for _, c := range line {
			s += c.Content
		}
		out = append(out, strings.TrimRight(s, " "))
	}
	return out
}

// Narrowing must break a long line at the new width, not leave it chopped at the old one.
func TestNarrowingRebreaksALongLine(t *testing.T) {
	term := New(20, 4)
	term.Write([]byte("\x1b[HABCDEFGHIJKLMNOPQRSTUVWXYZ")) // 26 characters: wraps once at 20

	if got := screenRows(t, term); got[0] != "ABCDEFGHIJKLMNOPQRST" || got[1] != "UVWXYZ" {
		t.Fatalf("before the resize the screen is %q", got)
	}
	term.Resize(10, 4)
	got := screenRows(t, term)
	if got[0] != "ABCDEFGHIJ" || got[1] != "KLMNOPQRST" || got[2] != "UVWXYZ" {
		t.Fatalf("after narrowing to 10 the screen is %q, want the line re-broken every 10", got)
	}
}

// Widening must join back up what the old width had broken, rather than leaving a ragged edge.
func TestWideningRejoinsAWrappedLine(t *testing.T) {
	term := New(10, 4)
	term.Write([]byte("\x1b[HABCDEFGHIJKLMNOPQRSTUVWXYZ"))
	term.Resize(30, 4)

	got := screenRows(t, term)
	if got[0] != "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		t.Fatalf("after widening to 30 the first row is %q, want the whole line", got[0])
	}
	if got[1] != "" {
		t.Fatalf("the second row is %q, want it emptied by the rejoin", got[1])
	}
}

// A line the program ended with a newline is NOT a wrapped line, and joining the two is the bug
// that makes a widened terminal run separate lines of output together.
func TestNewlinesAreNotRejoined(t *testing.T) {
	term := New(10, 4)
	term.Write([]byte("\x1b[Hfirst\r\nsecond\r\nthird"))
	term.Resize(30, 4)

	got := screenRows(t, term)
	if got[0] != "first" || got[1] != "second" || got[2] != "third" {
		t.Fatalf("after widening the screen is %q, want three separate lines", got)
	}
}

// Scrollback is text too, and the lines that scrolled off must be re-broken with the rest - a
// terminal that reflows only what is visible leaves the user's history at the old width for ever.
func TestScrollbackIsReflowedToo(t *testing.T) {
	term := New(10, 2)
	term.Write([]byte("\x1b[H0123456789ABCDEFGHIJ\r\nlast"))
	if got := historyRows(term); len(got) == 0 {
		t.Fatal("nothing scrolled off, so this test proves nothing")
	}
	term.Resize(20, 2)

	all := append(historyRows(term), screenRows(t, term)...)
	joined := strings.Join(all, "|")
	if !strings.Contains(joined, "0123456789ABCDEFGHIJ") {
		t.Fatalf("the scrolled-off line was not rejoined at the new width: %q", joined)
	}
}

// The cursor is where the next character goes. Reflow that leaves it somewhere else writes the
// user's next keystroke into the wrong place - the most visible bug a resize can have.
func TestCursorFollowsItsCharacter(t *testing.T) {
	term := New(20, 4)
	term.Write([]byte("\x1b[HABCDEFGHIJKLMNOPQRSTUVWXYZ")) // cursor sits after the Z
	term.Resize(10, 4)

	cur := term.Cursor()
	if cur.Row != 2 || cur.Col != 6 {
		t.Fatalf("the cursor is at %d,%d; want row 2 column 6, just after the Z on the third row", cur.Row, cur.Col)
	}
	// And writing continues from there rather than from wherever the cursor was left.
	term.Write([]byte("!"))
	if got := screenRows(t, term)[2]; got != "UVWXYZ!" {
		t.Fatalf("after the resize the next character landed wrong: %q", got)
	}
}

// A wide character must not be split across the new margin.
func TestWideCharacterIsNotSplitByReflow(t *testing.T) {
	term := New(20, 4)
	term.Write([]byte("\x1b[Hab日本語cd"))
	term.Resize(5, 4)

	got := screenRows(t, term)
	// Five columns: "ab" then a wide character needs two, so "ab日" fills four and the next wide
	// one cannot fit in the remaining column.
	if got[0] != "ab日" {
		t.Fatalf("the first row is %q, want \"ab日\" with the next wide character moved down", got[0])
	}
	if got[1] != "本語c" {
		t.Fatalf("the second row is %q", got[1])
	}
}

// A program on the alternate screen owns its display and redraws on SIGWINCH; re-breaking its
// lines would invent a screen it never drew. What must survive is the *primary* screen, which is
// reflowed when the program exits.
//
// The first version of this test asserted only that the alternate screen's second row was not the
// re-broken text - and passed when the guard was removed, because reflowing the alternate screen
// destroyed that chunk rather than moving it. An assertion that cannot tell "skipped" from
// "mangled" is not a test. This follows through to the exit, which is where the difference shows.
func TestAlternateScreenIsNotReflowed(t *testing.T) {
	term := New(20, 4)
	term.Write([]byte("\x1b[Hshell-text-that-wraps-past-twenty"))
	term.Write([]byte("\x1b[?1049h\x1b[HABCDEFGHIJKLMNOPQRSTUVWXYZ"))

	term.Resize(10, 4)
	if got := screenRows(t, term)[0]; got != "ABCDEFGHIJ" {
		t.Fatalf("the alternate screen's first row is %q, want it truncated rather than re-broken", got)
	}

	term.Write([]byte("\x1b[?1049l"))
	// Back on the user's screen, which must now be re-broken at ten columns with nothing lost.
	all := strings.Join(append(historyRows(term), screenRows(t, term)...), "")
	if want := "shell-text-that-wraps-past-twenty"; !strings.Contains(all, want) {
		t.Fatalf("after the program exited the primary screen reads %q, want it to still contain %q", all, want)
	}
	for _, r := range screenRows(t, term) {
		if len([]rune(r)) > 10 {
			t.Fatalf("row %q is wider than the screen", r)
		}
	}
}
