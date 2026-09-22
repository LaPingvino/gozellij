package fabric

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Watching a process this one cannot wait for.
//
// The real shape of it - a daemon that crashed, leaving its services reparented to the service
// manager - cannot be built inside a test: there is no way to stop being the parent of a process
// without ending. What can be checked is the mechanism, which is a pidfd rather than wait4, and
// the property that matters: it does not return while the process is alive, and it returns once it
// is not. Whether a non-parent may use it at all was measured against a real crash and recorded in
// docs/USER_STORIES.md C3.

func TestWatchingAnAdoptedProcessWaitsWhileItRuns(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	done := make(chan Exit, 1)
	go func() { done <- waitOrphan(cmd.Process.Pid) }()

	select {
	case e := <-done:
		t.Fatalf("it reported the process ended while it was still running: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWatchingAnAdoptedProcessReturnsWhenItEnds(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan Exit, 1)
	go func() { done <- waitOrphan(cmd.Process.Pid) }()

	time.Sleep(100 * time.Millisecond)
	_ = cmd.Process.Kill()

	select {
	case e := <-done:
		if !e.IsUnknown() {
			t.Fatalf("an adopted process ended as %+v; it should be the unknown exit", e)
		}
		if e.Clean() {
			// The choice, written down where it can be argued with: an ending nobody saw is
			// treated as a failure, because not restarting something that crashed costs more
			// than restarting something that had finished.
			t.Fatal("an exit nobody could observe reported itself as clean")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("it never noticed the process ended")
	}
	_ = cmd.Wait()
}

func TestWatchingAProcessThatIsAlreadyGoneReturnsAtOnce(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	done := make(chan Exit, 1)
	go func() { done <- waitOrphan(pid) }()
	select {
	case e := <-done:
		if !e.IsUnknown() {
			t.Fatalf("got %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("it waited for a process that had already gone")
	}
}

func TestAnUnknownExitIsNotMistakenForACleanOne(t *testing.T) {
	if UnknownExit.Clean() {
		t.Fatal("the unknown exit reports itself as clean, so -restart on-failure would ignore a crash")
	}
	if !UnknownExit.IsUnknown() {
		t.Fatal("the unknown exit does not recognise itself")
	}
	// And an ordinary exit is not mistaken for an unknown one, which is the direction that would
	// make every clean stop print "how is not known".
	if (Exit{}).IsUnknown() || (Exit{Code: 1}).IsUnknown() || (Exit{Signal: "killed"}).IsUnknown() {
		t.Fatal("an ordinary exit claims to be unknown")
	}
	// The restart policy has to act on it, which is the whole reason the distinction exists.
	if !RestartOnFailure.ShouldRestart(UnknownExit) {
		t.Fatal("-restart on-failure would not restart a service whose ending nobody saw")
	}
	if RestartNo.ShouldRestart(UnknownExit) {
		t.Fatal("-restart no restarted something")
	}
}

func TestAdoptingAnOrphanRefusesAProcessThatIsNotThere(t *testing.T) {
	// The same guard Adopt has, reached through the orphan door: a supervisor reporting a running
	// service that does not exist is a worse lie than admitting one was lost.
	svc := Service{Name: "gone", Command: "sleep"}
	if _, err := AdoptOrphan(svc, freePid(t), 0, time.Now(), StartOptions{}); err == nil {
		t.Fatal("adopted a pid with no process behind it")
	}
}

// freePid finds a pid that is not in use, by starting a process and letting it finish.
func freePid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	// Reaped, so the number is free again. It could in principle be reused before the check
	// below, which is why this is the only test that depends on it and why it checks rather than
	// assumes.
	if err := syscall.Kill(pid, 0); err == nil {
		t.Skipf("pid %d was reused before the test could use it", pid)
	}
	return pid
}
