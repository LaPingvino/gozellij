package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Ported from acceptance.sh "you come back to the arrangement you left".
//
// The script kills the client with SIGKILL so that nothing on the way out can run, and then looks
// for the layout on disk. Here the layout is read while the attach is still running, which asks the
// same question - was it written before the way out? - without needing a signal.

// dLayoutServices is the services named in the layout file on disk for service, read as the file
// rather than through loadLayout, so nothing the loader decides can stand between this and what was
// written.
func dLayoutServices(t *testing.T, service string) []string {
	t.Helper()
	path, err := layoutPath(service)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var l savedLayout
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatalf("the layout on disk is not JSON: %v\n%s", err, b)
	}
	var names []string
	for _, p := range l.Panes {
		names = append(names, p.Service)
	}
	return names
}

func TestPortDYouComeBackToTheArrangementYouLeft(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	dAdd(t, sock, "laya", "", `i=0; while :; do printf "LAYA-%d\r\n" $i; i=$((i+1)); sleep 0.05; done`)
	dAdd(t, sock, "layb", "", `i=0; while :; do printf "LAYB-%d\r\n" $i; i=$((i+1)); sleep 0.05; done`)

	s := attachScreen(t, sock, "laya", 60, 8, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("LAYA-")
	if got := dLayoutServices(t, "laya"); strings.Contains(strings.Join(got, " "), "layb") {
		t.Fatalf("layb is in the layout before any split: %v", got)
	}
	s.Prefix('|')
	s.Shows("LAYB-")

	// Written down while the attach is running, not on the way out.
	eventually(t, "a layout naming both panes, while still attached", func() bool {
		return len(dLayoutServices(t, "laya")) == 2
	})
	if !s.StillAttached() {
		t.Fatal("the attach ended before the layout was checked")
	}

	s.Prefix('d')
	if err := s.Ended(); err != nil {
		t.Fatalf("detaching: %v", err)
	}

	// Attaching again brings the second pane back without asking.
	s2 := attachScreen(t, sock, "laya", 60, 8, AttachOptions{Replay: true, Mode: RenderOn})
	s2.Shows("LAYA-")
	s2.Shows("LAYB-")

	// And the pane that came back is connected, not a picture of one.
	before := dMaxNum(s2.Text(), "LAYB-")
	s2.Until("the restored pane to move", func() bool { return dMaxNum(s2.Text(), "LAYB-") > before })
}
