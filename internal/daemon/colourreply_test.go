package daemon

import (
	"bytes"
	"testing"
	"time"
)

// The colour recogniser has to be right about three things, and only one of them is about colours.
//
// It sits between the user's keyboard and the pane, which means every mistake it makes is a
// keystroke that went somewhere else. These are those three things: an answer never reaches the
// pane, everything that is not an answer does, and a half-finished answer cannot hold typing for
// ever.

func askingScreen(t *testing.T) *renderedScreen {
	t.Helper()
	s := newRenderedScreen(&bytes.Buffer{}, 80, 24, 1, nil)
	s.AskColours()
	return s
}

func TestAColourAnswerNeverReachesThePane(t *testing.T) {
	s := askingScreen(t)
	left := s.TakeColourReplies([]byte("\x1b]11;rgb:1a1a/1b1b/1c1c\x1b\\"))
	if len(left) != 0 {
		t.Fatalf("the answer was passed on to the pane as %q", left)
	}
	if got, ok := s.ColourAnswer(11); !ok || got != "rgb:1a1a/1b1b/1c1c" {
		t.Fatalf("the background came out as %q ok=%v", got, ok)
	}
}

func TestBothColoursAreRecognisedAndBELEndsThemToo(t *testing.T) {
	s := askingScreen(t)
	// xterm answers with BEL, others with ST, and a terminal may mix them.
	if left := s.TakeColourReplies([]byte("\x1b]10;rgb:ffff/ffff/ffff\x07\x1b]11;rgb:0/0/0\x1b\\")); len(left) != 0 {
		t.Fatalf("something was passed on: %q", left)
	}
	if got, _ := s.ColourAnswer(10); got != "rgb:ffff/ffff/ffff" {
		t.Fatalf("foreground %q", got)
	}
	if got, _ := s.ColourAnswer(11); got != "rgb:0/0/0" {
		t.Fatalf("background %q", got)
	}
}

func TestTypingAroundAnAnswerStillReachesThePane(t *testing.T) {
	s := askingScreen(t)
	// Somebody typed while the terminal was answering. Every byte of it has to arrive, in order.
	left := s.TakeColourReplies([]byte("ab\x1b]11;rgb:0/0/0\x1b\\cd"))
	if string(left) != "abcd" {
		t.Fatalf("the typing came through as %q, wanted \"abcd\"", left)
	}
}

func TestAnAnswerSplitAcrossTwoReadsIsStillAnAnswer(t *testing.T) {
	s := askingScreen(t)
	if left := s.TakeColourReplies([]byte("\x1b]11;rgb:0/0")); len(left) != 0 {
		t.Fatalf("the first half was passed on: %q", left)
	}
	if left := s.TakeColourReplies([]byte("/0\x1b\\after")); string(left) != "after" {
		t.Fatalf("the second half gave %q, wanted \"after\"", left)
	}
	if got, ok := s.ColourAnswer(11); !ok || got != "rgb:0/0/0" {
		t.Fatalf("the reassembled answer is %q ok=%v", got, ok)
	}
}

func TestOrdinaryTypingIsUntouched(t *testing.T) {
	s := askingScreen(t)
	in := []byte("ls -la\r\x1b[A\x1b[B")
	if left := s.TakeColourReplies(in); !bytes.Equal(left, in) {
		t.Fatalf("typing was changed to %q", left)
	}
}

func TestAHalfFinishedAnswerCannotHoldTypingForEver(t *testing.T) {
	s := askingScreen(t)
	if left := s.TakeColourReplies([]byte("\x1b]11;rgb:0/0")); len(left) != 0 {
		t.Fatalf("the first half was passed on: %q", left)
	}
	// The terminal never finished. Once the window closes the bytes are the user's again: they
	// are given to the pane rather than kept, which is the difference between a recogniser and a
	// place keystrokes go to die.
	s.mu.Lock()
	s.askUntil = time.Now().Add(-time.Second)
	s.mu.Unlock()
	left := s.TakeColourReplies([]byte("typed"))
	if string(left) != "\x1b]11;rgb:0/0typed" {
		t.Fatalf("after the window closed the held bytes came out as %q", left)
	}
}

func TestAnEndlessAnswerCannotHoldTypingEither(t *testing.T) {
	s := askingScreen(t)
	// A terminal that starts a reply and keeps talking. Past the bound it is not a reply.
	long := append([]byte("\x1b]11;"), bytes.Repeat([]byte("x"), colourReplyLimit+10)...)
	if left := s.TakeColourReplies(long); !bytes.Equal(left, long) {
		t.Fatalf("a reply longer than the bound was swallowed instead of passed on (%d bytes back)", len(left))
	}
}

func TestNothingIsFilteredBeforeTheQuestionIsAsked(t *testing.T) {
	// The byte pipe never asks, so it must never catch: a program's own OSC 11 reply, typed or
	// pasted, belongs to whoever is reading the keyboard.
	s := newRenderedScreen(&bytes.Buffer{}, 80, 24, 1, nil)
	in := []byte("\x1b]11;rgb:0/0/0\x1b\\")
	if left := s.TakeColourReplies(in); !bytes.Equal(left, in) {
		t.Fatalf("filtered without having asked: %q", left)
	}
}

func TestTheQueryIsActuallySent(t *testing.T) {
	out := &bytes.Buffer{}
	s := newRenderedScreen(out, 80, 24, 1, nil)
	s.AskColours()
	s.AskColours() // once per attach, however often it is called
	if got, want := out.String(), "\x1b]10;?\x1b\\\x1b]11;?\x1b\\"; got != want {
		t.Fatalf("the client asked %q, wanted %q", got, want)
	}
}
