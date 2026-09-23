package daemon

import (
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Once the ring has wrapped, a replay began wherever the dropping stopped - here, three bytes into
// an escape sequence, so a terminal printed "[0m" as text at the top of its scrollback on every
// attach. It starts at a line now.
func TestAReplayAfterTheRingWrapsStartsAtALine(t *testing.T) {
	_, _, sock := newTestDaemon(t)
	c := dial(t, sock)
	// Twenty-byte lines, 400000 bytes: the 256 KiB kept then begin 16 bytes into a line, which is
	// the "[0m" of its closing sequence. -onlcr keeps "\n" one byte, so the arithmetic holds.
	script := `stty -onlcr; awk 'BEGIN{for(i=0;i<20000;i++) printf "\033[31mABCDEFGHIJ\033[0m\n"}'; sleep 300`
	if _, err := c.Add("wrapped", ipc.AddRequest{Command: "sh", Args: []string{"-c", script}, Start: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := dial(t, sock).Logs("wrapped", 0)
		if err == nil && len(out.Data) >= 256<<10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the service never wrote enough to wrap the ring")
		}
		time.Sleep(100 * time.Millisecond)
	}

	a, err := attachRaw(t, sock, "wrapped", ipc.AttachRequest{Replay: true})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	_ = a.Conn().SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		kind, payload, err := a.Reader().ReadFrame()
		if err != nil {
			t.Fatalf("no replay arrived: %v", err)
		}
		if kind != ipc.KindData || len(payload) == 0 {
			continue
		}
		if payload[0] != 0x1b {
			n := len(payload)
			if n > 12 {
				n = 12
			}
			t.Errorf("the replay starts mid-line: %q", payload[:n])
		}
		return
	}
}
