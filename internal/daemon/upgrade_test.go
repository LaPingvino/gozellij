package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/fabric"
	"github.com/LaPingvino/gozellij/internal/ipc"
	"github.com/creack/pty"
)

func TestManifestRoundTrip(t *testing.T) {
	m := Manifest{
		Version: ManifestVersion,
		FromPid: 4242,
		Processes: []fabric.Handover{
			{Name: "web", Pid: 11, PTYFd: 7, StartedAt: time.Now().UTC().Truncate(time.Second)},
		},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	t.Setenv(HandoverEnv, string(b))

	got, err := TakeHandover()
	if err != nil {
		t.Fatalf("TakeHandover: %v", err)
	}
	if got == nil {
		t.Fatal("TakeHandover returned nothing for a manifest that was there")
	}
	if got.FromPid != 4242 || len(got.Processes) != 1 || got.Processes[0].Name != "web" {
		t.Errorf("manifest did not survive: %+v", got)
	}

	// It must be consumed, so a later restart of this same process cannot adopt the same
	// descriptors a second time - which would attach two supervisors to one pty.
	if v, ok := os.LookupEnv(HandoverEnv); ok {
		t.Errorf("the handover is still in the environment after being taken: %q", v)
	}
}

func TestNoHandoverIsNotAnError(t *testing.T) {
	os.Unsetenv(HandoverEnv)
	m, err := TakeHandover()
	if err != nil {
		t.Fatalf("TakeHandover with no handover = %v, want nil", err)
	}
	if m != nil {
		t.Errorf("got a manifest from nowhere: %+v", m)
	}
}

// A manifest from a version we do not speak must be refused, not guessed at: adopting descriptors
// with the wrong field meanings would attach services to the wrong ptys.
func TestManifestFromAnotherVersionIsRefused(t *testing.T) {
	b, _ := json.Marshal(Manifest{Version: ManifestVersion + 99, FromPid: 1})
	t.Setenv(HandoverEnv, string(b))

	_, err := TakeHandover()
	if err == nil {
		t.Fatal("a manifest from an unknown version was accepted")
	}
	if !strings.Contains(err.Error(), "refusing to adopt") {
		t.Errorf("error should say it is refusing, got: %v", err)
	}
}

func TestUnreadableManifestIsReported(t *testing.T) {
	t.Setenv(HandoverEnv, "{not json")
	_, err := TakeHandover()
	if err == nil {
		t.Fatal("an unreadable manifest was accepted")
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("error = %v, want it to say the manifest is unreadable", err)
	}
}

// FD_CLOEXEC is the whole trick: Go sets it on everything, so a descriptor we mean to keep has to
// be opted back in explicitly. If this stops working, an upgrade silently loses every process.
func TestClearCloexecActuallyClearsIt(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "fd")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	fd := int(f.Fd())

	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatalf("F_GETFD: %v", errno)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Skip("this descriptor was not close-on-exec to begin with; nothing to prove")
	}

	if err := clearCloexec(fd); err != nil {
		t.Fatalf("clearCloexec: %v", err)
	}
	flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatalf("F_GETFD after clear: %v", errno)
	}
	if flags&syscall.FD_CLOEXEC != 0 {
		t.Error("FD_CLOEXEC is still set; the descriptor would not survive an exec")
	}
}

// Adopting a process that is not there must fail rather than produce a supervisor confidently
// reporting a service that does not exist.
func TestAdoptingAGhostIsRefused(t *testing.T) {
	svc := fabric.Service{Name: "ghost", Command: "sleep"}

	// A pid that has certainly gone: spawn, wait for it, then try to adopt it.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running true: %v", err)
	}
	dead := cmd.Process.Pid

	_, err := fabric.Adopt(svc, dead, 0, time.Now(), fabric.StartOptions{})
	if err == nil {
		t.Fatal("adopting a dead pid succeeded")
	}
	if !strings.Contains(err.Error(), "not there") {
		t.Errorf("error = %v, want it to say the process is not there", err)
	}

	if _, err := fabric.Adopt(svc, -1, 0, time.Now(), fabric.StartOptions{}); err == nil {
		t.Error("adopting pid -1 succeeded")
	}
	if _, err := fabric.Adopt(svc, os.Getpid(), -1, time.Now(), fabric.StartOptions{}); err == nil {
		t.Error("adopting with an invalid descriptor succeeded")
	}
}

// The real thing, in miniature: a child on a pty, adopted by a Process that did not spawn it, and
// still readable and waitable afterwards. This is what survives an exec.
func TestAdoptTakesOverALiveProcessAndItsPty(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo ADOPTED_OUTPUT; sleep 30")
	f, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty.Start: %v", err)
	}
	pid := cmd.Process.Pid
	started := time.Now()

	// Hand the descriptor over without closing it, exactly as the exec does.
	svc := fabric.Service{Name: "adopted", Command: "sh", Args: []string{"-c", "echo ADOPTED_OUTPUT; sleep 30"}}
	p, err := fabric.Adopt(svc, pid, int(f.Fd()), started, fabric.StartOptions{})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer p.Close()

	if p.Pid() != pid {
		t.Errorf("Pid = %d, want the adopted pid %d", p.Pid(), pid)
	}
	if !p.Adopted() {
		t.Error("Adopted() is false for an adopted process")
	}

	// Its output reaches us, which means the pty really is the same one.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := p.Output.Snapshot()
		if strings.Contains(string(snap), "ADOPTED_OUTPUT") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap, _ := p.Output.Snapshot()
	if !strings.Contains(string(snap), "ADOPTED_OUTPUT") {
		t.Fatalf("no output from the adopted process; got %q", snap)
	}

	// And we can still end it and learn how it went, without an exec.Cmd to do it with.
	exit := p.Stop()
	if exit.Signal == "" {
		t.Errorf("exit = %+v, want the terminating signal recorded", exit)
	}
	if _, done := p.Exited(); !done {
		t.Error("the adopted process is still running after Stop")
	}
}

// Handovers must describe only what is actually running: a stopped service has nothing to hand
// over, and inventing an entry for it would make the successor adopt a pid that is not there.
func TestHandoversCoverOnlyRunningServices(t *testing.T) {
	_, fab, sock := newTestDaemon(t)
	c := dial(t, sock)

	if _, err := c.Add("running", ipcAdd("sleep", "300", true)); err != nil {
		t.Fatalf("Add running: %v", err)
	}
	if _, err := c.Add("idle", ipcAdd("sleep", "300", false)); err != nil {
		t.Fatalf("Add idle: %v", err)
	}
	waitRunning(t, fab, "running", 15*time.Second)

	hs := fab.Handovers()
	if len(hs) != 1 {
		t.Fatalf("Handovers = %+v, want exactly the running service", hs)
	}
	if hs[0].Name != "running" {
		t.Errorf("handover names %q, want running", hs[0].Name)
	}
	if hs[0].Pid == 0 || hs[0].PTYFd < 0 {
		t.Errorf("handover has no usable pid/fd: %+v", hs[0])
	}

	m, problems := PrepareHandover(fab)
	if len(problems) != 0 {
		t.Errorf("PrepareHandover reported problems: %v", problems)
	}
	if m.Version != ManifestVersion {
		t.Errorf("manifest version = %d, want %d", m.Version, ManifestVersion)
	}
	if m.FromPid != os.Getpid() {
		t.Errorf("FromPid = %d, want this process %d", m.FromPid, os.Getpid())
	}
	if len(m.Processes) != 1 {
		t.Errorf("manifest covers %d processes, want 1", len(m.Processes))
	}
}

// ipcAdd is a small helper so the table above reads as intent rather than struct literals.
func ipcAdd(command, arg string, start bool) ipc.AddRequest {
	return ipc.AddRequest{Command: command, Args: []string{arg}, Start: start}
}

// A failed exec must be reported, not attempted blindly. syscall.Exec does not return on success,
// so anything wrong with the target has to be caught while there is still a program able to
// complain - and the daemon deliberately does not close its socket first, so that a refusal here
// leaves it serving exactly as before rather than turning a failed upgrade into an outage.
func TestExecSelfRefusesABinaryItCannotRun(t *testing.T) {
	m := Manifest{Version: ManifestVersion, FromPid: os.Getpid()}

	missing := t.TempDir() + "/not-here"
	err := ExecSelf(m, missing)
	if err == nil {
		t.Fatal("ExecSelf accepted a binary that does not exist (and would have exec'd it)")
	}
	if !strings.Contains(err.Error(), "not there") {
		t.Errorf("error = %v, want it to say the binary is not there", err)
	}

	// A file that exists but is not executable.
	notExec := t.TempDir() + "/plain"
	if werr := os.WriteFile(notExec, []byte("not a binary"), 0o644); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	if err := ExecSelf(m, notExec); err == nil {
		t.Error("ExecSelf accepted a non-executable file")
	} else if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("error = %v, want it to say the file is not executable", err)
	}

	// A directory.
	if err := ExecSelf(m, t.TempDir()); err == nil {
		t.Error("ExecSelf accepted a directory")
	}
}
