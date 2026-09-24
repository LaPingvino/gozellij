package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Ported from acceptance.sh, continued: the sections about defining, restarting and breaking
// services, as the command line reports them. Helpers are in port_a_cli_test.go.

// ------------------------------------------------------------ takeover tells you the truth either way

// What takeover finds depends on the machine, so this asserts only that it is honest: either it
// says there is nothing, or it names what it found, says it cannot adopt it and what to type.
func TestPortATakeoverTellsTheTruthEitherWay(t *testing.T) {
	aEnv(t)
	out, errOut, err := aRun(t, "takeover")
	if err != nil {
		t.Fatalf("takeover failed: %v\n%s%s", err, out, errOut)
	}
	switch {
	case strings.Contains(out, "Nothing else on this machine is holding a terminal"):
	case strings.Contains(out, "cannot take these over") && strings.Contains(out, "gozellij add"):
	default:
		t.Errorf("takeover printed neither shape:\n%s", out)
	}
	if regexp.MustCompile(`(?i)took over|taken over|moved [0-9]+ `).MatchString(out) {
		t.Errorf("takeover claimed to have done something:\n%s", out)
	}
}

// ------------------------------------------------------------ a service file you can read, repair, and break

func TestPortAServiceFileYouCanReadRepairAndBreak(t *testing.T) {
	state, _ := aDaemon(t, false)
	aMust(t, "add", "editable", "-start", "-restart", "no", "--", "sh", "-c", `printf "BEFORE-THE-EDIT\r\n"; sleep 300`)
	def := filepath.Join(state, "services", "editable.json")
	body, err := os.ReadFile(def)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"command"`) || !regexp.MustCompile(`(?m)^  "`).Match(body) {
		t.Errorf("the definition is not JSON with one field per line:\n%s", body)
	}
	aLogsShow(t, "editable", "BEFORE-THE-EDIT")

	// The escape hatch: edit the file, restart, get what you wrote.
	if err := os.WriteFile(def, []byte(strings.ReplaceAll(string(body), "BEFORE-THE-EDIT", "AFTER-THE-EDIT")), 0o600); err != nil {
		t.Fatal(err)
	}
	aMust(t, "restart", "editable")
	aLogsShow(t, "editable", "AFTER-THE-EDIT")

	// An editing mistake is named by doctor, by the file's own name - which only this check
	// prints, unlike the words "service definitions".
	aMust(t, "stop", "editable")
	if err := os.WriteFile(def, []byte("not json at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, _ := aRun(t, "doctor")
	if !strings.Contains(out, "editable.json") {
		t.Errorf("doctor does not name the broken file:\n%s", out)
	}
}

// ------------------------------------------------------------ when there is no daemon at all

func TestPortAWhenThereIsNoDaemonAtAll(t *testing.T) {
	_, runDir := aEnv(t) // a runtime directory with nothing in it

	out, errOut, err := aRun(t, "ls")
	if err == nil {
		t.Fatalf("ls with no daemon succeeded, printing %q %q", out, errOut)
	}
	if !strings.Contains(err.Error(), runDir) {
		t.Errorf("it did not say which socket it looked for: %v", err)
	}
	if !strings.Contains(err.Error(), "gozellijd") {
		t.Errorf("it did not say how to start a daemon: %v", err)
	}

	doc, _, _ := aRun(t, "doctor")
	if !regexp.MustCompile(`(?m)^FAIL +daemon`).MatchString(doc) {
		t.Errorf("doctor did not report the missing daemon as a failure:\n%s", doc)
	}
	if n := len(regexp.MustCompile(`(?m)^(ok|warn|note|FAIL) `).FindAllString(doc, -1)); n < 4 {
		t.Errorf("doctor printed only %d checks without a daemon:\n%s", n, doc)
	}
}

// ------------------------------------------------------------ gozellijd -logs off keeps output off the disk

func TestPortALogsOffKeepsOutputOffTheDisk(t *testing.T) {
	state, runDir := aDaemon(t, true)
	// Computed by the service: the command itself is in the definition on disk, as it must be.
	aMust(t, "add", "hushed", "-start", "-restart", "no", "--", "sh", "-c", `printf "OFFDISK-%s\r\n" $((6*7)); sleep 120`)
	// Still in memory, which is what makes the flag usable rather than merely safe.
	aLogsShow(t, "hushed", "OFFDISK-42")

	for _, dir := range []string{state, runDir} {
		_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || fi.Mode()&os.ModeType != 0 {
				return nil
			}
			if strings.HasSuffix(p, ".log") {
				t.Errorf("-logs off wrote a log file: %s", p)
			}
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), "OFFDISK-42") {
				t.Errorf("the service's output reached the disk: %s", p)
			}
			return nil
		})
	}

	ls := aMust(t, "ls")
	found := false
	for _, line := range strings.Split(ls, "\n") {
		f := strings.Fields(line)
		if len(f) > 6 && f[0] == "hushed" {
			found = true
			if f[6] != "-" {
				t.Errorf("ls shows a log size for a log that does not exist: %q", line)
			}
		}
	}
	if !found {
		t.Errorf("hushed is not in ls:\n%s", ls)
	}
}

// ------------------------------------------------------------ the mistakes you make by typing

func TestPortATheMistakesYouMakeByTyping(t *testing.T) {
	aDaemon(t, false)
	aMust(t, "add", "taken", "-start", "-restart", "no", "--", "sh", "-c", "sleep 300")

	refused := func(what, want string, args ...string) {
		t.Helper()
		out, errOut, err := aRun(t, args...)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v (printed %q %q), want an error containing %q", what, err, out, errOut, want)
		}
	}
	refused("adding a name already taken", "already exists", "add", "taken", "--", "sh", "-c", "sleep 1")
	refused("a restart policy that is not one", "no, on-failure or always", "add", "spelt", "-restart", "sometimes", "--", "sh", "-c", "sleep 1")
	refused("add with no command", "gozellij add", "add", "nocommand")
	refused("status of a service that is not there", "no such service: ghost", "status", "ghost")
	refused("stop of a service that is not there", "no such service: ghost", "stop", "ghost")
	aMust(t, "add", "mover", "-restart", "no", "--", "sh", "-c", "sleep 300")
	refused("renaming onto a name in use", "already exists", "rename", "mover", "taken")

	// rm of a running service takes its process with it.
	pid, err := strconv.Atoi(aField(t, "taken", "pid"))
	if err != nil || aGone(pid) {
		t.Fatalf("taken has no live pid to check (%d, %v)", pid, err)
	}
	aMust(t, "rm", "taken")
	aUntil(t, fmt.Sprintf("pid %d to go with its service", pid), func() bool { return aGone(pid) })
}

// ------------------------------------------------------------ where a service runs, and what it runs with

func TestPortAWhereAServiceRunsAndWhatItRunsWith(t *testing.T) {
	aDaemon(t, false)
	home := os.Getenv("HOME")
	where := filepath.Join(home, "somewhere")
	if err := os.MkdirAll(where, 0o700); err != nil {
		t.Fatal(err)
	}
	aMust(t, "add", "placed", "-dir", where, "-env", "ONE=first", "-env", "TWO=second", "-start", "-restart", "no", "--",
		"sh", "-c", `echo "AT=[$PWD] ONE=[$ONE] TWO=[$TWO] HOME=[$HOME]"; sleep 120`)
	saw := aLogsShow(t, "placed", "AT=")

	if !strings.Contains(saw, "AT=["+where+"]") {
		t.Errorf("-dir did not run it where asked: %q", saw)
	}
	if !strings.Contains(saw, "ONE=[first]") || !strings.Contains(saw, "TWO=[second]") {
		t.Errorf("not every -env arrived: %q", saw)
	}
	// HOME, not PATH: a shell with an empty environment invents a PATH, never a HOME.
	if !strings.Contains(saw, "HOME=["+home+"]") {
		t.Errorf("-env replaced the environment rather than adding to it: %q", saw)
	}
}

// ------------------------------------------------------------ the restart policy you asked for is the one you get

func TestPortATheRestartPolicyYouAskedForIsTheOneYouGet(t *testing.T) {
	aDaemon(t, false)
	// Exits at once: the first restart waits fabric.BackoffFirst (a second), which is this test.
	aMust(t, "add", "comesback", "-restart", "always", "-start", "--", "sh", "-c", "exit 0")
	aMust(t, "add", "staysdead", "-restart", "no", "-start", "--", "sh", "-c", "exit 0")
	aMust(t, "add", "onlyfails", "-restart", "on-failure", "-start", "--", "sh", "-c", "exit 0")
	aMust(t, "add", "failsalot", "-restart", "on-failure", "-start", "--", "sh", "-c", "exit 3")

	starts := func(n string) int { v, _ := strconv.Atoi(aField(t, n, "starts")); return v }
	aUntil(t, "-restart always to bring comesback back", func() bool { return starts("comesback") > 1 })
	aUntil(t, "-restart on-failure to bring back a service that exited 3", func() bool { return starts("failsalot") > 1 })

	// Those two restarting is the clock: the other two had the same second to come back and
	// must not have.
	for _, n := range []string{"staysdead", "onlyfails"} {
		if s := starts(n); s != 1 {
			t.Errorf("%s exited cleanly and was started %d times, want 1", n, s)
		}
		if st := aField(t, n, "state"); st != "exited" {
			t.Errorf("%s is %q, want exited and staying so", n, st)
		}
	}
}

// ------------------------------------------------------------ restart, and what it must not throw away

func TestPortARestartAndWhatItMustNotThrowAway(t *testing.T) {
	state, _ := aDaemon(t, false)
	aMust(t, "add", "bouncer", "-start", "-restart", "no", "--", "sh", "-c", "echo SAID-BEFORE-RESTART; sleep 300")
	aLogsShow(t, "bouncer", "SAID-BEFORE-RESTART")
	before := aField(t, "bouncer", "pid")
	aMust(t, "restart", "bouncer")
	after := aField(t, "bouncer", "pid")
	if before == "" || after == "" || before == after {
		t.Errorf("restart did not replace the process: pid %q became %q", before, after)
	}
	if st := aField(t, "bouncer", "state"); st != "running" {
		t.Errorf("after a restart the service is %q", st)
	}
	if out, _, _ := aRun(t, "logs", "bouncer"); !strings.Contains(out, "SAID-BEFORE-RESTART") {
		t.Errorf("the log lost what was said before the restart: %q", out)
	}

	// The buffer carried across, with no file to read it from.
	aMust(t, "add", "nofile", "-log", "off", "-start", "-restart", "no", "--", "sh", "-c", "echo CARRIED-ACROSS; sleep 300")
	aLogsShow(t, "nofile", "CARRIED-ACROSS")
	if _, err := os.Stat(filepath.Join(state, "logs", "nofile.log")); err == nil {
		t.Errorf("-log off wrote a log file anyway")
	}
	aMust(t, "restart", "nofile")
	// Counted: a restart prints the line again, so two means one was kept from before.
	aUntil(t, "the output from before and after the restart, both", func() bool {
		out, _, _ := aRun(t, "logs", "nofile")
		return strings.Count(out, "CARRIED-ACROSS") >= 2
	})

	// Several names at once, and a bad one among them reported without abandoning the rest.
	aMust(t, "add", "bounce2", "-start", "-restart", "no", "--", "sh", "-c", "sleep 300")
	p1, p2 := aField(t, "bouncer", "pid"), aField(t, "bounce2", "pid")
	aMust(t, "restart", "bouncer", "bounce2")
	q1, q2 := aField(t, "bouncer", "pid"), aField(t, "bounce2", "pid")
	if q1 == "" || q2 == "" || p1 == q1 || p2 == q2 {
		t.Errorf("one restart of two: %s->%s and %s->%s", p1, q1, p2, q2)
	}
	_, errOut, err := aRun(t, "restart", "bouncer", "nosuchthing", "bounce2")
	if err == nil || !strings.Contains(errOut, "nosuchthing") {
		t.Errorf("restart with a bad name among good ones: %v, saying %q", err, errOut)
	}
	if r1, r2 := aField(t, "bouncer", "pid"), aField(t, "bounce2", "pid"); r1 == q1 || r2 == q2 || r1 == "" || r2 == "" {
		t.Errorf("the bad name stopped the good ones being restarted: %s->%s and %s->%s", q1, r1, q2, r2)
	}
}

// ------------------------------------------------------------ a broken environment is reported, not suffered

func TestPortABrokenLogDirectoryIsReportedNotSuffered(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a mode of 500")
	}
	state, _ := aDaemon(t, false)
	logs := filepath.Join(state, "logs")
	aMust(t, "add", "quiet1", "-start", "-restart", "no", "--", "sh", "-c", "echo BEFORE-BREAK; sleep 120")
	aLogsShow(t, "quiet1", "BEFORE-BREAK")
	if err := os.Chmod(logs, 0o500); err != nil {
		t.Fatal(err)
	}
	// Registered after aDaemon's, so it runs before them and before TempDir's removal.
	t.Cleanup(func() { _ = os.Chmod(logs, 0o700) })

	aMust(t, "add", "broke", "-start", "-restart", "no", "--", "sh", "-c", "echo AFTER-BREAK; sleep 120")
	if st := aField(t, "broke", "state"); st != "running" || aField(t, "broke", "pid") == "" {
		t.Errorf("a service whose log cannot be written did not run: %q", st)
	}
	if le := aField(t, "broke", "log error"); !strings.Contains(le, "permission denied") {
		t.Errorf("status does not say why the log is not written: %q", le)
	}
	var out, errOut string
	aUntil(t, "logs to show the output while saying the file is behind", func() bool {
		out, errOut, _ = aRun(t, "logs", "broke")
		return strings.Contains(out, "AFTER-BREAK") && strings.Contains(errOut, "not being written")
	})
	if doc, _, _ := aRun(t, "doctor"); !strings.Contains(doc, "logs: broke") {
		t.Errorf("doctor does not name the service whose log is failing:\n%s", doc)
	}
	if st := aField(t, "quiet1", "state"); st != "running" {
		t.Errorf("the service that was already logging is %q", st)
	}
}
