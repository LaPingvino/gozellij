package fabric

import "testing"

func TestTermModesFollowWhatAFullScreenProgramSwitchesOn(t *testing.T) {
	var m TermModes
	// Claude Code's startup, as found in a real log, then a long stretch of drawing.
	m.Feed([]byte("\x1b[?1049h\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1006h\x1b[?1004h"))
	m.Feed([]byte("\x1b[?25l\x1b[Hdrawing\x1b[?25h\x1b[H more"))
	if got, want := string(m.Preamble()), "\x1b[?1049h\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1004h\x1b[?1006h"; got != want {
		t.Errorf("preamble %q\nwant     %q", got, want)
	}
	if got, want := string(m.Reset()), "\x1b[?1006l\x1b[?1004l\x1b[?1003l\x1b[?1002l\x1b[?1000l\x1b[?1049l"; got != want {
		t.Errorf("reset %q\nwant  %q", got, want)
	}
	if got := m.Preamble(); len(got) != 0 {
		t.Errorf("after a reset the state is still %q", got)
	}
}

// A mode switch cut in two by the pty's read boundaries is still seen.
func TestTermModesAcrossASplitWrite(t *testing.T) {
	var m TermModes
	m.Feed([]byte("text\x1b[?10"))
	m.Feed([]byte("49h more"))
	if got := string(m.Preamble()); got != "\x1b[?1049h" {
		t.Errorf("split sequence: preamble %q", got)
	}
}

// Switching a mode back off, in a combined sequence, leaves nothing to restore; untracked modes and
// other sequences are ignored; the cursor hidden at the end is put back hidden.
func TestTermModesEndState(t *testing.T) {
	var m TermModes
	m.Feed([]byte("\x1b[?1000;2004h\x1b[?1000l\x1b[?2026h\x1b[31m\x1b]0;title\x07\x1b=\x1b[?25l"))
	if got, want := string(m.Preamble()), "\x1b[?25l\x1b[?2004h\x1b="; got != want {
		t.Errorf("preamble %q, want %q", got, want)
	}
	if got, want := string(m.Reset()), "\x1b[?2004l\x1b[?25h\x1b>"; got != want {
		t.Errorf("reset %q, want %q", got, want)
	}
}
