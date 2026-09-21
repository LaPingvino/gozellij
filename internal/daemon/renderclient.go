package daemon

import (
	"io"
	"os"
	"strings"
	"sync"

	"github.com/LaPingvino/gozellij/internal/status"
	"github.com/LaPingvino/gozellij/internal/vt"
	"github.com/LaPingvino/gozellij/internal/vt/grid"
	"github.com/LaPingvino/gozellij/internal/vt/layout"
	"github.com/LaPingvino/gozellij/internal/vt/render"
)

// renderedScreen is an attach that owns the terminal instead of borrowing it.
//
// The default attach is a byte pipe: the service's output goes straight through and the user's own
// terminal does the emulating. That is DESIGN.md's Phase 1 and it is still the right default -
// it cannot corrupt a screen it does not interpret. But it cannot draw *alongside* the service
// either, which is where the status line's two defects come from: one cursor-save slot and one
// scrolling region, both shared with a program that does not know it is sharing.
//
// Here the client keeps a grid of its own, feeds the service's bytes into it, and paints the
// result. The status line is then simply a row the service was never given, and nothing is
// borrowed from anybody.
//
// Switched on with GOZELLIJ_RENDER=1, and off by default. It interprets every escape sequence the
// service emits, so a sequence the emulator gets wrong is a screen the user cannot fix by pressing
// Ctrl-L - whereas the byte pipe's failures are the terminal's own. The corpus says what is
// understood; until a pane needs it, the conservative default is the honest one.
type renderedScreen struct {
	mu   sync.Mutex
	term *grid.Term
	out  io.Writer

	cols, rows int
	// reserved is how many rows at the bottom belong to the status line.
	reserved int
	// line is the status text, asked for at paint time so it is never staler than the paint.
	line func(cols int) string
}

// renderEnabled reports whether this attach should own the screen.
func renderEnabled() bool {
	switch os.Getenv("GOZELLIJ_RENDER") {
	case "1", "true", "yes":
		return true
	}
	return false
}

func newRenderedScreen(out io.Writer, cols, rows, reserved int, line func(cols int) string) *renderedScreen {
	if rows-reserved < 1 {
		// A terminal too short to hold both is a terminal that gets no status line. Reserving a
		// row from a one-row screen would leave the service nothing to draw on at all.
		reserved = 0
	}
	s := &renderedScreen{
		out: out, cols: cols, rows: rows, reserved: reserved, line: line,
		term: grid.New(cols, rows-reserved),
	}
	return s
}

// ServiceSize is the screen the service is told it has, which is the terminal minus the status row.
func (s *renderedScreen) ServiceSize() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows - s.reserved
}

// Write feeds the service's output into the grid and repaints.
//
// A repaint per write, and per write rather than on a timer on purpose: a timer adds latency to
// every keystroke's echo, and this is what the user is looking at. Whole-screen repaints are what
// render.Screen does today; if that turns out to be too much for a busy pane, the fix is damage
// tracking in the renderer rather than a delay here.
func (s *renderedScreen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.term.Write(p); err != nil {
		return 0, err
	}
	return len(p), s.paint()
}

// Resize changes the terminal size and repaints.
func (s *renderedScreen) Resize(cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	reserved := s.reserved
	if rows-reserved < 1 {
		reserved = 0
	}
	s.cols, s.rows, s.reserved = cols, rows, reserved
	if err := s.term.Resize(cols, rows-reserved); err != nil {
		return err
	}
	return s.paint()
}

// Repaint draws again without anything having changed, for when something outside the grid has -
// the status line's clock, or the service being switched.
func (s *renderedScreen) Repaint() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paint()
}

// Close puts the terminal back the way a program that owned it should: attributes reset, cursor
// shown, and a newline so the shell's prompt does not land on top of the last frame.
func (s *renderedScreen) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := io.WriteString(s.out, "\x1b[0m\x1b[?25h\r\n")
	return err
}

func (s *renderedScreen) paint() error {
	panes := []layout.Pane{{
		Rect:    layout.Rect{Cols: s.cols, Rows: s.rows - s.reserved},
		Term:    s.term,
		Focused: true,
	}}
	frame := layout.Compose(s.cols, s.rows, panes)
	if s.reserved > 0 && s.line != nil {
		writeStatus(frame, s.rows-1, s.line(s.cols))
	}
	_, err := s.out.Write(render.Screen(frame))
	return err
}

// writeStatus draws the status text across one row of a frame, in reverse video.
//
// Into the frame rather than onto the terminal separately, which is the whole point: it is part of
// the picture being drawn, not something painted over one. Nothing has to move the cursor away and
// back, so nothing can lose the cursor.
func writeStatus(f *layout.Frame, row int, text string) {
	if row < 0 || row >= len(f.Cells) {
		return
	}
	style := vt.Style{Reverse: true}
	col := 0
	for _, r := range text {
		w := vt.RuneWidth(r)
		if w == 0 {
			// A combining mark belongs to the cell before it.
			if col > 0 && f.Cells[row][col-1].Content != "" {
				f.Cells[row][col-1].Content += string(r)
			}
			continue
		}
		if col+w > len(f.Cells[row]) {
			break
		}
		f.Cells[row][col] = vt.Cell{Content: string(r), Width: w, Style: style}
		for k := 1; k < w; k++ {
			f.Cells[row][col+k] = vt.Cell{Content: "", Width: 0, Style: style}
		}
		col += w
	}
	// The rest of the row is part of the bar, not of the screen behind it.
	for ; col < len(f.Cells[row]); col++ {
		f.Cells[row][col] = vt.Cell{Content: " ", Width: 1, Style: style}
	}
}

// statusLine is the text the bar shows, rendered to a width.
func statusLine(cfg status.Config, ctx func() status.Context) func(int) string {
	return func(cols int) string {
		if cols <= 0 {
			return ""
		}
		return strings.TrimRight(status.Render(ctx(), cfg.Left, cfg.Right, cols), " ")
	}
}
