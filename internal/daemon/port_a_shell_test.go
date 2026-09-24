package daemon

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Ported from acceptance.sh: "an interactive shell on a pty", "the shell captures YOUR term" (the
// half the daemon owns; the half the CLI owns is in cmd/gozellij/port_a_cli_test.go) and "detach
// keeps it running".

// aEnsureShell defines and starts a shell the way `gozellij shell` does: one Ensure call, a login
// shell in your home directory that closes on exit. env is what the CLI would have captured from
// the terminal.
func aEnsureShell(t *testing.T, sock, name string, env ...string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	if _, err := dial(t, sock).Ensure(name, ipc.AddRequest{
		Command:     "/bin/sh",
		Args:        []string{"-l"},
		Dir:         home,
		Env:         append(env, "GOZELLIJ="+name),
		Restart:     "no",
		CloseOnExit: true,
		AutoName:    true,
	}); err != nil {
		t.Fatalf("ensure %s: %v", name, err)
	}
}

// aCaptureStderr points os.Stderr at a pipe until the returned function is called, which restores
// it and returns what was written. The attach client writes its detach message straight to the
// process's standard error, which in a real terminal is the terminal.
func aCaptureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	var (
		buf  strings.Builder
		mu   sync.Mutex
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			mu.Lock()
			buf.Write(b[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	var once sync.Once
	finish := func() string {
		once.Do(func() {
			os.Stderr = old
			w.Close()
			<-done
			r.Close()
		})
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
	t.Cleanup(func() { finish() })
	return finish
}

func TestPortAShellIsInteractiveOnARealPts(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	aEnsureShell(t, sock, "shell")

	stderr := aCaptureStderr(t)
	s := attachScreen(t, sock, "shell", 80, 12, AttachOptions{Replay: true, Mode: RenderOff})
	// Neither answer can come from the echo of what was typed: tty prints a path the command
	// does not contain, and the marker is arithmetic only the shell does.
	s.Type("tty; case $- in *i*) echo INTER-$((6*7));; esac\r")
	s.Shows("/dev/pts/")
	s.Shows("INTER-42")

	// exit in your only shell hands the terminal back.
	s.Type("exit\r")
	if err := s.Ended(); err != nil {
		t.Fatalf("exit ended the attach with %v", err)
	}
	if said := stderr(); !strings.Contains(said, "shell exited") {
		t.Errorf("the terminal was handed back without saying why; it said %q", said)
	}
}

func TestPortAShellRunsWithTheTermItWasDefinedWith(t *testing.T) {
	screenEnv(t) // the daemon's (this process's) TERM is xterm-256color
	_, _, sock := newTestDaemon(t)
	aEnsureShell(t, sock, "shell", "TERM=xterm-portA")

	s := attachScreen(t, sock, "shell", 80, 12, AttachOptions{Replay: true, Mode: RenderOff})
	s.Type("echo MY-TERM=$TERM.\r")
	s.Shows("MY-TERM=xterm-portA.")
	if strings.Contains(s.Text(), "MY-TERM=xterm-256color") {
		t.Errorf("the shell saw the daemon's TERM:\n%s", s.Text())
	}
}

func TestPortADetachKeepsItRunning(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	aEnsureShell(t, sock, "shell")
	before, err := dial(t, sock).Status("shell")
	if err != nil || before.Pid == 0 {
		t.Fatalf("the shell is not running before the attach: %+v %v", before, err)
	}

	stderr := aCaptureStderr(t)
	s := attachScreen(t, sock, "shell", 80, 12, AttachOptions{Replay: true, Mode: RenderOff})
	s.Type("echo ALIVE-$((6*7))\r")
	s.Shows("ALIVE-42")
	s.Prefix('d')
	if err := s.Ended(); err != nil {
		t.Fatalf("the detach ended the attach with %v", err)
	}
	if said := stderr(); !strings.Contains(said, "detached from shell") {
		t.Errorf("Ctrl-] d did not say it detached; it said %q", said)
	}

	after, err := dial(t, sock).Status("shell")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" || after.Pid != before.Pid {
		t.Errorf("after a detach the shell is %s pid %d, want running pid %d", after.State, after.Pid, before.Pid)
	}
}
