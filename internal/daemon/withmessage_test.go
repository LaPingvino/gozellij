package daemon

import (
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/vt"
)

// A message sits next to the tabs rather than replacing them, and it is the bar that gives way -
// never the end of the message, which is where "now on shell" is.
func TestAMessageKeepsTheTabsAndItsOwnEnd(t *testing.T) {
	bar := func(width int) string {
		// Wider than asked, which is what made the message lose its end.
		return "Ctrl-] ? [shell] 1/1 up 8cpu 9.0G/23.5G 6.6G free 2026-09-24 04:19"[:min(width+3, 67)]
	}
	msg := "shell-2 exited and was closed - now on shell"
	got := withMessage(bar, msg, 100)
	if !strings.HasSuffix(got, msg) {
		t.Errorf("the message lost its end: %q", got)
	}
	if !strings.HasPrefix(got, "Ctrl-] ? [shell] 1/1") {
		t.Errorf("the tabs are gone: %q", got)
	}
	if w := vt.StringWidth(got); w > 100 {
		t.Errorf("%d columns on a 100-column terminal", w)
	}
	// Too narrow for both: the message has the row.
	if got := withMessage(bar, msg, 50); !strings.Contains(got, "now on") {
		t.Errorf("on a narrow terminal: %q", got)
	}
}
