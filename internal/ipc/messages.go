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
	OpServiceRename Op = "service.rename"
	// OpAttach turns the connection into a stream: the daemon sends the service's output as
	// data frames and reads the client's keystrokes as data frames, until the client hangs up.
	OpAttach Op = "attach"
	// OpResize tells the daemon the attached client's terminal size.
	OpResize Op = "resize"
	// OpServiceEnsure makes a service exist and run, defining or updating it as needed, without
	// ever making it briefly not exist. It is one operation because two clients doing it at the
	// same moment must not be able to delete each other's work.
	OpServiceEnsure Op = "service.ensure"
	// OpServiceLogs returns the retained output of a service without attaching to it.
	OpServiceLogs Op = "service.logs"
	// OpUpgrade asks the daemon to replace its own binary in place, keeping every process.
	OpUpgrade Op = "upgrade"
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
	// EventFinished says the service has exited and is not coming back, so an attached client
	// should let go rather than wait.
	//
	// Distinct from EventProcessExited, which also covers a process that is about to restart and
	// a keystroke that arrived while nothing was running. A client has to be able to tell "that
	// one is over" from "that one is busy being born", and one event kind carrying both meanings
	// would make it guess from the wording.
	EventFinished = "finished"
	// EventNotice is something the daemon wants the person at the other end to read, with no
	// change of state behind it. A read-only attach swallowing a keystroke is one: rule 1 says
	// an action that did nothing must not look like it worked.
	EventNotice = "notice"
	// EventRenamed says the service this client is attached to has a new name, carried in the
	// event's Service. The stream goes on; what changes is the name any later request about this
	// service must use - a client still using the old one would find "no such service".
	EventRenamed = "renamed"
)

// AddRequest is the payload of OpServiceAdd.
type AddRequest struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	Env     []string `json:"env,omitempty"`
	Restart string   `json:"restart,omitempty"`
	Start   bool     `json:"start,omitempty"`
	// NoLog asks for this service's output NOT to be written to disk.
	NoLog bool `json:"no_log,omitempty"`
}

// AttachRequest is the payload of OpAttach.
type AttachRequest struct {
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
	// Replay asks for the retained output before the live stream, so an attaching client sees
	// what is already on screen rather than a blank pane until something moves.
	Replay bool `json:"replay"`
	// ReadOnly asks to watch without being able to touch: the daemon drops this connection's
	// keystrokes and its resizes. Asked for by the client and enforced by the server, because
	// "my Ctrl-C does not reach it" is a claim about the far end and a client that promises not
	// to send is only a promise.
	ReadOnly bool `json:"readOnly,omitempty"`
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
	// ExitUnknown means the service ended while this daemon was not its parent - adopted after a
	// crash through systemd's file-descriptor store - so there is no code or signal to report.
	// A separate field rather than a sentinel in the two above, because every reader of those
	// would otherwise print "code -1" or "killed by unknown".
	ExitUnknown bool      `json:"exit_unknown,omitempty"`
	HasExited   bool      `json:"has_exited,omitempty"`
	NextRestart time.Time `json:"next_restart,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Enabled     bool      `json:"enabled"`
	Command     string    `json:"command,omitempty"`
	// LogError is why this service's output is not reaching disk, empty when it is.
	LogError string `json:"log_error,omitempty"`
	// Viewers is how many clients are attached right now. Only the daemon can know this, and it
	// is what tells you which service your other terminal is sitting in.
	Viewers int `json:"viewers,omitempty"`
	// Watchers is how many of those are read-only. The difference is the one a person cares
	// about: three terminals showing a service is reassuring, three terminals that can type into
	// it is a reason to find out whose they are before you restart it.
	Watchers int `json:"watchers,omitempty"`
	// LogPath and LogBytes are where this service's output is kept and how much of it there is,
	// counting the rotated generation. Disk is the resource a log-keeping supervisor quietly
	// spends, so it should be visible without going looking for the directory.
	LogPath  string `json:"log_path,omitempty"`
	LogBytes int64  `json:"log_bytes,omitempty"`
}

// RenameRequest is the payload of OpServiceRename; the request's Service is the current name.
type RenameRequest struct {
	To string `json:"to"`
}

// RemoveRequest is the payload of OpServiceRemove.
type RemoveRequest struct {
	// KeepLogs asks for the service's log file to be left on disk. By default it goes with the
	// service: for a shell that file is the whole transcript of the session, and removing the
	// service while silently keeping the transcript is a surprise nobody asked for.
	KeepLogs bool `json:"keep_logs,omitempty"`
}

// RemoveReply says what was removed, so the client can tell the user what is now gone.
type RemoveReply struct {
	// Logs are the log files that were deleted, if any.
	Logs []string `json:"logs,omitempty"`
}

// LogsRequest is the payload of OpServiceLogs.
type LogsRequest struct {
	// MaxBytes limits the answer to the most recent bytes. Zero means everything the daemon is
	// willing to put in one frame.
	MaxBytes int `json:"max_bytes,omitempty"`
	// Follow turns this request into a stream: the daemon answers once and then sends data
	// frames until the client hangs up, exactly like an attach with no keyboard attached.
	//
	// The one-shot form reads the file, which outlives the daemon; the follow form reads the
	// live buffer, which does not. They answer different questions and both are wanted.
	Follow bool `json:"follow,omitempty"`
}

// LogsReply carries retained output.
//
// Data is raw bytes, escape sequences and all, because that is what the process wrote; JSON
// encodes it as base64, which is acceptable for a one-shot query even though it would be wasteful
// for the live stream (which uses data frames instead).
type LogsReply struct {
	Data []byte `json:"data"`
	// Truncated says whether older output was left out, so "this is everything" and "this is
	// the tail" are distinguishable rather than both just being some bytes.
	Truncated bool `json:"truncated,omitempty"`
	// Running says whether anything is producing output right now; without it an empty answer
	// is ambiguous between "quiet" and "not running".
	Running bool `json:"running"`
	// Path is the file this came from, empty when it came from the daemon's in-memory ring.
	// Worth saying: one of those survives the daemon being restarted and the other does not,
	// and the client can point the operator at a file it can read with anything.
	Path string `json:"path,omitempty"`
	// LogError is why this answer may be incomplete - the writer is broken, so the file is
	// older than what the service has printed. An incomplete answer that does not say it is
	// incomplete is the one failure this project keeps refusing to ship.
	LogError string `json:"log_error,omitempty"`
}

// UpgradeReply describes what an in-place upgrade did.
type UpgradeReply struct {
	// Accepted is false when the daemon refused to try.
	Accepted bool `json:"accepted"`
	// Processes is how many running processes are being carried across.
	Processes int `json:"processes"`
	// Problems names services that will NOT survive, so the caller learns it before the
	// connection drops rather than by noticing a changed pid afterwards.
	Problems []string `json:"problems,omitempty"`
}

// ListReply is the payload of a successful OpServiceList.
type ListReply struct {
	Services []StatusReply `json:"services"`
	// Problems are definitions the daemon could not load. They travel with the list rather than
	// replacing it: a broken service file must not hide the nine working ones, and must not be
	// hidden by them either.
	Problems []string `json:"problems,omitempty"`
}
