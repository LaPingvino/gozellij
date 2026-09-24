package daemon

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Ported from acceptance.sh "a full-screen program in one pane of a split".
//
// The script uses vim. What the promise needs of it is a program that addresses the cursor
// absolutely - row 1 column 1, meaning it - and redraws when told its size changed; this one does
// exactly that and nothing else, so what is on screen is not also a test of vim's colour probes.
//
// Not ported: "and the status line comes back after what gozellij had to say". It waits out
// messageLinger, a six-second constant, which is the thing being timed.

const dFullScreenEditor = `draw() { printf '\033[H\033[2J\033[1;1HALPHAWORD\033[2;1Hsecond line'; }
trap draw WINCH
draw
while :; do sleep 0.05; done`

// dLastWidth is the last width the quiet service reported on screen, or -1.
func dLastWidth(text string) int {
	m := regexp.MustCompile(`QUIETMARK-([0-9]+)`).FindAllStringSubmatch(text, -1)
	if len(m) == 0 {
		return -1
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

func TestPortDAFullScreenProgramInOnePaneOfASplit(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	dAdd(t, sock, "quiet", "", `while :; do printf "QUIETMARK-%s\r\n" "$(stty size | cut -d' ' -f2)"; sleep 0.05; done`)
	dAdd(t, sock, "edity", "", dFullScreenEditor)

	s := attachScreen(t, sock, "quiet", 80, 14, AttachOptions{Replay: true, Mode: RenderOn})
	s.Until("the service to report the full width", func() bool { return dLastWidth(s.Text()) > 70 })
	wide := dLastWidth(s.Text())

	s.Prefix('|')
	s.Shows("ALPHAWORD")

	// Splitting the screen tells the service its pane is narrower.
	s.Until("the service to report a narrower pane", func() bool {
		n := dLastWidth(s.Text())
		return n > 0 && n < wide
	})

	// Both on one screen, each on its own side of the seam: the editor addresses column 1 and
	// means its own pane's column 1, not the screen's.
	rows := s.Rows()
	qr, qc := dRowWith(rows, "QUIETMARK")
	ar, ac := dRowWith(rows, "ALPHAWORD")
	if qr < 0 || ar < 0 {
		t.Fatalf("one of them is missing:\n%s", s.Text())
	}
	if qc >= 40 || ac < 40 {
		t.Errorf("the panes overlap: QUIETMARK at column %d, ALPHAWORD at column %d of 80:\n%s", qc+1, ac+1, s.Text())
	}
	for r, line := range rows {
		if strings.Contains(line[:min(40, len(line))], "ALPHAWORD") {
			t.Errorf("the editor wrote into the other pane on row %d: %q", r+1, line)
		}
	}
}
