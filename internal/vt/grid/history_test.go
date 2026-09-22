package grid

import (
	"fmt"
	"strings"
	"testing"
)

// The scrollback ring had no test that made it drop anything.
//
// Every corpus case and every unit test put fewer lines through a terminal than the scrollback
// holds, so the eviction path - the whole reason the ring exists - ran only in production. Made
// the ring advance by two instead of one and the entire vt test suite still passed, which is the
// definition of an untested branch.

// linesOf reads the scrollback back as text, oldest first.
func linesOf(t *Term) []string {
	var out []string
	for _, row := range t.Scrollback() {
		var b strings.Builder
		for _, c := range row {
			b.WriteString(c.Content)
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return out
}

func TestTheScrollbackDropsTheOldestAndKeepsTheOrder(t *testing.T) {
	term := New(20, 3)
	term.SetScrollback(5)
	for i := 0; i < 20; i++ {
		fmt.Fprintf(term, "line%d\r\n", i)
	}
	// Twenty lines each ending in a newline through a three-row screen: line0 to line17 have
	// scrolled off, and the five most recent of those are what is kept. line18, line19 and the
	// empty line the cursor is now on are still screen rather than history.
	got := linesOf(term)
	want := []string{"line13", "line14", "line15", "line16", "line17"}
	if len(got) != len(want) {
		t.Fatalf("kept %d lines, not %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scrollback is %q, wanted %q", got, want)
		}
	}
}

func TestTheScrollbackStopsAtItsLimit(t *testing.T) {
	term := New(20, 3)
	term.SetScrollback(4)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(term, "line%d\r\n", i)
	}
	if n := term.MaxScroll(); n != 4 {
		t.Fatalf("a hundred lines into a scrollback of four left %d", n)
	}
}

func TestShrinkingTheScrollbackKeepsTheNewest(t *testing.T) {
	term := New(20, 3)
	term.SetScrollback(10)
	for i := 0; i < 20; i++ {
		fmt.Fprintf(term, "line%d\r\n", i)
	}
	term.SetScrollback(3)
	got := linesOf(term)
	want := []string{"line15", "line16", "line17"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after shrinking, the scrollback is %q, wanted %q", got, want)
	}
	// And it goes on working as a ring at the new size rather than staying frozen.
	fmt.Fprint(term, "after\r\n")
	if got, want := linesOf(term), []string{"line16", "line17", "line18"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("one more line after shrinking gives %q, wanted %q", got, want)
	}
}

func TestGrowingTheScrollbackKeepsWhatIsThere(t *testing.T) {
	term := New(20, 3)
	term.SetScrollback(3)
	for i := 0; i < 10; i++ {
		fmt.Fprintf(term, "line%d\r\n", i)
	}
	term.SetScrollback(20)
	if got, want := linesOf(term), []string{"line5", "line6", "line7"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("growing the scrollback changed it to %q, wanted %q", got, want)
	}
	for i := 10; i < 14; i++ {
		fmt.Fprintf(term, "line%d\r\n", i)
	}
	if n := term.MaxScroll(); n != 7 {
		t.Fatalf("after growing to twenty and adding four, the scrollback holds %d, wanted 7", n)
	}
}

// A resize rebuilds the scrollback from scratch, which empties the ring and fills it again. The
// ring reused its backing array on reset once, and reported a full scrollback after one line.
func TestAResizeAfterTheScrollbackHasWrappedKeepsItRight(t *testing.T) {
	term := New(20, 3)
	term.SetScrollback(5)
	for i := 0; i < 30; i++ {
		fmt.Fprintf(term, "line%d\r\n", i)
	}
	_ = term.Resize(20, 3)
	if n := term.MaxScroll(); n > 5 {
		t.Fatalf("after a resize the scrollback holds %d lines, more than its limit of 5", n)
	}
	got := linesOf(term)
	if len(got) == 0 {
		t.Fatal("the resize emptied the scrollback")
	}
	if last := got[len(got)-1]; last != "line27" {
		t.Fatalf("the newest scrolled-off line is %q, wanted line27; the whole scrollback is %q", last, got)
	}
}
