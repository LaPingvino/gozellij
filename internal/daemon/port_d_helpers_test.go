package daemon

import (
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/fabric"
	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Helpers for the group-D ports of scripts/acceptance.sh. Prefixed d so they cannot collide with
// the helpers other ports add to this package.

// dAdd defines and starts `sh -c script` under name, with the restart policy given ("" is the
// daemon's default).
func dAdd(t *testing.T, sock, name, restart, script string) {
	t.Helper()
	if _, err := dial(t, sock).Add(name, ipc.AddRequest{
		Command: "sh", Args: []string{"-c", script}, Restart: restart, Start: true,
	}); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
}

// dPid is the service's pid as the daemon reports it, 0 when not running.
func dPid(t *testing.T, sock, name string) int {
	t.Helper()
	st, err := dial(t, sock).Status(name)
	if err != nil {
		t.Fatalf("status %s: %v", name, err)
	}
	return st.Pid
}

// dExists is whether the daemon still knows a service by this name.
func dExists(t *testing.T, sock, name string) bool {
	t.Helper()
	_, err := dial(t, sock).Status(name)
	return err == nil
}

// dWithin is Until with a deadline of its own, for promises about how *soon* something happens:
// screenWait's ten seconds would let a slow path pass.
func dWithin(s *testScreen, within time.Duration, what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			s.t.Fatalf("waited %s for %s; the screen shows:\n%s", within, what, s.Text())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// dPrefix sends a configured prefix key and then key, spaced the way Prefix spaces Ctrl-].
func dPrefix(s *testScreen, prefix, key byte) {
	_, _ = s.master.Write([]byte{prefix})
	time.Sleep(20 * time.Millisecond)
	_, _ = s.master.Write([]byte{key})
}

// dMaxNum is the largest N in text of the form <prefix>N, or -1.
func dMaxNum(text, prefix string) int {
	best := -1
	for _, m := range regexp.MustCompile(regexp.QuoteMeta(prefix)+`([0-9]+)`).FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > best {
			best = n
		}
	}
	return best
}

// dRowWith is the first row containing text, and the column (0-based, in cells) it starts at.
func dRowWith(rows []string, text string) (row, col int) {
	for r, line := range rows {
		// Bytes stand for cells: every character these tests look for is ASCII.
		if i := strings.Index(line, text); i >= 0 {
			return r, i
		}
	}
	return -1, -1
}

// dRelisten starts a second server on the socket of one just closed, over the same fabric: the
// daemon being replaced while every process keeps running, as far as a client can tell.
func dRelisten(t *testing.T, fab *fabric.Fabric, sock string) *Server {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := Listen(sock, fab, quiet)
	if err != nil {
		t.Fatalf("Listen again: %v", err)
	}
	go func(srv *Server) { _ = srv.Serve() }(srv)
	t.Cleanup(func() { srv.Close() })
	return srv
}
