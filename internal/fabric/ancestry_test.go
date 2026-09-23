package fabric

import (
	"os"
	"os/exec"
	"testing"
)

func TestDescendsFrom(t *testing.T) {
	me := os.Getpid()
	if !DescendsFrom(me, me) {
		t.Error("a process does not count as inside itself")
	}
	if !DescendsFrom(me, os.Getppid()) {
		t.Error("this test does not descend from its own parent")
	}

	// A child with a name built to confuse a naive parse of /proc/<pid>/stat.
	dir := t.TempDir()
	odd := dir + "/a) b (c"
	if err := os.Symlink("/bin/sleep", odd); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(odd, "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()

	if !DescendsFrom(child.Process.Pid, me) {
		t.Error("a child started here does not descend from here")
	}
	if !DescendsFrom(child.Process.Pid, os.Getppid()) {
		t.Error("a grandchild does not descend from its grandparent")
	}
	// The other direction: the parent is not inside its child.
	if DescendsFrom(me, child.Process.Pid) {
		t.Error("this process claims to descend from its own child")
	}
	// Init is everybody's ancestor, so asking about it tells nothing and must not be a yes.
	if DescendsFrom(me, 1) {
		t.Error("pid 1 counted as an ancestor")
	}
	if DescendsFrom(me, 0) || DescendsFrom(0, me) {
		t.Error("pid 0 matched")
	}
}
