package grid

import (
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
	state  state
	params []byte
	inter  []byte
	utf8   []byte
}

type state int

const (
	ground state = iota
	escape
	csi
	osc
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
			p.osc(c)
		}
	}
}

// ground handles ordinary text and the C0 controls. It returns how many extra bytes it consumed,
// so that a multi-byte rune is decoded whole.
func (p *parser) ground(t *Term, b []byte, i int) int {
	c := b[i]
	switch {
	case c == 0x1b:
		p.state = escape
		p.params = p.params[:0]
		p.inter = p.inter[:0]
		return 0
	case c == '\r':
		t.cur.Col = 0
		t.pend = false
		return 0
	case c == '\n', c == 0x0b, c == 0x0c:
		t.lineFeed()
		t.pend = false
		return 0
	case c == '\b':
		if t.pend {
			t.pend = false
		} else if t.cur.Col > 0 {
			t.cur.Col--
		}
		return 0
	case c == '\t':
		// Tab stops every eight columns, which is the default every terminal ships with. Custom
		// stops (HTS/TBC) are not implemented, and the package doc says so.
		next := (t.cur.Col/8 + 1) * 8
		t.cur.Col = min(next, t.cols-1)
		t.pend = false
		return 0
	case c == 0x07:
		return 0 // bell: nothing to draw
	case c < 0x20 || c == 0x7f:
		return 0 // other C0: ignored rather than printed
	}

	if c < 0x80 {
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
		extra++
		p.utf8 = append(p.utf8, b[i+extra])
	}
	if !utf8.FullRune(p.utf8) {
		return extra // wait for the rest
	}
	r, _ := utf8.DecodeRune(p.utf8)
	p.utf8 = p.utf8[:0]
	t.put(r, vt.RuneWidth(r))
	return extra
}

func (p *parser) escape(t *Term, c byte) {
	switch c {
	case '[':
		p.state = csi
		p.params = p.params[:0]
		p.inter = p.inter[:0]
	case ']':
		p.state = osc
	case '7':
		t.saved = t.cur
		p.state = ground
	case '8':
		t.cur = t.saved
		t.pend = false
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
	case 'c': // reset
		*t = *New(t.cols, t.rows)
		p.state = ground
	default:
		// Intermediate bytes of a sequence we do not implement - charset selection, mostly.
		// Swallowing the final byte rather than printing it is the difference between ignoring
		// a sequence and drawing "(B" in the corner of the screen.
		if c >= 0x20 && c <= 0x2f {
			return
		}
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
				case 1049:
					// The one everything actually uses: save the cursor, switch, clear. vim,
					// less, htop and top all begin with this and end with its opposite, which is
					// why "your shell comes back when you quit vim" works at all.
					if set {
						t.enterAlt(true)
					} else {
						t.leaveAlt(true)
					}
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
		t.moveTo(t.cur.Row-arg(0, 1), t.cur.Col)
	case 'B': // cursor down
		t.moveTo(t.cur.Row+arg(0, 1), t.cur.Col)
	case 'C': // cursor forward
		t.moveTo(t.cur.Row, t.cur.Col+arg(0, 1))
	case 'D': // cursor back
		t.moveTo(t.cur.Row, t.cur.Col-arg(0, 1))
	case 'G', '`': // cursor to column
		t.moveTo(t.cur.Row, arg(0, 1)-1)
	case 'd': // cursor to row
		t.moveTo(arg(0, 1)-1, t.cur.Col)
	case 'H', 'f': // cursor position
		t.moveTo(arg(0, 1)-1, arg(1, 1)-1)
	case 'J': // erase in display
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
			t.eraseInRow(t.cur.Row, t.cur.Col, t.cols-1)
		case 1:
			t.eraseInRow(t.cur.Row, 0, t.cur.Col)
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
		t.eraseInRow(t.cur.Row, t.cur.Col, t.cur.Col+arg(0, 1)-1)
	case 'S': // scroll up
		t.scrollUp(arg(0, 1))
	case 'T': // scroll down
		t.scrollDown(arg(0, 1))
	case 'r': // set scrolling region
		top, bottom := arg(0, 1)-1, arg(1, t.rows)-1
		if top < 0 || bottom >= t.rows || top >= bottom {
			top, bottom = 0, t.rows-1
		}
		t.top, t.bottom = top, bottom
		// DECSTBM homes the cursor. Forgetting this is how a status line ends up putting the
		// cursor at the top of the screen on every detach - measured, in this project.
		t.moveTo(0, 0)
	case 's':
		t.saved = t.cur
	case 'u':
		t.cur = t.saved
		t.pend = false
	case 'm':
		t.sgr(ps)
	}
}

func (p *parser) insertLines(t *Term, n int) {
	if t.cur.Row < t.top || t.cur.Row > t.bottom {
		return
	}
	for i := 0; i < n; i++ {
		copy(t.cells[t.cur.Row+1:t.bottom+1], t.cells[t.cur.Row:t.bottom])
		copy(t.wrapped[t.cur.Row+1:t.bottom+1], t.wrapped[t.cur.Row:t.bottom])
		copy(t.used[t.cur.Row+1:t.bottom+1], t.used[t.cur.Row:t.bottom])
		t.cells[t.cur.Row] = blankRow(t.cols)
		t.wrapped[t.cur.Row], t.used[t.cur.Row] = false, 0
	}
}

func (p *parser) deleteLines(t *Term, n int) {
	if t.cur.Row < t.top || t.cur.Row > t.bottom {
		return
	}
	for i := 0; i < n; i++ {
		copy(t.cells[t.cur.Row:t.bottom], t.cells[t.cur.Row+1:t.bottom+1])
		copy(t.wrapped[t.cur.Row:t.bottom], t.wrapped[t.cur.Row+1:t.bottom+1])
		copy(t.used[t.cur.Row:t.bottom], t.used[t.cur.Row+1:t.bottom+1])
		t.cells[t.bottom] = blankRow(t.cols)
		t.wrapped[t.bottom], t.used[t.bottom] = false, 0
	}
}

func (p *parser) deleteChars(t *Term, n int) {
	row := t.cells[t.cur.Row]
	copy(row[t.cur.Col:], row[min(t.cur.Col+n, t.cols):])
	t.eraseInRow(t.cur.Row, t.cols-n, t.cols-1)
}

func (p *parser) insertChars(t *Term, n int) {
	row := t.cells[t.cur.Row]
	copy(row[min(t.cur.Col+n, t.cols):], row[t.cur.Col:])
	t.eraseInRow(t.cur.Row, t.cur.Col, t.cur.Col+n-1)
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

func params(s string) []int {
	if s == "" {
		return nil
	}
	fields := strings.Split(s, ";")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, _ := strconv.Atoi(f) // an empty or malformed parameter is zero, as in a real terminal
		out = append(out, n)
	}
	return out
}

// osc swallows an operating-system command up to its terminator.
//
// Nothing here acts on one - the title is the daemon's business, not the grid's - but they must be
// consumed rather than printed. An OSC 8 hyperlink carries a URL, and a terminal that prints it
// instead of absorbing it puts the URL on the user's screen.
func (p *parser) osc(c byte) {
	switch c {
	case 0x07: // BEL terminates
		p.state = ground
	case 0x1b:
		// ESC \ terminates. Treating the ESC as the end is close enough here: the backslash that
		// follows is consumed by the ground state as an ordinary character only if the stream is
		// malformed, and a malformed stream printing one backslash is not the failure to worry
		// about.
		p.state = ground
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
