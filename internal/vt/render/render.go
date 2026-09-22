// Package render turns a grid back into the bytes that draw it.
//
// The other half of the emulator, and the half that makes it worth having. Owning a grid is only
// useful if something can put it on a real terminal: that is what a pane is, and it is also what
// finally fixes the status line's two borrowed pieces of state. A renderer that owns the screen
// does not share the terminal's one cursor-save slot with the service, and does not need a
// scrolling region to keep a row for itself - it simply does not draw the service there.
//
// What is drawn is a whole screen, every time. Damage tracking - redrawing only what changed - is
// the obvious optimisation and is deliberately not here, and there is now a measurement instead of
// an intention.
//
// Twenty thousand lines poured into a pane used to take about 0.6-0.8s through the byte pipe and
// about 1.3-2.3s rendered - two to three times - and nothing at all that a person would notice on
// a shell or an editor. The two changes below have taken most of that out. The end-to-end figure
// is deliberately not restated: measuring it again gave 0.5s to 4.7s for the same flood in the same
// mode, on a machine busy enough that the byte pipe's own runs spread just as wide, and a headline
// number drawn from that would be a story about the load. What is quoted instead is what could be
// counted: repaints, and the time inside them.
//
// The cost *was* the number of repaints, and the measurement that said otherwise was wrong.
//
// Batching events that were ready at the same instant changed neither the time nor the bytes -
// about 8-10 KB for the whole flood, which was read as three or four whole-screen paints. Counting
// them says 379: one per frame the daemon delivered. Batching found nothing to batch because the
// client painted between every pair of events, so there was never a second one waiting; the
// experiment measured its own premise. The 8-10 KB was the bytes the *service* sent, not the bytes
// the client wrote.
//
// Counted instead of reasoned about, twenty thousand lines into one pane: 378 frames in, 379
// whole-screen repaints out, 4.7 seconds in those repaints against 1.1 seconds interpreting the
// bytes, out of 5.9 seconds altogether. A repaint costs well under a millisecond to build - see
// BenchmarkFloodPaint below - so what it costs is handing eight kilobytes of escape sequences to a
// real terminal and waiting for it to draw them, several times more than the byte pipe ever writes.
//
// Two things came out of that, and both are measured rather than argued:
//
//   - The scrollback was three quarters of the interpreting half. Dropping a line from the front
//     of a slice moved the whole scrollback one place left for every line that scrolled off;
//     BenchmarkFloodEmulate here went from 1.2-2.5s to 0.3-0.7s when it became a ring. See
//     history.go in internal/vt/grid.
//   - A repaint rate rather than a repaint per frame. internal/daemon's minRepaint bounds output
//     to twenty screens a second, which took the same flood from 379 repaints and 4.7s of
//     painting to 11-22 repaints and 0.15-0.9s.
//
// Damage tracking still is not here, and now there is a reason rather than an intention: at twenty
// repaints a second the painting is no longer the expensive half, and drawing less of each screen
// would be optimising what is left.
package render

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/LaPingvino/gozellij/internal/vt"
)

// Screen is the byte sequence that draws a terminal's visible grid, cursor included.
//
// It begins by resetting the attributes and the scrolling region and homing the cursor, because it
// is a repaint of everything: whatever the terminal was doing before, this is what it shows now.
func Screen(t vt.Grid) []byte {
	cols, rows := t.Size()
	grid := t.Snapshot()

	var b strings.Builder
	// SGR reset, full-screen scrolling region, cursor home, erase. In that order: DECSTBM homes
	// the cursor itself, so resetting the region after homing would undo the homing.
	b.WriteString("\x1b[0m\x1b[r\x1b[H\x1b[2J")

	style := vt.Style{}
	for r := 0; r < rows && r < len(grid); r++ {
		if r > 0 {
			// An explicit move rather than a newline. Measured, not assumed: replacing this with
			// \r\n changes no case, because the newline only ever comes *before* a row and so
			// never runs off the bottom. It stays a cursor move because it cannot scroll by
			// accident if that ever stops being true - not because the alternative is broken
			// today. The comment here used to claim it was.
			fmt.Fprintf(&b, "\x1b[%d;1H", r+1)
		}
		col := 0
		for c := 0; c < cols && c < len(grid[r]); c++ {
			cell := grid[r][c]
			if cell.Content == "" && cell.Width == 0 {
				// The continuation column of a wide character: drawn by the character itself.
				continue
			}
			if cell.Style != style {
				b.WriteString(sgr(style, cell.Style))
				style = cell.Style
			}
			if cell.Content == "" {
				b.WriteString(" ")
			} else {
				b.WriteString(cell.Content)
			}
			col += max(cell.Width, 1)
		}
	}
	if style != (vt.Style{}) {
		b.WriteString("\x1b[0m")
	}

	cur := t.Cursor()
	if cur.Pending {
		// A pending wrap cannot be restored by moving the cursor there: it is the state a
		// terminal is in after a character has filled the last column, and no cursor movement
		// produces it - every movement clears it. So the character is written again, which is
		// the only thing that does.
		//
		// Without this, replaying a rendered screen put the cursor one column to the left of
		// where it was and the next character appeared inside the line instead of wrapping.
		w := widthAt(grid, cur.Row, cols-1)
		if cell, ok := cellAt(grid, cur.Row, cols-w); ok {
			fmt.Fprintf(&b, "\x1b[%d;%dH", cur.Row+1, cols-w+1)
			b.WriteString(sgr(style, cell.Style))
			style = cell.Style
			if cell.Content == "" {
				b.WriteString(" ")
			} else {
				b.WriteString(cell.Content)
			}
		}
		if style != (vt.Style{}) {
			b.WriteString("\x1b[0m")
		}
		if !cur.Visible {
			b.WriteString("\x1b[?25l")
		} else {
			b.WriteString("\x1b[?25h")
		}
		return []byte(b.String())
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", cur.Row+1, cur.Col+1)
	if !cur.Visible {
		b.WriteString("\x1b[?25l")
	} else {
		b.WriteString("\x1b[?25h")
	}
	return []byte(b.String())
}

// sgr is the shortest sequence that turns one style into another.
//
// A full reset and rebuild when anything is switched off, because the codes that switch a single
// attribute off are the least well supported part of SGR and getting one wrong leaves a screen
// underlined for ever. Turning things on is done incrementally, which is the common case: a run of
// coloured text follows a run of plain text far more often than the reverse.
func sgr(from, to vt.Style) string {
	if turnsSomethingOff(from, to) {
		from = vt.Style{}
		return "\x1b[0m" + attrs(from, to)
	}
	return attrs(from, to)
}

func turnsSomethingOff(from, to vt.Style) bool {
	return (from.Bold && !to.Bold) ||
		(from.Faint && !to.Faint) ||
		(from.Italic && !to.Italic) ||
		(from.Underline && !to.Underline) ||
		(from.Blink && !to.Blink) ||
		(from.Reverse && !to.Reverse) ||
		(from.Strikethrough && !to.Strikethrough) ||
		(from.Fg != to.Fg && to.Fg.Kind == vt.ColorDefault) ||
		(from.Bg != to.Bg && to.Bg.Kind == vt.ColorDefault)
}

func attrs(from, to vt.Style) string {
	var ps []string
	add := func(on bool, code string) {
		if on {
			ps = append(ps, code)
		}
	}
	add(to.Bold && !from.Bold, "1")
	add(to.Faint && !from.Faint, "2")
	add(to.Italic && !from.Italic, "3")
	add(to.Underline && !from.Underline, "4")
	add(to.Blink && !from.Blink, "5")
	add(to.Reverse && !from.Reverse, "7")
	add(to.Strikethrough && !from.Strikethrough, "9")
	if to.Fg != from.Fg {
		ps = append(ps, color(to.Fg, false)...)
	}
	if to.Bg != from.Bg {
		ps = append(ps, color(to.Bg, true)...)
	}
	if len(ps) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(ps, ";") + "m"
}

func color(c vt.Color, bg bool) []string {
	base := 30
	if bg {
		base = 40
	}
	switch c.Kind {
	case vt.ColorIndexed:
		switch {
		case c.Index < 8:
			return []string{strconv.Itoa(base + int(c.Index))}
		case c.Index < 16:
			// The bright half of the palette has its own codes; 38;5;N would also work but is
			// longer and less widely understood by old terminals.
			return []string{strconv.Itoa(base + 60 + int(c.Index) - 8)}
		default:
			return []string{strconv.Itoa(base + 8), "5", strconv.Itoa(int(c.Index))}
		}
	case vt.ColorRGB:
		return []string{strconv.Itoa(base + 8), "2", strconv.Itoa(int(c.R)), strconv.Itoa(int(c.G)), strconv.Itoa(int(c.B))}
	default:
		return []string{strconv.Itoa(base + 9)}
	}
}

// widthAt is the width of the character occupying a column, which for the continuation half of a
// wide character is the width of the character it belongs to.
func widthAt(grid [][]vt.Cell, row, col int) int {
	if row >= len(grid) || col >= len(grid[row]) {
		return 1
	}
	for c := col; c >= 0; c-- {
		if grid[row][c].Content != "" {
			return max(grid[row][c].Width, 1)
		}
	}
	return 1
}

// cellAt reads from a snapshot, so that the renderer needs no more of a screen than its grid.
func cellAt(grid [][]vt.Cell, row, col int) (vt.Cell, bool) {
	if row < 0 || row >= len(grid) || col < 0 || col >= len(grid[row]) {
		return vt.Cell{}, false
	}
	return grid[row][col], true
}
