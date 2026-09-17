package fabric

import (
	"context"
	"errors"
	"fmt"
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
		reg:    reg,
		opts:   opts,
		sups:   make(map[string]*Supervisor),
		ctx:    ctx,
		cancel: cancel,
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
	svc.Enabled = start
	if err := f.reg.Add(svc); err != nil {
		return err
	}
	if err := f.adopt(svc); err != nil {
		return err
	}
	if start {
		return f.Start(svc.Name)
	}
	return nil
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
	s, err := f.supervisor(name)
	if err != nil {
		return err
	}
	if err := f.setEnabled(name, true); err != nil {
		return err
	}

	if st := s.Status(); st.State == StateStopped || st.State == StateFailed {
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
//   - the flapping history is not carried across. An operator has intervened, so a thirty second
//     backoff inherited from whatever went wrong before is no longer about the present.
func (f *Fabric) replaceSupervisor(name string, old *Supervisor) *Supervisor {
	svc := old.Service()
	if fromDisk, err := f.reg.Get(name); err == nil {
		svc = fromDisk
	}

	opts := f.opts
	opts.Output = old.Output()
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
func (f *Fabric) Restart(name string) error {
	s, err := f.supervisor(name)
	if err != nil {
		return err
	}
	s.Stop()
	f.replaceSupervisor(name, s)
	return f.Start(name)
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

// Remove stops a service and deletes its definition.
func (f *Fabric) Remove(name string) error {
	s, err := f.supervisor(name)
	if err != nil {
		// Still try to remove a definition with no supervisor, so a fabric that failed to
		// adopt a broken service can still be cleaned up. Otherwise a service file that
		// cannot be loaded also cannot be deleted, which is a trap.
		if errors.Is(err, ErrNoSuchService) {
			return f.reg.Remove(name)
		}
		return err
	}
	s.Stop()

	f.mu.Lock()
	delete(f.sups, name)
	f.mu.Unlock()

	return f.reg.Remove(name)
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
