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
// combining marks, and no scrollback search or selection - the grid keeps the lines, nothing reads
// them back except a renderer. The corpus says exactly which
// real-program cases that costs, which at the time of writing is none of them - meaning the gap is
// in the corpus as much as in the emulator.
//
// What it does do, each with cases behind it: the grid and the cursor, wrapping with a deferred
// last column, scrolling regions, erase and insert/delete, the alternate screen, scrollback,
// styles, reflow on resize, the line-drawing character set, tab stops a program has moved, the
// modes and title that belong to whatever terminal is showing the screen, and the answers a
// program expects when it asks the terminal where the cursor is. Reflow is the one with no oracle behind it, because tmux does not
// reflow at all; reflow.go says so.
package grid

import (
	"fmt"

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

	// modes are the terminal-level modes a program has asked for: mouse reporting, bracketed
	// paste, focus events. Nothing here acts on them - they belong to whatever terminal is finally
	// showing this screen, and are kept so that a multiplexer painting the screen can put the real
	// terminal into the same state. A byte pipe passes them through for free; owning the grid
	// means owning this too, and not doing it is how pasting into vim breaks in a mode that
	// otherwise looks right.
	modes map[int]bool
	// dir is the working directory the program reported with OSC 7, as it sent it - a file URL.
	// Kept for the same reason as the title: it belongs to the terminal showing the screen, and
	// a client that interprets the stream instead of passing it through has to carry it or the
	// terminal stops learning where the shell is. That is what a terminal uses to open its next
	// tab in the directory you were already in.
	dir string
	// title is what the program asked the window to be called, for the same reason as modes: a
	// byte pipe hands OSC 2 to the real terminal and a client that interprets it has to carry it.
	title string
	// titles is the title stack of CSI 22 t / CSI 23 t.
	titles []string
	// irm is insert/replace mode, CSI 4 h and CSI 4 l.
	irm bool
	// awm is autowrap, DECAWM, on by default. Off means the last column overwrites itself.
	awm bool
	// keypad is application keypad mode, ESC = and ESC >.
	keypad bool
	// colourAsks are OSC 10 and OSC 11 queries this grid cannot answer by itself.
	colourAsks []int
	// replies are what this terminal owes the program: answers to the questions it asked, like
	// where the cursor is. A byte pipe gets these for free because the user's real terminal
	// answers them; a client that interprets the stream is the terminal, and a program that asks
	// and is not answered waits.
	replies []byte

	// unknown counts the sequences this emulator did not implement, by name. A byte pipe hands
	// everything it does not understand to the terminal, which may well understand it; this
	// interprets the stream, so anything unimplemented is simply dropped. Counting it turns that
	// from something a user notices as "the screen looks odd" into something they can be told.
	unknown map[string]int

	// g0 and g1 are the two character sets a program can select between, and active says which is
	// in use. See charset.go: this is what turns "lqqqk" into the top of a box.
	g0, g1 charset
	active int

	// tabs marks the columns a tab jumps to. Every eighth by default, which is what every
	// terminal ships with, but a program may set and clear them.
	tabs []bool

	// hist is what has scrolled off the top of the primary screen, oldest first, and how many
	// lines of it are kept. A ring, for the reason written down in history.go. A limit of zero
	// would mean a multiplexer that loses your scrollback the moment it takes over the terminal,
	// which is the most visible way to be worse than what it replaced.
	hist histRing
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
	t := &Term{cols: cols, rows: rows, top: 0, bottom: rows - 1, awm: true}
	t.hist.setLimit(DefaultScrollback)
	t.cur.Visible = true
	t.cells = make([][]vt.Cell, rows)
	for r := range t.cells {
		t.cells[r] = blankRow(cols)
	}
	t.wrapped = make([]bool, rows)
	t.used = make([]int, rows)
	t.tabs = defaultTabs(cols)
	return t
}

// defaultTabs is a stop every eight columns, the setting every terminal starts with.
func defaultTabs(cols int) []bool {
	tabs := make([]bool, cols)
	for c := 8; c < cols; c += 8 {
		tabs[c] = true
	}
	return tabs
}

// nextTab is the column a tab moves to: the next stop, or the last column when there is none.
//
// The last column rather than staying put, measured against tmux: with every stop cleared, a tab
// from column 1 on a twenty-column screen lands on column 19.
func (t *Term) nextTab() int {
	for c := t.cur.Col + 1; c < t.cols; c++ {
		if c < len(t.tabs) && t.tabs[c] {
			return c
		}
	}
	return t.cols - 1
}

// setTab and clearTabs are HTS and TBC: a stop at the cursor, one cleared, or all of them.
func (t *Term) setTab() {
	if t.cur.Col < len(t.tabs) {
		t.tabs[t.cur.Col] = true
	}
}

func (t *Term) clearTabs(all bool) {
	if all {
		for i := range t.tabs {
			t.tabs[i] = false
		}
		return
	}
	if t.cur.Col < len(t.tabs) {
		t.tabs[t.cur.Col] = false
	}
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
		// Stops go back to every eighth column: one set for the old width means nothing at the
		// new one, and a terminal resets them.
		t.tabs = defaultTabs(cols)
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
	t.putString(string(r), width)
}

// putString writes one grapheme at the cursor and advances.
//
// A string rather than a rune because a cell can hold something that is not one: a character
// translated through the line-drawing set, or a letter with a combining mark attached later.
func (t *Term) putString(content string, width int) {
	if t.pend {
		t.wrap()
	}
	if t.cur.Col+width > t.cols {
		if !t.awm {
			// Autowrap off: the character stays on this line and overwrites the end of it. Never
			// a new line, which is the whole point of switching it off.
			t.cur.Col = max(t.cols-width, 0)
		} else {
			// A wide character that does not fit does not straddle the margin: it moves to the
			// next line whole, which is what a terminal does and what half of one is not.
			t.wrap()
		}
	}
	if t.irm {
		// Insert mode: the character pushes the rest of the row right rather than replacing what
		// is there, and whatever falls off the end is gone. The same thing ICH does, one
		// character at a time. Programs that use it are rare - everything on this machine sends
		// only the sequence that switches it *off* - but a terminal that ignores the switch and
		// then overwrites is worse than one that never claimed to have it.
		t.shiftRight(t.cur.Row, t.cur.Col, width)
	}
	t.cells[t.cur.Row][t.cur.Col] = vt.Cell{Content: content, Width: width, Style: t.style}
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
		// have scrolled the screen in between. With autowrap off there is nothing pending: the
		// next character overwrites this one where it stands.
		t.cur.Col = t.cols - 1
		t.pend = t.awm
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
	if t.onAlt {
		return
	}
	t.hist.push(line, wrapped, used)
}

// Scrollback is what has scrolled off the top, oldest first.
//
// A copy: the caller is a renderer or a test, and handing out the live slices would let either of
// them edit a user's history by accident.
func (t *Term) Scrollback() [][]vt.Cell {
	out := make([][]vt.Cell, t.hist.len())
	for i := range out {
		line := t.hist.at(i)
		out[i] = make([]vt.Cell, len(line.cells))
		copy(out[i], line.cells)
	}
	return out
}

// SetScrollback changes how many lines are kept, dropping the oldest if that is fewer.
func (t *Term) SetScrollback(n int) {
	t.hist.setLimit(n)
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
	// Visibility is not part of what DECSC saves, so restoring must not change it. Taking the
	// whole saved cursor did: with nothing ever saved, the zero value has Visible false, so a
	// lone `\e8` hid the cursor - which the corpus reported the first time cursor visibility was
	// compared at all.
	visible := t.cur.Visible
	t.cur, t.style = t.saved, t.savedStyle
	t.cur.Visible = visible
	t.cur.Row = clamp(t.cur.Row, 0, t.rows-1)
	t.cur.Col = clamp(t.cur.Col, 0, t.cols-1)
	t.pend = false
}

// moveVertically moves the cursor up or down, stopping at a margin it would cross.
//
// The margins are barriers, not a cage: a cursor moving down stops at the bottom margin if it
// started at or above it, and a cursor moving up stops at the top margin if it started at or below
// it. A cursor already past a margin is not dragged back to it - moving down from below the bottom
// margin is bounded by the screen.
//
// Measured, over six probes, because the obvious rule is wrong twice. "Bounded by the region only
// when the cursor is inside it" - which this said a commit ago - let `\e[6;7r` then `\e[9B` run
// from row 0 all the way to the bottom of the screen, where a terminal stops it at the bottom
// margin it crossed on the way.
func (t *Term) moveVertically(delta int) {
	lo, hi := 0, t.rows-1
	if delta > 0 && t.cur.Row <= t.bottom {
		hi = t.bottom
	}
	if delta < 0 && t.cur.Row >= t.top {
		lo = t.top
	}
	t.cur.Row = clamp(t.cur.Row+delta, lo, hi)
	t.pend = false
}

// effCol is where the cursor really is for operations that act on columns.
//
// One past the last column while a wrap is pending, because that is where it is: the character
// that filled the last column has been drawn and the next one belongs on the line below. Delete,
// insert and erase all then act on a range that is empty, which is what a terminal does - measured
// on all four: with a full row, `\e[P`, `\e[@`, `\e[X` and `\e[K` each leave it untouched.
//
// Using cur.Col directly made them act on the last column instead, so `\e[P` after filling a row
// deleted the character that had just been written there.
func (t *Term) effCol() int {
	if t.pend {
		return t.cols
	}
	return t.cur.Col
}

func (t *Term) moveTo(row, col int) {
	t.cur.Row = clamp(row, 0, t.rows-1)
	t.cur.Col = clamp(col, 0, t.cols-1)
	t.pend = false
}

// PassthroughModes are the private modes a grid keeps for whoever is drawing it.
//
// A list rather than everything, because most private modes are the emulator's own business -
// DECTCEM and the alternate screen are handled here and must not also be handed on. These are the
// ones that only mean something to the terminal a person is actually looking at.
var PassthroughModes = []int{
	1,                // application cursor keys: what the arrow keys send
	12,               // a blinking cursor
	1000, 1002, 1003, // mouse reporting: clicks, drags, all motion
	1005, 1006, 1015, // how those reports are encoded
	1004, // focus in and out
	2004, // bracketed paste
}

// unknownLimit caps how many distinct sequences are remembered. A stream of junk must not be able
// to grow this without bound, and a pane that has met fifty things this emulator cannot do has
// made the point.
const unknownLimit = 50

// noteUnknown records a sequence that went nowhere.
func (t *Term) noteUnknown(name string) {
	if t.unknown == nil {
		t.unknown = make(map[string]int, 4)
	}
	if _, seen := t.unknown[name]; !seen && len(t.unknown) >= unknownLimit {
		return
	}
	t.unknown[name]++
}

// shiftRight pushes a row's cells right from a column, dropping what falls off the end. What ICH
// does, and what a character written in insert mode does.
func (t *Term) shiftRight(row, col, n int) {
	if n <= 0 || col >= t.cols {
		return
	}
	line := t.cells[row]
	for c := t.cols - 1; c >= col+n; c-- {
		line[c] = line[c-n]
	}
	for c := col; c < min(col+n, t.cols); c++ {
		line[c] = vt.Cell{Content: " ", Width: 1, Style: t.style}
	}
	if t.used[row] > 0 {
		t.used[row] = min(t.used[row]+n, t.cols)
	}
}

// titleStackLimit caps the title stack. A program that pushes and never pops - or a stream of junk
// doing it on purpose - must not be able to grow this without bound. Ten is more nesting than any
// real program does; xterm's own limit is the same order.
const titleStackLimit = 10

// pushTitle and popTitle are CSI 22 t and CSI 23 t: save and restore the window title.
//
// less, vim, htop and nano all do this, and without it the title a program set on its way in is
// the title you are left with after it exits - the shell's own title never comes back.
func (t *Term) pushTitle() {
	if len(t.titles) >= titleStackLimit {
		// Drop the oldest rather than refusing: the newest is the one a pop is about to want.
		t.titles = append(t.titles[:0], t.titles[1:]...)
	}
	t.titles = append(t.titles, t.title)
}

func (t *Term) popTitle() {
	if len(t.titles) == 0 {
		// Nothing to restore. Not an error and not a reason to blank the title: a pop without a
		// push leaves the title alone, which is what the program that sent it will have meant.
		return
	}
	t.title = t.titles[len(t.titles)-1]
	t.titles = t.titles[:len(t.titles)-1]
}

// colourAskLimit caps the outstanding colour queries. A program that asks and never reads the
// answer must not be able to grow this; two is the number a real one sends and ten is room to
// spare.
const colourAskLimit = 10

// TakeColourAsks returns and clears the OSC 10 and OSC 11 queries this grid was sent.
//
// They are handed up rather than answered here because the answer is a property of the terminal
// the user is looking at, which a grid has no way to know. Whoever is drawing it does - it asked
// that terminal the same question when it started - and answers on the grid's behalf.
func (t *Term) TakeColourAsks() []int {
	if len(t.colourAsks) == 0 {
		return nil
	}
	out := t.colourAsks
	t.colourAsks = nil
	return out
}

// Keypad reports whether this pane's program asked for application keypad mode.
func (t *Term) Keypad() bool { return t.keypad }

// Unknown is what this terminal was sent and did not implement, by name and count.
func (t *Term) Unknown() map[string]int {
	out := make(map[string]int, len(t.unknown))
	for k, v := range t.unknown {
		out[k] = v
	}
	return out
}

// TakeReplies returns and clears what this terminal owes the program, to be written back to it as
// input. Empty almost always: a program asks these questions when it starts and rarely again.
func (t *Term) TakeReplies() []byte {
	if len(t.replies) == 0 {
		return nil
	}
	out := t.replies
	t.replies = nil
	return out
}

func (t *Term) reply(format string, args ...any) {
	t.replies = append(t.replies, fmt.Sprintf(format, args...)...)
}

// Dir is the working directory the program running here last reported, empty if it has not said.
// A file URL, in the form OSC 7 carries it, because that is the form it is passed on in.
func (t *Term) Dir() string { return t.dir }

// Title is what the program running here asked the window to be called, empty if it has not said.
func (t *Term) Title() string { return t.title }

// Modes returns the terminal-level modes currently asked for, so a renderer can match them.
// ScrollRegion is the scrolling region, as zero-based top and bottom rows (DECSTBM).
func (t *Term) ScrollRegion() (top, bottom int) { return t.top, t.bottom }

// AltScreen is whether the alternate screen is showing (modes 47, 1047, 1049), which Modes does
// not report because it is kept as the swapped-out main screen rather than as a flag.
func (t *Term) AltScreen() bool { return t.alt != nil }

func (t *Term) Modes() map[int]bool {
	out := make(map[int]bool, len(t.modes))
	for m, on := range t.modes {
		if on {
			out[m] = true
		}
	}
	return out
}

// setMode records a terminal-level mode.
func (t *Term) setMode(n int, on bool) {
	if t.modes == nil {
		t.modes = make(map[int]bool, 4)
	}
	t.modes[n] = on
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
	// Erasing fills with the current background and nothing else.
	//
	// This said the opposite for several commits, on a misreading: a case that set a blue
	// background and erased to the end of a line came back from tmux with a plain tail, so the
	// conclusion was that background-colour erase was off. It was not. capture-pane trims a row's
	// trailing cells, and those blue cells were exactly the trimmed ones. Erasing into the middle
	// of a row - blue background, erase, then a mark further along - shows nine blue cells and
	// settles it.
	//
	// The background only. An underline on a blank draws a line, and a terminal does not leave one
	// behind after an erase.
	fill := vt.Cell{Content: " ", Width: 1, Style: vt.Style{Bg: t.style.Bg}}
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
	if offset > t.hist.len() {
		offset = t.hist.len()
	}
	return &view{term: t, offset: offset}
}

// MaxScroll is how far back this terminal can be scrolled.
func (t *Term) MaxScroll() int { return t.hist.len() }

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
	start := v.term.hist.len() - v.offset
	for i := start; i < v.term.hist.len() && len(out) < v.term.rows; i++ {
		cells := v.term.hist.at(i).cells
		row := make([]vt.Cell, v.term.cols)
		copy(row, cells[:min(v.term.cols, len(cells))])
		for c := len(cells); c < v.term.cols; c++ {
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
