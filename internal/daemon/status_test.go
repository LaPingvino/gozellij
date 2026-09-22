package daemon

// Adversarial tests for the VIEWERS / LOG columns added in 683f55c. Written independently of
// attach_test.go. newTestDaemon builds its fabric with no LogDir, so nothing in the author's tests
// exercises statusReply's LogPath/LogBytes against a real file; this file makes its own daemon
// with logging on.

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/fabric"
	"github.com/LaPingvino/gozellij/internal/ipc"
)

func newLoggedTestDaemon(t *testing.T) (*Server, *fabric.Fabric, string, string) {
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
	logDir := t.TempDir()
	fab := fabric.NewFabric(reg, fabric.StartOptions{LogDir: logDir})
	t.Cleanup(fab.Shutdown)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := Listen(sock, fab, quiet)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { srv.Close() })
	return srv, fab, sock, logDir
}

// A terminal running `logs -f` is watching the service, but followLogs never calls
// Server.watching, so ls says nobody is there.
func TestAFollowerIsCountedAsAViewer(t *testing.T) {
	_, _, sock, _ := newLoggedTestDaemon(t)
	c := dial(t, sock)

	if _, err := c.Add("tailed", ipc.AddRequest{
		Command: "sh", Args: []string{"-c", "sleep 30"}, Start: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	follower, err := Dial(sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- follower.FollowLogs("tailed", 0, io.Discard, io.Discard) }()
	t.Cleanup(func() {
		follower.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the follower did not end after its connection was closed")
		}
	})

	// Give the daemon time to accept the follow. There is no positive signal for a follower's
	// arrival other than the daemon's own count, which is what is under test, so poll for the
	// count that *should* appear and report what appeared instead.
	deadline := time.Now().Add(2 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		st, err := c.Status("tailed")
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		got = st.Viewers
		if got == 1 {
			// And counted as one that cannot type. Not a policy: a follower has no way to send
			// anything, so it is what `attach -r` asks to be, arrived at from the other
			// direction. Without this, `ls` says somebody could be typing into a service when
			// what is actually attached is a log tail.
			if st.Watchers != 1 {
				t.Errorf("Watchers = %d while a `logs -f` client is following, want 1 - a follower cannot type",
					st.Watchers)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Viewers = %d while a `logs -f` client is following, want 1", got)
}

// A read-only attach is counted apart from one that can type, which is the whole point of counting
// it: "two terminals attached" and "two terminals that can restart your build by leaning on the
// keyboard" are different facts.
func TestAReadOnlyAttachIsCountedApartFromOneThatCanType(t *testing.T) {
	_, _, sock, _ := newLoggedTestDaemon(t)
	c := dial(t, sock)

	if _, err := c.Add("watchme", ipc.AddRequest{
		Command: "sh", Args: []string{"-c", "sleep 30"}, Start: true,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	open := func(readOnly bool) *Client {
		a, err := Dial(sock)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		a.SetReadOnly(readOnly)
		if _, err := a.Call(ipc.OpAttach, "watchme", ipc.AttachRequest{Cols: 80, Rows: 24, ReadOnly: readOnly}); err != nil {
			t.Fatalf("attach(readOnly=%v): %v", readOnly, err)
		}
		t.Cleanup(func() { a.Close() })
		return a
	}
	open(true)
	open(false)

	deadline := time.Now().Add(2 * time.Second)
	var v, w int
	for time.Now().Before(deadline) {
		st, err := c.Status("watchme")
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		v, w = st.Viewers, st.Watchers
		if v == 2 && w == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("with one read-only attach and one ordinary one: Viewers = %d, Watchers = %d, want 2 and 1", v, w)
}

// Fabric.Logs refuses to answer from a file when the service has logging off, naming exactly the
// `rm -keep-logs` then `add -log off` sequence: the file is the previous service's transcript.
// statusReply has no such guard, so `status` and `ls` label the leftover as this service's log.
func TestStatusDoesNotAttributeALeftoverLogToAServiceWithLoggingOff(t *testing.T) {
	_, _, sock, logDir := newLoggedTestDaemon(t)
	c := dial(t, sock)

	// A logged service. Its log file exists the moment the sink opens (it writes a header).
	if _, err := c.Add("x", ipc.AddRequest{Command: "sh", Args: []string{"-c", "echo hello; sleep 30"}, Start: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	path := filepath.Join(logDir, "x.log")
	waitFor(t, func() bool { fi, err := os.Stat(path); return err == nil && fi.Size() > 0 })

	if _, err := c.Remove("x", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Redefine it with logging off.
	if _, err := c.Add("x", ipc.AddRequest{Command: "sh", Args: []string{"-c", "sleep 30"}, NoLog: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The daemon's own logs answer refuses the file for this service...
	if lr, err := c.Logs("x", 0); err != nil {
		t.Fatalf("Logs: %v", err)
	} else if lr.Path != "" {
		t.Errorf("Logs.Path = %q for a service with logging off (the Logs guard is supposed to prevent this)", lr.Path)
	}

	// ...while status hands the same file over as this service's log.
	st, err := c.Status("x")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.LogPath != "" || st.LogBytes != 0 {
		t.Errorf("Status reports log %q (%d bytes) for a service with logging off; that file is the previous definition's transcript", st.LogPath, st.LogBytes)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// attach returns without sub.Detach() when the OK response or the replay cannot be written
// (attach.go:75-83); followLogs detaches on the same paths. A client that hangs up right after
// asking leaves a subscriber in the buffer that copies every chunk until it lags at 4 MiB.
//
// Best effort: whether the daemon's write beats the client's close is a race, so this sends the
// request and closes at once, many times, and reports if any subscriber was left behind.
func TestAClientThatHangsUpDuringTheHandshakeLeavesNoSubscriber(t *testing.T) {
	_, fab, sock, _ := newLoggedTestDaemon(t)
	c := dial(t, sock)

	if _, err := c.Add("quiet", ipc.AddRequest{Command: "sh", Args: []string{"-c", "sleep 30"}, Start: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	out, err := fab.Output("quiet")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	base := out.Subscribers() // the log sink

	for i := 0; i < 200; i++ {
		a, err := Dial(sock)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		req := ipc.Request{ID: 1, Op: ipc.OpAttach, Service: "quiet"}
		if err := a.Writer().WriteJSON(ipc.KindRequest, req); err != nil {
			t.Fatalf("write: %v", err)
		}
		a.Close()
	}

	// Wait for the subscriber count to come back to the baseline, rather than for the viewer
	// count to be zero. Zero viewers is true before any handler has started as well as after
	// they have all finished, so the original wait was satisfied instantly and the check then
	// read a number while two hundred handlers were still in flight - 13 of them under -race.
	//
	// A leak does not settle, so a bounded wait distinguishes the two: the point is whether the
	// count returns, not how fast.
	deadline := time.Now().Add(10 * time.Second)
	var n int
	for {
		n = out.Subscribers()
		if n == base {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("%d subscriber(s) left on the buffer after every attach ended (baseline %d): "+
		"a failed handshake in attach() returns without sub.Detach()", n-base, base)
}
