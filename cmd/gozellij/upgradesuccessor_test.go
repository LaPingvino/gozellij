package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// A daemon that answers a ping and then dies is the predecessor, not the successor.
//
// This is the window that made the acceptance suite fail about one run in fifteen: the old daemon
// is still listening while it winds down - it has to be, or the reply to the upgrade request could
// not reach the client - so it accepts a connection, answers a ping, and execs itself away before
// the next request on that connection. The upgrade had worked; the command that asked for it
// reported a broken pipe.
//
// The server here does exactly that, deterministically: the first connection answers a ping and
// then hangs up, every later one answers everything.
func TestAskTheSuccessorOutlivesADaemonThatDiesMidExchange(t *testing.T) {
	var conns int32
	sock := fakeDaemon(t, func(c net.Conn, n int32) {
		r, w := ipc.NewReader(c), ipc.NewWriter(c)
		for {
			var req ipc.Request
			if err := r.ReadJSON(ipc.KindRequest, &req); err != nil {
				return
			}
			switch {
			case req.Op == ipc.OpPing:
				_ = w.WriteJSON(ipc.KindResponse,
					ipc.OKResponse(req.ID, map[string]string{"version": "the-successor"}))
			case req.Op == ipc.OpServiceList && n == 1:
				// The predecessor, going away exactly here.
				_ = c.Close()
				return
			case req.Op == ipc.OpServiceList:
				_ = w.WriteJSON(ipc.KindResponse, ipc.OKResponse(req.ID, ipc.ListReply{
					Services: []ipc.StatusReply{{Service: "kept", Pid: 4242}},
				}))
			default:
				_ = w.WriteJSON(ipc.KindResponse, ipc.OKResponse(req.ID, nil))
			}
		}
	}, &conns)

	version, list, err := askTheSuccessor(sock, 5*time.Second)
	if err != nil {
		t.Fatalf("gave up on a daemon that was there: %v", err)
	}
	if version != "the-successor" {
		t.Fatalf("version = %q", version)
	}
	if len(list.Services) != 1 || list.Services[0].Pid != 4242 {
		t.Fatalf("service list = %+v", list.Services)
	}
	if got := atomic.LoadInt32(&conns); got < 2 {
		t.Fatalf("it answered from %d connection(s), so it never retried", got)
	}
}

// Running out of time says what actually went wrong, not what the clock did.
//
// The retry loop checked the deadline and then slept, so the sleep could carry it past and the
// next attempt asked for a connection within a negative duration. That reported "the daemon did
// not come back within -50ms (last error: <nil>)" - and because it was the most recent error, it
// replaced the one that mattered. A person on the rare bad day would have been handed a negative
// duration instead of the reason.
//
// The daemon here answers a ping and then stops answering, so every attempt gets that far and no
// further, and the deadline arrives mid-exchange rather than while dialling.
func TestRunningOutOfTimeKeepsTheRealError(t *testing.T) {
	var conns int32
	sock := fakeDaemon(t, func(c net.Conn, n int32) {
		r, w := ipc.NewReader(c), ipc.NewWriter(c)
		for {
			var req ipc.Request
			if err := r.ReadJSON(ipc.KindRequest, &req); err != nil {
				return
			}
			if req.Op == ipc.OpPing {
				_ = w.WriteJSON(ipc.KindResponse,
					ipc.OKResponse(req.ID, map[string]string{"version": "whoever"}))
				continue
			}
			// Anything else: go away, the way a daemon on its way out does.
			_ = c.Close()
			return
		}
	}, &conns)

	_, _, err := askTheSuccessor(sock, 400*time.Millisecond)
	if err == nil {
		t.Fatal("a daemon that never answers a list was reported as a success")
	}
	if strings.Contains(err.Error(), "within -") {
		t.Fatalf("gave up with a negative duration in the message: %v", err)
	}
	if !strings.Contains(err.Error(), "service.list") {
		t.Fatalf("the reason was lost on the way out: %v", err)
	}
}

// And it still gives up when there is nothing there, rather than retrying until the deadline it
// was given runs out somewhere else. A retry loop that cannot fail is its own bug.
func TestAskTheSuccessorGivesUpWhenNothingComesBack(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nothing.sock")
	start := time.Now()
	if _, _, err := askTheSuccessor(sock, 600*time.Millisecond); err == nil {
		t.Fatal("it claimed to reach a daemon that does not exist")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("it took %v to give up on a socket that was never there", d)
	}
}

// fakeDaemon listens on a short path and hands each connection to handle, with the connection's
// number so a handler can behave differently on the first one.
func fakeDaemon(t *testing.T, handle func(net.Conn, int32), conns *int32) string {
	t.Helper()
	// Short, because a deeply nested TempDir blows the 108-byte sockaddr_un limit and the bind
	// error says nothing about path length.
	dir, err := os.MkdirTemp("", "gzu")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "fabric.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(c, atomic.AddInt32(conns, 1))
		}
	}()
	return sock
}
