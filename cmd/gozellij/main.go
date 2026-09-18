// Command gozellij is the command line client for the fabric daemon.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/LaPingvino/gozellij/internal/daemon"
	"github.com/LaPingvino/gozellij/internal/ipc"
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
	fmt.Fprintf(os.Stderr, `gozellij %s - a host-native process fabric

Usage:
  gozellij                             land in your shell (starts one if there is none)
  gozellij ls                          list services
  gozellij status <name>               show one service
  gozellij add <name> -- <cmd> [args]  define a service
  gozellij start|stop|restart <name>   change its state
  gozellij attach <name>               attach your terminal to it (Ctrl-] detaches)
  gozellij logs <name>                 print its recent output and exit
  gozellij logs -f <name>              follow its output until you press Ctrl-C
  gozellij upgrade                     replace the daemon binary, keeping every process
  gozellij shell [-name <name>]        the same, with a different service name
  gozellij rm <name>                   remove it
  gozellij ping                        check the daemon is alive

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

Global:
  -socket <path>   daemon socket (default $XDG_RUNTIME_DIR/gozellij/fabric.sock)

The daemon is gozellijd. Services keep running when it stops.
`, Version)
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
	default:
		usage()
		// Name it. "Unknown command" without saying which is a small unkindness that adds up.
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

// cmdShell lands you in a shell, creating one if there is not one already.
//
// The rule, which is worth stating because it decides what happens to a customised service: if the
// shell is running, you are reattached to it, environment and all, exactly as you left it. If it is
// not running, it is defined again from the terminal you are typing in now - because a shell that
// is not running has nothing worth keeping, and inheriting a TERM from three logins ago is how you
// end up with a vim that draws garbage. Keep a shell you have customised under another name and
// attach to it by name.
func cmdShell(args []string) error {
	fs := flag.NewFlagSet("shell", flag.ContinueOnError)
	sock := socketFlag(fs)
	name := fs.String("name", DefaultShellService, "the service to land in")
	if err := fs.Parse(args); err != nil {
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

	running, known, err := shellState(c, *name)
	if err != nil {
		c.Close()
		return err
	}

	if !running {
		if known {
			// Defined but not running: replace the definition, so the shell you are about to
			// get belongs to the terminal you are in.
			if err := c.Remove(*name); err != nil {
				c.Close()
				return fmt.Errorf("replacing the old %s definition: %w", *name, err)
			}
		}
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		home, _ := os.UserHomeDir()
		env := captureShellEnv()
		// Say where you are, the way tmux sets $TMUX and zellij sets $ZELLIJ.
		//
		// This is not decoration. A login shell runs your profile, and profiles start
		// multiplexers: the first time this was run by hand it dropped straight into gezellij,
		// because ~/.profile autostarts it and nothing said "you are already in one". Anything
		// that autostarts a session should guard on this variable.
		env = append(env, "GOZELLIJ="+*name)
		if _, err := c.Add(*name, ipc.AddRequest{
			Command: shell,
			// A login shell: under systemd the daemon's environment is nearly empty, so the
			// shell has to build its own rather than inherit one that was never set up.
			Args:    []string{"-l"},
			Dir:     home,
			Env:     env,
			Restart: "no",
			Start:   true,
		}); err != nil {
			c.Close()
			return err
		}
	}
	c.Close()

	return daemon.AttachLoop(path, *name, os.Stdin, os.Stdout, true)
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
		fmt.Fprintln(w, "NAME\tSTATE\tPID\tUPTIME\tRESTARTS\tCOMMAND")
		for _, s := range list.Services {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				s.Service, stateWord(s), pidWord(s), uptime(s), restartWord(s), s.Command)
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
	if fs.NArg() != 1 {
		return errors.New("status needs exactly one service name")
	}
	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	s, err := c.Status(fs.Arg(0))
	if err != nil {
		return err
	}
	printStatus(s)
	return nil
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
	if s.Restarts > 0 {
		fmt.Fprintf(w, "restarts:\t%d\n", s.Restarts)
	}
	if !s.NextRestart.IsZero() && time.Until(s.NextRestart) > 0 {
		fmt.Fprintf(w, "next try:\tin %s\n", time.Until(s.NextRestart).Round(time.Second))
	}
	if s.HasExited {
		if s.ExitSignal != "" {
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

	name := fs.Arg(0)
	cmdArgs := fs.Args()[1:]
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
	if fs.NArg() != 1 {
		return fmt.Errorf("%s needs exactly one service name", op)
	}
	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	name := fs.Arg(0)
	var s ipc.StatusReply
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
	if err := fs.Parse(hoistName(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("attach needs exactly one service name")
	}
	path := *sock
	if path == "" {
		path = daemon.SocketPath()
	}
	// AttachLoop rather than a single attach: a daemon upgrade takes the socket with it, and a
	// terminal that silently returns to a shell prompt cannot tell you whether your service died
	// or the daemon was replaced.
	return daemon.AttachLoop(path, fs.Arg(0), os.Stdin, os.Stdout, !*noReplay)
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	sock := socketFlag(fs)
	maxBytes := fs.Int("n", 0, "show at most this many bytes from the end (0 = as much as fits)")
	follow := fs.Bool("f", false, "follow the live output until interrupted")
	if err := fs.Parse(hoistName(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("logs needs exactly one service name")
	}
	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	if *follow {
		// Ctrl-C is how you stop following, so it must end this cleanly rather than looking
		// like a crash. Closing the connection unblocks the read.
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-stop
			c.Close()
		}()
		return c.FollowLogs(fs.Arg(0), *maxBytes, os.Stdout, os.Stderr)
	}

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
	if err := fs.Parse(hoistName(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("rm needs exactly one service name")
	}
	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	name := fs.Arg(0)
	if err := c.Remove(name); err != nil {
		return err
	}
	// Confirm in words. A command that removes something and prints nothing leaves you
	// wondering whether it did anything at all.
	fmt.Printf("removed %s\n", name)
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
