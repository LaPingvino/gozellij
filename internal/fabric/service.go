package fabric

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Service is one thing the fabric runs and looks after.
//
// This is the on-disk record, so it is plain data: no handles, no channels, nothing that cannot
// be written to a file and read back after a reboot. Design rule 5 - state the fabric must not
// lose goes on disk, in a format a human can read and repair. If gozellij is broken and someone
// needs to fix a service by hand at 3am, they should be able to do it with a text editor.
type Service struct {
	// Name identifies the service. See ValidServiceName for what is allowed and why.
	Name string `json:"name"`
	// Command is the program to run, and Args its arguments. Kept separate rather than as one
	// shell string: no shell means no quoting bugs and no injection surface.
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	// Dir is the working directory. Empty means the fabric's own.
	Dir string `json:"dir,omitempty"`
	// Env are extra environment entries as KEY=VALUE, applied on top of the inherited
	// environment.
	Env []string `json:"env,omitempty"`
	// Restart is what to do when it exits.
	Restart RestartPolicy `json:"restart"`
	// Enabled is the *desired* state, and it is what makes a reboot uneventful: the fabric
	// starts enabled services when it loads and leaves the rest alone. Stopping a service
	// clears it, so something you stopped on purpose stays stopped afterwards rather than
	// quietly coming back - which is the behaviour that makes people distrust a supervisor.
	Enabled bool `json:"enabled"`
	// NoLog turns off writing this service's output to disk.
	//
	// It is off-by-absence rather than on-by-absence because the safe default for a supervisor
	// is to keep the output: "what did that build print" is the question logs exist for. But a
	// login shell prints whatever you `cat`, so the choice has to be available per service, and
	// it has to be visible in the file an operator reads at 3am.
	NoLog bool `json:"no_log,omitempty"`
	// CloseOnExit is a tab you opened to type in: when it exits by itself while you are looking
	// at it, the attach removes it from the list rather than leaving a dead entry, the way a tmux
	// window closes. Its log is kept. Set by `gozellij shell` and Ctrl-] c, and by add
	// -close-on-exit; carried through a rename because it is part of the definition, not the name.
	CloseOnExit bool `json:"close_on_exit,omitempty"`
	// CreatedAt is when the service was first defined.
	CreatedAt time.Time `json:"created_at"`
}

// RestartPolicy round-trips as its string spelling, so an on-disk service file says
// "on-failure" rather than "1". A number here would be unreadable and would silently change
// meaning if the constants were ever reordered.
func (p RestartPolicy) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.String())
}

func (p *RestartPolicy) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("restart policy must be a string: %w", err)
	}
	parsed, err := ParseRestartPolicy(s)
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// ErrNoSuchService is returned when a named service is not defined.
var ErrNoSuchService = errors.New("no such service")

// ErrServiceExists is returned when adding a service whose name is taken.
var ErrServiceExists = errors.New("service already exists")

// serviceNamePattern is deliberately strict. A service name becomes a filename, part of a unix
// socket path, and a cgroup directory name, so anything clever in it turns into a path traversal
// or an unopenable socket. Rejecting early is much kinder than failing later in three different
// ways.
var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidServiceName checks a name and explains the rule when it fails, rather than returning a
// bare false. An error message that states the rule saves the reader a trip to the source.
func ValidServiceName(name string) error {
	if name == "" {
		return errors.New("service name is empty")
	}
	if !serviceNamePattern.MatchString(name) {
		return fmt.Errorf("invalid service name %q: use 1-64 characters, lowercase a-z, 0-9, "+
			"underscore or hyphen, starting with a letter or digit "+
			"(the name becomes a filename, a socket path and a cgroup directory)", name)
	}
	return nil
}

// Validate checks a service definition is usable.
func (s *Service) Validate() error {
	if err := ValidServiceName(s.Name); err != nil {
		return err
	}
	if strings.TrimSpace(s.Command) == "" {
		return fmt.Errorf("service %q has no command", s.Name)
	}
	for _, e := range s.Env {
		if !strings.Contains(e, "=") {
			return fmt.Errorf("service %q: environment entry %q is not KEY=VALUE", s.Name, e)
		}
	}
	return nil
}

// Registry is the set of defined services, backed by one JSON file per service in a directory.
//
// One file per service rather than a single file: two concurrent edits to different services
// cannot then clobber each other, and a corrupted service loses one service instead of all of
// them.
type Registry struct {
	dir string
}

// NewRegistry opens (and creates if needed) a registry in dir.
func NewRegistry(dir string) (*Registry, error) {
	if dir == "" {
		return nil, errors.New("registry directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating registry directory %s: %w", dir, err)
	}
	return &Registry{dir: dir}, nil
}

// Dir returns the directory backing this registry.
func (r *Registry) Dir() string { return r.dir }

func (r *Registry) path(name string) string {
	return filepath.Join(r.dir, name+".json")
}

// Add writes a new service. It fails if the name is already taken.
func (r *Registry) Add(s Service) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	if _, err := os.Stat(r.path(s.Name)); err == nil {
		return fmt.Errorf("%w: %s", ErrServiceExists, s.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking for existing service %s: %w", s.Name, err)
	}
	return r.write(s)
}

// Put writes a service, replacing any existing definition of the same name.
func (r *Registry) Put(s Service) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	return r.write(s)
}

// write saves a service atomically: a temporary file in the same directory, then a rename. A
// half-written service file after a crash or a full disk would be worse than no file, because the
// fabric would start refusing to load a service that looks defined.
func (r *Registry) write(s Service) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding service %s: %w", s.Name, err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(r.dir, "."+s.Name+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file for service %s: %w", s.Name, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best effort: if we got as far as the rename this is already gone.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("writing service %s: %w", s.Name, err)
	}
	// Sync before the rename. Without it the rename can be durable while the contents are not,
	// which is precisely the empty-file-after-power-loss case.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing service %s: %w", s.Name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing service %s: %w", s.Name, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("setting permissions on service %s: %w", s.Name, err)
	}
	if err := os.Rename(tmpName, r.path(s.Name)); err != nil {
		return fmt.Errorf("installing service %s: %w", s.Name, err)
	}
	return nil
}

// Get loads one service.
func (r *Registry) Get(name string) (Service, error) {
	if err := ValidServiceName(name); err != nil {
		return Service{}, err
	}
	b, err := os.ReadFile(r.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return Service{}, fmt.Errorf("%w: %s", ErrNoSuchService, name)
	} else if err != nil {
		return Service{}, fmt.Errorf("reading service %s: %w", name, err)
	}
	var s Service
	if err := json.Unmarshal(b, &s); err != nil {
		// Name the file. Someone is going to have to open it.
		return Service{}, fmt.Errorf("service file %s is not valid: %w", r.path(name), err)
	}
	return s, nil
}

// Remove deletes a service definition.
func (r *Registry) Remove(name string) error {
	if err := ValidServiceName(name); err != nil {
		return err
	}
	err := os.Remove(r.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNoSuchService, name)
	} else if err != nil {
		return fmt.Errorf("removing service %s: %w", name, err)
	}
	return nil
}

// List returns every service, sorted by name.
//
// A file that cannot be parsed is reported rather than skipped: silently omitting a service the
// user can see on disk is exactly the "empty answer that looks like no data" failure this project
// is trying not to repeat (design rule 1). Callers get both the services that loaded and the
// problems, so they can show a list *and* complain.
func (r *Registry) List() ([]Service, []error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, []error{fmt.Errorf("reading registry directory %s: %w", r.dir, err)}
	}
	var (
		out  []Service
		errs []error
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		s, err := r.Get(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}
