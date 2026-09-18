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
