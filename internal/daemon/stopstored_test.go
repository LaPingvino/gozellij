package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/fabric"
)

func listenForTest(t *testing.T) (*Server, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "gzs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, SocketName)
	reg, err := fabric.NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatal(err)
	}
	fab := fabric.NewFabric(reg, fabric.StartOptions{})
	t.Cleanup(fab.Shutdown)
	srv, err := Listen(sock, fab, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve()
	// Let Serve get into Accept, which is where it was stuck.
	time.Sleep(100 * time.Millisecond)
	return srv, sock
}

func closeWithin(t *testing.T, srv *Server, within time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("Close did not return within %s: the daemon hangs on SIGTERM and systemd has to kill it", within)
	}
}

// A daemon that stored its own listening socket with systemd hung in shutdown: storing it left the
// socket blocking, so Accept sat in accept(2) where closing the listener could not reach it. The
// first daemon after every fresh start is that daemon.
func TestADaemonThatStoredItsSocketStillStops(t *testing.T) {
	startFakeSystemd(t)
	srv, sock := listenForTest(t)
	if !srv.held {
		t.Fatal("with a systemd to hand it to, the socket was not recorded as held")
	}
	closeWithin(t, srv, 3*time.Second)
	// And its path stays, because the next daemon will be handed this very socket.
	if _, err := os.Stat(sock); err != nil {
		t.Errorf("the path of a socket systemd is holding was removed: %v", err)
	}
}

// Without systemd nothing holds the socket, and stopping removes it as it always did.
func TestWithoutSystemdStoppingRemovesTheSocket(t *testing.T) {
	t.Setenv(NotifyEnv, "")
	srv, sock := listenForTest(t)
	closeWithin(t, srv, 3*time.Second)
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("the socket is still there after a stop with nothing holding it: %v", err)
	}
}
