package render

import (
	"strings"
	"testing"

	"github.com/LaPingvino/gozellij/internal/vt/grid"
)

// Hyperlinks, which the conformance corpus cannot see.
//
// tmux's capture-pane does not report them at all, so a recording of a terminal that keeps
// hyperlinks and one that throws them away are the same bytes. The oracle is blind here by
// construction, which is why this is checked against the renderer's own output instead.

func TestAHyperlinkSurvivesTheGridAndComesOutAgain(t *testing.T) {
	term := grid.New(40, 3)
	_, _ = term.Write([]byte("see \x1b]8;;https://example.com/a\x1b\\this link\x1b]8;;\x1b\\ and not this"))
	out := string(Screen(term))

	if !strings.Contains(out, "\x1b]8;;https://example.com/a\x1b\\") {
		t.Fatalf("the link was not drawn:\n%q", out)
	}
	// Opened before the linked text and closed after it, or the words either side join the link.
	open := strings.Index(out, "\x1b]8;;https://example.com/a\x1b\\")
	text := strings.Index(out, "this link")
	closed := strings.Index(out[text:], "\x1b]8;;\x1b\\")
	if open > text || closed < 0 {
		t.Fatalf("the link does not wrap the text it belongs to:\n%q", out)
	}
	if strings.Index(out, "and not this") < text+closed {
		t.Fatal("the text after the link is inside it")
	}
}

func TestAnUnclosedHyperlinkIsClosedByTheFrame(t *testing.T) {
	// A program that opens a link and never closes it would otherwise leave every later thing the
	// terminal draws inside it - the status line, the next pane, the shell prompt after a detach.
	term := grid.New(20, 2)
	_, _ = term.Write([]byte("\x1b]8;;https://example.com/b\x1b\\dangling"))
	out := string(Screen(term))
	if !strings.HasSuffix(strings.TrimSuffix(out, "\x1b[?25h"), "\x1b]8;;\x1b\\") &&
		!strings.Contains(out, "\x1b]8;;\x1b\\") {
		t.Fatalf("an unclosed link was left open at the end of the frame:\n%q", out)
	}
}

func TestOrdinaryTextDrawsNoHyperlinkSequences(t *testing.T) {
	// The cost of this feature on every screen that does not use it has to be nothing.
	term := grid.New(20, 2)
	_, _ = term.Write([]byte("just some text"))
	if out := string(Screen(term)); strings.Contains(out, "\x1b]8") {
		t.Fatalf("a screen with no links emitted link sequences:\n%q", out)
	}
}
