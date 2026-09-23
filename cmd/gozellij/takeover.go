package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// What is trapped in the other multiplexer, and what to type to get it back.
//
// The honest limit first, because everything here follows from it: gozellij cannot take a running
// terminal away from another program. The pty master is a descriptor inside that process, and the
// kernel will not copy it out without either that process's cooperation (SCM_RIGHTS, which means
// patching it) or ptrace permission over it (pidfd_getfd, which Linux's default ptrace_scope of 1
// refuses for anything that is not your own child). Neither is available to a program that arrives
// after the fact.
//
// So this does the thing that is available and useful: it says what is in there. Which terminals,
// what is running on each, and which directory it is in - read from /proc, needing no permission
// beyond being the same user - and then the commands that put you back in the same directories
// under gozellij, and the one that ends the old session when you are ready.
//
// That is not a handover and this does not pretend otherwise. It is the difference between losing
// a session and losing the processes in it, which on a machine you reach only over ssh is most of
// the difference that matters.

// ptyHolder is a process holding pty masters that is not this program's daemon.
type ptyHolder struct {
	Pid      int
	Name     string
	Terminal []terminalIn
}

// terminalIn is one terminal inside another multiplexer.
type terminalIn struct {
	Pts     int
	LeafPid int
	Dir     string
	Running string
}

func cmdTakeover(args []string) error {
	fs := flag.NewFlagSet("takeover", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	holders, err := findPTYHolders()
	if err != nil {
		return err
	}
	if len(holders) == 0 {
		fmt.Println("Nothing else on this machine is holding a terminal.")
		return nil
	}

	// Which of them, if any, owns the terminal this is being read on. Without this the advice at
	// the bottom says `kill 6084` about the session the person is sitting in, which is the worst
	// thing a helpful message has ever done.
	mine := holderOfMyTerminal(holders)

	for _, h := range holders {
		here := ""
		if h.Pid == mine {
			here = "  <- you are in this one"
		}
		fmt.Printf("%s (pid %d) is holding %d terminal(s):%s\n\n", h.Name, h.Pid, len(h.Terminal), here)
		for _, t := range h.Terminal {
			running := t.Running
			if running == "" {
				running = "(an idle shell)"
			}
			fmt.Printf("  /dev/pts/%-3d  %-32s  %s\n", t.Pts, shortHome(t.Dir), running)
		}
		fmt.Println()
	}

	fmt.Println("gozellij cannot take these over. The terminal itself lives inside that process, and")
	fmt.Println("the kernel will not hand it to another program without that program's help - which")
	fmt.Println("means patching it - or ptrace permission over it, which Linux refuses by default.")
	fmt.Println()
	fmt.Println("What it can do is put you back where you were. To recreate these under gozellij:")
	fmt.Println()
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	for _, h := range holders {
		for i, t := range h.Terminal {
			fmt.Printf("  gozellij add %s -dir %s -start -- %s -l\n",
				suggestedName(t, i), t.Dir, shell)
		}
	}
	fmt.Println()
	fmt.Println("Whatever was *running* in them does not come back - a shell can be recreated, the")
	fmt.Println("program inside it cannot. Finish or note anything you care about first.")
	fmt.Println()
	fmt.Println("Then, when nothing you want is left in the old session:")
	fmt.Println()
	for _, h := range holders {
		if h.Pid == mine {
			fmt.Printf("  kill %d      # NOT from in here: this is the session you are reading this in.\n", h.Pid)
			fmt.Printf("               # Detach or log in another way first, or you end this with it.\n")
			continue
		}
		fmt.Printf("  kill %d\n", h.Pid)
	}
	return nil
}

// holderOfMyTerminal is which of these is holding the terminal this process is on, or zero.
//
// Found by the number rather than by an environment variable: $ZELLIJ and $TMUX say you are inside
// *a* multiplexer, not which process, and they are inherited by things that have since moved.
func holderOfMyTerminal(holders []ptyHolder) int {
	n, ok := ptsOfProcess(os.Getpid())
	if !ok {
		return 0
	}
	for _, h := range holders {
		for _, t := range h.Terminal {
			if t.Pts == n {
				return h.Pid
			}
		}
	}
	return 0
}

// suggestedName is a service name a person would recognise: the directory they were in.
func suggestedName(t terminalIn, i int) string {
	base := filepath.Base(t.Dir)
	if base == "" || base == "." || base == "/" || base == os.Getenv("HOME") {
		base = "shell"
	}
	// Service names go on to be file names for logs, so keep them to something plain.
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '-'
		}
	}, base)
	if clean == "" {
		return fmt.Sprintf("old-%d", i+1)
	}
	return clean
}

func shortHome(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rest, ok := strings.CutPrefix(path, home); ok {
			return "~" + rest
		}
	}
	return path
}

// findPTYHolders looks for processes holding pty masters, and works out what is on the far end.
//
// Everything here is /proc and nothing is privileged: the descriptor list of your own processes,
// the tty index the kernel records for a held master, and the working directory of a process you
// own. Nothing is opened, nothing is taken.
func findPTYHolders() ([]ptyHolder, error) {
	pids, err := allPids()
	if err != nil {
		return nil, err
	}
	// Which pts each process is sitting on, so a master can be matched to what is using it.
	onPts := map[int][]int{}
	for _, pid := range pids {
		if n, ok := ptsOfProcess(pid); ok {
			onPts[n] = append(onPts[n], pid)
		}
	}

	var out []ptyHolder
	me := os.Getpid()
	for _, pid := range pids {
		if pid == me {
			continue
		}
		name := processName(pid)
		// Our own daemon's terminals are not "trapped in another multiplexer".
		if name == "gozellijd" || name == "gozellij" {
			continue
		}
		indexes := heldPTYIndexes(pid)
		if len(indexes) == 0 {
			continue
		}
		h := ptyHolder{Pid: pid, Name: name}
		for _, n := range indexes {
			t := terminalIn{Pts: n}
			// The process on that terminal that is deepest in the tree is what is running;
			// the shallowest is the shell, and its directory is the one worth keeping.
			if users := onPts[n]; len(users) > 0 {
				sort.Ints(users)
				t.LeafPid = users[0]
				t.Dir = cwdOf(users[0])
				for _, u := range users {
					if cmd := cmdlineOf(u); cmd != "" && !isShell(cmd) {
						t.Running = cmd
					}
				}
			}
			if t.Dir == "" {
				t.Dir = os.Getenv("HOME")
			}
			h.Terminal = append(h.Terminal, t)
		}
		sort.Slice(h.Terminal, func(i, j int) bool { return h.Terminal[i].Pts < h.Terminal[j].Pts })
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pid < out[j].Pid })
	return out, nil
}

func allPids() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("cannot read /proc: %w", err)
	}
	var pids []int
	for _, e := range entries {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, n)
		}
	}
	return pids, nil
}

// heldPTYIndexes is which pts numbers a process holds the master of.
//
// The kernel records the number in fdinfo, so this needs no ioctl and so no access to the
// descriptor itself - which is the only reason this works at all on somebody else's process.
func heldPTYIndexes(pid int) []int {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil || target != "/dev/ptmx" {
			continue
		}
		body, err := os.ReadFile(fmt.Sprintf("/proc/%d/fdinfo/%s", pid, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			if rest, ok := strings.CutPrefix(line, "tty-index:"); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
					out = append(out, n)
				}
			}
		}
	}
	sort.Ints(out)
	return out
}

// ptsOfProcess is which pts a process has as its standard input, or false.
func ptsOfProcess(pid int) (int, bool) {
	target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", pid))
	if err != nil {
		return 0, false
	}
	rest, ok := strings.CutPrefix(target, "/dev/pts/")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}

func processName(pid int) string {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func cwdOf(pid int) string {
	dir, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return ""
	}
	return dir
}

func cmdlineOf(pid int) string {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimRight(string(body), "\x00"), "\x00")
	return strings.Join(parts, " ")
}

// isShell keeps the shell itself from being reported as "what is running in this terminal", which
// would make every idle terminal look busy.
func isShell(cmd string) bool {
	base := filepath.Base(strings.Fields(cmd + " ")[0])
	switch strings.TrimPrefix(base, "-") {
	case "bash", "sh", "zsh", "fish", "dash", "ksh", "tcsh", "csh":
		return true
	}
	return false
}
