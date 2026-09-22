package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LaPingvino/gozellij/internal/ipc"
	"github.com/LaPingvino/gozellij/internal/vt"
	"github.com/LaPingvino/gozellij/internal/vt/grid"
	"github.com/LaPingvino/gozellij/internal/vt/layout"
	"golang.org/x/term"
)

// The rendered session is where gozellij stops being one service in one terminal.
//
// The byte-pipe attach can only ever show one thing: it hands the service's output to the user's
// terminal unchanged, and two services would be two programs writing over each other. Owning the
// grid is what makes a split screen possible at all, which is the whole reason DESIGN.md puts the
// emulator before the multiplexing.
//
// One goroutine per pane reads that pane's frames and posts them to a single channel, and one
// select owns everything else - the keyboard, the commands, the resizes. The same reasoning as
// runSession's: the things that can happen next are few and naming them all in one place is easier
// to be sure about than a mutex shared between readers.

// a livePane is one service being shown.
type livePane struct {
	service string
	client  *Client
	term    *grid.Term
	rect    layout.Rect
	// finished marks a service that has ended. Its pane stays on screen with its last output,
	// because a pane that vanishes takes the error message with it.
	finished bool
	// scroll is how many lines back this pane is being looked at. Zero is live.
	scroll int
	// told is which unimplemented sequences the user has already been told about, so that a
	// program emitting one on every keystroke says it once rather than filling the status line
	// with the same complaint.
	told map[string]bool
	// weight is this pane's share of the screen. Every pane starts equal; growing one takes from
	// the others rather than from nowhere, because the screen does not get any bigger.
	weight float64
}

// Rect, Grid and Service are what the painter needs of a pane.
func (p *livePane) Rect() layout.Rect { return p.rect }
func (p *livePane) Grid() vt.Grid     { return p.term.Scrolled(p.scroll) }
func (p *livePane) Service() string   { return p.service }

// Modes are the terminal-level modes this pane's program has asked for.
func (p *livePane) Modes() map[int]bool { return p.term.Modes() }

// CursorShape is the shape this pane's program asked for, zero for the terminal's default.
func (p *livePane) CursorShape() int { return p.term.Cursor().Shape }

// Keypad is whether this pane's program asked for application keypad mode.
func (p *livePane) Keypad() bool { return p.term.Keypad() }

// Title is what this pane's program asked the window to be called, or the service's name when it
// has not asked. A window with no title at all is worse than one named after what is in it.
func (p *livePane) Title() string {
	if t := p.term.Title(); t != "" {
		return t
	}
	return p.service
}

// paneEvent is something a pane's connection had to say.
type paneEvent struct {
	pane     *livePane
	data     []byte
	finished bool
	// gone means this pane's connection ended. Distinct from finished, which is the service
	// itself ending: a daemon being replaced ends every connection and no service has stopped.
	gone    bool
	err     error
	message string
}

// minRepaint is the shortest time between two repaints caused by a service's output.
//
// A measured number, not a taste. A flood of twenty thousand lines arrived as 378 frames and drew
// 379 whole screens - one per frame - and those repaints took 4.7 of the 5.9 seconds the flood
// lasted, against 1.1 seconds spent interpreting the bytes. A whole-screen repaint costs well
// under a millisecond to *build* (BenchmarkFloodPaint in internal/vt/render); what it costs is
// handing eight kilobytes of escape sequences to a real terminal and waiting for it to draw them,
// and doing that once per frame writes several times more to the terminal than the byte pipe does.
//
// The paint loop had been ruled out once before, by an experiment that batched events that were
// ready at the same instant and found it changed nothing. It changed nothing because the events
// were not ready at the same instant: the client was painting between every pair of them, so
// there was never a second one waiting. A rate is what breaks that, where draining the queue could
// not.
//
// Fifty milliseconds is twenty screens a second, which is smooth to watch, and it bounds the cost
// of output at twenty repaints a second however fast a program shouts. A keystroke's own answer is
// not delayed by it: only output goes through the limit.
const minRepaint = 50 * time.Millisecond

// renderedSession shows one or more services at once and returns why it ended.
func renderedSession(socket string, first *Client, service string, input *terminalInput, in *os.File, screen *renderedScreen, replay bool) (attachOutcome, error) {
	cols, rows := screen.ServiceSize()
	if _, err := first.Call(ipc.OpAttach, service, ipc.AttachRequest{Cols: cols, Rows: rows, Replay: replay}); err != nil {
		return outcomeDisconnected, err
	}

	events := make(chan paneEvent, 64)
	panes := []*livePane{{service: service, client: first, term: grid.New(cols, rows)}}
	// How the panes are arranged, set by whichever split key was pressed last.
	how := inColumns
	focus := 0
	// What this attach last looked like, if it has been here before. Anything that goes wrong
	// reading it is said on the status line once the status line exists, a few lines down: a
	// layout file that cannot be read is a thing to hear about, not a reason to refuse to attach.
	var restored []string
	// What is already on disk. A restore that had to leave a pane out - a service that has since
	// been removed, a terminal too narrow for all of them - must not then write the smaller
	// arrangement back, or a service that was stopped for an afternoon is forgotten forever. Only
	// something the user did changes the file.
	written := ""
	if l, ok, err := loadLayout(service); err != nil {
		restored = append(restored, err.Error())
	} else if ok {
		var used bool
		panes, how, focus, used, restored = restorePanes(socket, service, first, screen, l)
		if used {
			written = l.signature()
		}
	}
	layoutPanes(panes, screen, how)
	if len(panes) > 1 {
		resizePanes(panes)
	}
	for _, p := range panes {
		go readFrames(p, events)
	}

	defer func() {
		for _, p := range panes {
			p.client.Close()
		}
	}()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	// Messages go to the status line, where they will be seen. note() writes to standard error,
	// which in a rendered session the next repaint covers within milliseconds.
	note := screen.Say
	lastPaint := time.Time{}
	paint := func() {
		ps := make([]layoutPane, len(panes))
		for i, p := range panes {
			ps[i] = p
		}
		_ = screen.PaintPanes(ps, focus)
		lastPaint = time.Now()
	}
	// A repaint owed but not yet drawn, because one was drawn too recently. due fires when it is
	// allowed; stopped and unset the rest of the time, so an idle session is an idle process.
	due := time.NewTimer(time.Hour)
	due.Stop()
	owed := false
	defer due.Stop()
	// paintSoon draws, or arranges to draw, whichever keeps the screen inside minRepaint of the
	// truth. Everything a *service* produces goes through this; a keystroke's own answer still
	// draws at once, because that is the one a person is waiting for.
	paintSoon := func() {
		if wait := minRepaint - time.Since(lastPaint); wait > 0 {
			if !owed {
				owed = true
				due.Reset(wait)
			}
			return
		}
		owed = false
		paint()
	}
	for _, m := range restored {
		note(m)
	}
	paint()

	// The arrangement is written down after every command that could have changed it, not on the
	// way out. A login multiplexer's client usually ends by its connection dropping rather than by
	// somebody pressing detach, and a layout saved only on a clean exit is missing in exactly the
	// case it exists for. One site rather than one per command, because a mutating case added
	// later would not think to call it: the signature is what keeps this from being a file write
	// per keystroke.
	remember := func() {
		l := layoutOf(panes, how, focus)
		if sig := l.signature(); sig != written {
			if err := saveLayout(service, l); err != nil {
				note(err.Error())
			}
			written = sig
		}
	}
	remember()

	data, cmds, ended := input.data, input.cmds, input.ended
	for {
		select {
		case chunk := <-data:
			// The terminal's answer to the colour question comes back this way, because to a
			// terminal an answer and a keystroke are the same thing. Taken out before anything
			// else looks at it; everything that is not an answer carries on as typing.
			chunk = screen.TakeColourReplies(chunk)
			if len(chunk) == 0 {
				continue
			}
			// To the focused pane only. A keystroke that went to all of them would be typed
			// into every shell on the screen at once, which is the kind of mistake that is
			// discovered by running rm in the wrong one.
			if err := panes[focus].client.Writer().WriteFrame(ipc.KindData, chunk); err != nil {
				return outcomeDisconnected, err
			}

		case want := <-cmds:
			// Everything typed before the command goes first, for the same reason runSession
			// drains here: two ready channels are chosen between at random.
			for draining := true; draining; {
				select {
				case chunk := <-data:
					if chunk = screen.TakeColourReplies(chunk); len(chunk) > 0 {
						_ = panes[focus].client.Writer().WriteFrame(ipc.KindData, chunk)
					}
				default:
					draining = false
				}
			}
			switch want {
			case outcomeSplit, outcomeSplitRows:
				next, err := nextUnshown(socket, panes[focus].service, panes)
				if err != nil {
					note(err.Error())
					paint()
					continue
				}
				p, err := openPane(socket, next, screen, len(panes)+1)
				if err != nil {
					note(err.Error())
					paint()
					continue
				}
				panes = append(panes, p)
				focus = len(panes) - 1
				how = want == outcomeSplitRows
				layoutPanes(panes, screen, how)
				resizePanes(panes)
				go readFrames(p, events)
				paint()

			case outcomeScrollBack, outcomeScrollForward, outcomeScrollLive:
				p := panes[focus]
				_, rows := p.term.Size()
				step := max(rows/2, 1)
				switch want {
				case outcomeScrollBack:
					p.scroll = min(p.scroll+step, p.term.MaxScroll())
				case outcomeScrollForward:
					p.scroll = max(p.scroll-step, 0)
				default:
					p.scroll = 0
				}
				paint()

			case outcomeNext, outcomePrev:
				// In a split, next and previous change what the focused pane is showing rather
				// than ending the session. Returning would have closed every pane and started
				// again with one - the user's arrangement thrown away without a word, which is
				// what this did until somebody pressed n with two panes open and watched the
				// other one vanish.
				//
				// With a single pane there is nothing to preserve, so it goes back to the loop
				// outside, which knows how to replay and reattach.
				if len(panes) == 1 {
					return want, nil
				}
				next, err := neighbourService(socket, panes[focus].service, want == outcomeNext)
				if err != nil {
					note(err.Error())
					paint()
					continue
				}
				if err := swapPane(socket, panes[focus], next); err != nil {
					note(err.Error())
				} else {
					// The old connection's reader ended with the connection; the new one needs
					// its own. Without this the pane drew its replay and then never moved again.
					go readFrames(panes[focus], events)
				}
				paint()

			case outcomeGrow, outcomeShrink:
				if len(panes) == 1 {
					note("nothing to share the screen with")
					paint()
					continue
				}
				// A step of a fifth, which is a noticeable change without being a jump, and
				// bounded so a pane cannot be squeezed to nothing or grown until its neighbours
				// are. A pane of one column is not a pane.
				step := 0.2
				if want == outcomeShrink {
					step = -step
				}
				panes[focus].weight = min(max(panes[focus].weight+step, 0.2), float64(len(panes)))
				layoutPanes(panes, screen, how)
				resizePanes(panes)
				paint()

			case outcomeRedraw:
				// Everything again, from the grid rather than from the service: the grid is what
				// this client believes the screen to be, and a redraw is asking it to say so
				// again. If the grid itself is wrong, the service has to be asked - which is what
				// detaching and attaching again does, and what -no-render is for.
				screen.Forget()
				paint()

			case outcomeFocus:
				focus = (focus + 1) % len(panes)
				paint()

			case outcomeClosePane:
				if len(panes) == 1 {
					// Closing the only pane is detaching, and saying so is better than either
					// doing nothing or leaving an empty screen.
					return outcomeDetached, nil
				}
				panes[focus].client.Close()
				panes = append(panes[:focus], panes[focus+1:]...)
				focus = focus % len(panes)
				layoutPanes(panes, screen, how)
				resizePanes(panes)
				paint()

			default:
				return want, nil
			}
			remember()

		case <-ended:
			data, cmds, ended = nil, nil, nil

		case ev := <-events:
			// One event, then draw. Batching several before drawing was written here and then
			// removed: it changed neither the time nor the bytes written (see the measurement in
			// internal/vt/render), and an optimisation that cannot be shown to optimise anything
			// is a claim with code attached.
			applyEvent(socket, ev, events, note, screen.ColourAnswer)
			paintSoon()
			if allDone(panes) {
				// On the way out, whatever is owed is drawn: the last thing a service said - an
				// error, usually - must not be the one frame the rate limit swallowed.
				paint()
				if ev.finished {
					return outcomeFinished, nil
				}
				return outcomeDisconnected, ev.err
			}

		case <-due.C:
			owed = false
			paint()

		case <-winch:
			w, h, err := term.GetSize(int(in.Fd()))
			if err != nil || w <= 0 || h <= 0 {
				continue
			}
			if err := screen.Resize(w, h); err != nil {
				return outcomeDisconnected, err
			}
			layoutPanes(panes, screen, how)
			resizePanes(panes)
			paint()
		}
	}
}

// layoutPanes divides the screen into equal columns, one per pane, with a blank column between.
//
// Columns rather than rows because a terminal is wider than it is tall and a shell needs its
// width more than its height. Equal rather than adjustable because a pane you cannot resize is a
// limitation, and a resize handle nobody has built yet is a lie.
// stacked chooses between columns side by side and rows one above another.
//
// One orientation for the whole screen rather than a tree of splits. A tree is what a mature
// multiplexer has and it is a different piece of work - resizing, moving a pane between branches,
// a layout to save and restore. Two arrangements cover the case this is actually for: something
// alongside your shell, or something underneath it. Three panes in columns on an eighty-column
// terminal give twenty-six each, which is not a pane, it is a margin.
type stacked bool

const (
	inColumns stacked = false
	inRows    stacked = true
)

func layoutPanes(panes []*livePane, screen *renderedScreen, how stacked) {
	cols, rows := screen.ServiceSize()
	n := len(panes)
	if n == 0 {
		return
	}
	total := 0.0
	for _, p := range panes {
		if p.weight <= 0 {
			p.weight = 1
		}
		total += p.weight
	}
	if how == inRows {
		// No gap row between them: a screen is short and a blank line costs more of it than a
		// blank column costs of a width. The change of content is the seam.
		y := 0
		for i, p := range panes {
			h := max(int(float64(rows)*p.weight/total), 1)
			if i == n-1 {
				h = rows - y
			}
			p.rect = layout.Rect{Col: 0, Row: y, Cols: cols, Rows: max(h, 1)}
			y += h
		}
		return
	}
	// n-1 single-column gaps, so the panes do not run into each other with no visible seam.
	usable := cols - (n - 1)
	if usable < n {
		usable = n
	}
	x := 0
	for i, p := range panes {
		w := max(int(float64(usable)*p.weight/total), 1)
		if i == n-1 {
			// The last pane takes the remainder, so that a width that does not divide evenly
			// leaves no unused stripe down the right-hand side.
			w = cols - x
		}
		p.rect = layout.Rect{Col: x, Row: 0, Cols: w, Rows: rows}
		x += w + 1
	}
}

// resizePanes tells each pane's grid and each pane's service how big it now is.
func resizePanes(panes []*livePane) {
	for _, p := range panes {
		_ = p.term.Resize(p.rect.Cols, p.rect.Rows)
		_ = p.client.sendResize(p.rect.Cols, p.rect.Rows)
	}
}

func allDone(panes []*livePane) bool {
	for _, p := range panes {
		if !p.finished {
			return false
		}
	}
	return true
}

// openPane attaches to another service for a new pane.
func openPane(socket, service string, screen *renderedScreen, count int) (*livePane, error) {
	c, err := Dial(socket)
	if err != nil {
		return nil, err
	}
	cols, rows := screen.ServiceSize()
	// A first guess at the size; layoutPanes and resizePanes correct it immediately. Attaching at
	// the full width for an instant is better than attaching at zero, which some programs read as
	// "no terminal" and never redraw from.
	cols = max(cols/max(count, 1), 1)
	if _, err := c.Call(ipc.OpAttach, service, ipc.AttachRequest{Cols: cols, Rows: rows, Replay: true}); err != nil {
		c.Close()
		return nil, err
	}
	return &livePane{service: service, client: c, term: grid.New(cols, rows)}, nil
}

// applyEvent takes one thing a pane's connection said and does it, without drawing.
//
// Drawing is the caller's, once, after a whole batch: see the comment where these are gathered.
func applyEvent(socket string, ev paneEvent, events chan<- paneEvent, note func(string), colour func(int) (string, bool)) {
	if ev.message != "" {
		note(ev.message)
	}
	if len(ev.data) > 0 {
		_, _ = ev.pane.term.Write(ev.data)
		// Anything the terminal owes the program goes back to it as input. A byte pipe gets this
		// for free because the user's real terminal answers; here the emulator is the terminal,
		// and a program that asks where the cursor is and is never told waits for an answer that
		// is not coming.
		if replies := ev.pane.term.TakeReplies(); len(replies) > 0 {
			_ = ev.pane.client.Writer().WriteFrame(ipc.KindData, replies)
		}
		// The colour questions, answered from what the real terminal said when this attach
		// started. Answered here, in one place, rather than by telling every pane the colours as
		// it is opened: there are four places a pane is made and each of them would have had to
		// remember.
		for _, which := range ev.pane.term.TakeColourAsks() {
			value, ok := colour(which)
			if !ok {
				// The terminal never said. Not answering is the honest outcome - a made-up
				// colour is worse than the guess the program was already going to make - and the
				// user hears about it, once, like any other sequence that went nowhere.
				note(fmt.Sprintf("%s asked what colour this terminal is and it never said, so %s is guessing",
					ev.pane.service, ev.pane.service))
				continue
			}
			_ = ev.pane.client.Writer().WriteFrame(ipc.KindData,
				[]byte(fmt.Sprintf("\x1b]%d;%s\x1b\\", which, value)))
		}
		if ev.pane.scroll > 0 {
			// Output while somebody is reading back pushes the lines they are looking at further
			// into the scrollback, so the offset grows with it and the view stays still. Capped,
			// so a pane that scrolls faster than it has history does not walk off the top.
			ev.pane.scroll = min(ev.pane.scroll+countNewlines(ev.data), ev.pane.term.MaxScroll())
		}
	}
	// Anything the emulator did not implement is worth saying once. A byte pipe hands what it does
	// not understand to the terminal, which may well understand it; this drops it, and a user who
	// is never told has no way to connect an odd-looking screen to a sequence that went nowhere.
	for name := range ev.pane.term.Unknown() {
		if ev.pane.told[name] {
			continue
		}
		if ev.pane.told == nil {
			ev.pane.told = make(map[string]bool, 4)
		}
		ev.pane.told[name] = true
		note(fmt.Sprintf("%s used %s, which gozellij does not implement - the byte pipe would have "+
			"passed it to your terminal (gozellij attach -no-render %s)", ev.pane.service, name, ev.pane.service))
	}

	if ev.finished {
		ev.pane.finished = true
	}
	if ev.gone && !ev.pane.finished {
		// The connection went away without the service ending, which is what a daemon upgrade
		// looks like from here. Reconnect this pane where it stands rather than ending the
		// session: `gozellij upgrade` keeps every process running, and a split screen that has to
		// be rebuilt afterwards makes that promise worth less than it sounds.
		//
		// Before this, a pane whose connection ended simply stopped updating and nothing said so:
		// both halves of a split froze at the instant of the upgrade and stayed frozen, which
		// looked exactly like two idle shells.
		if err := reopenPane(socket, ev.pane); err != nil {
			note(fmt.Sprintf("%s: %v", ev.pane.service, err))
			ev.pane.finished = true
		} else {
			// Say so. The pane comes back working, but whatever the service printed while the
			// daemon was being replaced is not on this screen and never will be - the byte-pipe
			// path says exactly that on its way back in, and a rendered attach that reconnects
			// silently is the same loss with nothing to explain it.
			note(fmt.Sprintf("reconnected to %s after the daemon restarted; anything printed "+
				"meanwhile is in `gozellij logs %s`", ev.pane.service, ev.pane.service))
			go readFrames(ev.pane, events)
		}
	}
}

// swapPane points an existing pane at a different service, keeping its place on the screen.
//
// A fresh grid, because the old one holds another service's output and anything kept would be a
// screen the new service never drew. The replay is asked for: this is somebody choosing to look at
// a service, and starting from a blank pane would hide everything it has already printed.
func swapPane(socket string, p *livePane, service string) error {
	c, err := Dial(socket)
	if err != nil {
		return err
	}
	cols, rows := p.rect.Cols, p.rect.Rows
	if _, err := c.Call(ipc.OpAttach, service, ipc.AttachRequest{Cols: cols, Rows: rows, Replay: true}); err != nil {
		c.Close()
		return err
	}
	p.client.Close()
	p.client, p.service, p.scroll, p.finished = c, service, 0, false
	p.term = grid.New(cols, rows)
	return nil
}

// reopenPane reconnects a pane to its service after the connection went away.
//
// The grid is kept, not rebuilt. The screen this pane was showing is still the screen the service
// has, and asking for a replay would redraw a screenful of scrollback over it. What is asked for is
// the same size it already had, so the service never learns that anything happened.
func reopenPane(socket string, p *livePane) error {
	c, err := waitForDaemonClient(socket, ReattachWindow)
	if err != nil {
		return err
	}
	cols, rows := p.term.Size()
	if _, err := c.Call(ipc.OpAttach, p.service, ipc.AttachRequest{Cols: cols, Rows: rows, Replay: false}); err != nil {
		c.Close()
		return err
	}
	p.client.Close()
	p.client = c
	return nil
}

// nextUnshown finds the next service that is not already on the screen.
//
// Next in the same rotation Ctrl-] n walks, starting from the focused pane, rather than first in
// the list. Splitting and switching should agree about what "the next one" means: picking the
// alphabetically first unshown service instead meant that in a session with several services the
// split showed whichever one happened to sort first, which is not a thing anyone asked for and was
// caught by an acceptance check pairing the wrong two panes.
func nextUnshown(socket, from string, panes []*livePane) (string, error) {
	names, err := serviceNames(socket)
	if err != nil {
		return "", err
	}
	shown := make(map[string]bool, len(panes))
	for _, p := range panes {
		shown[p.service] = true
	}
	start := 0
	for i, n := range names {
		if n == from {
			start = i
			break
		}
	}
	for i := 1; i <= len(names); i++ {
		n := names[(start+i)%len(names)]
		if !shown[n] {
			return n, nil
		}
	}
	return "", errors.New("every service is already on the screen")
}

// readFrames turns one connection into events.
func readFrames(p *livePane, events chan<- paneEvent) {
	for {
		kind, payload, err := p.client.Reader().ReadFrame()
		if err != nil {
			// The connection ended. Whether that is the daemon being replaced or something worse
			// is not knowable from here, so it is reported as what it is and the session decides.
			events <- paneEvent{pane: p, gone: true, err: err}
			return
		}
		switch kind {
		case ipc.KindData:
			// Copied: the frame reader reuses its buffer, and a pane's grid would otherwise be
			// written from a slice that is about to hold somebody else's output.
			b := make([]byte, len(payload))
			copy(b, payload)
			events <- paneEvent{pane: p, data: b}
		case ipc.KindEvent:
			var ev ipc.Event
			if jsonUnmarshal(payload, &ev) == nil {
				events <- paneEvent{pane: p, finished: ev.Kind == ipc.EventFinished, message: ev.Message}
			}
		case ipc.KindResponse:
			var r ipc.Response
			if jsonUnmarshal(payload, &r) == nil && !r.OK {
				events <- paneEvent{pane: p, message: r.Error}
			}
		}
	}
}

// countNewlines is how many lines a chunk of output is likely to have pushed onto the screen.
//
// An approximation, and a deliberate one: what actually scrolls depends on the escape sequences in
// the chunk, and asking the emulator would mean asking it before and after every write. This keeps
// a reader's place roughly still while a service is chattering, which is what the reader wants; it
// does not claim to be exact, and the way to make it exact is for the grid to report how far it
// scrolled, which is a change to make when something needs it.
func countNewlines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}
