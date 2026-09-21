// Package layout puts several terminals on one screen.
//
// This is what a multiplexer is for, and it is the last structural piece: the emulator gives a
// pane its own grid, the renderer puts a grid on a terminal, and this decides which part of the
// terminal each grid occupies. A split screen is the composition of the three.
//
// It also settles the status line's two defects rather than working around them. A line drawn by
// whoever owns the screen is not sharing the terminal's single cursor-save slot with anybody, and
// needs no scrolling region: the pane above it is simply given one row fewer. The service cannot
// draw on a row it was never told about.
package layout

import "github.com/LaPingvino/gozellij/internal/vt"

// A Rect is where a pane sits on the screen, in cells, counted from the top left.
type Rect struct {
	Col, Row   int
	Cols, Rows int
}

// A Pane is one terminal and the area it occupies.
//
// The terminal's own size and the rectangle are allowed to disagree, and the composition clips or
// pads rather than failing. They will disagree in practice for as long as it takes a resize to
// reach the process, which is one round trip through the daemon and the pty - and a screen that
// refuses to draw during that window is a screen that flickers on every resize.
type Pane struct {
	Rect
	// A Grid, not a Terminal: composing a screen reads panes, it never writes to them. Asking for
	// the whole of Terminal here would stop a composed frame being used as a pane inside another
	// one, which is what a nested layout is.
	Term vt.Grid
	// Focused marks the pane whose cursor is the screen's cursor. A screen has one cursor; the
	// panes that are not focused keep theirs and do not get to show it.
	Focused bool
}

// A Frame is a composed screen: cells to draw and where the cursor goes.
//
// Size, Snapshot and Cursor are what the renderer reads, and nothing more. A composed screen is an
// output - there is no Write, because bytes written to it would have no pane to belong to.
type Frame struct {
	Cols, Rows int
	Cells      [][]vt.Cell
	Cur        vt.Cursor
}

func (f *Frame) Size() (int, int)      { return f.Cols, f.Rows }
func (f *Frame) Snapshot() [][]vt.Cell { return f.Cells }
func (f *Frame) Cursor() vt.Cursor     { return f.Cur }

// Compose lays panes onto a screen of the given size.
//
// Later panes draw over earlier ones. There is no z-order beyond that and no overlap detection: a
// layout that overlaps its panes is a layout bug, and hiding it behind an error return would make
// the caller handle a case it should simply not produce.
func Compose(cols, rows int, panes []Pane) *Frame {
	f := &Frame{Cols: cols, Rows: rows, Cells: make([][]vt.Cell, rows)}
	for r := range f.Cells {
		f.Cells[r] = blankRow(cols)
	}
	// No pane focused means no cursor to show. Hiding it is the honest answer: a cursor parked at
	// the top left of a screen nothing is typing into is a cursor in the wrong place.
	f.Cur = vt.Cursor{Visible: false}

	for _, p := range panes {
		grid := p.Term.Snapshot()
		for r := 0; r < p.Rows; r++ {
			y := p.Row + r
			if y < 0 || y >= rows || r >= len(grid) {
				continue
			}
			for c := 0; c < p.Cols; c++ {
				x := p.Col + c
				if x < 0 || x >= cols || c >= len(grid[r]) {
					continue
				}
				f.Cells[y][x] = grid[r][c]
			}
			// A wide character straddling the right edge of a pane cannot be drawn: half of one
			// is not a character. It is blanked rather than left as a stray continuation cell,
			// which would draw as the neighbouring pane's first column being eaten.
			if edge := p.Col + p.Cols; edge > 0 && edge <= cols && f.Cells[y][edge-1].Width == 2 {
				f.Cells[y][edge-1] = vt.Cell{Content: " ", Width: 1}
			}
		}
		if p.Focused {
			cur := p.Term.Cursor()
			cur.Row += p.Row
			cur.Col += p.Col
			// Clipped into the pane, because a cursor outside the pane it belongs to is worse
			// than one at its edge: it lands in somebody else's text.
			cur.Row = clamp(cur.Row, p.Row, min(p.Row+p.Rows-1, rows-1))
			cur.Col = clamp(cur.Col, p.Col, min(p.Col+p.Cols-1, cols-1))
			f.Cur = cur
		}
	}
	return f
}

func blankRow(cols int) []vt.Cell {
	row := make([]vt.Cell, cols)
	for i := range row {
		row[i] = vt.Cell{Content: " ", Width: 1}
	}
	return row
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }
