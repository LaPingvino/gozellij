package daemon

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/LaPingvino/gozellij/internal/status"
	"golang.org/x/term"
)

// lockedWriter serialises writes to the terminal.
//
// Two things draw there now - the service's output and the status line - and without this they
// interleave mid-escape-sequence, which is not a glitch but a corrupted screen: half a cursor
// movement followed by half a colour change is a sequence the terminal will happily obey.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// atomically runs f with the terminal to itself.
func (l *lockedWriter) atomically(f func(io.Writer)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(l.w)
}

// statusPainter draws the status line at the bottom of the terminal.
//
// It works by reserving the last row with a scrolling region (DECSTBM): the service's output
// scrolls inside rows 1..n-1 and row n is ours. This needs no terminal emulator, which is the
// whole reason it is possible at all in a program that proxies bytes - but it is also the reason
// it is not perfect. A full-screen program sets its own scrolling region and uses the whole
// screen, so vim and top will draw over the line and reset the region when they exit. The region
// is re-asserted on every repaint, so the line comes back by itself within a tick.
//
// That is the honest state of it, and it is the thing a terminal emulator would fix properly by
// owning the grid.
// statusQueryTimeout bounds one status query. See StatusContext.
const statusQueryTimeout = 1500 * time.Millisecond

type statusPainter struct {
	out  *lockedWriter
	in   *os.File
	cfg  status.Config
	info func() status.Context

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// newStatusPainter starts painting, or returns nil when there is nothing to paint on.
func newStatusPainter(out *lockedWriter, in *os.File, cfg status.Config, info func() status.Context) *statusPainter {
	if cfg.Where != status.Bottom && cfg.Where != status.Title {
		return nil
	}
	if !term.IsTerminal(int(in.Fd())) {
		// No terminal, no status line. A pipe gets the service's bytes and nothing of ours.
		return nil
	}
	p := &statusPainter{
		out:  out,
		in:   in,
		cfg:  cfg,
		info: info,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go p.run()
	return p
}

// Reserved is how many rows of the terminal belong to the status line rather than to the service.
//
// The service must be told a screen that many rows shorter, and that is not a nicety. A program
// that believes it owns the last row will draw on it: less puts its prompt there, vim puts its
// command line there, and both were being overwritten by the next tick - the `:` vanishing from
// under the user's fingers two seconds after they typed it. Telling the service the truth about
// how much screen it has is what stops the collision; re-asserting our region afterwards only
// fights over the wreckage.
//
// It also keeps the cursor inside the scrolling region, which is what makes the save-and-restore
// in paint safe at all.
func (p *statusPainter) Reserved() int {
	if p == nil || p.cfg.Where != status.Bottom {
		return 0
	}
	return 1
}

func (p *statusPainter) run() {
	defer close(p.done)

	tick := time.NewTicker(p.cfg.Every)
	defer tick.Stop()

	// A resize moves the row the line lives on, and most terminals reset the margins when they
	// resize - so without this the line sits at the old row, in a terminal with no reserved
	// region, until the next tick happens to notice.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	p.reserve()
	p.paint()
	for {
		select {
		case <-tick.C:
			p.paint()
		case <-winch:
			p.reserve()
			p.paint()
		case <-p.stop:
			p.clear()
			return
		}
	}
}

// closeWait bounds how long detaching waits for the painter to notice.
//
// A paint can only be as slow as one status query, which is bounded - but "bounded" is not
// "instant", and nothing about letting go of a terminal should wait on a daemon that has stopped
// answering. If the painter has not finished by then, the terminal is put back from here instead;
// the painter's own clear afterwards is the same sequence again, which costs nothing.
const closeWait = statusQueryTimeout + time.Second

// Close stops painting and puts the terminal back.
func (p *statusPainter) Close() {
	if p == nil {
		return
	}
	p.once.Do(func() { close(p.stop) })
	select {
	case <-p.done:
	case <-time.After(closeWait):
		p.clear()
	}
}

// Repaint draws immediately, for when something has changed that a tick should not have to wait
// for - switching service, most obviously.
func (p *statusPainter) Repaint() {
	if p == nil {
		return
	}
	// Not after Close. Painting again would re-assert the scrolling region that Close just
	// released, and leave the terminal short by a row for whatever runs next - which is the
	// state a detach is supposed to get you out of.
	select {
	case <-p.stop:
		return
	default:
	}
	p.paint()
}

func (p *statusPainter) size() (cols, rows int) {
	c, r, err := term.GetSize(int(p.in.Fd()))
	if err != nil || c <= 0 || r <= 1 {
		return 0, 0
	}
	return c, r
}

// reserve sets the scrolling region up once, before anything else is drawn.
//
// It also puts the cursor inside the region, and that is the point. Whatever was on the terminal
// before the attach - a shell prompt at the bottom of the screen, most likely - may have left the
// cursor on the last row, which is about to be outside the region. A cursor below the bottom
// margin does not scroll on a line feed, so the screen simply stops moving: the user types and
// nothing happens, which reads as a hang rather than as a bug. Measured on a real terminal: three
// commands typed, nothing shown, the rows never moved.
//
// This runs before the service's output is replayed, so moving the cursor costs nothing.
func (p *statusPainter) reserve() {
	if p.cfg.Where != status.Bottom {
		return
	}
	_, rows := p.size()
	if rows < 2 {
		return
	}
	p.out.atomically(func(w io.Writer) {
		fmt.Fprintf(w, "\x1b[1;%dr\x1b[%d;1H", rows-1, rows-1)
	})
}

func (p *statusPainter) paint() {
	cols, rows := p.size()
	if cols == 0 {
		return
	}
	ctx := p.info()

	if p.cfg.Where == status.Title {
		// OSC 2. Nothing can draw over a title bar, which is the whole appeal; the cost is that
		// you only see it if your terminal shows one.
		line := status.Render(ctx, p.cfg.Left, p.cfg.Right, 0)
		p.out.atomically(func(w io.Writer) {
			fmt.Fprintf(w, "\x1b]2;%s\x07", line)
		})
		return
	}

	line := status.Render(ctx, p.cfg.Left, p.cfg.Right, cols)
	p.out.atomically(func(w io.Writer) {
		// Save the cursor, re-assert the region (a full-screen program that has exited will have
		// reset it), go to the last row, draw, and put the cursor back. The service never sees
		// any of this: it is written to the terminal, not to the pty.
		fmt.Fprintf(w, "\x1b7\x1b[1;%dr\x1b[%d;1H\x1b[2K\x1b[7m%s\x1b[0m\x1b8", rows-1, rows, line)
	})
}

// clear releases the reserved row and puts the scrolling region back to the whole screen.
func (p *statusPainter) clear() {
	if p.cfg.Where == status.Title {
		p.out.atomically(func(w io.Writer) { fmt.Fprint(w, "\x1b]2;\x07") })
		return
	}
	_, rows := p.size()
	if rows == 0 {
		return
	}
	p.out.atomically(func(w io.Writer) {
		// The region reset goes *inside* the save and restore. DECSTBM homes the cursor like
		// any other DECSTBM, so resetting after the restore undid it: every detach put the
		// cursor at the top of the screen, and the next shell prompt printed over whatever was
		// already there. Measured on a real terminal, row 24 to row 1.
		fmt.Fprintf(w, "\x1b7\x1b[%d;1H\x1b[2K\x1b[r\x1b8", rows)
	})
}

// StatusContext gathers what only the daemon knows, for the widgets that need it.
//
// On a fresh connection each time rather than a held one: the status line is a bystander, and a
// bystander that keeps a connection open across a daemon upgrade would be one more thing to
// reconnect. A local socket round trip every couple of seconds is not the expensive part of
// anything.
func StatusContext(socket, service string) status.Context {
	ctx := status.Context{Service: service, StateDir: StateDir()}

	c, err := Dial(socket)
	if err != nil {
		// The daemon is being replaced, most likely. The host widgets still work, so draw what
		// can be drawn rather than blanking the line.
		return ctx
	}
	defer c.Close()

	// A short deadline, not the ordinary thirty second one. This runs on the painter's goroutine
	// and Close waits for that goroutine, so a daemon that has stopped answering would turn
	// detaching into a forty second wait. A status line is a bystander: if it cannot get an
	// answer promptly it should draw what it has and try again on the next tick.
	if err := c.Conn().SetDeadline(time.Now().Add(statusQueryTimeout)); err != nil {
		return ctx
	}

	list, err := c.List()
	if err != nil {
		return ctx
	}

	names := make([]string, 0, len(list.Services))
	for _, svc := range list.Services {
		names = append(names, svc.Service)
		ctx.LogBytes += svc.LogBytes
		if svc.State == "running" {
			ctx.Running++
		}
		if svc.Service == service {
			ctx.Viewers = svc.Viewers
		}
	}
	ctx.Services = len(names)

	sort.Strings(names)
	for i, n := range names {
		if n == service {
			ctx.Position = i + 1
			break
		}
	}
	ctx.Of = len(names)
	return ctx
}
