package grid

import (
	"strings"
	"testing"
)

func rowText(t *testing.T, term *Term, r int) string {
	t.Helper()
	var b strings.Builder
	cols, _ := term.Size()
	for c := 0; c < cols; c++ {
		cell, _ := term.Cell(r, c)
		b.WriteString(cell.Content)
	}
	return strings.TrimRight(b.String(), " ")
}

// The set that draws boxes. Without it, a program drawing a frame puts scattered letters on the
// screen where a person expects lines - which is what this emulator did until it was implemented.
func TestLineDrawingCharset(t *testing.T) {
	term := New(20, 3)
	term.Write([]byte("\x1b(0lqqqk\x1b(B done"))
	if got, want := rowText(t, term, 0), "┌───┐ done"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Going back to ordinary text has to actually go back.
func TestCharsetIsLeftBehind(t *testing.T) {
	term := New(20, 3)
	term.Write([]byte("\x1b(0q\x1b(Bq"))
	if got, want := rowText(t, term, 0), "─q"; got != want {
		t.Fatalf("got %q, want %q: the second q is ordinary text", got, want)
	}
}

// SI and SO switch between the two selected sets without reselecting either.
func TestShiftBetweenTwoSets(t *testing.T) {
	term := New(20, 3)
	// G0 stays ASCII, G1 holds the drawing set; shift out, draw, shift back in.
	term.Write([]byte("\x1b)0A\x0eq\x0fB"))
	if got, want := rowText(t, term, 0), "A─B"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Only the range the set covers is translated: it is a map of some ASCII, not a filter on
// everything.
func TestCharsetLeavesOtherCharactersAlone(t *testing.T) {
	term := New(20, 3)
	term.Write([]byte("\x1b(0A1!q"))
	if got, want := rowText(t, term, 0), "A1!─"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
