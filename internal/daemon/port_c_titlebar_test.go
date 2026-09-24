package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ported from acceptance.sh "the status line in the title bar" and "a new shell, from inside, where
// you are".

func cStatusConfig(t *testing.T, home, body string) {
	t.Helper()
	path := filepath.Join(home, "statuscfg")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOZELLIJ_STATUS_CONFIG", path)
}

func TestPortCStatusLineInTheTitleBarReservesNoRow(t *testing.T) {
	home := screenEnv(t)
	cStatusConfig(t, home, "where=title\n")
	_, _, sock := newTestDaemon(t)
	// Says the size it was given each time it is asked, which is the reserved row measured from
	// the service's side: whatever the status line takes, the service does not get.
	addRunning(t, sock, "titled", "sh", "-c", `printf 'TITLED-UP\r\n'; while read x; do printf 'SIZE=%s\r\n' "$(stty size)"; done`)

	s := attachScreen(t, sock, "titled", 60, 12, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("TITLED-UP") // the service's output is still shown
	s.Until("the service's name in the terminal's title", func() bool { return strings.Contains(cTitle(s), "titled") })

	s.Type("\r")
	s.Shows("SIZE=")
	if !strings.Contains(s.Text(), "SIZE=12 60") {
		t.Errorf("the service was not given the whole screen; it says %q", cSizeLine(s))
	}

	// And the bottom row is the service's: with enough lines to scroll, the last answer sits on
	// the row above the bottom and the bottom is the empty line the cursor went to. With a status
	// row reserved, the bottom would be the status line and the last answer two rows up.
	for i := 0; i < 14; i++ {
		s.Type("\r")
	}
	s.Until("the service's lines down to the bottom row", func() bool {
		r := s.Rows()
		return strings.HasPrefix(r[len(r)-2], "SIZE=") && r[len(r)-1] == "" && strings.HasPrefix(r[0], "SIZE=")
	})
}

func cSizeLine(s *testScreen) string {
	for _, r := range s.Rows() {
		if strings.Contains(r, "SIZE=") {
			return r
		}
	}
	return ""
}

func TestPortCNewShellFromInsideOpensWhereYouAre(t *testing.T) {
	home := screenEnv(t)
	deep := filepath.Join(home, "deep", "er")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "shell", "sh", "-c", `PS1="sh$ "; export PS1; exec /bin/sh -i`)

	s := attachScreen(t, sock, "shell", 200, 10, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("sh$")
	s.Type("cd " + deep + "; echo CD-$((1+1))\r")
	s.Shows("CD-2")

	s.Prefix('c')
	eventually(t, "a service called new-1", func() bool {
		list, err := dial(t, sock).List()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range list.Services {
			if e.Service == "new-1" {
				return true
			}
		}
		return false
	})
	s.BottomShows("[new-1]")

	s.Type(`echo "IN=[$GOZELLIJ] AT=[$(pwd)]"` + "\r")
	s.Shows("IN=[new-1] AT=[" + deep + "]")
}
