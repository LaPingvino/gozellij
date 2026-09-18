package fabric

// Adversarial review test for commit 53ade1f. Needs a delegated cgroup:
//
//	systemd-run --user --scope -p Delegate=yes go test ./internal/fabric/ -run Cgroup -v

import (
	"context"
	"testing"
	"time"
)

// A service that exits on its own and leaves a detached child is deliberately not swept. But
// Process.Close now retries the cgroup rmdir for two seconds, synchronously, in the supervisor
// loop - and that directory is never going to empty. So every self-exit with a leftover costs
// two seconds before the status says exited, which for a shell is two seconds between typing
// `exit` (with a nohup'd job behind you) and the terminal coming back.
func TestAnExitWithALeftoverChildIsReportedPromptly(t *testing.T) {
	cg := requireCgroups(t)

	s := NewSupervisor(Service{
		Name: "leaver", Command: "sh",
		// The 0.3s is only so the test can read the cgroup before the leader is gone.
		Args:    []string{"-c", `setsid sleep 600 </dev/null >/dev/null 2>&1 & echo STARTED; sleep 0.3; exit 0`},
		Restart: RestartNo,
	}, StartOptions{Cgroups: cg})
	t0 := time.Now()
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForOutput(t, s, "STARTED")
	p := s.Current()
	if p == nil {
		// It may already have exited; that is fine, but then we cannot get the cgroup.
		t.Skip("process exited before its cgroup could be read")
	}
	group := p.Cgroup()
	t.Cleanup(func() { _ = group.Kill() })

	s.Wait()
	elapsed := time.Since(t0)

	left, _ := group.Pids()
	if len(left) == 0 {
		t.Fatalf("the leftover child is gone, so this test's premise (an unswept self-exit) does not hold")
	}
	// DrainGrace covers the leftover child holding nothing open (it was redirected), so the
	// only thing that can account for two seconds is removeCgroup's retry loop.
	if elapsed >= 2*time.Second {
		t.Errorf("a self-exit that left a child behind took %v to be reported as finished; "+
			"removeCgroup retried for its whole 2s budget on a directory that will never empty", elapsed)
	}
}
