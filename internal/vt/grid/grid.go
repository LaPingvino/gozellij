// Package grid is a terminal emulator: bytes in, a screen out.
//
// It is written after the oracle and against it. Every claim about what it does is a case in
// internal/vt/conform that was recorded from a real tmux, so "this handles scrolling regions" is
// not a sentence in a comment, it is a file that fails when it stops being true. That ordering is
// the whole point of DESIGN.md's Phase 2 and the reason this package did not exist until now.
//
// What it is for: splitting the screen. A single-pane attach needs no emulator at all - the bytes
// go straight to the user's terminal - and the status line's two unfixable defects (one cursor-save
// slot, one scrolling region, both shared with the service) are what owning a grid fixes.
//
// What it does not do yet, said here rather than discovered later: no styles beyond storing them,
// no alternate screen buffer, no scrollback, no reflow on resize, no grapheme clustering beyond
// combining marks, no tabs stops other than every eight columns, no charset selection, no mouse.
// The corpus says exactly which real-program cases that costs.
package grid

import (
	"github.com/LaPingvino/gozellij/internal/vt"
)

// Term is a terminal emulator with a fixed-size grid and no scrollback.
//
// Not safe for concurrent use: vt.Terminal says the caller serialises access, and one owner per
// pane is how a multiplexer is built anyway.
type Term struct {
	cols, rows int
	cells      [][]vt.Cell

	cur    vt.Cursor
	saved  vt.Cursor
	style  vt.Style
	pend   bool // the cursor is past the last column, waiting to wrap on the next character
	top    int  // scrolling region, inclusive, 0-based
	bottom int

	parser parser

	// alt is the alternate screen buffer: the one full-screen programs draw on so that what was
	// on the terminal before them comes back when they exit. Kept as a whole spare grid rather
	// than as a flag, because that is what it is - switching is swapping which grid is live.
	alt      [][]vt.Cell
	altSaved vt.Cursor
	onAlt    bool
}

// New makes a terminal of the given size with an empty grid.
func New(cols, rows int) *Term {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	t := &Term{cols: cols, rows: rows, top: 0, bottom: rows - 1}
	t.cur.Visible = true
	t.cells = make([][]vt.Cell, rows)
	for r := range t.cells {
		t.cells[r] = blankRow(cols)
	}
	return t
}

func blankRow(cols int) []vt.Cell {
	row := make([]vt.Cell, cols)
	for i := range row {
		row[i] = vt.Cell{Content: " ", Width: 1}
	}
	return row
}

func (t *Term) Size() (cols, rows int) { return t.cols, t.rows }

func (t *Term) Cursor() vt.Cursor {
	cur := t.cur
	cur.Pending = t.pend
	return cur
}

func (t *Term) Cell(row, col int) (vt.Cell, bool) {
	if row < 0 || row >= t.rows || col < 0 || col >= t.cols {
		// Out of range is an answer, not a panic. The survey found a real emulator that panics
		// on scroll-after-shrink, which is a multiplexer that dies because a window got smaller.
		return vt.Cell{}, false
	}
	return t.cells[row][col], true
}

func (t *Term) Snapshot() [][]vt.Cell {
	out := make([][]vt.Cell, t.rows)
	for r := range t.cells {
		out[r] = make([]vt.Cell, t.cols)
		copy(out[r], t.cells[r])
	}
	return out
}

func (t *Term) Close() error { return nil }

// enterAlt switches to the alternate screen, saving the cursor and the primary grid.
//
// The saved cursor is separate from the DECSC slot on purpose. Mode 1049 saves the cursor as part
// of switching, and a program that also uses DECSC while on the alternate screen must not clobber
// the position its own exit depends on. One save slot shared between the two is the defect the
// status line has lived with all along, and it is not being reproduced here.
func (t *Term) enterAlt(save bool) {
	if t.onAlt {
		return
	}
	if save {
		t.altSaved = t.cur
	}
	t.alt = t.cells
	// A fresh grid, which is also the clearing that mode 1049 specifies: there is nothing to
	// erase afterwards. An explicit eraseAll() was here until a sabotage showed that removing it
	// changed no case - it could not, because these rows have never been written to.
	t.cells = make([][]vt.Cell, t.rows)
	for r := range t.cells {
		t.cells[r] = blankRow(t.cols)
	}
	t.onAlt = true
}

// leaveAlt switches back, putting the primary grid and its cursor where they were.
func (t *Term) leaveAlt(restore bool) {
	if !t.onAlt {
		return
	}
	// A plain swap. The invariant that both grids are always the current size is kept by Resize,
	// which resizes the hidden one too - and having the fix in both places meant neither could be
	// shown to matter: disabling either alone changed no test, because the other covered it. One
	// mechanism that a sabotage can reach is worth more than two that hide each other.
	t.cells = t.alt
	t.alt = nil
	t.onAlt = false
	if restore {
		t.cur = t.altSaved
		t.cur.Row = min(t.cur.Row, t.rows-1)
		t.cur.Col = min(t.cur.Col, t.cols-1)
	}
	t.pend = false
}

// resizeCells fits a grid to a size, keeping what still fits and blanking the rest.
func resizeCells(cells [][]vt.Cell, cols, rows int) [][]vt.Cell {
	out := make([][]vt.Cell, rows)
	for r := 0; r < rows; r++ {
		out[r] = blankRow(cols)
		if r < len(cells) {
			copy(out[r], cells[r][:min(cols, len(cells[r]))])
		}
	}
	return out
}

// Resize changes the grid size, keeping what still fits.
//
// No reflow: a line that was wrapped stays broken where it was. That is a real difference from
// what a user expects and it is written down rather than glossed - it is also where the two
// candidate backends disagree most, which is precisely what a differential harness is for.
func (t *Term) Resize(cols, rows int) error {
	if cols < 1 || rows < 1 {
		return nil
	}
	t.cells = resizeCells(t.cells, cols, rows)
	if t.alt != nil {
		// The buffer that is not being shown is resized too. A program that is on the alternate
		// screen when the window changes still expects its shell's screen to be the right shape
		// when it exits.
		t.alt = resizeCells(t.alt, cols, rows)
	}
	t.cols, t.rows = cols, rows
	t.top, t.bottom = 0, rows-1
	t.cur.Row = min(t.cur.Row, rows-1)
	t.cur.Col = min(t.cur.Col, cols-1)
	t.pend = false
	return nil
}

func (t *Term) Write(p []byte) (int, error) {
	t.parser.feed(t, p)
	return len(p), nil
}

// ------------------------------------------------------------------------------ drawing

// put writes one grapheme at the cursor and advances.
func (t *Term) put(r rune, width int) {
	if width == 0 {
		// A combining mark belongs to the cell before the cursor, not to a cell of its own.
		t.combine(r)
		return
	}
	if t.pend {
		t.wrap()
	}
	if t.cur.Col+width > t.cols {
		// A wide character that does not fit does not straddle the margin: it moves to the next
		// line whole, which is what a terminal does and what half of one is not.
		t.wrap()
	}
	t.cells[t.cur.Row][t.cur.Col] = vt.Cell{Content: string(r), Width: width, Style: t.style}
	for i := 1; i < width; i++ {
		// The continuation cell of a wide character: no content of its own, as the conform
		// package's format says.
		t.cells[t.cur.Row][t.cur.Col+i] = vt.Cell{Content: "", Width: 0, Style: t.style}
	}
	t.cur.Col += width
	if t.cur.Col >= t.cols {
		// Deferred wrap. The cursor stays in the last column until another character arrives,
		// because a program that writes exactly to the margin and then moves the cursor must not
		// have scrolled the screen in between.
		t.cur.Col = t.cols - 1
		t.pend = true
	}
}

func (t *Term) combine(r rune) {
	col := t.cur.Col
	if t.pend {
		col = t.cols - 1
	}
	for col > 0 && t.cells[t.cur.Row][col].Content == "" {
		col-- // step back over a wide character's continuation cell
	}
	if col >= 0 && t.cells[t.cur.Row][col].Content != "" {
		t.cells[t.cur.Row][col].Content += string(r)
	}
}

func (t *Term) wrap() {
	t.pend = false
	t.cur.Col = 0
	t.lineFeed()
}

func (t *Term) lineFeed() {
	if t.cur.Row == t.bottom {
		t.scrollUp(1)
		return
	}
	if t.cur.Row < t.rows-1 {
		t.cur.Row++
	}
}

func (t *Term) scrollUp(n int) {
	for i := 0; i < n; i++ {
		copy(t.cells[t.top:t.bottom], t.cells[t.top+1:t.bottom+1])
		t.cells[t.bottom] = blankRow(t.cols)
	}
}

func (t *Term) scrollDown(n int) {
	for i := 0; i < n; i++ {
		copy(t.cells[t.top+1:t.bottom+1], t.cells[t.top:t.bottom])
		t.cells[t.top] = blankRow(t.cols)
	}
}

func (t *Term) moveTo(row, col int) {
	t.cur.Row = clamp(row, 0, t.rows-1)
	t.cur.Col = clamp(col, 0, t.cols-1)
	t.pend = false
}

func (t *Term) eraseInRow(row, from, to int) {
	if row < 0 || row >= t.rows {
		return
	}
	for c := max(from, 0); c <= min(to, t.cols-1); c++ {
		t.cells[row][c] = vt.Cell{Content: " ", Width: 1, Style: t.style}
	}
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }
