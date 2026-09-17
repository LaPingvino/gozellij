package daemon

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/fabric"
	"github.com/LaPingvino/gozellij/internal/ipc"
)

// loggingDaemon starts a server whose fabric writes service output to disk, over a state directory
// the caller supplies - so a test can throw the daemon away and start another one on the same
// state, which is the whole question this feature answers.
func loggingDaemon(t *testing.T, state string) (*Server, *fabric.Fabric, string) {
	t.Helper()

	sockDir, err := os.MkdirTemp("", "gzl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, SocketName)

	reg, err := fabric.NewRegistry(filepath.Join(state, "services"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	fab := fabric.NewFabric(reg, fabric.StartOptions{LogDir: filepath.Join(state, "logs")})
	fab.Load()
	t.Cleanup(fab.Shutdown)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := Listen(sock, fab, quiet)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { srv.Close() })

	return srv, fab, sock
}

// waitForLogs polls `logs` until the answer contains want.
func waitForLogs(t *testing.T, c *Client, name, want string) ipc.LogsReply {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last ipc.LogsReply
	for time.Now().Before(deadline) {
		out, err := c.Logs(name, 0)
		if err == nil {
			last = out
			if strings.Contains(string(out.Data), want) {
				return out
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waiting for %q in the logs of %s; have %q", want, name, last.Data)
	return last
}

func TestLogsSurviveTheDaemonThatCollectedThem(t *testing.T) {
	// The gap this closes: before, scrollback was a ring in the daemon's memory, so "what did
	// that build print" had no answer once the daemon was gone.
	state := t.TempDir()

	srv, fab, sock := loggingDaemon(t, state)
	c := dial(t, sock)
	if _, err := c.Add("web", ipc.AddRequest{
		Command: "sh", Args: []string{"-c", "echo pineapple-42; sleep 30"}, Start: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitForLogs(t, c, "web", "pineapple-42")

	// Now lose the daemon entirely, the way a crash or a reboot would.
	c.Close()
	srv.Close()
	fab.Shutdown()

	_, _, sock2 := loggingDaemon(t, state)
	c2 := dial(t, sock2)

	out, err := c2.Logs("web", 0)
	if err != nil {
		t.Fatalf("Logs after the daemon was replaced: %v", err)
	}
	if !strings.Contains(string(out.Data), "pineapple-42") {
		t.Errorf("logs = %q, want the output from before the daemon died", out.Data)
	}
	if out.Path == "" {
		t.Error("Path is empty; the reply should say which file it came from")
	}
}

func TestLogsOfAServiceWithLoggingOffStayInMemory(t *testing.T) {
	state := t.TempDir()
	_, _, sock := loggingDaemon(t, state)
	c := dial(t, sock)

	if _, err := c.Add("shell", ipc.AddRequest{
		Command: "sh", Args: []string{"-c", "echo topsecret; sleep 30"}, Start: true, NoLog: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out := waitForLogs(t, c, "shell", "topsecret")

	// Still answerable from the ring, but honestly labelled as not being a file.
	if out.Path != "" {
		t.Errorf("Path = %q, want empty for a service that is not logged to disk", out.Path)
	}
	if _, err := os.Stat(filepath.Join(state, "logs", "shell.log")); !os.IsNotExist(err) {
		t.Errorf("Stat = %v, want no file: -log off must mean nothing reaches the disk", err)
	}
}

func TestFollowLogsStreamsLiveOutput(t *testing.T) {
	_, _, sock := loggingDaemon(t, t.TempDir())
	c := dial(t, sock)

	if _, err := c.Add("ticker", ipc.AddRequest{
		Command: "sh", Args: []string{"-c", "while true; do echo tick-9; sleep 0.05; done"}, Start: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	follower, err := Dial(sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	var mu sync.Mutex
	var seen bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- follower.FollowLogs("ticker", 0, writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return seen.Write(p)
		}), io.Discard)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := seen.String()
		mu.Unlock()
		if strings.Contains(got, "tick-9") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("followed for five seconds and saw %q, want tick-9", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Ctrl-C is a close from another goroutine; the follow must end cleanly rather than looking
	// like a failure.
	follower.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("FollowLogs after close = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FollowLogs did not return after the connection was closed")
	}
}

// TestFollowLogsDoesNotTypeAtTheService guards a real hazard: `logs -f` is documented as read-only,
// and the daemon reuses the attach machinery to serve it. If it reused readInput too, anything a
// follower sent would land on the service's keyboard.
func TestFollowLogsDoesNotTypeAtTheService(t *testing.T) {
	_, _, sock := loggingDaemon(t, t.TempDir())
	c := dial(t, sock)

	if _, err := c.Add("cat", ipc.AddRequest{
		Command: "sh", Args: []string{"-c", "cat; echo GOT-INPUT"}, Start: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	follower, err := Dial(sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer follower.Close()

	done := make(chan error, 1)
	go func() { done <- follower.FollowLogs("cat", 0, io.Discard, io.Discard) }()

	// Give the follow time to be established, then send input on the same stream.
	time.Sleep(200 * time.Millisecond)
	if err := follower.Writer().WriteFrame(ipc.KindData, []byte("typed\n")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	out, err := c.Logs("cat", 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if strings.Contains(string(out.Data), "typed") {
		t.Errorf("logs = %q: a follower's bytes reached the service's terminal", out.Data)
	}
}

func TestLogsNamesAServiceItDoesNotKnow(t *testing.T) {
	_, _, sock := loggingDaemon(t, t.TempDir())
	_, err := dial(t, sock).Logs("ghost", 0)
	if err == nil {
		t.Fatal("Logs of an unknown service succeeded")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error does not name the service: %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
