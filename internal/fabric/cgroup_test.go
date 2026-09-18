package fabric

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireCgroups skips a test that needs a delegated cgroup, saying why.
//
// Most of the time there is not one: a test run from a login shell inherits a root-owned session
// scope. To run these, put the test under a delegated scope:
//
//	systemd-run --user --scope -p Delegate=yes go test ./internal/fabric/ -run Cgroup
func requireCgroups(t *testing.T) *Cgroups {
	t.Helper()
	cg := DetectCgroups()
	if !cg.Available() {
		t.Skipf("no delegated cgroup here (%s); run under: systemd-run --user --scope -p Delegate=yes", cg.Why())
	}
	return cg
}

func TestCgroupsSayWhyTheyAreUnavailable(t *testing.T) {
	// A disabled set must explain itself. "tree-kill is off" with no reason is the kind of
	// answer that sends somebody reading source at 3am.
	cg := NoCgroups()
	if cg.Available() {
		t.Error("NoCgroups reports itself available")
	}
	if cg.Why() == "" {
		t.Error("no explanation for unavailable cgroups")
	}
	if _, err := cg.Create("web"); err == nil {
		t.Error("Create succeeded on an unavailable set")
	}
}

func TestNilCgroupsAreSafeToUse(t *testing.T) {
	// StartOptions.Cgroups is nil in every test that does not care, so the nil path has to be
	// as ordinary as the others.
	var cg *Cgroups
	if cg.Available() {
		t.Error("a nil set reports itself available")
	}
	if cg.Why() == "" {
		t.Error("a nil set gives no explanation")
	}
	if cg.Root() != "" {
		t.Errorf("Root = %q, want empty", cg.Root())
	}
}

// The case the whole mechanism exists for: a child that calls setsid leaves the process group, and
// a process-group kill cannot reach it. A cgroup can.
func TestStopKillsASetsidChildWhenThereIsACgroup(t *testing.T) {
	cg := requireCgroups(t)

	s := NewSupervisor(Service{
		Name: "detacher", Command: "sh",
		Args:    []string{"-c", `setsid sleep 600 & echo STARTED; wait`},
		Restart: RestartNo,
	}, StartOptions{Cgroups: cg})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForOutput(t, s, "STARTED")

	p := s.Current()
	if p == nil {
		t.Fatal("no process")
	}
	group := p.Cgroup()
	if group == nil {
		t.Fatal("the process has no cgroup although one was available")
	}

	// The setsid'd grandchild is in the cgroup even though it is in another session: cgroup
	// membership is inherited and cannot be left from inside. That is the whole point.
	pids := waitForCgroupPids(t, group, 2)
	t.Logf("cgroup holds %v", pids)

	s.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for {
		left, err := group.Pids()
		if err != nil || len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d process(es) survived stop: %v", len(left), left)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A stop that is not needed must still sweep. The first version killed the cgroup only after the
// grace period, so a service whose main process exited promptly never swept at all - and the
// setsid'd child this exists for survived every well-behaved stop.
func TestAPromptExitStillSweepsTheCgroup(t *testing.T) {
	cg := requireCgroups(t)

	s := NewSupervisor(Service{
		Name: "prompt", Command: "sh",
		// Exits the moment it is asked, leaving its detached child behind.
		Args:    []string{"-c", `setsid sleep 600 & echo STARTED; wait`},
		Restart: RestartNo,
	}, StartOptions{Cgroups: cg})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForOutput(t, s, "STARTED")

	p := s.Current()
	group := p.Cgroup()
	waitForCgroupPids(t, group, 2)

	t0 := time.Now()
	s.Stop()
	if elapsed := time.Since(t0); elapsed > StopGrace {
		t.Errorf("stop took %v, longer than the grace period: it should not have needed it", elapsed)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		left, err := group.Pids()
		if err != nil || len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a prompt stop left %v behind", left)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Adoption across a daemon upgrade has to reclaim the cgroup, or the successor keeps every process
// and quietly loses the ability to stop them completely.
func TestAdoptReclaimsTheCgroupOfARunningProcess(t *testing.T) {
	cg := requireCgroups(t)

	group, err := cg.Create("adoptme")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { group.Kill(); group.Remove() })

	// Put a process of our own making in it, then ask for it back by pid.
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if err := group.Add(cmd.Process.Pid); err != nil {
		t.Fatalf("Add: %v", err)
	}

	back, err := cg.Adopt(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if back.Dir() != group.Dir() {
		t.Errorf("Adopt returned %q, want %q", back.Dir(), group.Dir())
	}
}

// Adopting must refuse anything outside our own subtree: those are not ours to kill.
func TestAdoptRefusesAProcessOutsideOurSubtree(t *testing.T) {
	cg := requireCgroups(t)

	// pid 1 is systemd, comfortably outside a user scope.
	if _, err := cg.Adopt(1); err == nil {
		t.Error("Adopt accepted a process outside the delegated subtree")
	}
}

func TestCgroupKillOfAMissingCgroupIsNotAnError(t *testing.T) {
	cg := requireCgroups(t)
	group, err := cg.Create("gone")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := group.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Nothing left to kill is the outcome that was asked for, not a failure.
	if err := group.Kill(); err != nil {
		t.Errorf("Kill of a removed cgroup = %v, want nil", err)
	}
}

func waitForOutput(t *testing.T, s *Supervisor, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := s.Output().Snapshot()
		if strings.Contains(string(data), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %q in the output; have %q", want, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForCgroupPids(t *testing.T, g *Cgroup, want int) []int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		pids, err := g.Pids()
		if err != nil {
			t.Fatalf("Pids: %v", err)
		}
		if len(pids) >= want {
			return pids
		}
		if time.Now().After(deadline) {
			t.Fatalf("cgroup holds %v, wanted at least %d processes", pids, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The grace period belongs to the service, not to its leader.
//
// A previous version waited for the leader to be reaped and then killed the cgroup immediately, so
// a child still shutting down when its parent exited was SIGKILLed a few milliseconds into a five
// second grace. That is worse than the process-group version it replaced, which left children
// alone entirely.
func TestChildrenGetTheGracePeriodTooNotJustTheLeader(t *testing.T) {
	cg := requireCgroups(t)

	done := filepath.Join(t.TempDir(), "child-finished")
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("finding the test binary: %v", err)
	}

	// The child is this test binary re-executed in slow-child mode: it catches SIGTERM, takes
	// half a second, and records that it finished. A shell script cannot play that part
	// reliably - its trap waits for whatever command is running and re-entry differs between
	// shells - and a test that failed one run in three was measuring that, not this.
	//
	// It calls setsid, so it is in neither the leader's process group nor its session: a cgroup
	// is the only thing that still reaches it, which is the point.
	s := NewSupervisor(Service{
		Name: "graceful", Command: "sh",
		Args: []string{"-c", "setsid " + self + " </dev/null >/dev/null 2>&1 &\necho STARTED\nexec sleep 600"},
		Env:  []string{"GOZELLIJ_TEST_SLOW_CHILD=" + done},
	}, StartOptions{Cgroups: cg})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForOutput(t, s, "STARTED")

	p := s.Current()
	if p == nil || p.Cgroup() == nil {
		t.Fatal("no process or no cgroup")
	}
	waitForCgroupPids(t, p.Cgroup(), 2)

	// Wait for the child to be ready to catch a signal, not merely to exist. A SIGTERM that
	// arrives before it installs its handler kills it outright, and the test would then be
	// measuring Go's start-up time.
	ready := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(done + ".ready"); err == nil {
			break
		}
		if time.Now().After(ready) {
			t.Fatal("the child never became ready to catch a signal")
		}
		time.Sleep(10 * time.Millisecond)
	}

	t0 := time.Now()
	s.Stop()
	elapsed := time.Since(t0)

	if _, err := os.Stat(done); err != nil {
		t.Errorf("after %v the child had not finished shutting down: %v", elapsed, err)
	}
	// And the stop waited for it rather than for the whole grace period: once the service is
	// empty there is nothing left to wait for.
	if elapsed >= StopGrace {
		t.Errorf("stop took %v, the entire grace period; it should end when the service does", elapsed)
	}
}

// cgroup.kill is asynchronous, so a task that has been killed is still listed until it finishes
// exiting and an immediate rmdir gets EBUSY. Without a retry that left one dead directory per stop
// for the life of the daemon.
func TestTheCgroupDirectoryIsRemovedAfterASweep(t *testing.T) {
	cg := requireCgroups(t)

	for round := 0; round < 3; round++ {
		s := NewSupervisor(Service{
			Name: "tree", Command: "sh",
			Args: []string{"-c", `setsid sleep 600 </dev/null >/dev/null 2>&1 &
setsid sleep 600 </dev/null >/dev/null 2>&1 &
echo STARTED; exec sleep 600`},
			Restart: RestartNo,
		}, StartOptions{Cgroups: cg})
		if err := s.Start(context.Background()); err != nil {
			t.Fatalf("round %d: Start: %v", round, err)
		}
		waitForOutput(t, s, "STARTED")

		p := s.Current()
		dir := p.Cgroup().Dir()
		waitForCgroupPids(t, p.Cgroup(), 3)

		s.Stop()
		s.Wait()

		// The removal is deliberately off the exit path - a directory that will not empty must
		// not delay a service being reported as finished - so poll for it rather than assuming
		// it has already happened.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("round %d: %s survived the stop", round, dir)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}
