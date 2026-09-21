package daemon

import (
	"io"
	"os"
	"strings"
	"sync"

	"github.com/LaPingvino/gozellij/internal/status"
	"github.com/LaPingvino/gozellij/internal/vt"
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
	mu  sync.Mutex
	out io.Writer

	// lastPanes is what was drawn last, so that a repaint for the clock draws the same screen
	// rather than a different one.
	lastPanes []layoutPane
	lastFocus int

	cols, rows int
	// reserved is how many rows at the bottom belong to the status line.
	reserved int
	// line is the status text, asked for at paint time so it is never staler than the paint.
	line func(cols int) string
}

// RenderMode is how an attach decides whether to own the screen.
type RenderMode int

const (
	// RenderAuto takes the setting from the environment: GOZELLIJ_RENDER=1 turns it on.
	RenderAuto RenderMode = iota
	// RenderOn and RenderOff are an explicit choice, from a flag. A flag beats the environment
	// so that a person who has switched rendering on for their session can still get the plain
	// attach for one command, which is what they will want the first time a screen looks wrong.
	RenderOn
	RenderOff
)

// renderEnabled reports whether this attach should own the screen.
func renderEnabled(mode RenderMode) bool {
	switch mode {
	case RenderOn:
		return true
	case RenderOff:
		return false
	}
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
	return &renderedScreen{out: out, cols: cols, rows: rows, reserved: reserved, line: line}
}

// ServiceSize is the screen the service is told it has, which is the terminal minus the status row.
func (s *renderedScreen) ServiceSize() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows - s.reserved
}

// Repaint draws the same panes again, for when something outside them has changed - the status
// line's clock, most obviously.
//
// The same panes, remembered from the last paint, and that is the point. An earlier version kept a
// grid of its own for this and painted that instead, so the ticker quietly replaced the screen
// with an empty one a second after every keystroke. Two things that both paint the terminal is one
// too many; there is now a single path and it cannot disagree with itself.
func (s *renderedScreen) Repaint() error {
	s.mu.Lock()
	panes, focus := s.lastPanes, s.lastFocus
	s.mu.Unlock()
	if len(panes) == 0 {
		return nil
	}
	return s.PaintPanes(panes, focus)
}

// Resize records the new terminal size. It does not draw: the session that owns the panes has to
// give each of them its new rectangle first, and painting in between would show a screen laid out
// for the old size.
func (s *renderedScreen) Resize(cols, rows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	reserved := s.reserved
	if rows-reserved < 1 {
		reserved = 0
	}
	s.cols, s.rows, s.reserved = cols, rows, reserved
	return nil
}

// Close puts the terminal back the way a program that owned it should: attributes reset, cursor
// shown, and a newline so the shell's prompt does not land on top of the last frame.
func (s *renderedScreen) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := io.WriteString(s.out, "\x1b[0m\x1b[?25h\r\n")
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

// PaintPanes draws several panes and the status line as one screen.
//
// The whole picture in one composition: the panes, the blank seams between them, a marker on the
// focused one, and the status row. Nothing is drawn over anything else, which is what makes the
// cursor land where it belongs without anything having to save and restore it.
func (s *renderedScreen) PaintPanes(panes []layoutPane, focus int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPanes, s.lastFocus = panes, focus

	ps := make([]layout.Pane, 0, len(panes))
	for i, p := range panes {
		ps = append(ps, layout.Pane{Rect: p.Rect(), Term: p.Grid(), Focused: i == focus})
	}
	frame := layout.Compose(s.cols, s.rows, ps)
	if s.reserved > 0 && s.line != nil {
		text := s.line(s.cols)
		if len(panes) > 1 {
			// Which pane has the keyboard, since with two shells on screen there is no other way
			// to tell. In front of the rest of the line: it is the thing that changes what your
			// next keystroke does.
			text = trimToWidth(paneMarker(panes, focus)+" "+text, s.cols)
		}
		writeStatus(frame, s.rows-1, text)
	}
	_, err := s.out.Write(render.Screen(frame))
	return err
}

// layoutPane is what PaintPanes needs of a pane, so that this file does not have to know what a
// session is.
type layoutPane interface {
	Rect() layout.Rect
	Grid() vt.Grid
	Service() string
}

func paneMarker(panes []layoutPane, focus int) string {
	var b strings.Builder
	for i, p := range panes {
		if i > 0 {
			b.WriteString(" ")
		}
		if i == focus {
			b.WriteString("[" + p.Service() + "]")
			continue
		}
		b.WriteString(p.Service())
	}
	return b.String()
}

func trimToWidth(s string, cols int) string {
	if vt.StringWidth(s) <= cols {
		return s
	}
	return vt.TruncateToWidth(s, cols)
}
