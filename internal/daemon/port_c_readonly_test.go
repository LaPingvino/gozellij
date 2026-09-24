package daemon

import (
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/ipc"
)

// Ported from acceptance.sh "watching without being able to touch".

// watchedService traps Ctrl-C loudly and echoes each line it reads. The echoes are the oracle for
// the negative, with one catch measured while writing this: sh runs a trap only after the command
// it interrupted has finished, so a leaked Ctrl-C prints *after* the echo of the next line. Two
// lines typed afterwards, then: by the second echo, any trap from before the first has run.
const watchedService = `trap 'printf "GOT-THE-INTERRUPT\r\n"' INT; printf 'WATCHED-IS-UP\r\n'; ` +
	`while :; do if read x; then printf 'ECHO-%s\r\n' "$x"; fi; done`

func TestPortCReadOnlyAttachWatchesButCannotTouch(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "watched", "sh", "-c", `stty -echo; `+watchedService)

	s := attachScreen(t, sock, "watched", 50, 8, AttachOptions{Replay: true, Mode: RenderOff, ReadOnly: true})
	s.Shows("WATCHED-IS-UP") // a read-only attach still shows the service's output

	writer := cAttachStream(t, sock, "watched", ipc.AttachRequest{Cols: 50, Rows: 8})

	s.Type("\x03")
	// A swallowed keystroke says so rather than going quiet.
	s.Shows("read-only")

	writer.Type("after\r")
	writer.Shows("ECHO-after")
	writer.Type("again\r")
	writer.Shows("ECHO-again")
	s.Shows("ECHO-again") // and the watcher sees what someone else typed
	if strings.Contains(writer.String(), "GOT-THE-INTERRUPT") {
		t.Fatalf("Ctrl-C reached the service through a read-only attach:\n%q", writer.String())
	}

	// Ctrl-] d still detaches a read-only attach.
	s.Prefix('d')
	if err := s.Ended(); err != nil {
		t.Fatalf("detaching a read-only attach ended with %v", err)
	}
}

// The daemon drops the keystrokes, not only the client: a read-only connection that sends them
// anyway reaches nothing. This is the half the acceptance comment says the check must survive the
// client's own drop being removed.
func TestPortCReadOnlyIsEnforcedByTheDaemon(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "watched", "sh", "-c", `stty -echo; `+watchedService)

	writer := cAttachStream(t, sock, "watched", ipc.AttachRequest{Cols: 50, Rows: 8, Replay: true})
	writer.Shows("WATCHED-IS-UP")
	watcher := cAttachStream(t, sock, "watched", ipc.AttachRequest{Cols: 50, Rows: 8, ReadOnly: true})

	watcher.Type("\x03sneaked\r")
	writer.Type("after\r")
	writer.Shows("ECHO-after")
	writer.Type("again\r")
	writer.Shows("ECHO-again")
	if got := writer.String(); strings.Contains(got, "GOT-THE-INTERRUPT") || strings.Contains(got, "sneaked") {
		t.Fatalf("a read-only connection's keystrokes reached the service:\n%q", got)
	}
}

// Which of the terminals attached can type: the list and the status count them apart.
func TestPortCViewersAreCountedApartFromWatchers(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "watched", "sh", "-c", `stty -echo; `+watchedService)

	s := attachScreen(t, sock, "watched", 50, 8, AttachOptions{Replay: true, Mode: RenderOff, ReadOnly: true})
	s.Shows("WATCHED-IS-UP")
	_ = cAttachStream(t, sock, "watched", ipc.AttachRequest{Cols: 50, Rows: 8})

	eventually(t, "two viewers, one of them read-only", func() bool {
		list, err := dial(t, sock).List()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list.Services {
			if e.Service == "watched" {
				return e.Viewers == 2 && e.Watchers == 1
			}
		}
		return false
	})
	st, err := dial(t, sock).Status("watched")
	if err != nil {
		t.Fatal(err)
	}
	if st.Viewers != 2 || st.Watchers != 1 {
		t.Errorf("status says %d viewers, %d read-only; want 2 and 1", st.Viewers, st.Watchers)
	}
}
