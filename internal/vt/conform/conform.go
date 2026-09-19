// Package conform is the oracle: a way to find out whether a terminal emulator is right, before
// there is one.
//
// DESIGN.md puts this before any emulator work on purpose. The only tractable way to get a VT
// implementation right is thousands of fast iterations against something that already knows the
// answer, and eyeballing a screen is not that. So the harness comes first and the emulator is
// machine-checked from its first line.
//
// The oracle is tmux. Not a second Go emulator library: tmux is a real emulator that real programs
// have run under for twenty years, it is already a dependency of the acceptance script, and it
// costs nothing to add. A case is fed to a real tmux pane of a known size, the resulting screen is
// recorded as a golden file, and anything implementing vt.Terminal is then compared against that
// recording without needing tmux at test time.
//
// # What is compared, and what is not
//
// Content and cursor position. Not style: colours, bold and underline are not compared by anything
// here yet, so a passing case says nothing about them. That limit is written here rather than in a
// footnote because a harness that quietly checks less than it appears to is worse than no harness.
//
// # Two things the format has to decide, and does
//
//   - A wide character occupies two columns. The first carries the grapheme and Width 2; the
//     second is a continuation cell, Content "" and Width 0, and is not a character of its own.
//     Without saying this, every CJK comparison is ambiguous about which column disagreed.
//   - Trailing cells that were never written are ignored. tmux's capture-pane trims a row's
//     trailing spaces and there is no way to ask it not to, so comparing them would be comparing
//     an artefact of the oracle. Both sides are right-trimmed of spaces before the diff.
package conform

import (
	"fmt"
	"strings"

	"github.com/LaPingvino/gozellij/internal/vt"
)

// A Case is one input to drive a terminal with, at a known size.
type Case struct {
	Name string
	// Cols and Rows are the size of the screen the case expects.
	Cols, Rows int
	// Input is what is written to the terminal, already unescaped.
	Input []byte
}

// Screen is what a terminal ended up showing: one string per row, plus the cursor.
//
// Rows rather than cells because that is what both sides can honestly produce - tmux gives text
// per row, and a vt.Terminal's Snapshot is rendered down to the same thing by ScreenOf. The
// display column of a difference is recovered when diffing, which is where it is needed.
type Screen struct {
	Cols, Rows int
	Lines      []string
	CursorRow  int
	CursorCol  int

	// cells is the grid laid out by column, when the screen came from an emulator rather than
	// from a recording: cells[row][col] is what is drawn in that column, and a wide character's
	// second column is empty.
	//
	// It exists because comparing rendered text is not comparing a grid. An emulator that treats
	// a wide character as one column wide produces exactly the same row of text as one that gets
	// it right, and that sabotage passed the whole corpus until this field existed. tmux can only
	// hand over text, so the recording's columns are still derived with RuneWidth - but the
	// emulator's side is now its own geometry, which is the thing under test.
	cells [][]string
}

// ScreenOf renders a terminal's grid the way the oracle records one.
//
// Continuation cells (Width 0 directly after a wide cell) contribute nothing, because they are
// not characters - the wide grapheme before them already occupies their column. A combining mark
// also has Width 0 and *is* content, so it is kept: the difference is whether Content is empty.
func ScreenOf(t vt.Terminal) Screen {
	cols, rows := t.Size()
	cur := t.Cursor()
	s := Screen{Cols: cols, Rows: rows, CursorRow: cur.Row, CursorCol: cur.Col}
	if cur.Pending {
		// A pending wrap is recorded the way tmux reports it: the column past the last one. The
		// alternative was to throw the distinction away on the tmux side, which would have made
		// the one case that tests it untestable.
		s.CursorCol = cols
	}
	for _, row := range t.Snapshot() {
		cells := make([]string, len(row))
		var b strings.Builder
		for i, c := range row {
			cells[i] = c.Content
			if c.Content == "" {
				continue
			}
			b.WriteString(c.Content)
		}
		s.cells = append(s.cells, trimBlanks(cells))
		s.Lines = append(s.Lines, strings.TrimRight(b.String(), " "))
	}
	return s
}

// trimBlanks drops the trailing cells that were never written, matching what capture-pane does to
// the other side of every comparison.
func trimBlanks(cells []string) []string {
	for len(cells) > 0 {
		last := cells[len(cells)-1]
		if last != "" && last != " " {
			break
		}
		cells = cells[:len(cells)-1]
	}
	return cells
}

// columnsAt is one row laid out by display column, however the screen arrived: from an emulator's
// grid directly, or derived from a recording's text.
func (s Screen) columnsAt(row int) ([]string, bool) {
	if s.cells != nil {
		if row >= len(s.cells) {
			return nil, false
		}
		return s.cells[row], true
	}
	if row >= len(s.Lines) {
		return nil, false
	}
	return columns(s.Lines[row]), true
}

// A Difference is one disagreement, named precisely enough to act on without rerunning anything.
type Difference struct {
	// Row and Col are the display position. Col is -1 for a difference about the whole screen,
	// such as its size or a missing row.
	Row, Col int
	Want     string
	Got      string
	// What names the kind of difference, for a reader skimming a list of them.
	What string
}

func (d Difference) String() string {
	if d.Col < 0 {
		return fmt.Sprintf("%s: want %s, got %s", d.What, quote(d.Want), quote(d.Got))
	}
	return fmt.Sprintf("row %d col %d: %s: want %s, got %s", d.Row, d.Col, d.What, quote(d.Want), quote(d.Got))
}

func quote(s string) string {
	if s == "" {
		return "empty"
	}
	return fmt.Sprintf("%q", s)
}

// Diff reports every way got disagrees with want.
//
// Every way, not the first: a single wrong scroll moves the whole screen, and a harness that
// reports one cell of that leaves you bisecting by hand. One Difference per disagreeing column is
// what makes a failure a bug report rather than a hint.
func Diff(want, got Screen) []Difference {
	var diffs []Difference

	if want.Cols != got.Cols || want.Rows != got.Rows {
		diffs = append(diffs, Difference{Row: -1, Col: -1, What: "screen size",
			Want: fmt.Sprintf("%dx%d", want.Cols, want.Rows),
			Got:  fmt.Sprintf("%dx%d", got.Cols, got.Rows)})
	}

	n := max(want.rowCount(), got.rowCount())
	for row := 0; row < n; row++ {
		w, wok := want.columnsAt(row)
		g, gok := got.columnsAt(row)
		switch {
		case !gok:
			diffs = append(diffs, Difference{Row: row, Col: -1, What: "row is missing", Want: strings.Join(w, "")})
			continue
		case !wok:
			diffs = append(diffs, Difference{Row: row, Col: -1, What: "row was not expected", Got: strings.Join(g, "")})
			continue
		}
		diffs = append(diffs, diffColumns(row, w, g)...)
	}

	if want.CursorRow != got.CursorRow || want.CursorCol != got.CursorCol {
		diffs = append(diffs, Difference{Row: -1, Col: -1, What: "cursor",
			Want: fmt.Sprintf("%d,%d", want.CursorRow, want.CursorCol),
			Got:  fmt.Sprintf("%d,%d", got.CursorRow, got.CursorCol)})
	}
	return diffs
}

func (s Screen) rowCount() int {
	if s.cells != nil {
		return len(s.cells)
	}
	return len(s.Lines)
}

// diffColumns compares one row column by column, so that a wide character disagreeing is reported
// at the column it is drawn in rather than at its offset in a byte string.
func diffColumns(row int, w, g []string) []Difference {
	n := max(len(w), len(g))
	var diffs []Difference
	for col := 0; col < n; col++ {
		var wc, gc string
		if col < len(w) {
			wc = w[col]
		}
		if col < len(g) {
			gc = g[col]
		}
		if wc != gc {
			diffs = append(diffs, Difference{Row: row, Col: col, What: "cell", Want: wc, Got: gc})
		}
	}
	return diffs
}

// columns lays a row out into display columns, so that column i is what is drawn in column i.
//
// A wide character owns two columns and the second is empty: that is the continuation rule from
// the package doc, applied to text. A combining mark has no column of its own and is appended to
// the character it belongs to, which is what a terminal does with it.
func columns(s string) []string {
	var out []string
	for _, r := range s {
		switch w := vt.RuneWidth(r); {
		case w == 0 && len(out) > 0:
			out[len(out)-1] += string(r)
		case w == 2:
			out = append(out, string(r), "")
		default:
			out = append(out, string(r))
		}
	}
	// Trailing blanks are the oracle's own trimming, not content. See the package doc.
	for len(out) > 0 && (out[len(out)-1] == "" || out[len(out)-1] == " ") {
		out = out[:len(out)-1]
	}
	return out
}
