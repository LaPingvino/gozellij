package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// StatusContext claims to bound its query at statusQueryTimeout. Call() sets its own deadline
// on the same connection after that, so the bound may not be the one that applies.
func TestTheStatusQueryIsBoundedAsClaimed(t *testing.T) {
	dir, err := os.MkdirTemp("", "gz-adv")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			// A daemon that accepts and never answers.
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

	start := time.Now()
	_ = StatusContext(sock, "web")
	took := time.Since(start)
	t.Logf("StatusContext took %v (statusQueryTimeout=%v, CallTimeout=%v)", took, statusQueryTimeout, CallTimeout)
	if took > statusQueryTimeout+time.Second {
		t.Errorf("status query took %v, claimed bound is %v: Call() overrides the deadline with CallTimeout", took, statusQueryTimeout)
	}
}
