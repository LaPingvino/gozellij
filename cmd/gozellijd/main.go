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
	// Detected once, at startup: it depends on how this daemon was started, which does not
	// change while it runs. `gozellij doctor` reports which mode is in force, because a
	// tree-kill that is silently only a process-group kill is the worst of both.
	cgroups := fabric.DetectCgroups()
	if cgroups.Available() {
		log.Info("services will be stopped by cgroup", "cgroup", cgroups.Root())
	} else {
		log.Info("services will be stopped by process group; a child that calls setsid will survive",
			"why", cgroups.Why())
	}

	// Whatever systemd is holding for us, taken out of the environment once and held until
	// something claims it: the services' terminals below, and the listening socket in Listen.
	daemon.CollectStoredFDs()

	fab := fabric.NewFabric(reg, fabric.StartOptions{
		LogDir:  logs,
		Cgroups: cgroups,
		// Each service's terminal goes to systemd to hold, and is dropped when its process
		// ends. This is what lets the *next* daemon pick the services up after a crash; see
		// internal/daemon/ptystore.go and docs/USER_STORIES.md C3.
		OnRunning: func(name string, pid int, pty *os.File) { daemon.StorePTY(log, name, pid, pty) },
		OnEnded:   func(name string, pid int) { daemon.DropPTY(log, name, pid) },
	})

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
	switch recovered := daemon.RecoveredPTYs(log); {
	case handover != nil:
		log.Info("adopting processes from the previous daemon",
			"count", len(handover.Processes), "pid", os.Getpid(), "was", handover.FromPid)
		problems = fab.AdoptAll(handover.Processes)

	case len(recovered) > 0:
		// No handover, but systemd was holding terminals: this daemon is the replacement for
		// one that died. The services never stopped - they were children of a process, not
		// parts of it - and what was lost was the terminal they were talking to.
		log.Info("recovering services from a daemon that did not shut down cleanly",
			"count", len(recovered))
		problems = fab.AdoptAll(recovered)

	default:
		// Bring back what was running before. Load reports every problem rather than the
		// first: one unreadable service file must not stop the other nine, and must not
		// disappear either.
		problems = fab.Load()
	}
	for _, err := range problems {
		log.Error("problem while loading services", "err", err)
	}

	// Said once, here, if there is nowhere to keep the terminals: the promise in this program's
	// own README is not being kept on this machine, and silence about that is exactly the
	// failure the design rules are about.
	daemon.NoteIfNothingIsHolding(log, len(fab.List()))

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

	// Nothing else is going to claim a descriptor systemd handed back, so whatever is left is
	// closed and dropped from the store. A store quietly accumulating things nothing will ever
	// use is a leak wearing a feature's clothes.
	daemon.ReleaseUnadoptedFDs(log)

	// Type=notify waits for this before it calls the unit started, and the unit has to be
	// Type=notify for the file-descriptor store to be usable at all. Outside systemd there is
	// nowhere to send it, which is not a failure.
	if err := daemon.NotifyReady(); err != nil && err != daemon.ErrNoNotifySocket {
		log.Warn("could not tell systemd we are up", "err", err)
	}

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
