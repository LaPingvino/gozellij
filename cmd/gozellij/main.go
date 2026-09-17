// Command gozellij is the command line client for the fabric daemon.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
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
  gozellij ls                          list services
  gozellij status <name>               show one service
  gozellij add <name> -- <cmd> [args]  define a service
  gozellij start|stop|restart <name>   change its state
  gozellij attach <name>               attach your terminal to it (Ctrl-] detaches)
  gozellij logs <name>                 print its recent output and exit
  gozellij upgrade                     replace the daemon binary, keeping every process
  gozellij rm <name>                   remove it
  gozellij ping                        check the daemon is alive

Flags for add:
  -restart no|on-failure|always   what to do when it exits (default no)
  -dir <path>                     working directory
  -env KEY=VALUE                  repeatable
  -start                          start it immediately

Global:
  -socket <path>   daemon socket (default $XDG_RUNTIME_DIR/gozellij/fabric.sock)

The daemon is gozellijd. Services keep running when it stops.
`, Version)
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		os.Exit(2)
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
	w.Flush()
}

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	sock := socketFlag(fs)
	restart := fs.String("restart", "no", "no|on-failure|always")
	dir := fs.String("dir", "", "working directory")
	start := fs.Bool("start", false, "start it immediately")
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
	c, err := connect(*sock)
	if err != nil {
		return err
	}
	defer c.Close()

	return c.Attach(fs.Arg(0), os.Stdin, os.Stdout, !*noReplay)
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	sock := socketFlag(fs)
	maxBytes := fs.Int("n", 0, "show at most this many bytes from the end (0 = everything kept)")
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

	out, err := c.Logs(fs.Arg(0), *maxBytes)
	if err != nil {
		return err
	}
	if out.Truncated {
		fmt.Fprintf(os.Stderr, "[showing the last %d bytes; older output was dropped]\n", len(out.Data))
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
