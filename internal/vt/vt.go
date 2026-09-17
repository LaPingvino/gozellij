// Package vt is the seam between gozellij and a terminal emulator.
//
// Nothing here implements an emulator yet, and that is deliberate. Phase 1 of the plan (see
// DESIGN.md) has no emulator at all: a single-pane attach proxies the child's PTY bytes straight
// to your terminal, so your terminal *is* the emulator. Only splitting the screen needs a grid.
//
// The interface exists this early for one reason: the research says whichever emulator we pick,
// we will end up running two of them side by side and diffing their output. Both candidate
// backends have serious caveats:
//
//   - charmbracelet/x/vt: pure Go, real grid model, actively developed - but in a repo labelled
//     experimental, with no tagged release, no reflow on resize, a fixed 4 MiB parser buffer per
//     emulator, and open correctness bugs including a panic on scroll-after-shrink.
//   - go.mitchellh.com/libghostty: Ghostty's VT semantics including reflow, via cgo - at the cost
//     of CGO_ENABLED=0, easy cross-compilation, and a Zig toolchain, with an API the author says
//     is not yet stable.
//
// The one Go multiplexer with real users forked the former, tripled it, and still ships releases
// on the latter, keeping both behind an interface with a differential fuzz harness between them.
// We are taking the hint and putting the interface in before the implementation, so that the
// oracle (Phase 2) can be built against it rather than retrofitted.
package vt

// Cell is one character cell of a terminal grid.
//
// Content is a grapheme cluster, not a rune: "é" may be two code points and a flag emoji may be
// several, and they occupy one cell together. Width is the display width in columns (0 for a
// combining mark that belongs to the previous cell, 1 normally, 2 for wide East Asian glyphs).
// Getting this wrong is the single most common way a multiplexer corrupts a screen.
type Cell struct {
	Content string
	Width   int
	Style   Style
}

// Style is the presentation of a cell. Colours are kept as an opaque 32-bit value plus a tag so
// that indexed, 256-colour and truecolour all round-trip without loss.
type Style struct {
	Fg, Bg        Color
	Bold          bool
	Faint         bool
	Italic        bool
	Underline     bool
	Blink         bool
	Reverse       bool
	Strikethrough bool
	// Hyperlink is the OSC 8 target, empty when the cell is not part of a link.
	Hyperlink string
}

// ColorKind distinguishes how a Color should be interpreted.
type ColorKind uint8

const (
	ColorDefault ColorKind = iota // the terminal's default fg/bg
	ColorIndexed                  // 0-255 palette
	ColorRGB
)

// Color is a colour in one of the three forms a terminal can express.
type Color struct {
	Kind ColorKind
	// Index is meaningful when Kind is ColorIndexed.
	Index uint8
	// R, G, B are meaningful when Kind is ColorRGB.
	R, G, B uint8
}

// Cursor is where the cursor is and what it looks like.
type Cursor struct {
	Row, Col int
	Visible  bool
	// Shape is the DECSCUSR shape (0 = default).
	Shape int
}

// Terminal is a terminal emulator: bytes in, a grid out.
//
// Implementations are not required to be safe for concurrent use; the caller serialises access.
// (x/vt has an open data race on its closed flag, which is a reason to be explicit about this
// rather than hopeful.)
type Terminal interface {
	// Write feeds output from the child process to the emulator. It must never fail in a way
	// that loses the rest of the stream: malformed escape sequences are the normal case, not an
	// error, and a multiplexer that dies on one is useless.
	Write(p []byte) (int, error)

	// Resize changes the grid size. Whether wrapped lines are re-wrapped is
	// implementation-defined and is precisely the behaviour the differential harness compares,
	// because it is where the candidate backends disagree most.
	Resize(cols, rows int) error

	// Size returns the current grid size.
	Size() (cols, rows int)

	// Cell returns the cell at a position. Out-of-range positions return the zero Cell and
	// false rather than panicking - see the scroll-after-shrink panic in the survey.
	Cell(row, col int) (Cell, bool)

	// Cursor returns the current cursor state.
	Cursor() Cursor

	// Snapshot returns a copy of the visible grid, row-major. Used by the renderer and, more
	// importantly, by the differential harness to compare two implementations cell by cell.
	Snapshot() [][]Cell

	// Close releases the emulator's resources.
	Close() error
}
