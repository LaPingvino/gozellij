package layout

import (
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/vt"
	"github.com/LaPingvino/gozellij/internal/vt/conform"
	"github.com/LaPingvino/gozellij/internal/vt/grid"
	"github.com/LaPingvino/gozellij/internal/vt/render"
)

const corpus = "../conform/testdata"

func load(t *testing.T, name string, cols, rows int) *grid.Term {
	t.Helper()
	c, err := conform.LoadCase(corpus + "/" + name + ".in")
	if err != nil {
		t.Fatal(err)
	}
	term := grid.New(cols, rows)
	for _, st := range c.Steps {
		if st.IsResize() {
			continue // the pane's size is the layout's business, not the case's
		}
		if _, err := term.Write(st.Write); err != nil {
			t.Fatal(err)
		}
	}
	return term
}

func rowText(cells []vt.Cell, from, to int) string {
	var b strings.Builder
	for c := from; c < to && c < len(cells); c++ {
		if cells[c].Content == "" {
			continue
		}
		b.WriteString(cells[c].Content)
	}
	return strings.TrimRight(b.String(), " ")
}

// A single pane covering the whole screen must compose to exactly that pane.
func TestOnePaneIsItself(t *testing.T) {
	term := load(t, "vim", 40, 10)
	f := Compose(40, 10, []Pane{{Rect: Rect{Cols: 40, Rows: 10}, Term: term, Focused: true}})

	want := conform.ScreenOf(term).WithoutHistory()
	got := conform.ScreenOf(f).WithoutHistory()
	for _, d := range conform.Diff(want, got) {
		t.Errorf("%s", d)
	}
}

// Two panes side by side, each keeping its own text in its own columns.
func TestTwoPanesSideBySide(t *testing.T) {
	left := load(t, "scrollback", 20, 8)
	right := load(t, "erase", 19, 8)

	f := Compose(40, 8, []Pane{
		{Rect: Rect{Cols: 20, Rows: 8}, Term: left, Focused: true},
		{Rect: Rect{Col: 21, Cols: 19, Rows: 8}, Term: right},
	})

	lg, rg := left.Snapshot(), right.Snapshot()
	for r := 0; r < 8; r++ {
		if got, want := rowText(f.Cells[r], 0, 20), rowText(lg[r], 0, 20); got != want {
			t.Errorf("row %d left is %q, want %q", r, got, want)
		}
		if got, want := rowText(f.Cells[r], 21, 40), rowText(rg[r], 0, 19); got != want {
			t.Errorf("row %d right is %q, want %q", r, got, want)
		}
		// The column between them belongs to neither and must stay blank.
		if f.Cells[r][20].Content != " " {
			t.Errorf("row %d column 20 is %q, want a blank gap", r, f.Cells[r][20].Content)
		}
	}
}

// The screen's cursor is the focused pane's, moved to where that pane sits.
func TestCursorComesFromTheFocusedPane(t *testing.T) {
	a := load(t, "wrap", 20, 5)
	// A case whose cursor is not in column zero, deliberately. With one that is, removing the
	// offset entirely still passes: the clamp into the pane pulls the cursor to the pane's left
	// edge, which is the right answer for the wrong reason.
	b := load(t, "erase", 20, 5)
	if b.Cursor().Col == 0 {
		t.Fatal("this test needs a pane whose cursor is not at column zero")
	}

	f := Compose(40, 5, []Pane{
		{Rect: Rect{Cols: 20, Rows: 5}, Term: a},
		{Rect: Rect{Col: 20, Cols: 20, Rows: 5}, Term: b, Focused: true},
	})
	want := b.Cursor()
	if f.Cur.Row != want.Row || f.Cur.Col != want.Col+20 {
		t.Fatalf("the cursor is at %d,%d; want %d,%d", f.Cur.Row, f.Cur.Col, want.Row, want.Col+20)
	}
}

// Nothing focused means no cursor. A cursor parked at the top left of a screen nothing is typing
// into is a cursor in the wrong place.
func TestNoFocusMeansNoCursor(t *testing.T) {
	f := Compose(20, 5, []Pane{{Rect: Rect{Cols: 20, Rows: 5}, Term: load(t, "wrap", 20, 5)}})
	if f.Cur.Visible {
		t.Fatal("a screen with no focused pane is showing a cursor")
	}
}

// A pane smaller than its terminal is clipped, and one larger is padded, rather than either
// failing. Both happen for real in the window between a resize and the process noticing it.
func TestPaneAndTerminalMayDisagreeAboutSize(t *testing.T) {
	big := load(t, "cjkls", 60, 12)
	f := Compose(20, 4, []Pane{{Rect: Rect{Cols: 20, Rows: 4}, Term: big}})
	if len(f.Cells) != 4 || len(f.Cells[0]) != 20 {
		t.Fatalf("the frame is %dx%d, want 20x4", len(f.Cells[0]), len(f.Cells))
	}

	small := load(t, "wrap", 10, 2)
	f = Compose(30, 6, []Pane{{Rect: Rect{Cols: 30, Rows: 6}, Term: small}})
	for r := 2; r < 6; r++ {
		if got := rowText(f.Cells[r], 0, 30); got != "" {
			t.Fatalf("row %d beyond the terminal is %q, want blank", r, got)
		}
	}
}

// A wide character cut in half by a pane's edge must not be drawn: half of one is not a character,
// and its continuation cell would eat the first column of the pane beside it.
func TestWideCharacterAtAPaneEdgeIsNotSplit(t *testing.T) {
	term := grid.New(20, 1)
	term.Write([]byte("\x1b[H12345\xe6\x97\xa5"))
	// The wide character occupies columns 5 and 6; the pane ends after column 5.
	f := Compose(20, 1, []Pane{{Rect: Rect{Cols: 6, Rows: 1}, Term: term}})

	if c := f.Cells[0][5]; c.Content == "日" || c.Width == 2 {
		t.Fatalf("a wide character was drawn straddling the pane edge: %+v", c)
	}
	if c := f.Cells[0][6]; c.Content != " " {
		t.Fatalf("the column past the pane edge is %q, want it untouched", c.Content)
	}
}

// The composition rendered and played into a real tmux must show what each pane shows. This is the
// one that is not my code checking my code.
func TestComposedScreenDrawsInTmux(t *testing.T) {
	left := load(t, "scrollback", 20, 8)
	right := load(t, "erase", 19, 8)
	f := Compose(40, 8, []Pane{
		{Rect: Rect{Cols: 20, Rows: 8}, Term: left},
		{Rect: Rect{Col: 21, Cols: 19, Rows: 8}, Term: right, Focused: true},
	})

	got, err := conform.Record(conform.Case{Name: "composed", Cols: 40, Rows: 8, Input: render.Screen(f)})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range conform.Diff(conform.ScreenOf(f).WithoutHistory(), got.WithoutHistory()) {
		t.Errorf("%s", d)
	}
}
