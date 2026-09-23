// Command gozellij is the command line client for the fabric daemon.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/LaPingvino/gozellij/internal/daemon"
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
	fmt.Fprintf(os.Stderr, `gozellij %[1]s - a host-native process fabric

Usage:
  gozellij                             land in your shell (starts one if there is none)
  gozellij ls                          list services
  gozellij status <name>...            show one service, or several
  gozellij add <name> -- <cmd> [args]  define a service
  gozellij start|stop|restart <name>.. change the state of one or several
  gozellij attach <name> [-render]     attach your terminal to it
  gozellij logs <name>                 print its recent output and exit
  gozellij logs -f <name>...           follow one or several services until you press Ctrl-C
  gozellij upgrade                     replace the daemon binary, keeping every process
  gozellij shell [-name <n>] [-render] the same, with a different service name
  gozellij rm <name>... [-keep-logs]   stop them, forget them, delete their logs
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

logs reads the file on disk, which outlives the daemon; logs -f follows the daemon's live buffer,
which does not. A service added with -log off has no file, and logs then falls back to that
buffer - which holds a few hundred KiB and dies with the daemon.

The status line at the bottom of an attach is byobu-shaped: the same widget names, and a leading
# in the config switches one off. See gozellij stats -example.

While attached, %[2]s is gozellij's own key. Set prefix=C-b in the config file
to change it; gozellij doctor says which key is in force.
  %[2]s d         detach; the service keeps running
  %[2]s n / p     next / previous service, in this same terminal
  %[2]s l         list the services and pick one by number
  %[2]s c         a new shell, in the directory you are in
  %[2]s k         remove the service you are looking at
  %[2]s u         revive a pane that has frozen: reconnect it, starting its service if needed
  %[2]s ?         show these keys
  %[2]s %[2]s    send a literal %[2]s to the service

Global:
  -socket <path>   daemon socket (default $XDG_RUNTIME_DIR/gozellij/fabric.sock)

The daemon is gozellijd. Services keep running when it stops.
`, Version, status.PrefixLabel(status.Load().Prefix))
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
func refuseNesting(target string, nest bool) error {
	inside := os.Getenv("GOZELLIJ")
	if inside == "" {
		return nil
	}
	label := status.PrefixLabel(status.Load().Prefix)
	if inside == target {
		return fmt.Errorf("you are already inside %s - attaching it to itself would draw its own "+
			"output back into itself.\nIf this pane has frozen, %s u reconnects it", target, label)
	}
	if nest {
		return nil
	}
	return fmt.Errorf("you are inside %s already, and a gozellij inside gozellij cannot be reached: "+
		"the outer one takes %s.\n%s l picks %s from here, in place. To nest anyway: gozellij attach -nest %s",
		inside, label, label, target, target)
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
func cmdShell(args []string) error {
	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	sock := socketFlag(fs)
	name := fs.String("name", DefaultShellService, "the service to land in")
	nest := fs.Bool("nest", false, "land even from inside another gozellij service")
	render := renderFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Bare `gozellij`, typed inside a gozellij shell, is the same nesting as `attach` - and the
	// same feedback loop when the shell it would land in is this one.
	if err := refuseNesting(*name, *nest); err != nil {
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
	if err := refuseNesting(fs.Arg(0), *nest); err != nil {
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

	if *follow {
		return followLogs(*sock, fs.Args(), *maxBytes)
	}

	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	out, err := c.Logs(fs.Arg(0), *maxBytes)
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
	return nil, fmt.Errorf("the daemon did not come back within %v (last error: %v); check its log",
		within, lastErr)
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
	back, err := waitForDaemon(path, upgradeWait)
	if err != nil {
		return err
	}
	defer back.Close()

	newVersion, _ := back.PingVersion()
	after, err := back.List()
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
