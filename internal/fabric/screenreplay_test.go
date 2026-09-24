package fabric

import (
	"fmt"
	"strings"
	"testing"
)

// A full-screen program's replay is a picture of its screen, even when its recent output is only
// small edits to a screen drawn long before - more than the ring holds. A shell's is its output.
func TestAFullScreenProgramIsReplayedAsItsScreen(t *testing.T) {
	o := NewOutputBuffer(4096)
	o.SetSize(40, 6)
	o.Write([]byte("\x1b[?1049h\x1b[H\x1b[2J"))
	for r := 1; r <= 5; r++ {
		o.Write([]byte(fmt.Sprintf("\x1b[%d;1HROW-%d", r, r)))
	}
	for i := 0; i < 2000; i++ { // far past the 4 KiB ring: the rows are no longer in it
		o.Write([]byte(fmt.Sprintf("\x1b[1;20Htick %06d", i)))
	}
	snap, sub, err := o.Attach(0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Detach()
	if strings.Contains(string(snap), "ROW-3") {
		t.Fatal("the test is broken: the ring still holds the drawing")
	}
	if sub.Screen == nil {
		t.Fatal("no picture for a program on the alternate screen")
	}
	for _, want := range []string{"ROW-1", "ROW-3", "ROW-5", "tick 001999"} {
		if !strings.Contains(string(sub.Screen), want) {
			t.Errorf("the picture lacks %q", want)
		}
	}

	// Back on the normal screen, the picture goes and the replay is the output again.
	o.Write([]byte("\x1b[?1049l$ "))
	_, sub2, _ := o.Attach(0)
	defer sub2.Detach()
	if sub2.Screen != nil {
		t.Error("a picture is still kept after the program left the alternate screen")
	}
}
