package grid

import "github.com/LaPingvino/gozellij/internal/vt"

// Reflow re-breaks wrapped lines when the screen changes width.
//
// This is the part of a resize a user actually notices. Without it, narrowing a window leaves every
// long line chopped at the old width with a ragged gap, and widening leaves them broken where they
// no longer need to be - the text is intact but the screen is wrong, and it stays wrong until
// something redraws it. A login shell full of scrollback never redraws.
//
// It is not checked by the oracle, and that is a real gap rather than an oversight: tmux does not
// reflow at all, so it has no answer to compare against, and a conform case carries one size so a
// resize cannot be expressed as one anyway. Everything here is asserted against what a terminal is
// specified to do, which is weaker evidence than the rest of this package rests on, and is tested
// in reflow_test.go accordingly.
//
// Only the primary screen. A program on the alternate screen owns its own display and redraws it
// on SIGWINCH; re-breaking vim's status line underneath it would be inventing a screen vim never
// drew.

// logical is a paragraph: the cells of one or more physical rows that were joined by wrapping
// rather than separated by a newline.
type logical struct {
	cells []vt.Cell
	// cursor is the offset of the cursor within these cells, or -1 when it is elsewhere.
	cursor int
}

// reflowTo re-lays the primary screen and its scrollback at a new width.
func (t *Term) reflowTo(cols, rows int) {
	lines := t.logicalLines()

	// Re-wrap every paragraph at the new width, remembering where the cursor lands.
	var (
		out       [][]vt.Cell
		outUsed   []int
		outWrap   []bool
		curRow    = -1
		curColumn int
	)
	for _, l := range lines {
		rowsOf, usedOf, cursorRow, cursorCol := wrapCells(l.cells, cols, l.cursor)
		if cursorRow >= 0 {
			curRow, curColumn = len(out)+cursorRow, cursorCol
		}
		for i := range rowsOf {
			out = append(out, rowsOf[i])
			outUsed = append(outUsed, usedOf[i])
			// Every row of a paragraph continues onto the next, except the last.
			outWrap = append(outWrap, i < len(rowsOf)-1)
		}
	}

	// The cursor must end up on the screen: it is where the next character goes, and a cursor in
	// the scrollback is a character written into history. So the screen is the `rows` rows ending
	// at the cursor's row, not simply the last `rows` produced.
	if curRow < 0 {
		curRow, curColumn = len(out), 0
	}
	start := curRow - rows + 1
	if last := len(out) - rows; start < last {
		start = last
	}
	if start < 0 {
		start = 0
	}

	t.cols, t.rows = cols, rows
	t.cells = make([][]vt.Cell, rows)
	t.wrapped = make([]bool, rows)
	t.used = make([]int, rows)
	for r := 0; r < rows; r++ {
		src := start + r
		if src < len(out) {
			t.cells[r] = fitRow(out[src], cols)
			t.wrapped[r] = outWrap[src]
			t.used[r] = min(outUsed[src], cols)
			continue
		}
		t.cells[r] = blankRow(cols)
	}

	// Everything above the screen is scrollback again.
	t.history = t.history[:0]
	for i := 0; i < start && i < len(out); i++ {
		t.history = append(t.history, histLine{cells: fitRow(out[i], cols), wrapped: outWrap[i], used: min(outUsed[i], cols)})
	}
	if len(t.history) > t.limit {
		t.history = append(t.history[:0], t.history[len(t.history)-t.limit:]...)
	}

	t.cur.Row = clamp(curRow-start, 0, rows-1)
	t.cur.Col = clamp(curColumn, 0, cols-1)
	t.pend = false
	t.top, t.bottom = 0, rows-1
}

// logicalLines joins the scrollback and the screen back into paragraphs.
func (t *Term) logicalLines() []logical {
	var lines []logical
	cur := logical{cursor: -1}
	open := false

	add := func(cells []vt.Cell, used int, wrapped bool, cursorCol int) {
		if used > len(cells) {
			used = len(cells)
		}
		if cursorCol >= 0 {
			cur.cursor = len(cur.cells) + cursorCol
		}
		// Only the columns that were written. Copying the whole row would make every paragraph
		// exactly as wide as the old screen, so re-wrapping would produce a screenful of trailing
		// blanks instead of text.
		cur.cells = append(cur.cells, cells[:used]...)
		if wrapped {
			open = true
			return
		}
		lines = append(lines, cur)
		cur = logical{cursor: -1}
		open = false
	}

	for _, h := range t.history {
		add(h.cells, h.used, h.wrapped, -1)
	}
	// Only as far as the screen has content. The blank rows below the cursor are not text - they
	// are the part of the screen nothing has been written to - and carrying them through as empty
	// paragraphs makes the output longer than the screen, which pushes real lines into the
	// scrollback. Measured: narrowing a 26-character line put its first row into history.
	last := t.cur.Row
	for r := t.rows - 1; r > last; r-- {
		if t.used[r] > 0 {
			last = r
			break
		}
	}
	for r := 0; r <= last; r++ {
		col := -1
		if r == t.cur.Row {
			col = t.cur.Col
			// A row the cursor sits past the end of still has to reach that far, or the cursor
			// would be pulled back to the last written column by the join.
			if t.cur.Col > t.used[r] {
				t.used[r] = t.cur.Col
			}
		}
		add(t.cells[r], t.used[r], t.wrapped[r], col)
	}
	if open || cur.cursor >= 0 {
		lines = append(lines, cur)
	}
	return lines
}

// wrapCells breaks one paragraph into rows of at most cols columns.
//
// A wide character is never split across the margin: if it does not fit, the row ends one column
// short and it begins the next. That is what a terminal does, and half of one is not a thing that
// can be drawn.
func wrapCells(cells []vt.Cell, cols, cursor int) (rows [][]vt.Cell, used []int, cursorRow, cursorCol int) {
	cursorRow, cursorCol = -1, 0
	row := blankRow(cols)
	col := 0
	flush := func() {
		rows = append(rows, row)
		used = append(used, col)
		row = blankRow(cols)
		col = 0
	}
	for i := 0; i < len(cells); i++ {
		c := cells[i]
		w := c.Width
		if c.Content == "" && w == 0 {
			// The continuation half of a wide character. It is placed by the character itself.
			continue
		}
		if w == 0 {
			w = 1 // a combining mark rides along with the cell before it; treat defensively
		}
		if col+w > cols {
			flush()
		}
		if i == cursor {
			cursorRow, cursorCol = len(rows), col
		}
		row[col] = c
		for k := 1; k < w; k++ {
			row[col+k] = vt.Cell{Content: "", Width: 0, Style: c.Style}
		}
		col += w
	}
	if cursor >= len(cells) {
		// The cursor sits just past the last character, which is where it is after writing a line.
		if col >= cols {
			flush()
		}
		cursorRow, cursorCol = len(rows), col
	}
	flush()
	return rows, used, cursorRow, cursorCol
}

// fitRow makes a row exactly cols wide, keeping what fits.
func fitRow(row []vt.Cell, cols int) []vt.Cell {
	out := blankRow(cols)
	copy(out, row[:min(cols, len(row))])
	return out
}
