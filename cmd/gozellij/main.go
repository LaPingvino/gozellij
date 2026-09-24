// Command gozellij is the command line client for the fabric daemon.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/LaPingvino/gozellij/internal/daemon"
	"github.com/LaPingvino/gozellij/internal/fabric"
	"github.com/LaPingvino/gozellij/internal/ipc"
	"github.com/LaPingvino/gozellij/internal/status"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "gozellij: %v\n", err)
		// 1 for "the thing you asked for did not work", distinct from 2 for "I did not
		// understand what you asked for".
		os.Exit(1)
	}
}

func usage() {
	prefix := status.Load().Prefix
	fmt.Fprintf(os.Stderr, `gozellij %[1]s - a host-native process fabric

Usage:
  gozellij                             land in your shell (starts one if there is none)
  gozellij shell [-name <n>] [-render] the same, with a different service name (-last: where you were)
  gozellij ls                          list services
  gozellij status <name>...            show one service, or several
  gozellij add <name> -- <cmd> [args]  define a service
  gozellij start|stop|restart <name>.. change the state of one or several
  gozellij attach <name> [-render]     attach your terminal to it
  gozellij logs <name>                 print its recent output and exit
  gozellij logs -f <name>...           follow one or several services until you press Ctrl-C
  gozellij upgrade                     replace the daemon binary, keeping every process
  gozellij rm <name>... [-keep-logs]   stop them, forget them, delete their logs
  gozellij rename <old> <new>          give a service a new name, running or not; its log goes with it
  gozellij set <name> [flags] [-- cmd] change its command, -restart, -dir or -env in place
  gozellij ping                        check the daemon is alive
  gozellij doctor                      check the promises that depend on the host
  gozellij login-setup                 say how to make it what your login shell starts (-install to do it)
  gozellij takeover                    what is trapped in another multiplexer, and how to get it back
  gozellij stats                       print the status line once and exit

Flags for add:
  -restart no|on-failure|always   what to do when it exits (default no)
  -dir <path>                     working directory
  -env KEY=VALUE                  repeatable
  -start                          start it immediately
  -log on|off                     write its output to disk (default on)

Flags for logs:
  -n <bytes>   show at most this many bytes from the end
  -f           follow the live output instead of printing the file
  -since <t>   only what was written since: 10m, 1h30m, 14:05 (to within a minute)

logs reads the file on disk, which outlives the daemon; logs -f follows the daemon's live buffer,
which does not. A service added with -log off has no file, and logs then falls back to that
buffer - which holds a few hundred KiB and dies with the daemon.

The status line at the bottom of an attach is byobu-shaped: the same widget names, and a leading
# in the config switches one off. See gozellij stats -example.

While attached, %[2]s is gozellij's own key. Put a line like prefix=%[3]s in
%[4]s to change it; gozellij doctor says which key is in force.
  %[2]s d         detach; the service keeps running
  %[2]s n / p     next / previous service, in this same terminal
  %[2]s l         list the services and pick one by number
  %[2]s c         a new shell, in the directory you are in
  %[2]s ,         rename the service you are looking at
  %[2]s k         remove the service you are looking at
  %[2]s u         revive a pane that has frozen: reconnect it, starting its service if needed
  %[2]s ?         show these keys
  %[2]s %[2]s    send a literal %[2]s to the service

Global:
  -socket <path>   daemon socket (default $XDG_RUNTIME_DIR/gozellij/fabric.sock)

The daemon is gozellijd. Services keep running when it stops.
`, Version, status.PrefixLabel(prefix), otherPrefix(prefix), status.ConfigPath())
}

// otherPrefix is the example the usage text offers for changing the key. It has to be a key other
// than the one in force: the text used to say prefix=C-b to somebody who had already set exactly
// that, which reads as an instruction that did not work.
func otherPrefix(current byte) string {
	if current == 0x02 {
		return "C-]"
	}
	return "C-b"
}

func run(args []string) error {
	// No arguments means "put me somewhere useful". Printing usage was the wrong answer for a
	// program whose job is to be the thing you land in when you log in: nobody types the name of
	// their multiplexer in order to read its help.
	if len(args) == 0 {
		return cmdShell(nil)
	}

	switch args[0] {
	case "-h", "--help", "help":
		usage()
		return nil
	case "-version", "--version", "version":
		fmt.Printf("gozellij %s\n", Version)
		return nil
	}

	cmd, rest := args[0], args[1:]

	switch cmd {
	case "shell":
		return cmdShell(rest)
	case "ls", "list":
		return cmdList(rest)
	case "status":
		return cmdStatus(rest)
	case "add":
		return cmdAdd(rest)
	case "start", "stop", "restart":
		return cmdLifecycle(cmd, rest)
	case "rm", "remove":
		return cmdRemove(rest)
	case "rename", "mv":
		return cmdRename(rest)
	case "set", "edit":
		return cmdSet(rest)
	case "attach":
		return cmdAttach(rest)
	case "logs":
		return cmdLogs(rest)
	case "upgrade":
		return cmdUpgrade(rest)
	case "ping":
		return cmdPing(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "login-setup":
		return cmdLoginSetup(rest)
	case "takeover":
		return cmdTakeover(rest)
	case "stats":
		return cmdStats(rest)
	default:
		usage()
		// Name it. "Unknown command" without saying which is a small unkindness that adds up.
		//
		// And guess what they meant, because there is only one thing it can be. A service name
		// is the obvious thing to type after the program's own name - every other multiplexer
		// takes one there - and `gozellij work` is a whole login session that did not happen.
		if looksLikeAServiceName(cmd) {
			return fmt.Errorf("unknown command %q\n"+
				"if you meant a service:  gozellij attach %s\n"+
				"or to land in it as a shell, creating it if needed:  gozellij shell -name %s", cmd, cmd, cmd)
		}
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// DefaultShellService is the service bare `gozellij` lands you in.
//
// A name, not a session: in this design a shell is just a service, which is a different model from
// gezellij's named sessions. Whether that is the better model is an open question; what is not open
// is that typing the program's name has to put you somewhere.
const DefaultShellService = "shell"

// shellEnv are the variables a shell needs from the terminal you are typing in, rather than from
// whatever environment the daemon happens to have.
//
// This matters more than it looks. Under the systemd user unit the daemon has no TERM at all, and
// a service started from it gets TERM=dumb and no locale - so less, vim and top are broken in a
// shell that otherwise looks fine. Every by-hand test before this one started the daemon from a
// terminal and inherited the answer by accident.
var shellEnv = []string{"TERM", "COLORTERM", "LANG"}

// captureShellEnv reads the terminal's environment for a shell we are about to define.
func captureShellEnv() []string {
	var env []string
	for _, k := range shellEnv {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	// Locale is spelled across a family of variables and taking only LANG gets it subtly wrong
	// for anyone who sets LC_TIME or LC_COLLATE on its own.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}

// refuseNesting stops an attach from inside a gozellij service, which is almost never what
// somebody meant and is never what they meant when the target is the service they are in.
//
// Attaching a service to itself draws its own output back into itself: the client shows the pane
// it is running in, which includes the client, which includes... Found in use as a pane that
// "started double rendering, shaking" after `gozellij attach shell`, typed inside shell to revive
// it. That is refused outright, because nothing good comes of it.
//
// Attaching to a *different* service from inside one is nesting, and refused by default the way
// tmux refuses it, for the same reason tmux does: both layers use the same prefix key, the outer
// one takes it, and the inner one can never be told anything. The thing that was wanted is nearly
// always switching in place, so that is what the message offers. -nest does it anyway.
//
// targetPid is the service's process, or 0 when it is not running; asked says whether the daemon
// answered at all. Being underneath that process is the evidence that holds when the name does not
// - GOZELLIJ is frozen into the shell when it starts, so it says nothing once a service has been
// renamed, and nothing at all if the service sets GOZELLIJ itself.
//
// So the name counts only when the daemon could not be asked. Counting it always refused bare
// `gozellij` inside a shell renamed from "shell" to "work" as attaching shell to itself - a service
// that no longer existed.
func refuseNesting(target string, targetPid int, asked, nest bool) error {
	inside := os.Getenv("GOZELLIJ")
	label := status.PrefixLabel(status.Load().Prefix)
	self := targetPid > 0 && fabric.DescendsFrom(os.Getpid(), targetPid)
	if !asked && inside == target {
		self = true
	}
	if self {
		return fmt.Errorf("you are already inside %s - attaching it to itself would draw its own "+
			"output back into itself.\nIf this pane has frozen, %s u reconnects it", target, label)
	}
	if inside == "" || nest {
		return nil
	}
	return fmt.Errorf("you are inside %s already, and a gozellij inside gozellij cannot be reached: "+
		"the outer one takes %s.\n%s l picks %s from here, in place. To nest anyway: gozellij attach -nest %s",
		inside, label, label, target, target)
}

// servicePid asks the daemon for a service's process, for refuseNesting: 0 when it is not
// running or not defined, and asked false when the daemon could not be reached - the attach that
// follows reports that far better than a guard could.
func servicePid(sock, name string) (pid int, asked bool) {
	c, err := connect(sock)
	if err != nil {
		return 0, false
	}
	defer c.Close()
	s, err := c.Status(name)
	if err != nil {
		// It answered - most often "no such service", which is an answer.
		return 0, true
	}
	return s.Pid, true
}

// looksLikeAServiceName reports whether an unknown first argument is plausibly one, so that the
// error can guess. Not a flag, not a path, not empty: those are somebody mistyping a command.
func looksLikeAServiceName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, "/ \t") {
		return false
	}
	return true
}

// cmdShell lands you in a shell, creating one if there is not one already.
//
// The rule, which is worth stating because it decides what happens to a customised service: if the
// shell is running, you are reattached to it, environment and all, exactly as you left it. If it is
// not running, its definition is updated from the terminal you are typing in now - because a shell
// that is not running has nothing worth keeping, and inheriting a TERM from three logins ago is how
// you end up with a vim that draws garbage. Keep a shell you have customised under another name and
// attach to it by name.
// landInsteadOfRecreating is the running service to land in when the shell asked for no longer
// exists, or "" to go ahead as asked. See cmdShell.
func landInsteadOfRecreating(sock, name string) string {
	path := sock
	if path == "" {
		path = daemon.SocketPath()
	}
	c, err := daemon.Dial(path)
	if err != nil {
		return ""
	}
	defer c.Close()
	list, err := c.List()
	if err != nil {
		return ""
	}
	var running []string
	for _, s := range list.Services {
		if s.Service == name {
			return "" // it exists, running or not: as asked
		}
		if s.Pid != 0 {
			running = append(running, s.Service)
		}
	}
	if len(running) == 0 {
		return ""
	}
	last := daemon.LastShown()
	for _, n := range running {
		if n == last {
			return n
		}
	}
	sort.Strings(running)
	return running[0]
}

func cmdShell(args []string) error {
	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	sock := socketFlag(fs)
	name := fs.String("name", DefaultShellService, "the service to land in")
	nest := fs.Bool("nest", false, "land even from inside another gozellij service")
	last := fs.Bool("last", false, "land in whatever a terminal was last showing, if it is still running; otherwise -name")
	render := renderFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Following a renamed shell is a choice, not a default: without -last a login lands in the
	// service called -name, as it always has. With it, in the one you were last looking at - but
	// only while it runs, because a stopped one would be redefined as a fresh shell under a name
	// that means something else to you.
	if *last {
		if n := daemon.LastShown(); n != "" && n != *name {
			if pid, _ := servicePid(*sock, n); pid > 0 {
				*name = n
			}
		}
	}
	// A shell you closed by exiting it stays closed. Tabs close when you exit them now, so
	// "land in shell, and make it if it is gone" brought back the tab you had just Ctrl-D'd the
	// moment you reattached, while the ones you left running waited behind it. When the shell
	// named is gone and something else is running, land there instead - where you last were, if
	// that is still running - the way tmux attaches to the session you have rather than making
	// a new one. Only with nothing running is a new shell the answer.
	if n := landInsteadOfRecreating(*sock, *name); n != "" {
		*name = n
	}
	// Bare `gozellij`, typed inside a gozellij shell, is the same nesting as `attach` - and the
	// same feedback loop when the shell it would land in is this one.
	pid, asked := servicePid(*sock, *name)
	if err := refuseNesting(*name, pid, asked, *nest); err != nil {
		return err
	}

	path := *sock
	if path == "" {
		path = daemon.SocketPath()
	}

	c, err := daemon.Dial(path)
	if err != nil {
		if errors.Is(err, daemon.ErrNoDaemon) {
			// The bare invocation is the one a person types at login, so the answer has to be
			// the next command rather than the name of a program.
			return fmt.Errorf("%w\nStart it with:  systemctl --user start gozellijd\n"+
				"or, without systemd:  gozellijd >/dev/null 2>&1 &", err)
		}
		return err
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	home, _ := os.UserHomeDir()
	env := captureShellEnv()
	// Say where you are, the way tmux sets $TMUX and zellij sets $ZELLIJ.
	//
	// This is not decoration. A login shell runs your profile, and profiles start multiplexers:
	// the first time this was run by hand it dropped straight into gezellij, because ~/.profile
	// autostarts it and nothing said "you are already in one". Anything that autostarts a
	// session should guard on this variable.
	env = append(env, "GOZELLIJ="+*name)

	// One call, deliberately. Asking whether the shell exists and then creating it is a race
	// two terminals lose together: both find it defined and not running, both redefine it, and
	// one of them deletes the other's freshly created service. Measured with three at once, and
	// the loser's attach failed with "no such service". The daemon decides instead.
	_, err = c.Ensure(*name, ipc.AddRequest{
		Command: shell,
		// A login shell: under systemd the daemon's environment is nearly empty, so the shell
		// has to build its own rather than inherit one that was never set up.
		Args:    []string{"-l"},
		Dir:     home,
		Env:     env,
		Restart: "no",
		// A shell you live in closes when you exit it, as long as there is somewhere else to be.
		CloseOnExit: true,
	})
	c.Close()
	if err != nil {
		return err
	}

	return daemon.AttachLoopMode(path, *name, os.Stdin, os.Stdout, true, renderMode(render))
}

// shellState reports whether the service is running and whether it exists at all.
//
// It asks for the list rather than matching on the text of a "no such service" error: an error
// string is a message for a person, and branching on it breaks the first time somebody improves
// the wording.
func shellState(c *daemon.Client, name string) (running, known bool, err error) {
	list, err := c.List()
	if err != nil {
		return false, false, err
	}
	for _, svc := range list.Services {
		if svc.Service == name {
			return svc.State == "running", true, nil
		}
	}
	return false, false, nil
}

// connect dials the daemon, honouring a -socket flag already parsed into path.
func connect(path string) (*daemon.Client, error) {
	if path == "" {
		path = daemon.SocketPath()
	}
	return daemon.Dial(path)
}

func socketFlag(fs *flag.FlagSet) *string {
	return fs.String("socket", "", "daemon socket path")
}

func cmdPing(args []string) error {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	sock := socketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Ping(); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	sock := socketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	list, err := c.List()
	if err != nil {
		return err
	}

	if len(list.Services) == 0 {
		// Say it in words. An empty table is indistinguishable from a broken query, which is
		// the exact confusion this project was started over.
		fmt.Println("no services defined")
	} else {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSTATE\tPID\tUPTIME\tRESTARTS\tVIEWERS\tLOG\tCOMMAND")
		for _, s := range list.Services {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				s.Service, stateWord(s), pidWord(s), uptime(s), restartWord(s),
				viewerWord(s), byteWord(s.LogBytes), s.Command)
		}
		w.Flush()
	}

	// The one moment somebody reliably looks, after a package upgrade put a new client on disk
	// and left the old daemon running. doctor says it too, but nobody runs doctor after pacman.
	if running, verr := c.PingVersion(); verr == nil {
		if note := versionSkew(Version, running); note != "" {
			fmt.Fprintln(os.Stderr, note)
		}
	}

	// Problems travel with the list rather than replacing it.
	for _, p := range list.Problems {
		fmt.Fprintf(os.Stderr, "warning: %s\n", p)
	}
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	sock := socketFlag(fs)
	if err := fs.Parse(hoistName(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("status needs at least one service name")
	}
	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	return eachService(fs.Args(), func(name string) error {
		s, err := c.Status(name)
		if err != nil {
			return err
		}
		printStatus(s)
		return nil
	})
}

// viewerWord says how many terminals are watching, and "-" for none.
//
// A dash rather than a zero: a column of zeroes reads as a measurement, and what is being said is
// that nobody is there.
//
// Read-only viewers are marked, because the number on its own answers the wrong question. Three
// terminals showing a service is reassuring; three terminals that can type into it is a reason to
// find out whose they are before you restart it. "2+1r" is two that can type and one that cannot.
func viewerWord(s ipc.StatusReply) string {
	if s.Viewers == 0 {
		return "-"
	}
	if s.Watchers > 0 && s.Watchers < s.Viewers {
		return fmt.Sprintf("%d+%dr", s.Viewers-s.Watchers, s.Watchers)
	}
	if s.Watchers >= s.Viewers {
		return fmt.Sprintf("%dr", s.Viewers)
	}
	return strconv.Itoa(s.Viewers)
}

// byteWord is a size a person can read at a glance. Exact bytes matter to nobody looking at a
// list of services; whether a log is 40 MB matters to everybody on a small disk.
func byteWord(n int64) string {
	if n <= 0 {
		return "-"
	}
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	// Promote on the *rounded* value, not the raw one. Comparing the raw bytes against the unit
	// boundary printed 1048575 as "1024K", which is a unit this program does not have.
	v := float64(n) / 1024
	for _, unit := range []string{"K", "M", "G"} {
		if v < 1023.95 {
			return fmt.Sprintf("%.1f%s", v, unit)
		}
		v /= 1024
	}
	return fmt.Sprintf("%.1fT", v)
}

func printStatus(s ipc.StatusReply) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "service:\t%s\n", s.Service)
	fmt.Fprintf(w, "state:\t%s\n", stateWord(s))
	fmt.Fprintf(w, "enabled:\t%t\n", s.Enabled)
	if s.Command != "" {
		fmt.Fprintf(w, "command:\t%s\n", s.Command)
	}
	if s.Pid != 0 {
		fmt.Fprintf(w, "pid:\t%d\n", s.Pid)
		fmt.Fprintf(w, "uptime:\t%s\n", uptime(s))
	}
	if s.TotalStarts > 0 {
		fmt.Fprintf(w, "starts:\t%d\n", s.TotalStarts)
	}
	if s.Viewers > 0 {
		// Spelled out here rather than the list's shorthand: this is the page you read when you
		// are deciding whether restarting it will interrupt somebody.
		switch {
		case s.Watchers == 0:
			fmt.Fprintf(w, "viewers:\t%d\n", s.Viewers)
		case s.Watchers == s.Viewers:
			fmt.Fprintf(w, "viewers:\t%d (all read-only)\n", s.Viewers)
		default:
			fmt.Fprintf(w, "viewers:\t%d (%d read-only)\n", s.Viewers, s.Watchers)
		}
	}
	if s.LogPath != "" {
		fmt.Fprintf(w, "log:\t%s (%s)\n", s.LogPath, byteWord(s.LogBytes))
	}
	if s.Restarts > 0 {
		fmt.Fprintf(w, "restarts:\t%d\n", s.Restarts)
	}
	if !s.NextRestart.IsZero() && time.Until(s.NextRestart) > 0 {
		fmt.Fprintf(w, "next try:\tin %s\n", time.Until(s.NextRestart).Round(time.Second))
	}
	if s.HasExited {
		if s.ExitUnknown {
			// Adopted after a daemon crash: watchable, not waitable. See internal/fabric/orphan.go.
			fmt.Fprint(w, "last exit:\tended while this daemon was not its parent, so how is not known\n")
		} else if s.ExitSignal != "" {
			fmt.Fprintf(w, "last exit:\tkilled by %s\n", s.ExitSignal)
		} else {
			fmt.Fprintf(w, "last exit:\tcode %d\n", s.ExitCode)
		}
	}
	// Last, and always when present: this is the line that explains a service that will not
	// start, and burying it would defeat the point of having it.
	if s.LastError != "" {
		fmt.Fprintf(w, "last error:\t%s\n", s.LastError)
	}
	// A log that is not being written is a failure nobody notices until they go looking for
	// output that was never there, which is the worst possible moment to find out.
	if s.LogError != "" {
		fmt.Fprintf(w, "log error:\t%s\n", s.LogError)
	}
	w.Flush()
}

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	sock := socketFlag(fs)
	restart := fs.String("restart", "no", "no|on-failure|always")
	dir := fs.String("dir", "", "working directory")
	start := fs.Bool("start", false, "start it immediately")
	logMode := fs.String("log", "on", "on|off: write this service's output to disk")
	closeOnExit := fs.Bool("close-on-exit", false, "a shell tab: exiting it while attached removes it from the list (its log is kept)")
	var env stringList
	fs.Var(&env, "env", "KEY=VALUE (repeatable)")
	// People type the service name first - `gozellij add web -restart always -- caddy run` - but
	// Go's flag package stops at the first non-flag argument, which would silently turn
	// "-restart" into the command. Hoist the name out first so both orders work.
	if err := fs.Parse(hoistName(args)); err != nil {
		return err
	}

	// Before the slice, not after it. `gozellij add` on its own - which is what somebody types to
	// find out what add wants - panicked with a Go stack trace on fs.Args()[1:] of an empty
	// slice, and the helpful message four lines below never got the chance to run.
	name := fs.Arg(0)
	var cmdArgs []string
	if rest := fs.Args(); len(rest) > 1 {
		cmdArgs = rest[1:]
	}
	if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}
	if name == "" || len(cmdArgs) == 0 {
		return errors.New("add needs a name and a command, e.g.\n" +
			"  gozellij add web -restart always -start -- caddy run")
	}
	// A command that looks like a flag is a parsing accident, not a program. Refusing here beats
	// defining a service called "web" that runs "-restart", which is what happens if this slips
	// through: the add succeeds, the start fails later, and the message makes no sense.
	if strings.HasPrefix(cmdArgs[0], "-") {
		return fmt.Errorf("the command is %q, which looks like a flag.\n"+
			"Put gozellij's own flags before the command and separate the command with --, e.g.\n"+
			"  gozellij add %s -restart always -start -- your-program --its-own-flags",
			cmdArgs[0], name)
	}

	var noLog bool
	switch *logMode {
	case "on":
	case "off":
		noLog = true
	default:
		// Name the value. "invalid -log" without saying what was wrong with it is the kind of
		// small unkindness this project keeps trying not to commit.
		return fmt.Errorf("-log is %q; it takes on or off", *logMode)
	}

	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	s, err := c.Add(name, ipc.AddRequest{
		Command: cmdArgs[0],
		Args:    cmdArgs[1:],
		Dir:     *dir,
		Env:     env,
		Restart: *restart,
		Start:   *start,
		NoLog:   noLog,

		CloseOnExit: *closeOnExit,
	})
	if err != nil {
		return err
	}
	printStatus(s)
	return nil
}

func cmdLifecycle(op string, args []string) error {
	fs := flag.NewFlagSet(op, flag.ContinueOnError)
	sock := socketFlag(fs)
	if err := fs.Parse(hoistName(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("%s needs at least one service name", op)
	}
	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	return eachService(fs.Args(), func(name string) error {
		return oneLifecycle(c, op, name)
	})
}

func oneLifecycle(c *daemon.Client, op, name string) error {
	var (
		s   ipc.StatusReply
		err error
	)
	switch op {
	case "start":
		s, err = c.Start(name)
	case "stop":
		s, err = c.Stop(name)
	case "restart":
		s, err = c.Restart(name)
	}
	if err != nil {
		return err
	}
	printStatus(s)
	return nil
}

func cmdAttach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	sock := socketFlag(fs)
	noReplay := fs.Bool("no-replay", false, "do not replay the recent output before the live stream")
	readOnly := fs.Bool("r", false, "watch without touching: your keystrokes and your window size do not reach the service")
	nest := fs.Bool("nest", false, "attach even from inside another gozellij service (the outer one keeps the prefix key)")
	render := renderFlags(fs)
	if err := fs.Parse(hoistName(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("attach needs exactly one service name")
	}
	pid, asked := servicePid(*sock, fs.Arg(0))
	if err := refuseNesting(fs.Arg(0), pid, asked, *nest); err != nil {
		return err
	}
	path := *sock
	if path == "" {
		path = daemon.SocketPath()
	}
	// AttachLoop rather than a single attach: a daemon upgrade takes the socket with it, and a
	// terminal that silently returns to a shell prompt cannot tell you whether your service died
	// or the daemon was replaced.
	return daemon.AttachLoopWith(path, fs.Arg(0), os.Stdin, os.Stdout, daemon.AttachOptions{
		Replay: !*noReplay, Mode: renderMode(render), ReadOnly: *readOnly,
	})
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	sock := socketFlag(fs)
	maxBytes := fs.Int("n", 0, "show at most this many bytes from the end (0 = as much as fits)")
	follow := fs.Bool("f", false, "follow the live output until interrupted")
	sinceArg := fs.String("since", "", "only what was written since: 10m, 1h30m, 14:05, or an RFC 3339 time")
	if err := fs.Parse(hoistName(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("logs needs at least one service name")
	}
	if fs.NArg() > 1 && !*follow {
		// Printing several finished logs one after another would run them together with no way
		// to tell where one ended. Following them interleaves them with a name on every line,
		// which is the thing several services at once is actually for.
		return errors.New("logs takes one service name, or several with -f")
	}

	var since time.Time
	if *sinceArg != "" {
		t, err := parseSince(*sinceArg, time.Now())
		if err != nil {
			return err
		}
		since = t
		if *follow {
			// Refused rather than half done: following reads the live buffer, which does not
			// know when anything was written, and stitching the file onto it without printing
			// the seam twice is its own piece of work.
			return errors.New("-since and -f together are not supported yet; run logs -since, then logs -f")
		}
	}

	if *follow {
		return followLogs(*sock, fs.Args(), *maxBytes)
	}

	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	var out ipc.LogsReply
	if since.IsZero() {
		out, err = c.Logs(fs.Arg(0), *maxBytes)
	} else {
		out, err = c.LogsSince(fs.Arg(0), since, *maxBytes)
	}
	if err != nil {
		return err
	}
	if out.Truncated {
		if out.Path != "" {
			// Say where the rest is. A truncation notice that does not name the file leaves
			// the reader with a question and no way to answer it.
			fmt.Fprintf(os.Stderr, "[showing the last %d bytes; the whole log is in %s]\n",
				len(out.Data), out.Path)
		} else {
			fmt.Fprintf(os.Stderr, "[showing the last %d bytes; older output was dropped]\n", len(out.Data))
		}
	}
	// Before the output, not after: a warning printed under a screen of scrollback is a warning
	// nobody reads.
	if out.Note != "" {
		fmt.Fprintf(os.Stderr, "[gozellij: %s]\n", out.Note)
	}
	if out.LogError != "" {
		fmt.Fprintf(os.Stderr, "[gozellij: this log is not being written and may be behind: %s]\n", out.LogError)
	}
	os.Stdout.Write(out.Data)
	if len(out.Data) == 0 {
		// An empty answer is ambiguous between "quiet" and "not running", so say which.
		if out.Running {
			fmt.Fprintln(os.Stderr, "[no output yet; the service is running]")
		} else {
			fmt.Fprintln(os.Stderr, "[no output; the service is not running]")
		}
	}
	return nil
}

// followLogs follows one service, or several at once with a name on every line.
//
// One connection each rather than one multiplexed stream: the daemon already knows how to send a
// service's output down a connection, and a second mechanism for the same thing would be a second
// mechanism to get wrong. The cost is a socket per service, which for a handful of services on a
// local socket is not a cost.
func followLogs(socket string, names []string, maxBytes int) error {
	// Ctrl-C is how you stop following, so it must end this cleanly rather than looking like a
	// crash. Closing the connections unblocks the reads.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	var (
		mu      sync.Mutex
		clients []*daemon.Client
		closed  bool
	)
	closeAll := func() {
		mu.Lock()
		defer mu.Unlock()
		closed = true
		for _, c := range clients {
			c.Close()
		}
	}
	defer closeAll()

	go func() {
		<-stop
		closeAll()
	}()

	// One writer, locked, so two services cannot interleave mid-line.
	out := &prefixedOutput{w: os.Stdout}

	var wg sync.WaitGroup
	errs := make(chan error, len(names))
	for _, name := range names {
		c, err := connect(socket)
		if err != nil {
			return err
		}
		mu.Lock()
		if closed {
			// Interrupted while we were still connecting.
			mu.Unlock()
			c.Close()
			return nil
		}
		clients = append(clients, c)
		mu.Unlock()

		prefix := ""
		if len(names) > 1 {
			prefix = name + " | "
		}

		wg.Add(1)
		go func(c *daemon.Client, name, prefix string) {
			defer wg.Done()
			if err := c.FollowLogs(name, maxBytes, out.for_(prefix), os.Stderr); err != nil {
				errs <- fmt.Errorf("following %s: %w", name, err)
			}
		}(c, name, prefix)
	}

	wg.Wait()
	close(errs)

	mu.Lock()
	interrupted := closed
	mu.Unlock()
	if interrupted {
		// Ctrl-C closed the connections underneath the readers, so every one of them has an
		// error to report about it. That is what the user asked for, not a failure.
		return nil
	}
	// The first real failure, if any.
	for err := range errs {
		return err
	}
	return nil
}

// prefixedOutput writes several services' output to one place, putting a name at the start of
// every line and never letting two of them share one.
type prefixedOutput struct {
	mu sync.Mutex
	w  io.Writer
	// atStart is whether the next byte begins a physical line, and last is whose bytes are on
	// the current one. Both are about the output, not about any one service: a chunk that
	// arrives split across a newline must not lose its prefix, and a service that writes while
	// another is mid-line must not continue that line.
	atStart bool
	last    string
	begun   bool
}

func (p *prefixedOutput) for_(prefix string) io.Writer {
	return prefixWriter{out: p, prefix: prefix}
}

type prefixWriter struct {
	out    *prefixedOutput
	prefix string
}

func (w prefixWriter) Write(b []byte) (int, error) {
	w.out.mu.Lock()
	defer w.out.mu.Unlock()

	if w.prefix == "" {
		// One service: its bytes go through untouched, escape sequences and all, exactly as
		// before. Prefixing a single stream would only get in the way of `logs -f | grep`.
		return w.out.w.Write(b)
	}
	if !w.out.begun {
		w.out.atStart, w.out.begun = true, true
	}

	// Built up and written once: a write per byte would be a syscall per byte, and a service
	// that logs fast would spend all of it here.
	out := make([]byte, 0, len(b)+len(w.prefix)*8)
	if !w.out.atStart && w.out.last != w.prefix {
		// Another service is mid-line. Break it rather than continuing it under the wrong name:
		// a line whose first half belongs to one service and second half to another is worse
		// than a line that ends early, because nothing about it says so.
		out = append(out, '\n')
		w.out.atStart = true
	}
	for _, c := range b {
		if w.out.atStart {
			out = append(out, w.prefix...)
			w.out.atStart = false
		}
		out = append(out, c)
		if c == '\n' {
			w.out.atStart = true
		}
	}
	w.out.last = w.prefix
	if _, err := w.out.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// renderFlags adds -render and -no-render to a command.
//
// Two flags rather than one taking a value, because `-render` is what a person types and
// `-render=false` is not. They are read together by renderMode, which rejects being given both -
// a command line that says two opposite things is a mistake worth pointing at rather than
// resolving by whichever came last.
type renderChoice struct{ on, off *bool }

func renderFlags(fs *flag.FlagSet) renderChoice {
	return renderChoice{
		on: fs.Bool("render", false, "draw the screen here instead of passing the bytes through: "+
			"needed for splitting panes, and it interprets everything the service emits"),
		off: fs.Bool("no-render", false, "pass the bytes through even if GOZELLIJ_RENDER is set"),
	}
}

func renderMode(c renderChoice) daemon.RenderMode {
	switch {
	case *c.on && *c.off:
		// Neither is chosen, which surfaces as the default. Saying so is better than silently
		// picking one, and this is checked by the caller before it gets here in practice.
		fmt.Fprintln(os.Stderr, "gozellij: -render and -no-render contradict each other; using the default")
		return daemon.RenderAuto
	case *c.on:
		return daemon.RenderOn
	case *c.off:
		return daemon.RenderOff
	default:
		return daemon.RenderAuto
	}
}

// upgradeWait is how long we give the daemon to come back after replacing itself.
const upgradeWait = 30 * time.Second

// upgradeReplyGrace mirrors the daemon's own pause between accepting an upgrade and exec'ing. The
// client waits longer than this before it starts looking for the successor, so it does not find
// the predecessor instead.
const upgradeReplyGrace = 150 * time.Millisecond

// waitForDaemon polls until a daemon both accepts a connection and answers on it.
func waitForDaemon(path string, within time.Duration) (*daemon.Client, error) {
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := daemon.Dial(path)
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if err := c.Ping(); err != nil {
			// Opened but did not answer: almost certainly the predecessor on its way out.
			c.Close()
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return c, nil
	}
	return nil, fmt.Errorf("%w within %v (last error: %v); check its log",
		errDaemonNeverAnswered, within, lastErr)
}

// errDaemonNeverAnswered marks the one failure waitForDaemon reports by itself: it ran out of time
// without a connection that answered.
//
// Worth a sentinel because a caller retrying the whole exchange has to be able to tell it apart
// from a failure it got *from* the daemon. "Could not reach it" must never replace "reached it,
// and here is what broke" - the second one is the cause and the first is just the clock. With the
// deadline clamped, a remaining slice under a round-trip still lets waitForDaemon run zero
// iterations and return this, which would otherwise overwrite the real error on the way out.
var errDaemonNeverAnswered = errors.New("the daemon did not come back")

// askTheSuccessor gets the version and the service list from the daemon that came back, retrying
// the whole exchange rather than any one request in it.
//
// Answering a ping is not proof of being the successor. The predecessor is still listening while
// it winds down - it has to be, or the reply to the upgrade request could not reach us - so it can
// accept a connection, answer a ping, and then exec itself out from under the next request on that
// same connection. What that looks like is a successful upgrade reporting
//
//	listing services after the upgrade: ... write: broken pipe
//
// about a connection that had just answered, while the daemon really is the new binary. It showed
// up about one run in fifteen of the acceptance suite and was diagnosed only because the check was
// changed to say which half had failed.
//
// Waiting longer would have made it rarer, which is the wrong fix for a window: a connection that
// dies part-way through is not an error to report, it is a reason to dial again, because the
// successor is there and the next connection reaches it.
func askTheSuccessor(path string, within time.Duration) (string, ipc.ListReply, error) {
	deadline := time.Now().Add(within)
	var lastErr error
	for {
		// How long is left, checked before dialling rather than after failing.
		//
		// The previous shape checked the deadline and then slept, so the sleep could carry it
		// past and the next attempt called waitForDaemon with a negative duration. Its loop then
		// ran zero times and it reported "the daemon did not come back within -50ms (last error:
		// <nil>)" - which is nonsense on its own, and worse than nonsense in context, because it
		// overwrote the real error and that is what the person saw. The cause of a failure is the
		// one thing a message about it has to keep.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if lastErr == nil {
				lastErr = fmt.Errorf("no time left to ask the daemon after the upgrade")
			}
			return "", ipc.ListReply{}, lastErr
		}
		version, list, err := func() (string, ipc.ListReply, error) {
			back, err := waitForDaemon(path, remaining)
			if err != nil {
				return "", ipc.ListReply{}, err
			}
			defer back.Close()
			version, err := back.PingVersion()
			if err != nil {
				return "", ipc.ListReply{}, err
			}
			list, err := back.List()
			if err != nil {
				return "", ipc.ListReply{}, err
			}
			return version, list, nil
		}()
		if err == nil {
			return version, list, nil
		}
		// A "could not reach it" never replaces a "reached it, and here is what broke": the
		// second names a cause and the first names the clock.
		if lastErr == nil || !errors.Is(err, errDaemonNeverAnswered) {
			lastErr = err
		}
		// Never sleep past the deadline: the next turn of the loop is where it is noticed, and
		// it has to be noticed with time left to say something true about it.
		time.Sleep(min(50*time.Millisecond, time.Until(deadline)))
	}
}

func cmdUpgrade(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	sock := socketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *sock
	if path == "" {
		path = daemon.SocketPath()
	}

	c, err := connect(path)
	if err != nil {
		return err
	}

	before, err := c.List()
	if err != nil {
		c.Close()
		return fmt.Errorf("listing services before the upgrade: %w", err)
	}
	oldVersion, _ := c.PingVersion()

	// Remember the pids. Comparing them afterwards is the only honest way to say whether the
	// processes really survived - it is the check that caught this being broken in the first
	// place, so the command that claims to keep them had better run it too.
	wasRunning := map[string]int{}
	for _, s := range before.Services {
		if s.Pid != 0 {
			wasRunning[s.Service] = s.Pid
		}
	}

	reply, err := c.Upgrade()
	if err != nil {
		c.Close()
		return err
	}
	c.Close()

	for _, p := range reply.Problems {
		fmt.Fprintf(os.Stderr, "warning: %s\n", p)
	}
	fmt.Printf("upgrading the daemon, carrying %d process(es)...\n", reply.Processes)

	// Wait for the *successor*, which is subtler than waiting for a socket to answer. The old
	// daemon is still listening for a moment after it accepts the request - it has to be, so the
	// reply can reach us - so the first connection that succeeds may well be the one that is
	// about to exec itself out of existence. Connecting and then having the socket die under you
	// is exactly what that looks like.
	//
	// So: give the predecessor time to go, then poll until a connection both opens *and*
	// answers, retrying the transient failures in between instead of taking the first of them
	// as final.
	time.Sleep(3 * upgradeReplyGrace)

	newVersion, after, err := askTheSuccessor(path, upgradeWait)
	if err != nil {
		return fmt.Errorf("listing services after the upgrade: %w", err)
	}

	kept, changed := 0, []string{}
	for _, s := range after.Services {
		old, had := wasRunning[s.Service]
		if !had {
			continue
		}
		if s.Pid == old {
			kept++
		} else {
			changed = append(changed, fmt.Sprintf("%s (%d -> %d)", s.Service, old, s.Pid))
		}
	}

	if oldVersion != newVersion {
		fmt.Printf("daemon upgraded: %s -> %s\n", displayVersion(oldVersion), displayVersion(newVersion))
	} else {
		fmt.Printf("daemon restarted in place (version %s, unchanged)\n", displayVersion(newVersion))
	}
	fmt.Printf("%d process(es) kept their pid\n", kept)
	if len(changed) > 0 {
		// Do not round this up into a success. A changed pid means the process was restarted,
		// and whoever ran this needs to know which ones.
		fmt.Fprintf(os.Stderr, "warning: %d process(es) did NOT survive and were restarted: %s\n",
			len(changed), strings.Join(changed, ", "))
		return fmt.Errorf("%d process(es) were restarted by the upgrade", len(changed))
	}
	return nil
}

func displayVersion(v string) string {
	if v == "" {
		return "(unknown)"
	}
	return v
}

// parseSince reads -since: a duration back from now ("10m", "1h30m"), a clock time meaning the most
// recent one ("14:05" at 09:00 is yesterday's), or an RFC 3339 timestamp.
func parseSince(s string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			d = -d
		}
		return now.Add(-d), nil
	}
	for _, layout := range []string{"15:04", "15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, now.Location())
			if at.After(now) {
				at = at.AddDate(0, 0, -1)
			}
			return at, nil
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		// Zero on the wire means no -since at all, so a time at or before the epoch would come
		// back unfiltered. Nothing was logged then anyway.
		if t.Unix() <= 0 {
			return time.Time{}, fmt.Errorf("-since %q is before anything could have been logged", s)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("-since %q: give a duration (10m, 1h30m), a clock time (14:05) or an RFC 3339 time", s)
}

// versionSkew says, in one line, that the daemon is not the version of this client - or nothing,
// when they match or when either is a development build, whose version string says nothing about
// its contents. Package builds are pacman's 0.r<commits>.<hash>, so which one is older can be told
// from the commit count rather than guessed.
func versionSkew(client, daemon string) string {
	if client == daemon || client == "" || daemon == "" || client == "dev" || daemon == "dev" {
		return ""
	}
	how := "is " + daemon + ", not " + client
	if c, cok := commitCount(client); cok {
		if d, dok := commitCount(daemon); dok {
			switch {
			case d < c:
				how = "is older (" + daemon + ") than this client (" + client + ")"
			case d > c:
				how = "is newer (" + daemon + ") than this client (" + client + ")"
			}
		}
	}
	return "note: the running daemon " + how + ". gozellij upgrade replaces it in place; every service keeps its pid."
}

// commitCount reads the r<N> out of a pacman VCS version such as 0.r175.14ae127.
func commitCount(v string) (int, bool) {
	for _, part := range strings.Split(v, ".") {
		if n, ok := strings.CutPrefix(part, "r"); ok {
			if i, err := strconv.Atoi(n); err == nil {
				return i, true
			}
		}
	}
	return 0, false
}

// cmdSet changes part of a service's definition in place: the flags given, and the command if one
// follows. Everything else stays as it was. The fix for a wrong flag used to be rm then add, which
// stopped the service and deleted its log.
func cmdSet(args []string) error {
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	sock := socketFlag(fs)
	restart := fs.String("restart", "", "no|on-failure|always")
	dir := fs.String("dir", "", "working directory")
	var env stringList
	fs.Var(&env, "env", "KEY=VALUE (repeatable); replaces the whole environment list")
	if err := fs.Parse(hoistName(args)); err != nil {
		return err
	}
	name := fs.Arg(0)
	var cmdArgs []string
	if rest := fs.Args(); len(rest) > 1 {
		cmdArgs = rest[1:]
	}
	if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}
	if name == "" {
		return errors.New("set needs a service name, e.g.\n" +
			"  gozellij set web -restart always\n" +
			"  gozellij set web -- caddy run --config /etc/caddy/Caddyfile")
	}

	var req ipc.SetRequest
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if given["restart"] {
		req.Restart = restart
	}
	if given["dir"] {
		req.Dir = dir
	}
	if given["env"] {
		e := []string(env)
		req.Env = &e
	}
	if len(cmdArgs) > 0 {
		if strings.HasPrefix(cmdArgs[0], "-") {
			return fmt.Errorf("the command is %q, which looks like a flag.\n"+
				"Put gozellij's own flags before the command and separate the command with --", cmdArgs[0])
		}
		command, rest := cmdArgs[0], cmdArgs[1:]
		req.Command, req.Args = &command, &rest
	}
	if req == (ipc.SetRequest{}) {
		// Rule 1: a set that changes nothing must not answer as though it had.
		return fmt.Errorf("nothing to change: give -restart, -dir, -env, or -- and a new command")
	}

	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()
	reply, err := c.Set(name, req)
	if err != nil {
		return err
	}
	printStatus(reply.Status)
	if reply.Pending {
		fmt.Printf("\n%s is still running the previous definition; this applies from its next start.\n"+
			"  gozellij restart %s\n", name, name)
	}
	return nil
}

// cmdRename gives a service a new name, running or not. See fabric.Rename for what follows it.
func cmdRename(args []string) error {
	fs := flag.NewFlagSet("rename", flag.ContinueOnError)
	sock := socketFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("rename needs the current name and the new one: gozellij rename <old> <new>")
	}
	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()
	from, to := fs.Arg(0), fs.Arg(1)
	warning, err := c.Rename(from, to)
	if err != nil {
		return err
	}
	if warning != "" {
		// Renamed: exit zero, because it was, and say what did not follow.
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
		return nil
	}
	fmt.Printf("renamed %s to %s\n", from, to)
	return nil
}

func cmdRemove(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	sock := socketFlag(fs)
	keepLogs := fs.Bool("keep-logs", false, "leave the service's log file on disk")
	if err := fs.Parse(hoistName(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("rm needs at least one service name")
	}
	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	return eachService(fs.Args(), func(name string) error {
		logs, err := c.Remove(name, *keepLogs)
		if err != nil {
			return err
		}
		// Confirm in words, and name the files. A command that removes something and prints
		// nothing leaves you wondering whether it did anything at all - and a log that was
		// deleted without being mentioned is worse, because for a shell that file was the whole
		// transcript.
		fmt.Printf("removed %s\n", name)
		for _, l := range logs {
			fmt.Printf("removed its log %s\n", l)
		}
		return nil
	})
}

// eachService applies an operation to every named service, and keeps going when one of them fails.
//
// Stopping at the first failure is wrong here for the same reason `rm a b c` in a shell does not
// stop: the names are independent, and a typo in the second one should not silently leave the
// third running. Every failure is reported as it happens, so the output stays in step with what
// was actually done, and the exit status says how many.
//
// With a single name the error is returned unchanged, so `stop nosuch` still prints exactly what
// it printed before rather than a summary wrapped around it.
func eachService(names []string, do func(string) error) error {
	if len(names) == 1 {
		return do(names[0])
	}
	failed := 0
	for _, name := range names {
		if err := do(name); err != nil {
			fmt.Fprintf(os.Stderr, "gozellij: %s: %v\n", name, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d services failed", failed, len(names))
	}
	return nil
}

// boolFlags are the flags that take no value, which is all hoistName needs in order to tell a
// flag's value apart from the service name.
var boolFlags = map[string]bool{
	"-start": true, "--start": true,
	"-no-replay": true, "--no-replay": true,
}

// hoistName moves the first bare word (the service name) in front of the flags, and stops at "--"
// so the command's own flags are never touched.
//
// Without this, `gozellij add web -restart always -- caddy run` parses as a service named "web"
// whose command is "-restart" - an invocation that succeeds and does something absurd, which is
// the failure mode this project is named after avoiding.
func hoistName(args []string) []string {
	var (
		name  string
		flags []string
		tail  []string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			tail = args[i:]
			break
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			// A non-boolean flag written as "-flag value" consumes the next token; one
			// written as "-flag=value" does not.
			if !boolFlags[a] && !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if name == "" {
			name = a
			continue
		}
		// A second bare word before "--" is the start of the command.
		tail = args[i:]
		break
	}

	out := flags
	if name != "" {
		out = append(out, name)
	}
	return append(out, tail...)
}

// presentation helpers

func stateWord(s ipc.StatusReply) string {
	if s.State == "failed" && s.LastError != "" {
		return "failed"
	}
	return s.State
}

func pidWord(s ipc.StatusReply) string {
	if s.Pid == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", s.Pid)
}

func restartWord(s ipc.StatusReply) string {
	if s.Restarts == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", s.Restarts)
}

func uptime(s ipc.StatusReply) string {
	if s.StartedAt.IsZero() || s.Pid == 0 {
		return "-"
	}
	return time.Since(s.StartedAt).Round(time.Second).String()
}

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }
