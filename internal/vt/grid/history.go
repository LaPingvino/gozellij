package grid

import "github.com/LaPingvino/gozellij/internal/vt"

// The scrollback is a ring, and it is a ring for a measured reason.
//
// It was a slice with the front dropped on every line past the limit:
//
//	drop := len(t.history) - t.limit
//	t.history = append(t.history[:0], t.history[drop:]...)
//
// which moves the whole scrollback one place left for every line that scrolls off the screen. With
// the default two thousand lines kept, twenty thousand lines of output is eighteen thousand of
// those moves - about one and a half gigabytes of memmove to keep two thousand lines. The package
// doc for internal/vt/render had recorded that the rendered attach costs two to three times the
// byte pipe on a flood and that the reason was *not* the number of repaints; this was the reason.
// BenchmarkFloodEmulate in that package is where the number comes from, and it is the same
// benchmark that says whether this helped.
//
// A ring also answers the objection the old comment raised against the obvious alternative. Keeping
// `history[drop:]` - just moving the slice header - would have left the whole backing array alive
// for as long as the pane exists, which for a login shell is a bounded scrollback in name only. A
// ring's backing array *is* the bound: it never grows past the limit, and the line it overwrites is
// the one it is dropping.

// histRing holds the last `limit` scrolled-off lines, oldest first.
type histRing struct {
	// lines is the backing array, used as a ring once it has reached limit. Grown as lines
	// arrive rather than allocated at full size, so a pane with a million-line scrollback
	// configured and nothing in it costs nothing.
	lines []histLine
	// start is where the oldest line is. Meaningless until the ring is full, and zero until then.
	start int
	n     int
	limit int
}

func (h *histRing) len() int { return h.n }

// at returns a line by age, 0 being the oldest.
func (h *histRing) at(i int) histLine { return h.lines[(h.start+i)%len(h.lines)] }

// push adds a line, dropping the oldest if that is what it takes.
//
// The cells are copied, because the caller's row is the live screen and is about to be blanked.
// When the ring is full the copy goes into the slice the dropped line was using: a flood is a
// terminal-width allocation per line otherwise, twenty thousand of them for the benchmark above,
// all of them garbage the instant the next line arrives.
func (h *histRing) push(cells []vt.Cell, wrapped bool, used int) {
	if h.limit <= 0 {
		return
	}
	if len(h.lines) < h.limit {
		kept := make([]vt.Cell, len(cells))
		copy(kept, cells)
		h.lines = append(h.lines, histLine{cells: kept, wrapped: wrapped, used: used})
		h.n = len(h.lines)
		return
	}
	dst := h.lines[h.start].cells
	if cap(dst) >= len(cells) {
		dst = dst[:len(cells)]
	} else {
		dst = make([]vt.Cell, len(cells))
	}
	copy(dst, cells)
	h.lines[h.start] = histLine{cells: dst, wrapped: wrapped, used: used}
	h.start = (h.start + 1) % h.limit
	h.n = h.limit
}

// reset empties the ring. Used by a reflow, which rebuilds the whole scrollback from the logical
// lines at the new width.
//
// The backing array is truncated rather than kept: the lines about to be pushed are a different
// width, so there is nothing to reuse, and leaving stale entries behind would put push straight
// into its overwrite branch with an empty ring - which reported `limit` lines of scrollback after
// the first one was pushed.
func (h *histRing) reset() { h.lines, h.start, h.n = h.lines[:0], 0, 0 }

// setLimit changes how many lines are kept, keeping the newest of what is already there.
func (h *histRing) setLimit(n int) {
	if n < 0 {
		n = 0
	}
	if n == h.limit {
		return
	}
	keep := min(n, h.n)
	// Unrolled into a fresh slice rather than rotated in place: this happens when somebody
	// changes a setting, not on every line, and a rotation done wrong is a scrollback silently
	// out of order.
	lines := make([]histLine, 0, keep)
	for i := h.n - keep; i < h.n; i++ {
		lines = append(lines, h.at(i))
	}
	h.lines, h.start, h.n, h.limit = lines, 0, keep, n
}
