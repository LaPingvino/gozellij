package daemon

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/fabric"
	"github.com/LaPingvino/gozellij/internal/ipc"
)

// newTestDaemon starts a server on a short socket path. Short matters: t.TempDir() nests deeply
// enough on some systems to blow the 108 byte sockaddr_un limit, and the resulting bind error says
// nothing about path length.
func newTestDaemon(t *testing.T) (*Server, *fabric.Fabric, string) {
	t.Helper()

	sockDir, err := os.MkdirTemp("", "gzd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, SocketName)

	reg, err := fabric.NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	fab := fabric.NewFabric(reg, fabric.StartOptions{})
	t.Cleanup(fab.Shutdown)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := Listen(sock, fab, quiet)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		if err := srv.Serve(); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() { srv.Close() })

	return srv, fab, sock
}

func dial(t *testing.T, sock string) *Client {
	t.Helper()
	c, err := Dial(sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestPing(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	if err := dial(t, sock).Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestSocketIsNotReadableByOthers(t *testing.T) {
	// The socket controls every process the fabric runs.
	_, _, sock := newTestDaemon(t)
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("socket mode = %04o, want no group/other access", mode)
	}
	dirInfo, err := os.Stat(filepath.Dir(sock))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("socket directory mode = %04o, want no group/other access", mode)
	}
}

func TestAddListStartStopRemove(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)

	st, err := c.Add("web", ipc.AddRequest{
		Command: "sleep", Args: []string{"300"}, Restart: "always", Start: true,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if st.Service != "web" {
		t.Errorf("Service = %q, want web", st.Service)
	}
	if !st.Enabled {
		t.Error("a service added with Start was not reported as enabled")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err = c.Status("web")
		if err == nil && st.State == "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.State != "running" {
		t.Fatalf("State = %q, want running (status: %+v)", st.State, st)
	}
	if st.Pid == 0 {
		t.Error("running but no pid reported")
	}
	if st.Command != "sleep" {
		t.Errorf("Command = %q, want sleep - the definition should reach the wire", st.Command)
	}

	list, err := c.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Services) != 1 || list.Services[0].Service != "web" {
		t.Errorf("List = %+v, want just web", list.Services)
	}

	if _, err := c.Stop("web"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, _ = c.Status("web")
	if st.Enabled {
		t.Error("a stopped service is still marked enabled")
	}

	if _, err := c.Remove("web", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if list, err = c.List(); err != nil {
		t.Fatalf("List after Remove: %v", err)
	}
	if len(list.Services) != 0 {
		t.Errorf("after Remove, List = %+v, want empty", list.Services)
	}
}

// Failures must come back as failures, carrying the daemon's own words.
func TestErrorsReachTheClientIntact(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)

	_, err := c.Status("ghost")
	if err == nil {
		t.Fatal("Status of an unknown service succeeded")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error does not name the service: %v", err)
	}

	_, err = c.Add("bad name with spaces", ipc.AddRequest{Command: "sleep"})
	if err == nil {
		t.Fatal("Add accepted an invalid service name")
	}
	if !strings.Contains(err.Error(), "invalid service name") {
		t.Errorf("error should explain the naming rule, got: %v", err)
	}

	_, err = c.Add("nocmd", ipc.AddRequest{Command: ""})
	if err == nil {
		t.Fatal("Add accepted a service with no command")
	}
}

// An operation this daemon does not know must be refused by name, so a newer client learns it is
// talking to an older daemon rather than merely that something went wrong.
func TestUnknownOpIsNamedInTheError(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)

	resp, err := c.Call(ipc.Op("service.teleport"), "web", nil)
	if err == nil {
		t.Fatal("an unknown op succeeded")
	}
	if !strings.Contains(resp.Error, "service.teleport") {
		t.Errorf("error does not name the operation: %q", resp.Error)
	}
	if !strings.Contains(resp.Error, "older") {
		t.Errorf("error should hint at a version mismatch, got: %q", resp.Error)
	}
}

// Resize only means something inside an attach, and asking for it outside one must be refused with
// a reason rather than quietly accepted.
func TestResizeOutsideAnAttachIsRefused(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)

	resp, err := c.Call(ipc.OpResize, "web", ipc.ResizeRequest{Cols: 80, Rows: 24})
	if err == nil {
		t.Fatal("resize outside an attach was accepted")
	}
	if !strings.Contains(resp.Error, "attached") {
		t.Errorf("error = %q, want it to explain that resize needs an attach", resp.Error)
	}
}

// Every request gets exactly one response, and the ids line up. A daemon that answers only the
// requests it likes leaves a client on a timeout.
func TestEveryRequestGetsExactlyOneAnswer(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)

	for i := 0; i < 20; i++ {
		// Alternate between something that works and something that fails.
		var err error
		if i%2 == 0 {
			err = c.Ping()
		} else {
			_, err = c.Status("nope")
		}
		if i%2 == 0 && err != nil {
			t.Fatalf("ping %d failed: %v", i, err)
		}
		if i%2 == 1 && err == nil {
			t.Fatalf("status of a missing service %d succeeded", i)
		}
	}
	// If any response had been missing or misnumbered, Call would have reported the stream out
	// of step by now.
}

func TestMalformedRequestGetsAnAnswerNotASilentDrop(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	w := ipc.NewWriter(conn)
	if err := w.WriteFrame(ipc.KindRequest, []byte("{this is not json")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var resp ipc.Response
	if err := ipc.NewReader(conn).ReadJSON(ipc.KindResponse, &resp); err != nil {
		t.Fatalf("no answer to a malformed request: %v", err)
	}
	if resp.OK {
		t.Error("a malformed request was reported as successful")
	}
	if !strings.Contains(resp.Error, "malformed") {
		t.Errorf("error = %q, want it to say the request was malformed", resp.Error)
	}
}

// A data frame outside an attach is a client bug, and must be said out loud.
func TestUnexpectedDataFrameIsRefusedNotIgnored(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if err := ipc.NewWriter(conn).WriteFrame(ipc.KindData, []byte("keystrokes")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var resp ipc.Response
	if err := ipc.NewReader(conn).ReadJSON(ipc.KindResponse, &resp); err != nil {
		t.Fatalf("no answer to an out-of-place data frame: %v", err)
	}
	if resp.OK || !strings.Contains(resp.Error, "not attached") {
		t.Errorf("response = %+v, want a refusal mentioning the connection is not attached", resp)
	}
}

// Two daemons on one socket would leave services split between them with nobody the wiser.
func TestSecondDaemonOnTheSameSocketIsRefused(t *testing.T) {
	_, _, sock := newTestDaemon(t)

	reg, _ := fabric.NewRegistry(filepath.Join(t.TempDir(), "services2"))
	fab2 := fabric.NewFabric(reg, fabric.StartOptions{})
	defer fab2.Shutdown()

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := Listen(sock, fab2, quiet)
	if !errors.Is(err, ErrDaemonRunning) {
		t.Fatalf("second Listen = %v, want ErrDaemonRunning", err)
	}
	if !strings.Contains(err.Error(), sock) {
		t.Errorf("error does not name the socket: %v", err)
	}
}

// A socket left behind by a daemon that died must not block the next start.
func TestStaleSocketIsCleanedUp(t *testing.T) {
	dir, err := os.MkdirTemp("", "gzstale")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, SocketName)

	// Bind and abandon: close the listener's file without unlinking, the way a killed daemon
	// leaves things.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	ln.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("precondition: the stale socket should still exist: %v", err)
	}

	reg, _ := fabric.NewRegistry(filepath.Join(t.TempDir(), "services"))
	fab := fabric.NewFabric(reg, fabric.StartOptions{})
	defer fab.Shutdown()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := Listen(sock, fab, quiet)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	defer srv.Close()
	go srv.Serve()

	if err := dial(t, sock).Ping(); err != nil {
		t.Errorf("Ping after reclaiming a stale socket: %v", err)
	}
}

func TestCloseRemovesTheSocketAndIsIdempotent(t *testing.T) {
	srv, _, sock := newTestDaemon(t)
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket still present after Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// Closing the daemon must not take the services down: a daemon restarting to pick up a new binary
// is exactly the case this project exists for.
func TestClosingTheServerLeavesTheFabricAlone(t *testing.T) {
	srv, fab, sock := newTestDaemon(t)
	c := dial(t, sock)
	if _, err := c.Add("survivor", ipc.AddRequest{Command: "sleep", Args: []string{"300"}, Start: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := fab.Status("survivor"); err == nil && st.State == fabric.StateRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	before, err := fab.Status("survivor")
	if err != nil || before.Pid == 0 {
		t.Fatalf("precondition: service should be running, got %+v (%v)", before, err)
	}

	srv.Close()

	after, err := fab.Status("survivor")
	if err != nil {
		t.Fatalf("Status after server close: %v", err)
	}
	if after.Pid != before.Pid {
		t.Errorf("pid changed from %d to %d when the server closed; the process should not have noticed",
			before.Pid, after.Pid)
	}
}

func TestDialWithNoDaemonSaysWhatToDo(t *testing.T) {
	dir, err := os.MkdirTemp("", "gznone")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)

	_, err = Dial(filepath.Join(dir, SocketName))
	if !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("Dial = %v, want ErrNoDaemon", err)
	}
	if !strings.Contains(err.Error(), "gozellijd") {
		t.Errorf("error should say how to start one, got: %v", err)
	}
}

// A path too long for sockaddr_un must be refused with an error that says so, not with a bare
// EINVAL from bind().
func TestOverlongSocketPathIsExplained(t *testing.T) {
	long := filepath.Join("/tmp", strings.Repeat("d", 120), SocketName)
	err := CheckSocketPath(long)
	if err == nil {
		t.Fatal("an overlong socket path was accepted")
	}
	if !strings.Contains(err.Error(), "too long") || !strings.Contains(err.Error(), "GOZELLIJ_RUNTIME_DIR") {
		t.Errorf("error should explain the limit and the way out, got: %v", err)
	}
}

func TestManyConcurrentClients(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	done := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() {
			c, err := Dial(sock)
			if err != nil {
				done <- err
				return
			}
			defer c.Close()
			for j := 0; j < 10; j++ {
				if err := c.Ping(); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	for i := 0; i < 16; i++ {
		if err := <-done; err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
	}
}
