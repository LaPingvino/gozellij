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
	// StateFailed is a terminal state: the service exited badly and its policy says not to
	// restart, or it could not be started at all.
	StateFailed
	// StateExited is a terminal state for a service that finished cleanly - exit code 0, no
	// signal - and whose policy is done with it.
	//
	// Split out from StateFailed because calling a clean exit a failure is a small lie that
	// costs trust: the first time `gozellij` dropped someone into a shell and they typed exit,
	// `ls` said their shell had failed. Nothing failed.
	StateExited
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
	case StateExited:
		return "exited"
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
	// Started is true once the supervision loop has been launched, which is not the same as the
	// service running: there is a moment between the two in which the state is still Stopped.
	//
	// Anything that asks "should I start this?" has to consult this and not the state. Asking
	// the state instead meant that two clients starting a service at the same instant both saw
	// "stopped", both replaced the supervisor, and both spawned - one of them orphaned, running,
	// and invisible to everything that looks the service up by name.
	Started bool
	// Ended is true once the supervision loop has finished, which is the exact question
	// "is anything more going to happen to this service?" - and therefore the question an
	// attached client is really asking.
	//
	// It is not derivable from State. Stopped covers both a service nobody has started yet and
	// one whose loop has ended; failed-at-spawn never sets HasExited because nothing ever
	// exited. Answering from those left attached clients waiting on services that were never
	// going to move again.
	Ended bool
	// LogError is why this service's output is not reaching disk, empty when it is (or when
	// logging is off for it on purpose). Somebody has to say so before the operator finds out
	// by going looking for output that was never written.
	LogError string
	// LastError is why the most recent start attempt failed, empty when it did not.
	//
	// This is the field that stops a supervisor being a silent failure: a service whose binary
	// does not exist would otherwise sit in backing-off forever with nothing anywhere saying
	// why. Design rule 1.
	LastError string
}

// Running reports whether there is a live process.
func (s Status) Running() bool { return s.State == StateRunning }

// Live reports whether this supervisor is looking after the service right now - started, and not
// yet finished. It covers the gap between Start returning and the first process appearing, which
// Running does not.
func (s Status) Live() bool { return s.Started && !s.Ended }

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

	// watchers is shared across supervisor replacements, like the output buffer above and for
	// the same reason: the thing being watched is the *service*, and a supervisor is only its
	// current incarnation.
	watchers *StatusWatchers
}

// NewSupervisor creates a supervisor for a service. It does not start anything.
func NewSupervisor(s Service, opts StartOptions) *Supervisor {
	watchers := opts.Watchers
	if watchers == nil {
		watchers = NewStatusWatchers()
	}

	out := opts.Output
	if out == nil {
		out = NewOutputBuffer(opts.OutputBytes)
		// The sink is created exactly where the buffer is, and only there. A supervisor handed
		// an existing buffer is a *successor* (see Fabric.replaceSupervisor), and the buffer it
		// was handed already has a sink; opening a second one on the same file would write
		// every byte twice, which is the kind of quiet corruption nobody notices until they
		// read the log.
		attachLogSink(out, s, opts)
	}
	// Each spawn borrows the shared buffer.
	opts.Output = out

	return &Supervisor{
		svc:      s,
		opts:     opts,
		out:      out,
		after:    time.After,
		status:   Status{Service: s.Name, State: StateStopped},
		watchers: watchers,
	}
}

// attachLogSink starts writing this buffer to disk, unless the service says not to.
//
// A sink that will not open is recorded on the buffer rather than returned: a service whose log
// cannot be written should still run - it is a service, not a logger - but the operator must be
// able to find out, which `gozellij status` now tells them.
func attachLogSink(out *OutputBuffer, svc Service, opts StartOptions) {
	if opts.LogDir == "" {
		return
	}
	if svc.NoLog {
		return
	}
	if _, err := NewLogSink(out, LogPath(opts.LogDir, svc.Name), opts.LogBytes); err != nil {
		out.SetSinkError(err.Error())
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

// Service returns the definition this supervisor was built from.
//
// Under the lock, because the name in it can change while the loop runs: a service renamed while
// it is up keeps its supervisor, its process and its buffer, and only what it is called moves.
// This used to be documented as written once and read freely, which rename made untrue.
func (s *Supervisor) Service() Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.svc
}

// redefine replaces the definition this supervisor spawns from, keeping its name.
//
// For `gozellij set`. The supervisor restarts a crashed process by itself, from this copy, and
// that is the "next start" a restart policy exists for - so a definition changed only on disk was
// ignored exactly there: a new restart policy did not apply to the crash it was set for.
func (s *Supervisor) redefine(def Service) {
	s.mu.Lock()
	def.Name = s.svc.Name
	s.svc = def
	s.mu.Unlock()
}

// rename changes the name this supervisor answers to, and returns the process it is looking after
// at that moment, if any, so the caller can move whatever else is filed under the old name.
//
// The process itself keeps what it was started with: GOZELLIJ in its environment and its cgroup
// directory carry the old name until it next starts, because neither can be changed from outside.
// Nothing depends on either being current - the nesting guard asks the process tree.
func (s *Supervisor) rename(to string) *Process {
	s.mu.Lock()
	s.svc.Name = to
	p := s.cur
	s.mu.Unlock()
	s.setStatus(func(st *Status) { st.Service = to })
	return p
}

// Output is the buffer every process of this service writes into. It outlives any one process, so
// a viewer attached across a restart sees the old output, the restart notice and the new output as
// one continuous stream.
func (s *Supervisor) Output() *OutputBuffer { return s.out }

// Status returns a snapshot.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	st := s.status
	s.mu.Unlock()
	// Read from the buffer, not from a copy taken when the supervisor was built: the sink keeps
	// running while this supervisor does nothing, and a disk that filled up an hour ago is news.
	st.LogError = s.out.LogError()
	return st
}

// Current returns the live process, or nil when there is none.
func (s *Supervisor) Current() *Process {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// Watch returns a channel that fires when the status changes, and a function to stop watching.
// See StatusWatchers.
func (s *Supervisor) Watch() (<-chan struct{}, func()) { return s.watchers.Watch() }

// Watchers is the watcher set, so a replacement supervisor can inherit it.
func (s *Supervisor) Watchers() *StatusWatchers { return s.watchers }

func (s *Supervisor) setStatus(f func(*Status)) {
	s.mu.Lock()
	f(&s.status)
	s.mu.Unlock()
	// After the lock is released, so a watcher that wakes up and immediately reads Status cannot
	// be blocked by the goroutine that is telling it. The ordering that matters is the other
	// one: the status is already updated before anybody is told, so a watcher that registers,
	// reads, and then waits either sees the new status or is woken by this.
	s.watchers.Notify()
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

	// Published before the loop is launched, so that nobody can look at this supervisor in the
	// gap and conclude it needs starting.
	s.setStatus(func(st *Status) { st.Started = true })

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
	// idempotent. But wait for *both*: the loop ends when the leader is reaped, while stopping
	// the service also means giving what it started the rest of the grace period to finish. If
	// the loop wins the race - which it does whenever the leader dies before its children - then
	// waiting only for the loop returns while the children are still being asked to stop, and
	// `gozellij stop` reports done before the service is.
	stopped := make(chan struct{})
	if cur != nil {
		go func() {
			defer close(stopped)
			cur.Stop()
		}()
	} else {
		close(stopped)
	}

	<-done
	<-stopped
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
	// Registered after close(done), so it runs *before* it: anybody woken by the loop finishing
	// must already be able to see that it finished.
	defer s.setStatus(func(st *Status) { st.Ended = true })

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
			p, err = Start(s.Service(), s.opts)
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
			if s.Service().Restart != RestartAlways {
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
		// Outside the lock, and before the status goes out: whoever is keeping this descriptor
		// safe should have it before anything can act on the service being up.
		if s.opts.OnRunning != nil {
			s.opts.OnRunning(s.Service().Name, p.Pid(), p.PTY())
		}
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
		endedPid := p.Pid()
		p.Close()

		// The name is read in the same breath as the process is let go. Read later, a rename in
		// between saw no process to re-file and moved nothing - while this dropped the descriptor
		// under the new name, leaving the one filed under the old name in systemd's store for good.
		s.mu.Lock()
		s.cur = nil
		endedName := s.svc.Name
		s.mu.Unlock()
		// Told before anything else, because this is what stops a descriptor for a process that
		// no longer exists being kept for the next daemon to adopt.
		if s.opts.OnEnded != nil {
			s.opts.OnEnded(endedName, endedPid)
		}

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
		case !s.Service().Restart.ShouldRestart(exit):
			// Terminal, and say so plainly. A service that has finished is not "stopped" -
			// stopped is something an operator did - and it has not failed if it exited
			// cleanly.
			next = StateFailed
			if exit.Clean() {
				next = StateExited
			}
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
		s.Service().Name, delay.Round(time.Millisecond), restarts); werr != nil {
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
