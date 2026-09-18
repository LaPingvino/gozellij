package fabric

// Regressions found by an independent adversarial pass, kept as assertions of the fixed behaviour.
//
// All three are the same shape: two callers decide what should be running, and act on that
// decision without holding anything. They are the reason Fabric takes a per-service lifecycle lock
// rather than trusting the state it just read.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// pidfileService is a service that records its pid and then sleeps, ignoring SIGTERM so that a
// stop takes the whole grace period - which is what an interactive shell does too.
func pidfileService(t *testing.T, name string, ignoreTerm bool) (Service, string) {
	t.Helper()
	pidfile := filepath.Join(t.TempDir(), "pids")
	script := `echo $$ >> ` + pidfile + `; exec sleep 300`
	if ignoreTerm {
		script = `trap "" TERM; ` + script
	}
	svc := Service{Name: name, Command: "sh", Args: []string{"-c", script}, Restart: RestartNo}
	t.Cleanup(func() {
		// The whole point of these tests is that the fabric loses track of some of these
		// processes, so the fabric's own Shutdown cannot be relied on to kill them.
		for _, pid := range alivePids(pidfile) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return svc, pidfile
}

// alivePids reads the pidfile and returns the pids in it that still exist.
func alivePids(pidfile string) []int {
	data, err := os.ReadFile(pidfile)
	if err != nil {
		return nil
	}
	var alive []int
	for _, line := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		if syscall.Kill(pid, 0) == nil {
			alive = append(alive, pid)
		}
	}
	return alive
}

// `gozellij restart shell` in one terminal and bare `gozellij` in another must leave one shell.
//
// Restart installs a fresh, never-started supervisor before stopping the old process, so that
// watchers see "not finished" - and Live() is false for that fresh supervisor, so anything asking
// "does this need starting?" during the stop was told yes. The stop takes the whole grace period
// for a shell, because bash and zsh ignore SIGTERM, so the window is five seconds wide and lands
// on the pair of commands most likely to be typed together. The second process had a pty nobody
// drained and was invisible to ls, stop and rm.
func TestRestartAndEnsureTogetherLeaveOneProcess(t *testing.T) {
	f, _ := newTestFabric(t)
	svc, pidfile := pidfileService(t, "shell", true)

	if err := f.Add(svc, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitFabric(t, f, "shell", 10*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})

	restarted := make(chan error, 1)
	go func() { restarted <- f.Restart("shell") }()

	// Let Restart install its successor and start stopping the old process; that stop takes
	// StopGrace because the process ignores SIGTERM.
	time.Sleep(200 * time.Millisecond)
	if err := f.Ensure(svc); err != nil {
		t.Fatalf("Ensure during a restart: %v", err)
	}
	if err := <-restarted; err != nil {
		t.Fatalf("Restart: %v", err)
	}

	// Wait for the survivor rather than sleeping and sampling. A fixed wait fails in both
	// directions on a loaded machine: too short and the legitimate process has not written its
	// pid yet (measured - this reported zero processes once on a box at load 11), too long and a
	// second process that is about to appear has not appeared yet.
	waitFabric(t, f, "shell", 15*time.Second, "running again", func(st Status) bool {
		return st.State == StateRunning
	})
	deadline := time.Now().Add(10 * time.Second)
	var alive []int
	for {
		alive = alivePids(pidfile)
		if len(alive) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			st, _ := f.Status("shell")
			t.Fatalf("no process alive after restart+ensure; the fabric says pid %d, state %v", st.Pid, st.State)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And then that it stays at one. The bug this guards against is a second, orphaned process,
	// which appears slightly later than the legitimate one - so a single sample taken at the
	// right moment would miss it.
	settle := time.Now().Add(2 * time.Second)
	for time.Now().Before(settle) {
		if alive = alivePids(pidfile); len(alive) > 1 {
			st, _ := f.Status("shell")
			t.Fatalf("%d processes alive after restart+ensure (%v), want 1; the fabric knows only about pid %d",
				len(alive), alive, st.Pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Two Starts at the same instant on a service that is not running must spawn one process.
//
// Nothing used to be held between reading the status and acting on it, and replaceSupervisor does
// a disk read in between, so both callers saw "not live", both installed a fresh supervisor, and
// both started theirs. Nondeterministic by nature - run it with -count to mean anything.
func TestTwoConcurrentStartsSpawnOneProcess(t *testing.T) {
	f, reg := newTestFabric(t)
	svc, pidfile := pidfileService(t, "twice", false)

	if err := f.Add(svc, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Enabled on disk already, so setEnabled's fsync'd write does not stagger the racers.
	svc.Enabled = true
	if err := reg.Put(svc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	const racers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := f.Start("twice"); err != nil {
				t.Errorf("Start: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	waitFabric(t, f, "twice", 10*time.Second, "running", func(st Status) bool {
		return st.State == StateRunning
	})
	time.Sleep(500 * time.Millisecond)

	alive := alivePids(pidfile)
	if len(alive) != 1 {
		t.Errorf("%d processes alive after %d concurrent Starts (%v), want 1", len(alive), racers, alive)
	}
}

// `rm` must not report deleting a transcript that is about to be written back.
//
// Closing the output buffer used only to close the subscriber channels; the LogSink goroutine kept
// draining megabytes of queued output with its file still open. A queued chunk that tripped a
// rotation after the
// unlink finds no file to rename and opens a new one - so the transcript's tail reappears after
// `rm` said it was gone.
func TestRemoveDoesNotLeaveTheLogBehindWhenTheSinkIsBusy(t *testing.T) {
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	logs := t.TempDir()
	// Small rotation threshold so that a queued backlog rotates several times.
	f := NewFabric(reg, StartOptions{LogDir: logs, LogBytes: 4096})
	t.Cleanup(f.Shutdown)

	if err := f.Add(Service{Name: "shell", Command: "true"}, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out, err := f.Output("shell")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	path := LogPath(logs, "shell")

	// Queue a backlog faster than the sink can write it. Well under the 8 MiB queue, so this is
	// the ordinary path and not the lagged one.
	chunk := []byte(strings.Repeat("cat ~/.ssh/id_ed25519\n", 100)) // ~2.2 KB
	for i := 0; i < 400; i++ {
		if _, err := out.Write(chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if err := f.Remove("shell", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Let the sink finish whatever it was still doing.
	time.Sleep(500 * time.Millisecond)

	for _, p := range []string{path, path + ".1"} {
		if st, err := os.Stat(p); err == nil {
			t.Errorf("%s exists after rm (%d bytes): the transcript was recreated by the log writer after it was deleted", p, st.Size())
		}
	}
}
