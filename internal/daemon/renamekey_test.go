package daemon

import (
	"strings"
	"testing"
)

func promptWith(chunks ...string) (*terminalInput, func() (string, *attachOutcome, bool, []string)) {
	in := &terminalInput{data: make(chan []byte, len(chunks)+1), cmds: make(chan attachOutcome, 1)}
	for _, c := range chunks {
		in.data <- []byte(c)
	}
	return in, func() (string, *attachOutcome, bool, []string) {
		var said []string
		name, instead, ok := askName(in, func(s string) { said = append(said, s) }, "rename x to: ", "shell-2")
		return name, instead, ok, said
	}
}

func TestTheRenamePromptEdits(t *testing.T) {
	cases := []struct {
		what   string
		typed  []string
		want   string
		wantOK bool
	}{
		{"Enter keeps what was offered", []string{"\r"}, "shell-2", true},
		{"typing appends to it", []string{"-work\r"}, "shell-2-work", true},
		{"Ctrl-U clears it", []string{"\x15", "notes\r"}, "notes", true},
		{"backspace takes a whole character", []string{"\x15", "café", "\x7f", "e\r"}, "cafe", true},
		{"spaces round the edges go", []string{"\x15", "  logs \r"}, "logs", true},
		{"Escape cancels", []string{"abc", "\x1b"}, "", false},
		{"Ctrl-C cancels", []string{"\x03"}, "", false},
		// An arrow key is an escape sequence. Its bytes are not a name.
		{"an arrow key cancels rather than typing [A", []string{"\x1b[A"}, "", false},
	}
	for _, c := range cases {
		_, run := promptWith(c.typed...)
		name, instead, ok, _ := run()
		if instead != nil || ok != c.wantOK || name != c.want {
			t.Errorf("%s: got %q ok=%v instead=%v, want %q ok=%v", c.what, name, ok, instead, c.want, c.wantOK)
		}
	}
}

// The prompt shows what has been typed so far, so a person can see what they are about to get.
func TestTheRenamePromptShowsTheLineAsItIsTyped(t *testing.T) {
	_, run := promptWith("\x15", "ab", "\r")
	_, _, _, said := run()
	if len(said) < 3 || !strings.HasSuffix(said[len(said)-1], "ab_") || !strings.HasPrefix(said[0], "rename x to: shell-2") {
		t.Errorf("the prompt showed %q", said)
	}
}

// A gozellij command pressed mid-prompt is handed back, not eaten: somebody who presses detach
// over a question they have changed their mind about means to detach.
func TestACommandMidPromptIsHandedBack(t *testing.T) {
	in, run := promptWith()
	in.cmds <- outcomeDetached
	_, instead, ok, _ := run()
	if ok || instead == nil || *instead != outcomeDetached {
		t.Errorf("a detach pressed mid-prompt came back as %v, ok=%v", instead, ok)
	}
}
