package fabric

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// State is what a supervised service is doing.
type State int

const (
	// StateStopped means the supervisor is not running the service, either because it has not
	// been started or because it was stopped deliberately.
	StateStopped State = iota
	// StateRunning means there is a live process.
	StateRunning
	// StateBackingOff means the process exited and a restart is scheduled.
	StateBackingOff
	// StateFailed is a terminal state: the service exited and its policy says not to restart,
	// or it could not be started at all.
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateRunning:
		return "running"
	case StateBackingOff:
		return "backing-off"
	case StateFailed:
		return "failed"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// Status is a snapshot of a supervised service.
//
// It is a plain value so callers cannot hold a reference into the supervisor's internals and read
// a half-updated field.
type Status struct {
	Service string
	State   State
	// Pid of the current process, 0 when there is none.
	Pid int
	// StartedAt is when the current process started; zero when there is none.
	StartedAt time.Time
	// Restarts is how many times it has been restarted since it was last considered stable.
	Restarts int
	// TotalStarts counts every spawn since the supervisor began, including the first.
	TotalStarts int
	// LastExit is how the previous process ended, valid when HasExited is true.
	LastExit  Exit
	HasExited bool
	// NextRestart is when the next attempt is due, valid in StateBackingOff.
	NextRestart time.Time
	// LastError is why the most recent start attempt failed, empty when it did not.
	//
	// This is the field that stops a supervisor being a silent failure: a service whose binary
	// does not exist would otherwise sit in backing-off forever with nothing anywhere saying
	// why. Design rule 1.
	LastError string
}

// Running reports whether there is a live process.
func (s Status) Running() bool { return s.State == StateRunning }

// Supervisor keeps one service running according to its restart policy.
type Supervisor struct {
	svc  Service
	opts StartOptions

	// out is shared across restarts, so scrollback survives one. The supervisor owns it.
	out *OutputBuffer

	// after is time.After, replaceable in tests so backoff does not mean waiting for it.
	after func(time.Duration) <-chan time.Time

	mu      sync.Mutex
	status  Status
	cur     *Process
	tracker RestartTracker

	cancel  context.CancelFunc
	done    chan struct{}
	started bool

	// adopted is a process inherited across an exec-in-place, to be supervised instead of
	// spawning a new one. Consumed by the first turn of the loop.
	adopted *Process

	// notify is a coalescing signal that Status changed. Capacity one: a watcher that has not
	// caught up does not need to be told twice, it needs to read the current status.
	notify chan struct{}
}

// NewSupervisor creates a supervisor for a service. It does not start anything.
func NewSupervisor(s Service, opts StartOptions) *Supervisor {
	out := opts.Output
	if out == nil {
		out = NewOutputBuffer(opts.OutputBytes)
	}
	// Each spawn borrows the shared buffer.
	opts.Output = out

	return &Supervisor{
		svc:    s,
		opts:   opts,
		out:    out,
		after:  time.After,
		status: Status{Service: s.Name, State: StateStopped},
		notify: make(chan struct{}, 1),
	}
}

// AdoptRunning tells the supervisor to look after an already-running process rather than starting
// one. It must be called before Start.
//
// The output buffer of the adopted process is the supervisor's, so scrollback from before the
// upgrade continues into whatever the process says afterwards - one stream across a daemon that
// was replaced underneath it.
func (s *Supervisor) AdoptRunning(p *Process) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adopted = p
}

// Service returns the definition this supervisor was built from. It is written once at
// construction and never modified, so the copy is safe to read while the loop runs.
func (s *Supervisor) Service() Service { return s.svc }

// Output is the buffer every process of this service writes into. It outlives any one process, so
// a viewer attached across a restart sees the old output, the restart notice and the new output as
// one continuous stream.
func (s *Supervisor) Output() *OutputBuffer { return s.out }

// Status returns a snapshot.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Current returns the live process, or nil when there is none.
func (s *Supervisor) Current() *Process {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// Changed signals that the status has changed. It coalesces: one wakeup may cover several
// changes, so read Status after receiving.
func (s *Supervisor) Changed() <-chan struct{} { return s.notify }

func (s *Supervisor) setStatus(f func(*Status)) {
	s.mu.Lock()
	f(&s.status)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// ErrAlreadyStarted is returned by Start when the supervisor is already running.
var ErrAlreadyStarted = fmt.Errorf("supervisor already started")

// Start begins supervising. It returns as soon as the loop is running; use Status or Changed to
// follow what happens.
//
// The returned error covers only refusing to start at all. A service whose first spawn fails is
// not an error here - it is a status, because the policy may well say to keep trying and a binary
// that is missing now may exist in a second.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return ErrAlreadyStarted
	}
	s.started = true
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()

	go s.run(loopCtx, done)
	return nil
}

// Stop stops the service and waits for the loop to finish.
//
// It is safe to call on a supervisor that was never started or has already stopped.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	cur := s.cur
	s.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	// Nudge the child now rather than waiting for the loop to notice; the loop's own Stop is
	// idempotent.
	if cur != nil {
		go cur.Stop()
	}
	<-done
}

// Wait blocks until the supervision loop has finished.
func (s *Supervisor) Wait() {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return
	}
	<-done
}

// run is the supervision loop.
func (s *Supervisor) run(ctx context.Context, done chan struct{}) {
	defer close(done)

	for {
		if ctx.Err() != nil {
			s.setStatus(func(st *Status) {
				st.State = StateStopped
				st.Pid = 0
				st.StartedAt = time.Time{}
			})
			return
		}

		// An inherited process is supervised as it is: not restarted, not re-spawned, not
		// disturbed. This is the whole point of the upgrade path.
		s.mu.Lock()
		p := s.adopted
		s.adopted = nil
		s.mu.Unlock()

		var err error
		if p == nil {
			p, err = Start(s.svc, s.opts)
		}
		if err != nil {
			// A spawn that fails is reported, not swallowed. Whether we try again is the
			// policy's business: "always" means a binary that appears later will be picked
			// up, while the others stop here rather than spinning on a typo forever.
			s.setStatus(func(st *Status) {
				st.LastError = err.Error()
				st.Pid = 0
				st.StartedAt = time.Time{}
			})
			if s.svc.Restart != RestartAlways {
				s.setStatus(func(st *Status) { st.State = StateFailed })
				return
			}
			delay, restarts := s.nextBackoff(0)
			s.setStatus(func(st *Status) {
				st.State = StateBackingOff
				st.Restarts = restarts
				st.NextRestart = time.Now().Add(delay)
			})
			if !s.waitBackoff(ctx, delay, restarts) {
				return
			}
			continue
		}

		s.mu.Lock()
		s.cur = p
		s.mu.Unlock()
		s.setStatus(func(st *Status) {
			st.State = StateRunning
			st.Pid = p.Pid()
			st.StartedAt = p.StartedAt()
			st.TotalStarts++
			st.LastError = ""
			// Clear the old deadline. Leaving it set meant a running service carried a
			// restart time that had already passed, for the rest of its life - and the CLI
			// grew a `time.Until(...) > 0` guard to hide it, which treated the symptom and
			// left the wrong data on the wire for every other consumer.
			st.NextRestart = time.Time{}
		})

		// Wait for the process to exit, or for us to be told to stop.
		select {
		case <-p.Done():
		case <-ctx.Done():
			p.Stop()
		}

		exit := p.Wait()
		ran := p.Ran()
		p.Close()

		s.mu.Lock()
		s.cur = nil
		s.mu.Unlock()

		// Work out where we are going *before* publishing, so the snapshot a watcher sees is
		// internally consistent. Publishing the exit first and the state afterwards opened a
		// window - announced by Changed(), so watchers were actively invited into it - where
		// Status said StateRunning, Pid 0, HasExited true, all at once. Every one of those
		// fields was true of a different instant.
		next := StateBackingOff
		var delay time.Duration
		var restarts int
		switch {
		case ctx.Err() != nil:
			next = StateStopped
		case !s.svc.Restart.ShouldRestart(exit):
			// Terminal, and say so plainly. A service that has finished is not "stopped" -
			// stopped is something an operator did.
			next = StateFailed
		default:
			// Work the delay out now, so it can be published together with the state it
			// belongs to. Announcing StateBackingOff first and filling in NextRestart
			// afterwards just moves the inconsistency rather than removing it: watchers then
			// see a service backing off until an unspecified time, which is not an
			// improvement on a running service with a stale deadline.
			delay, restarts = s.nextBackoff(ran)
		}

		s.setStatus(func(st *Status) {
			st.LastExit = exit
			st.HasExited = true
			st.Pid = 0
			st.StartedAt = time.Time{}
			st.State = next
			if next == StateBackingOff {
				st.Restarts = restarts
				st.NextRestart = time.Now().Add(delay)
			}
		})

		if next != StateBackingOff {
			return
		}

		if !s.waitBackoff(ctx, delay, restarts) {
			return
		}
	}
}

// nextBackoff advances the flapping tracker and reports the delay and restart count. Separated
// from the waiting so the caller can publish them in the same Status update as the state.
func (s *Supervisor) nextBackoff(ran time.Duration) (time.Duration, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracker.Died(ran), s.tracker.Restarts
}

// waitBackoff announces the restart and waits. It reports false if the wait was interrupted, in
// which case the loop should end.
func (s *Supervisor) waitBackoff(ctx context.Context, delay time.Duration, restarts int) bool {
	// Announce the restart in the output itself. A viewer watching a pane should not have to
	// guess why the program they were using suddenly started again from the top.
	//
	// If that write fails the buffer is closed, which means every future line this service
	// produces goes nowhere and no viewer can ever attach to it again. Restarting into a void
	// is not a service, it is a process nobody can see - so stop, and record why, instead of
	// looping forever in the dark.
	if _, werr := fmt.Fprintf(s.out, "\r\n[gozellij] %s restarting in %s (restart %d)\r\n",
		s.svc.Name, delay.Round(time.Millisecond), restarts); werr != nil {
		s.setStatus(func(st *Status) {
			st.State = StateFailed
			st.LastError = fmt.Sprintf("cannot write to this service's output buffer (%v); "+
				"not restarting, because nothing would be able to see it", werr)
			st.NextRestart = time.Time{}
		})
		return false
	}

	select {
	case <-s.after(delay):
		return true
	case <-ctx.Done():
		s.setStatus(func(st *Status) {
			st.State = StateStopped
			st.NextRestart = time.Time{}
		})
		return false
	}
}
