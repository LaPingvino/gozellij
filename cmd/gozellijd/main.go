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
	"time"

	"github.com/LaPingvino/gozellij/internal/daemon"
	"github.com/LaPingvino/gozellij/internal/fabric"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "dev"

// upgradeReplyGrace is how long the daemon waits after accepting an upgrade request before
// replacing itself, so the client's answer is on the wire first. The exec closes the socket, and a
// reply that arrives after the socket has gone is a reply nobody reads.
const upgradeReplyGrace = 150 * time.Millisecond

func main() {
	var (
		socket  = flag.String("socket", "", "unix socket path (default: $XDG_RUNTIME_DIR/gozellij/fabric.sock)")
		state   = flag.String("state", "", "directory for service definitions (default: $XDG_STATE_HOME/gozellij)")
		logs    = flag.String("logs", "", "directory for service output logs (default: <state>/logs; \"off\" keeps output in RAM only)")
		verbose = flag.Bool("v", false, "log at debug level")
		version = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *version {
		fmt.Printf("gozellijd %s\n", Version)
		return
	}

	if err := run(*socket, *state, *logs, *verbose); err != nil {
		fmt.Fprintf(os.Stderr, "gozellijd: %v\n", err)
		os.Exit(1)
	}
}

func run(socket, state, logs string, verbose bool) error {
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

	// Logs go beside the definitions, under the state directory, because they answer the same
	// kind of question a definition does - what is this machine doing, and what did it do - and
	// because the runtime directory is wiped on reboot, which is exactly when you want them.
	switch logs {
	case "":
		logs = filepath.Join(state, "logs")
	case "off":
		logs = ""
	}

	reg, err := fabric.NewRegistry(filepath.Join(state, "services"))
	if err != nil {
		return err
	}
	fab := fabric.NewFabric(reg, fabric.StartOptions{LogDir: logs})

	// Did a predecessor hand its processes over? If so we are the *same process* it was - the
	// exec kept the pid - and the ptys it opened are still open on the descriptors it names.
	handover, herr := daemon.TakeHandover()
	if herr != nil {
		// A handover we cannot read is not fatal, but it must be loud: the services it
		// described are about to be started fresh instead of adopted, and somebody should
		// know why their pids changed.
		log.Error("could not take the handover from the previous daemon; services will be started fresh", "err", herr)
	}

	var problems []error
	if handover != nil {
		log.Info("adopting processes from the previous daemon",
			"count", len(handover.Processes), "pid", os.Getpid(), "was", handover.FromPid)
		problems = fab.AdoptAll(handover.Processes)
	} else {
		// Bring back what was running before. Load reports every problem rather than the
		// first: one unreadable service file must not stop the other nine, and must not
		// disappear either.
		problems = fab.Load()
	}
	for _, err := range problems {
		log.Error("problem while loading services", "err", err)
	}

	srv, err := daemon.Listen(socket, fab, log)
	if err != nil {
		return err
	}
	srv.SetVersion(Version)
	log.Info("gozellij daemon listening", "socket", srv.Addr(), "state", state, "logs", logs, "version", Version)

	// A signal means "stop serving", not "kill everything". The services are the point; the
	// daemon is replaceable, which is the whole design.
	//
	// SIGUSR1 is the upgrade: replace this binary with the one on disk without the processes
	// noticing. See DESIGN.md.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	for {
		select {
		case err := <-serveErr:
			return err

		case <-srv.Upgrades():
			// A client asked. Give its answer a moment to reach the socket before the exec
			// takes the socket away with it - the reply says what is about to happen, and it
			// is no use arriving after the thing it describes.
			time.Sleep(upgradeReplyGrace)
			if err := upgrade(srv, fab, log); err != nil {
				log.Error("upgrade failed; carrying on with the current binary", "err", err)
				continue
			}
			log.Info("shutting down the daemon; services keep running", "reason", "upgrade")
			return nil

		case sig := <-sigs:
			if sig == syscall.SIGUSR1 {
				if err := upgrade(srv, fab, log); err != nil {
					// Still here, so the exec did not happen. Carry on serving rather
					// than dying: a failed upgrade must not become an outage.
					log.Error("upgrade failed; carrying on with the current binary", "err", err)
					continue
				}
			}
			log.Info("shutting down the daemon; services keep running", "signal", sig.String())
			if err := srv.Close(); err != nil {
				log.Warn("error while closing", "err", err)
			}
			return nil
		}
	}
}

// upgrade replaces this daemon with the binary now on disk, keeping the running processes.
//
// It only returns on failure; on success the image is gone mid-call.
func upgrade(srv *daemon.Server, fab *fabric.Fabric, log *slog.Logger) error {
	manifest, problems := daemon.PrepareHandover(fab)
	for _, err := range problems {
		// Each of these is a service that will *not* survive. Name them before the exec, while
		// there is still something to log with.
		log.Error("process will not survive the upgrade", "err", err)
	}

	log.Info("upgrading in place", "processes", len(manifest.Processes), "pid", os.Getpid())

	// Deliberately *not* closing the listener first.
	//
	// The obvious order - close, then exec - has a trap in it: if the exec fails, the daemon is
	// still running but is serving nothing, and a failed upgrade has silently become an outage.
	// The listener's descriptor is close-on-exec like everything else Go opens, so a successful
	// exec closes it for us; the socket file is then stale, and the successor reclaims it the
	// same way it would after a crash. A failed exec, meanwhile, leaves us listening exactly as
	// before, which is the whole point.
	return daemon.ExecSelf(manifest, "")
}
