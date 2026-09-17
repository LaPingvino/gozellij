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

		p, err := Start(s.svc, s.opts)
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
			if !s.backOff(ctx, 0) {
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
		s.setStatus(func(st *Status) {
			st.LastExit = exit
			st.HasExited = true
			st.Pid = 0
			st.StartedAt = time.Time{}
		})

		if ctx.Err() != nil {
			s.setStatus(func(st *Status) { st.State = StateStopped })
			return
		}

		if !s.svc.Restart.ShouldRestart(exit) {
			// Terminal, and say so plainly. A service that has finished is not "stopped" -
			// stopped is something an operator did.
			s.setStatus(func(st *Status) { st.State = StateFailed })
			return
		}

		if !s.backOff(ctx, ran) {
			return
		}
	}
}

// backOff waits before the next attempt. It reports false if the wait was interrupted, in which
// case the loop should end.
func (s *Supervisor) backOff(ctx context.Context, ran time.Duration) bool {
	s.mu.Lock()
	delay := s.tracker.Died(ran)
	restarts := s.tracker.Restarts
	s.mu.Unlock()

	s.setStatus(func(st *Status) {
		st.State = StateBackingOff
		st.Restarts = restarts
		st.NextRestart = time.Now().Add(delay)
	})

	// Announce the restart in the output itself. A viewer watching a pane should not have to
	// guess why the program they were using suddenly started again from the top.
	fmt.Fprintf(s.out, "\r\n[gozellij] %s restarting in %s (restart %d)\r\n",
		s.svc.Name, delay.Round(time.Millisecond), restarts)

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
