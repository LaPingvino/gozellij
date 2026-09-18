package fabric

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ADVERSARIAL (review of 892bc5d/0cabb42): the early path of Stop - leader already reaped - claims
// to "ask, wait, and only then sweep". signalCgroup skips every pid whose process group is the
// leader's, on the premise that stopSignal already reached it; but the early path never calls
// stopSignal (SignalGroup refuses after a reap). So a child that stayed in the leader's process
// group is never asked, waitCgroupEmpty burns the whole StopGrace, and the sweep SIGKILLs it.
func TestEarlyStopPathAsksChildrenInTheLeadersProcessGroup(t *testing.T) {
	cg := requireCgroups(t)

	done := filepath.Join(t.TempDir(), "child-finished")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// No setsid: the child stays in the leader's process group. The leader exits at once. HUP is
	// ignored so the pty going away with the leader does not kill the child before Stop runs.
	p, err := Start(Service{
		Name: "orphaner", Command: "sh",
		Args: []string{"-c", "trap \"\" HUP; " + self + " </dev/null >/dev/null 2>&1 &\necho STARTED\nexit 0"},
		Env:  []string{"GOZELLIJ_TEST_SLOW_CHILD=" + done},
	}, StartOptions{Cgroups: cg})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()
	if p.Cgroup() == nil {
		t.Fatal("no cgroup")
	}

	waitFor(t, 10*time.Second, "child ready", func() bool {
		_, err := os.Stat(done + ".ready")
		return err == nil
	})
	p.Wait() // leader reaped: Stop will take the early path
	if _, ex := p.Exited(); !ex {
		t.Fatal("leader not exited")
	}

	t0 := time.Now()
	p.Stop()
	elapsed := time.Since(t0)
	t.Logf("early-path Stop took %v", elapsed)

	if _, err := os.Stat(done); err != nil {
		t.Errorf("the child in the leader's process group was never asked to stop (no %s after %v): it was SIGKILLed by the sweep", done, elapsed)
	}
	// Against the grace period itself, not a round number. The bug burned the whole of it
	// waiting for children nothing had signalled; the healthy path ends when the service does.
	// It is the secondary check - the file above is the one that catches a SIGKILLed child -
	// and a fixed threshold below this is not safe, because the drain can legitimately add up to
	// DrainGrace+DrainAbandon on a loaded machine or under -race.
	if elapsed >= StopGrace {
		t.Errorf("early-path Stop took %v, the whole grace period: nothing was signalled before the wait", elapsed)
	}
}
