package grid

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/LaPingvino/gozellij/internal/vt"
)

// parser turns a byte stream into operations on the grid.
//
// A state machine rather than a scan for patterns, for one reason that matters: a sequence can
// arrive split across writes. A pty hands over whatever happened to be in the buffer, so "\x1b[1;4r"
// routinely arrives as "\x1b[1" and ";4r", and a parser that only recognises whole sequences
// silently prints half of one as text. The state lives here, between calls.
type parser struct {
	state state
	// strEsc marks an ESC seen inside a control string, so that the backslash after it ends the
	// string rather than being part of it.
	strEsc bool
	// strIntro is the character that opened the current control string - P, X, ^ or _ - and
	// strHead the first two bytes of its body, which is all it takes to tell a request apart
	// from a statement. Kept because the decision can only be made once the string is over.
	strIntro byte
	strHead  []byte
	params []byte
	inter  []byte
	utf8   []byte
	// oscBuf collects an operating-system command until its terminator.
	oscBuf []byte
	// charsetSlot is which of G0 and G1 the sequence being parsed is about.
	charsetSlot int
	// invalid marks bytes that turned out not to be a character. The replacement is drawn at the
	// next opportunity rather than immediately, so that an escape sequence arriving in between is
	// obeyed first - see the comment where it is set.
	invalid bool
}

// drawPending draws the replacement character owed for bytes that could not be decoded.
func (p *parser) drawPending(t *Term) {
	if p.invalid {
		p.invalid = false
		t.put(utf8.RuneError, 1)
	}
}

type state int

const (
	ground state = iota
	escape
	csi
	osc
	charsetSelect
	// str is a control string this emulator does not act on - DCS, SOS, PM, APC - being read to
	// its terminator and thrown away. It has to be a state of its own: the body of one is
	// ordinary printable text, and a parser that returns to ground at the ESC P draws it. vim
	// sends "\eP$qm\e\\" on startup and this put "$qm" in the corner of the screen.
	str
)

func (p *parser) feed(t *Term, b []byte) {
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch p.state {
		case ground:
			i += p.ground(t, b, i)
		case escape:
			p.escape(t, c)
		case csi:
			p.csi(t, c)
		case osc:
			p.osc(t, c)
		case charsetSelect:
			t.selectCharset(p.charsetSlot, c)
			p.state = ground
		case str:
			p.str(t, c)
		}
	}
}

// ground handles ordinary text and the C0 controls. It returns how many extra bytes it consumed,
// so that a multi-byte rune is decoded whole.
func (p *parser) ground(t *Term, b []byte, i int) int {
	c := b[i]

	// A byte that is not a continuation byte ends any half-decoded character, whatever it is.
	//
	// At the top, before the escape check, because this has to hold however the bytes were split.
	// The look-ahead below can only see within one write, so with a pty handing over one byte at a
	// time the truncated character was never noticed and its replacement never drawn - which
	// split-invariance reported and a single-write test could not.
	if len(p.utf8) > 0 && (c < 0x80 || c > 0xbf) {
		p.utf8 = p.utf8[:0]
		p.invalid = true
	}
	// Anything but an escape settles the debt now. Waiting for something that draws meant a
	// backspace, a carriage return or the end of the stream left the replacement unwritten:
	// "\xed\b" drew nothing where tmux draws a replacement and then moves the cursor. An escape
	// is the exception, and the reason is the whole point of deferring: the sequence may change
	// the colour, and the replacement is drawn in the colour that results.
	if c != 0x1b {
		p.drawPending(t)
	}
	if c == 0x1b {
		p.state = escape
		p.params = p.params[:0]
		p.inter = p.inter[:0]
		return 0
	}
	if p.control(t, c) {
		return 0
	}

	if c < 0x80 {
		if glyph, ok := t.mapRune(rune(c)); ok {
			// A character set is in force and this byte means something else in it.
			t.putString(glyph, vt.StringWidth(glyph))
			return 0
		}
		t.put(rune(c), vt.RuneWidth(rune(c)))
		return 0
	}

	// UTF-8. An incomplete rune at the end of a write is kept for the next one, because a pty
	// splits wherever it likes and half a character must not become a replacement character.
	//
	// The return is how many *extra* bytes were taken, not how many in total: the caller's loop
	// advances by one itself. Returning the total consumed two bytes per ASCII character and
	// shifted every row of every case - which the corpus reported on this emulator's first run,
	// at the exact column, before a line of it had been read by eye.
	p.utf8 = append(p.utf8, c)
	extra := 0
	for !utf8.FullRune(p.utf8) && extra < 3 && i+extra+1 < len(b) {
		next := b[i+extra+1]
		if next < 0x80 || next > 0xbf {
			// Not a continuation byte, so the character is truncated and this byte belongs to
			// whatever comes next. Absorbing it anyway is how an escape sequence following a
			// half-written character got eaten: vim emitted a colour change straight after one,
			// utf8.FullRune said three bytes were a complete rune because it only counts them,
			// and the ESC vanished into a replacement character while `[34m` printed as text
			// across the screen. Found by running vim through gozellij and diffing against the
			// same vim in tmux. The byte is left for the next pass, where the rule at the top of
			// this function turns the leftovers into a replacement.
			break
		}
		extra++
		p.utf8 = append(p.utf8, next)
	}
	if !utf8.FullRune(p.utf8) {
		if len(p.utf8) >= utf8.UTFMax || !couldComplete(p.utf8) {
			// These bytes will never become a character, so a replacement is drawn for them
			// rather than dropped - dropping them shifted everything after them one column left,
			// which the corpus reported at the exact column. But not yet: what follows may be an
			// escape sequence, and a terminal obeys that first, so the replacement appears in
			// whatever style is current by then. tmux draws it blue when a colour change follows
			// the truncated character, which is exactly what vim emits.
			p.utf8 = p.utf8[:0]
			p.invalid = true
			return extra
		}
		return extra // wait for the rest
	}
	r, size := utf8.DecodeRune(p.utf8)
	if r == utf8.RuneError && size <= 1 {
		// Bytes that are a complete-looking sequence but not a valid character.
		p.utf8 = p.utf8[:0]
		t.put(utf8.RuneError, 1)
		return extra
	}
	p.utf8 = p.utf8[:0]
	t.put(r, vt.RuneWidth(r))
	return extra
}

// control executes a C0 control character, reporting whether it was one.
//
// Shared by every state, because a control inside an escape sequence is executed where it appears
// and the sequence carries on afterwards - measured: `\e[3;5` then a tab then `H` moves the cursor
// to row 3 column 5 *and* the tab took effect on the way. Swallowing them, which this did, lost
// both the control and (for a tab) eight columns of cursor movement.
func (p *parser) control(t *Term, c byte) bool {
	switch {
	case c == '\r':
		t.cur.Col = 0
		t.pend = false
	case c == '\n', c == 0x0b, c == 0x0c:
		t.lineFeed()
		t.pend = false
	case c == '\b':
		if t.pend {
			t.pend = false
		} else if t.cur.Col > 0 {
			t.cur.Col--
		}
	case c == '\t':
		// A tab does nothing at all while a wrap is pending: the cursor is already past the last
		// column and the next character belongs on the next line. Clearing the pending flag here,
		// which this used to do, made that character land at the right margin instead of wrapping.
		if t.pend {
			return true
		}
		t.cur.Col = t.nextTab()
	case c == 0x0e:
		t.shiftTo(1) // shift out: G1 is in use
	case c == 0x0f:
		t.shiftTo(0) // shift in: back to G0
	case c == 0x07:
		// Bell: nothing to draw.
	case c < 0x20 || c == 0x7f:
		// Other C0: ignored rather than printed.
	default:
		return false
	}
	return true
}

func (p *parser) escape(t *Term, c byte) {
	if c == 0x1b {
		// Another escape starts again from the beginning. `\e\eA` draws nothing: the second ESC
		// restarts the sequence and the A ends it. Dropping to ground on the second one, which is
		// what this did, printed the A.
		p.params = p.params[:0]
		p.inter = p.inter[:0]
		return
	}
	if c < 0x20 {
		// A control character between the ESC and the byte that names the sequence is executed
		// where it appears, and the sequence is still waiting afterwards. `\e\rB` is a carriage
		// return and then ESC B, and draws nothing; this used to abandon the escape on the
		// carriage return and print the B.
		p.control(t, c)
		return
	}
	switch c {
	case '[':
		p.state = csi
		p.params = p.params[:0]
		p.inter = p.inter[:0]
	case ']':
		p.state = osc
	case 'P', 'X', '^', '_':
		// DCS, SOS, PM and APC: a command with a body, ending at ST. Read and dropped. What is in
		// them is a terminal's own business - vim asks for the current SGR with a DCS, ncurses
		// asks for terminfo strings - and a program that is not answered does without. What it
		// must not do is print the body, which is what happened before this state existed.
		//
		// Consuming one is not a gap, so it is not reported as one. vim opens by sending
		// "\x1bPzz\x1b\\" and then asking where the cursor is: the whole point is to find out
		// whether the terminal swallows a control string or prints its body, and swallowing it
		// is the right answer. Reporting that as unimplemented meant every editor in a pane
		// announced a fault on the status line for behaving correctly - noise that costs the
		// report the attention the real cases need. What is genuinely unanswered is the subset
		// that asks a question, and finishStr says which those are.
		p.state = str
		p.strEsc = false
		p.strIntro = c
		p.strHead = p.strHead[:0]
	case '7':
		t.saveCursor()
		p.state = ground
	case '8':
		t.restoreCursor()
		p.state = ground
	case 'M': // reverse index
		if t.cur.Row == t.top {
			t.scrollDown(1)
		} else if t.cur.Row > 0 {
			t.cur.Row--
		}
		p.state = ground
	case 'D': // index
		t.lineFeed()
		p.state = ground
	case 'E': // next line
		t.cur.Col = 0
		t.lineFeed()
		p.state = ground
	case 'H': // a tab stop here
		t.setTab()
		p.state = ground
	case '=', '>':
		// Application and numeric keypad, DECKPAM and DECKPNM. The terminal's rather than the
		// grid's: it changes what the keypad sends, and in a rendered attach the keys come from
		// the user's real terminal. Every interactive program on this machine sends one of these
		// and both were being dropped, so a program in application keypad mode was reading the
		// numeric one's bytes.
		t.keypad = c == '='
		p.state = ground
	case 'c': // reset
		*t = *New(t.cols, t.rows)
		p.state = ground
	case '(', ')':
		// Which character set G0 or G1 holds. The byte after this says which.
		p.charsetSlot = 0
		if c == ')' {
			p.charsetSlot = 1
		}
		p.state = charsetSelect
	default:
		// Intermediate bytes of a sequence we do not implement. Swallowing the final byte rather
		// than printing it is the difference between ignoring a sequence and drawing "(B" in the
		// corner of the screen.
		if c >= 0x20 && c <= 0x2f {
			return
		}
		t.noteUnknown(fmt.Sprintf("ESC %c", c))
		p.state = ground
	}
}

func (p *parser) csi(t *Term, c byte) {
	switch {
	case c >= 0x30 && c <= 0x3f: // parameter bytes, including ? < = >
		p.params = append(p.params, c)
		return
	case c >= 0x20 && c <= 0x2f: // intermediate bytes
		p.inter = append(p.inter, c)
		return
	case c == 0x1b:
		// An escape inside a sequence starts a new sequence. It does not merely abandon this one:
		// abandoning and returning to ground made the `[` that followed print as text, which is
		// how `\e[\e[m` put "[m" on the screen where a terminal shows nothing. Found by the
		// generator in five bytes.
		p.state = escape
		p.params = p.params[:0]
		p.inter = p.inter[:0]
		return
	case c < 0x20:
		// A control character inside a sequence is executed here and now, and the sequence goes
		// on afterwards. Not swallowed: a tab arriving mid-CSI moves the cursor eight columns in
		// a real terminal, and this code was losing it.
		//
		// After the escape case, not before it. ESC is itself below 0x20, so putting this first
		// made `\e[;6\eB` stay inside the CSI and let the B complete it as a cursor movement -
		// found by the generator within a minute of the mistake being made.
		p.control(t, c)
		return
	case c < 0x40 || c > 0x7e:
		// Not a final byte at all: abandon rather than hang in this state for ever.
		p.state = ground
		return
	}
	p.dispatch(t, c)
	p.state = ground
}

func (p *parser) dispatch(t *Term, final byte) {
	if len(p.params) > 0 && p.params[0] == '?' {
		// Private modes: DECTCEM is the only one that changes the screen we compare.
		switch final {
		case 'h', 'l':
			set := final == 'h'
			for _, n := range params(string(p.params[1:])) {
				switch n {
				case 25:
					t.cur.Visible = set
				case 47, 1047:
					// The plain alternate screen: no cursor saving of its own.
					if set {
						t.enterAlt(false)
					} else {
						t.leaveAlt(false)
					}
				case 1048:
					// Save or restore the cursor, without switching anything.
					if set {
						t.altSaved = t.cur
					} else {
						t.cur = t.altSaved
						t.pend = false
					}
				case 7:
					// Autowrap. Off means a character written in the last column overwrites it
					// rather than moving to the next line - what a program drawing a table in the
					// rightmost column relies on, and what makes writing there not scroll.
					t.awm = set
				case 1, 12:
					// DECCKM and cursor blink: the terminal's, not the grid's. A program in
					// application cursor-key mode expects the arrow keys to send \eOA rather than
					// \e[A, and in a rendered attach the keys come from the user's real terminal
					// - which sends what it was last told to send, and was never told. Every
					// interactive program on this machine sets DECCKM; it was being dropped.
					t.setMode(n, set)
				case 1000, 1002, 1003, 1004, 1005, 1006, 1015, 2004:
					// Not ours to act on: mouse reporting, its encoding, focus events and
					// bracketed paste all belong to the terminal a person is looking at. Kept so
					// that whatever is drawing this grid can put that terminal into the same
					// state; a byte pipe gets this for free by passing the bytes along.
					t.setMode(n, set)
				case 1049:
					// The one everything actually uses: save the cursor, switch, clear. vim,
					// less, htop and top all begin with this and end with its opposite, which is
					// why "your shell comes back when you quit vim" works at all.
					if set {
						t.enterAlt(true)
					} else {
						t.leaveAlt(true)
					}
				default:
					// Counted, like every other sequence that goes nowhere. This branch had no
					// default at all, so a private mode this emulator does not implement was
					// dropped *and* invisible - the one place in the parser where the survey of
					// what real programs send could not see anything.
					t.noteUnknown(fmt.Sprintf("CSI ?%d %c", n, final))
				}
			}
		}
		return
	}
	ps := params(string(p.params))
	arg := func(i, def int) int {
		if i < len(ps) && ps[i] > 0 {
			return ps[i]
		}
		return def
	}

	switch final {
	case 'A': // cursor up
		t.moveVertically(-arg(0, 1))
	case 'B': // cursor down
		t.moveVertically(arg(0, 1))
	case 'C': // cursor forward
		t.moveTo(t.cur.Row, t.cur.Col+arg(0, 1))
	case 'D': // cursor back
		t.moveTo(t.cur.Row, t.cur.Col-arg(0, 1))
	case 'E': // cursor to the start of a line below
		t.moveVertically(arg(0, 1))
		t.cur.Col = 0
	case 'F': // cursor to the start of a line above
		t.moveVertically(-arg(0, 1))
		t.cur.Col = 0
	case 'G', '`': // cursor to column
		t.moveTo(t.cur.Row, arg(0, 1)-1)
	case 'd': // cursor to row
		t.moveTo(arg(0, 1)-1, t.cur.Col)
	case 'H', 'f': // cursor position
		t.moveTo(arg(0, 1)-1, arg(1, 1)-1)
	case 'J': // erase in display
		if n := arg(0, 0); n > 3 {
			// ED takes 0 to 3. Anything else is discarded rather than falling through to "erase
			// everything", which is what this did: `\e[4J` wiped the screen where a terminal
			// ignores it.
			return
		}
		switch arg(0, 0) {
		case 0:
			t.eraseInRow(t.cur.Row, t.cur.Col, t.cols-1)
			for r := t.cur.Row + 1; r < t.rows; r++ {
				t.eraseInRow(r, 0, t.cols-1)
			}
		case 1:
			for r := 0; r < t.cur.Row; r++ {
				t.eraseInRow(r, 0, t.cols-1)
			}
			t.eraseInRow(t.cur.Row, 0, t.cur.Col)
		default:
			for r := 0; r < t.rows; r++ {
				t.eraseInRow(r, 0, t.cols-1)
			}
		}
	case 'K': // erase in line
		switch arg(0, 0) {
		case 0:
			t.eraseInRow(t.cur.Row, t.effCol(), t.cols-1)
		case 1:
			t.eraseInRow(t.cur.Row, 0, t.effCol())
		default:
			t.eraseInRow(t.cur.Row, 0, t.cols-1)
		}
	case 'L': // insert lines
		p.insertLines(t, arg(0, 1))
	case 'M': // delete lines
		p.deleteLines(t, arg(0, 1))
	case 'P': // delete characters
		p.deleteChars(t, arg(0, 1))
	case '@': // insert characters
		p.insertChars(t, arg(0, 1))
	case 'X': // erase characters
		t.eraseInRow(t.cur.Row, t.effCol(), t.effCol()+arg(0, 1)-1)
	case 'S': // scroll up
		t.scrollUp(arg(0, 1))
	case 'T': // scroll down
		t.scrollDown(arg(0, 1))
	case 'r': // set scrolling region
		// An explicitly written zero is not "use the default" here, it is nonsense, and a
		// terminal discards the whole sequence. `\e[0;0r` looked like a request for the whole
		// screen to this code, which set the region and homed the cursor - moving it for a
		// sequence tmux throws away. `\e[r` with no parameters at all is still the reset.
		if (len(ps) > 0 && ps[0] == 0) || (len(ps) > 1 && ps[1] == 0) {
			return
		}
		top, bottom := arg(0, 1)-1, arg(1, t.rows)-1
		if top < 0 || bottom >= t.rows || top >= bottom {
			// Out of range: ignored completely, region and cursor both left alone. Not reset to
			// the whole screen, which is what this did - a stray `\e[77r` on an eight-row screen
			// then homed the cursor, moving it for a sequence a real terminal had discarded.
			// Measured against tmux, which leaves the cursor exactly where it was.
			return
		}
		t.top, t.bottom = top, bottom
		// DECSTBM homes the cursor. Forgetting this is how a status line ends up putting the
		// cursor at the top of the screen on every detach - measured, in this project.
		t.moveTo(0, 0)
	case 'h', 'l': // ANSI modes, as opposed to the private ones handled above
		set := final == 'h'
		for _, n := range ps {
			switch n {
			case 4:
				t.irm = set
			default:
				t.noteUnknown(fmt.Sprintf("CSI %d %c", n, final))
			}
		}
	case 't': // window manipulation
		// Only the title stack, which is the part of this a person sees. A program that is about
		// to change the window title pushes the old one and pops it on the way out, so that
		// leaving `less` gives you back the title your shell had set. Everything else in this
		// sequence asks to move, resize, raise or report the window, and a multiplexer's pane is
		// not a window - those are counted rather than obeyed.
		switch arg(0, 0) {
		case 22:
			t.pushTitle()
		case 23:
			t.popTitle()
		default:
			t.noteUnknown(fmt.Sprintf("CSI %d t", arg(0, 0)))
		}
	case 's':
		t.saveCursor()
	case 'u':
		t.restoreCursor()
	case 'm':
		t.sgr(ps)
	default:
		// Nothing here implements this. Recorded rather than dropped silently: a byte pipe would
		// have handed it to the terminal, which may well have understood it.
		t.noteUnknown(fmt.Sprintf("CSI %s%c", string(p.inter), final))
	case 'n': // device status report
		switch arg(0, 0) {
		case 5:
			t.reply("\x1b[0n") // "I am fine", which is the only answer there is
		case 6:
			// Where the cursor is, counted from one. A program that asks this is usually working
			// out how wide something it just printed turned out to be, and a wrong answer is
			// worse than none: it will lay out the rest of its screen from it.
			t.reply("\x1b[%d;%dR", t.cur.Row+1, min(t.cur.Col, t.cols-1)+1)
		}
	case 'c': // device attributes
		if len(p.params) > 0 && p.params[0] == '>' {
			// Secondary: what kind of terminal and which version. Not tmux's answer, which is
			// what this was recorded against - claiming to be tmux is a lie a program can act on.
			// Zero is "unknown", which is true.
			t.reply("\x1b[>0;1;0c")
			return
		}
		// Primary: the same answer tmux gives, because programs are tested against it and this
		// emulator is in the same class - a VT100 with an advanced video option.
		t.reply("\x1b[?1;2;4c")
	case 'g': // clear tab stops: this one, or all of them
		t.clearTabs(arg(0, 0) == 3)
	case 'q':
		// DECSCUSR: the shape of the cursor, written `CSI Ps SP q`. The space is what tells it
		// apart from other sequences ending in q, so the intermediate byte has to be checked -
		// acting on every `q` would make an unrelated sequence change the cursor.
		if len(p.inter) == 1 && p.inter[0] == ' ' {
			t.cur.Shape = arg(0, 0)
		}
	}
}

// shiftBottom is the last row that an insert or delete of lines moves.
//
// The scrolling region's bottom when the cursor is inside the region, and the bottom of the screen
// when it is not. Measured against tmux rather than reasoned about: with a region over rows 3-5, a
// delete-line with the cursor above the region shifted the whole screen up, one inside it shifted
// only to the region's bottom, and one below it moved that row alone. The previous rule here -
// ignore the operation entirely when the cursor is outside the region - left a line on screen that
// a real terminal had removed, which the generator found in twelve streams.
func (t *Term) shiftBottom() int {
	if t.cur.Row >= t.top && t.cur.Row <= t.bottom {
		return t.bottom
	}
	return t.rows - 1
}

func (p *parser) insertLines(t *Term, n int) {
	bottom := t.shiftBottom()
	if t.cur.Row > bottom {
		return
	}
	for i := 0; i < n; i++ {
		copy(t.cells[t.cur.Row+1:bottom+1], t.cells[t.cur.Row:bottom])
		copy(t.wrapped[t.cur.Row+1:bottom+1], t.wrapped[t.cur.Row:bottom])
		copy(t.used[t.cur.Row+1:bottom+1], t.used[t.cur.Row:bottom])
		t.cells[t.cur.Row] = blankRow(t.cols)
		t.wrapped[t.cur.Row], t.used[t.cur.Row] = false, 0
	}
}

func (p *parser) deleteLines(t *Term, n int) {
	bottom := t.shiftBottom()
	if t.cur.Row > bottom {
		return
	}
	for i := 0; i < n; i++ {
		copy(t.cells[t.cur.Row:bottom], t.cells[t.cur.Row+1:bottom+1])
		copy(t.wrapped[t.cur.Row:bottom], t.wrapped[t.cur.Row+1:bottom+1])
		copy(t.used[t.cur.Row:bottom], t.used[t.cur.Row+1:bottom+1])
		t.cells[bottom] = blankRow(t.cols)
		t.wrapped[bottom], t.used[bottom] = false, 0
	}
}

func (p *parser) deleteChars(t *Term, n int) {
	col := t.effCol()
	if col >= t.cols {
		return
	}
	row := t.cells[t.cur.Row]
	copy(row[col:], row[min(col+n, t.cols):])
	// The blanks go at the end of what moved, never before the cursor. Erasing from cols-n
	// started before the cursor whenever n was larger than the tail, so `\e[40P` on a forty-column
	// screen erased the whole row including the character to the left of the cursor - which a
	// terminal leaves alone, because deleting characters at the cursor cannot touch what is
	// behind it.
	t.eraseInRow(t.cur.Row, max(t.cols-n, col), t.cols-1)
}

func (p *parser) insertChars(t *Term, n int) {
	col := t.effCol()
	if col >= t.cols {
		return
	}
	row := t.cells[t.cur.Row]
	copy(row[min(col+n, t.cols):], row[col:])
	t.eraseInRow(t.cur.Row, col, col+n-1)
}

// sgr sets the style for cells written from now on. It is stored and never compared by the
// conform harness yet, which that package says out loud: a case passing says nothing about colour.
func (t *Term) sgr(ps []int) {
	if len(ps) == 0 {
		ps = []int{0}
	}
	for i := 0; i < len(ps); i++ {
		n := ps[i]
		switch {
		case n == 0:
			t.style = vt.Style{}
		case n == 1:
			t.style.Bold = true
		case n == 2:
			t.style.Faint = true
		case n == 3:
			t.style.Italic = true
		case n == 4:
			t.style.Underline = true
		case n == 5:
			t.style.Blink = true
		case n == 7:
			t.style.Reverse = true
		case n == 9:
			t.style.Strikethrough = true
		case n == 22:
			t.style.Bold, t.style.Faint = false, false
		case n == 23:
			t.style.Italic = false
		case n == 24:
			t.style.Underline = false
		case n == 27:
			t.style.Reverse = false
		case n == 25:
			t.style.Blink = false
		case n == 29:
			t.style.Strikethrough = false
		case n >= 30 && n <= 37:
			t.style.Fg = vt.Color{Kind: vt.ColorIndexed, Index: uint8(n - 30)}
		case n >= 40 && n <= 47:
			t.style.Bg = vt.Color{Kind: vt.ColorIndexed, Index: uint8(n - 40)}
		case n == 39:
			t.style.Fg = vt.Color{}
		case n == 49:
			t.style.Bg = vt.Color{}
		case n >= 90 && n <= 97:
			// The bright colours, 8-15 of the palette. Missing these is not cosmetic: vim draws
			// the tildes past the end of a buffer in bright blue, and the corpus caught it the
			// first time styles were compared at all.
			t.style.Fg = vt.Color{Kind: vt.ColorIndexed, Index: uint8(n - 90 + 8)}
		case n >= 100 && n <= 107:
			t.style.Bg = vt.Color{Kind: vt.ColorIndexed, Index: uint8(n - 100 + 8)}
		case n == 38 || n == 48:
			// 38;5;N and 38;2;R;G;B, and the same for the background. The parameters belong to
			// this code rather than being separate attributes, so they are consumed here.
			c, used := extendedColor(ps[i+1:])
			if n == 38 {
				t.style.Fg = c
			} else {
				t.style.Bg = c
			}
			i += used
		}
	}
}

// params splits the numeric parameters of a sequence.
//
// An omitted parameter comes back as -1 rather than 0, because the two are not the same thing and
// one sequence cares: DECSTBM treats an explicitly written zero as nonsense and discards the whole
// sequence, while an omitted parameter means "the default". Conflating them made `\e[;6r` - a
// perfectly ordinary request for a region ending at row six - be thrown away, which the generator
// caught immediately after the zero rule was added.
func params(s string) []int {
	if s == "" {
		return nil
	}
	fields := strings.Split(s, ";")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		if f == "" {
			out = append(out, -1)
			continue
		}
		n, _ := strconv.Atoi(f) // a malformed parameter is zero, as in a real terminal
		out = append(out, n)
	}
	return out
}

// osc swallows an operating-system command up to its terminator.
//
// Nothing here acts on one - the title is the daemon's business, not the grid's - but they must be
// consumed rather than printed. An OSC 8 hyperlink carries a URL, and a terminal that prints it
// instead of absorbing it puts the URL on the user's screen.
func (p *parser) osc(t *Term, c byte) {
	switch c {
	case 0x07: // BEL terminates
		p.finishOSC(t)
	case 0x1b:
		// ESC \ terminates. Treating the ESC as the end is close enough here: the backslash that
		// follows is consumed by the ground state as an ordinary character only if the stream is
		// malformed, and a malformed stream printing one backslash is not the failure to worry
		// about.
		p.finishOSC(t)
	default:
		// The command and its argument, kept only as far as a title can be. A cap, because this
		// is a buffer filled by whatever a service chooses to send: an OSC that never terminates
		// would otherwise grow until the process died, which is a denial of service written by
		// accident.
		if len(p.oscBuf) < 1024 {
			p.oscBuf = append(p.oscBuf, c)
		}
	}
}

// str reads a control string to its end and throws it away.
//
// Terminated by ST - ESC followed by a backslash - and by nothing else. BEL ends an *OSC*, and
// this was written to accept it here too; the oracle disagreed. tmux sent "\eX sos body \aseven"
// and drew nothing at all, where accepting the BEL puts "seven" on the screen. So the body runs to
// ST however long that takes, and a program that opens one of these and never closes it has turned
// its own output off - which is what a real terminal does to it.
//
// An ESC followed by anything else stays inside the string: a stray ESC is not a terminator, and
// abandoning on it would put the rest of the body on the screen, which is the bug this state
// exists to prevent.
func (p *parser) str(t *Term, c byte) {
	if p.strEsc {
		p.strEsc = false
		if c == '\\' {
			p.state = ground
			p.finishStr(t)
		}
		return
	}
	if c == 0x1b {
		p.strEsc = true
		return
	}
	if len(p.strHead) < 2 {
		p.strHead = append(p.strHead, c)
	}
}

// finishStr reports a control string that asked something this emulator never answers.
//
// Only a DCS can ask: DECRQSS ("$q", what is the current setting of ...) and XTGETTCAP ("+q",
// what does your terminfo say about ...). A program that gets no reply falls back to a default,
// which is survivable and occasionally wrong, and that is worth one line on the status line.
// Everything else - a string that states something, and every SOS, PM and APC, none of which
// carry a question - is consumed and that is the end of it.
func (p *parser) finishStr(t *Term) {
	if p.strIntro != 'P' || len(p.strHead) < 2 || p.strHead[1] != 'q' {
		return
	}
	switch p.strHead[0] {
	case '$', '+':
		t.noteUnknown(fmt.Sprintf("DCS %sq request", string(p.strHead[0])))
	}
}

// finishOSC acts on a completed operating-system command.
//
// Only the title, and only because somebody is looking at it: a byte pipe passes OSC 2 straight
// through, so a shell's title tracks what it is running. A client that interprets the stream has
// to carry that itself or the title freezes at whatever it said when the attach started.
func (p *parser) finishOSC(t *Term) {
	p.state = ground
	cmd := string(p.oscBuf)
	p.oscBuf = p.oscBuf[:0]
	num, arg, ok := strings.Cut(cmd, ";")
	if !ok {
		return
	}
	switch num {
	default:
		// An operating-system command this does not act on - a hyperlink, a colour query, a
		// clipboard write. A byte pipe would have passed it to the terminal.
		t.noteUnknown("OSC " + num)
	case "10", "11":
		// The foreground and background colour. A query - the argument is "?" - is a question the
		// program waits for an answer to: vim asks what colour the terminal is so that it can
		// choose a light or a dark scheme, and guesses when nobody says. This grid does not know
		// the answer, because it is a property of the terminal the user is actually looking at,
		// so the query is recorded and whoever is drawing this grid answers it. Anything else is
		// a program *setting* the colour, which is the terminal's business and not the grid's.
		if arg != "?" {
			t.noteUnknown("OSC " + num)
			return
		}
		n, _ := strconv.Atoi(num)
		if len(t.colourAsks) < colourAskLimit {
			t.colourAsks = append(t.colourAsks, n)
		}
	case "8":
		// A hyperlink: OSC 8 ; params ; URI. An empty URI closes the one that was open. The
		// parameters are an optional id for joining split runs together, which nothing here
		// needs - the cells remember the URL and that is what a renderer has to emit.
		//
		// man emits thirty-odd of these on a single page. Dropping them was the largest thing
		// left on the list of what this emulator does not implement.
		_, uri, ok := strings.Cut(arg, ";")
		if !ok {
			// No second semicolon at all: malformed, and closing whatever was open is the
			// conservative reading - a link that never ends swallows the rest of the screen.
			t.style.Link = ""
			return
		}
		t.style.Link = uri
	case "0", "2":
		// 0 sets the icon name and the title, 2 sets the title. Nothing here distinguishes them,
		// because nothing downstream of it does either.
		t.title = arg
	}
}

// extendedColor reads the 5;N and 2;R;G;B forms of SGR 38 and 48, returning how many parameters it
// consumed so the caller can skip them.
func extendedColor(rest []int) (vt.Color, int) {
	if len(rest) == 0 {
		return vt.Color{}, 0
	}
	switch rest[0] {
	case 5:
		if len(rest) < 2 {
			return vt.Color{}, 1
		}
		return vt.Color{Kind: vt.ColorIndexed, Index: uint8(rest[1])}, 2
	case 2:
		if len(rest) < 4 {
			return vt.Color{}, len(rest)
		}
		return vt.Color{Kind: vt.ColorRGB, R: uint8(rest[1]), G: uint8(rest[2]), B: uint8(rest[3])}, 4
	}
	return vt.Color{}, 1
}

// couldComplete reports whether these bytes might still become a character once more arrive.
//
// A lead byte says how many bytes follow it. Anything else - a stray continuation byte, or a lead
// byte that promises fewer bytes than are already buffered - is rubbish that will never resolve,
// and a parser that waits for it stops drawing.
func couldComplete(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	var want int
	switch c := b[0]; {
	case c < 0x80:
		want = 1
	case c >= 0xc2 && c <= 0xdf:
		want = 2
	case c >= 0xe0 && c <= 0xef:
		want = 3
	case c >= 0xf0 && c <= 0xf4:
		want = 4
	default:
		return false
	}
	return len(b) < want
}
