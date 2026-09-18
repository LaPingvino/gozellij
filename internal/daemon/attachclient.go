package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/LaPingvino/gozellij/internal/ipc"
	"golang.org/x/term"
)

// PrefixKey introduces a command to gozellij itself rather than to the service you are attached to.
//
// Ctrl-] , as telnet has used for decades. Everything else on the wire is the service's: the
// attach is a byte pipe, so exactly one key can be ours and it has to be one no program wants.
//
// It was a bare detach key until services became switchable. A prefix costs one extra keystroke to
// detach and buys every other command there will ever be, which is the trade tmux and zellij both
// made; `Ctrl-] Ctrl-]` sends a literal Ctrl-] through for the programs that do want it.
const PrefixKey = 0x1d

// DetachKey is the old name for PrefixKey, kept because it is referenced from docs and tests.
const DetachKey = PrefixKey

// attachOutcome is why an attach session ended. The difference between these is the difference
// between reconnecting, moving somewhere else, and going home.
type attachOutcome int

const (
	// outcomeDisconnected means the stream ended without us asking - a daemon upgrade, usually.
	outcomeDisconnected attachOutcome = iota
	// outcomeDetached means the user asked to leave.
	outcomeDetached
	// outcomeFinished means the service is over and said so.
	outcomeFinished
	// outcomeNext and outcomePrev mean the user asked for a different service in the same
	// terminal - tabs, without a terminal emulator anywhere in the picture.
	outcomeNext
	outcomePrev
)

// prefixHelp is what Ctrl-] ? prints. Short on purpose: it is displayed over whatever the service
// was showing.
const prefixHelp = "Ctrl-] d detach · n/p next/previous service · ? this · Ctrl-] sends a literal Ctrl-]"

// ReattachWindow is how long an attached client keeps trying to get back in after the stream ends
// unexpectedly.
//
// The case this exists for is a daemon upgrade: the exec takes the socket with it, so every attach
// drops even though nothing is wrong and the service never noticed. Without this, "upgrade the
// daemon without the processes noticing" is true for the processes and a lie for the person
// watching them, whose terminal just returns to a shell prompt with no explanation.
const ReattachWindow = 15 * time.Second

// AttachLoop connects the terminal to a service, and reconnects if the daemon goes away underneath
// it.
//
// It returns when the user detaches, when the service is gone, or when the daemon does not come
// back within ReattachWindow.
func AttachLoop(socket, service string, in *os.File, out io.Writer, replay bool) error {
	// Raw mode and the terminal reader belong to the loop, not to one session: keystrokes go to
	// the far end untouched (including Ctrl-C, which belongs to the program you are attached to
	// and not to us), and switching services must not hand the terminal back and forth.
	restore := func() {}
	if term.IsTerminal(int(in.Fd())) {
		state, err := term.MakeRaw(int(in.Fd()))
		if err != nil {
			return fmt.Errorf("putting the terminal in raw mode: %w", err)
		}
		restore = func() { _ = term.Restore(int(in.Fd()), state) }
	}
	defer restore()

	input := startTerminalInput(in)
	defer input.stop()

	first := true
	for {
		c, err := Dial(socket)
		if err != nil {
			if first {
				return err
			}
			return fmt.Errorf("lost the daemon and could not get back: %w", err)
		}

		outcome, err := c.runSession(service, input, in, out, replay && first)
		c.Close()
		if err != nil {
			return err
		}

		switch outcome {
		case outcomeDetached:
			restore()
			fmt.Fprintf(os.Stderr, "\r\n[detached from %s; it keeps running]\r\n", service)
			return nil
		case outcomeFinished:
			restore()
			return nil

		case outcomeNext, outcomePrev:
			// Tabs, the cheap way. Switching which service this terminal is showing needs no
			// terminal emulator at all: the attach is a byte pipe, so replaying the new
			// service's output repaints the screen because the escape sequences that drew it
			// are in the bytes. A multiplexer that owns a grid has to render this; here the
			// terminal does, exactly as it does on a first attach.
			next, nerr := neighbourService(socket, service, outcome == outcomeNext)
			if nerr != nil {
				fmt.Fprintf(os.Stderr, "\r\n[gozellij: %v]\r\n", nerr)
				// Staying put beats dropping the user at a shell prompt because a list
				// lookup failed.
				first, replay = false, false
				continue
			}
			service = next
			fmt.Fprint(out, "\x1b[H\x1b[2J")
			fmt.Fprintf(os.Stderr, "[gozellij: %s]\r\n", service)
			first, replay = true, true
			continue
		}

		// The stream ended without us asking. Either the daemon went away - an upgrade, most
		// likely - or the connection broke. Say so, then try to get back in.
		fmt.Fprintf(os.Stderr, "\r\n[gozellij: connection to the daemon ended; reattaching...]\r\n")

		back, werr := waitForDaemonClient(socket, ReattachWindow)
		if werr != nil {
			return fmt.Errorf("the daemon did not come back: %w", werr)
		}
		back.Close()

		// Do not replay on the way back in. The terminal already shows the history, and
		// repainting it would duplicate what is on screen; but output produced while we were
		// away is genuinely missing, and a client that cannot tell is exactly what this
		// project keeps refusing to ship.
		fmt.Fprintf(os.Stderr, "\r\n[gozellij: reattached; anything printed while the daemon "+
			"was restarting was not captured here - `gozellij logs %s` has it]\r\n", service)
		first = false
		replay = false
	}
}

// neighbourService is the service before or after this one, wrapping around.
//
// The list is fetched on a fresh connection because the attached one has been given over to the
// stream. Sorted by name, which is arbitrary but stable - and a switcher whose order changes
// between presses would be useless.
func neighbourService(socket, current string, forward bool) (string, error) {
	c, err := Dial(socket)
	if err != nil {
		return "", fmt.Errorf("cannot list services: %w", err)
	}
	defer c.Close()

	list, err := c.List()
	if err != nil {
		return "", fmt.Errorf("cannot list services: %w", err)
	}
	names := make([]string, 0, len(list.Services))
	for _, svc := range list.Services {
		names = append(names, svc.Service)
	}
	sort.Strings(names)

	if len(names) == 0 {
		return "", errors.New("there are no services to switch to")
	}
	if len(names) == 1 {
		return "", fmt.Errorf("%s is the only service", names[0])
	}

	at := -1
	for i, n := range names {
		if n == current {
			at = i
			break
		}
	}
	if at < 0 {
		// The service we are attached to is no longer listed - removed while we watched. Going
		// to the first one beats refusing to move.
		return names[0], nil
	}
	step := 1
	if !forward {
		step = -1
	}
	return names[(at+step+len(names))%len(names)], nil
}

// waitForDaemonClient polls until the daemon both accepts and answers.
func waitForDaemonClient(socket string, within time.Duration) (*Client, error) {
	deadline := time.Now().Add(within)
	var last error
	for time.Now().Before(deadline) {
		c, err := Dial(socket)
		if err == nil {
			if perr := c.Ping(); perr == nil {
				return c, nil
			} else {
				c.Close()
				last = perr
			}
		} else {
			last = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("gave up after %v (last error: %v)", within, last)
}

// Attach connects the local terminal to a service until the user detaches.
//
// in and out are normally os.Stdin and os.Stdout; they are parameters so this is testable without
// a controlling terminal.
func (c *Client) Attach(service string, in *os.File, out io.Writer, replay bool) error {
	input := startTerminalInput(in)
	defer input.stop()
	_, err := c.runSession(service, input, in, out, replay)
	return err
}

// terminalInput reads the terminal once, for the whole time the user is here.
//
// One reader, not one per session, and that is the point. A read on a tty cannot be cancelled -
// SetReadDeadline is refused on it, measured - so a session that started its own reader left it
// blocked in read(2) when it ended, and the next session started a second one beside it. Two
// goroutines reading the same terminal means the next keystroke goes to whichever wakes first, and
// half the time that is the one attached to a connection that is already closed. With one switch
// per session that was a rare lost keystroke after a daemon upgrade; with a key that switches
// services it would be every other press.
type terminalInput struct {
	// data carries bytes meant for whatever service is attached.
	data chan []byte
	// cmds carries the things the user asked gozellij itself for.
	cmds chan attachOutcome
	// ended fires when the terminal reaches EOF or fails.
	ended chan error

	done chan struct{}
	once sync.Once
}

// startTerminalInput begins reading the terminal.
func startTerminalInput(in *os.File) *terminalInput {
	t := &terminalInput{
		// Buffered so a burst read is not held up by a session that is mid-switch.
		data:  make(chan []byte, 64),
		cmds:  make(chan attachOutcome, 1),
		ended: make(chan error, 1),
		done:  make(chan struct{}),
	}
	go t.run(in)
	return t
}

// stop abandons the reader. It does not interrupt the read in progress - nothing can - but it does
// mean nothing is ever delivered again, and the goroutine ends on the next keystroke or at exit.
func (t *terminalInput) stop() { t.once.Do(func() { close(t.done) }) }

// run is the two-state machine: everything is forwarded until PrefixKey, and the byte after that
// is a command for us.
//
// Bytes are forwarded in runs rather than one at a time, because a paste is a single read of
// several thousand of them and a frame per byte would be visible.
func (t *terminalInput) run(in *os.File) {
	buf := make([]byte, 4096)
	var pending []byte
	prefixed := false

	flush := func() bool {
		if len(pending) == 0 {
			return true
		}
		chunk := make([]byte, len(pending))
		copy(chunk, pending)
		pending = pending[:0]
		select {
		case t.data <- chunk:
			return true
		case <-t.done:
			return false
		}
	}
	command := func(o attachOutcome) bool {
		if !flush() {
			return false
		}
		select {
		case t.cmds <- o:
			return true
		case <-t.done:
			return false
		}
	}

	for {
		n, err := in.Read(buf)
		for i := 0; i < n; i++ {
			b := buf[i]

			if prefixed {
				prefixed = false
				switch b {
				case PrefixKey:
					// A literal, for the programs that want this key themselves.
					pending = append(pending, b)
				case 'd', 'D':
					if !command(outcomeDetached) {
						return
					}
				case 'n', 'N', ' ':
					if !command(outcomeNext) {
						return
					}
				case 'p', 'P':
					if !command(outcomePrev) {
						return
					}
				case '?', 'h':
					if !flush() {
						return
					}
					fmt.Fprintf(os.Stderr, "\r\n[gozellij: %s]\r\n", prefixHelp)
				default:
					// Say what to do rather than swallowing it. A prefix key that silently
					// eats the next keystroke is indistinguishable from a dropped one.
					if !flush() {
						return
					}
					fmt.Fprintf(os.Stderr, "\r\n[gozellij: Ctrl-] %q does nothing. %s]\r\n", b, prefixHelp)
				}
				continue
			}

			if b == PrefixKey {
				prefixed = true
				continue
			}
			pending = append(pending, b)
		}

		if !flush() {
			return
		}
		if err != nil {
			select {
			case t.ended <- err:
			case <-t.done:
			}
			return
		}
	}
}

// runSession runs one attach and reports why it ended, which is the difference between going home,
// moving to another service, and being cut off.
//
// Everything happens in one select rather than in goroutines writing to shared variables: the
// terminal's bytes, the user's commands, the service's output ending, and a window resize are four
// things that can happen next, and a loop that names all four is easier to be sure about than
// three goroutines and a mutex.
func (c *Client) runSession(service string, input *terminalInput, in *os.File, out io.Writer, replay bool) (attachOutcome, error) {
	cols, rows := 0, 0
	if term.IsTerminal(int(in.Fd())) {
		if w, h, err := term.GetSize(int(in.Fd())); err == nil {
			cols, rows = w, h
		}
	}

	if _, err := c.Call(ipc.OpAttach, service, ipc.AttachRequest{Cols: cols, Rows: rows, Replay: replay}); err != nil {
		return outcomeDisconnected, err
	}

	// Forward window changes, so a full-screen program follows the terminal it is displayed in.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	type outputResult struct {
		finished bool
		err      error
	}
	output := make(chan outputResult, 1)
	go func() {
		f, e := c.pumpOutput(out)
		output <- outputResult{f, e}
	}()

	// Local copies, so that a terminal which reaches EOF can be dropped out of the select
	// without ending the session. Redirected input runs out; the service's output still
	// matters, and a viewer that quit the moment its stdin closed would be useless for
	// `gozellij attach web </dev/null` and for every test that does the same.
	data, cmds, ended := input.data, input.cmds, input.ended

	for {
		select {
		case chunk := <-data:
			if err := c.Writer().WriteFrame(ipc.KindData, chunk); err != nil {
				// The connection is gone; let the output pump report why.
				c.Close()
				got := <-output
				return outcomeDisconnected, got.err
			}

		case want := <-cmds:
			// Closing is what ends the output pump: it is blocked on a read from the daemon,
			// which has no reason to say anything just because the user pressed a key.
			c.Close()
			<-output
			return want, nil

		case <-ended:
			// The terminal is gone, so nothing more will be typed - but plenty may still be
			// printed. Stop listening to it and carry on watching.
			data, cmds, ended = nil, nil, nil

		case got := <-output:
			if got.finished {
				return outcomeFinished, nil
			}
			return outcomeDisconnected, got.err

		case <-winch:
			w, h, err := term.GetSize(int(in.Fd()))
			if err != nil || w <= 0 || h <= 0 {
				continue
			}
			_ = c.sendResize(w, h)
		}
	}
}

// pumpOutput writes the service's output to the terminal and surfaces events.
//
// It reports whether the daemon said the service had finished, which is the difference between an
// ending and a disconnection - and therefore between letting go and trying to reattach.
func (c *Client) pumpOutput(out io.Writer) (bool, error) {
	finished := false
	for {
		kind, payload, err := c.Reader().ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return finished, nil
			}
			return finished, err
		}
		switch kind {
		case ipc.KindData:
			if _, werr := out.Write(payload); werr != nil {
				return finished, werr
			}
		case ipc.KindEvent:
			// Events are shown, not swallowed. "lagged" in particular means the screen is
			// now wrong, and the user needs to know that rather than wonder later.
			var ev ipc.Event
			if jerr := jsonUnmarshal(payload, &ev); jerr == nil {
				if ev.Kind == ipc.EventFinished {
					finished = true
				}
				if ev.Message != "" {
					fmt.Fprintf(os.Stderr, "\r\n[gozellij: %s]\r\n", ev.Message)
				}
			}
		case ipc.KindResponse:
			// An in-stream answer (to a resize, say). Nothing to display.
		default:
			fmt.Fprintf(os.Stderr, "\r\n[gozellij: unexpected %s frame]\r\n", kind)
		}
	}
}

func (c *Client) sendResize(cols, rows int) error {
	req := ipc.Request{Op: ipc.OpResize, Payload: mustJSON(ipc.ResizeRequest{Cols: cols, Rows: rows})}
	return c.Writer().WriteJSON(ipc.KindRequest, req)
}

// jsonUnmarshal and mustJSON keep the pumps readable; encoding failures on these tiny structs
// would be a programming error, not something a user can cause.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Unreachable for the fixed-shape structs used here; panicking beats sending a frame
		// that says nothing.
		panic(fmt.Sprintf("gozellij: encoding %T: %v", v, err))
	}
	return b
}
