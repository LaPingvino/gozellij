package daemon

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"

	"github.com/LaPingvino/gozellij/internal/ipc"
	"github.com/LaPingvino/gozellij/internal/vt/grid"
)

// Helpers for the group-C ports of acceptance.sh. Everything here is prefixed with c so it cannot
// collide with the other groups' helpers.

// cRaw is every byte the attach client wrote to its terminal, for the promises that are about
// bytes reaching the terminal rather than about what the screen shows: modes, colour queries.
type cRaw struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *cRaw) Has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Contains(r.buf.Bytes(), []byte(s))
}

func (r *cRaw) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// cAttachRecorded is attachScreen, with the client's output also kept as raw bytes.
func cAttachRecorded(t *testing.T, sock, service string, cols, rows int, opts AttachOptions) (*testScreen, *cRaw) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty: %v", err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		t.Fatalf("pty size: %v", err)
	}
	s := &testScreen{t: t, sock: sock, master: master, slave: slave, term: grid.New(cols, rows), done: make(chan error, 1)}
	raw := &cRaw{}

	showing = showingName{}
	_ = terminalModes.Reset()

	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := s.master.Read(buf)
			if n > 0 {
				raw.mu.Lock()
				raw.buf.Write(buf[:n])
				raw.mu.Unlock()
				s.mu.Lock()
				_, _ = s.term.Write(buf[:n])
				replies := s.term.TakeReplies()
				s.mu.Unlock()
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
	return s, raw
}

// cTitle is the title the client has given the terminal.
func cTitle(s *testScreen) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.Title()
}

// cStream is a second, raw attach whose output is read continuously - a connection that is not
// drained is one the daemon is entitled to drop - and kept for looking at.
type cStream struct {
	t   *testing.T
	c   *Client
	mu  sync.Mutex
	buf bytes.Buffer
}

func cAttachStream(t *testing.T, sock, service string, req ipc.AttachRequest) *cStream {
	t.Helper()
	c, err := attachRaw(t, sock, service, req)
	if err != nil {
		t.Fatalf("raw attach to %s: %v", service, err)
	}
	st := &cStream{t: t, c: c}
	go func() {
		for {
			kind, payload, err := c.Reader().ReadFrame()
			if err != nil {
				return
			}
			if kind == ipc.KindData {
				st.mu.Lock()
				st.buf.Write(payload)
				st.mu.Unlock()
			}
		}
	}()
	return st
}

func (st *cStream) String() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.buf.String()
}

// Shows waits for text in what this connection has received.
func (st *cStream) Shows(text string) {
	st.t.Helper()
	deadline := time.Now().Add(screenWait)
	for !strings.Contains(st.String(), text) {
		if time.Now().After(deadline) {
			st.t.Fatalf("waited %s for %q on the raw attach; it received:\n%q", screenWait, text, st.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Type sends keystrokes as this connection.
func (st *cStream) Type(keys string) {
	st.t.Helper()
	if err := st.c.Writer().WriteFrame(ipc.KindData, []byte(keys)); err != nil {
		st.t.Fatalf("typing %q on the raw attach: %v", keys, err)
	}
}

// cColumnOf is the column (in cells, not bytes: the seam between panes is not ASCII) text starts at on the first row holding it, or -1.
func cColumnOf(s *testScreen, text string) int {
	for _, r := range s.Rows() {
		if i := strings.Index(r, text); i >= 0 {
			return utf8.RuneCountInString(r[:i])
		}
	}
	return -1
}
