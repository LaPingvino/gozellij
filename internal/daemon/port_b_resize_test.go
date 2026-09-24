package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Ported from acceptance.sh, "a rendered attach survives the window changing size" and "and
// resizing while you are reading the scrollback".

// bLastNumber is the number after the last occurrence of prefix on the screen, or -1.
func bLastNumber(s *testScreen, prefix string) int {
	re := regexp.MustCompile(regexp.QuoteMeta(prefix) + `(\d+)`)
	m := re.FindAllStringSubmatch(s.Text(), -1)
	if len(m) == 0 {
		return -1
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

// bNewestLine is the highest LINE-n on the screen, or -1.
func bNewestLine(s *testScreen) int {
	best := -1
	for _, m := range regexp.MustCompile(`LINE-(\d+)`).FindAllStringSubmatch(s.Text(), -1) {
		if n, _ := strconv.Atoi(m[1]); n > best {
			best = n
		}
	}
	return best
}

func TestPortBRenderedAttachSurvivesTheWindowChangingSize(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	// It reports the width it believes it has, which is the thing the resize is supposed to change.
	addRunning(t, sock, "sizer", "sh", "-c",
		`while :; do printf 'SIZER-COLS-%s\r\n' "$(stty size 2>/dev/null | cut -d' ' -f2)"; sleep 0.1; done`)

	s, tap := bAttach(t, sock, "sizer", 80, 24, AttachOptions{Replay: true, Mode: RenderOn}, bOpts{})
	// a rendered attach is drawing before the window changes size
	s.Until("the service reporting 80 columns", func() bool { return bLastNumber(s, "SIZER-COLS-") == 80 })
	s.BottomShows("sizer")

	s.Resize(50, 14)
	// the service was told its new width - which also says the client has handled the resize,
	// since it is the client that tells the daemon
	s.Until("the service reporting 50 columns", func() bool { return bLastNumber(s, "SIZER-COLS-") == 50 })
	after := tap.Len()

	// and it is still drawing the service afterwards
	s.Until("new output after the resize", func() bool {
		return strings.Contains(string(tap.Since(after)), "SIZER-COLS-50")
	})

	// the status line moved to the new last row. The emulator clamps a move below the screen to
	// its last row, so the row it lands on proves nothing alone; what the client asked for does.
	s.BottomShows("sizer")
	for i, r := range s.Rows()[:13] {
		if strings.Contains(r, "[sizer]") {
			t.Errorf("row %d still has the status line: %q", i+1, r)
		}
	}
	s.Prefix('?') // a status line repaint, drawn at whatever row the client believes is last
	s.BottomShows("detach")
	if row := bMaxRow(tap.Since(after)); row > 14 {
		t.Errorf("after resizing to 14 rows the client still draws on row %d", row)
	}
}

func TestPortBResizingWhileReadingTheScrollback(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	// The size the service has, written where the test can read it without it touching the screen.
	sizeFile := filepath.Join(t.TempDir(), "size")
	addRunning(t, sock, "hist", "sh", "-c", `for i in $(seq 1 60); do echo "LINE-$i"; done
trap 'stty size > `+sizeFile+`' WINCH
while :; do sleep 0.05; done`)

	s := attachScreen(t, sock, "hist", 80, 20, AttachOptions{Replay: true, Mode: RenderOn})
	s.Until("the live screen, ending at LINE-60", func() bool { return bNewestLine(s) == 60 })

	// Ctrl-] b is showing older output than the live screen
	s.Prefix('b')
	s.Until("older output", func() bool { n := bNewestLine(s); return n > 0 && n < 60 })

	s.Resize(50, 14)
	eventually(t, "the service told its new size", func() bool {
		b, _ := os.ReadFile(sizeFile)
		return strings.TrimSpace(string(b)) == "13 50"
	})
	// A repaint after the one the resize caused, so what is on screen is from after it.
	s.Prefix('?')
	s.BottomShows("detach")

	// and it is still showing the scrollback after the window resized under it
	if n := bNewestLine(s); n <= 0 || n >= 60 {
		t.Errorf("after resizing while scrolled back the newest line on screen is %d; the screen shows:\n%s", n, s.Text())
	}

	// Ctrl-] g comes back to the live screen at the new size
	s.Prefix('g')
	s.Until("the live screen again", func() bool { return bNewestLine(s) == 60 })
}
