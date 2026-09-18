package fabric

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Cgroups are how a service is stopped completely rather than mostly.
//
// Killing the process group gets everything a service started except a child that called setsid,
// which leaves the group by definition - and "except the ones that deliberately detached" is not
// much of a promise for a supervisor. A cgroup has no such escape: membership is inherited by
// every descendant and cannot be left from inside, and `cgroup.kill` takes the lot.
//
// The catch is that it needs a cgroup we are allowed to subdivide, and that depends on how the
// daemon was started. Under the systemd user unit, Delegate=yes hands us our own subtree - that is
// exactly what that line in gozellijd.service is for. Started from a login shell, the daemon lands
// in a session scope, which is root-owned and cannot be written to at all. So this detects rather
// than assumes, and says which it found, because a tree-kill that silently is not one would be the
// worst of both.
type Cgroups struct {
	// root is the delegated directory service cgroups are created in, empty when unavailable.
	root string
	// why explains an empty root in terms an operator can act on.
	why string
}

// cgroupMount is where cgroup v2 is mounted on any system that has it.
const cgroupMount = "/sys/fs/cgroup"

// cgroupSeq makes each process's cgroup name unique.
//
// Reusing a name is tempting and wrong: a cgroup directory cannot be removed while anything is in
// it, so a service that is being restarted would try to create one that its predecessor still
// occupies - and the obvious repair, killing whatever is in the way, could kill a process adopted
// from a previous daemon. A fresh name has neither problem.
//
// Seeded from the clock rather than starting at zero, because an upgrade replaces the daemon's
// image while keeping its pid: a counter that restarted would hand out names the previous image
// had already used, and any directory it left behind - one holding a detached child, say - would
// make the new image's Create fail and quietly downgrade that service to a process-group kill.
var cgroupSeq atomic.Uint64

func init() { cgroupSeq.Store(uint64(time.Now().UnixNano())) }

// DetectCgroups works out whether this process can create cgroups, by trying.
//
// Trying rather than inspecting permissions: the mode bits are not the whole story (nsdelegate,
// read-only mounts and container runtimes all get a say), and the only question that matters is
// whether a mkdir succeeds.
func DetectCgroups() *Cgroups {
	path, err := cgroupPathOf("self")
	if err != nil {
		return &Cgroups{why: err.Error()}
	}

	root := filepath.Join(cgroupMount, path)
	if _, err := os.Stat(root); err != nil {
		return &Cgroups{why: fmt.Sprintf("this process's cgroup %s is not readable: %v", root, err)}
	}

	probe := filepath.Join(root, fmt.Sprintf("gozellij-probe-%d", os.Getpid()))
	if err := os.Mkdir(probe, 0o755); err != nil {
		if os.IsPermission(err) {
			return &Cgroups{why: fmt.Sprintf("%s is not writable, so it cannot be subdivided "+
				"(a daemon started from a login shell lands in a root-owned session scope; "+
				"the systemd user unit sets Delegate=yes for this)", root)}
		}
		return &Cgroups{why: fmt.Sprintf("cannot create a cgroup under %s: %v", root, err)}
	}
	defer os.Remove(probe)

	// cgroup.kill is what makes this worth having, and it arrived in Linux 5.14. Without it we
	// would have to walk cgroup.procs and signal pids, racing anything that forks meanwhile -
	// so if it is missing, say so and use the process group instead.
	if _, err := os.Stat(filepath.Join(probe, "cgroup.kill")); err != nil {
		return &Cgroups{why: "this kernel's cgroups have no cgroup.kill (Linux 5.14 or newer has it)"}
	}

	return &Cgroups{root: root}
}

// NoCgroups is a disabled set, for tests and for callers that do not want them.
func NoCgroups() *Cgroups { return &Cgroups{why: "cgroups are switched off"} }

// Available reports whether service cgroups can be created.
func (c *Cgroups) Available() bool { return c != nil && c.root != "" }

// Root is the delegated directory, empty when unavailable.
func (c *Cgroups) Root() string {
	if c == nil {
		return ""
	}
	return c.root
}

// Why explains why cgroups are unavailable, empty when they are not.
func (c *Cgroups) Why() string {
	if c == nil {
		return "cgroups were never detected"
	}
	return c.why
}

// Create makes a cgroup for one run of one service.
func (c *Cgroups) Create(service string) (*Cgroup, error) {
	if !c.Available() {
		return nil, errors.New("no delegated cgroup: " + c.Why())
	}
	dir := filepath.Join(c.root, fmt.Sprintf("gozellij-%s-%d", service, cgroupSeq.Add(1)))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the cgroup for %s: %w", service, err)
	}
	return &Cgroup{dir: dir}, nil
}

// Adopt returns the cgroup a running process is already in, when it is one of ours.
//
// This is what keeps tree-kill working across an upgrade: the successor daemon inherits the
// processes but not the handles, and a service it cannot kill completely is one it has quietly
// stopped making the promise about.
func (c *Cgroups) Adopt(pid int) (*Cgroup, error) {
	if !c.Available() {
		return nil, errors.New("no delegated cgroup: " + c.Why())
	}
	path, err := cgroupPathOf(strconv.Itoa(pid))
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(cgroupMount, path)

	// Only adopt something inside our own subtree. Anything else is not ours to kill.
	rel, err := filepath.Rel(c.root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("process %d is in %s, which is not under %s", pid, dir, c.root)
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("the cgroup of process %d is gone: %w", pid, err)
	}
	return &Cgroup{dir: dir}, nil
}

// Cgroup is one service run's cgroup.
type Cgroup struct {
	dir string
}

// Dir is the cgroup's directory.
func (g *Cgroup) Dir() string {
	if g == nil {
		return ""
	}
	return g.dir
}

// Open returns a file handle on the cgroup directory, for SysProcAttr.CgroupFD.
//
// Placing the child at fork rather than afterwards closes a real hole: a process that forks before
// we get round to writing its pid leaves grandchildren outside the cgroup, and those are exactly
// the ones this exists to catch.
func (g *Cgroup) Open() (*os.File, error) {
	if g == nil {
		return nil, errors.New("no cgroup")
	}
	return os.Open(g.dir)
}

// Add puts an existing process into the cgroup. Used when placing it at fork was not possible.
func (g *Cgroup) Add(pid int) error {
	if g == nil {
		return errors.New("no cgroup")
	}
	if err := os.WriteFile(filepath.Join(g.dir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("adding process %d to %s: %w", pid, g.dir, err)
	}
	return nil
}

// Kill kills every process in the cgroup, including the ones that left the process group.
func (g *Cgroup) Kill() error {
	if g == nil {
		return errors.New("no cgroup")
	}
	if err := os.WriteFile(filepath.Join(g.dir, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ENODEV) {
			// Already gone: there is nothing left to kill, which is the outcome asked for.
			//
			// ENODEV as well as ENOENT, because cgroupfs answers that way for a directory
			// removed after the file was opened - which happens whenever two Stops race and
			// one removes the cgroup while the other is still sweeping. The daemon log said
			// "cgroup kill failed" for a kill that had nothing left to do.
			return nil
		}
		return fmt.Errorf("killing %s: %w", g.dir, err)
	}
	return nil
}

// Pids lists what is in the cgroup. For checking that a kill worked, and for tests.
func (g *Cgroup) Pids() ([]int, error) {
	if g == nil {
		return nil, errors.New("no cgroup")
	}
	data, err := os.ReadFile(filepath.Join(g.dir, "cgroup.procs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var pids []int
	for _, line := range strings.Fields(string(data)) {
		if pid, err := strconv.Atoi(line); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// Remove deletes the cgroup directory, which only works once it is empty.
func (g *Cgroup) Remove() error {
	if g == nil {
		return nil
	}
	if err := os.Remove(g.dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", g.dir, err)
	}
	return nil
}

// cgroupPathOf reads the unified cgroup path of a process from /proc.
//
// The v2 line is the one beginning "0::". A system running cgroup v1 only has no such line, and
// this says that rather than returning something that looks like a path and is not.
func cgroupPathOf(who string) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", who, "cgroup"))
	if err != nil {
		return "", fmt.Errorf("reading the cgroup of %s: %w", who, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if after, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(after), nil
		}
	}
	return "", errors.New("no cgroup v2 (unified) hierarchy on this system")
}
