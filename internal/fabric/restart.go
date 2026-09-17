// Package fabric is the part of gozellij that owns running things: processes, their PTYs, their
// cgroups, and the decision of what to do when one of them dies.
//
// Nothing in this package knows what a terminal looks like.
package fabric

import (
	"fmt"
	"strings"
	"time"
)

// RestartPolicy says what should happen when a supervised process exits.
type RestartPolicy int

const (
	// RestartNo leaves the process dead. The pane is held open so the exit status is still
	// readable - a process that dies must not also erase the evidence.
	RestartNo RestartPolicy = iota
	// RestartOnFailure restarts only on a non-zero exit or a signal. A clean exit is taken at
	// its word.
	RestartOnFailure
	// RestartAlways restarts regardless of exit status.
	RestartAlways
)

func (p RestartPolicy) String() string {
	switch p {
	case RestartNo:
		return "no"
	case RestartOnFailure:
		return "on-failure"
	case RestartAlways:
		return "always"
	default:
		return fmt.Sprintf("RestartPolicy(%d)", int(p))
	}
}

// ParseRestartPolicy accepts the spellings the CLI and the on-disk service files use.
func ParseRestartPolicy(s string) (RestartPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "no", "never", "":
		return RestartNo, nil
	case "on-failure", "onfailure", "on_failure":
		return RestartOnFailure, nil
	case "always":
		return RestartAlways, nil
	default:
		return RestartNo, fmt.Errorf("unknown restart policy %q (want no, on-failure or always)", s)
	}
}

// Exit describes how a supervised process ended.
type Exit struct {
	Code   int
	Signal string // empty unless the process was killed by a signal
}

// Clean reports whether this was an ordinary successful exit.
func (e Exit) Clean() bool { return e.Code == 0 && e.Signal == "" }

// ShouldRestart applies the policy to an exit.
func (p RestartPolicy) ShouldRestart(e Exit) bool {
	switch p {
	case RestartAlways:
		return true
	case RestartOnFailure:
		return !e.Clean()
	default:
		return false
	}
}

// Backoff timings. A process that crashes instantly and repeatedly must not become a busy loop,
// but a process that has been up for a while and then dies once should come back promptly - so the
// delay grows while it is flapping and resets once it has proven it can stay up.
const (
	BackoffFirst = 1 * time.Second
	BackoffMax   = 30 * time.Second
	// StableAfter is how long a process must run before we stop considering it to be flapping.
	StableAfter = 60 * time.Second
)

// RestartTracker decides how long to wait before the next restart of one process.
//
// It is deliberately a value with no clock of its own: the caller passes the times in, so this is
// testable without sleeping. (The fork's equivalent logic could only be tested by waiting, which
// meant in practice it was not tested.)
type RestartTracker struct {
	// Delay is the wait before the next restart. Zero until the first failure.
	Delay time.Duration
	// Restarts counts restarts since the process was last considered stable.
	Restarts int
}

// Died records an exit and returns how long to wait before restarting.
//
// ranFor is how long the process was alive. A process that managed StableAfter is treated as a
// fresh start: its history of flapping is forgiven and the next delay goes back to BackoffFirst.
func (t *RestartTracker) Died(ranFor time.Duration) time.Duration {
	if ranFor >= StableAfter {
		t.Delay = 0
		t.Restarts = 0
	}
	switch {
	case t.Delay == 0:
		t.Delay = BackoffFirst
	case t.Delay < BackoffMax:
		t.Delay *= 2
		if t.Delay > BackoffMax {
			t.Delay = BackoffMax
		}
	}
	t.Restarts++
	return t.Delay
}

// Reset forgets the flapping history, e.g. after an operator restarts the service by hand.
func (t *RestartTracker) Reset() {
	t.Delay = 0
	t.Restarts = 0
}
