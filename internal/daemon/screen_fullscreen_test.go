package daemon

import (
	"fmt"
	"strings"
	"testing"
)

// A full-screen program the way Claude Code is one: modes switched on once, a screen drawn once,
// then only small edits - far more of them than the replay ring holds. It never redraws, so only
// the picture the daemon keeps can show its screen.
const stubbornTUI = `printf '\033[?1049h\033[?1000h\033[?1006h\033[H\033[2J'
r=1; while [ $r -le 8 ]; do printf '\033[%d;1HFULL-ROW-%d' $r $r; r=$((r+1)); done
i=0; while :; do i=$((i+1)); printf '\033[1;30Htick %06d' $i; [ $((i % 500)) -eq 0 ] && sleep 0.05; done`

func TestScreenAFullScreenProgramComesBackWithItsScreenAndModes(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "plain", "sh", "-c", `printf 'PLAIN\r\n'; exec cat`)
	addRunning(t, sock, "tui", "sh", "-c", stubbornTUI)

	s := attachScreen(t, sock, "plain", 80, 12, AttachOptions{Replay: true, Mode: RenderOff})
	s.Shows("PLAIN")
	if m := s.Modes(); m[1049] || m[1000] {
		t.Fatalf("modes on before any full-screen program: %v", m)
	}

	s.Prefix('n')
	s.Until("all eight rows of the program's screen", func() bool {
		return strings.Count(s.Text(), "FULL-ROW-") == 8
	})
	if m := s.Modes(); !m[1049] || !m[1000] || !m[1006] {
		t.Errorf("arriving at the program did not put back its modes: %v", m)
	}

	// Leaving takes them off again, or a shell gets mouse reports typed into it.
	s.Prefix('n')
	s.Shows("PLAIN")
	if m := s.Modes(); m[1049] || m[1000] || m[1006] {
		t.Errorf("modes left on after switching to a shell: %v", m)
	}

	// And back, with the screen still there although the ring has long lost it.
	s.Prefix('n')
	s.Until("the screen again", func() bool { return strings.Count(s.Text(), "FULL-ROW-") == 8 })
	_ = fmt.Sprint
}
