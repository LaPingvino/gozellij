package fabric

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// TermModes follows the terminal modes a stream of output switches on and off, so that they can be
// put back when the stream is replayed and taken off again when it is left.
//
// Found on the first morning gozellij ran Claude Code for real. A full-screen program switches the
// terminal into modes once, at startup - the alternate screen, mouse reporting, focus events - and
// then only draws. Those modes live in the terminal, not in the program, which makes two failures:
//
//   - arriving: the replay is the last 256 KiB of output, and the switch into the alternate screen
//     was a megabyte ago. The program went on drawing for a terminal in a state nobody put it in.
//   - leaving: switching to a shell left mouse reporting on, and every move of the mouse typed an
//     escape sequence into bash - "(arg: 1) 5 3" in the shell's log.
//
// So the daemon feeds each service's output through one of these and starts every replay with
// Preamble, and the client feeds what it passes to the terminal through another and writes Reset
// when it leaves a service.
//
// Only the modes that change what a terminal does with later input or output are followed, and
// only as far as their current value: the end state, not the history. A replay applies the changes
// it contains in order, so starting from the end state still ends in it.
type TermModes struct {
	mu     sync.Mutex
	on     map[int]bool // tracked private modes that differ from their default
	keypad bool         // DECKPAM (ESC =), application keypad
	carry  []byte       // an escape sequence cut off at the end of the last Feed
}

// trackedModes are the DEC private modes followed, with their power-on default.
var trackedModes = map[int]bool{
	1:    false, // application cursor keys
	25:   true,  // cursor visible
	47:   false, // alternate screen (old form)
	1047: false, // alternate screen
	1049: false, // alternate screen, saving the cursor
	1000: false, // mouse: clicks
	1002: false, // mouse: drags
	1003: false, // mouse: all motion
	1004: false, // focus in/out reports
	1005: false, // mouse: UTF-8 coordinates
	1006: false, // mouse: SGR coordinates
	1015: false, // mouse: urxvt coordinates
	2004: false, // bracketed paste
}

// maxCarry bounds how much of a cut-off sequence is held for the next Feed. Anything longer is not
// a mode switch.
const maxCarry = 64

// Feed follows the modes set in p.
func (t *TermModes) Feed(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.carry) > 0 {
		p = append(append([]byte(nil), t.carry...), p...)
		t.carry = t.carry[:0]
	}
	for i := 0; i < len(p); {
		j := bytes.IndexByte(p[i:], 0x1b)
		if j < 0 {
			return
		}
		i += j
		n, complete := t.escape(p[i:])
		if !complete {
			if len(p)-i <= maxCarry {
				t.carry = append(t.carry[:0], p[i:]...)
			}
			return
		}
		i += n
	}
}

// escape reads one escape sequence at the start of p, acts on it if it is one of ours, and says
// how long it was - or that p ends before it does.
func (t *TermModes) escape(p []byte) (int, bool) {
	if len(p) < 2 {
		return 0, false
	}
	switch p[1] {
	case '=':
		t.keypad = true
		return 2, true
	case '>':
		t.keypad = false
		return 2, true
	case '[':
	default:
		return 1, true
	}
	if len(p) < 3 {
		return 0, false
	}
	if p[2] != '?' {
		return 2, true // some other CSI; the scan goes on from inside it, which is harmless
	}
	k := 3
	for k < len(p) && (p[k] >= '0' && p[k] <= '9' || p[k] == ';') {
		k++
	}
	if k == len(p) {
		return 0, false
	}
	final := p[k]
	if final != 'h' && final != 'l' {
		return k + 1, true
	}
	for _, f := range strings.Split(string(p[3:k]), ";") {
		n, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		def, tracked := trackedModes[n]
		if !tracked {
			continue
		}
		if set := final == 'h'; set == def {
			delete(t.on, n)
		} else {
			if t.on == nil {
				t.on = map[int]bool{}
			}
			t.on[n] = set
		}
	}
	return k + 1, true
}

// Preamble is what puts a terminal in the state the stream has left it in, assuming it starts from
// the defaults. Empty when nothing differs.
func (t *TermModes) Preamble() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b bytes.Buffer
	// The alternate screen first, so everything after it applies to the screen being drawn on.
	for _, n := range t.sorted() {
		b.WriteString(modeSeq(n, t.on[n]))
	}
	if t.keypad {
		b.WriteString("\x1b=")
	}
	return b.Bytes()
}

// Reset is what takes a terminal from the state the stream left it in back to the defaults - the
// alternate screen last, so the rest is undone on the screen that stays - and forgets the state.
func (t *TermModes) Reset() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b bytes.Buffer
	order := t.sorted()
	for i := len(order) - 1; i >= 0; i-- {
		n := order[i]
		b.WriteString(modeSeq(n, trackedModes[n]))
	}
	if t.keypad {
		b.WriteString("\x1b>")
	}
	t.on, t.keypad, t.carry = nil, false, t.carry[:0]
	return b.Bytes()
}

// sorted lists the modes that differ from their default, alternate screens first.
func (t *TermModes) sorted() []int {
	out := make([]int, 0, len(t.on))
	for n := range t.on {
		out = append(out, n)
	}
	alt := func(n int) bool { return n == 47 || n == 1047 || n == 1049 }
	sort.Slice(out, func(i, j int) bool {
		if alt(out[i]) != alt(out[j]) {
			return alt(out[i])
		}
		return out[i] < out[j]
	})
	return out
}

func modeSeq(n int, set bool) string {
	if set {
		return "\x1b[?" + strconv.Itoa(n) + "h"
	}
	return "\x1b[?" + strconv.Itoa(n) + "l"
}
