package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LaPingvino/gozellij/internal/daemon"
)

// Ported from acceptance.sh: "the status line as a command, and landing where you were",
// "changing a service in place, and renaming a running one", and "Ctrl-C stops watching, not the
// service".

func TestPortDStatsPrintsTheStatusLineAsACommand(t *testing.T) {
	dEnv(t)
	sock, _ := dStartDaemon(t)
	dAddSvc(t, sock, "lastone", "no", "exec cat")

	out, err := dCapture(t, func() error { return cmdStats([]string{"-socket", sock}) })
	if err != nil {
		t.Fatalf("stats: %v\n%s", err, out)
	}
	if !strings.Contains(out, "lastone") {
		t.Errorf("stats does not name the service: %q", out)
	}
	// Which of how many, the way the attached line does.
	if !regexp.MustCompile(`[0-9]+/[0-9]+`).MatchString(out) {
		t.Errorf("stats printed no service count: %q", out)
	}
}

func TestPortDShellLandsWhereYouWere(t *testing.T) {
	dEnv(t)
	sock, _ := dStartDaemon(t)
	dAddSvc(t, sock, "lastone", "no", "exec cat")
	dAddSvc(t, sock, "other", "no", "exec cat")
	// A running shell from the start, so that landing in lastone below is -last's doing and not
	// the fallback for a shell that is gone.
	dAddSvc(t, sock, "shell", "", "exec cat")

	// Land somewhere, so there is a "where you were" to come back to. Recorded when the attach
	// starts showing it, not on the way out.
	a := dRunInTerminal(t, 100, 8, func() error { return run([]string{"attach", "-socket", sock, "lastone"}) })
	a.bottomShows("[lastone]")
	if got := daemon.LastShown(); got != "lastone" {
		t.Fatalf("while attached to lastone, the last shown is %q", got)
	}
	if err := a.detach(); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// -last follows it.
	l := dRunInTerminal(t, 100, 8, func() error { return run([]string{"shell", "-socket", sock, "-last"}) })
	l.bottomShows("[lastone]")
	if err := l.detach(); err != nil {
		t.Fatalf("shell -last: %v", err)
	}

	// Without the flag, a login lands in the service called shell when there is one, whatever
	// anybody was last looking at.
	p := dRunInTerminal(t, 100, 8, func() error { return run([]string{"shell", "-socket", sock}) })
	p.bottomShows("[shell]")
	if err := p.detach(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	// But a shell that is gone is not made again while something else runs: the login lands
	// where you last were instead. Last at other, which is not first by name, so landing there
	// is the last-shown rule and not the alphabetical fallback.
	o := dRunInTerminal(t, 100, 8, func() error { return run([]string{"attach", "-socket", sock, "other"}) })
	o.bottomShows("[other]")
	if err := o.detach(); err != nil {
		t.Fatalf("attach other: %v", err)
	}
	if _, err := dDial(t, sock).Remove("shell", false); err != nil {
		t.Fatal(err)
	}
	g := dRunInTerminal(t, 100, 8, func() error { return run([]string{"shell", "-socket", sock}) })
	g.bottomShows("[other]")
	if err := g.detach(); err != nil {
		t.Fatalf("shell with shell gone: %v", err)
	}
	if _, err := dDial(t, sock).Status("shell"); err == nil {
		t.Error("a plain gozellij shell made shell again although other services were running")
	}
}

func TestPortDChangingAServiceInPlaceAndRenamingARunningOne(t *testing.T) {
	dEnv(t)
	sock, logDir := dStartDaemon(t)
	dAddSvc(t, sock, "setme", "no", "echo FIRST; exec cat")
	pid := dPidOf(t, sock, "setme")
	if pid == 0 {
		t.Fatal("setme is not running")
	}

	// A set with nothing to change is refused rather than answered as though it had.
	if out, err := dCapture(t, func() error { return cmdSet([]string{"-socket", sock, "setme"}) }); err == nil {
		t.Errorf("a set with no flags was accepted: %q", out)
	}

	// One that changes something says the running process is still the old definition, and what
	// to type.
	said, err := dCapture(t, func() error { return cmdSet([]string{"-socket", sock, "setme", "-restart", "always"}) })
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.Contains(said, "previous definition") || !strings.Contains(said, "restart setme") {
		t.Errorf("set said: %q", said)
	}

	// Setting what the running process already has is not a change. setme started with
	// -restart no.
	noop, err := dCapture(t, func() error { return cmdSet([]string{"-socket", sock, "setme", "-restart", "no"}) })
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if strings.Contains(noop, "previous definition") {
		t.Errorf("setting a value the running process already has asked for a restart: %q", noop)
	}

	// Saying it again still says it: the process is still the old definition.
	repeat, err := dCapture(t, func() error { return cmdSet([]string{"-socket", sock, "setme", "-restart", "always"}) })
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.Contains(repeat, "previous definition") {
		t.Errorf("the second set went quiet while the process was still the old definition: %q", repeat)
	}
	if now := dPidOf(t, sock, "setme"); now != pid {
		t.Fatalf("a set restarted the service: pid %d became %d", pid, now)
	}

	// Renaming a running service: it keeps its pid, answers to the new name, and its log goes
	// with it.
	if out, err := dCapture(t, func() error { return cmdRename([]string{"-socket", sock, "setme", "renamed"}) }); err != nil {
		t.Fatalf("rename: %v\n%s", err, out)
	}
	if now := dPidOf(t, sock, "renamed"); now != pid {
		t.Errorf("the pid changed across the rename: %d became %d", pid, now)
	}
	if _, err := dDial(t, sock).Status("setme"); err == nil {
		t.Error("the old name still answers after a rename")
	}
	if _, err := os.Stat(filepath.Join(logDir, "renamed.log")); err != nil {
		t.Errorf("no log under the new name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, "setme.log")); err == nil {
		t.Error("the log stayed under the old name")
	}
	logs, err := dCapture(t, func() error { return cmdLogs([]string{"-socket", sock, "renamed"}) })
	if err != nil || !strings.Contains(logs, "FIRST") {
		t.Errorf("what it said before the rename is not readable: %v %q", err, logs)
	}
}

func TestPortDCtrlCStopsWatchingNotTheService(t *testing.T) {
	dEnv(t)
	sock, _ := dStartDaemon(t)
	dAddSvc(t, sock, "watched_f", "no", `i=0; while :; do echo "WATCHED-$i"; i=$((i+1)); sleep 0.05; done`)
	pid := dPidOf(t, sock, "watched_f")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	oldOut := os.Stdout
	os.Stdout = w
	done := make(chan error, 1)
	go func() { done <- cmdLogs([]string{"-socket", sock, "-f", "watched_f"}) }()

	// The follower is showing output before it is stopped - which also means its interrupt
	// handler is in place, since it is set up before anything is followed.
	got := make([]byte, 0, 256)
	buf := make([]byte, 256)
	_ = r.SetReadDeadline(time.Now().Add(10 * time.Second))
	for !strings.Contains(string(got), "WATCHED-") {
		n, err := r.Read(buf)
		if err != nil {
			os.Stdout = oldOut
			t.Fatalf("the follower printed nothing: %v", err)
		}
		got = append(got, buf[:n]...)
	}

	// A real SIGINT, the way the terminal would deliver it.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	// Drained while it winds down, so a write to a full pipe cannot hold it up.
	go func() {
		for {
			if _, err := r.Read(buf); err != nil {
				return
			}
		}
	}()
	select {
	case err := <-done:
		os.Stdout = oldOut
		w.Close()
		if err != nil {
			t.Errorf("the follower ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		os.Stdout = oldOut
		t.Fatal("the follower did not stop on SIGINT")
	}

	if now := dPidOf(t, sock, "watched_f"); now != pid {
		t.Fatalf("interrupting the follower changed the service: pid %d became %d", pid, now)
	}
}
