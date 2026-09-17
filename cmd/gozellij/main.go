// Command gozellij is the CLI front end to the fabric.
//
// Right now it only reports what it is, so that the repository builds and runs from the first
// commit. Subcommands arrive with Phase 1 (see DESIGN.md).
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `gozellij %s - a host-native process fabric

Usage: gozellij <command> [flags]

No commands are implemented yet. See DESIGN.md for the plan; Phase 1 is the
fabric daemon plus a single-pane attach.

Flags:
`, Version)
		flag.PrintDefaults()
	}
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("gozellij %s (%s %s/%s)\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}

	if flag.NArg() == 0 {
		flag.Usage()
		// Exit 2 for "you did not tell me what to do", distinct from a command that ran and
		// failed. A CLI that exits 0 having done nothing is the failure mode this project
		// exists partly to stop repeating.
		os.Exit(2)
	}

	fmt.Fprintf(os.Stderr, "gozellij: %q is not implemented yet (see DESIGN.md)\n", flag.Arg(0))
	os.Exit(2)
}
