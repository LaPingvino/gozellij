package grid

import "testing"

// Tab stops, which a program may move. Measured against tmux before being written: a stop set at
// column four is where a tab from column one lands, clearing every stop sends it to the last
// column rather than leaving it where it was, and clearing one sends it to the next one along.

func tabTo(t *testing.T, input string) int {
	t.Helper()
	term := New(20, 3)
	if _, err := term.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	return term.Cursor().Col
}

func TestDefaultTabStopsAreEveryEight(t *testing.T) {
	if got := tabTo(t, "A\t"); got != 8 {
		t.Fatalf("a tab from column 1 landed on %d, want 8", got)
	}
}

func TestATabStopCanBeSet(t *testing.T) {
	// Move to column 4, set a stop there, come back, then tab.
	if got := tabTo(t, "\x1b[1;5H\x1bH\x1b[1;1HA\t"); got != 4 {
		t.Fatalf("landed on %d, want the stop at 4", got)
	}
}

func TestClearingEveryStopSendsATabToTheMargin(t *testing.T) {
	if got := tabTo(t, "\x1b[3gA\t"); got != 19 {
		t.Fatalf("landed on %d, want the last column of a twenty-column screen", got)
	}
}

func TestClearingOneStopSkipsToTheNext(t *testing.T) {
	// Clear the stop at column 8; a tab from column 1 goes on to 16.
	if got := tabTo(t, "\x1b[1;9H\x1b[0g\x1b[1;1HA\t"); got != 16 {
		t.Fatalf("landed on %d, want 16", got)
	}
}

// A stop set for one width means nothing at another, so a resize puts them back.
func TestResizeResetsTabStops(t *testing.T) {
	term := New(20, 3)
	term.Write([]byte("\x1b[3g")) // clear them all
	if err := term.Resize(40, 3); err != nil {
		t.Fatal(err)
	}
	term.Write([]byte("A\t"))
	if got := term.Cursor().Col; got != 8 {
		t.Fatalf("after a resize a tab landed on %d, want the default stop at 8", got)
	}
}
