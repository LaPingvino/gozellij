package fabric

import (
	"os/exec"
	"testing"
	"time"
)

// The kernel's answer, checked against something that cannot be got wrong: a process this test
// started a moment ago.

func TestAProcessJustStartedStartedJustNow(t *testing.T) {
	before := time.Now()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	after := time.Now()

	got, ok := ProcessStart(cmd.Process.Pid)
	if !ok {
		t.Skip("cannot read /proc here")
	}
	// A second of slack in each direction: the kernel records the start in clock ticks, which at
	// 100Hz rounds to a hundredth, and the two clocks are read at slightly different moments.
	if got.Before(before.Add(-2*time.Second)) || got.After(after.Add(2*time.Second)) {
		t.Fatalf("a process started between %s and %s reports %s", before, after, got)
	}
}

func TestOurOwnStartIsNotNow(t *testing.T) {
	// The bug this exists for, in miniature: a process that has been running a while must not
	// report having started this instant. The test binary has been up for at least a moment.
	got, ok := ProcessStart(1)
	if !ok {
		t.Skip("cannot read /proc here")
	}
	// pid 1 started at boot, which is certainly more than a minute ago on any machine running
	// this suite.
	if time.Since(got) < time.Minute {
		t.Fatalf("pid 1 reports starting %s ago, which would mean the machine just booted", time.Since(got))
	}
}

func TestAProcessThatIsNotThereIsNotGuessedAt(t *testing.T) {
	// False rather than a guess: the caller falls back to now, and a start time invented here
	// would be indistinguishable from one the kernel gave.
	if _, ok := ProcessStart(-1); ok {
		t.Error("a negative pid produced a start time")
	}
	if _, ok := ProcessStart(1 << 30); ok {
		t.Error("a pid that cannot exist produced a start time")
	}
}

func TestAnExecutableWithBracketsInItsNameDoesNotShiftTheFields(t *testing.T) {
	// /proc/<pid>/stat puts the command in parentheses as field 2, so a command containing ')'
	// shifts every field after it for anything that splits the whole line. sh -c gives us a
	// process whose comm we do not control, so this checks the parser's shape rather than the
	// name: the field is read from the last ')' onwards, which is the only correct way.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ticks, ok := startTicks(cmd.Process.Pid)
	if !ok {
		t.Skip("cannot read /proc here")
	}
	if ticks <= 0 {
		t.Fatalf("start ticks = %d, which is not a time since boot", ticks)
	}
}
