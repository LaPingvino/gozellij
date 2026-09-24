package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/daemon"
	"github.com/LaPingvino/gozellij/internal/fabric"
	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Ported from acceptance.sh "doctor is itself a promise", and the ls and status half of "watching
// without being able to touch": the promises that are about what the CLI prints.

// cDaemon starts a daemon of the test's own, with a home, state and configuration of its own, and
// returns its socket. Nothing here can reach the real one: every command is given -socket.
func cDaemon(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := filepath.Join(home, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOZELLIJ_STATE_DIR", state)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("GOZELLIJ_STATUS_CONFIG", "")

	sockDir, err := os.MkdirTemp("", "gzc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	t.Setenv("GOZELLIJ_RUNTIME_DIR", sockDir)
	sock := filepath.Join(sockDir, daemon.SocketName)

	reg, err := fabric.NewRegistry(filepath.Join(state, "services"))
	if err != nil {
		t.Fatal(err)
	}
	fab := fabric.NewFabric(reg, fabric.StartOptions{LogDir: filepath.Join(state, "logs")})
	t.Cleanup(fab.Shutdown)
	srv, err := daemon.Listen(sock, fab, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { srv.Close() })
	return sock
}

func cAdd(t *testing.T, sock, name string, args ...string) {
	t.Helper()
	c, err := daemon.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Add(name, ipc.AddRequest{Command: "sh", Args: args, Start: true}); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
}

func TestPortCDoctorOnAHealthyDaemon(t *testing.T) {
	sock := cDaemon(t)
	cAdd(t, sock, "doc", "-c", "exec sleep 60")

	var err error
	out := captureStdout(t, func() { err = cmdDoctor([]string{"-socket", sock}) })

	if err != nil {
		t.Errorf("doctor failed on a working setup: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`(?m)^ok    daemon `).MatchString(out) {
		t.Errorf("doctor did not report the daemon it just talked to:\n%s", out)
	}
	// Every line is one of its four levels, a fix heading, a fix, or blank. A malformed row is a
	// check that returned something the printer did not expect, which is how a diagnostic starts
	// lying.
	shape := regexp.MustCompile(`^(ok|note|warn|FAIL)  |^$|^[a-z].*:$|^  `)
	for _, line := range strings.Split(out, "\n") {
		if !shape.MatchString(line) {
			t.Errorf("doctor printed a line in no known shape: %q", line)
		}
	}
}

func TestPortCListAndStatusSayWhichViewersCanType(t *testing.T) {
	sock := cDaemon(t)
	cAdd(t, sock, "watched", "-c", "exec sleep 60")

	for _, ro := range []bool{true, false} {
		c, err := daemon.Dial(sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		if _, err := c.Call(ipc.OpAttach, "watched", ipc.AttachRequest{Cols: 50, Rows: 8, ReadOnly: ro}); err != nil {
			t.Fatalf("attach (read-only %v): %v", ro, err)
		}
	}

	row := regexp.MustCompile(`(?m)^watched\s.*\s1\+1r\s`)
	deadline := time.Now().Add(10 * time.Second)
	var ls string
	for {
		var err error
		ls = captureStdout(t, func() { err = cmdList([]string{"-socket", sock}) })
		if err != nil {
			t.Fatal(err)
		}
		if row.MatchString(ls) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("with one read-only and one ordinary attach, ls says:\n%s", ls)
		}
		time.Sleep(20 * time.Millisecond)
	}

	var err error
	st := captureStdout(t, func() { err = cmdStatus([]string{"-socket", sock, "watched"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`viewers:\s+2 \(1 read-only\)`).MatchString(st) {
		t.Errorf("status does not spell it out:\n%s", st)
	}
}

// cHasService says whether the daemon has a service by that name.
func cHasService(sock, name string) bool {
	c, err := daemon.Dial(sock)
	if err != nil {
		return false
	}
	defer c.Close()
	_, err = c.Status(name)
	return err == nil
}

// cViewers is how many terminals are attached to a service, 0 when it is not there.
func cViewers(sock, name string) int {
	c, err := daemon.Dial(sock)
	if err != nil {
		return 0
	}
	defer c.Close()
	s, err := c.Status(name)
	if err != nil {
		return 0
	}
	return s.Viewers
}
