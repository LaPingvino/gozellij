package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/LaPingvino/gozellij/internal/vt/grid"
)

// The screen harness: an attach client on a real pty, read by gozellij's own terminal emulator.
//
// This is what replaced the tmux-driven acceptance script for everything that happens on a screen.
// That script guessed how long each step took - sleep 2, then look - so it was slow when the
// machine was idle and wrong when it was busy. Here every look is a wait for a condition with a
// deadline, the terminal is internal/vt/grid (conformance-tested against tmux), and the client runs
// in the test's own process, so a test takes as long as the thing it tests.
//
// The client keeps some state per process - the terminal's modes, the name being shown - so tests
// using this must not run in parallel with each other.

// screenWait is how long a screen condition may take before a test gives up. Generous, because it is
// only ever reached on failure: a passing test waits exactly as long as the screen takes.
const screenWait = 10 * time.Second

// testScreen is one terminal with a gozellij attach running in it.
type testScreen struct {
	t      *testing.T
	sock   string
	master *os.File
	slave  *os.File

	mu   sync.Mutex
	term *grid.Term

	done chan error
}

// screenEnv gives the attach client a home, state and configuration of its own, so nothing a test
// does reaches the real ones and no test sees another's.
func screenEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GOZELLIJ_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("GOZELLIJ_STATUS_CONFIG", "")
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("TERM", "xterm-256color")
	// A fresh state directory would greet, and the greeting owns the status row for seconds.
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firstRunMarker(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// attachScreen starts `gozellij attach service` in a cols x rows terminal.
func attachScreen(t *testing.T, sock, service string, cols, rows int, opts AttachOptions) *testScreen {
	t.Helper()
	return attachScreenOver(t, sock, service, cols, rows, opts, nil)
}

// attachScreenOver is attachScreen into a terminal that already shows what before draws - a screen
// somebody has been using, with the cursor at the bottom, which is where the bugs are that a
// blank screen hides.
func attachScreenOver(t *testing.T, sock, service string, cols, rows int, opts AttachOptions, before []byte) *testScreen {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty: %v", err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		t.Fatalf("pty size: %v", err)
	}
	s := &testScreen{t: t, sock: sock, master: master, slave: slave, term: grid.New(cols, rows), done: make(chan error, 1)}
	if len(before) > 0 {
		_, _ = s.term.Write(before)
	}

	// Process-wide client state starts clean for each screen.
	showing = showingName{}
	_ = terminalModes.Reset()

	go s.read()
	go func() { s.done <- AttachLoopWith(sock, service, slave, slave, opts) }()
	t.Cleanup(s.close)
	return s
}

// read feeds what the client writes into the emulator, and answers what the client asks the
// terminal, the way a real one would.
func (s *testScreen) read() {
	buf := make([]byte, 64<<10)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
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
}

func (s *testScreen) close() {
	select {
	case <-s.done:
	default:
		s.Prefix('d')
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
	}
	s.slave.Close()
	s.master.Close()
}

// Type sends keystrokes, as typed.
func (s *testScreen) Type(keys string) {
	s.t.Helper()
	if _, err := s.master.Write([]byte(keys)); err != nil {
		s.t.Fatalf("typing %q: %v", keys, err)
	}
}

// Prefix sends the prefix key and then key: Prefix('n') is Ctrl-] n.
func (s *testScreen) Prefix(key byte) {
	_, _ = s.master.Write([]byte{0x1d})
	// The reader takes the prefix and the key as one command only if they arrive as two reads
	// no more than a moment apart, the way a person types them; as one write they are a paste.
	time.Sleep(20 * time.Millisecond)
	_, _ = s.master.Write([]byte{key})
}

// Rows is the screen as text, one string per row, trailing blanks trimmed.
func (s *testScreen) Rows() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.term.Snapshot()
	out := make([]string, len(snap))
	for r, row := range snap {
		var b strings.Builder
		for _, c := range row {
			if c.Content == "" {
				b.WriteByte(' ')
			} else {
				b.WriteString(c.Content)
			}
		}
		out[r] = strings.TrimRight(b.String(), " ")
	}
	return out
}

// Text is the whole screen as one string.
func (s *testScreen) Text() string { return strings.Join(s.Rows(), "\n") }

// Bottom is the last row: the status line.
func (s *testScreen) Bottom() string {
	r := s.Rows()
	return r[len(r)-1]
}

// Modes is the terminal's private modes as the client left them: alternate screen, mouse.
func (s *testScreen) Modes() map[int]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.term.Modes()
	if s.term.AltScreen() {
		m[1049] = true
	}
	return m
}

// Until waits for cond to hold, or fails the test saying what the screen showed.
func (s *testScreen) Until(what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(screenWait)
	for !cond() {
		if time.Now().After(deadline) {
			s.t.Fatalf("waited %s for %s; the screen shows:\n%s", screenWait, what, s.Text())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Shows waits for text to be on the screen.
func (s *testScreen) Shows(text string) {
	s.t.Helper()
	s.Until(fmt.Sprintf("%q on screen", text), func() bool { return strings.Contains(s.Text(), text) })
}

// BottomShows waits for text on the status line.
func (s *testScreen) BottomShows(text string) {
	s.t.Helper()
	s.Until(fmt.Sprintf("%q on the status line", text), func() bool { return strings.Contains(s.Bottom(), text) })
}

// Ended waits for the attach to end by itself, and returns how it ended.
func (s *testScreen) Ended() error {
	s.t.Helper()
	select {
	case err := <-s.done:
		s.done <- err // for close
		return err
	case <-time.After(screenWait):
		s.t.Fatalf("the attach did not end; the screen shows:\n%s", s.Text())
		return nil
	}
}

// StillAttached checks the attach has not ended.
func (s *testScreen) StillAttached() bool {
	select {
	case err := <-s.done:
		s.done <- err
		return false
	default:
		return true
	}
}

// Cursor is where the terminal's cursor is, zero-based.
func (s *testScreen) Cursor() (row, col int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.term.Cursor()
	return c.Row, c.Col
}

// ScrollRegion is the terminal's scrolling region, zero-based rows.
func (s *testScreen) ScrollRegion() (top, bottom int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.ScrollRegion()
}

// Resize changes the terminal's size, the way a window being dragged does.
func (s *testScreen) Resize(cols, rows int) {
	s.t.Helper()
	if err := pty.Setsize(s.master, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		s.t.Fatalf("resize: %v", err)
	}
	s.mu.Lock()
	_ = s.term.Resize(cols, rows)
	s.mu.Unlock()
	// The client hears about it the way it would from a window: SIGWINCH, to this process.
	_ = syscall.Kill(os.Getpid(), syscall.SIGWINCH)
}

// eventually waits for cond without a screen to show, for daemon-side conditions.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(screenWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", screenWait, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
