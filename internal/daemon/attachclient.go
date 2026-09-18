package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LaPingvino/gozellij/internal/ipc"
	"golang.org/x/term"
)

// DetachKey is the key that lets go of a session without touching what is running in it.
//
// Ctrl-] , as telnet has used for decades. The distinction matters more than the choice: detaching
// must never be confused with stopping, because the entire point of this program is that the thing
// keeps running after you walk away.
const DetachKey = 0x1d

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
	first := true
	for {
		c, err := Dial(socket)
		if err != nil {
			if first {
				return err
			}
			return fmt.Errorf("lost the daemon and could not get back: %w", err)
		}

		detached, err := c.attachOnce(service, in, out, replay && first)
		c.Close()
		if err != nil {
			return err
		}
		if detached {
			return nil
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
	_, err := c.attachOnce(service, in, out, replay)
	return err
}

// attachOnce runs one attach session. It reports whether the user detached deliberately, which is
// the difference between "we are done" and "we were cut off".
func (c *Client) attachOnce(service string, in *os.File, out io.Writer, replay bool) (bool, error) {
	cols, rows := 0, 0
	restore := func() {}

	if term.IsTerminal(int(in.Fd())) {
		if w, h, err := term.GetSize(int(in.Fd())); err == nil {
			cols, rows = w, h
		}
		// Raw mode: keystrokes go to the far end untouched, including Ctrl-C, which belongs
		// to the program you are attached to and not to us.
		state, err := term.MakeRaw(int(in.Fd()))
		if err != nil {
			return false, fmt.Errorf("putting the terminal in raw mode: %w", err)
		}
		restore = func() { _ = term.Restore(int(in.Fd()), state) }
	}
	defer restore()

	resp, err := c.Call(ipc.OpAttach, service, ipc.AttachRequest{Cols: cols, Rows: rows, Replay: replay})
	if err != nil {
		return false, err
	}
	_ = resp

	// Forward window changes, so a full-screen program follows the terminal it is displayed in.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			w, h, err := term.GetSize(int(in.Fd()))
			if err != nil || w <= 0 || h <= 0 {
				continue
			}
			_ = c.sendResize(w, h)
		}
	}()

	// Keystrokes to the daemon, watching for the detach key.
	inputDone := make(chan error, 1)
	detached := make(chan struct{})
	go func() {
		inputDone <- c.pumpInput(in, detached)
	}()

	// Output from the daemon to the terminal, until the far end hangs up or we detach.
	finished, err := c.pumpOutput(out)

	if finished {
		// The service is over and said so. That is an ending, not a disconnection, so do not
		// go looking for the daemon: reattaching would put the terminal back into a stream
		// that has nothing left to send.
		c.Close()
		releaseInput(in, inputDone)
		restore()
		return true, nil
	}

	select {
	case <-detached:
		// Ours: a clean detach, not a failure.
		c.Close()
		releaseInput(in, inputDone)
		restore()
		fmt.Fprintf(os.Stderr, "\r\n[detached from %s; it keeps running]\r\n", service)
		return true, nil
	default:
	}

	c.Close()
	releaseInput(in, inputDone)
	return false, err
}

// inputReleaseGrace bounds how long we wait for the keystroke pump to notice it is finished.
const inputReleaseGrace = 500 * time.Millisecond

// releaseInput stops the keystroke pump and waits for it, without waiting forever.
//
// The pump is blocked in read(2) on the *terminal*, not on the socket, so closing the connection
// does not wake it - which is why waiting for it unconditionally hung the client on a real
// terminal the moment the service finished. (It did not hang in tests, because a test's stdin is
// not a tty and reaches EOF immediately. A bug that only appears on the thing the program is for
// is the kind worth a comment.)
//
// A deadline in the past unblocks a read the runtime can poll, which covers a tty. If it cannot -
// some descriptors are not pollable - we stop waiting rather than deadlock, and say here why that
// is safe: we are done with this attach either way, and the deadline is cleared so a reattach on
// the same terminal starts from a clean state.
func releaseInput(in *os.File, inputDone <-chan error) {
	_ = in.SetReadDeadline(time.Now())
	select {
	case <-inputDone:
	case <-time.After(inputReleaseGrace):
	}
	_ = in.SetReadDeadline(time.Time{})
}

// pumpInput copies keystrokes to the daemon until the detach key or end of input.
func (c *Client) pumpInput(in *os.File, detached chan struct{}) error {
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if i := indexByte(chunk, DetachKey); i >= 0 {
				// Send whatever came before the detach key, then stop. Dropping it would
				// silently eat the user's last keystrokes.
				if i > 0 {
					_ = c.Writer().WriteFrame(ipc.KindData, chunk[:i])
				}
				close(detached)
				return nil
			}
			if werr := c.Writer().WriteFrame(ipc.KindData, chunk); werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded) {
				// Deadline exceeded is releaseInput telling us to stop, not a failure.
				return nil
			}
			return err
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

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
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
