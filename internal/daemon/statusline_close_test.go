package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/status"
	"github.com/creack/pty"
)

// Close waits for the run goroutine, and the run goroutine may be inside info() - which dials the
// daemon with a 10s Dial timeout and a 30s Call timeout. A detach must not wait on that.
// Letting go of a terminal must not wait on a daemon that has stopped answering.
//
// A paint dials the daemon, and Close waits for the painter - so before the query was bounded, a
// hung daemon turned detaching into a wait of up to Dial's ten seconds plus Call's thirty. The
// query is bounded now, and Close is bounded on top of that: if the painter has not noticed in
// time, Close puts the terminal back itself rather than holding the user there.
func TestCloseDoesNotWaitOnASlowDaemon(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	// Cleanups, not defers, and the file closes last: Close is bounded now, so it can return
	// while the painter is still inside a slow query - and that painter goes on to touch this
	// terminal. Closing the file out from under it is a race the detector is right about.
	t.Cleanup(func() { ptmx.Close(); tty.Close() })
	if err := pty.Setsize(tty, &pty.Winsize{Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("Setsize: %v", err)
	}

	release := make(chan struct{})
	entered := make(chan struct{}, 1)

	var out syncBuffer
	cfg := status.DefaultConfig()
	cfg.Every = time.Hour
	p := newStatusPainter(&lockedWriter{w: &out}, tty, cfg, func() status.Context {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // a daemon that never answers
		return status.Context{Service: "web"}
	})
	if p == nil {
		t.Fatal("no painter")
	}
	<-entered

	closed := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		p.Close()
		closed <- time.Since(start)
	}()

	select {
	case d := <-closed:
		if d > closeWait+2*time.Second {
			t.Errorf("Close took %v with the daemon hung, want at most about %v", d, closeWait)
		}
	case <-time.After(closeWait + 5*time.Second):
		t.Fatalf("Close never returned with the daemon hung; detaching would hang with it")
	}

	// And the terminal was put back, which is the thing the wait was protecting.
	if !strings.Contains(out.String(), "\x1b[r") {
		t.Errorf("the scrolling region was not released: %q", out.String())
	}

	// Let the painter finish before the terminal is closed underneath it.
	close(release)
	p.Close()
}

func TestRepaintAfterCloseDrawsNothing(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	if err := pty.Setsize(tty, &pty.Winsize{Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("Setsize: %v", err)
	}
	var out syncBuffer
	cfg := status.DefaultConfig()
	cfg.Every = time.Hour
	cfg.Left = []string{"session"}
	cfg.Right = nil
	p := newStatusPainter(&lockedWriter{w: &out}, tty, cfg, func() status.Context { return status.Context{Service: "web"} })
	waitFor(t, func() bool { return strings.Contains(out.String(), "[web]") })
	p.Close()
	before := out.String()
	p.Repaint()
	after := out.String()
	if after != before {
		t.Errorf("Repaint after Close drew on the terminal after the region was released: %q", after[len(before):])
	}
}
