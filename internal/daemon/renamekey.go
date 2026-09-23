package daemon

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Ctrl-] , renames the service you are looking at - tmux's key for renaming a window, and the
// thing zellij users do to a tab more than anything else. Shells made with Ctrl-] c get names
// like shell-2, and a list of shell-2, shell-3 and shell-4 tells you nothing about which is which.

// askTimeout is how long the prompt waits between keystrokes before giving up. Longer than the
// service list's, because this is typing rather than choosing - but not for ever, since the
// prompt holds the keyboard and a key pressed by accident must not leave it held.
const askTimeout = 60 * time.Second

// askName reads a name from the keyboard, showing the question and what has been typed so far on
// the status line through say.
//
// Enter accepts. Escape, Ctrl-C and Ctrl-G cancel, as does anything else that starts an escape
// sequence - an arrow key has nothing to move in a line this short, and treating its bytes as
// text would put "[A" in a service name. Backspace and Ctrl-U edit. A gozellij command pressed
// mid-prompt is handed back rather than swallowed, the way the service list does it.
func askName(input *terminalInput, say func(string), question, initial string) (name string, instead *attachOutcome, ok bool) {
	buf := initial
	show := func() { say(question + buf + "_") }
	show()
	for {
		select {
		case chunk := <-input.data:
			for len(chunk) > 0 {
				b := chunk[0]
				switch {
				case b == '\r' || b == '\n':
					return strings.TrimSpace(buf), nil, true
				case b == 0x1b || b == 0x03 || b == 0x07:
					return "", nil, false
				case b == 0x7f || b == 0x08:
					if buf != "" {
						_, size := utf8.DecodeLastRuneInString(buf)
						buf = buf[:len(buf)-size]
					}
				case b == 0x15:
					buf = ""
				case b >= 0x20:
					// Bytes, not runes: a multi-byte character arrives whole in one chunk, and
					// appending its bytes in order keeps it whole.
					buf += string([]byte{b})
				}
				chunk = chunk[1:]
			}
			show()
		case want := <-input.cmds:
			return "", &want, false
		case <-time.After(askTimeout):
			return "", nil, false
		}
	}
}

// renameFromKey asks for a new name for service and renames it, returning the name the service
// has afterwards and what to tell the user. Cancelling, or answering with the name it already
// has, changes nothing and says so.
func renameFromKey(socket, service string, input *terminalInput, say func(string)) (string, *attachOutcome, string) {
	name, instead, ok := askName(input, say, "rename "+service+" to: ", service)
	if instead != nil {
		return service, instead, ""
	}
	if !ok || name == "" || name == service {
		return service, nil, "rename cancelled; still " + service
	}
	c, err := Dial(socket)
	if err != nil {
		return service, nil, fmt.Sprintf("could not rename %s: %v", service, err)
	}
	defer c.Close()
	warning, err := c.Rename(service, name)
	if err != nil {
		return service, nil, fmt.Sprintf("could not rename %s: %v", service, err)
	}
	if warning != "" {
		return name, nil, warning
	}
	return name, nil, fmt.Sprintf("renamed %s to %s", service, name)
}
