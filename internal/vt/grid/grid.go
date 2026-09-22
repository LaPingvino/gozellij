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
// What it does not do yet, said here rather than discovered later: no grapheme clustering beyond
// combining marks, no tab stops other than every eight columns, no charset selection, no mouse
// reporting, no bracketed paste, and nothing is done with the title. The corpus says exactly which
// real-program cases that costs, which at the time of writing is none of them - meaning the gap is
// in the corpus as much as in the emulator.
//
// What it does do, each with cases behind it: the grid and the cursor, wrapping with a deferred
// last column, scrolling regions, erase and insert/delete, the alternate screen, scrollback,
// styles, and reflow on resize. Reflow is the one with no oracle behind it, because tmux does not
// reflow at all; reflow.go says so.
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
	// wrapped[r] is true when row r ran into the right margin and continues on the row below,
	// as opposed to ending because the program printed a newline. Reflow is the difference
	// between the two: one may be re-broken at a new width, the other may not.
	wrapped []bool
	// used[r] is how many columns of row r were written. A row that wrapped early because a wide
	// character would not fit has one column fewer than the screen, and joining rows without
	// knowing that inserts a space into the middle of a re-wrapped line.
	used []int

	cur   vt.Cursor
	saved vt.Cursor
	// savedStyle travels with the saved cursor. DECSC saves the graphic rendition as well as the
	// position and DECRC puts both back, which is not a detail: a program that sets a colour,
	// saves, draws elsewhere and restores expects to be drawing in the colour it saved, and a
	// terminal that restores only the position leaves the rest of its output the wrong colour.
	savedStyle vt.Style
	style      vt.Style
	pend       bool // the cursor is past the last column, waiting to wrap on the next character
	top        int  // scrolling region, inclusive, 0-based
	bottom     int

	parser parser

	// alt is the alternate screen buffer: the one full-screen programs draw on so that what was
	// on the terminal before them comes back when they exit. Kept as a whole spare grid rather
	// than as a flag, because that is what it is - switching is swapping which grid is live.
	alt [][]vt.Cell
	// altWrapped and altUsed are the hidden buffer's per-row bookkeeping. They travel with the
	// grid they describe: leaving them behind meant the primary screen came back described by the
	// alternate screen's rows, which reflowed it wrongly and panicked when the row counts differed.
	altWrapped []bool
	altUsed    []int
	altSaved   vt.Cursor
	onAlt      bool
	// altPending records that the screen changed size while a program owned it, so the primary
	// grid still has to be reflowed when that program exits.
	altPending bool

	// history is what has scrolled off the top of the primary screen, oldest first.
	history []histLine
	// limit is how many lines of it are kept. Zero would mean a multiplexer that loses your
	// scrollback the moment it takes over the terminal, which is the most visible way to be worse
	// than what it replaced.
	limit int
}

// histLine is a scrolled-off line and whether it continued onto the line below it. The flag is
// what lets a resize re-break a paragraph that scrolled off halfway.
type histLine struct {
	cells   []vt.Cell
	wrapped bool
	used    int
}

// DefaultScrollback is how many scrolled-off lines a terminal keeps.
//
// tmux's own default is 2000 and this matches it rather than inventing a number: the corpus is
// recorded against tmux, and a different limit would make every long case disagree for a reason
// that is about the setting rather than about the emulator.
const DefaultScrollback = 2000

// New makes a terminal of the given size with an empty grid.
func New(cols, rows int) *Term {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	t := &Term{cols: cols, rows: rows, top: 0, bottom: rows - 1, limit: DefaultScrollback}
	t.cur.Visible = true
	t.cells = make([][]vt.Cell, rows)
	for r := range t.cells {
		t.cells[r] = blankRow(cols)
	}
	t.wrapped = make([]bool, rows)
	t.used = make([]int, rows)
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
	t.alt, t.altWrapped, t.altUsed = t.cells, t.wrapped, t.used
	t.wrapped = make([]bool, t.rows)
	t.used = make([]int, t.rows)
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
	t.cells, t.wrapped, t.used = t.alt, t.altWrapped, t.altUsed
	t.alt, t.altWrapped, t.altUsed = nil, nil, nil
	t.onAlt = false
	if t.altPending {
		// The window changed size while the program was running. Now that its screen is gone,
		// the user's own text can be re-broken to fit.
		t.altPending = false
		t.reflowTo(t.cols, t.rows)
	}
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
	if t.onAlt {
		// A program owns this screen and will redraw it on SIGWINCH. Both buffers are brought to
		// the new size, and the primary one is reflowed when it comes back rather than now: doing
		// it now would re-break a screen the user cannot see, twice if they resize again.
		t.cells = resizeCells(t.cells, cols, rows)
		t.wrapped, t.used = resizeFlags(t.wrapped, t.used, rows)
		// The hidden primary buffer is deliberately left at its old width. Truncating it here
		// destroyed every character past the new margin before reflow could see them, so leaving
		// vim in a narrowed window returned a shell screen with the middle of its lines cut out.
		// It is re-laid whole when the program exits, which is what altPending is for.
		t.cols, t.rows = cols, rows
		t.top, t.bottom = 0, rows-1
		t.cur.Row = min(t.cur.Row, rows-1)
		t.cur.Col = min(t.cur.Col, cols-1)
		t.pend = false
		t.altPending = true
		return nil
	}
	t.reflowTo(cols, rows)
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
	if end := t.cur.Col + width; end > t.used[t.cur.Row] {
		t.used[t.cur.Row] = end
	}
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
	if t.cur.Col == 0 && !t.pend {
		// A combining mark with nothing to combine with. A terminal drops it rather than
		// inventing a cell to hang it on, which is what this did - leaving a space with an accent
		// on it where tmux has an empty screen.
		return
	}
	// The cell *before* the cursor, which is the character just written. Starting at the cursor
	// attached the mark to the blank cell the cursor is sitting on, so "e" followed by a combining
	// acute produced "e" and a decorated space beside it instead of "é" - which is what the
	// generator reported in a two-character stream.
	col := t.cur.Col - 1
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
	// This row ran into the margin rather than being ended by a newline, which is the whole
	// distinction reflow rests on. Recorded before the line feed, because that may scroll and the
	// row is then somewhere else.
	t.wrapped[t.cur.Row] = true
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
		// The row leaving the top becomes the blank row arriving at the bottom, rather than one
		// being freed and another allocated. A login shell scrolls hundreds of thousands of
		// times; that is hundreds of thousands of rows of garbage for no reason.
		//
		// It is also what makes remember's copy load-bearing rather than decorative: the row it
		// is handed is about to be blanked and put back on the screen, so keeping the slice
		// itself would leave the user's scrollback aliasing live cells. A sabotage that stores
		// the row directly now fails four cases; before this it failed none.
		recycled := t.cells[t.top]
		t.remember(recycled, t.wrapped[t.top], t.used[t.top])
		copy(t.cells[t.top:t.bottom], t.cells[t.top+1:t.bottom+1])
		copy(t.wrapped[t.top:t.bottom], t.wrapped[t.top+1:t.bottom+1])
		copy(t.used[t.top:t.bottom], t.used[t.top+1:t.bottom+1])
		blank(recycled)
		t.cells[t.bottom] = recycled
		t.wrapped[t.bottom], t.used[t.bottom] = false, 0
	}
}

// blank clears a row in place.
func blank(row []vt.Cell) {
	for i := range row {
		row[i] = vt.Cell{Content: " ", Width: 1}
	}
}

// remember puts a line into the scrollback.
//
// One exclusion, and it is the important one: the alternate screen. A program scrolling its own
// full-screen display must not pour vim's redraws into the user's history, which is why the
// alternate screen exists at all.
//
// Scrolling regions are NOT excluded, and that is the oracle's answer rather than mine. Two cases
// were written asserting that a region would not feed history - one with the region at the top of
// the screen, one with two rows still visible above it - and tmux put the lines in history both
// times. This is a place where terminals genuinely differ; gozellij matches the one it is recorded
// against and the one its users already have in their fingers. scrollregion and scrollregion-low
// are those two cases, kept because they are the evidence.
func (t *Term) remember(line []vt.Cell, wrapped bool, used int) {
	if t.onAlt || t.limit <= 0 {
		return
	}
	kept := make([]vt.Cell, len(line))
	copy(kept, line)
	t.history = append(t.history, histLine{cells: kept, wrapped: wrapped, used: used})
	if len(t.history) > t.limit {
		// Drop from the front. Copying the slice header forward would keep the whole backing
		// array alive for as long as the pane exists, which for a long-running login shell is
		// the difference between a bounded scrollback and a leak wearing its costume.
		drop := len(t.history) - t.limit
		t.history = append(t.history[:0], t.history[drop:]...)
	}
}

// Scrollback is what has scrolled off the top, oldest first.
//
// A copy: the caller is a renderer or a test, and handing out the live slices would let either of
// them edit a user's history by accident.
func (t *Term) Scrollback() [][]vt.Cell {
	out := make([][]vt.Cell, len(t.history))
	for i, line := range t.history {
		out[i] = make([]vt.Cell, len(line.cells))
		copy(out[i], line.cells)
	}
	return out
}

// SetScrollback changes how many lines are kept, dropping the oldest if that is fewer.
func (t *Term) SetScrollback(n int) {
	if n < 0 {
		n = 0
	}
	t.limit = n
	if len(t.history) > n {
		t.history = append(t.history[:0], t.history[len(t.history)-n:]...)
	}
}

func (t *Term) scrollDown(n int) {
	for i := 0; i < n; i++ {
		copy(t.cells[t.top+1:t.bottom+1], t.cells[t.top:t.bottom])
		copy(t.wrapped[t.top+1:t.bottom+1], t.wrapped[t.top:t.bottom])
		copy(t.used[t.top+1:t.bottom+1], t.used[t.top:t.bottom])
		t.cells[t.top] = blankRow(t.cols)
		t.wrapped[t.top], t.used[t.top] = false, 0
	}
}

// saveCursor and restoreCursor are DECSC and DECRC: the position, the pending wrap and the
// current graphic rendition, together.
func (t *Term) saveCursor() {
	t.saved, t.savedStyle = t.cur, t.style
}

func (t *Term) restoreCursor() {
	t.cur, t.style = t.saved, t.savedStyle
	t.cur.Row = clamp(t.cur.Row, 0, t.rows-1)
	t.cur.Col = clamp(t.cur.Col, 0, t.cols-1)
	t.pend = false
}

// moveVertically moves the cursor up or down, bounded by the scrolling region.
//
// A cursor inside the region cannot be moved out of it by CUU or CUD: it stops at the margin. This
// stopped at the edge of the screen instead, so `\e[;6r` followed by `\e[9B` landed two rows
// below where a terminal puts it. A cursor that starts outside the region is bounded by the screen,
// because the region is not its cage.
func (t *Term) moveVertically(delta int) {
	lo, hi := 0, t.rows-1
	if t.cur.Row >= t.top && t.cur.Row <= t.bottom {
		lo, hi = t.top, t.bottom
	}
	t.cur.Row = clamp(t.cur.Row+delta, lo, hi)
	t.pend = false
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
	// Erasing fills with the background colour and nothing else. Not the whole active style: vim
	// draws a tilde in bright blue and then erases to the end of the line, and carrying the
	// foreground into those blanks paints a row of invisible blue spaces that a real terminal
	// does not have. The corpus reported it at every column of the row the first time styles were
	// compared.
	// Erasing fills with the terminal's default, not with the active style and not even with its
	// background. Background-colour erase is a terminal capability that the oracle does not have
	// switched on, and a case that sets a blue background and erases to the end of the line came
	// back from tmux with a plain tail. Matching what an emulator "should" do here would mean
	// disagreeing with the terminal this is recorded against, and with the one the user has.
	fill := vt.Cell{Content: " ", Width: 1}
	for c := max(from, 0); c <= min(to, t.cols-1); c++ {
		t.cells[row][c] = fill
	}
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

// resizeFlags brings the per-row bookkeeping to a new row count, keeping what still fits.
func resizeFlags(wrapped []bool, used []int, rows int) ([]bool, []int) {
	w := make([]bool, rows)
	u := make([]int, rows)
	copy(w, wrapped[:min(rows, len(wrapped))])
	copy(u, used[:min(rows, len(used))])
	return w, u
}

// Scrolled is a read-only view of this terminal with the screen moved back into its scrollback.
//
// A view rather than a mode: the terminal keeps running, its grid keeps being written to, and what
// changes is only which rows somebody draws. A multiplexer that had to stop the world to let you
// look at what scrolled past would be a worse terminal than the one it replaced - you would miss
// the line you were waiting for while reading the line before it.
//
// offset is how many lines back the top of the view is. Zero is the live screen.
func (t *Term) Scrolled(offset int) vt.Grid {
	if offset < 0 {
		offset = 0
	}
	if offset > len(t.history) {
		offset = len(t.history)
	}
	return &view{term: t, offset: offset}
}

// MaxScroll is how far back this terminal can be scrolled.
func (t *Term) MaxScroll() int { return len(t.history) }

type view struct {
	term   *Term
	offset int
}

func (v *view) Size() (cols, rows int) { return v.term.cols, v.term.rows }

// Cursor is hidden whenever the view is scrolled back: the cursor belongs to the live screen, and
// drawing it among old lines would put it somewhere the next character will not appear.
func (v *view) Cursor() vt.Cursor {
	if v.offset == 0 {
		return v.term.Cursor()
	}
	return vt.Cursor{Visible: false}
}

func (v *view) Snapshot() [][]vt.Cell {
	if v.offset == 0 {
		return v.term.Snapshot()
	}
	out := make([][]vt.Cell, 0, v.term.rows)
	// The tail of the scrollback first, then as much of the live screen as still fits.
	start := len(v.term.history) - v.offset
	for i := start; i < len(v.term.history) && len(out) < v.term.rows; i++ {
		row := make([]vt.Cell, v.term.cols)
		copy(row, v.term.history[i].cells[:min(v.term.cols, len(v.term.history[i].cells))])
		for c := len(v.term.history[i].cells); c < v.term.cols; c++ {
			row[c] = vt.Cell{Content: " ", Width: 1}
		}
		out = append(out, row)
	}
	for r := 0; r < v.term.rows && len(out) < v.term.rows; r++ {
		row := make([]vt.Cell, v.term.cols)
		copy(row, v.term.cells[r])
		out = append(out, row)
	}
	return out
}
