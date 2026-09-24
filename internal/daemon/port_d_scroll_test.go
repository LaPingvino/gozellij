package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Ported from acceptance.sh "looking back through a rendered pane" and "output reaches the screen
// without waiting for the clock".

func TestPortDScrollBackThroughARenderedPane(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	dAdd(t, sock, "scroller", "", `i=1; while [ $i -le 60 ]; do printf "row-%02d\r\n" $i; i=$((i+1)); done; exec cat`)

	s := attachScreen(t, sock, "scroller", 40, 10, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("row-60")
	row8 := func() string { return s.Rows()[7] }
	live := row8()
	if !strings.Contains(live, "row-") {
		t.Fatalf("row 8 is not a line of output to begin with: %q", live)
	}

	// Ctrl-] b gives back the lines the rendered screen took from the terminal's own scrollback.
	s.Prefix('b')
	s.Until("row 8 to show an earlier line", func() bool {
		r := row8()
		return r != live && strings.Contains(r, "row-")
	})

	// Ctrl-] g returns to the live screen.
	s.Prefix('g')
	s.Until("row 8 to be live again", func() bool { return row8() == live })
}

func TestPortDOutputIsDrawnWithoutWaitingForTheStatusClock(t *testing.T) {
	home := screenEnv(t)
	// The status line's clock repaints everything; turned down to thirty seconds, the only thing
	// that can put the marker on screen is the output path itself being prompt.
	cfg := filepath.Join(home, "slowstatus")
	if err := os.WriteFile(cfg, []byte("every=30s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOZELLIJ_STATUS_CONFIG", cfg)
	latch := filepath.Join(home, "latch")

	_, _, sock := newTestDaemon(t)
	dAdd(t, sock, "latchy", "", `printf 'WAITING\r\n'; while [ ! -e `+latch+` ]; do sleep 0.01; done; printf 'ZZLATEZZ\r\n'; exec cat`)

	s := attachScreen(t, sock, "latchy", 40, 6, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("WAITING")
	if strings.Contains(s.Text(), "ZZLATEZZ") {
		t.Fatal("the marker was on screen before it was asked for, so this proves nothing")
	}
	if err := os.WriteFile(latch, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Far inside the clock's thirty seconds, and ten times the repaint interval.
	dWithin(s, 500*time.Millisecond, "the marker, drawn without the clock", func() bool {
		return strings.Contains(s.Text(), "ZZLATEZZ")
	})
}
