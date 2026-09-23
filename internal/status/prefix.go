package status

import (
	"fmt"
	"strconv"
	"strings"
)

// The key that talks to gozellij rather than to the thing you are attached to.
//
// It lives in this package because this package parses the configuration file, and the file is
// gozellij's rather than the status line's - it is named `status` for its first tenant, which is
// the sort of thing that is obvious only to whoever named it. Everything else about the prefix
// belongs to the attach loop.
//
// Configurable because the default is a matter of muscle memory, not of correctness. Ctrl-] is
// telnet's, which is why it was chosen: almost nothing else wants it. But somebody arriving from
// tmux has years of Ctrl-B in their fingers, and a multiplexer that makes them relearn that is
// asking for something it has no right to ask.

// DefaultPrefix is Ctrl-], as telnet has used for decades.
const DefaultPrefix byte = 0x1d

// ParsePrefix reads a key from the spellings a person would actually write.
//
// All of "C-b", "^B", "Ctrl-b", "ctrl+b" and "0x02" mean the same key. Refusing the one somebody
// happened to type, in a file they cannot test without logging out, is not a standard worth
// holding.
func ParsePrefix(s string) (byte, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("no key given")
	}
	// A number, for anybody who knows exactly which byte they want.
	if n, err := strconv.ParseUint(t, 0, 8); err == nil {
		if n == 0 || n > 0x1f {
			return 0, fmt.Errorf("%s is not a control character; a prefix has to be one, or every "+
				"press of an ordinary key would be a command", t)
		}
		return byte(n), nil
	}

	low := strings.ToLower(t)
	for _, p := range []string{"ctrl-", "ctrl+", "c-", "^"} {
		if rest, ok := strings.CutPrefix(low, p); ok {
			low = rest
			break
		}
	}
	if len(low) != 1 {
		return 0, fmt.Errorf("%q is not a key this understands; write it like C-b, ^B or 0x02", s)
	}
	c := low[0]
	switch {
	case c >= 'a' && c <= 'z':
		return c - 'a' + 1, nil // Ctrl-A is 1
	case c == ']':
		return 0x1d, nil
	case c == '[':
		return 0x1b, nil
	case c == '\\':
		return 0x1c, nil
	case c == '^':
		return 0x1e, nil
	case c == '_':
		return 0x1f, nil
	case c == ' ':
		return 0x00, nil
	}
	return 0, fmt.Errorf("%q is not a key this understands; write it like C-b, ^B or 0x02", s)
}

// PrefixLabel is how to write a key so a person recognises it, which is how every message about
// it is built rather than each one spelling the default out and going stale when it changes.
func PrefixLabel(b byte) string {
	switch {
	case b >= 1 && b <= 26:
		return fmt.Sprintf("Ctrl-%c", 'A'+b-1)
	case b == 0x1b:
		return "Ctrl-["
	case b == 0x1c:
		return "Ctrl-\\"
	case b == 0x1d:
		return "Ctrl-]"
	case b == 0x1e:
		return "Ctrl-^"
	case b == 0x1f:
		return "Ctrl-_"
	}
	return fmt.Sprintf("0x%02x", b)
}
