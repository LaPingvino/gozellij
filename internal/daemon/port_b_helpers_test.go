package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/creack/pty"

	"github.com/LaPingvino/gozellij/internal/vt/grid"
)

// Helpers for the group B ports of acceptance.sh: the rendered attach. Everything here is prefixed
// with b so it cannot collide with the other ports in this package.

// bTap is every byte the attach client wrote to its terminal, in order. Some promises are about
// what reaches the terminal rather than what the screen ends up looking like - a scrolling region
// the emulator would apply and forget, a title sent a second time that changes nothing - and those
// can only be read from the stream.
type bTap struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *bTap) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// Len is where the stream is now, so that a later look can be at what came after it.
func (b *bTap) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// Since is what was written after offset.
func (b *bTap) Since(offset int) []byte { return b.Bytes()[offset:] }

// Count is how many times s has been written so far.
func (b *bTap) Count(s string) int { return bytes.Count(b.Bytes(), []byte(s)) }

// bOpts are the ways a tapped screen can differ from attachScreen's.
type bOpts struct {
	// dropCursorReports keeps the emulator's answer to "where is the cursor" from reaching the
	// client, so that a service that gets one can only have got it from the client.
	dropCursorReports bool
}

var bCursorReport = regexp.MustCompile(`\x1b\[\d+;\d+R`)

// bAttach is attachScreen with the client's output also recorded.
func bAttach(t *testing.T, sock, service string, cols, rows int, opts AttachOptions, bo bOpts) (*testScreen, *bTap) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty: %v", err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		t.Fatalf("pty size: %v", err)
	}
	s := &testScreen{t: t, sock: sock, master: master, slave: slave, term: grid.New(cols, rows), done: make(chan error, 1)}
	tap := &bTap{}

	showing = showingName{}
	_ = terminalModes.Reset()

	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := s.master.Read(buf)
			if n > 0 {
				s.mu.Lock()
				tap.mu.Lock()
				tap.buf.Write(buf[:n])
				tap.mu.Unlock()
				_, _ = s.term.Write(buf[:n])
				replies := s.term.TakeReplies()
				s.mu.Unlock()
				if bo.dropCursorReports {
					replies = bCursorReport.ReplaceAll(replies, nil)
				}
				if len(replies) > 0 {
					_, _ = s.master.Write(replies)
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { s.done <- AttachLoopWith(sock, service, slave, slave, opts) }()
	t.Cleanup(s.close)
	return s, tap
}

// bTitle, bDir and bShape are what the terminal was told, as the emulator standing in for it holds.
func bTitle(s *testScreen) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.Title()
}

func bDir(s *testScreen) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.Dir()
}

func bShape(s *testScreen) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.Cursor().Shape
}

// bRegions is every scrolling region (DECSTBM) set in the stream, as [top, bottom], 1-based. A
// bare ESC [ r, which puts the region back to the whole screen, comes out as [0, 0].
var bDECSTBM = regexp.MustCompile(`\x1b\[(\d*);?(\d*)r`)

func bRegions(stream []byte) [][2]int {
	var out [][2]int
	for _, m := range bDECSTBM.FindAllSubmatch(stream, -1) {
		top, _ := strconv.Atoi(string(m[1]))
		bottom, _ := strconv.Atoi(string(m[2]))
		out = append(out, [2]int{top, bottom})
	}
	return out
}

// bBorrowedRegion is a region in the stream that leaves some of a rows-row screen out.
func bBorrowedRegion(stream []byte, rows int) ([2]int, bool) {
	for _, r := range bRegions(stream) {
		if r == [2]int{0, 0} {
			continue
		}
		if r[0] > 1 || (r[1] != 0 && r[1] < rows) {
			return r, true
		}
	}
	return [2]int{}, false
}

// bMaxRow is the lowest row, 1-based, that the stream moves the cursor to with an absolute move.
var bCUP = regexp.MustCompile(`\x1b\[(\d+)(?:;\d+)?H`)

func bMaxRow(stream []byte) int {
	max := 0
	for _, m := range bCUP.FindAllSubmatch(stream, -1) {
		if r, _ := strconv.Atoi(string(m[1])); r > max {
			max = r
		}
	}
	return max
}

// bStatusEvery makes the status line tick as fast as it is allowed to, for promises about
// something surviving the tick.
func bStatusEvery(t *testing.T, home, every string) {
	t.Helper()
	p := filepath.Join(home, "status.conf")
	if err := os.WriteFile(p, []byte("every="+every+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOZELLIJ_STATUS_CONFIG", p)
}

// bStderrTo points standard error at the screen for the rest of the test. The client writes some
// things - the picker's menu - to standard error, which in a real terminal is the terminal.
func bStderrTo(t *testing.T, s *testScreen) {
	old := os.Stderr
	os.Stderr = s.slave
	t.Cleanup(func() { os.Stderr = old })
}

// bRowWith is the first row containing text, or -1.
func bRowWith(s *testScreen, text string) int {
	for i, r := range s.Rows() {
		if strings.Contains(r, text) {
			return i
		}
	}
	return -1
}
