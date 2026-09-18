package fabric

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Fabric owns every supervised service: the definitions on disk, the supervisors running them,
// and the mapping between the two.
//
// This is the object the daemon wraps in a socket. Nothing in it knows about terminals, panes or
// layouts - a UI is just something that attaches to an output buffer and writes to a pty.
type Fabric struct {
	reg  *Registry
	opts StartOptions

	mu   sync.Mutex
	sups map[string]*Supervisor

	// lifecycle holds one lock per service, taken by every operation that decides what should be
	// running and then acts on that decision.
	//
	// f.mu is not enough and never could be: it guards the map for the length of a lookup, while
	// the dangerous part is the gap between reading a service's state and changing it. Two
	// callers both read "not running", both install a fresh supervisor, and both spawn - one of
	// them orphaned, running, and invisible to ls, stop and rm. The widest version of that gap
	// was Restart, which installs a not-yet-started supervisor and then spends up to the whole
	// grace period stopping the old one; anything asking "does this need starting?" during those
	// five seconds was told yes.
	//
	// Per service rather than one lock for the fabric, because stopping one service must not
	// block starting another for five seconds.
	lifecycle map[string]*sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
	closed bool
}

// NewFabric creates a fabric backed by a registry. Nothing is started until Load or Start.
func NewFabric(reg *Registry, opts StartOptions) *Fabric {
	// Each supervisor makes its own shared output buffer; a single buffer handed to all of them
	// would interleave every service's output into one stream.
	opts.Output = nil

	ctx, cancel := context.WithCancel(context.Background())
	return &Fabric{
		reg:       reg,
		opts:      opts,
		sups:      make(map[string]*Supervisor),
		lifecycle: make(map[string]*sync.Mutex),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// ErrFabricClosed is returned once the fabric has been shut down.
var ErrFabricClosed = errors.New("fabric is shut down")

// Load reads every service definition and starts the ones marked enabled.
//
// It returns the problems it hit rather than the first one, because one unreadable service file
// must not stop the other nine services from coming up after a reboot - and must not vanish
// either. Design rule 1: the caller gets a fabric that is running *and* a list of what is wrong
// with it.
func (f *Fabric) Load() []error {
	services, errs := f.reg.List()

	for _, svc := range services {
		if err := f.adopt(svc); err != nil {
			errs = append(errs, err)
			continue
		}
		if svc.Enabled {
			if err := f.Start(svc.Name); err != nil {
				errs = append(errs, fmt.Errorf("starting %s at load: %w", svc.Name, err))
			}
		}
	}
	return errs
}

// adopt creates a supervisor for a definition without starting it.
func (f *Fabric) adopt(svc Service) error {
	if err := svc.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrFabricClosed
	}
	if _, exists := f.sups[svc.Name]; exists {
		return nil
	}
	f.sups[svc.Name] = NewSupervisor(svc, f.opts)
	return nil
}

// Handover describes one process inherited across an exec-in-place.
type Handover struct {
	Name      string    `json:"name"`
	Pid       int       `json:"pid"`
	PTYFd     int       `json:"pty_fd"`
	StartedAt time.Time `json:"started_at"`
}

// AdoptAll takes over processes inherited from a previous daemon, then loads everything else.
//
// Adoption happens before Load so that a service which is already running is never started twice.
// Getting that order wrong produces two copies of the same service and no complaint from anybody,
// which is exactly the silent-success failure this project is built to avoid.
//
// Problems are returned rather than fatal: a handover that lost one process out of ten must still
// bring the other nine across, and must say which one it lost.
func (f *Fabric) AdoptAll(handovers []Handover) []error {
	var errs []error

	for _, h := range handovers {
		svc, err := f.reg.Get(h.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("adopting %s: %w", h.Name, err))
			continue
		}
		sup := NewSupervisor(svc, f.opts)
		opts := f.opts
		opts.Output = sup.Output()

		p, err := Adopt(svc, h.Pid, h.PTYFd, h.StartedAt, opts)
		if err != nil {
			// Say so and carry on: Load will start it fresh below, which is a worse outcome
			// than a true handover but a much better one than a service that vanishes.
			//
			// Close the buffer on the way past. Building the supervisor opened a log file and
			// started a writer goroutine for it, and abandoning the supervisor here left both
			// alive for the life of the daemon - one leaked descriptor per failed handover,
			// per upgrade.
			sup.Output().Close()
			errs = append(errs, err)
			continue
		}
		sup.AdoptRunning(p)

		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			errs = append(errs, ErrFabricClosed)
			continue
		}
		f.sups[h.Name] = sup
		f.mu.Unlock()

		if err := sup.Start(f.ctx); err != nil && !errors.Is(err, ErrAlreadyStarted) {
			errs = append(errs, fmt.Errorf("supervising adopted %s: %w", h.Name, err))
		}
	}

	return append(errs, f.Load()...)
}

// Handovers describes every running process, for passing to a successor daemon.
//
// Only running processes appear: a service that is backing off or stopped has nothing to hand
// over, and the successor will start it from its definition like any other.
func (f *Fabric) Handovers() []Handover {
	f.mu.Lock()
	sups := make(map[string]*Supervisor, len(f.sups))
	for name, sup := range f.sups {
		sups[name] = sup
	}
	f.mu.Unlock()

	var out []Handover
	for name, sup := range sups {
		p := sup.Current()
		if p == nil {
			continue
		}
		fd := p.PTYFd()
		if fd < 0 {
			continue
		}
		out = append(out, Handover{
			Name:      name,
			Pid:       p.Pid(),
			PTYFd:     fd,
			StartedAt: p.StartedAt(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Add defines a new service and, if start is true, starts it.
//
// The definition is written to disk first. If we started it and then failed to persist it, a
// reboot would silently lose a service the user believes exists.
func (f *Fabric) Add(svc Service, start bool) error {
	l := f.lock(svc.Name)
	l.Lock()
	defer l.Unlock()
	return f.addLocked(svc, start)
}

func (f *Fabric) addLocked(svc Service, start bool) error {
	svc.Enabled = start
	if err := f.reg.Add(svc); err != nil {
		return err
	}
	if err := f.adopt(svc); err != nil {
		return err
	}
	if start {
		return f.startLocked(svc.Name)
	}
	return nil
}

// Ensure makes a service exist and run, without ever making it briefly not exist.
//
// This is what bare `gozellij` needs, and doing it from the client was a race with teeth. Two
// terminals starting at the same moment both saw a shell that was defined and not running, both
// removed it, and both added it - so one of them removed the *other's* freshly created service and
// the other's attach failed with "no such service: shell". Measured, with three at once.
//
// The rules, in the order they are checked:
//
//   - not defined: define it and start it;
//   - defined and running: leave it entirely alone. Someone is using it, and it is theirs;
//   - defined and not running: update the definition in place and start it. In place, not
//     remove-and-add, so there is never a moment when the service does not exist for somebody
//     else to trip over.
func (f *Fabric) Ensure(svc Service) error {
	if err := svc.Validate(); err != nil {
		return err
	}

	l := f.lock(svc.Name)
	l.Lock()
	defer l.Unlock()

	existing, err := f.reg.Get(svc.Name)
	if err != nil {
		if errors.Is(err, ErrNoSuchService) {
			return f.addLocked(svc, true)
		}
		return err
	}

	// Live, not Running: a service someone started a moment ago has no process yet, and
	// redefining it from under them would spawn a second one.
	if st, serr := f.Status(svc.Name); serr == nil && st.Live() {
		return nil
	}

	// Keep what belongs to the service's history rather than to this invocation.
	svc.CreatedAt = existing.CreatedAt
	svc.Enabled = true
	if err := f.reg.Put(svc); err != nil {
		return fmt.Errorf("updating %s: %w", svc.Name, err)
	}

	// A supervisor that has already finished will not run again, and Start replaces it - reading
	// the definition we just wrote, which is how the new environment reaches the new process.
	if _, err := f.supervisor(svc.Name); err != nil {
		if errors.Is(err, ErrNoSuchService) {
			// On disk but never adopted, which happens after a definition is repaired by hand.
			if aerr := f.adopt(svc); aerr != nil {
				return aerr
			}
		} else {
			return err
		}
	}
	return f.startLocked(svc.Name)
}

// lock returns the lifecycle lock for one service, creating it on first use.
//
// Locks are never removed, even when the service is. There is one small mutex per service name
// this fabric has ever handled, which is nothing, and removing them would reintroduce the race
// they exist to prevent: a caller waiting on the lock for a service being removed would otherwise
// find its lock deleted and take a fresh one.
func (f *Fabric) lock(name string) *sync.Mutex {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.lifecycle[name]
	if !ok {
		l = &sync.Mutex{}
		f.lifecycle[name] = l
	}
	return l
}

// supervisor looks one up.
func (f *Fabric) supervisor(name string) (*Supervisor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, ErrFabricClosed
	}
	s, ok := f.sups[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchService, name)
	}
	return s, nil
}

// Start runs a service and records that it should be running.
//
// A supervisor's loop ends for good when the service is stopped or gives up, and a Supervisor
// cannot be started twice - so starting a service that has already run its course needs a fresh
// one. Without this, `gozellij stop web` followed by `gozellij start web` wrote enabled: true,
// started nothing, and exited 0: the first command pair anybody types, silently doing nothing.
func (f *Fabric) Start(name string) error {
	l := f.lock(name)
	l.Lock()
	defer l.Unlock()
	return f.startLocked(name)
}

func (f *Fabric) startLocked(name string) error {
	s, err := f.supervisor(name)
	if err != nil {
		return err
	}
	if err := f.setEnabled(name, true); err != nil {
		return err
	}

	// Replace only a supervisor that is not looking after the service any more. Asking the state
	// instead - stopped, failed, exited - could not tell "never started" from "started a
	// microsecond ago and not spawned yet", so two Starts in quick succession replaced a live
	// supervisor and spawned a second process beside the first, which then had nothing pointing
	// at it.
	if st := s.Status(); !st.Live() {
		s = f.replaceSupervisor(name, s)
	}

	if err := s.Start(f.ctx); err != nil && !errors.Is(err, ErrAlreadyStarted) {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	return nil
}

// replaceSupervisor swaps in a fresh supervisor for a service whose old one has finished.
//
// Two things are deliberately carried across and one is deliberately not:
//
//   - the output buffer is reused, so `logs` still shows what the service said before it was
//     stopped. Scrollback surviving a restart is the same promise as scrollback surviving a
//     crash-and-restart, and an operator does not care which of the two happened;
//   - the definition is re-read from disk, because design rule 5 says a service file is meant to
//     be repairable with a text editor. Building from the in-memory copy meant an edit was
//     honoured by `gozellij upgrade` (which reloads) but ignored by `restart` (which did not) -
//     half-working, which is worse than either answer;
//   - the watcher set is reused, because what a client asked to watch is the service, not this
//     particular supervisor of it;
//   - the flapping history is not carried across. An operator has intervened, so a thirty second
//     backoff inherited from whatever went wrong before is no longer about the present.
func (f *Fabric) replaceSupervisor(name string, old *Supervisor) *Supervisor {
	svc := old.Service()
	if fromDisk, err := f.reg.Get(name); err == nil {
		svc = fromDisk
	}

	opts := f.opts
	opts.Output = old.Output()
	// The watcher set comes across too. It is not an optimisation: something attached to this
	// service is waiting to be told when it finishes, and leaving its watcher on the supervisor
	// we are throwing away is how that client ends up waiting forever.
	opts.Watchers = old.Watchers()
	fresh := NewSupervisor(svc, opts)

	f.mu.Lock()
	if !f.closed {
		f.sups[name] = fresh
	}
	f.mu.Unlock()
	return fresh
}

// Stop stops a service and records that it should stay stopped.
//
// A service you stopped on purpose must not come back by itself after a reboot; that is the
// behaviour that makes people stop trusting a supervisor and start using `kill` instead.
func (f *Fabric) Stop(name string) error {
	l := f.lock(name)
	l.Lock()
	defer l.Unlock()

	s, err := f.supervisor(name)
	if err != nil {
		return err
	}
	if err := f.setEnabled(name, false); err != nil {
		return err
	}
	s.Stop()
	return nil
}

// Restart stops a service and starts it again, clearing its flapping history so the operator does
// not inherit a thirty second backoff from whatever went wrong before.
//
// The successor is installed *before* the old one is stopped, and the order is the whole point.
// Stopping first published a status that said stopped, exited, killed by SIGTERM - every word of
// it true of the old process and none of it true of the service - and anything watching for the
// service to finish believed it. An attached client was dropped by a plain `gozellij restart`,
// told its service had been killed, while the service was two milliseconds from running again.
//
// With the successor already in place, a watcher woken during the stop reads a supervisor that has
// not run yet, which is not finished, which is the truth.
func (f *Fabric) Restart(name string) error {
	l := f.lock(name)
	l.Lock()
	defer l.Unlock()

	old, err := f.supervisor(name)
	if err != nil {
		return err
	}

	fresh := f.replaceSupervisor(name, old)

	// Still before the new spawn: two copies of a service running at once is a worse failure
	// than any status confusion. Stop waits for the old loop to finish.
	old.Stop()

	if err := f.setEnabled(name, true); err != nil {
		return err
	}
	if err := fresh.Start(f.ctx); err != nil && !errors.Is(err, ErrAlreadyStarted) {
		return fmt.Errorf("restarting %s: %w", name, err)
	}
	return nil
}

// setEnabled records the desired state on disk.
func (f *Fabric) setEnabled(name string, enabled bool) error {
	svc, err := f.reg.Get(name)
	if err != nil {
		return err
	}
	if svc.Enabled == enabled {
		return nil
	}
	svc.Enabled = enabled
	if err := f.reg.Put(svc); err != nil {
		return fmt.Errorf("recording desired state for %s: %w", name, err)
	}
	// Deliberately not mirrored onto the supervisor. Its copy of the definition is written once
	// at construction and read by its loop goroutine forever after; poking a field in it from
	// here would be a data race, and the registry is the single source of truth for desired
	// state anyway.
	return nil
}

// Remove stops a service and deletes its definition, and by default its log.
//
// The log goes because of what a log is here. For a shell it is the complete transcript of
// everything you typed and everything it answered, and "remove this service" leaving that on disk
// is a surprise of exactly the kind this project is meant not to spring on people. keepLogs is for
// the times you are removing a definition to redefine it and want the history to continue.
func (f *Fabric) Remove(name string, keepLogs bool) error {
	l := f.lock(name)
	l.Lock()
	defer l.Unlock()

	s, err := f.supervisor(name)
	if err != nil {
		// Still try to remove a definition with no supervisor, so a fabric that failed to
		// adopt a broken service can still be cleaned up. Otherwise a service file that
		// cannot be loaded also cannot be deleted, which is a trap.
		if errors.Is(err, ErrNoSuchService) {
			if rerr := f.reg.Remove(name); rerr != nil {
				return rerr
			}
			if !keepLogs {
				return f.removeLogs(name)
			}
			return nil
		}
		return err
	}
	s.Stop()

	// Close the buffer as well as the supervisor. It is what keeps the log writer alive, and a
	// service that no longer exists should not still have a file handle open on its behalf.
	// Anything attached to it has its stream ended, which is the honest answer to "the service
	// you were watching has been removed".
	s.Output().Close()

	f.mu.Lock()
	delete(f.sups, name)
	f.mu.Unlock()

	if err := f.reg.Remove(name); err != nil {
		return err
	}
	if keepLogs {
		return nil
	}
	return f.removeLogs(name)
}

// removeLogs deletes a service's log and its rotated generation.
//
// Only after the buffer has been closed, which stops the writer: deleting a file a process still
// holds open removes the name and not the bytes, and the next rotation would recreate it.
func (f *Fabric) removeLogs(name string) error {
	if f.opts.LogDir == "" {
		return nil
	}
	path := LogPath(f.opts.LogDir, name)
	var errs []error
	for _, p := range []string{path, path + ".1"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		// Worth reporting: the service is gone and its transcript is not, which is the state
		// somebody would want to know about rather than discover.
		return fmt.Errorf("removed the service but not its log: %w", errors.Join(errs...))
	}
	return nil
}

// LogFiles are the log files a service has on disk, for telling the user what is about to go.
func (f *Fabric) LogFiles(name string) []string {
	if f.opts.LogDir == "" {
		return nil
	}
	var found []string
	path := LogPath(f.opts.LogDir, name)
	for _, p := range []string{path, path + ".1"} {
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
		}
	}
	return found
}

// Status returns one service's status.
func (f *Fabric) Status(name string) (Status, error) {
	s, err := f.supervisor(name)
	if err != nil {
		return Status{}, err
	}
	return s.Status(), nil
}

// List returns the status of every known service, sorted by name.
func (f *Fabric) List() []Status {
	f.mu.Lock()
	sups := make([]*Supervisor, 0, len(f.sups))
	for _, s := range f.sups {
		sups = append(sups, s)
	}
	f.mu.Unlock()

	out := make([]Status, 0, len(sups))
	for _, s := range sups {
		out = append(out, s.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// LogTail is one answer to "what did this service print".
type LogTail struct {
	Data []byte
	// Truncated says whether older output was left out, so "this is everything" and "this is
	// the tail" are distinguishable rather than both being some bytes.
	Truncated bool
	// Path is the file the answer came from, or "" when it came from the in-memory ring - which
	// is worth saying out loud, because one of those survives the daemon and the other does not.
	Path string
	// Err is why this answer may be incomplete: the log writer is broken, so what is on disk is
	// older than what the service has actually printed. Without it, `logs` hands over a stale
	// file with the confidence of a complete one.
	Err string
}

// Logs returns what a service printed, preferring the file on disk.
//
// The file is the point: a ring in RAM answers "what is it doing" but not "what did that build
// print last night", because the daemon it lived in has been replaced twice since. The ring is the
// fallback for a service whose logging is off, and for the window between a service being defined
// and its first byte reaching disk.
func (f *Fabric) Logs(name string, maxBytes int) (LogTail, error) {
	// Look the supervisor up first, so an unknown service is reported as unknown rather than as
	// a missing file.
	sup, err := f.supervisor(name)
	if err != nil {
		return LogTail{}, err
	}

	// A service with logging off must never be answered from a file, even when one exists.
	// Removing a service leaves its log behind, so `rm x` followed by `add x -log off` would
	// otherwise print the *previous* service's output and label it as this one's - which is not
	// a stale answer, it is somebody else's answer.
	logged := f.opts.LogDir != ""
	if def, derr := f.reg.Get(name); derr == nil && def.NoLog {
		logged = false
	}

	// A broken writer changes which half is worth having. The file holds the long history but
	// stops at the moment the writing broke; the ring holds the most recent output, which is
	// what somebody typing `logs` almost always wants. So answer from the ring and say plainly
	// that the file is there and is behind - rather than handing over the older half with the
	// confidence of a complete answer.
	var logErr string
	if logged {
		if e := sup.Output().LogError(); e != "" {
			logErr = fmt.Sprintf("%s; %s stops where the writing stopped, and this is the "+
				"most recent output instead", e, LogPath(f.opts.LogDir, name))
			logged = false
		}
	}

	if logged {
		data, truncated, rerr := ReadLogTail(f.opts.LogDir, name, maxBytes)
		switch {
		case rerr == nil:
			return LogTail{Data: data, Truncated: truncated, Path: LogPath(f.opts.LogDir, name)}, nil
		case errors.Is(rerr, os.ErrNotExist):
			// No file yet. Fall through to the ring.
		default:
			return LogTail{}, rerr
		}
	}

	data, _ := sup.Output().Snapshot()
	truncated := false
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
		truncated = true
	}
	// Path stays empty: this came from memory, and saying otherwise would misreport where it
	// came from in exactly the situation where that matters most.
	return LogTail{Data: data, Truncated: truncated, Err: logErr}, nil
}

// Cgroups reports how completely this fabric can stop a service.
func (f *Fabric) Cgroups() *Cgroups { return f.opts.Cgroups }

// Watch returns a channel that fires when a service's status changes, and a function to stop
// watching. See Supervisor.Watch.
func (f *Fabric) Watch(name string) (<-chan struct{}, func(), error) {
	sup, err := f.supervisor(name)
	if err != nil {
		return nil, nil, err
	}
	ch, stop := sup.Watch()
	return ch, stop, nil
}

// Finished reports whether the supervisor is done with this service.
//
// The question an attached client is asking is "will anything more happen here?", and the honest
// answer is whether the supervision loop has ended - not whether a process exited. Three cases got
// this wrong when it was derived from the state instead:
//
//   - a service whose binary does not exist fails at spawn, so nothing ever exited;
//   - a service stopped before it managed to spawn is stopped without having exited either;
//   - a service nobody has started yet is also stopped, and something attached to it is
//     reasonably waiting for somebody to start it - which is the case that must still wait.
//
// Ended distinguishes the last one from the first two, which no combination of State and HasExited
// does.
func (s Status) Finished() bool { return s.Ended }

// Output returns a service's output buffer, which outlives any one process of it.
func (f *Fabric) Output(name string) (*OutputBuffer, error) {
	s, err := f.supervisor(name)
	if err != nil {
		return nil, err
	}
	return s.Output(), nil
}

// Process returns a service's live process, or nil when it is not running. Used to send input and
// to resize.
func (f *Fabric) Process(name string) (*Process, error) {
	s, err := f.supervisor(name)
	if err != nil {
		return nil, err
	}
	return s.Current(), nil
}

// Definition returns the on-disk definition of a service.
func (f *Fabric) Definition(name string) (Service, error) {
	return f.reg.Get(name)
}

// Shutdown stops every service and releases the fabric.
//
// Services are stopped concurrently: a dozen services each taking their SIGTERM grace period one
// after another would turn a shutdown into a minute of waiting.
func (f *Fabric) Shutdown() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	sups := make([]*Supervisor, 0, len(f.sups))
	for _, s := range f.sups {
		sups = append(sups, s)
	}
	f.mu.Unlock()

	f.cancel()

	var wg sync.WaitGroup
	for _, s := range sups {
		wg.Add(1)
		go func(s *Supervisor) {
			defer wg.Done()
			s.Stop()
		}(s)
	}
	wg.Wait()
}
