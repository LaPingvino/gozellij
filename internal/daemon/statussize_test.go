package daemon

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/status"
)

// The size the status line builds its sequence from has to be read while the terminal lock is
// held, not before taking it.
//
// Both paint and reserve write a scrolling region derived from the row count - "\x1b[1;%dr" with
// rows-1. Read outside the lock, that number can describe a screen that has since been resized:
// a SIGWINCH in the gap, which is exactly what splitting a window delivers to the pane that
// shrinks, leaves the terminal with a region set for a screen it no longer has.
//
// Asserted through TryLock rather than by trying to race it. A Go mutex is not reentrant, so a
// TryLock that *succeeds* proves nobody holds the lock - which is the failure. Racing it would
// need a window narrower than the one this is about, and a test that has to win a race to fail is
// a test that will one day quietly stop failing.
func TestTheStatusLineReadsTheSizeUnderTheTerminalLock(t *testing.T) {
	for _, c := range []struct {
		name string
		call func(p *statusPainter)
	}{
		{"paint", func(p *statusPainter) { p.paint() }},
		{"reserve", func(p *statusPainter) { p.reserve() }},
		{"clear", func(p *statusPainter) { p.clear() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			out := &lockedWriter{w: &buf}
			asked := 0
			p := &statusPainter{
				out: out,
				cfg: status.Config{Where: status.Bottom, Left: status.DefaultLeft},
				sizeOf: func() (int, int) {
					asked++
					if out.mu.TryLock() {
						out.mu.Unlock()
						t.Errorf("the size was read with the terminal lock free, so a resize "+
							"between reading it and writing the sequence built from it is "+
							"invisible (call %d)", asked)
					}
					return 80, 24
				},
				info: func() status.Context { return status.Context{Service: "svc"} },
			}
			c.call(p)
			if asked == 0 {
				t.Fatal("the size was never read at all, so this checks nothing")
			}
		})
	}
}

// And the region it writes is derived from the size it just read, which is what makes reading it
// under the lock worth anything.
func TestTheStatusLineRegionMatchesTheSizeItRead(t *testing.T) {
	var buf bytes.Buffer
	out := &lockedWriter{w: &buf}
	p := &statusPainter{
		out:    out,
		cfg:    status.Config{Where: status.Bottom, Left: status.DefaultLeft},
		sizeOf: func() (int, int) { return 80, 10 },
		info:   func() status.Context { return status.Context{Service: "svc"} },
	}
	p.paint()
	// Ten rows: the region is rows 1 to 9 and the line is drawn on row 10.
	if got := buf.String(); !strings.Contains(got, "\x1b[1;9r") || !strings.Contains(got, "\x1b[10;1H") {
		t.Fatalf("a ten-row terminal was drawn as %q", got)
	}
}
