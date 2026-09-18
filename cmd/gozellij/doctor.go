package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/LaPingvino/gozellij/internal/daemon"
	"github.com/LaPingvino/gozellij/internal/fabric"
)

// `gozellij doctor` exists because several of this program's promises are kept by things outside
// it, and nothing was checking any of them.
//
// "Your services survive a logout" is true only if `loginctl enable-linger` has been run.
// "An upgrade keeps every pid" is true only if you are not about to `systemctl --user restart`.
// "Your shell works" is true only if the environment it was started with matches the terminal you
// are in. Each of those was documented in packaging/README.md and enforced by nothing, which means
// the first time anyone finds out is when it has already gone wrong - a whole class of design
// rule 1 violations sitting in the gaps between this program and its host.
//
// So this command asks the host the questions the documentation answers, and says what to type.

// checkLevel is how much a finding matters.
type checkLevel int

const (
	// levelOK means the thing is as it should be.
	levelOK checkLevel = iota
	// levelNote means it is worth knowing but nothing is wrong.
	levelNote
	// levelWarn means a promise this program makes does not currently hold.
	levelWarn
	// levelFail means something is broken now, not merely at risk.
	levelFail
)

func (l checkLevel) String() string {
	switch l {
	case levelOK:
		return "ok"
	case levelNote:
		return "note"
	case levelWarn:
		return "warn"
	default:
		return "FAIL"
	}
}

// check is one question and its answer.
type check struct {
	name   string
	level  checkLevel
	detail string
	// fix is the thing to type. Empty when there is nothing to do.
	fix string
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	sock := socketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *sock
	if path == "" {
		path = daemon.SocketPath()
	}

	var checks []check
	checks = append(checks, checkSocketPath(path))

	// The daemon answers several of the questions below, so connect once and pass it down. A
	// doctor that cannot reach the daemon still has plenty to say, so this is not fatal.
	c, dialErr := daemon.Dial(path)
	if c != nil {
		defer c.Close()
	}
	checks = append(checks, checkDaemon(c, dialErr, path))
	checks = append(checks, checkVersions(c, dialErr)...)
	checks = append(checks, checkTreeKill(c, dialErr))
	checks = append(checks, checkStateDir())
	checks = append(checks, checkLinger())
	checks = append(checks, checkUnit())
	checks = append(checks, checkShellEnvironment(c, dialErr)...)
	checks = append(checks, checkServiceLogs(c, dialErr)...)

	worst := printChecks(checks)

	// Exit non-zero only for things that are broken now. A warning is a sentence worth reading,
	// not a reason for a script that calls this to fall over.
	if worst == levelFail {
		return errors.New("some checks failed; see above")
	}
	return nil
}

func printChecks(checks []check) checkLevel {
	worst := levelOK
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, c := range checks {
		if c.level > worst {
			worst = c.level
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", c.level, c.name, c.detail)
	}
	w.Flush()

	// Fixes go last and in full, rather than squeezed into the table: a command you are meant to
	// paste must not be wrapped or truncated by a column.
	var fixes []check
	for _, c := range checks {
		if c.fix != "" {
			fixes = append(fixes, c)
		}
	}
	if len(fixes) > 0 {
		fmt.Println()
		for _, c := range fixes {
			fmt.Printf("%s:\n  %s\n", c.name, strings.ReplaceAll(c.fix, "\n", "\n  "))
		}
	}
	return worst
}

// checkSocketPath catches the 108-byte sockaddr_un limit before bind() does, since bind's own
// error does not mention path length at all.
func checkSocketPath(path string) check {
	if err := daemon.CheckSocketPath(path); err != nil {
		return check{
			name:   "socket path",
			level:  levelFail,
			detail: err.Error(),
			fix:    "export GOZELLIJ_RUNTIME_DIR=/run/user/$UID/gz",
		}
	}
	return check{name: "socket path", level: levelOK, detail: fmt.Sprintf("%s (%d bytes)", path, len(path))}
}

func checkDaemon(c *daemon.Client, dialErr error, path string) check {
	if dialErr != nil {
		level := levelFail
		fix := ""
		if errors.Is(dialErr, daemon.ErrNoDaemon) {
			fix = "systemctl --user start gozellijd\nor, without systemd:  gozellijd >/dev/null 2>&1 &"
		}
		return check{name: "daemon", level: level, detail: dialErr.Error(), fix: fix}
	}
	if err := c.Ping(); err != nil {
		return check{name: "daemon", level: levelFail, detail: "connected but it did not answer: " + err.Error()}
	}
	return check{name: "daemon", level: levelOK, detail: "answering on " + path}
}

// checkVersions looks for the mismatch that actually happens: a new binary built on disk that
// nobody has told the running daemon about, so the daemon is still the old one.
func checkVersions(c *daemon.Client, dialErr error) []check {
	if dialErr != nil {
		return nil
	}
	running, err := c.PingVersion()
	if err != nil {
		return []check{{name: "daemon version", level: levelWarn, detail: "could not ask: " + err.Error()}}
	}
	if running == "" {
		running = "unknown"
	}

	out := []check{{name: "daemon version", level: levelOK, detail: running}}

	onDisk, path, derr := gozellijdVersion()
	switch {
	case derr != nil:
		out = append(out, check{
			name:   "gozellijd on PATH",
			level:  levelNote,
			detail: "not found, so a version comparison is not possible: " + derr.Error(),
		})
	case onDisk == running:
		out = append(out, check{name: "gozellijd on PATH", level: levelOK, detail: path + " matches the running daemon"})
	case running == "dev" || onDisk == "dev":
		// Two development builds have the same version string and different contents, so this
		// comparison cannot say anything. Saying that is better than implying they match.
		out = append(out, check{
			name:   "gozellijd on PATH",
			level:  levelNote,
			detail: fmt.Sprintf("%s is %q and the daemon is %q; development builds cannot be compared by version", path, onDisk, running),
		})
	default:
		out = append(out, check{
			name:   "gozellijd on PATH",
			level:  levelWarn,
			detail: fmt.Sprintf("%s is %s but the running daemon is %s", path, onDisk, running),
			fix:    "gozellij upgrade    # replaces the daemon in place, keeping every pid",
		})
	}

	// The client's own version too, because a CLI from a different build is the other half of
	// the same mistake.
	if Version != running && Version != "dev" && running != "dev" {
		out = append(out, check{
			name:   "this client",
			level:  levelNote,
			detail: fmt.Sprintf("gozellij is %s, talking to a daemon of %s", Version, running),
		})
	}
	return out
}

// gozellijdVersion asks the gozellijd on PATH what it is.
func gozellijdVersion() (version, path string, err error) {
	path, err = exec.LookPath("gozellijd")
	if err != nil {
		return "", "", err
	}
	cmd := exec.Command(path, "-version")
	out, err := cmd.Output()
	if err != nil {
		return "", path, err
	}
	// "gozellijd v1.2.3"
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", path, errors.New("it printed nothing")
	}
	return fields[len(fields)-1], path, nil
}

// checkTreeKill says whether `gozellij stop` stops everything or only nearly everything.
//
// The difference is invisible until the day it matters, and it is decided by how the daemon was
// started rather than by anything in the program - so it is exactly the kind of thing this command
// exists to surface.
func checkTreeKill(c *daemon.Client, dialErr error) check {
	if dialErr != nil {
		return check{name: "stopping services", level: levelNote, detail: "cannot ask without a daemon"}
	}
	info, err := c.Info()
	if err != nil {
		return check{name: "stopping services", level: levelWarn, detail: "could not ask: " + err.Error()}
	}
	switch info["tree_kill"] {
	case "cgroup":
		return check{
			name:   "stopping services",
			level:  levelOK,
			detail: "by cgroup (" + info["cgroup"] + "), so nothing a service starts can escape",
		}
	case "process group":
		return check{
			name:  "stopping services",
			level: levelWarn,
			detail: "by process group only, so a child that calls setsid survives `stop`: " +
				info["tree_kill_why"],
			fix: "run the daemon from the systemd user unit, which sets Delegate=yes:\n" +
				"systemctl --user enable --now gozellijd",
		}
	default:
		// A daemon older than this client. Say which, rather than reporting nothing.
		return check{
			name:   "stopping services",
			level:  levelNote,
			detail: "this daemon does not say; it is probably older than this client",
		}
	}
}

func checkStateDir() check {
	dir := daemon.StateDir()
	fi, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Not a fault: it is created on first use. Worth saying, because someone looking
			// for their service files needs to know they are not there yet.
			return check{name: "state directory", level: levelNote, detail: dir + " does not exist yet"}
		}
		return check{name: "state directory", level: levelFail, detail: err.Error()}
	}
	if !fi.IsDir() {
		return check{name: "state directory", level: levelFail, detail: dir + " is not a directory"}
	}
	// Service definitions carry environments, and environments carry secrets.
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		return check{
			name:   "state directory",
			level:  levelWarn,
			detail: fmt.Sprintf("%s is mode %04o, so other users can read your service definitions", dir, mode),
			fix:    fmt.Sprintf("chmod 700 %s", dir),
		}
	}
	return check{name: "state directory", level: levelOK, detail: dir}
}

// checkLinger is the one that matters most on a VPS. Without it the systemd user manager stops
// when your last session ends, which takes the daemon and therefore every service with it - so
// "it keeps running after you log out" is false, quietly, until the day you notice.
func checkLinger() check {
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("LOGNAME")
	}
	out, err := exec.Command("loginctl", "show-user", user, "--property=Linger").Output()
	if err != nil {
		return check{
			name:   "linger",
			level:  levelNote,
			detail: "could not ask loginctl (no systemd here?): " + err.Error(),
		}
	}
	value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "Linger="))
	if value == "yes" {
		return check{name: "linger", level: levelOK, detail: "on, so your services survive logging out"}
	}
	return check{
		name:   "linger",
		level:  levelWarn,
		detail: "off: the user manager stops when your last session ends, taking the daemon and every service with it",
		fix:    fmt.Sprintf("sudo loginctl enable-linger %s", user),
	}
}

func checkUnit() check {
	out, err := exec.Command("systemctl", "--user", "is-enabled", "gozellijd").Output()
	state := strings.TrimSpace(string(out))
	if err != nil && state == "" {
		return check{name: "systemd unit", level: levelNote, detail: "could not ask systemctl: " + err.Error()}
	}
	switch state {
	case "enabled", "enabled-runtime", "static":
		return check{name: "systemd unit", level: levelOK, detail: "gozellijd is " + state}
	case "not-found":
		return check{
			name:   "systemd unit",
			level:  levelWarn,
			detail: "gozellijd.service is not installed, so nothing starts the daemon at boot",
			fix: "mkdir -p ~/.config/systemd/user\n" +
				"cp packaging/systemd/gozellijd.service ~/.config/systemd/user/\n" +
				"systemctl --user daemon-reload\n" +
				"systemctl --user enable --now gozellijd",
		}
	default:
		return check{
			name:   "systemd unit",
			level:  levelWarn,
			detail: "gozellijd.service is " + state,
			fix:    "systemctl --user enable --now gozellijd",
		}
	}
}

// checkShellEnvironment compares the shell service's recorded environment with the terminal you
// are typing in.
//
// A service's environment is fixed when it is defined, and a shell defined from a different
// terminal has the wrong TERM - which does not look like a configuration problem, it looks like
// vim being broken. See cmdShell for why the shell is redefined rather than restarted.
func checkShellEnvironment(c *daemon.Client, dialErr error) []check {
	if dialErr != nil {
		return nil
	}
	running, known, err := shellState(c, DefaultShellService)
	if err != nil {
		return []check{{name: "shell service", level: levelWarn, detail: "could not ask: " + err.Error()}}
	}
	if !known {
		return []check{{
			name:   "shell service",
			level:  levelNote,
			detail: "not defined yet; bare `gozellij` will create it from this terminal",
		}}
	}
	if !running {
		return []check{{
			name:   "shell service",
			level:  levelNote,
			detail: "defined but not running; bare `gozellij` will define it again from this terminal",
		}}
	}

	// It is running, so its environment is frozen and may not be yours.
	mine := os.Getenv("TERM")
	theirs, err := serviceEnvValue(c, DefaultShellService, "TERM")
	switch {
	case err != nil:
		return []check{{name: "shell service", level: levelNote, detail: "running; could not read its environment: " + err.Error()}}
	case theirs == mine:
		return []check{{name: "shell service", level: levelOK, detail: "running with TERM=" + theirs + ", which matches this terminal"}}
	default:
		return []check{{
			name:  "shell service",
			level: levelWarn,
			detail: fmt.Sprintf("running with TERM=%q but this terminal is TERM=%q, so full-screen programs in it may draw wrongly",
				theirs, mine),
			fix: "gozellij stop shell && gozellij   # redefines it from this terminal",
		}}
	}
}

// serviceEnvValue reads one variable out of a service definition on disk.
//
// Read from the state directory rather than asked over the socket: the definition is a file this
// program promises is readable and repairable by hand (design rule 5), and adding a wire message
// to read something already sitting in a JSON file would be the wrong kind of thorough.
func serviceEnvValue(_ *daemon.Client, service, key string) (string, error) {
	reg, err := fabric.NewRegistry(filepath.Join(daemon.StateDir(), "services"))
	if err != nil {
		return "", err
	}
	svc, err := reg.Get(service)
	if err != nil {
		return "", err
	}
	for _, kv := range svc.Env {
		if name, value, ok := strings.Cut(kv, "="); ok && name == key {
			return value, nil
		}
	}
	return "", nil
}

// checkServiceLogs surfaces any service whose output is not reaching disk. That failure is
// invisible until somebody goes looking for output that was never written, which is the worst
// moment to learn about it.
func checkServiceLogs(c *daemon.Client, dialErr error) []check {
	if dialErr != nil {
		return nil
	}
	list, err := c.List()
	if err != nil {
		return []check{{name: "logs", level: levelWarn, detail: "could not list services: " + err.Error()}}
	}

	var broken []check
	for _, svc := range list.Services {
		if svc.LogError != "" {
			broken = append(broken, check{
				name:   "logs: " + svc.Service,
				level:  levelFail,
				detail: svc.LogError,
			})
		}
	}
	if len(broken) > 0 {
		return broken
	}

	// Also report the definitions the daemon could not load at all. They travel with the list
	// precisely so somebody can be told about them.
	for _, p := range list.Problems {
		broken = append(broken, check{name: "service definition", level: levelWarn, detail: p})
	}
	if len(broken) > 0 {
		return broken
	}
	return []check{{name: "logs", level: levelOK, detail: fmt.Sprintf("%d service(s), all writing to disk or logging off on purpose", len(list.Services))}}
}
