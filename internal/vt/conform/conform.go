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
	for _, row := range t.Snapshot() {
		var b strings.Builder
		for _, c := range row {
			if c.Content == "" {
				continue
			}
			b.WriteString(c.Content)
		}
		s.Lines = append(s.Lines, strings.TrimRight(b.String(), " "))
	}
	return s
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

	n := len(want.Lines)
	if len(got.Lines) > n {
		n = len(got.Lines)
	}
	for row := 0; row < n; row++ {
		w, wok := line(want.Lines, row)
		g, gok := line(got.Lines, row)
		switch {
		case !gok:
			diffs = append(diffs, Difference{Row: row, Col: -1, What: "row is missing", Want: w})
			continue
		case !wok:
			diffs = append(diffs, Difference{Row: row, Col: -1, What: "row was not expected", Got: g})
			continue
		}
		diffs = append(diffs, diffRow(row, w, g)...)
	}

	if want.CursorRow != got.CursorRow || want.CursorCol != got.CursorCol {
		diffs = append(diffs, Difference{Row: -1, Col: -1, What: "cursor",
			Want: fmt.Sprintf("%d,%d", want.CursorRow, want.CursorCol),
			Got:  fmt.Sprintf("%d,%d", got.CursorRow, got.CursorCol)})
	}
	return diffs
}

func line(lines []string, i int) (string, bool) {
	if i >= len(lines) {
		return "", false
	}
	return lines[i], true
}

// diffRow compares one row by display column, so that a wide character disagreeing is reported at
// the column it is drawn in rather than at its offset in a byte string.
func diffRow(row int, want, got string) []Difference {
	w := columns(want)
	g := columns(got)
	n := len(w)
	if len(g) > n {
		n = len(g)
	}
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
