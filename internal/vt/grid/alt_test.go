package grid

import "testing"

// These are not oracle-backed, and that is the point of saying so here. A conform case carries one
// size, so a stream that resizes halfway cannot be expressed as one - the corpus is structurally
// unable to cover this, rather than merely not covering it yet. Until the case format grows a
// resize directive, this is asserted against what a terminal is specified to do rather than
// against what tmux was observed doing, which is a weaker kind of evidence and is labelled as such.

func row(t *testing.T, term *Term, r int) string {
	t.Helper()
	cols, _ := term.Size()
	out := ""
	for c := 0; c < cols; c++ {
		cell, ok := term.Cell(r, c)
		if !ok {
			t.Fatalf("cell %d,%d is out of range", r, c)
		}
		out += cell.Content
	}
	return out
}

// A window that changes size while a full-screen program is running must not leave the shell's
// screen the wrong shape when that program exits.
func TestResizeWhileOnTheAlternateScreen(t *testing.T) {
	term := New(20, 5)
	term.Write([]byte("\x1b[Hshell-line"))
	term.Write([]byte("\x1b[?1049h"))
	term.Write([]byte("\x1b[Hfullscreen"))

	if err := term.Resize(10, 4); err != nil {
		t.Fatal(err)
	}
	term.Write([]byte("\x1b[?1049l"))

	if cols, rows := term.Size(); cols != 10 || rows != 4 {
		t.Fatalf("size is %dx%d, want 10x4", cols, rows)
	}
	// Ten columns of a line that was twenty: kept, truncated, and above all not indexing off the
	// end of a row that is now shorter.
	if got, want := row(t, term, 0), "shell-line"; got != want {
		t.Fatalf("the primary screen came back as %q, want %q", got, want)
	}
	for r := 1; r < 4; r++ {
		if got := row(t, term, r); got != "          " {
			t.Fatalf("row %d of the primary screen is %q, want blank", r, got)
		}
	}
}

// Growing while on the alternate screen is the dangerous direction, and the first version of this
// file missed it: a screen that only ever shrinks reads fine from a grid that is too big. Grow, and
// the primary grid comes back with rows shorter than the screen claims to be - and the next write
// past the old width indexes off the end of the row.
func TestGrowWhileOnTheAlternateScreen(t *testing.T) {
	term := New(10, 3)
	term.Write([]byte("\x1b[Hshell"))
	term.Write([]byte("\x1b[?1049h"))

	if err := term.Resize(30, 6); err != nil {
		t.Fatal(err)
	}
	term.Write([]byte("\x1b[?1049l"))

	// Every cell of the grown screen must exist.
	for r := 0; r < 6; r++ {
		for c := 0; c < 30; c++ {
			if _, ok := term.Cell(r, c); !ok {
				t.Fatalf("cell %d,%d does not exist on a 30x6 screen", r, c)
			}
		}
	}
	// And writing across the full width must not panic or be dropped.
	term.Write([]byte("\x1b[6;1H012345678901234567890123456789"))
	if got, want := row(t, term, 5), "012345678901234567890123456789"; got != want {
		t.Fatalf("the last row is %q, want %q", got, want)
	}
}

// The cursor saved on entering must be brought inside a screen that has since shrunk, or the next
// write lands outside the grid.
func TestCursorRestoredIntoASmallerScreen(t *testing.T) {
	term := New(20, 5)
	term.Write([]byte("\x1b[5;18H"))  // near the bottom right
	term.Write([]byte("\x1b[?1049h")) // saves it
	term.Resize(10, 3)
	term.Write([]byte("\x1b[?1049l"))

	cur := term.Cursor()
	if cur.Row > 2 || cur.Col > 9 {
		t.Fatalf("the cursor came back at %d,%d, which is outside a 10x3 screen", cur.Row, cur.Col)
	}
}
