package daemon

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

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
	mu  sync.Mutex
	out io.Writer

	// lastPanes is what was drawn last, so that a repaint for the clock draws the same screen
	// rather than a different one.
	lastPanes []layoutPane
	lastFocus int

	// message is the last thing gozellij had to say, and when it said it. It takes over the
	// status line for a few seconds rather than going to standard error, which in a rendered
	// session is covered by the next repaint within milliseconds - so "your service exited with
	// code 1" was being written to a screen that erased it before anybody could read it.
	message string
	said    time.Time

	// applied is what the real terminal has been put into: mouse reporting, bracketed paste and
	// focus events, as asked for by whichever pane has the keyboard. A byte pipe passes those
	// sequences straight through and gets this for free; a client that interprets them has to
	// hand them on deliberately, and not doing it is how pasting into vim breaks in a mode that
	// otherwise looks right.
	applied map[int]bool
	// title is what the terminal has been told to call itself, so that it is only told when it
	// changes rather than on every repaint.
	title string
	// shape is the cursor shape the terminal has been told to use, so that it is told only when
	// it changes.
	shape int

	// suspended stops painting while something else owns the screen - the service picker, which
	// draws a menu and waits for a keystroke. Without it the repaint that keeps the clock moving
	// drew the last frame straight over the menu, so `Ctrl-] l` asked a question nobody could see
	// and the answer still worked, which is the worst of both.
	suspended bool

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
	// Clear the status row on the way out. What the panes drew can stay - it is output, and a
	// terminal keeps output - but a status bar left along the bottom says gozellij is still here
	// when it is not, and the next shell prompt appears above it.
	s.releaseModes()
	if s.shape != 0 {
		// Back to the terminal's own default. A cursor left as a blinking bar after a detach is
		// the same class of mess as mouse reporting left switched on: it outlives the program
		// that asked for it.
		s.shape = 0
		fmt.Fprint(s.out, "\x1b[0 q")
	}
	var b strings.Builder
	b.WriteString("\x1b[0m")
	if s.reserved > 0 && s.rows > 0 {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", s.rows)
	}
	fmt.Fprintf(&b, "\x1b[%d;1H\x1b[?25h\r\n", max(s.rows-s.reserved, 1))
	_, err := io.WriteString(s.out, b.String())
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

// messageLinger is how long something gozellij says stays on the status line.
const messageLinger = 6 * time.Second

// Suspend and Resume hand the screen to something else and take it back.
//
// Whatever drew while suspended is not cleared here: the caller drew it and the caller is the one
// that knows when it is finished with it. Resuming paints the panes again, which covers it.
func (s *renderedScreen) Suspend() {
	s.mu.Lock()
	s.suspended = true
	s.mu.Unlock()
}

func (s *renderedScreen) Resume() {
	s.mu.Lock()
	s.suspended = false
	s.mu.Unlock()
	_ = s.Repaint()
}

// Forget drops what this client believes the terminal already has, so that the next paint sends
// everything again.
//
// The modes, the title and the cursor shape are only sent when they change, which means a terminal
// that lost them - a program that reset it, a serial line that dropped bytes - would not get them
// back until something changed again. A redraw is somebody saying the screen is wrong, and the
// honest response is to stop assuming anything about it.
func (s *renderedScreen) Forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = nil
	s.title = ""
	s.shape = 0
}

// Say puts a message on the status line for a few seconds.
func (s *renderedScreen) Say(msg string) {
	if msg == "" {
		return
	}
	s.mu.Lock()
	s.message, s.said = msg, time.Now()
	s.mu.Unlock()
	_ = s.Repaint()
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
	if s.suspended {
		// Remembered but not drawn, so that resuming shows the current screen rather than a
		// stale one.
		return nil
	}

	ps := make([]layout.Pane, 0, len(panes))
	for i, p := range panes {
		ps = append(ps, layout.Pane{Rect: p.Rect(), Term: p.Grid(), Focused: i == focus})
	}
	// The focused pane's modes, because they are about the keyboard and the mouse and those go to
	// one pane at a time.
	if focus < len(panes) {
		s.applyModes(panes[focus].Modes())
		s.applyTitle(panes[focus].Title())
		s.applyShape(panes[focus].CursorShape())
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
		if s.message != "" && time.Since(s.said) < messageLinger {
			// A message takes the whole line, marker included. It is transient and it is the
			// thing to read right now - "your service exited with code 3" truncated to "exited
			// with code" because a pane marker had the first fifteen columns is not worth having.
			// The marker is back in a few seconds.
			text = trimToWidth("gozellij: "+s.message, s.cols)
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
	// Modes are the terminal-level modes this pane's program has asked for.
	Modes() map[int]bool
	// Title is what this pane's program asked the window to be called.
	Title() string
	// CursorShape is the shape that program asked for, zero for the terminal's default.
	CursorShape() int
}

// applyModes puts the real terminal into the state a pane asked for, changing only what differs.
//
// Only the difference, because these sequences are not free: a terminal that is told to enable
// mouse reporting on every repaint is being told several times a second.
func (s *renderedScreen) applyModes(want map[int]bool) {
	if s.applied == nil {
		s.applied = make(map[int]bool, len(grid.PassthroughModes))
	}
	var b strings.Builder
	for _, m := range grid.PassthroughModes {
		on := want[m]
		if on == s.applied[m] {
			continue
		}
		verb := "l"
		if on {
			verb = "h"
		}
		fmt.Fprintf(&b, "\x1b[?%d%s", m, verb)
		s.applied[m] = on
	}
	if b.Len() > 0 {
		_, _ = io.WriteString(s.out, b.String())
	}
}

// applyTitle tells the terminal what to call itself, when that has changed.
//
// The focused pane's, because a window has one title and the keyboard is in one pane. Falls back
// to the service's name: a shell that has not set a title should still leave something useful in
// the window, and "gozellij" alone would be worse than useless with two of them open.
func (s *renderedScreen) applyTitle(title string) {
	if title == "" {
		return
	}
	if title == s.title {
		return
	}
	s.title = title
	fmt.Fprintf(s.out, "\x1b]2;%s\x07", title)
}

// applyShape tells the terminal what shape to draw the cursor, when that has changed.
//
// The focused pane's, like the title and the modes: there is one cursor. A program that asks for a
// bar while editing and a block otherwise is doing something the user can see, and a client that
// interprets the stream has to carry it or the shape is whatever the last program to set it left
// behind.
func (s *renderedScreen) applyShape(shape int) {
	if shape == s.shape {
		return
	}
	s.shape = shape
	fmt.Fprintf(s.out, "\x1b[%d q", shape)
}

// releaseModes puts back everything this client switched on.
//
// On the way out, always. Leaving mouse reporting enabled after a detach means the user's own
// shell starts receiving escape sequences whenever they click, which is the kind of mess that
// outlives the program that caused it.
func (s *renderedScreen) releaseModes() {
	var b strings.Builder
	for m, on := range s.applied {
		if on {
			fmt.Fprintf(&b, "\x1b[?%dl", m)
			s.applied[m] = false
		}
	}
	if b.Len() > 0 {
		_, _ = io.WriteString(s.out, b.String())
	}
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
