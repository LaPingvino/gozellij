package ipc

import (
	"encoding/json"
	"fmt"
	"time"
)

// Op names an operation. Strings rather than numbers: a socket dump stays readable, and an op a
// peer does not know can be reported by name instead of as "op 7".
type Op string

const (
	OpPing          Op = "ping"
	OpServiceAdd    Op = "service.add"
	OpServiceList   Op = "service.list"
	OpServiceStatus Op = "service.status"
	OpServiceStart  Op = "service.start"
	OpServiceStop   Op = "service.stop"
	OpServiceRestrt Op = "service.restart"
	OpServiceRemove Op = "service.remove"
	// OpAttach turns the connection into a stream: the daemon sends the service's output as
	// data frames and reads the client's keystrokes as data frames, until the client hangs up.
	OpAttach Op = "attach"
	// OpResize tells the daemon the attached client's terminal size.
	OpResize Op = "resize"
)

// Request is one call from a client.
type Request struct {
	// ID correlates a Response with its Request. A client may have several in flight, and a
	// response that cannot be matched to a request is worse than useless.
	ID uint64 `json:"id"`
	Op Op     `json:"op"`
	// Service is the service the operation is about, where it takes one.
	Service string `json:"service,omitempty"`
	// Payload is the operation's arguments, if any.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Response is the answer to exactly one Request.
//
// Every request gets one, including the ones that fail. A daemon that answers only when things go
// well leaves a client waiting on a timeout to discover a typo, which is precisely the failure this
// project keeps finding in other people's code.
type Response struct {
	ID uint64 `json:"id"`
	// OK is false when Error explains what went wrong.
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Payload is the result, if any.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Err builds a failed response. The error text is the whole point, so it is never empty: an
// unexplained failure is barely better than a silent one.
func Err(id uint64, err error) Response {
	msg := "unspecified error"
	if err != nil {
		msg = err.Error()
	}
	return Response{ID: id, OK: false, Error: msg}
}

// OKResponse builds a successful response carrying v, or a failed one if v cannot be encoded -
// because a marshalling bug must not be reported to the client as success.
func OKResponse(id uint64, v any) Response {
	if v == nil {
		return Response{ID: id, OK: true}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return Err(id, fmt.Errorf("encoding response: %w", err))
	}
	return Response{ID: id, OK: true, Payload: b}
}

// Decode unpacks a response payload.
func (r Response) Decode(v any) error {
	if len(r.Payload) == 0 {
		return fmt.Errorf("response %d has no payload", r.ID)
	}
	return json.Unmarshal(r.Payload, v)
}

// Event is something the daemon reports without being asked.
type Event struct {
	Kind    string    `json:"kind"`
	Service string    `json:"service,omitempty"`
	At      time.Time `json:"at"`
	// Message is human-readable and may be empty.
	Message string          `json:"message,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Event kinds.
const (
	EventStateChanged = "state-changed"
	// EventLagged tells an attached client that the daemon could not keep it fed and its view
	// is incomplete, so it should re-attach. Without this a client silently shows a screen that
	// is subtly wrong, which is the worst of the available options.
	EventLagged = "lagged"
	// EventProcessExited is sent when a service's process ends, so a viewer can say so rather
	// than leaving a dead pane looking merely quiet.
	EventProcessExited = "process-exited"
)

// AddRequest is the payload of OpServiceAdd.
type AddRequest struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	Env     []string `json:"env,omitempty"`
	Restart string   `json:"restart,omitempty"`
	Start   bool     `json:"start,omitempty"`
}

// AttachRequest is the payload of OpAttach.
type AttachRequest struct {
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
	// Replay asks for the retained output before the live stream, so an attaching client sees
	// what is already on screen rather than a blank pane until something moves.
	Replay bool `json:"replay"`
}

// ResizeRequest is the payload of OpResize.
type ResizeRequest struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// StatusReply describes one service. It mirrors fabric.Status but is its own type on purpose: the
// wire format is a contract with other versions of this program, and it must not change silently
// because an internal struct was refactored.
type StatusReply struct {
	Service     string    `json:"service"`
	State       string    `json:"state"`
	Pid         int       `json:"pid,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	Restarts    int       `json:"restarts,omitempty"`
	TotalStarts int       `json:"total_starts,omitempty"`
	ExitCode    int       `json:"exit_code,omitempty"`
	ExitSignal  string    `json:"exit_signal,omitempty"`
	HasExited   bool      `json:"has_exited,omitempty"`
	NextRestart time.Time `json:"next_restart,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Enabled     bool      `json:"enabled"`
	Command     string    `json:"command,omitempty"`
}

// ListReply is the payload of a successful OpServiceList.
type ListReply struct {
	Services []StatusReply `json:"services"`
	// Problems are definitions the daemon could not load. They travel with the list rather than
	// replacing it: a broken service file must not hide the nine working ones, and must not be
	// hidden by them either.
	Problems []string `json:"problems,omitempty"`
}
