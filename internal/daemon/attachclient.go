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

	"github.com/LaPingvino/gozellij/internal/ipc"
	"golang.org/x/term"
)

// DetachKey is the key that lets go of a session without touching what is running in it.
//
// Ctrl-] , as telnet has used for decades. The distinction matters more than the choice: detaching
// must never be confused with stopping, because the entire point of this program is that the thing
// keeps running after you walk away.
const DetachKey = 0x1d

// Attach connects the local terminal to a service until the user detaches.
//
// in and out are normally os.Stdin and os.Stdout; they are parameters so this is testable without
// a controlling terminal.
func (c *Client) Attach(service string, in *os.File, out io.Writer, replay bool) error {
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
			return fmt.Errorf("putting the terminal in raw mode: %w", err)
		}
		restore = func() { _ = term.Restore(int(in.Fd()), state) }
	}
	defer restore()

	resp, err := c.Call(ipc.OpAttach, service, ipc.AttachRequest{Cols: cols, Rows: rows, Replay: replay})
	if err != nil {
		return err
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
	err = c.pumpOutput(out)

	select {
	case <-detached:
		// Ours: a clean detach, not a failure.
		c.Close()
		<-inputDone
		restore()
		fmt.Fprintf(os.Stderr, "\r\n[detached from %s; it keeps running]\r\n", service)
		return nil
	default:
	}

	c.Close()
	<-inputDone
	return err
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
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// pumpOutput writes the service's output to the terminal and surfaces events.
func (c *Client) pumpOutput(out io.Writer) error {
	for {
		kind, payload, err := c.Reader().ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		switch kind {
		case ipc.KindData:
			if _, werr := out.Write(payload); werr != nil {
				return werr
			}
		case ipc.KindEvent:
			// Events are shown, not swallowed. "lagged" in particular means the screen is
			// now wrong, and the user needs to know that rather than wonder later.
			var ev ipc.Event
			if jerr := jsonUnmarshal(payload, &ev); jerr == nil && ev.Message != "" {
				fmt.Fprintf(os.Stderr, "\r\n[gozellij: %s]\r\n", ev.Message)
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
