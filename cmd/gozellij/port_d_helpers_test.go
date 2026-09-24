package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/LaPingvino/gozellij/internal/daemon"
	"github.com/LaPingvino/gozellij/internal/fabric"
	"github.com/LaPingvino/gozellij/internal/ipc"
	"github.com/LaPingvino/gozellij/internal/vt/grid"
)

// Helpers for the group-D ports of scripts/acceptance.sh that are about the command line: a daemon
// of the test's own, and the command's output captured. Prefixed d so they cannot collide with
// helpers other ports add to this package.

// dEnv isolates home, state and configuration, and says this is not inside a gozellij service.
func dEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GOZELLIJ_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("GOZELLIJ_STATUS_CONFIG", "")
	t.Setenv("GOZELLIJ_RENDER", "")
	t.Setenv("GOZELLIJ", "")
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("TERM", "xterm-256color")
	// Seen the keys already: a first attach greets, and the greeting owns the status row.
	daemon.FirstAttach()
	return home
}

// dStartDaemon runs a daemon on a socket of the test's own, logging to logDir.
func dStartDaemon(t *testing.T) (sock, logDir string) {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "gzd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock = filepath.Join(sockDir, daemon.SocketName)
	reg, err := fabric.NewRegistry(filepath.Join(t.TempDir(), "services"))
	if err != nil {
		t.Fatal(err)
	}
	logDir = t.TempDir()
	fab := fabric.NewFabric(reg, fabric.StartOptions{LogDir: logDir})
	t.Cleanup(fab.Shutdown)
	srv, err := daemon.Listen(sock, fab, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	go func(srv *daemon.Server) { _ = srv.Serve() }(srv)
	t.Cleanup(func() { srv.Close() })
	return sock, logDir
}

// dDial is a client closed with the test.
func dDial(t *testing.T, sock string) *daemon.Client {
	t.Helper()
	c, err := daemon.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// dAddSvc defines and starts `sh -c script`.
func dAddSvc(t *testing.T, sock, name, restart, script string) {
	t.Helper()
	if _, err := dDial(t, sock).Add(name, ipc.AddRequest{
		Command: "sh", Args: []string{"-c", script}, Restart: restart, Start: true,
	}); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
}

// dPidOf is the service's pid, 0 when it is not running or does not exist.
func dPidOf(t *testing.T, sock, name string) int {
	t.Helper()
	st, err := dDial(t, sock).Status(name)
	if err != nil {
		return 0
	}
	return st.Pid
}

// dEventually waits for cond, failing with what after a deadline only a failure reaches.
func dEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10s for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// dCapture runs f with standard output and standard error going to one buffer, and returns what
// it wrote. Not safe alongside anything else writing to them, which is why nothing here is
// parallel.
func dCapture(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()
	ferr := f()
	os.Stdout, os.Stderr = oldOut, oldErr
	w.Close()
	<-done
	r.Close()
	return buf.String(), ferr
}

// dTerminal is a pty with a command's attach running in it, read by gozellij's own emulator: the
// screen harness of internal/daemon, cut down to what a command-line test needs. The command reads
// os.Stdin and writes os.Stdout, so those are pointed at the pty while it runs.
type dTerminal struct {
	t      *testing.T
	master *os.File
	slave  *os.File
	mu     sync.Mutex
	term   *grid.Term
	done   chan error
}

func dRunInTerminal(t *testing.T, cols, rows int, f func() error) *dTerminal {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		t.Fatal(err)
	}
	d := &dTerminal{t: t, master: master, slave: slave, term: grid.New(cols, rows), done: make(chan error, 1)}
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				d.mu.Lock()
				_, _ = d.term.Write(buf[:n])
				replies := d.term.TakeReplies()
				d.mu.Unlock()
				if len(replies) > 0 {
					_, _ = master.Write(replies)
				}
			}
			if err != nil {
				return
			}
		}
	}()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = slave, slave
	go func() { d.done <- f() }()
	t.Cleanup(func() {
		d.detach()
		os.Stdin, os.Stdout = oldIn, oldOut
		slave.Close()
		master.Close()
	})
	return d
}

// detach presses Ctrl-] d and waits for the command to return, once.
func (d *dTerminal) detach() error {
	select {
	case err := <-d.done:
		d.done <- err
		return err
	default:
	}
	_, _ = d.master.Write([]byte{0x1d})
	time.Sleep(20 * time.Millisecond) // two reads, the way a person types a prefix command
	_, _ = d.master.Write([]byte{'d'})
	select {
	case err := <-d.done:
		d.done <- err
		return err
	case <-time.After(5 * time.Second):
		d.t.Errorf("the command did not end after Ctrl-] d")
		return nil
	}
}

func (d *dTerminal) bottom() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	snap := d.term.Snapshot()
	var b strings.Builder
	for _, c := range snap[len(snap)-1] {
		if c.Content == "" {
			b.WriteByte(' ')
		} else {
			b.WriteString(c.Content)
		}
	}
	return b.String()
}

// bottomShows waits for text on the status line.
func (d *dTerminal) bottomShows(text string) {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(d.bottom(), text) {
		if time.Now().After(deadline) {
			d.t.Fatalf("waited 10s for %q on the status line; it reads %q", text, d.bottom())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
