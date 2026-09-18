package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/LaPingvino/gozellij/internal/daemon"
	"github.com/LaPingvino/gozellij/internal/status"
)

// cmdStats prints the status line once.
//
// The same line the attach draws at the bottom of the terminal, on stdout and gone. It exists so
// that "what would that say?" can be answered without attaching to anything, and so that a
// configuration can be checked before it is trusted to draw itself over your shell.
func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	sock := socketFlag(fs)
	service := fs.String("service", "", "the service to report on (default: the shell)")
	width := fs.Int("width", 0, "pad to this width; 0 prints the fields without padding")
	list := fs.Bool("list", false, "list the widget names and exit")
	example := fs.Bool("example", false, "print a configuration file with the defaults and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *list {
		fmt.Println(strings.Join(status.Names(), "\n"))
		return nil
	}
	if *example {
		fmt.Print(status.Example())
		return nil
	}

	cfg := status.Load()
	// Problems on stderr, the line on stdout: a misspelt widget should be visible without
	// corrupting a line somebody is piping somewhere.
	for _, p := range cfg.Problems {
		fmt.Fprintf(os.Stderr, "gozellij: %s\n", p)
	}

	path := *sock
	if path == "" {
		path = daemon.SocketPath()
	}
	name := *service
	if name == "" {
		name = DefaultShellService
	}

	line := status.Render(daemon.StatusContext(path, name), cfg.Left, cfg.Right, *width)
	if strings.TrimSpace(line) == "" {
		return errors.New("every widget had nothing to say; is this a Linux machine, and is the daemon running?")
	}
	fmt.Println(line)
	return nil
}
