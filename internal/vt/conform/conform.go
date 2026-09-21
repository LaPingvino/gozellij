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
// Content, cursor position, scrollback and style. Style as far as a screen can show it: see
// style.go for the signature, and the note in diffStyles for the one thing no oracle of this kind
// can see - a blank cell carrying only a foreground colour draws exactly like a plain space.
//
// Not compared: anything that is not on the screen. Cursor shape and visibility, the title, mouse
// modes, bracketed paste. An emulator can get all of those wrong and pass every case here, and
// that is written down rather than left to be discovered, because a harness that quietly checks
// less than it appears to is worse than no harness.
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
	"sort"
	"strings"

	"github.com/LaPingvino/gozellij/internal/vt"
)

// A Case is one input to drive a terminal with, at a known size.
type Case struct {
	Name string
	// Cols and Rows are the size the screen starts at.
	Cols, Rows int
	// Input is everything the case writes, already unescaped. It is the concatenation of the
	// Steps' bytes, kept because split-invariance replays a case as one stream.
	Input []byte
	// Steps is the case as an ordered script: write these bytes, then change to that size, then
	// write these. A case with no resize in it is a single step and reads exactly as before.
	Steps []Step
}

// A Step is one thing a case does.
//
// Resize is a separate step rather than an escape sequence because it is not one: the size of a
// terminal changes from outside, through SIGWINCH, and an emulator finds out by being told. The
// corpus could not express it at all until now, which left every resize in this project tested
// against what a terminal is specified to do rather than against what one does.
type Step struct {
	// Write is the bytes to write, when this is a write step.
	Write []byte
	// Cols and Rows are the new size, when this is a resize step. Both zero means a write.
	Cols, Rows int
}

// IsResize reports whether this step changes the screen size.
func (s Step) IsResize() bool { return s.Cols > 0 && s.Rows > 0 }

// Screen is what a terminal ended up showing: one string per row, plus the cursor.
//
// Rows rather than cells because that is what both sides can honestly produce - tmux gives text
// per row, and a vt.Terminal's Snapshot is rendered down to the same thing by ScreenOf. The
// display column of a difference is recovered when diffing, which is where it is needed.
type Screen struct {
	Cols, Rows int
	Lines      []string
	// History is what has scrolled off the top, oldest first. A multiplexer that owns the grid
	// owns this too: it is the user's scrollback, and losing it is the most visible way to be
	// worse than the terminal you replaced.
	History   []string
	CursorRow int
	CursorCol int

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
	// histCells is History the same way, when the screen came from an emulator.
	histCells [][]string
	// Styles is one signature per display column per row: see style.go. Recorded from tmux with
	// capture-pane -e and built from an emulator's cells, so the two are comparable without the
	// harness ever interpreting escape codes with the implementation under test.
	Styles [][]string
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
	if sb, ok := t.(Scrollbacker); ok {
		for _, row := range sb.Scrollback() {
			cells := make([]string, len(row))
			for i, c := range row {
				cells[i] = c.Content
			}
			s.histCells = append(s.histCells, trimBlanks(cells))
			s.History = append(s.History, "")
		}
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
		s.Styles = append(s.Styles, trimDefaults(stylesOf(row)))
	}
	return s
}

// stylesOf is one row's styles, by display column: a wide character's continuation column carries
// the same signature as the character itself, because that is what the terminal will draw there.
func stylesOf(row []vt.Cell) []string {
	out := make([]string, len(row))
	for i, c := range row {
		out[i] = styleSig(c.Style)
	}
	return out
}

// trimDefaults drops trailing columns in the terminal's default style, matching what is done to
// trailing blanks on the other side of every comparison.
func trimDefaults(sigs []string) []string {
	for len(sigs) > 0 && sigs[len(sigs)-1] == "-" {
		sigs = sigs[:len(sigs)-1]
	}
	return sigs
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

// historyAt is one scrolled-off row laid out by display column.
func (s Screen) historyAt(row int) ([]string, bool) {
	if s.histCells != nil {
		if row >= len(s.histCells) {
			return nil, false
		}
		return s.histCells[row], true
	}
	if row >= len(s.History) {
		return nil, false
	}
	return columns(s.History[row]), true
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

// Scrollbacker is an emulator that keeps what scrolled off the top.
//
// Optional rather than part of vt.Terminal: an emulator for a pane that is only ever a live view -
// the status line's, say - has no business keeping history, and requiring it would be requiring a
// misfeature. An implementation that does not keep scrollback simply is not compared on it, which
// the corpus makes visible because the recording still has the lines.
type Scrollbacker interface {
	Scrollback() [][]vt.Cell
}

// WithoutHistory is the same screen with its scrollback dropped.
//
// For comparing things that only ever describe a visible screen - a repaint, for instance. A
// repainted terminal has no history because nothing scrolled to make any, and comparing it would
// be holding the renderer to something it never claimed to carry.
func (s Screen) WithoutHistory() Screen {
	s.History, s.histCells = nil, nil
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
		diffs = append(diffs, diffStyles(row, g, want.stylesAt(row), got.stylesAt(row))...)
	}

	// Scrollback, oldest first. Compared after the screen because a disagreement here is usually
	// a consequence of one up there, and reading the cause first is the point of ordering a bug
	// report at all.
	if len(want.History) != len(got.History) && want.histCells == nil && got.histCells == nil {
		diffs = append(diffs, Difference{Row: -1, Col: -1, What: "scrollback length",
			Want: fmt.Sprintf("%d lines", len(want.History)),
			Got:  fmt.Sprintf("%d lines", len(got.History))})
	}
	hn := max(want.historyCount(), got.historyCount())
	for row := 0; row < hn; row++ {
		w, wok := want.historyAt(row)
		g, gok := got.historyAt(row)
		switch {
		case !gok:
			diffs = append(diffs, Difference{Row: -1, Col: -1, What: fmt.Sprintf("scrollback line %d is missing", row), Want: strings.Join(w, "")})
		case !wok:
			diffs = append(diffs, Difference{Row: -1, Col: -1, What: fmt.Sprintf("scrollback line %d was not expected", row), Got: strings.Join(g, "")})
		default:
			for _, d := range diffColumns(row, w, g) {
				d.What = "scrollback " + d.What
				diffs = append(diffs, d)
			}
		}
	}

	if want.CursorRow != got.CursorRow || want.CursorCol != got.CursorCol {
		diffs = append(diffs, Difference{Row: -1, Col: -1, What: "cursor",
			Want: fmt.Sprintf("%d,%d", want.CursorRow, want.CursorCol),
			Got:  fmt.Sprintf("%d,%d", got.CursorRow, got.CursorCol)})
	}
	return diffs
}

// stylesAt is one row's style signatures, or nil when this screen carries none.
func (s Screen) stylesAt(row int) []string {
	if row >= len(s.Styles) {
		return nil
	}
	return s.Styles[row]
}

// diffStyles compares one row's styles by column.
//
// A screen with no styles recorded at all is not compared, so that a case written before styles
// existed does not start failing about something it never claimed.
//
// One kind of column is skipped, and it is a limit of the oracle rather than a convenience: a cell
// that draws as a blank in the terminal's default background. tmux's capture-pane -e emits the
// escape sequences needed to *reproduce* the screen, not a dump of its cells, so a space carrying
// only a foreground colour - which looks exactly like a plain space - comes back unstyled. vim
// fills the rows past the end of a buffer with exactly those, and the emulator is right to keep
// the colour while the recording cannot show it. Where tmux does report a style on a blank, as on
// the reverse-video status line of a pager, the comparison still happens.
func diffStyles(row int, content, want, got []string) []Difference {
	if want == nil && got == nil {
		return nil
	}
	blank := func(col int) bool {
		if col >= len(content) {
			return true
		}
		return content[col] == "" || content[col] == " "
	}
	var diffs []Difference
	for col := 0; col < max(len(want), len(got)); col++ {
		w, g := "-", "-"
		if col < len(want) {
			w = want[col]
		}
		if col < len(got) {
			g = got[col]
		}
		if blank(col) {
			// On a blank cell only some of a style is visible, and the rest cannot be seen by
			// any oracle that reads a screen rather than a cell dump: a space with a foreground
			// colour draws exactly like a plain space. Underline, reverse, strikethrough and the
			// background all do show on a blank, so those are still compared.
			w, g = visibleOnBlank(w), visibleOnBlank(g)
		}
		if w != g {
			diffs = append(diffs, Difference{Row: row, Col: col, What: "style", Want: w, Got: g})
		}
	}
	return diffs
}

// visibleOnBlank reduces a style signature to the parts a blank cell actually shows.
//
// Foreground colour, bold, faint, italic and blink change nothing about a space. Underline,
// reverse and strikethrough draw marks, and a background colour fills the cell.
func visibleOnBlank(sig string) string {
	var kept []string
	for i := 0; i < len(sig); i++ {
		switch c := sig[i]; {
		case c == 'u', c == 'r', c == 's':
			kept = append(kept, string(c))
		case c == '^':
			j := i + 1
			for j < len(sig) && sig[j] != 'u' && sig[j] != 'r' && sig[j] != 's' && sig[j] != '^' {
				j++
			}
			kept = append(kept, sig[i:j])
			i = j - 1
		}
	}
	if len(kept) == 0 {
		return "-"
	}
	sort.Strings(kept)
	return strings.Join(kept, "")
}

func (s Screen) historyCount() int {
	if s.histCells != nil {
		return len(s.histCells)
	}
	return len(s.History)
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
