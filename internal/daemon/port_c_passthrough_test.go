package daemon

import (
	"testing"
)

// Ported from acceptance.sh "the terminal is asked what colour it is, on the way in", "the arrow
// keys and the title stack have to reach the terminal" and "mouse and paste modes have to reach
// the real terminal". All of it is invisible on a screen, so these read the bytes the rendered
// client writes to its terminal, the way the script read a file the client's output went to.

func TestPortCRenderedAttachAsksTheTerminalItsColours(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "colourer", "sh", "-c", `printf 'COLOUR-DEMO\r\n'; exec cat`)

	s, raw := cAttachRecorded(t, sock, "colourer", 40, 6, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("COLOUR-DEMO")
	s.Until("both colour queries sent to the terminal", func() bool {
		return raw.Has("\x1b]11;?") && raw.Has("\x1b]10;?")
	})
}

func TestPortCApplicationCursorKeysAndKeypadReachTheTerminal(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "keypadder", "sh", "-c", `printf '\033[?1h\033=KEYS-ON\r\n'; exec cat`)

	s, raw := cAttachRecorded(t, sock, "keypadder", 40, 6, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("KEYS-ON")
	s.Until("application cursor keys and keypad passed to the terminal", func() bool {
		return raw.Has("\x1b[?1h") && raw.Has("\x1b=")
	})
}

func TestPortCAPoppedTitleIsTheOneThatWasPushed(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	// Each stage waits for a line, so the attach is certainly there for all three: the script's
	// version once ran through them before the attach existed and passed for the wrong reason.
	addRunning(t, sock, "stacker", "sh", "-c", `stty -echo; printf 'STACK-UP\r\n'; read x
printf '\033]2;FIRST-TITLE\007'; read x
printf '\033[22t\033]2;SECOND-TITLE\007'; read x
printf '\033[23tPOPPED\r\n'; exec cat`)

	s := attachScreen(t, sock, "stacker", 40, 6, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("STACK-UP")
	s.Type("\r")
	s.Until("the first title", func() bool { return cTitle(s) == "FIRST-TITLE" })
	s.Type("\r")
	s.Until("the second title, pushed over the first", func() bool { return cTitle(s) == "SECOND-TITLE" })
	s.Type("\r")
	s.Shows("POPPED")
	s.Until("the first title back after the pop", func() bool { return cTitle(s) == "FIRST-TITLE" })
}

func TestPortCMouseAndPasteModesReachTheTerminalAndAreTakenBack(t *testing.T) {
	screenEnv(t)
	_, _, sock := newTestDaemon(t)
	addRunning(t, sock, "moder", "sh", "-c", `printf '\033[?2004h\033[?1000hMODES-ON\r\n'; exec cat`)
	addRunning(t, sock, "noder", "sh", "-c", `printf 'NO-MODES\r\n'; exec cat`)

	s, raw := cAttachRecorded(t, sock, "moder", 40, 6, AttachOptions{Replay: true, Mode: RenderOn})
	s.Shows("MODES-ON")
	s.Until("bracketed paste and mouse reporting passed to the terminal", func() bool {
		return raw.Has("\x1b[?2004h") && raw.Has("\x1b[?1000h")
	})
	if m := s.Modes(); !m[2004] || !m[1000] {
		t.Errorf("the terminal does not have the modes on: %v", m)
	}

	// Withdrawn when the keyboard moves to a service that did not ask for them.
	s.Prefix('n')
	s.Shows("NO-MODES")
	s.Until("the modes taken back", func() bool {
		return raw.Has("\x1b[?2004l") && raw.Has("\x1b[?1000l")
	})
	if m := s.Modes(); m[2004] || m[1000] {
		t.Errorf("modes left on after switching to a service without them: %v", m)
	}
}
