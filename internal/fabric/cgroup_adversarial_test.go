package fabric

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A child that outlives the main process by even a moment gets no grace at all: Stop waits for
// p.done (the *leader* reaped and drained), then cgroup.kill, which is SIGKILL, takes the rest.
// Run with and without a cgroup: the marker written by the child's SIGTERM handler appears only
// in the process-group-only run.
//
//	systemd-run --user --scope -p Delegate=yes go test ./internal/fabric/ -run Adversarial -v
func TestAdversarialChildrenGetNoGraceOnceTheLeaderIsGone(t *testing.T) {
	cg := requireCgroups(t)

	run := func(t *testing.T, opts StartOptions) bool {
		marker := filepath.Join(t.TempDir(), "graceful")
		// The child stays in the process group (so both modes SIGTERM it), ignores HUP (so the
		// leader dying does not take it), and redirects its fds away from the pty (so the
		// leader's pty drains the instant it dies and p.done closes promptly). Its TERM handler
		// needs one second to finish - a database flushing, say.
		script := `sh -c 'trap "" HUP; trap "sleep 1; echo DONE > ` + marker + `; exit 0" TERM; while :; do sleep 0.1; done' >/dev/null 2>&1 </dev/null & echo STARTED; exec sleep 600`
		s := NewSupervisor(Service{
			Name: "graceful", Command: "sh", Args: []string{"-c", script}, Restart: RestartNo,
		}, opts)
		if err := s.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitForOutput(t, s, "STARTED")
		time.Sleep(300 * time.Millisecond) // let the child install its traps

		t0 := time.Now()
		s.Stop()
		t.Logf("stop took %v", time.Since(t0))

		// Give the handler every chance.
		time.Sleep(2 * time.Second)
		_, err := os.Stat(marker)
		return err == nil
	}

	withCgroup := run(t, StartOptions{Cgroups: cg})
	withoutCgroup := run(t, StartOptions{})
	t.Logf("child's TERM handler completed: with cgroup=%v, process group only=%v", withCgroup, withoutCgroup)
	if withoutCgroup && !withCgroup {
		t.Errorf("with a cgroup the child was SIGKILLed before its SIGTERM handler could finish; " +
			"without one it finished. The grace period only ever applied to the leader.")
	}
}

// After a stop that had something left to sweep, cgroup.kill is asynchronous and Close calls
// Remove right away. If rmdir races the dying processes, the directory stays behind for good.
func TestAdversarialCgroupDirectoryIsRemovedAfterASweep(t *testing.T) {
	cg := requireCgroups(t)

	leaked := 0
	const rounds = 5
	for i := 0; i < rounds; i++ {
		// A small tree that is still alive when the leader is reaped, with its fds off the
		// pty so the leader drains at once and Close follows the sweep immediately.
		s := NewSupervisor(Service{
			Name: "detacher", Command: "sh",
			Args: []string{"-c", `sh -c 'trap "" HUP TERM; while :; do sleep 0.1; done' >/dev/null 2>&1 </dev/null &
setsid sleep 600 >/dev/null 2>&1 </dev/null & echo STARTED; exec sleep 600`},
			Restart: RestartNo,
		}, StartOptions{Cgroups: cg})
		if err := s.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitForOutput(t, s, "STARTED")
		group := s.Current().Cgroup()
		waitForCgroupPids(t, group, 3)

		s.Stop() // returns after the loop ends, i.e. after p.Close() has called Remove

		time.Sleep(500 * time.Millisecond)
		if _, err := os.Stat(group.Dir()); err == nil {
			leaked++
			pids, _ := group.Pids()
			t.Logf("round %d: %s still exists after stop (pids now: %v)", i, group.Dir(), pids)
			_ = group.Remove() // tidy up; a second try succeeds once the tasks are gone
		}
	}
	if leaked > 0 {
		t.Errorf("%d of %d stops left their cgroup directory behind: Remove ran before cgroup.kill had finished", leaked, rounds)
	}
}
