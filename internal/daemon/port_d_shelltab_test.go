package daemon

import (
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Ported from acceptance.sh "a shell tab you exit closes, and keeps its log".
//
// A real shell tab, made with Ctrl-] c: what decides closing is the flag that sets, and the shell
// has to exit by itself.

// dNewTab waits for a new-N service to appear and returns its name.
func dNewTab(t *testing.T, sock string, not map[string]bool) string {
	t.Helper()
	var name string
	eventually(t, "a new-N shell", func() bool {
		list, err := dial(t, sock).List()
		if err != nil {
			return false
		}
		for _, s := range list.Services {
			if strings.HasPrefix(s.Service, "new-") && !not[s.Service] && s.Pid != 0 {
				name = s.Service
				return true
			}
		}
		return false
	})
	return name
}

// dShellRan waits for `echo marker` to have run in a shell and for the prompt after it: a line of
// output that is the marker alone, and a prompt below it. A Ctrl-D typed before the shell has put
// its terminal in the mode it reads in is lost when it does - the line discipline drops the pending
// end-of-file on the switch - so the command line echoing back is not enough.
func dShellRan(s *testScreen, marker string) {
	s.t.Helper()
	s.Until("the shell to run the echo and prompt again", func() bool {
		rows := s.Rows()
		for i, r := range rows {
			if strings.Contains(r, marker) && !strings.Contains(r, "echo") {
				for _, later := range rows[i+1:] {
					if strings.Contains(later, "$") {
						return true
					}
				}
			}
		}
		return false
	})
}

func TestPortDAShellTabYouExitClosesAndKeepsItsLog(t *testing.T) {
	screenEnv(t)
	_, _, sock, _ := newLoggedTestDaemon(t)
	dAdd(t, sock, "anchorsvc", "no", `printf "ANCHOR-UP\r\n"; exec cat`)

	s := attachScreen(t, sock, "anchorsvc", 60, 10, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("ANCHOR-UP")

	s.Prefix('c')
	made := dNewTab(t, sock, nil)
	s.BottomShows("[" + made + "]")
	// Something in its log, so there is something to keep.
	s.Type("echo TAB-SAID-THIS\r")
	dShellRan(s, "TAB-SAID-THIS")
	s.Type("\x04") // Ctrl-D

	// Gone from ls once you exit it, and you are back on the service you pressed c in.
	s.Until(made+" to be gone from the list", func() bool { return !dExists(t, sock, made) })
	s.BottomShows("now on anchorsvc")
	if !s.StillAttached() {
		t.Fatal("exiting the tab ended the attach")
	}

	// And its log is still readable, with a word about the name being gone.
	logs, err := dial(t, sock).Logs(made, 0)
	if err != nil {
		t.Fatalf("logs for the closed tab: %v", err)
	}
	if !strings.Contains(logs.Note, "no service is called") {
		t.Errorf("the note for the closed tab's log is %q", logs.Note)
	}
	if !strings.Contains(string(logs.Data), "TAB-SAID-THIS") {
		t.Errorf("the closed tab's log lost what it said: %q", logs.Data)
	}
}

func TestPortDARenamedShellTabStillClosesSoItIsTheFlagNotTheName(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	dAdd(t, sock, "anchorsvc", "no", `printf "ANCHOR-UP\r\n"; exec cat`)

	s := attachScreen(t, sock, "anchorsvc", 80, 10, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("ANCHOR-UP")
	s.Prefix('c')
	made := dNewTab(t, sock, nil)
	s.BottomShows("[" + made + "]")
	// The shell reading its terminal before anything is asked of it; see the test above.
	s.Type("echo TAB-IS-READY\r")
	dShellRan(s, "TAB-IS-READY")

	s.Prefix(',')
	s.BottomShows("rename " + made + " to: " + made + "_")
	s.Type("\x15not-called-shell\r")
	eventually(t, "the rename to take", func() bool { return dExists(t, sock, "not-called-shell") })
	s.BottomShows("renamed " + made + " to not-called-shell")

	s.Type("\x04")
	eventually(t, "the renamed tab to close", func() bool { return !dExists(t, sock, "not-called-shell") })
	s.BottomShows("now on anchorsvc")
}

// And the one that must not close: a service without the flag that ends by itself while you are
// looking at it stays defined. The script stops it with nobody attached; here it exits cleanly
// under an attach, which is the case the attach's closing code actually decides.
func TestPortDAServiceAddedWithoutTheFlagStaysDefinedAfterItEnds(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	dAdd(t, sock, "anchorsvc", "no", `printf "ANCHOR-UP\r\n"; exec cat`)
	if _, err := dial(t, sock).Add("plainshell", ipc.AddRequest{
		Command: "sh", Args: []string{"-c", `printf "PLAIN-UP\r\n"; read x; exit 0`}, Restart: "no", Start: true,
	}); err != nil {
		t.Fatal(err)
	}

	s := attachScreen(t, sock, "plainshell", 60, 10, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("PLAIN-UP")
	s.Type("\r")
	s.BottomShows("now on anchorsvc")
	if !dExists(t, sock, "plainshell") {
		t.Fatal("a service without the flag was removed when it ended")
	}
}
