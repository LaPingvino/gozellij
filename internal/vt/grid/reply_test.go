package grid

import (
	"strings"
	"testing"
)

// A terminal that is asked a question has to answer it.
//
// A byte-pipe attach gets this for free: the query reaches the user's real terminal and the reply
// comes back through the same pipe. A client that interprets the stream *is* the terminal, and a
// program that asks where the cursor is and is never told waits for an answer that is not coming.

func replyTo(t *testing.T, input string) string {
	t.Helper()
	term := New(20, 5)
	if _, err := term.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	return string(term.TakeReplies())
}

func TestCursorPositionReport(t *testing.T) {
	// Counted from one, as the report is: row 3, column 7.
	if got, want := replyTo(t, "\x1b[3;7H\x1b[6n"), "\x1b[3;7R"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The column reported is a real column even when a wrap is pending, because the program is going
// to lay out the rest of its screen from the answer.
func TestCursorPositionReportAtTheMargin(t *testing.T) {
	got := replyTo(t, "12345678901234567890"+"\x1b[6n")
	if got != "\x1b[1;20R" {
		t.Fatalf("got %q, want the last column of a twenty-column screen", got)
	}
}

func TestDeviceStatusIsAlwaysFine(t *testing.T) {
	if got, want := replyTo(t, "\x1b[5n"), "\x1b[0n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The primary answer matches what tmux gives, because programs are tested against it. The
// secondary deliberately does not: claiming to be tmux, version and all, is a lie a program can
// act on.
func TestDeviceAttributes(t *testing.T) {
	if got, want := replyTo(t, "\x1b[c"), "\x1b[?1;2;4c"; got != want {
		t.Fatalf("primary: got %q, want %q", got, want)
	}
	secondary := replyTo(t, "\x1b[>c")
	if !strings.HasPrefix(secondary, "\x1b[>0;") {
		t.Fatalf("secondary: got %q, want it to report an unknown terminal type", secondary)
	}
}

// Taking the replies clears them: they are owed once, and sending a stale answer to a later
// question is worse than sending none.
func TestRepliesAreTakenOnce(t *testing.T) {
	term := New(20, 5)
	term.Write([]byte("\x1b[6n"))
	if first := term.TakeReplies(); len(first) == 0 {
		t.Fatal("nothing was owed after a query")
	}
	if again := term.TakeReplies(); len(again) != 0 {
		t.Fatalf("the same reply came back twice: %q", again)
	}
}

// Nothing is owed for ordinary output, so the common path allocates and sends nothing.
func TestNoRepliesWithoutAQuestion(t *testing.T) {
	if got := replyTo(t, "just some text\r\nand more"); got != "" {
		t.Fatalf("got %q, want nothing", got)
	}
}
