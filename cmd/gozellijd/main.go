// Command gozellijd is the fabric daemon: it owns the processes, their ptys and their state, and
// serves them over a unix socket.
//
// It is meant to outlive every UI that talks to it. Stopping the daemon does not stop the
// services; see the note on Close in internal/daemon.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/LaPingvino/gozellij/internal/daemon"
	"github.com/LaPingvino/gozellij/internal/fabric"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	var (
		socket  = flag.String("socket", "", "unix socket path (default: $XDG_RUNTIME_DIR/gozellij/fabric.sock)")
		state   = flag.String("state", "", "directory for service definitions (default: $XDG_STATE_HOME/gozellij)")
		verbose = flag.Bool("v", false, "log at debug level")
		version = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *version {
		fmt.Printf("gozellijd %s\n", Version)
		return
	}

	if err := run(*socket, *state, *verbose); err != nil {
		fmt.Fprintf(os.Stderr, "gozellijd: %v\n", err)
		os.Exit(1)
	}
}

func run(socket, state string, verbose bool) error {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if socket == "" {
		socket = daemon.SocketPath()
	}
	if state == "" {
		state = daemon.StateDir()
	}

	reg, err := fabric.NewRegistry(filepath.Join(state, "services"))
	if err != nil {
		return err
	}
	fab := fabric.NewFabric(reg, fabric.StartOptions{})

	// Bring back what was running before. Load reports every problem rather than the first, and
	// they are logged individually: one unreadable service file must not stop the other nine,
	// and must not disappear either.
	for _, err := range fab.Load() {
		log.Error("problem while loading services", "err", err)
	}

	srv, err := daemon.Listen(socket, fab, log)
	if err != nil {
		return err
	}
	log.Info("gozellij daemon listening", "socket", srv.Addr(), "state", state, "version", Version)

	// A signal means "stop serving", not "kill everything". The services are the point; the
	// daemon is replaceable, which is the whole design. A second signal is the escalation for
	// someone who really does want it all to stop.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	select {
	case err := <-serveErr:
		return err
	case sig := <-sigs:
		log.Info("shutting down the daemon; services keep running", "signal", sig.String())
		if err := srv.Close(); err != nil {
			log.Warn("error while closing", "err", err)
		}
		return nil
	}
}
