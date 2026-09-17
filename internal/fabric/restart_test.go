package fabric

import (
	"testing"
	"time"
)

func TestShouldRestart(t *testing.T) {
	clean := Exit{Code: 0}
	failed := Exit{Code: 1}
	signalled := Exit{Code: 0, Signal: "SIGKILL"}

	cases := []struct {
		policy RestartPolicy
		exit   Exit
		want   bool
	}{
		{RestartNo, clean, false},
		{RestartNo, failed, false},
		{RestartOnFailure, clean, false},
		{RestartOnFailure, failed, true},
		// A process killed by a signal exits with code 0 in some reporting paths; it is still
		// not a clean exit, and on-failure must restart it.
		{RestartOnFailure, signalled, true},
		{RestartAlways, clean, true},
		{RestartAlways, failed, true},
	}
	for _, c := range cases {
		if got := c.policy.ShouldRestart(c.exit); got != c.want {
			t.Errorf("%s.ShouldRestart(%+v) = %v, want %v", c.policy, c.exit, got, c.want)
		}
	}
}

func TestParseRestartPolicy(t *testing.T) {
	for _, s := range []string{"no", "NO", " no ", "never", ""} {
		if p, err := ParseRestartPolicy(s); err != nil || p != RestartNo {
			t.Errorf("ParseRestartPolicy(%q) = %v, %v; want no, nil", s, p, err)
		}
	}
	for _, s := range []string{"on-failure", "on_failure", "OnFailure"} {
		if p, err := ParseRestartPolicy(s); err != nil || p != RestartOnFailure {
			t.Errorf("ParseRestartPolicy(%q) = %v, %v; want on-failure, nil", s, p, err)
		}
	}
	if _, err := ParseRestartPolicy("sometimes"); err == nil {
		t.Error("ParseRestartPolicy(\"sometimes\") should have failed")
	}
}

// A process that crashes immediately, over and over, must back off - and must stop doubling once
// it reaches the cap rather than growing without bound.
func TestBackoffGrowsAndCaps(t *testing.T) {
	var tr RestartTracker
	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second, // 32 clamped to the cap
		30 * time.Second,
		30 * time.Second,
	}
	for i, w := range want {
		if got := tr.Died(10 * time.Millisecond); got != w {
			t.Fatalf("restart %d: delay = %v, want %v", i+1, got, w)
		}
	}
	if tr.Restarts != len(want) {
		t.Errorf("Restarts = %d, want %d", tr.Restarts, len(want))
	}
}

// A process that stayed up long enough is not flapping, so its next failure should be treated as
// a first failure. This is the case that matters in practice: a service that runs for a week and
// then dies once should come back in a second, not in thirty.
func TestStableRunResetsBackoff(t *testing.T) {
	var tr RestartTracker
	tr.Died(10 * time.Millisecond) // 1s
	tr.Died(10 * time.Millisecond) // 2s
	if tr.Delay != 2*time.Second {
		t.Fatalf("precondition: Delay = %v, want 2s", tr.Delay)
	}

	if got := tr.Died(StableAfter); got != BackoffFirst {
		t.Errorf("after a stable run, delay = %v, want %v", got, BackoffFirst)
	}
	if tr.Restarts != 1 {
		t.Errorf("after a stable run, Restarts = %d, want 1 (history forgiven)", tr.Restarts)
	}
}

func TestResetForgetsHistory(t *testing.T) {
	var tr RestartTracker
	tr.Died(0)
	tr.Died(0)
	tr.Reset()
	if tr.Delay != 0 || tr.Restarts != 0 {
		t.Fatalf("after Reset: Delay = %v, Restarts = %d, want 0, 0", tr.Delay, tr.Restarts)
	}
	if got := tr.Died(0); got != BackoffFirst {
		t.Errorf("first failure after Reset = %v, want %v", got, BackoffFirst)
	}
}
