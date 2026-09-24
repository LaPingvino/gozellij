package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/LaPingvino/gozellij/internal/vt/grid"
)

// Ported from acceptance.sh "the login shell actually lands in gozellij".
//
// The real chain, hermetically: bash -l reads a profile in a throwaway home, the block login-setup
// wrote runs the real CLI, which Ensures a shell service on a daemon of the test's own and attaches
// to it. The CLI is this test binary: with cCLIEnv set it is `gozellij` and nothing else, so no
// build step is needed and the code run is the code under test.

const cCLIEnv = "GOZELLIJ_PORTC_AS_CLI"

func init() {
	if os.Getenv(cCLIEnv) != "1" {
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "gozellij: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestPortCALoginShellLandsInGozellij(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash")
	}
	sock := cDaemon(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// A home carrying the shape of the problem: another multiplexer's autostart, pointed at a
	// stand-in so nothing real can start.
	lhome := t.TempDir()
	bin := filepath.Join(lhome, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "othermux"), []byte("#!/bin/sh\necho OTHER-MULTIPLEXER-STARTED\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(lhome, ".profile")
	if err := os.WriteFile(profile, []byte(`export EDITOR=vim
if [ -n "$PS1" ] && [ -z "${ZELLIJ:-}" ] && [ -z "${TMUX:-}" ]; then
    tmux attach -t main 2>/dev/null || "`+bin+`/othermux"
fi
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var ierr error
	captureStdout(t, func() { ierr = loginInstall(lhome, profile, "shell", self, false, true) })
	if ierr != nil {
		t.Fatal(ierr)
	}
	body, _ := os.ReadFile(profile)
	if !strings.Contains(string(body), "#gz# ") || !strings.Contains(string(body), "shell -name shell") {
		t.Fatalf("login-setup did not disable the other multiplexer and add its own block:\n%s", body)
	}

	// Everything the environment would otherwise carry in is left out, $TMUX and $ZELLIJ above
	// all: the block rightly stands down for them, and a check run from inside a multiplexer
	// would read that as the chain failing.
	cmd := exec.Command("/bin/bash", "-l")
	cmd.Dir = lhome
	cmd.Env = []string{
		"HOME=" + lhome,
		"PATH=/usr/bin:/bin",
		"SHELL=/bin/bash",
		"TERM=xterm-256color",
		"XDG_CONFIG_HOME=" + filepath.Join(lhome, ".config"),
		"GOZELLIJ_RUNTIME_DIR=" + filepath.Dir(sock),
		"GOZELLIJ_STATE_DIR=" + os.Getenv("GOZELLIJ_STATE_DIR"),
		cCLIEnv + "=1",
	}
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = master.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	var mu sync.Mutex
	term := grid.New(80, 20)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				mu.Lock()
				_, _ = term.Write(buf[:n])
				replies := term.TakeReplies()
				mu.Unlock()
				if len(replies) > 0 {
					_, _ = master.Write(replies)
				}
			}
			if err != nil {
				return
			}
		}
	}()
	screen := func() string {
		mu.Lock()
		defer mu.Unlock()
		var b strings.Builder
		for _, row := range term.Snapshot() {
			for _, c := range row {
				if c.Content == "" {
					b.WriteByte(' ')
				} else {
					b.WriteString(c.Content)
				}
			}
			b.WriteByte('\n')
		}
		return b.String()
	}
	until := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("waited for %s; the screen shows:\n%s", what, screen())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// The shell service exists once the block has run; typing before that would go to the outer
	// login shell and answer the question for the wrong one.
	until("the shell service on the test daemon", func() bool { return cHasService(sock, "shell") })
	until("the shell inside to take input", func() bool { return cViewers(sock, "shell") > 0 })

	// The attach is reading the terminal now, so this goes to the shell inside. If the inner
	// shell's own profile is still running it reads the line all the same, just later. The outer
	// login shell would answer INSIDE-[]-42.
	_, _ = master.Write([]byte("echo INSIDE-[$GOZELLIJ]-$((6*7))\r"))
	until("the answer from inside gozellij", func() bool { return strings.Contains(screen(), "INSIDE-[shell]-42") })
	if strings.Contains(screen(), "OTHER-MULTIPLEXER-STARTED") {
		t.Errorf("the other multiplexer started as well, so you would be in two at once:\n%s", screen())
	}
}
