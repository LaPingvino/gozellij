package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/LaPingvino/gozellij/internal/fabric"
	"github.com/LaPingvino/gozellij/internal/ipc"
	"github.com/LaPingvino/gozellij/internal/status"
	"golang.org/x/term"
)

// PrefixKey introduces a command to gozellij itself rather than to the service you are attached to.
//
// Ctrl-] , as telnet has used for decades. Everything else on the wire is the service's: the
// attach is a byte pipe, so exactly one key can be ours and it has to be one no program wants.
//
// It was a bare detach key until services became switchable. A prefix costs one extra keystroke to
// detach and buys every other command there will ever be, which is the trade tmux and zellij both
// made; `Ctrl-] Ctrl-]` sends a literal Ctrl-] through for the programs that do want it.
const PrefixKey = status.DefaultPrefix

// DetachKey is the old name for PrefixKey, kept because it is referenced from docs and tests.
const DetachKey = PrefixKey

// attachOutcome is why an attach session ended. The difference between these is the difference
// between reconnecting, moving somewhere else, and going home.
type attachOutcome int

const (
	// outcomeDisconnected means the stream ended without us asking - a daemon upgrade, usually.
	outcomeDisconnected attachOutcome = iota
	// outcomeDetached means the user asked to leave.
	outcomeDetached
	// outcomeFinished means the service is over and said so.
	outcomeFinished
	// outcomeNext and outcomePrev mean the user asked for a different service in the same
	// terminal - tabs, without a terminal emulator anywhere in the picture.
	outcomeNext
	outcomePrev
	// outcomeList means the user asked to see what there is and pick one.
	outcomeList
	// outcomeSplit, outcomeFocus and outcomeClosePane are the split-screen commands. They only
	// mean anything in a rendered session - a byte pipe has one screen and no way to divide it -
	// and the reader says so rather than letting the key do nothing.
	outcomeSplit
	// outcomeSplitRows is the same thing stacked: something underneath your shell rather than
	// beside it, which is what you want when the terminal is narrow or the pane is a log.
	outcomeSplitRows
	outcomeFocus
	outcomeClosePane
	// outcomeScrollBack, outcomeScrollForward and outcomeScrollLive look through a pane's
	// scrollback. Only a rendered session has any: a byte pipe leaves the scrollback to the
	// user's own terminal, which is one of the things it is better at.
	outcomeScrollBack
	outcomeScrollForward
	outcomeScrollLive
	// outcomeGrow and outcomeShrink change the focused pane's share of the screen.
	outcomeGrow
	outcomeShrink
	// outcomeRemove removes the service being looked at - stops it and forgets it, the way
	// `gozellij rm` does. Not merely stop: a stopped service lingers in the list and on the tab
	// bar, and "I am done with this" means gone. A multiplexer that can show you a thing and not
	// get rid of it is asking you to leave and use another command for it.
	outcomeRemove
	// outcomeRevive reconnects the pane you are looking at to its service, starting the service
	// only if it is not running. For a pane that has frozen by accident - a connection that went
	// stale across a daemon crash, or the two-readers bug - while the process behind it is fine.
	// Not the undo of k: k removes a service on purpose, and there is nothing left to revive.
	outcomeRevive
	// outcomeCreate starts a new shell and shows it - the new-tab key every other multiplexer has.
	outcomeCreate
	// outcomeRedraw paints everything again from the grid. The recovery for a screen that looks
	// wrong, which is the first thing anybody reaches for.
	outcomeRedraw
	// outcomeRename asks for a new name for the service you are looking at and gives it that.
	outcomeRename
)

// prefixHelp is what Ctrl-] ? prints. Short on purpose: it is displayed over whatever the service
// was showing.
// prefixHelp is what `<prefix> ?` prints. Short on purpose: it is displayed over whatever the
// service was showing. Built from the configured key rather than spelling Ctrl-] out, because a
// help text that names a key the user has changed is worse than none.
func prefixHelp(label string) string {
	return label + " d detach · c new shell · n/p next/previous · l list and pick · , rename · k remove · u revive · " +
		"| split beside · - split below · < > resize · o switch pane · x close pane · " +
		"b/f scroll back/forward · g live · r redraw · ? this · " + label + " sends a literal " + label
}

// pickTimeout is how long the list waits for a choice before giving up and going back.
//
// Long enough to read a list of services, short enough that a key pressed by accident does not
// leave the terminal apparently frozen with no explanation.
const pickTimeout = 30 * time.Second

// ReattachWindow is how long an attached client keeps trying to get back in after the stream ends
// unexpectedly.
//
// The case this exists for is a daemon upgrade: the exec takes the socket with it, so every attach
// drops even though nothing is wrong and the service never noticed. Without this, "upgrade the
// daemon without the processes noticing" is true for the processes and a lie for the person
// watching them, whose terminal just returns to a shell prompt with no explanation.
const ReattachWindow = 15 * time.Second

// AttachLoop connects the terminal to a service, and reconnects if the daemon goes away underneath
// it.
//
// It returns when the user detaches, when the service is gone, or when the daemon does not come
// back within ReattachWindow.
// AttachLoop attaches with the byte pipe, the default and the conservative choice.
//
// Kept as the name the rest of the program and its tests already call, so that adding a way to ask
// for the other mode did not mean touching every caller.
func AttachLoop(socket, service string, in *os.File, out io.Writer, replay bool) error {
	return AttachLoopMode(socket, service, in, out, replay, RenderAuto)
}

// AttachLoopMode attaches, choosing explicitly whether the client owns the screen.
func AttachLoopMode(socket, service string, in *os.File, out io.Writer, replay bool, mode RenderMode) error {
	return AttachLoopWith(socket, service, in, out, AttachOptions{Replay: replay, Mode: mode})
}

// AttachOptions is how an attach differs from the ordinary one. A struct rather than a fourth and
// fifth boolean parameter: the third one was already one too many to read at a call site.
type AttachOptions struct {
	// Replay asks for the output already on screen before the live stream.
	Replay bool
	// Mode is whether this client owns the screen.
	Mode RenderMode
	// ReadOnly watches without touching. The daemon enforces it; this stops the keystrokes
	// leaving in the first place and says so when one does.
	ReadOnly bool
}

// AttachLoopWith attaches with the options given.
func AttachLoopWith(socket, service string, in *os.File, out io.Writer, opts AttachOptions) error {
	replay, mode := opts.Replay, opts.Mode
	// Raw mode and the terminal reader belong to the loop, not to one session: keystrokes go to
	// the far end untouched (including Ctrl-C, which belongs to the program you are attached to
	// and not to us), and switching services must not hand the terminal back and forth.
	restore := func() {}
	if term.IsTerminal(int(in.Fd())) {
		state, err := term.MakeRaw(int(in.Fd()))
		if err != nil {
			return fmt.Errorf("putting the terminal in raw mode: %w", err)
		}
		restore = func() {
			// The modes the last service left on go with it: the prompt this terminal returns
			// to should not be sent mouse reports, or be left on the alternate screen.
			_, _ = out.Write(terminalModes.Reset())
			_ = term.Restore(int(in.Fd()), state)
		}
	}
	defer restore()

	// The configuration before the reader, because the reader needs the prefix key and runs for
	// the whole life of the attach.
	cfg := status.Load()
	input := startTerminalInput(in, cfg.Prefix)
	defer input.stop()

	// The first attach on this machine says how to get out of it. See firstrun.go.
	greet := FirstAttach()

	// Everything this loop has to tell the user goes through here. Standard error while the byte
	// pipe owns the terminal, the status line once something is painting over it - the same sink
	// the keyboard reader uses, for the same reason: three separate messages have already been
	// written to a screen that erased them before anyone could read them.
	say := sayToStderr

	// The status line, if it is switched on. It writes to the same terminal as the service's
	// output, so everything that draws goes through one lock from here on.
	screen := &lockedWriter{w: out}
	for _, p := range cfg.Problems {
		fmt.Fprintf(os.Stderr, "[gozellij: %s]\r\n", p)
	}
	// Two ways to put a screen on a terminal, and only one of them is on by default.
	//
	// The painter borrows: it writes a status line onto a terminal the service is also drawing
	// on, which needs the terminal's single cursor-save slot and a scrolling region. The rendered
	// screen owns: it interprets the service's output into a grid of its own and paints the
	// result, so the status line is a row the service was never given. renderclient.go says why
	// the borrowing one is still the default.
	var (
		painter  *statusPainter
		rendered *renderedScreen
	)
	if renderEnabled(mode) {
		cols, rows := 0, 0
		if term.IsTerminal(int(in.Fd())) {
			cols, rows, _ = term.GetSize(int(in.Fd()))
		}
		reserved := 0
		if cfg.Where == status.Bottom {
			reserved = 1
		}
		if cols > 0 && rows > 0 {
			rendered = newRenderedScreen(screen, cols, rows, reserved, statusLine(cfg, func() status.Context {
				return StatusContext(socket, showing.get(service))
			}))
			defer rendered.Close()
			// Anything the keyboard reader has to say now goes on the status line, where a paint
			// will not erase it a moment later.
			input.sayTo(rendered.Say)
			input.panes.Store(true)
			say = rendered.Say
			// Ask the terminal what colour it is, once, before any pane needs to know. The
			// answer arrives whenever it arrives; see AskColours.
			rendered.AskColours()
		}
	}
	if rendered == nil {
		painter = newStatusPainter(screen, in, cfg, func() status.Context {
			return StatusContext(socket, showing.get(service))
		})
		defer painter.Close()
		out = screen
		if painter != nil {
			// The byte pipe can talk now. Everything this loop and the keyboard reader have to
			// say goes to the status row instead of to standard error, where the service's next
			// repaint used to wipe it out.
			input.sayTo(painter.Say)
			say = painter.Say
		}
	}

	// A terminal that is closed, or a client that is told to stop, must still get its screen
	// back. Without this the scrolling region stays set after the client is gone: the shell that
	// comes next scrolls in the top rows only, with a frozen status line stuck along the bottom,
	// and nothing in sight explains why. SIGKILL cannot be helped; these can.
	//
	// The handler restores and then re-raises, so the exit status is still the signal's.
	fatal := make(chan os.Signal, 1)
	signal.Notify(fatal, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(fatal)
	go func() {
		sig, ok := <-fatal
		if !ok {
			return
		}
		painter.Close()
		if rendered != nil {
			_ = rendered.Close()
		}
		restore()
		signal.Stop(fatal)
		if s, isUnix := sig.(syscall.Signal); isUnix {
			_ = syscall.Kill(os.Getpid(), s)
		}
	}()

	if rendered != nil {
		// The status line has a clock in it. Nothing in the service's output makes it tick, so
		// something has to ask for a repaint; the painter had its own ticker for the same reason.
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			t := time.NewTicker(cfg.Every)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					_ = rendered.Repaint()
				case <-stop:
					return
				}
			}
		}()
	}

	// Said once the screen exists and whatever is drawing it is ready, so that it lands on the
	// status line rather than on a terminal that is about to be repainted over.
	if greet {
		say(firstRunGreeting(input.label))
	}

	first := true
	// shown is the service on screen and cameFrom the one before it, which is where an exit
	// goes back to: Ctrl-] c and then exit should put you where you pressed c.
	var shown, cameFrom string
	for {
		showing.set(service)
		if service != shown {
			if shown != "" {
				cameFrom = shown
			}
			shown = service
		}
		// Where this terminal is now, for `gozellij shell -last`. A watcher is not somewhere
		// anybody was working.
		if !opts.ReadOnly {
			RememberShown(service)
		}
		c, err := Dial(socket)
		if err == nil {
			c.SetReadOnly(opts.ReadOnly)
		}
		if err != nil {
			if first {
				return err
			}
			// Not the first connection, so the daemon existed a moment ago and may be being
			// replaced right now. Give it the same window the reattach path gives it, rather
			// than giving up on a refusal that lasts milliseconds - which is what a Ctrl-] n
			// pressed during an upgrade used to hit.
			back, werr := waitForDaemonClient(socket, ReattachWindow)
			if werr != nil {
				return fmt.Errorf("lost the daemon and could not get back: %w", err)
			}
			back.SetReadOnly(opts.ReadOnly)
			c = back
		}

		// Whether it was running when we arrived, which decides what its finishing means.
		//
		// A service that ends while you are watching it is you typing `exit`, and the attach has
		// to end so you get your prompt back. A service that was already stopped before you
		// switched to it is a tab you have landed on, and ending the attach there is how pressing
		// n used to drop somebody out of gozellij altogether.
		wasRunningOnArrival := serviceIsRunning(socket, service)

		var outcome attachOutcome
		if rendered != nil {
			// A session that owns the screen can show more than one service at a time, which a
			// byte pipe cannot: two services writing to one terminal would be two programs
			// drawing over each other. This is what the emulator was built for.
			outcome, err = renderedSession(socket, c, service, input, in, rendered, replay && first, &service)
		} else {
			outcome, err = c.runSession(service, input, in, out, replay && first, painter.Reserved())
		}
		c.Close()
		// Renamed while attached, so everything from here on - the detach message, k, u, the
		// reconnect after an upgrade - has to use the name it has now.
		if renamed := c.RenamedTo(); renamed != "" {
			service, shown = renamed, renamed
		}
		if err != nil {
			return err
		}

		// A label, because one thing the user can do while the service list is up is give
		// another command - and that command has to be acted on here rather than swallowed.
	dispatch:
		for {
			switch outcome {
			case outcomeDetached:
				restore()
				// Straight to the terminal, not through the sink: the screen has just been
				// handed back and there is nothing left to paint over this.
				fmt.Fprintf(os.Stderr, "\r\n[detached from %s; it keeps running]\r\n", service)
				return nil

			case outcomeFinished:
				if wasRunningOnArrival {
					// It ended under you: `exit` in the shell you were working in. If another
					// service is running, go there, the way closing a tab in tmux lands you on the
					// next one - leaving gozellij because one of several shells exited threw away
					// the others' screen for a keystroke that meant "done with this one". Only when
					// nothing else is running is the terminal handed back, which is what exit in
					// your last shell is asking for.
					next := cameFrom
					if next == service || !serviceIsRunningStrict(socket, next) {
						next = runningNeighbour(socket, service)
					}
					// A shell you opened for yourself closes when you exit it, the way a tmux
					// window does - otherwise every Ctrl-] c leaves a dead shell-N in the list.
					// Only a clean exit of a login shell: a crash keeps its tab, with the reason
					// on screen and u to start it again, and a service you defined yourself is
					// never removed because it ended.
					ended := service + " exited"
					if closesOnExit(socket, service) {
						if rc, err := Dial(socket); err == nil {
							// Keep the log: closing the tab is not forgetting what it printed.
							if _, err := rc.Remove(service, true); err == nil {
								ended = service + " exited and was closed"
							}
							rc.Close()
						}
					}
					if next != "" {
						say(ended + " - now on " + next)
						service = next
						showService(out, service)
						painter.Repaint()
						first, replay = true, true
						break dispatch
					}
					restore()
					return nil
				}
				// Stay on it, the way a pane in a rendered split does.
				//
				// renderedsession.go says a pane whose service has ended stays on screen with its
				// last output on purpose, and says what can be done about it. The byte pipe used
				// to do the opposite: the attach ended, so pressing n onto a tab that happened to
				// be stopped dropped you back to your shell - and `u`, which this help offers,
				// could never be reached in the one case it exists for, because by the time you
				// would press it there was nothing left to press it in.
				//
				// So: say what happened, keep the keyboard, and wait. d still leaves, u starts it
				// again, n and p move on, k removes it. The last output stays on the screen, which
				// is usually where the reason is.
				say(service + " is not running: " + input.label + " u starts it, " +
					input.label + " n/p move on, " + input.label + " d detaches")
				next, alive := waitForCommandOnADeadService(input)
				if !alive {
					restore()
					return nil
				}
				outcome = next
				continue dispatch

			case outcomeRename:
				// The session's connection is closed while the name is typed, like the list, so
				// nothing typed here can reach the service by mistake.
				name, instead, msg := renameFromKey(socket, service, input, say)
				if instead != nil {
					outcome = *instead
					continue dispatch
				}
				service, shown = name, name
				say(msg)
				first, replay = true, true
				break dispatch

			case outcomeRevive:
				// A fresh connection is the revival - the one the session used is closed by now -
				// with the service started first if it needs to be, so typing reaches a process.
				msg, _ := revive(socket, service)
				say(msg)
				first, replay = true, true
				break dispatch

			case outcomeCreate:
				name, err := newShell(socket, service)
				if err != nil {
					say(err.Error())
					first, replay = true, true
					break dispatch
				}
				say("new shell: " + name)
				service = name
				showService(out, service)
				painter.Repaint()
				first, replay = true, true
				break dispatch

			case outcomeRemove:
				// Gone, so there is nothing to go back to. On to the next service if there is
				// one, which is what closing a tab does; out of the terminal with a line saying
				// so if that was the last.
				next, msg := removeAndMoveOn(socket, service)
				if next == "" {
					restore()
					fmt.Fprintf(os.Stderr, "\r\n[%s]\r\n", msg)
					return nil
				}
				say(msg)
				service = next
				showService(out, service)
				painter.Repaint()
				first, replay = true, true
				break dispatch

			case outcomeNext, outcomePrev:
				// Tabs, the cheap way. Switching which service this terminal is showing needs no
				// terminal emulator at all: the attach is a byte pipe, so replaying the new
				// service's output repaints the screen because the escape sequences that drew it
				// are in the bytes. A multiplexer that owns a grid has to render this; here the
				// terminal does, exactly as it does on a first attach.
				next, nerr := neighbourService(socket, service, outcome == outcomeNext)
				if nerr != nil {
					say(nerr.Error())
					// Staying put beats dropping the user at a shell prompt because a list
					// lookup failed.
					first, replay = false, false
					break dispatch
				}
				service = next
				showService(out, service)
				painter.Repaint()
				first, replay = true, true
				break dispatch

			case outcomeRedraw:
				// Ctrl-] r in the byte pipe: nothing here to repaint from, so start over - clear,
				// replay, and the resize nudge after it that makes a full-screen program draw
				// itself again. This used to fall through to a plain reconnect without a replay,
				// which changed nothing on screen: a key the help offered that did nothing.
				_, _ = out.Write(terminalModes.Reset())
				fmt.Fprint(out, "\x1b[H\x1b[2J")
				painter.Repaint()
				first, replay = true, true
				break dispatch

			case outcomeList:
				// Cycling with n/p is fine for two services and tedious for six. The list is
				// printed over whatever was on screen and the next keystroke chooses; the
				// connection is already closed, so that keystroke cannot reach a service by
				// mistake.
				// Nothing else may draw while the menu is up. The repaint that keeps the status
				// clock moving was covering it within two seconds, so the question was invisible
				// and the answer still worked - a menu nobody could see, answered by guesswork.
				if rendered != nil {
					rendered.Suspend()
				}
				picked, instead, perr := pickService(socket, service, input, out)
				if rendered != nil {
					rendered.Resume()
				}
				if perr != nil {
					say(perr.Error())
					first, replay = false, false
					break dispatch
				}
				if instead != nil {
					// A command arrived while the list was up. Act on it rather than
					// throwing it away: someone who types Ctrl-] d over a list they have
					// changed their mind about means to detach, and having to press it
					// twice is the program telling them it was not listening.
					outcome = *instead
					showService(out, service)
					continue dispatch
				}
				if picked == service {
					// Cancelled, or chose where they already were. Repaint so the list is
					// not left sitting on top of the service's screen - and the status line
					// too, because clearing the screen wipes the reserved row along with
					// everything else.
					showService(out, service)
					painter.Repaint()
					first, replay = true, true
					break dispatch
				}
				service = picked
				showService(out, service)
				painter.Repaint()
				first, replay = true, true
				break dispatch
			}
			break dispatch
		}
		if outcome != outcomeDisconnected {
			continue
		}

		// The stream ended without us asking. Either the daemon went away - an upgrade, most
		// likely - or the connection broke. Say so, then try to get back in.
		say("connection to the daemon ended; reattaching...")

		back, werr := waitForDaemonClient(socket, ReattachWindow)
		if werr != nil {
			return fmt.Errorf("the daemon did not come back: %w", werr)
		}
		back.Close()

		// Do not replay on the way back in. The terminal already shows the history, and
		// repainting it would duplicate what is on screen; but output produced while we were
		// away is genuinely missing, and a client that cannot tell is exactly what this
		// project keeps refusing to ship.
		say(fmt.Sprintf("reattached; anything printed while the daemon was restarting was not "+
			"captured here - `gozellij logs %s` has it", service))
		first = false
		replay = false
	}
}

// showing is the name of the service this terminal is showing, for the status line - which runs on
// its own and has to know about a rename the moment the stream reports one, not when the session
// next ends. Renamed from under you is common now that tabs name themselves (autoname.go): the bar
// lost the [brackets] on the current tab until the next switch.
var showing showingName

type showingName struct{ p atomic.Pointer[string] }

func (s *showingName) set(name string) { s.p.Store(&name) }

// get is the name shown, or fallback before anything has been.
func (s *showingName) get(fallback string) string {
	if n := s.p.Load(); n != nil {
		return *n
	}
	return fallback
}

// terminalModes is what the services shown in this terminal have switched it into, so that leaving
// one can switch it back. One per process, because a process has one terminal. See
// fabric.TermModes.
var terminalModes fabric.TermModes

// showService clears the terminal and says where you now are.
//
// The clear matters: what is on screen belongs to the service you just left, and replaying the new
// one on top of it would interleave two screens into something that looks like corruption.
func showService(out io.Writer, service string) {
	// Take off whatever modes the service being left switched on - mouse reporting, the
	// alternate screen - before anything of the next one is shown. The next one's replay starts
	// by putting back its own.
	_, _ = out.Write(terminalModes.Reset())
	// The status line is repainted straight after this, and has to name where you now are: set
	// only at the top of the loop, the bar showed the tab you had just left for up to a tick.
	showing.set(service)
	fmt.Fprint(out, "\x1b[H\x1b[2J")
	fmt.Fprintf(os.Stderr, "[gozellij: %s]\r\n", service)
}

// pickService shows the services and returns the one chosen, or the current one if the user
// changes their mind.
func pickService(socket, current string, input *terminalInput, out io.Writer) (string, *attachOutcome, error) {
	list, err := settledStatuses(socket, current)
	if err != nil {
		return "", nil, err
	}
	if len(list) == 0 {
		return "", nil, errors.New("there are no services")
	}
	names := make([]string, len(list))
	for i, s := range list {
		names[i] = s.Service
	}

	fmt.Fprint(out, "\x1b[H\x1b[2J")
	fmt.Fprint(os.Stderr, "[gozellij] pick a service:\r\n")
	now := time.Now()
	for i, s := range list {
		marker := "  "
		if s.Service == current {
			marker = "* "
		}
		fmt.Fprintf(os.Stderr, "  %s%s %s\r\n", marker, string(pickKey(i)), pickLine(s, now))
	}
	fmt.Fprint(os.Stderr, "  (any other key to stay where you are)\r\n")

	// The reader is still running, so the choice arrives as ordinary input - which is exactly
	// why there is one reader for the whole session rather than one per attach.
	select {
	case chunk := <-input.data:
		if len(chunk) == 0 {
			return current, nil, nil
		}
		i := pickIndex(chunk[0])
		if i < 0 || i >= len(names) {
			return current, nil, nil
		}
		return names[i], nil, nil
	case want := <-input.cmds:
		// Ctrl-] something, mid-list. Hand it back rather than swallowing it.
		return current, &want, nil
	case <-time.After(pickTimeout):
		return current, nil, nil
	}
}

// pickKey is the key that selects the nth service, and pickIndex is its inverse.
//
// Digits first, then letters. One keystroke per choice on purpose: reading a number would mean
// waiting for Enter, and with "10" typed at a nine-service list the 1 selects a service and the 0
// is then typed at the shell you have just landed in - a keystroke arriving somewhere nobody sent
// it. Thirty-five entries is more services than anyone is switching between by eye.
func pickKey(i int) byte {
	if i < 9 {
		return byte('1' + i)
	}
	if i < 9+26 {
		return byte('a' + i - 9)
	}
	return '.'
}

func pickIndex(b byte) int {
	switch {
	case b >= '1' && b <= '9':
		return int(b - '1')
	case b >= 'a' && b <= 'z':
		return int(b-'a') + 9
	case b >= 'A' && b <= 'Z':
		return int(b-'A') + 9
	default:
		return -1
	}
}

// serviceNames lists the services, sorted, on a fresh connection.
func serviceNames(socket string) ([]string, error) {
	list, err := serviceStatuses(socket)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, svc := range list {
		names = append(names, svc.Service)
	}
	return names, nil
}

// serviceStatuses lists the services with their state, sorted by name, on a fresh connection.
func serviceStatuses(socket string) ([]ipc.StatusReply, error) {
	c, err := Dial(socket)
	if err != nil {
		return nil, fmt.Errorf("cannot list services: %w", err)
	}
	defer c.Close()

	// Bounded, like the status line's query and for the same reason: this runs on the main loop
	// when you press Ctrl-] n or Ctrl-] l, so an unbounded call means a wedged daemon holds a
	// service switch for the whole thirty second call timeout. Measured at 30.27s before.
	list, err := c.ListWithin(statusQueryTimeout)
	if err != nil {
		return nil, fmt.Errorf("cannot list services: %w", err)
	}
	out := append([]ipc.StatusReply(nil), list.Services...)
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// settledStatuses is serviceStatuses once this terminal's own session has stopped being counted.
//
// The session was closed a moment ago, but the daemon uncounts a viewer only once it has noticed
// the connection end, and a list asked for straight away counted this terminal as "open in 1
// other terminal" about half the time. Only the service it was looking at can be affected, so
// while that one shows a viewer, ask again briefly: this terminal's count goes within
// milliseconds, and one that is still there after that is somebody else.
func settledStatuses(socket, current string) ([]ipc.StatusReply, error) {
	viewersOf := func(list []ipc.StatusReply) int {
		for _, s := range list {
			if s.Service == current {
				return s.Viewers
			}
		}
		return 0
	}
	list, err := serviceStatuses(socket)
	for tries := 0; err == nil && tries < 6 && viewersOf(list) > 0; tries++ {
		time.Sleep(50 * time.Millisecond)
		again, aerr := serviceStatuses(socket)
		if aerr != nil {
			break
		}
		if viewersOf(again) < viewersOf(list) {
			return again, nil
		}
		list = again
	}
	return list, err
}

// pickLine is one entry in the Ctrl-] l list: enough to choose by. A list of names alone could
// not tell the shell you were working in from one that exited an hour ago, or say that another
// terminal is sitting in it - which is worth knowing before you type into it.
func pickLine(s ipc.StatusReply, now time.Time) string {
	line := fmt.Sprintf("%-16s %s", s.Service, s.State)
	if s.State == "running" && !s.StartedAt.IsZero() {
		line += " " + shortAge(now.Sub(s.StartedAt))
	}
	// This terminal's own session is closed while the list is up, so any viewer is somebody else.
	if s.Viewers > 0 {
		line += fmt.Sprintf(" · open in %d other terminal", s.Viewers)
		if s.Viewers > 1 {
			line += "s"
		}
	}
	return line
}

// shortAge is a duration in the one unit that matters: 40s, 12m, 3h, 2d.
func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// serviceIsRunning says whether a service has a process right now, on a fresh connection.
//
// Anything that goes wrong answers "yes": the question only decides whether a service finishing
// should end the attach, and the old behaviour - end it - is the one that does not leave somebody
// stuck in a screen they did not ask for.
func serviceIsRunning(socket, name string) bool {
	list, err := serviceStatuses(socket)
	if err != nil {
		return true
	}
	for _, svc := range list {
		if svc.Service == name {
			return svc.Pid != 0
		}
	}
	return true
}

// waitForCommandOnADeadService holds the keyboard while nothing is attached, and returns the next
// thing gozellij was asked for.
//
// Typing is read and dropped on purpose: there is no process to send it to, and the reader would
// block on a channel nobody is draining if it were not read at all - which would take the prefix
// key down with it, leaving a screen that cannot even be detached from.
//
// alive is false when the terminal has gone, which is the one way out that is not a command.
func waitForCommandOnADeadService(input *terminalInput) (outcome attachOutcome, alive bool) {
	for {
		select {
		case want, ok := <-input.cmds:
			if !ok {
				return 0, false
			}
			return want, true
		case _, ok := <-input.data:
			if !ok {
				return 0, false
			}
		}
	}
}

// neighbourService is the service before or after this one, wrapping around.
//
// The list is fetched on a fresh connection because the attached one has been given over to the
// stream. Sorted by name, which is arbitrary but stable - and a switcher whose order changes
// between presses would be useless.
func neighbourService(socket, current string, forward bool) (string, error) {
	names, err := serviceNames(socket)
	if err != nil {
		return "", err
	}

	if len(names) == 0 {
		return "", errors.New("there are no services to switch to")
	}
	if len(names) == 1 {
		return "", fmt.Errorf("%s is the only service", names[0])
	}

	at := -1
	for i, n := range names {
		if n == current {
			at = i
			break
		}
	}
	if at < 0 {
		// The service we are attached to is no longer listed - removed while we watched. Going
		// to the first one beats refusing to move.
		return names[0], nil
	}
	step := 1
	if !forward {
		step = -1
	}
	return names[(at+step+len(names))%len(names)], nil
}

// serviceIsRunningStrict is serviceIsRunning without the benefit of the doubt: false when it cannot
// tell, because this chooses where to go and a guess could land on a service that is not there.
func serviceIsRunningStrict(socket, name string) bool {
	if name == "" {
		return false
	}
	list, err := serviceStatuses(socket)
	if err != nil {
		return false
	}
	for _, s := range list {
		if s.Service == name {
			return s.Pid != 0
		}
	}
	return false
}

// closesOnExit is whether a service that just ended under you is a tab you opened to type in,
// whose entry should close now that you are done with it.
//
// Marked when it was made (CloseOnExit: `gozellij shell`, Ctrl-] c, add -close-on-exit), so a
// rename does not change the answer. Shells made before the mark existed are recognised the old
// way: a name of shell or shell-N running your login shell.
//
// Only when it exited by itself: one killed by a signal - a crash, or `gozellij stop` - keeps its
// tab, with the reason on screen and u to start it again. Not the exit code: a shell's is its last
// command's, so Ctrl-D after a failed grep would keep the tab for no reason anybody could see.
func closesOnExit(socket, name string) bool {
	c, err := Dial(socket)
	if err != nil {
		return false
	}
	defer c.Close()
	st, err := c.Status(name)
	if err != nil || st.Pid != 0 || !st.HasExited || st.ExitUnknown || st.ExitSignal != "" {
		return false
	}
	if st.CloseOnExit {
		return true
	}
	if name != "shell" && !shellTabName.MatchString(name) {
		return false
	}
	login := os.Getenv("SHELL")
	if login == "" {
		login = "/bin/sh"
	}
	return st.Command == login
}

var shellTabName = regexp.MustCompile(`^shell-[0-9]+$`)

// runningNeighbour is the next running service after current, in the order n goes, or "" when no
// other service is running. Stopped ones are passed over: landing on one after an exit would be
// a second dead end straight after the first.
func runningNeighbour(socket, current string) string {
	list, err := serviceStatuses(socket)
	if err != nil {
		return ""
	}
	at := -1
	for i, s := range list {
		if s.Service == current {
			at = i
			break
		}
	}
	for step := 1; step <= len(list); step++ {
		s := list[(at+step+len(list))%len(list)]
		if s.Service != current && s.Pid != 0 {
			return s.Service
		}
	}
	return ""
}

// waitForDaemonClient polls until the daemon both accepts and answers.
func waitForDaemonClient(socket string, within time.Duration) (*Client, error) {
	deadline := time.Now().Add(within)
	var last error
	for time.Now().Before(deadline) {
		c, err := Dial(socket)
		if err == nil {
			if perr := c.Ping(); perr == nil {
				return c, nil
			} else {
				c.Close()
				last = perr
			}
		} else {
			last = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("gave up after %v (last error: %v)", within, last)
}

// Attach connects the local terminal to a service until the user detaches.
//
// in and out are normally os.Stdin and os.Stdout; they are parameters so this is testable without
// a controlling terminal.
func (c *Client) Attach(service string, in *os.File, out io.Writer, replay bool) error {
	input := startTerminalInput(in, status.Load().Prefix)
	defer input.stop()
	// No status line on this path, so no reserved row: Attach is the plain one-shot form, used
	// by tests and by anything embedding this that draws its own furniture.
	_, err := c.runSession(service, input, in, out, replay, 0)
	return err
}

// terminalInput reads the terminal once, for the whole time the user is here.
//
// One reader, not one per session, and that is the point. A read on a tty cannot be cancelled -
// SetReadDeadline is refused on it, measured - so a session that started its own reader left it
// blocked in read(2) when it ended, and the next session started a second one beside it. Two
// goroutines reading the same terminal means the next keystroke goes to whichever wakes first, and
// half the time that is the one attached to a connection that is already closed. With one switch
// per session that was a rare lost keystroke after a daemon upgrade; with a key that switches
// services it would be every other press.
type terminalInput struct {
	// panes is whether the session this reader feeds can have more than one of them. The
	// split, focus, close and scrollback keys mean nothing without it, and this is what lets the
	// reader say so instead of letting the key do nothing - which is what it did, under a comment
	// claiming otherwise.
	panes atomic.Bool
	// prefix is the key that addresses gozellij rather than the service, and label is how to
	// write it. Carried rather than looked up, because the reader runs for the whole life of an
	// attach and the configuration is read once at the start of it.
	prefix byte
	label  string

	// sayMu guards say, which is where messages to the user go and which changes once the attach
	// knows whether it is rendering. See sayTo.
	sayMu sync.Mutex
	say   func(string)

	// data carries bytes meant for whatever service is attached.
	data chan []byte
	// cmds carries the things the user asked gozellij itself for.
	cmds chan attachOutcome
	// ended fires when the terminal reaches EOF or fails.
	ended chan error

	done chan struct{}
	once sync.Once
}

// startTerminalInput begins reading the terminal.
// startTerminalInput reads the keyboard.
//
// What it has to tell the user goes through a sink rather than to standard error directly, because
// in a rendered attach standard error is covered by the next paint within milliseconds - which
// made `Ctrl-] ?`, the key whose entire job is to tell you what the keys are, print its answer
// onto a screen that erased it. The same went for the message saying that the key you just
// pressed does nothing. The sink starts as standard error and is redirected by sayTo once the
// attach knows whether it is drawing the screen itself.
func startTerminalInput(in *os.File, prefix byte) *terminalInput {
	t := &terminalInput{
		prefix: prefix,
		label:  status.PrefixLabel(prefix),
		say:    sayToStderr,
		// Buffered so a burst read is not held up by a session that is mid-switch.
		data:  make(chan []byte, 64),
		cmds:  make(chan attachOutcome, 1),
		ended: make(chan error, 1),
		done:  make(chan struct{}),
	}
	go t.run(in)
	return t
}

// sayTo redirects what this reader tells the user.
func (t *terminalInput) sayTo(f func(string)) {
	t.sayMu.Lock()
	defer t.sayMu.Unlock()
	t.say = f
}

// tell says something to the user, wherever that currently is.
func (t *terminalInput) tell(msg string) {
	t.sayMu.Lock()
	say := t.say
	t.sayMu.Unlock()
	say(msg)
}

// sayToStderr is the default sink: a line on standard error, which is right for a byte-pipe attach
// because nothing there is drawing over it.
func sayToStderr(msg string) {
	fmt.Fprintf(os.Stderr, "\r\n[gozellij: %s]\r\n", msg)
}

// newShell defines and starts a shell beside the one you are in, and returns its name.
//
// Where you are, in both senses. The same environment as the shell the key was pressed in -
// less GOZELLIJ, which names that shell and would name this one wrongly; the daemon sets the
// right one - and the same working directory, read from the running process rather than from
// wherever the attach happened to be started. tmux does this with pane_current_path, and a new
// shell that opens in your home directory when you were three directories deep is a small
// irritation repeated every time.
func newShell(socket, from string) (string, error) {
	c, err := Dial(socket)
	if err != nil {
		return "", fmt.Errorf("could not reach the daemon: %w", err)
	}
	defer c.Close()

	list, err := c.List()
	if err != nil {
		return "", fmt.Errorf("could not list services: %w", err)
	}
	taken := map[string]bool{}
	for _, s := range list.Services {
		taken[s.Service] = true
	}
	// new-N, a name that looks like the placeholder it is: the first program run in the tab
	// replaces it (AutoName, see autoname.go).
	name := ""
	for i := 1; i < 1000; i++ {
		if n := fmt.Sprintf("new-%d", i); !taken[n] {
			name = n
			break
		}
	}
	if name == "" {
		return "", errors.New("no free new-N name below new-1000")
	}

	dir, _ := os.UserHomeDir()
	if st, err := c.Status(from); err == nil && st.Pid > 0 {
		if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", st.Pid)); err == nil {
			dir = cwd
		}
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GOZELLIJ=") {
			env = append(env, kv)
		}
	}
	if _, err := c.Add(name, ipc.AddRequest{
		Command: shell, Args: []string{"-l"}, Dir: dir, Env: env, Start: true, CloseOnExit: true,
		// A placeholder until the first program run in it names it.
		AutoName: true,
	}); err != nil {
		return "", fmt.Errorf("could not start a new shell: %w", err)
	}
	return name, nil
}

// removeAndMoveOn removes a service and says which one to show instead, or "" when none is left.
//
// The next one in the same rotation Ctrl-] n walks, worked out *before* the removal, because
// afterwards the service being removed is not in the list to count from.
func removeAndMoveOn(socket, service string) (next, msg string) {
	next, _ = neighbourService(socket, service, true)
	if next == service {
		next = ""
	}
	c, err := Dial(socket)
	if err != nil {
		return service, "could not reach the daemon: " + err.Error()
	}
	defer c.Close()
	if _, err := c.Remove(service, false); err != nil {
		// Still there, so stay on it rather than pretending it went.
		return service, fmt.Sprintf("could not remove %s: %v", service, err)
	}
	if next == "" {
		return "", fmt.Sprintf("removed %s; nothing else is running", service)
	}
	return next, fmt.Sprintf("removed %s", service)
}

// revive gets a service ready to be reattached to, and says which of two things happened.
//
// The ordinary case is that nothing is wrong with the service at all: the pane froze because its
// connection went stale, and reconnecting is the whole cure. Starting the service is only for
// when it has actually stopped - and saying "started it again" about a process that had been
// running all along is the kind of message that makes the next real problem harder to read.
func revive(socket, service string) (string, bool) {
	c, err := Dial(socket)
	if err != nil {
		return "could not reach the daemon: " + err.Error(), false
	}
	defer c.Close()
	st, err := c.Status(service)
	if err != nil {
		return fmt.Sprintf("cannot revive %s: %v", service, err), false
	}
	if st.State == "running" {
		return fmt.Sprintf("reconnected to %s", service), true
	}
	if _, err := c.Start(service); err != nil {
		return fmt.Sprintf("%s was not running and would not start: %v", service, err), false
	}
	return fmt.Sprintf("%s was not running - started it again", service), true
}

// unctrl forgives a Ctrl still held down from the prefix.
//
// The natural way to type a prefix command is to press Ctrl-B, keep Ctrl down, and press the
// next key - which turns d into Ctrl-D and shift+/ into Ctrl-/, a byte that is neither. Found in
// use: `Ctrl-B ?` showed "'\x1f' does nothing", because 0x1f is what a held Ctrl makes of the key
// that types a question mark. tmux answers this with a C- binding for every command; this reads
// the key the person meant.
//
// Four bytes keep their own meaning: the prefix itself (pressed twice sends it through), Tab and
// Ctrl-L (already bound, to switching panes and to redraw), and Esc, which is a key of its own.
func (t *terminalInput) unctrl(b byte) byte {
	switch {
	case b == t.prefix, b == 0x09, b == 0x0c, b == 0x1b:
		return b
	case b >= 0x01 && b <= 0x1a:
		return b + 0x60 // Ctrl-D is d
	case b == 0x1f:
		return '?' // Ctrl-/, which is what a held Ctrl makes of shift+/
	}
	return b
}

// noPanes explains a pane key pressed where there are no panes.
//
// These keys are in the help and do something in a rendered attach, and in the byte pipe they fell
// through the loop's dispatch and did nothing at all - under a comment on the outcomes saying the
// reader would say so. It did not. Found in use: "x does nothing".
func (t *terminalInput) noPanes(b byte, what string) {
	t.tell(fmt.Sprintf("%s %c would %s, but this attach has only one: panes need the one that "+
		"draws the screen itself - gozellij attach -render <name>, or login-setup -render",
		t.label, b, what))
}

// stop abandons the reader. It does not interrupt the read in progress - nothing can - but it does
// mean nothing is ever delivered again, and the goroutine ends on the next keystroke or at exit.
func (t *terminalInput) stop() { t.once.Do(func() { close(t.done) }) }

// run is the two-state machine: everything is forwarded until PrefixKey, and the byte after that
// is a command for us.
//
// Bytes are forwarded in runs rather than one at a time, because a paste is a single read of
// several thousand of them and a frame per byte would be visible.
func (t *terminalInput) run(in *os.File) {
	buf := make([]byte, 4096)
	var pending []byte
	prefixed := false

	flush := func() bool {
		if len(pending) == 0 {
			return true
		}
		chunk := make([]byte, len(pending))
		copy(chunk, pending)
		pending = pending[:0]
		select {
		case t.data <- chunk:
			return true
		case <-t.done:
			return false
		}
	}
	command := func(o attachOutcome) bool {
		if !flush() {
			return false
		}
		select {
		case t.cmds <- o:
			return true
		case <-t.done:
			return false
		}
	}

	for {
		n, err := in.Read(buf)
		for i := 0; i < n; i++ {
			b := buf[i]

			if prefixed {
				prefixed = false
				b = t.unctrl(b)
				switch b {
				case t.prefix:
					// A literal, for the programs that want this key themselves.
					pending = append(pending, b)
				case 'd', 'D':
					if !command(outcomeDetached) {
						return
					}
				case 'n', 'N', ' ':
					if !command(outcomeNext) {
						return
					}
				case 'p', 'P':
					if !command(outcomePrev) {
						return
					}
				case 'l', 'L', 'w', 'W':
					if !command(outcomeList) {
						return
					}
				case '|', 's', 'S':
					if !t.panes.Load() {
						if !flush() {
							return
						}
						t.noPanes(b, "split the screen")
						continue
					}
					if !command(outcomeSplit) {
						return
					}
				case '-', '_':
					if !t.panes.Load() {
						if !flush() {
							return
						}
						t.noPanes(b, "split the screen")
						continue
					}
					if !command(outcomeSplitRows) {
						return
					}
				case 'o', 'O', '\t':
					if !t.panes.Load() {
						if !flush() {
							return
						}
						t.noPanes(b, "move between panes")
						continue
					}
					if !command(outcomeFocus) {
						return
					}
				case 'x', 'X':
					if !t.panes.Load() {
						if !flush() {
							return
						}
						t.noPanes(b, "close a pane")
						continue
					}
					if !command(outcomeClosePane) {
						return
					}
				case 'r', 'R', 0x0c:
					if !command(outcomeRedraw) {
						return
					}
				case '>', '+', '=':
					if !t.panes.Load() {
						if !flush() {
							return
						}
						t.noPanes(b, "resize a pane")
						continue
					}
					if !command(outcomeGrow) {
						return
					}
				case '<':
					if !t.panes.Load() {
						if !flush() {
							return
						}
						t.noPanes(b, "resize a pane")
						continue
					}
					if !command(outcomeShrink) {
						return
					}
				case 'b', 'B':
					if !t.panes.Load() {
						if !flush() {
							return
						}
						t.noPanes(b, "scroll back")
						continue
					}
					if !command(outcomeScrollBack) {
						return
					}
				case 'f', 'F':
					if !t.panes.Load() {
						if !flush() {
							return
						}
						t.noPanes(b, "scroll forward")
						continue
					}
					if !command(outcomeScrollForward) {
						return
					}
				case 'g', 'G':
					if !t.panes.Load() {
						if !flush() {
							return
						}
						t.noPanes(b, "return to the live screen")
						continue
					}
					if !command(outcomeScrollLive) {
						return
					}
				case 'c', 'C':
					// A new shell, where you are. The key tmux uses for a new window, and the
					// thing there was otherwise no way to do from inside: typing `gozellij shell
					// -name x` in a shell is nesting, which is refused.
					if !command(outcomeCreate) {
						return
					}
				case 'k', 'K':
					// Remove what you are looking at. Both modes, because getting rid of a thing
					// is not a split-screen feature - it is the thing a multiplexer is least
					// allowed to make you leave for.
					if !command(outcomeRemove) {
						return
					}
				case ',':
					// Rename: tmux's key for renaming a window. Both modes, like k.
					if !command(outcomeRename) {
						return
					}
				case 'u', 'U':
					// Revive: reconnect this pane, and start its service if it is not running.
					// For a pane that has frozen by accident with a perfectly good process behind
					// it, which is not something detaching and attaching again should be the
					// only cure for.
					if !command(outcomeRevive) {
						return
					}
				case '?', 'h':
					if !flush() {
						return
					}
					t.tell(prefixHelp(t.label))
				default:
					// Say what to do rather than swallowing it. A prefix key that silently
					// eats the next keystroke is indistinguishable from a dropped one.
					if !flush() {
						return
					}
					t.tell(fmt.Sprintf("%s %q does nothing. %s", t.label, b, prefixHelp(t.label)))
				}
				continue
			}

			if b == t.prefix {
				prefixed = true
				continue
			}
			pending = append(pending, b)
		}

		if !flush() {
			return
		}
		if err != nil {
			select {
			case t.ended <- err:
			case <-t.done:
			}
			return
		}
	}
}

// runSession runs one attach and reports why it ended, which is the difference between going home,
// moving to another service, and being cut off.
//
// Everything happens in one select rather than in goroutines writing to shared variables: the
// terminal's bytes, the user's commands, the service's output ending, and a window resize are four
// things that can happen next, and a loop that names all four is easier to be sure about than
// three goroutines and a mutex.
func (c *Client) runSession(service string, input *terminalInput, in *os.File, out io.Writer, replay bool, reserved int) (attachOutcome, error) {
	c.attachedAs = service
	cols, rows := 0, 0
	if term.IsTerminal(int(in.Fd())) {
		if w, h, err := term.GetSize(int(in.Fd())); err == nil {
			cols, rows = w, h-reserved
		}
	}

	if _, err := c.Call(ipc.OpAttach, service, ipc.AttachRequest{
		Cols: cols, Rows: rows, Replay: replay, ReadOnly: c.readOnly,
	}); err != nil {
		return outcomeDisconnected, err
	}

	// Forward window changes, so a full-screen program follows the terminal it is displayed in.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	type outputResult struct {
		finished bool
		err      error
	}
	output := make(chan outputResult, 1)
	go func() {
		f, e := c.pumpOutput(out)
		output <- outputResult{f, e}
	}()

	// Arriving somewhere, ask what is running there to draw itself again.
	//
	// The replay is the service's recent output, which is the right screen for a shell and the
	// wrong one for a full-screen program: Claude Code, vim or htop drew their screen long ago and
	// since then only changed parts of it, so the replay ends with half a screen of edits over a
	// cleared terminal. Such programs repaint completely when their window changes size, and the
	// only way to tell them from here is to change it - one row smaller and straight back, which
	// is two SIGWINCHs and a full repaint at the right size. Sent after the attach, so the daemon
	// writes the replay first and the repaint lands on top of it. Not for a watcher, which does
	// not get to resize what somebody else is using.
	if replay && !c.readOnly && cols > 0 && rows > 1 {
		_ = c.sendResize(cols, rows-1)
		_ = c.sendResize(cols, rows)
	}

	// Local copies, so that a terminal which reaches EOF can be dropped out of the select
	// without ending the session. Redirected input runs out; the service's output still
	// matters, and a viewer that quit the moment its stdin closed would be useless for
	// `gozellij attach web </dev/null` and for every test that does the same.
	data, cmds, ended := input.data, input.cmds, input.ended

	said := false
	for {
		select {
		case chunk := <-data:
			if c.readOnly {
				// Not sent, and said once. A keystroke that quietly goes nowhere is rule 1's
				// silent success; saying it on every key would mean a paste filling the line.
				if !said {
					said = true
					input.tell("read-only: your keystrokes go nowhere here. Ctrl-] d to leave")
				}
				continue
			}
			if err := c.Writer().WriteFrame(ipc.KindData, chunk); err != nil {
				// The connection is gone; let the output pump report why.
				c.Close()
				got := <-output
				return outcomeDisconnected, got.err
			}

		case want := <-cmds:
			// Whatever was typed before the command goes first. The reader flushes those bytes
			// and then sends the command, on two channels - and a select over two ready
			// channels picks at random, so without this the last thing typed at one service
			// could arrive at the next one instead.
			for draining := true; draining; {
				select {
				case chunk := <-data:
					_ = c.Writer().WriteFrame(ipc.KindData, chunk)
				default:
					draining = false
				}
			}
			// Closing is what ends the output pump: it is blocked on a read from the daemon,
			// which has no reason to say anything just because the user pressed a key.
			c.Close()
			<-output
			return want, nil

		case <-ended:
			// The terminal is gone, so nothing more will be typed - but plenty may still be
			// printed. Stop listening to it and carry on watching.
			data, cmds, ended = nil, nil, nil

		case got := <-output:
			if got.finished {
				return outcomeFinished, nil
			}
			return outcomeDisconnected, got.err

		case <-winch:
			w, h, err := term.GetSize(int(in.Fd()))
			if err != nil || w <= 0 || h-reserved <= 0 {
				continue
			}
			// Minus the reserved row here too, or a resize hands the service back the row the
			// status line is standing on.
			_ = c.sendResize(w, h-reserved)
		}
	}
}

// pumpOutput writes the service's output to the terminal and surfaces events.
//
// It reports whether the daemon said the service had finished, which is the difference between an
// ending and a disconnection - and therefore between letting go and trying to reattach.
func (c *Client) pumpOutput(out io.Writer) (bool, error) {
	finished := false
	for {
		kind, payload, err := c.Reader().ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return finished, nil
			}
			return finished, err
		}
		switch kind {
		case ipc.KindData:
			if _, werr := out.Write(payload); werr != nil {
				return finished, werr
			}
			terminalModes.Feed(payload)
		case ipc.KindEvent:
			// Events are shown, not swallowed. "lagged" in particular means the screen is
			// now wrong, and the user needs to know that rather than wonder later.
			var ev ipc.Event
			if jerr := jsonUnmarshal(payload, &ev); jerr == nil {
				if ev.Kind == ipc.EventFinished {
					finished = true
				}
				if ev.Kind == ipc.EventRenamed && ev.Service != "" {
					// Remembered, not printed: a line written into a byte pipe lands on top of
					// the service's screen, and the status line shows the new name anyway.
					name := ev.Service
					if prev := c.RenamedTo(); prev != "" {
						followRename(prev, name)
					} else {
						followRename(c.attachedAs, name)
					}
					c.renamedTo.Store(&name)
					showing.set(name)
					continue
				}
				if ev.Message != "" {
					fmt.Fprintf(os.Stderr, "\r\n[gozellij: %s]\r\n", ev.Message)
				}
			}
		case ipc.KindResponse:
			// An in-stream answer, to a resize. Successes are not worth a line; a refusal is,
			// because a resize the daemon rejected leaves a full-screen program drawing at the
			// wrong size and nothing else would ever mention it.
			var r ipc.Response
			if jerr := jsonUnmarshal(payload, &r); jerr == nil && !r.OK {
				fmt.Fprintf(os.Stderr, "\r\n[gozellij: %s]\r\n", r.Error)
			}
		default:
			fmt.Fprintf(os.Stderr, "\r\n[gozellij: unexpected %s frame]\r\n", kind)
		}
	}
}

func (c *Client) sendResize(cols, rows int) error {
	req := ipc.Request{Op: ipc.OpResize, Payload: mustJSON(ipc.ResizeRequest{Cols: cols, Rows: rows})}
	return c.Writer().WriteJSON(ipc.KindRequest, req)
}

// jsonUnmarshal and mustJSON keep the pumps readable; encoding failures on these tiny structs
// would be a programming error, not something a user can cause.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Unreachable for the fixed-shape structs used here; panicking beats sending a frame
		// that says nothing.
		panic(fmt.Sprintf("gozellij: encoding %T: %v", v, err))
	}
	return b
}
