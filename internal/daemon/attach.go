package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/LaPingvino/gozellij/internal/fabric"
	"github.com/LaPingvino/gozellij/internal/ipc"
)

// AttachQueueBytes is how far behind an attached client may fall before it is told its view is
// incomplete.
//
// Generous: a client on a slow link should be allowed to lag through a burst of output and catch
// up, because declaring it lagged forces a full resynchronisation. Small enough that a client that
// has genuinely stopped reading does not make the daemon hold the whole history for it.
const AttachQueueBytes = 4 << 20 // 4 MiB

// attachSession is one client attached to one service.
type attachSession struct {
	srv     *Server
	conn    net.Conn
	w       *ipc.Writer
	r       *ipc.Reader
	service string
}

// attach turns this connection into a stream until the client hangs up.
//
// Phase 1 has no terminal emulator: what the child wrote goes to the client verbatim and the
// client's keystrokes come back, so the *client's* terminal does the emulation. See DESIGN.md.
func (s *Server) attach(conn net.Conn, r *ipc.Reader, w *ipc.Writer, req ipc.Request) error {
	var ar ipc.AttachRequest
	if len(req.Payload) > 0 {
		if err := json.Unmarshal(req.Payload, &ar); err != nil {
			return fmt.Errorf("malformed attach payload: %w", err)
		}
	}

	out, err := s.fab.Output(req.Service)
	if err != nil {
		return err
	}

	// Size the pty to the attaching client before replaying anything, so a full-screen program
	// repaints at the right size rather than at whatever the last client used.
	if ar.Cols > 0 && ar.Rows > 0 {
		if p, perr := s.fab.Process(req.Service); perr == nil && p != nil {
			if rerr := p.Resize(ar.Cols, ar.Rows); rerr != nil && !errors.Is(rerr, fabric.ErrProcessGone) {
				s.log.Debug("resize on attach failed", "service", req.Service, "err", rerr)
			}
		}
	}

	// Counted from here: after the lookup, so an attach refused for an unknown service is not a
	// viewer, and *before* the subscription, so that a viewer is counted for the whole time one
	// can exist. Counting after would leave a window in which a handler holds a subscription
	// nobody is counted for, and "no viewers" would not mean "nothing attached" - which it has
	// to, because that is the only signal anything else has for when the attaches are done.
	leaving := s.watching(req.Service)
	defer leaving()

	// Snapshot and subscription are taken together, so nothing written in between is lost. See
	// the note on OutputBuffer.Attach.
	snapshot, sub, err := out.Attach(AttachQueueBytes)
	if err != nil {
		return err
	}

	// Detach is idempotent, so this defer covers the early returns below - a client that hangs
	// up between the request and the reply left a subscriber on the buffer for the life of the
	// daemon, each one getting a private copy of every byte the service wrote. The explicit
	// Detach further down still has to be where it is, for the ordering reason described there.
	//
	// Registered after the viewer count so that it runs before it: while a viewer is counted its
	// subscription exists, and once the count is zero none do.
	defer sub.Detach()

	sess := &attachSession{srv: s, conn: conn, w: w, r: r, service: req.Service}

	// The attach itself succeeded: say so before the stream starts, so the client can tell
	// "attached, nothing has happened yet" from "still waiting to be let in".
	if err := w.WriteJSON(ipc.KindResponse, ipc.OKResponse(req.ID, nil)); err != nil {
		return err
	}

	if ar.Replay && len(snapshot) > 0 {
		if err := sess.writeData(snapshot); err != nil {
			return err
		}
	}

	// A third goroutine watches for the service finishing. Without it, `exit` in an attached
	// shell left the terminal in an attach that would never end: the pump is ranging over a
	// buffer nobody closes, and readInput only notices a dead process when you type at it. The
	// first thing anybody does with a login shell is exit it.
	watchDone := make(chan struct{})
	stopWatching := sess.watchForExit(req.Service, sub, watchDone)

	// One goroutine pumps output to the client; this one reads the client's input. They end
	// together: whichever notices the connection is gone closes it, and the other unblocks.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Closing the connection when the output ends is what releases readInput, which is
		// otherwise blocked on a read nothing will ever satisfy. Without it, anything that
		// ends the stream from the far side - the service finishing, or `gozellij rm` closing
		// the buffer out from under an attached client - left the client hanging.
		defer conn.Close()
		sess.pumpOutput(sub)
	}()

	err = sess.readInput()

	// Order matters here, and getting it wrong deadlocks. Detach must come *before* waiting for
	// the pump: the pump ranges over the subscriber's channel, and only Detach closes it. With
	// a `defer sub.Detach()` instead, this function waits for a goroutine that is waiting for
	// the thing this function will do after it returns.
	conn.Close() // unblock the pump if it is mid-write
	sub.Detach() // close the channel the pump is ranging over
	<-done

	// The watcher last, and only once the pump is finished: it may itself be detaching the
	// subscriber, and stopping it first would close the channel it is selecting on.
	stopWatching()
	<-watchDone
	return err
}

// watchForExit ends the attach when the service has finished for good.
//
// Backing off is deliberately not finished: the supervisor has already written the restart notice
// into the output the client is watching, and a `restart always` service should carry its viewer
// across the gap rather than dropping them at a shell prompt. What ends the attach is a service
// that has exited and is not coming back.
//
// Ending the attach is done by detaching the subscriber rather than by closing the connection.
// That difference is a line of output: a closed connection cuts off whatever the pump still has
// queued, which for a shell is the last thing it printed before you typed exit. Closing the
// subscriber's channel lets the pump drain what is already in it and then finish, and the pump
// closes the connection on its way out.
//
// Returns a function that stops the watch; it must be called, and the done channel waited on,
// before the attach returns.
func (a *attachSession) watchForExit(service string, sub *fabric.Subscriber, done chan struct{}) func() {
	changed, stop, err := a.srv.fab.Watch(service)
	if err != nil {
		// The service went away between the lookup above and here. Nothing to watch, and the
		// attach will end on its own when the buffer closes.
		close(done)
		return func() {}
	}

	finished := func() bool {
		st, serr := a.srv.fab.Status(service)
		return serr != nil || st.Finished()
	}

	go func() {
		defer close(done)
		// Check before waiting: attaching to a service that has already finished must not wait
		// for a change that has already happened.
		for !finished() {
			if _, open := <-changed; !open {
				return
			}
		}
		st, _ := a.srv.fab.Status(service)
		a.notify(ipc.EventFinished, exitWords(service, st))
		sub.Detach()
	}()

	return stop
}

// exitWords says how a service ended in a way a person can act on.
//
// The name comes from the caller rather than from the status, because the status may be a zero
// value: the service can be removed out from under an attached client, and "[gozellij:  exited]"
// with a blank name is not an improvement on saying nothing.
func exitWords(service string, st fabric.Status) string {
	switch {
	case st.State == fabric.StateStopped && st.HasExited:
		// Stopped is something an operator did. Reporting the SIGTERM we sent as though the
		// service had been killed by something is technically true and completely misleading.
		return service + " was stopped"
	case st.LastError != "" && !st.HasExited:
		// It never ran at all - a missing binary, most often. The error is the whole message.
		return fmt.Sprintf("%s could not start: %s", service, st.LastError)
	case st.LastExit.Signal != "":
		return fmt.Sprintf("%s was killed by %s and is not restarting", service, st.LastExit.Signal)
	case st.LastExit.Code == 0:
		return service + " exited"
	default:
		return fmt.Sprintf("%s exited with code %d and is not restarting", service, st.LastExit.Code)
	}
}

// writeData sends output to the client in frames that fit.
func (a *attachSession) writeData(b []byte) error {
	const chunk = ipc.MaxFrameSize / 2
	for len(b) > 0 {
		n := len(b)
		if n > chunk {
			n = chunk
		}
		if err := a.w.WriteFrame(ipc.KindData, b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// pumpOutput forwards the service's output until the subscription ends.
func (a *attachSession) pumpOutput(sub *fabric.Subscriber) {
	defer func() {
		// The subscription can also end *because* this client fell behind - the buffer closes
		// the channel to wake a reader that would otherwise block forever. Check on the way
		// out, so a client that lagged at exactly the wrong moment is still told why its
		// stream stopped instead of watching it end for no stated reason.
		if sub.Lagged() {
			_ = a.w.WriteJSON(ipc.KindEvent, ipc.Event{
				Kind:    ipc.EventLagged,
				Service: a.service,
				At:      time.Now(),
				Message: "output was dropped because this client could not keep up; re-attach to resynchronise",
			})
		}
	}()

	for chunk := range sub.C() {
		if err := a.writeData(chunk); err != nil {
			return
		}
		sub.Consumed(len(chunk))

		if sub.Lagged() {
			// Tell the client its view is incomplete rather than letting it display
			// something subtly wrong. It can re-attach to resynchronise; a client that
			// cannot tell it missed data is worse off than one that is told.
			_ = a.w.WriteJSON(ipc.KindEvent, ipc.Event{
				Kind:    ipc.EventLagged,
				Service: a.service,
				At:      time.Now(),
				Message: "output was dropped because this client could not keep up; re-attach to resynchronise",
			})
			return
		}
	}
}

// readInput feeds the client's keystrokes to the process and handles in-stream requests.
func (a *attachSession) readInput() error {
	for {
		kind, payload, err := a.r.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}

		switch kind {
		case ipc.KindData:
			p, perr := a.srv.fab.Process(a.service)
			if perr != nil {
				return perr
			}
			if p == nil {
				// Typing at a service that is not running is not an error worth closing
				// the connection over, but it must not look like it worked either.
				a.notify(ipc.EventProcessExited, "input ignored: the service is not running")
				continue
			}
			if _, werr := p.Write(payload); werr != nil {
				if errors.Is(werr, fabric.ErrProcessGone) {
					a.notify(ipc.EventProcessExited, "input ignored: the process has exited")
					continue
				}
				return werr
			}

		case ipc.KindRequest:
			var req ipc.Request
			if jerr := json.Unmarshal(payload, &req); jerr != nil {
				_ = a.w.WriteJSON(ipc.KindResponse, ipc.Err(0, fmt.Errorf("malformed request during attach: %w", jerr)))
				continue
			}
			a.handleInStream(req)

		default:
			// Say so rather than dropping it. A client sending responses at us is confused,
			// and silence would leave it that way.
			_ = a.w.WriteJSON(ipc.KindResponse, ipc.Err(0,
				fmt.Errorf("unexpected %s frame while attached", kind)))
		}
	}
}

// readUntilHangup waits for a one-way follower to go away.
//
// A follower has no keyboard: `logs -f` is not an attach, and feeding what it sends to the process
// would let a command documented as read-only type into a shell. Anything arriving here is a
// client bug, and it is named rather than swallowed.
func (a *attachSession) readUntilHangup() error {
	for {
		kind, _, err := a.r.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		_ = a.w.WriteJSON(ipc.KindResponse, ipc.Err(0,
			fmt.Errorf("unexpected %s frame: this connection is following logs, which is one-way "+
				"(use attach to send input)", kind)))
	}
}

// handleInStream serves the small set of requests that make sense mid-attach.
func (a *attachSession) handleInStream(req ipc.Request) {
	switch req.Op {
	case ipc.OpResize:
		var rr ipc.ResizeRequest
		if err := json.Unmarshal(req.Payload, &rr); err != nil {
			_ = a.w.WriteJSON(ipc.KindResponse, ipc.Err(req.ID, fmt.Errorf("malformed resize: %w", err)))
			return
		}
		p, err := a.srv.fab.Process(a.service)
		if err != nil {
			_ = a.w.WriteJSON(ipc.KindResponse, ipc.Err(req.ID, err))
			return
		}
		if p == nil {
			// Not an error: the size is remembered by the next attach. But answer, because
			// the client asked.
			_ = a.w.WriteJSON(ipc.KindResponse, ipc.OKResponse(req.ID, nil))
			return
		}
		if err := p.Resize(rr.Cols, rr.Rows); err != nil && !errors.Is(err, fabric.ErrProcessGone) {
			_ = a.w.WriteJSON(ipc.KindResponse, ipc.Err(req.ID, err))
			return
		}
		_ = a.w.WriteJSON(ipc.KindResponse, ipc.OKResponse(req.ID, nil))

	default:
		_ = a.w.WriteJSON(ipc.KindResponse, ipc.Err(req.ID,
			fmt.Errorf("%q cannot be used while attached (detach first)", req.Op)))
	}
}

func (a *attachSession) notify(kind, msg string) {
	_ = a.w.WriteJSON(ipc.KindEvent, ipc.Event{
		Kind:    kind,
		Service: a.service,
		At:      time.Now(),
		Message: msg,
	})
}
