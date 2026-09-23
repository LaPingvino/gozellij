package grid

import "testing"

// OSC 7 is how a shell tells the terminal where it is, and it is the reason a new tab opens in
// the directory the last one was in. A byte pipe passes it through; an emulator has to carry it,
// or the attach that interprets the stream is the one that loses the feature.
//
// Found by the survey rather than reasoned about: of twenty-three real programs, fish is the only
// one here that sends it.
func TestWorkingDirectoryIsCarried(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want string
	}{
		{"a file URL, BEL-terminated", "\x1b]7;file://box/home/joop\x07", "file://box/home/joop"},
		{"a file URL, ST-terminated", "\x1b]7;file://box/tmp\x1b\\", "file://box/tmp"},
		{"the latest one wins", "\x1b]7;file://box/a\x07\x1b]7;file://box/b\x07", "file://box/b"},
		{"a path with spaces", "\x1b]7;file://box/two%20words\x07", "file://box/two%20words"},
		// Refused rather than passed on: this value is emitted again into a sequence this code
		// builds, and a directory name has no business carrying a control byte. An ESC cannot
		// get this far - it ends the OSC where it stands - but the other thirty can.
		{"a control byte", "\x1b]7;file://box/a\x01b\x07", ""},
		{"a newline", "\x1b]7;file://box/a\nb\x07", ""},
		{"a delete", "\x1b]7;file://box/a\x7fb\x07", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			term := New(20, 4)
			term.Write([]byte(c.in))
			if got := term.Dir(); got != c.want {
				t.Fatalf("Dir() = %q, want %q", got, c.want)
			}
		})
	}
}

// Nothing reports it as unimplemented any more, because it is implemented. The survey is what
// says whether that is true, and it reads Unknown().
func TestWorkingDirectoryIsNotReportedAsAGap(t *testing.T) {
	term := New(20, 4)
	term.Write([]byte("\x1b]7;file://box/home/joop\x07"))
	if u := term.Unknown(); len(u) != 0 {
		t.Fatalf("OSC 7 is handled but still reported: %v", u)
	}
}

// A refused one is said out loud. Rule 1: dropping it silently would leave a terminal quietly
// believing the wrong directory with nothing to connect that to a sequence that went nowhere -
// and without this the noteUnknown in the parser is a line nobody has watched run.
func TestARefusedWorkingDirectoryIsReported(t *testing.T) {
	term := New(20, 4)
	term.Write([]byte("\x1b]7;file://box/a\x01b\x07"))
	if term.Unknown()["OSC 7 with a control byte in it"] == 0 {
		t.Fatalf("a refused OSC 7 was dropped in silence: %v", term.Unknown())
	}
}

// And a program that never says leaves it empty, so the renderer has nothing to pass on and the
// terminal keeps whatever it had.
func TestNoWorkingDirectoryIsNoDirectory(t *testing.T) {
	term := New(20, 4)
	term.Write([]byte("hello\x1b]2;a title\x07"))
	if got := term.Dir(); got != "" {
		t.Fatalf("Dir() = %q with nothing sent", got)
	}
}
