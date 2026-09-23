package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// DialTimeout bounds how long a client waits to reach the daemon.
//
// Generous on purpose. Design rule 2: a short timeout is a bet that the machine is idle, and the
// machine this is written for runs at load 20. A connect that takes two seconds is mildly annoying;
// one that fails spuriously sends someone hunting for a daemon that was fine all along.
const DialTimeout = 10 * time.Second

// CallTimeout bounds how long a client waits for a response to a request.
const CallTimeout = 30 * time.Second

// Client talks to a daemon.
type Client struct {
	conn net.Conn
	r    *ipc.Reader
	w    *ipc.Writer

	nextID atomic.Uint64

	// mu serialises request/response pairs. The protocol carries ids so it could multiplex,
	// but nothing needs that yet and a single in-flight request is far easier to reason about.
	mu sync.Mutex

	// readOnly is carried on the connection rather than passed to each session, because that is
	// what it is: a property of this attachment, not of one screenful of it. A rendered attach
	// opens a connection per pane and each of them inherits it.
	readOnly bool
}

// ReadOnly reports whether this connection watches without touching.
func (c *Client) ReadOnly() bool { return c.readOnly }

// SetReadOnly marks this connection as a watcher. Before attaching: the daemon is told at attach
// time and is the thing that actually enforces it.
func (c *Client) SetReadOnly(ro bool) { c.readOnly = ro }

// ErrNoDaemon is returned when nothing is listening on the socket.
var ErrNoDaemon = errors.New("no gozellij daemon is running")

// Dial connects to a daemon.
func Dial(path string) (*Client, error) {
	conn, err := net.DialTimeout("unix", path, DialTimeout)
	if err != nil {
		// Translate the two cases a user actually hits - no socket file, or a socket nobody is
		// listening on - into something actionable, rather than passing a syscall name up.
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w (socket %s). Start one with `gozellijd`", ErrNoDaemon, path)
		}
		return nil, fmt.Errorf("connecting to %s: %w", path, err)
	}
	return &Client{
		conn: conn,
		r:    ipc.NewReader(conn),
		w:    ipc.NewWriter(conn),
	}, nil
}

// Close hangs up.
func (c *Client) Close() error { return c.conn.Close() }

// Conn exposes the underlying connection, for attach, which takes the stream over.
func (c *Client) Conn() net.Conn { return c.conn }

// Reader and Writer expose the framing, for attach.
func (c *Client) Reader() *ipc.Reader { return c.r }
func (c *Client) Writer() *ipc.Writer { return c.w }

// Call sends a request and returns the response.
//
// A failed response is returned as an error carrying the daemon's own words. Losing that text and
// replacing it with something generic is how a precise complaint from the far end turns into "the
// operation failed".
func (c *Client) Call(op ipc.Op, service string, payload any) (ipc.Response, error) {
	return c.callWithin(CallTimeout, op, service, payload)
}

// callWithin is Call with a deadline the caller chooses.
//
// It exists because a caller that set its own deadline on the connection and then used Call had it
// silently replaced: Call sets CallTimeout unconditionally, so a "bounded" query was bounded at
// thirty seconds rather than the one and a half the caller asked for and believed it had. A
// timeout that is quietly overridden is worse than no timeout, because it is written down.
func (c *Client) callWithin(within time.Duration, op ipc.Op, service string, payload any) (ipc.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	req := ipc.Request{ID: c.nextID.Add(1), Op: op, Service: service}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return ipc.Response{}, fmt.Errorf("encoding %s payload: %w", op, err)
		}
		req.Payload = b
	}

	if err := c.conn.SetDeadline(time.Now().Add(within)); err != nil {
		return ipc.Response{}, fmt.Errorf("setting deadline: %w", err)
	}
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()

	if err := c.w.WriteJSON(ipc.KindRequest, req); err != nil {
		return ipc.Response{}, fmt.Errorf("sending %s: %w", op, err)
	}

	var resp ipc.Response
	if err := c.r.ReadJSON(ipc.KindResponse, &resp); err != nil {
		return ipc.Response{}, fmt.Errorf("waiting for the answer to %s: %w", op, err)
	}
	if resp.ID != req.ID {
		// Should be impossible with one request in flight, and worth shouting about if it
		// happens: it means the stream is out of step and nothing after this is trustworthy.
		return resp, fmt.Errorf("response id %d does not match request id %d: the connection is out of step",
			resp.ID, req.ID)
	}
	if !resp.OK {
		return resp, fmt.Errorf("%s: %s", op, resp.Error)
	}
	return resp, nil
}

// Ping checks the daemon is answering.
func (c *Client) Ping() error {
	_, err := c.Call(ipc.OpPing, "", nil)
	return err
}

// List returns every service the daemon knows about.
func (c *Client) List() (ipc.ListReply, error) {
	resp, err := c.Call(ipc.OpServiceList, "", nil)
	if err != nil {
		return ipc.ListReply{}, err
	}
	var out ipc.ListReply
	if err := resp.Decode(&out); err != nil {
		return ipc.ListReply{}, fmt.Errorf("decoding the service list: %w", err)
	}
	return out, nil
}

// ListWithin returns every service, giving up after within.
//
// For the status line, which is a bystander: it runs on a goroutine that detaching waits for, and
// a daemon that has stopped answering must not turn letting go of a terminal into a wait.
func (c *Client) ListWithin(within time.Duration) (ipc.ListReply, error) {
	resp, err := c.callWithin(within, ipc.OpServiceList, "", nil)
	if err != nil {
		return ipc.ListReply{}, err
	}
	var out ipc.ListReply
	if err := resp.Decode(&out); err != nil {
		return ipc.ListReply{}, fmt.Errorf("decoding the service list: %w", err)
	}
	return out, nil
}

// Status returns one service's status.
func (c *Client) Status(name string) (ipc.StatusReply, error) {
	return c.callStatus(ipc.OpServiceStatus, name, nil)
}

// Add defines a service.
func (c *Client) Add(name string, req ipc.AddRequest) (ipc.StatusReply, error) {
	return c.callStatus(ipc.OpServiceAdd, name, req)
}

// Ensure makes a service exist and run, leaving a running one untouched. Safe to call from two
// terminals at the same moment, which Add and Remove from the client were not.
func (c *Client) Ensure(name string, req ipc.AddRequest) (ipc.StatusReply, error) {
	return c.callStatus(ipc.OpServiceEnsure, name, req)
}

// Start, Stop and Restart do what they say and return the resulting status.
func (c *Client) Start(name string) (ipc.StatusReply, error) {
	return c.callStatus(ipc.OpServiceStart, name, nil)
}

func (c *Client) Stop(name string) (ipc.StatusReply, error) {
	return c.callStatus(ipc.OpServiceStop, name, nil)
}

func (c *Client) Restart(name string) (ipc.StatusReply, error) {
	return c.callStatus(ipc.OpServiceRestrt, name, nil)
}

// Logs returns the retained output of a service.
func (c *Client) Logs(name string, maxBytes int) (ipc.LogsReply, error) {
	resp, err := c.Call(ipc.OpServiceLogs, name, ipc.LogsRequest{MaxBytes: maxBytes})
	if err != nil {
		return ipc.LogsReply{}, err
	}
	var out ipc.LogsReply
	if err := resp.Decode(&out); err != nil {
		return ipc.LogsReply{}, fmt.Errorf("decoding the logs of %s: %w", name, err)
	}
	return out, nil
}

// FollowLogs streams a service's output to out until the connection ends or the caller's process
// is interrupted. Notices from the daemon - such as "you fell behind" - go to notices.
//
// The connection is used up by this call: after it the client is only good for closing. That is
// the same bargain as Attach, and for the same reason - the stream takes the socket over.
func (c *Client) FollowLogs(name string, maxBytes int, out, notices io.Writer) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	req := ipc.Request{ID: c.nextID.Add(1), Op: ipc.OpServiceLogs, Service: name}
	b, err := json.Marshal(ipc.LogsRequest{MaxBytes: maxBytes, Follow: true})
	if err != nil {
		return fmt.Errorf("encoding the logs request: %w", err)
	}
	req.Payload = b

	// A deadline on the handshake only. Following has no timeout by design: a quiet service is
	// the normal case, and a `logs -f` that gave up after thirty seconds of silence would be
	// reporting on its own impatience rather than on the service.
	if err := c.conn.SetDeadline(time.Now().Add(CallTimeout)); err != nil {
		return fmt.Errorf("setting deadline: %w", err)
	}
	if err := c.w.WriteJSON(ipc.KindRequest, req); err != nil {
		return fmt.Errorf("asking to follow %s: %w", name, err)
	}
	var resp ipc.Response
	if err := c.r.ReadJSON(ipc.KindResponse, &resp); err != nil {
		return fmt.Errorf("waiting for the answer to follow %s: %w", name, err)
	}
	if err := c.conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clearing deadline: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("following %s: %s", name, resp.Error)
	}

	for {
		kind, payload, err := c.r.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				// The daemon went away, or we were closed from another goroutine. Either is
				// the ordinary end of a follow, not a failure.
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
			var ev ipc.Event
			if json.Unmarshal(payload, &ev) == nil && notices != nil {
				fmt.Fprintf(notices, "[gozellij: %s]\n", ev.Message)
			}
		default:
			// A response frame here means the daemon is complaining about something we sent,
			// which a follower does not do. Say so rather than ignoring it.
			var r ipc.Response
			if json.Unmarshal(payload, &r) == nil && !r.OK && notices != nil {
				fmt.Fprintf(notices, "[gozellij: %s]\n", r.Error)
			}
		}
	}
}

// Upgrade asks the daemon to replace its binary in place. The connection dies immediately
// afterwards, by design - the daemon execs and takes the socket with it.
func (c *Client) Upgrade() (ipc.UpgradeReply, error) {
	resp, err := c.Call(ipc.OpUpgrade, "", nil)
	if err != nil {
		return ipc.UpgradeReply{}, err
	}
	var out ipc.UpgradeReply
	if err := resp.Decode(&out); err != nil {
		return ipc.UpgradeReply{}, fmt.Errorf("decoding the upgrade reply: %w", err)
	}
	return out, nil
}

// PingVersion returns the daemon's version string.
func (c *Client) PingVersion() (string, error) {
	info, err := c.Info()
	if err != nil {
		return "", err
	}
	return info["version"], nil
}

// Info returns what the daemon says about itself: its version, and how completely it can stop a
// service. Only the daemon can answer the second one, since it depends on how it was started.
func (c *Client) Info() (map[string]string, error) {
	resp, err := c.Call(ipc.OpPing, "", nil)
	if err != nil {
		return nil, err
	}
	var out map[string]string
	if err := resp.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Rename gives a service a new name.
func (c *Client) Rename(from, to string) error {
	_, err := c.Call(ipc.OpServiceRename, from, ipc.RenameRequest{To: to})
	return err
}

// Remove deletes a service and, unless keepLogs, its log. It returns the log files it deleted.
func (c *Client) Remove(name string, keepLogs bool) ([]string, error) {
	resp, err := c.Call(ipc.OpServiceRemove, name, ipc.RemoveRequest{KeepLogs: keepLogs})
	if err != nil {
		return nil, err
	}
	var out ipc.RemoveReply
	if len(resp.Payload) > 0 {
		if derr := resp.Decode(&out); derr != nil {
			// The service is gone either way, so this is not a failure - but saying nothing
			// would leave the caller reporting a removal with no mention of the log, which is
			// indistinguishable from there having been no log.
			return nil, fmt.Errorf("removed %s, but could not read what else went with it: %w", name, derr)
		}
	}
	return out.Logs, nil
}

func (c *Client) callStatus(op ipc.Op, name string, payload any) (ipc.StatusReply, error) {
	resp, err := c.Call(op, name, payload)
	if err != nil {
		return ipc.StatusReply{}, err
	}
	var out ipc.StatusReply
	if err := resp.Decode(&out); err != nil {
		return ipc.StatusReply{}, fmt.Errorf("decoding the status of %s: %w", name, err)
	}
	return out, nil
}
