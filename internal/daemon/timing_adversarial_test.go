package daemon

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Adversarial review of 5b16421. Measures, does not read.
//
// Slow measurements are gated on GOZELLIJ_ADVERSARIAL_SLOW=1 so `go test ./...` stays fast.

// acceptNeverAnswer is a daemon that accepts and never answers.
func acceptNeverAnswer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	return sock
}

// The status line's own query is bounded at 1.5s now. Is the service switch, which runs
// serviceNames -> List on the MAIN loop for Ctrl-] n/p/l?
func TestAdversarialServiceSwitchIsBounded(t *testing.T) {
	if os.Getenv("GOZELLIJ_ADVERSARIAL_SLOW") == "" {
		t.Skip("set GOZELLIJ_ADVERSARIAL_SLOW=1: this takes as long as the bound it measures")
	}
	sock := acceptNeverAnswer(t)
	start := time.Now()
	_, err := neighbourService(sock, "web", true)
	took := time.Since(start)
	t.Logf("neighbourService against a wedged daemon took %v (err=%v); statusQueryTimeout=%v CallTimeout=%v",
		took, err, statusQueryTimeout, CallTimeout)
	if took > statusQueryTimeout+time.Second {
		t.Errorf("a service switch against a wedged daemon takes %v; the status query is bounded at %v but the switch's own list is not", took, statusQueryTimeout)
	}
}

// Dial happens BEFORE any deadline is set. DialTimeout is 10s. A unix socket whose listen backlog
// is full makes connect() wait. Measure what StatusContext does then.
func TestAdversarialDialBeforeDeadline(t *testing.T) {
	if os.Getenv("GOZELLIJ_ADVERSARIAL_SLOW") == "" {
		t.Skip("set GOZELLIJ_ADVERSARIAL_SLOW=1")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "d.sock")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: sock}); err != nil {
		t.Fatal(err)
	}
	// A backlog of zero and nobody calling accept.
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	// Fill whatever the kernel rounds the backlog up to, with short dials.
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < 64; i++ {
		c, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
		if err != nil {
			t.Logf("backlog filled after %d connections: %v", i, err)
			break
		}
		held = append(held, c)
	}

	start := time.Now()
	_ = StatusContext(sock, "web")
	took := time.Since(start)
	t.Logf("StatusContext against a socket with a full backlog took %v (DialTimeout=%v)", took, DialTimeout)
	if took > statusQueryTimeout+time.Second {
		t.Errorf("one paint took %v; the claimed bound is %v, but Dial runs before any deadline", took, statusQueryTimeout)
	}
}

// After a timed-out Call, is the deadline cleared and is the client still usable for a Close,
// and does a subsequent Call on the SAME client get a sane error rather than a hang?
func TestAdversarialTimedOutCallLeavesClientSane(t *testing.T) {
	sock := acceptNeverAnswer(t)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	_, err = c.ListWithin(300 * time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	t.Logf("ListWithin: %v after %v", err, time.Since(start))
	// Deadline must be cleared: a second bounded call must wait its OWN bound, not fail instantly.
	start = time.Now()
	_, err = c.ListWithin(300 * time.Millisecond)
	took := time.Since(start)
	t.Logf("second ListWithin: %v after %v", err, took)
	if took < 250*time.Millisecond {
		t.Errorf("second call returned after %v: the expired deadline from the first call was not cleared", took)
	}
}
