package daemon

import (
	"strings"
	"sync"
	"testing"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Ported from acceptance.sh "two terminals on one service, both of them live" and "two terminals
// starting a shell at the same moment".
//
// Two attach screens in one process would share the client's process-wide state, so the second
// terminal here is a raw attach on its own connection, read continuously: what the daemon sends it
// is exactly what a second `gozellij attach` byte pipe would write to its terminal.

func TestPortCTwoTerminalsOnOneServiceAreBothLive(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	// No prompt, and no echo, so that only what the shell *computes* can match: the command
	// typed says $((6*7)), the answer says 42.
	addRunning(t, sock, "pair", "sh", "-c", `stty -echo; PS1=""; export PS1; exec /bin/sh -i`)

	s := attachScreen(t, sock, "pair", 80, 20, AttachOptions{Replay: true, Mode: RenderOff})
	s.BottomShows("[pair]")
	other := cAttachStream(t, sock, "pair", ipc.AttachRequest{Cols: 80, Rows: 20})

	eventually(t, "two viewers of pair", func() bool { return viewersOf(t, sock, "pair") == 2 })

	// Typed into the first terminal; the answer has to reach both.
	s.Type("echo BOTH-$((6*7))-SEE\r")
	s.Shows("BOTH-42-SEE")
	other.Shows("BOTH-42-SEE")

	// And the other direction: typing in the second one also arrives, and the first sees it.
	other.Type("echo OTHER-$((6*8))-WAY\r")
	s.Shows("OTHER-48-WAY")
}

func TestPortCThreeTerminalsStartingOneShellAtOnceLeaveOneService(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)

	// What `gozellij shell -name raced` does, three times at once: Ensure, then attach. The CLI
	// process around it is not in the race; these two calls are.
	req := ipc.AddRequest{Command: "/bin/sh", Args: []string{"-c", "exec cat"}, Restart: "no", CloseOnExit: true, AutoName: true}
	var wg sync.WaitGroup
	errs := make(chan string, 6)
	start := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := Dial(sock)
			if err != nil {
				errs <- "dial: " + err.Error()
				return
			}
			defer c.Close()
			<-start
			if _, err := c.Ensure("raced", req); err != nil {
				errs <- "ensure: " + err.Error()
				return
			}
			a, err := Dial(sock)
			if err != nil {
				errs <- "dial: " + err.Error()
				return
			}
			defer a.Close()
			if _, err := a.Call(ipc.OpAttach, "raced", ipc.AttachRequest{Cols: 80, Rows: 20}); err != nil {
				errs <- "attach: " + err.Error()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		if strings.Contains(e, "no such service") {
			t.Errorf("one of them lost the race: %s", e)
		} else {
			t.Errorf("a racer failed: %s", e)
		}
	}

	list, err := dial(t, sock).List()
	if err != nil {
		t.Fatal(err)
	}
	made := 0
	for _, s := range list.Services {
		if s.Service == "raced" {
			made++
		}
	}
	if made != 1 {
		t.Errorf("%d services called raced exist after three simultaneous starts", made)
	}
}
