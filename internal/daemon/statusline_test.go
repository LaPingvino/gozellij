package daemon

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/status"
	"github.com/creack/pty"
)

// syncBuffer is a bytes.Buffer safe to read while the painter writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// sizedTerminal gives back something IsTerminal agrees with, at a known size, so that a test can
// ask where the bottom row is. `script` will not do: when its own stdin is a pipe it allocates a
// pty with no size at all, and the painter then correctly refuses to draw, because a status line
// at a guessed row is worse than none.
func sizedTerminal(t *testing.T, cols, rows int) *pty.Winsize {
	t.Helper()
	return &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}
}

func TestTheStatusLineDrawsAtTheBottomAndReservesTheRow(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	if err := pty.Setsize(tty, sizedTerminal(t, 80, 24)); err != nil {
		t.Fatalf("Setsize: %v", err)
	}

	var out syncBuffer
	cfg := status.DefaultConfig()
	cfg.Every = 20 * time.Millisecond
	cfg.Left = []string{"session"}
	cfg.Right = []string{"services"}

	p := newStatusPainter(&lockedWriter{w: &out}, tty, cfg, func() status.Context {
		return status.Context{Service: "web", Position: 2, Of: 3, Running: 2, Services: 3}
	})
	if p == nil {
		t.Fatal("no painter on a real terminal")
	}

	waitFor(t, func() bool { return strings.Contains(out.String(), "web 2/3") })
	drawn := out.String()
	p.Close()

	// The reserved region: rows 1..23 scroll, row 24 is ours.
	if !strings.Contains(drawn, "\x1b[1;23r") {
		t.Errorf("no scrolling region was set, so the service's output would scroll over the line: %q", drawn)
	}
	if !strings.Contains(drawn, "\x1b[24;1H") {
		t.Errorf("the line was not drawn on the last row: %q", drawn)
	}
	if !strings.Contains(drawn, "2/3 up") {
		t.Errorf("the services widget did not render: %q", drawn)
	}

	// And it puts the terminal back, or the next program to run would find a short screen.
	if !strings.Contains(out.String(), "\x1b[r") {
		t.Errorf("the scrolling region was not released on close: %q", out.String())
	}
}

func TestTheStatusLineCanBePutInTheTitleInstead(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	if err := pty.Setsize(tty, sizedTerminal(t, 80, 24)); err != nil {
		t.Fatalf("Setsize: %v", err)
	}

	var out syncBuffer
	cfg := status.DefaultConfig()
	cfg.Where = status.Title
	cfg.Every = 20 * time.Millisecond
	cfg.Left = []string{"session"}
	cfg.Right = nil

	p := newStatusPainter(&lockedWriter{w: &out}, tty, cfg, func() status.Context {
		return status.Context{Service: "web"}
	})
	if p == nil {
		t.Fatal("no painter")
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "\x1b]2;") })
	p.Close()

	drawn := out.String()
	if !strings.Contains(drawn, "\x1b]2;[web]") {
		t.Errorf("the title was not set: %q", drawn)
	}
	// Nothing that draws on the grid: that is the whole point of the title placement.
	if strings.Contains(drawn, "\x1b[1;23r") {
		t.Errorf("the title placement reserved a row, which it must not: %q", drawn)
	}
}

func TestNoStatusLineWhenItIsSwitchedOff(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	cfg := status.DefaultConfig()
	cfg.Where = status.Off
	var out syncBuffer
	if p := newStatusPainter(&lockedWriter{w: &out}, tty, cfg, func() status.Context {
		return status.Context{Service: "web"}
	}); p != nil {
		p.Close()
		t.Error("a painter was started although the status line is off")
	}
	if out.String() != "" {
		t.Errorf("something was drawn with the status line off: %q", out.String())
	}
}

// These check what the sequences mean, not merely that they were written. The first version of
// this file asserted that the right bytes appeared, and every one of them did - while the terminal
// they were sent to appeared to hang, because the cursor was restored below the scrolling region
// where a line feed does not scroll. Bytes emitted is not behaviour.

func TestTheReservedRowIsSetUpBeforeAnythingIsDrawn(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	if err := pty.Setsize(tty, sizedTerminal(t, 80, 24)); err != nil {
		t.Fatalf("Setsize: %v", err)
	}

	var out syncBuffer
	cfg := status.DefaultConfig()
	cfg.Every = time.Hour // only the setup paint, so the order is unambiguous
	cfg.Left = []string{"session"}
	cfg.Right = nil

	p := newStatusPainter(&lockedWriter{w: &out}, tty, cfg, func() status.Context {
		return status.Context{Service: "web"}
	})
	if p == nil {
		t.Fatal("no painter")
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "[web]") })
	p.Close()

	drawn := out.String()
	region := strings.Index(drawn, "\x1b[1;23r")
	safe := strings.Index(drawn, "\x1b[23;1H")
	if region < 0 || safe < 0 {
		t.Fatalf("the row was not reserved with the cursor placed inside it: %q", drawn)
	}
	// The cursor has to be moved into the region as part of reserving it. Whatever was on the
	// terminal before the attach may have left it on the last row, which is about to be outside
	// - and a cursor below the bottom margin does not scroll, so the screen stops moving.
	if safe < region {
		t.Errorf("the cursor was placed before the region was set: %q", drawn)
	}
}

func TestDetachDoesNotHomeTheCursor(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	if err := pty.Setsize(tty, sizedTerminal(t, 80, 24)); err != nil {
		t.Fatalf("Setsize: %v", err)
	}

	var out syncBuffer
	cfg := status.DefaultConfig()
	cfg.Every = time.Hour
	cfg.Left = []string{"session"}

	p := newStatusPainter(&lockedWriter{w: &out}, tty, cfg, func() status.Context {
		return status.Context{Service: "web"}
	})
	waitFor(t, func() bool { return strings.Contains(out.String(), "[web]") })
	p.Close()

	drawn := out.String()
	reset := strings.LastIndex(drawn, "\x1b[r")
	restore := strings.LastIndex(drawn, "\x1b8")
	if reset < 0 || restore < 0 {
		t.Fatalf("no region reset and cursor restore on close: %q", drawn)
	}
	// DECSTBM homes the cursor like any other DECSTBM, so the reset has to come *before* the
	// restore. The other order put the cursor at the top of the screen on every detach, and the
	// next shell prompt printed over whatever was already there.
	if reset > restore {
		t.Errorf("the region was reset after the cursor was restored, so the cursor was homed: %q", drawn)
	}
}

func TestTheServiceIsToldItHasOneRowFewer(t *testing.T) {
	// The single thing that stops a full-screen program fighting the status line: if the service
	// believes it has the whole screen, less will put its prompt on the reserved row and vim will
	// put its command line there, and the next tick will wipe them.
	cfg := status.DefaultConfig()

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	if err := pty.Setsize(tty, sizedTerminal(t, 80, 24)); err != nil {
		t.Fatalf("Setsize: %v", err)
	}

	var out syncBuffer
	p := newStatusPainter(&lockedWriter{w: &out}, tty, cfg, func() status.Context {
		return status.Context{Service: "web"}
	})
	defer p.Close()
	if got := p.Reserved(); got != 1 {
		t.Errorf("Reserved = %d for a bottom status line, want 1", got)
	}

	cfg.Where = status.Title
	titled := newStatusPainter(&lockedWriter{w: &out}, tty, cfg, func() status.Context {
		return status.Context{Service: "web"}
	})
	defer titled.Close()
	if got := titled.Reserved(); got != 0 {
		t.Errorf("Reserved = %d for a title status line, want 0: it takes no row", got)
	}

	var none *statusPainter
	if got := none.Reserved(); got != 0 {
		t.Errorf("Reserved = %d with no painter at all, want 0", got)
	}
}

// The reserve sequence makes room before it takes the row.
//
// Two things this pins that a comment could not. The scroll has to be there at all: without it the
// cursor lands on the last line of existing content and the replayed prompt prints over it, gone
// from the screen and from the scrollback both. And reserve has to run before the painter's
// goroutine, because the replay starts as soon as the attach is answered - a claim made once in a
// commit message while the call was still the first line of run(), where the edit had silently not
// matched.
func TestReserveMakesRoomBeforeTakingTheRow(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	t.Cleanup(func() { ptmx.Close(); tty.Close() })
	if err := pty.Setsize(tty, sizedTerminal(t, 80, 24)); err != nil {
		t.Fatalf("Setsize: %v", err)
	}

	var out syncBuffer
	cfg := status.DefaultConfig()
	cfg.Every = time.Hour
	cfg.Left = []string{"session"}
	cfg.Right = nil

	// info blocks, so nothing but reserve can have written by the time we look: if the sequence
	// is there, it was emitted before the goroutine got anywhere.
	release := make(chan struct{})
	p := newStatusPainter(&lockedWriter{w: &out}, tty, cfg, func() status.Context {
		<-release
		return status.Context{Service: "web"}
	})
	if p == nil {
		t.Fatal("no painter")
	}
	drawn := out.String()
	close(release)
	t.Cleanup(p.Close)

	if !strings.HasPrefix(drawn, "\x1b[2S") {
		t.Errorf("reserve did not make room before taking the row: %q", drawn)
	}
	scroll := strings.Index(drawn, "\x1b[2S")
	region := strings.Index(drawn, "\x1b[1;23r")
	if scroll < 0 || region < 0 || scroll > region {
		t.Errorf("the room was not made before the region was set: %q", drawn)
	}
	if !strings.Contains(drawn, "\x1b[23;1H") {
		t.Errorf("the cursor was not placed on the freed row: %q", drawn)
	}
}
